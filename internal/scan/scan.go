package scan

import (
	"context"
	"log/slog"
	"runtime"
	"syscall"
	"time"

	"github.com/example/process_exporter/internal/config"
	"github.com/example/process_exporter/internal/filter"
	"github.com/example/process_exporter/internal/namer"
	"github.com/example/process_exporter/internal/procfs"
)

// Process is one process as read in one scan. It is a working value
// consumed by the aggregator and discarded; it never leaves the scan
// and no PID from it ever reaches a metric label.
type Process struct {
	PID        int
	PPID       int
	Comm       string
	Name       string // derived group name
	User       string
	UID        uint32
	State      byte
	StartTicks uint64
	StartUnix  float64

	// Gauges, read fresh each scan
	RSSBytes    uint64
	VSizeBytes  uint64
	SharedBytes uint64
	DataBytes   uint64
	Threads     int
	OpenFDs     int
	MaxFDs      int

	// Counter deltas for this scan, already differenced against state
	DUTimeTicks  uint64
	DSTimeTicks  uint64
	DMinorFaults uint64
	DMajorFaults uint64
	DVolCtxSw    uint64
	DInvolCtxSw  uint64
	DReadBytes   uint64
	DWriteBytes  uint64

	// Read status, so the aggregator can distinguish unknown from zero.
	// An unprivileged exporter cannot read io or fd for processes it
	// does not own; reporting zero would be a lie.
	HaveDeltas bool
	HaveIO     bool
	HaveFDs    bool
	HaveStatus bool
}

// Result is the outcome of one scan.
type Result struct {
	Procs      []Process
	ScanAt     time.Time
	Duration   time.Duration
	Generation uint64

	Total    int
	Scanned  int
	Ignored  int
	Vanished int
	Denied   int
	ReadErrs map[string]int

	StateEntries int
	StatePruned  int
	SelfCPU      float64
}

// Deps holds what the Scanner needs from the rest of the system.
type Deps struct {
	Config func() *config.Config
	System *procfs.System
	Users  *procfs.UserCache
	Filter func() *filter.Filter
	Namer  func() *namer.Namer
}

// Scanner walks /proc and produces per-process readings. Exactly one
// scan runs at a time, because two would both mutate the state map.
type Scanner struct {
	deps   Deps
	state  *StateMap
	scanNo uint64
}

// New creates a Scanner.
func New(d Deps) *Scanner {
	return &Scanner{deps: d, state: NewStateMap()}
}

// StateLen returns the per-PID state map size, for the metric.
func (s *Scanner) StateLen() int { return s.state.Len() }

// ResetState discards every per-PID entry. It is called when a naming
// configuration change invalidates the cached group names.
func (s *Scanner) ResetState() { s.state = NewStateMap() }

// Scan performs one complete scan.
//
// The walk is broken into batches with a sleep between them. The sleep
// is not idle time wasted: it is what stops the scheduler treating the
// exporter as a CPU-bound task, which is what keeps it off the top of
// the process list. A scan of two thousand processes with a batch of
// fifty and a sleep of five milliseconds spends two hundred
// milliseconds sleeping and roughly a hundred reading, out of a fifteen
// second interval.
func (s *Scanner) Scan(ctx context.Context) (*Result, error) {
	cfg := s.deps.Config()
	f := s.deps.Filter()
	n := s.deps.Namer()

	start := time.Now()
	cpuBefore := selfCPU()

	gen := s.state.NextGeneration()
	s.scanNo++

	pids, err := procfs.ListPIDs(cfg.Scan.ProcPath)
	if err != nil {
		return nil, err
	}

	res := &Result{
		ScanAt:     start,
		Generation: gen,
		Total:      len(pids),
		Procs:      make([]Process, 0, len(pids)),
		ReadErrs:   make(map[string]int),
	}

	doFDs := s.shouldScanFDs(cfg)
	batch := cfg.Scan.BatchSize
	if batch < 1 {
		batch = 1
	}
	sleep := cfg.Scan.BatchSleep.D()

	for i := 0; i < len(pids); i += batch {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		end := i + batch
		if end > len(pids) {
			end = len(pids)
		}
		for _, pid := range pids[i:end] {
			if p := s.readProcess(pid, cfg, f, n, doFDs, res); p != nil {
				res.Procs = append(res.Procs, *p)
				res.Scanned++
			}
		}
		if end < len(pids) && sleep > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep):
			}
		}
	}

	res.StatePruned = s.state.Prune()
	res.StateEntries = s.state.Len()
	res.Duration = time.Since(start)
	res.SelfCPU = selfCPU() - cpuBefore
	if res.SelfCPU < 0 {
		res.SelfCPU = 0
	}

	slog.Debug("scan complete",
		"duration", res.Duration,
		"total", res.Total,
		"scanned", res.Scanned,
		"ignored", res.Ignored,
		"vanished", res.Vanished,
		"denied", res.Denied,
		"fd_scan", doFDs,
		"self_cpu_ms", res.SelfCPU*1000)

	return res, nil
}

// readProcess assembles one process. It returns nil when the process
// should be skipped, which covers both vanishing and being ignored.
func (s *Scanner) readProcess(pid int, cfg *config.Config, f *filter.Filter,
	n *namer.Namer, doFDs bool, res *Result) *Process {

	procPath := cfg.Scan.ProcPath

	// stat is read first and unconditionally. It is the cheapest read
	// and it supplies the start time, without which no other reading
	// can be attributed to a process identity.
	st, err := procfs.ReadStat(procPath, pid)
	if err != nil {
		if procfs.IsVanished(err) {
			res.Vanished++
		} else if procfs.IsDenied(err) {
			res.Denied++
		} else {
			res.ReadErrs["stat"]++
		}
		return nil
	}

	prev, havePrev := s.state.Lookup(pid, st.StartTicks)

	// The command line is read only when something needs it: the filter
	// when it inspects cmdline or detects kernel threads, or the namer
	// when there is no cached name for this process.
	var cmdline []string
	var cmdlineRead bool
	needCmdline := f.NeedsCmdline() ||
		!havePrev || !cfg.Scan.CacheCmdline || prev.Name == ""
	if needCmdline {
		cmdline, err = procfs.ReadCmdline(procPath, pid)
		if err != nil {
			if procfs.IsVanished(err) {
				res.Vanished++
				return nil
			}
			// A denied or malformed cmdline is not fatal; the namer
			// falls back to comm.
			res.ReadErrs["cmdline"]++
		}
		cmdlineRead = true
	}

	// The user is needed by the filter only when an ignore_users rule
	// exists, but it is needed by the group key always, so it is
	// resolved here from the cache when possible.
	var uid uint32
	var userName string
	var haveStatus bool
	var status *procfs.Status

	if havePrev && cfg.Scan.CacheCmdline && prev.User != "" {
		uid, userName = prev.UID, prev.User
	}

	if cfg.Scan.ReadStatus || userName == "" {
		status, err = procfs.ReadStatus(procPath, pid)
		if err != nil {
			if procfs.IsVanished(err) {
				res.Vanished++
				return nil
			}
			res.ReadErrs["status"]++
		} else {
			haveStatus = true
			uid = status.UID
			if userName == "" {
				if cfg.Naming.ResolveUsers {
					userName = s.deps.Users.Name(uid)
				} else {
					userName = uidString(uid)
				}
			}
		}
	}

	// The filter runs as early as it can. When no rule needs the
	// command line, an ignored process has cost exactly one small read.
	isKernel := cmdlineRead && filter.IsKernelThread(cmdline, st.PPID)
	keep, reason := f.Keep(filter.Input{
		Comm:     st.Comm,
		Cmdline:  cmdline,
		User:     userName,
		PPID:     st.PPID,
		IsKernel: isKernel,
	})
	if !keep {
		res.Ignored++
		// The entry is stamped rather than dropped, so an ignored
		// process is not rebuilt from scratch on every scan.
		s.state.Touch(pid)
		if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			slog.Debug("process ignored", "comm", st.Comm, "reason", reason)
		}
		return nil
	}

	name := ""
	if havePrev && cfg.Scan.CacheCmdline && prev.Name != "" {
		name = prev.Name
	} else {
		exe, _ := procfs.ReadExe(procPath, pid)
		name = n.Name(namer.Input{Comm: st.Comm, Exe: exe, Cmdline: cmdline})
	}
	if userName == "" {
		userName = uidString(uid)
	}

	sys := s.deps.System
	p := &Process{
		PID:        pid,
		PPID:       st.PPID,
		Comm:       st.Comm,
		Name:       name,
		User:       userName,
		UID:        uid,
		State:      st.State,
		StartTicks: st.StartTicks,
		StartUnix:  sys.StartTimeUnix(st.StartTicks),
		Threads:    st.Threads,
		VSizeBytes: st.VSizeBytes,
		RSSBytes:   sys.PagesToBytes(st.RSSPages),
		OpenFDs:    -1,
		MaxFDs:     -1,
		HaveStatus: haveStatus,
	}

	// statm supersedes the RSS figure from stat and adds the shared and
	// private-writable breakdown, which is the closer approximation to
	// unshared memory once the group sum over-counts shared pages.
	if sm, err := procfs.ReadStatm(procPath, pid); err == nil {
		p.RSSBytes = sys.PagesToBytes(sm.ResidentPages)
		p.VSizeBytes = sys.PagesToBytes(sm.SizePages)
		p.SharedBytes = sys.PagesToBytes(sm.SharedPages)
		p.DataBytes = sys.PagesToBytes(sm.DataPages)
	} else if procfs.IsVanished(err) {
		res.Vanished++
		return nil
	} else {
		res.ReadErrs["statm"]++
	}

	var io *procfs.IO
	if cfg.Scan.ReadIO {
		io, err = procfs.ReadIO(procPath, pid)
		if err != nil {
			if procfs.IsVanished(err) {
				res.Vanished++
				return nil
			}
			if !procfs.IsDenied(err) {
				res.ReadErrs["io"]++
			}
		} else {
			p.HaveIO = true
		}
	}

	// Descriptor counting is the dominant cost in the scan, so it runs
	// on its own schedule. Between fd scans the cached values are
	// reported, which keeps the gauge steady rather than dropping to
	// zero on the scans that skip the walk.
	if doFDs {
		if fds, err := procfs.CountFDs(procPath, pid); err == nil {
			p.OpenFDs = fds
			p.HaveFDs = true
			if max, err := procfs.MaxFDs(procPath, pid); err == nil {
				p.MaxFDs = max
			}
		} else if procfs.IsVanished(err) {
			res.Vanished++
			return nil
		} else if !procfs.IsDenied(err) {
			res.ReadErrs["fd"]++
		}
	} else if havePrev && prev.HaveFDs {
		p.OpenFDs = prev.OpenFDs
		p.MaxFDs = prev.MaxFDs
		p.HaveFDs = true
	}

	// Assemble the new state, then difference it against the old.
	cur := &PIDState{
		StartTicks:  st.StartTicks,
		UTimeTicks:  st.UTimeTicks,
		STimeTicks:  st.STimeTicks,
		MinorFaults: st.MinorFaults,
		MajorFaults: st.MajorFaults,
		Name:        name,
		User:        userName,
		UID:         uid,
		OpenFDs:     p.OpenFDs,
		MaxFDs:      p.MaxFDs,
		HaveFDs:     p.HaveFDs,
	}
	if haveStatus {
		cur.VolCtxSw = status.VolCtxSw
		cur.InvolCtxSw = status.InvolCtxSw
	}
	if p.HaveIO {
		cur.ReadBytes = io.ReadBytes
		cur.WriteBytes = io.WriteBytes
	}

	if havePrev {
		computeDeltas(p, cur, prev)
		p.HaveDeltas = true
	}
	// A process with no previous state contributes no deltas this scan.
	// Its state is recorded and it contributes from the next scan
	// onward. This is why the first scan after start publishes gauges
	// but no counter values.

	s.state.Put(pid, cur)
	return p
}

// computeDeltas differences the counters against the previous state and
// writes the results into p.
func computeDeltas(p *Process, cur, prev *PIDState) {
	p.DUTimeTicks = Delta(cur.UTimeTicks, prev.UTimeTicks)
	p.DSTimeTicks = Delta(cur.STimeTicks, prev.STimeTicks)
	p.DMinorFaults = Delta(cur.MinorFaults, prev.MinorFaults)
	p.DMajorFaults = Delta(cur.MajorFaults, prev.MajorFaults)
	p.DVolCtxSw = Delta(cur.VolCtxSw, prev.VolCtxSw)
	p.DInvolCtxSw = Delta(cur.InvolCtxSw, prev.InvolCtxSw)
	p.DReadBytes = Delta(cur.ReadBytes, prev.ReadBytes)
	p.DWriteBytes = Delta(cur.WriteBytes, prev.WriteBytes)
}

// shouldScanFDs reports whether this scan reads the descriptor count.
//
// Enumerating /proc/<pid>/fd requires the kernel to walk the file
// descriptor table, which is the dominant cost in the whole scan.
// Descriptor counts change slowly enough that a refresh every fourth
// scan is adequate, while CPU needs every scan to be useful.
func (s *Scanner) shouldScanFDs(cfg *config.Config) bool {
	every := cfg.Scan.FDScanEvery
	if every <= 1 {
		return true
	}
	return s.scanNo%uint64(every) == 1 || s.scanNo == 1
}

// selfCPU returns the CPU seconds this exporter process has consumed,
// user plus system.
//
// Publishing this lets an operator verify directly that the low-cost
// requirement is being met, rather than inferring it from an external
// tool.
func selfCPU() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	u := float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6
	sys := float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
	return u + sys
}

// uidString renders a UID when name resolution is off or unavailable.
func uidString(uid uint32) string {
	return "uid:" + itoa(int(uid))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// keep runtime referenced for GOMAXPROCS-aware tuning notes in logs.
var _ = runtime.NumCPU

