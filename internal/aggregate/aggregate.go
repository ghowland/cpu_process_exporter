// Package aggregate turns per-process readings into per-group series.
package aggregate

import (
	"sync"
	"time"

	"github.com/example/process_exporter/internal/procfs"
	"github.com/example/process_exporter/internal/scan"
)

// Key identifies one exported series set.
//
// It contains no PID. A PID label would create a new series on every
// process start, so a machine forking once a second would produce
// eighty-six thousand series a day, each living in memory until it aged
// out. The name is bounded by the machine's software inventory, and the
// user separates a master from its workers.
type Key struct {
	Name string `json:"name"`
	User string `json:"user"`
}

// Accum holds the monotonic totals for one group.
//
// A Prometheus counter must increase monotonically; any decrease is
// read as a reset and the interval is discarded. If the exported value
// were the sum of the live members' lifetime totals, it would drop
// every time a worker exited. The accumulator instead adds each scan's
// summed deltas, so it only ever increases regardless of process churn.
type Accum struct {
	UTimeSeconds float64 `json:"utime_seconds"`
	STimeSeconds float64 `json:"stime_seconds"`
	MinorFaults  uint64  `json:"minor_faults"`
	MajorFaults  uint64  `json:"major_faults"`
	VolCtxSw     uint64  `json:"voluntary_ctx_switches"`
	InvolCtxSw   uint64  `json:"involuntary_ctx_switches"`
	ReadBytes    uint64  `json:"read_bytes"`
	WriteBytes   uint64  `json:"write_bytes"`

	LastSeen time.Time `json:"last_seen"`
}

// Sample is the exported state of one group at one instant.
type Sample struct {
	Key Key `json:"key"`

	NumProcs    int            `json:"num_procs"`
	Threads     int            `json:"threads"`
	RSSBytes    uint64         `json:"rss_bytes"`
	VSizeBytes  uint64         `json:"vsize_bytes"`
	SharedBytes uint64         `json:"shared_bytes"`
	DataBytes   uint64         `json:"data_bytes"`
	SwapBytes   uint64         `json:"swap_bytes"`
	OpenFDs     int            `json:"open_fds"`
	MaxFDs      int            `json:"max_fds"`
	OldestStart float64        `json:"oldest_start"`
	States      map[byte]int   `json:"-"`
	StateNames  map[string]int `json:"states"`

	// WorstFDRatio is the maximum of the per-process ratios, not a
	// ratio of the group sums. A group of forty workers where one is
	// at its limit dilutes a sum ratio by a factor of forty, so the
	// sum form cannot raise an alert on the case it exists for.
	WorstFDRatio float64 `json:"worst_fd_ratio"`

	// Proportional set size. Summed across the group this is the true
	// physical footprint, because each shared page is divided by the
	// number of processes sharing it. RSSBytes counts each shared page
	// once per member and therefore over-counts.
	PSSBytes     uint64 `json:"pss_bytes"`
	SwapPSSBytes uint64 `json:"swap_pss_bytes"`

	Accum Accum `json:"accum"`

	// Coverage, so partial data is visible rather than silent. A ratio
	// below one means the total is a lower bound, which distinguishes
	// "this group does no I/O" from "I cannot see this group's I/O".
	FDsRead int `json:"fds_read"`
	IORead  int `json:"io_read"`
	PSSRead int `json:"pss_read"`
}

// FDCoverage returns the fraction of members whose descriptor count was
// read.
func (s *Sample) FDCoverage() float64 {
	if s.NumProcs == 0 {
		return 0
	}
	return float64(s.FDsRead) / float64(s.NumProcs)
}

// IOCoverage returns the fraction of members whose io file was readable.
func (s *Sample) IOCoverage() float64 {
	if s.NumProcs == 0 {
		return 0
	}
	return float64(s.IORead) / float64(s.NumProcs)
}

// PSSCoverage returns the fraction of members whose smaps was readable.
//
// smaps needs the same privilege as io, so an unprivileged exporter
// gets proportional memory only for its own processes. Below one, the
// PSS total is a lower bound.
func (s *Sample) PSSCoverage() float64 {
	if s.NumProcs == 0 {
		return 0
	}
	return float64(s.PSSRead) / float64(s.NumProcs)
}

// Snapshot is one complete scan result, published atomically.
type Snapshot struct {
	Groups   map[Key]*Sample `json:"-"`
	List     []*Sample       `json:"groups"`
	ScanAt   time.Time       `json:"scan_at"`
	Duration time.Duration   `json:"duration"`

	ProcsTotal    int            `json:"procs_total"`
	ProcsScanned  int            `json:"procs_scanned"`
	ProcsIgnored  int            `json:"procs_ignored"`
	ProcsVanished int            `json:"procs_vanished"`
	ProcsDenied   int            `json:"procs_denied"`
	ReadErrs      map[string]int `json:"read_errors"`

	StateEntries int     `json:"state_entries"`
	SelfCPU      float64 `json:"self_cpu_seconds"`
	NamesSeen    int     `json:"names_seen"`
	ScanNumber   uint64  `json:"scan_number"`
}

// Aggregator holds the group accumulators across scans and produces a
// snapshot from each scan result.
type Aggregator struct {
	mu        sync.Mutex
	accums    map[Key]*Accum
	namesSeen map[string]struct{}
	system    *procfs.System
	scans     uint64
}

// New creates an Aggregator.
func New(sys *procfs.System) *Aggregator {
	return &Aggregator{
		accums:    make(map[Key]*Accum, 128),
		namesSeen: make(map[string]struct{}, 128),
		system:    sys,
	}
}

// Apply folds one scan result into the accumulators and returns the
// snapshot to publish.
func (a *Aggregator) Apply(res *scan.Result, retention time.Duration) *Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.scans++
	now := res.ScanAt
	if now.IsZero() {
		now = time.Now()
	}

	snap := &Snapshot{
		Groups:        make(map[Key]*Sample, len(a.accums)+16),
		ScanAt:        now,
		Duration:      res.Duration,
		ProcsTotal:    res.Total,
		ProcsScanned:  res.Scanned,
		ProcsIgnored:  res.Ignored,
		ProcsVanished: res.Vanished,
		ProcsDenied:   res.Denied,
		ReadErrs:      res.ReadErrs,
		StateEntries:  res.StateEntries,
		SelfCPU:       res.SelfCPU,
		ScanNumber:    a.scans,
	}

	for i := range res.Procs {
		p := &res.Procs[i]
		k := Key{Name: p.Name, User: p.User}
		a.namesSeen[p.Name] = struct{}{}

		s := snap.Groups[k]
		if s == nil {
			s = &Sample{
				Key:         k,
				States:      make(map[byte]int, 4),
				OldestStart: p.StartUnix,
				OpenFDs:     0,
				MaxFDs:      0,
			}
			snap.Groups[k] = s
		}

		s.NumProcs++
		s.Threads += p.Threads
		s.RSSBytes += p.RSSBytes
		s.VSizeBytes += p.VSizeBytes
		s.SharedBytes += p.SharedBytes
		s.DataBytes += p.DataBytes
		s.SwapBytes += p.SwapBytes
		s.States[p.State]++

		if p.HavePSS {
			s.PSSBytes += p.PSSBytes
			s.SwapPSSBytes += p.SwapPSSBytes
			s.PSSRead++
		}

		// The oldest member gives the group age, which is what changes
		// when a service restarts.
		if p.StartUnix > 0 && (s.OldestStart == 0 || p.StartUnix < s.OldestStart) {
			s.OldestStart = p.StartUnix
		}

		if p.HaveFDs && p.OpenFDs >= 0 {
			s.OpenFDs += p.OpenFDs
			s.FDsRead++
			if p.MaxFDs > 0 {
				s.MaxFDs += p.MaxFDs
			}
			// The maximum, not a ratio of the sums. This is what makes
			// a single member near its limit visible in a large group.
			if p.FDRatio > s.WorstFDRatio {
				s.WorstFDRatio = p.FDRatio
			}
		}

		if p.HaveIO {
			s.IORead++
		}

		a.accumulate(k, p, now)
	}

	// Copy the accumulator into every live group, and materialise the
	// state map with readable names for the JSON view.
	for k, s := range snap.Groups {
		if acc := a.accums[k]; acc != nil {
			s.Accum = *acc
		}
		s.StateNames = make(map[string]int, len(s.States))
		for st, n := range s.States {
			s.StateNames[stateName(st)] = n
		}
	}

	a.prune(now, retention)
	snap.NamesSeen = len(a.namesSeen)

	snap.List = make([]*Sample, 0, len(snap.Groups))
	for _, s := range snap.Groups {
		snap.List = append(snap.List, s)
	}
	return snap
}

// accumulate adds one process's deltas into its group's accumulator.
//
// The deltas were computed per PID inside the scanner. Summing them
// here rather than summing lifetime totals is what keeps the exported
// counter monotonic across process churn.
func (a *Aggregator) accumulate(k Key, p *scan.Process, now time.Time) {
	acc := a.accums[k]
	if acc == nil {
		acc = &Accum{}
		a.accums[k] = acc
	}
	acc.LastSeen = now

	if !p.HaveDeltas {
		return
	}
	acc.UTimeSeconds += a.system.TicksToSeconds(p.DUTimeTicks)
	acc.STimeSeconds += a.system.TicksToSeconds(p.DSTimeTicks)
	acc.MinorFaults += p.DMinorFaults
	acc.MajorFaults += p.DMajorFaults
	if p.HaveStatus {
		acc.VolCtxSw += p.DVolCtxSw
		acc.InvolCtxSw += p.DInvolCtxSw
	}
	if p.HaveIO {
		acc.ReadBytes += p.DReadBytes
		acc.WriteBytes += p.DWriteBytes
	}
}

// prune removes accumulators for groups not seen within the retention
// window.
//
// Accumulators are not pruned on the same schedule as PID state. A
// group whose processes all exit and restart minutes later must resume
// its counter where it left off, or Prometheus sees a reset. Beyond the
// retention window the group is forgotten, on the assumption that the
// program was uninstalled or permanently stopped.
func (a *Aggregator) prune(now time.Time, retention time.Duration) int {
	if retention <= 0 {
		return 0
	}
	n := 0
	for k, acc := range a.accums {
		if now.Sub(acc.LastSeen) > retention {
			delete(a.accums, k)
			n++
		}
	}
	return n
}

// NamesSeen returns the count of distinct group names ever observed.
//
// A large gap between this and the current group count indicates churn:
// many names appearing briefly and disappearing, which is the signature
// of a naming rule extracting an unbounded value such as a PID, a
// container ID, or a timestamp.
func (a *Aggregator) NamesSeen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.namesSeen)
}

// Reset clears every accumulator, called when a naming configuration
// change makes the existing keys meaningless.
func (a *Aggregator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accums = make(map[Key]*Accum, 128)
	a.namesSeen = make(map[string]struct{}, 128)
}

// stateName renders a process state letter as a readable label value.
func stateName(s byte) string {
	switch s {
	case 'R':
		return "running"
	case 'S':
		return "sleeping"
	case 'D':
		return "disk_sleep"
	case 'Z':
		return "zombie"
	case 'T':
		return "stopped"
	case 't':
		return "tracing_stop"
	case 'X', 'x':
		return "dead"
	case 'I':
		return "idle"
	case 'K':
		return "wakekill"
	case 'W':
		return "waking"
	case 'P':
		return "parked"
	default:
		return "unknown"
	}
}
