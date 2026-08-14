package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stat is the parsed content of /proc/<pid>/stat. It is the cheapest
// and most valuable read: one small file gives CPU, state, threads,
// faults, and the start time that anchors process identity across PID
// reuse.
type Stat struct {
	PID         int
	Comm        string
	State       byte
	PPID        int
	MinorFaults uint64
	MajorFaults uint64
	UTimeTicks  uint64
	STimeTicks  uint64
	Threads     int
	StartTicks  uint64
	VSizeBytes  uint64
	RSSPages    uint64
}

// ReadStat parses /proc/<pid>/stat.
func ReadStat(procPath string, pid int) (*Stat, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st, err := parseStat(data)
	if err != nil {
		return nil, fmt.Errorf("procfs: parse %s: %w", path, err)
	}
	return st, nil
}

// parseStat extracts the wanted fields.
//
// The comm field is wrapped in parentheses and may itself contain
// spaces and parentheses, so a naive split on spaces produces wrong
// field offsets for any process whose name contains a space. The parser
// therefore locates the final closing parenthesis and splits only the
// remainder.
func parseStat(data []byte) (*Stat, error) {
	s := string(data)

	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return nil, fmt.Errorf("malformed comm field")
	}

	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return nil, fmt.Errorf("malformed pid field: %w", err)
	}

	st := &Stat{PID: pid, Comm: s[open+1 : close]}

	// Fields after comm, numbered as in proc(5) starting at field 3.
	rest := strings.Fields(s[close+1:])
	if len(rest) < 20 {
		return nil, fmt.Errorf("expected at least 20 fields after comm, got %d", len(rest))
	}

	// rest[0] is field 3 (state), so field N is rest[N-3].
	field := func(n int) string {
		i := n - 3
		if i < 0 || i >= len(rest) {
			return ""
		}
		return rest[i]
	}
	u64 := func(n int) uint64 {
		v, _ := strconv.ParseUint(field(n), 10, 64)
		return v
	}
	i := func(n int) int {
		v, _ := strconv.Atoi(field(n))
		return v
	}

	if v := field(3); v != "" {
		st.State = v[0]
	}
	st.PPID = i(4)
	st.MinorFaults = u64(10)
	st.MajorFaults = u64(12)
	st.UTimeTicks = u64(14)
	st.STimeTicks = u64(15)
	st.Threads = i(20)
	st.StartTicks = u64(22)
	st.VSizeBytes = u64(23)
	st.RSSPages = u64(24)

	return st, nil
}

