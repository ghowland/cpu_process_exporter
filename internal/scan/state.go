// Package scan walks /proc in batches and assembles per-process
// readings.
package scan

// PIDState is the memory between scans that makes counter deltas
// possible. Without it, a counter read gives only a lifetime total,
// which is not what anyone wants to see.
//
// The state is per-PID even though the output is per-group, because a
// counter belongs to one process. Summing lifetime totals across a
// group and differencing the sum would go negative the moment one
// member exits.
type PIDState struct {
	StartTicks uint64 // identity anchor; a mismatch means the PID was reused
	Generation uint64 // scan number when last seen

	UTimeTicks  uint64
	STimeTicks  uint64
	MinorFaults uint64
	MajorFaults uint64
	VolCtxSw    uint64
	InvolCtxSw  uint64
	ReadBytes   uint64
	WriteBytes  uint64

	// Cached immutable values. A process's command line, executable,
	// UID, and therefore its derived group name do not change for its
	// lifetime, so they are read once and reused. This removes one read
	// and one namer evaluation per process per scan for every
	// long-lived process, which is the large majority.
	Name string
	User string
	UID  uint32

	// Cached descriptor values, refreshed only on an fd scan. Between
	// fd scans the previous values are reported, so the gauge does not
	// drop to zero on the three scans out of four that skip the walk.
	OpenFDs int
	MaxFDs  int
	HaveFDs bool
}

// StateMap holds the per-PID state. It is owned by the scanner and is
// not accessed concurrently, because two scans never run at once.
type StateMap struct {
	m   map[int]*PIDState
	gen uint64
}

// NewStateMap creates an empty StateMap.
func NewStateMap() *StateMap {
	return &StateMap{m: make(map[int]*PIDState, 512)}
}

// NextGeneration increments the generation counter and returns the new
// value. Every PID observed during a scan is stamped with it.
func (s *StateMap) NextGeneration() uint64 {
	s.gen++
	return s.gen
}

// Generation returns the current generation.
func (s *StateMap) Generation() uint64 { return s.gen }

// Lookup returns the state for a PID, and whether it belongs to the
// same process.
//
// The start time is the identity check. The kernel wraps PIDs, so an
// old entry can be matched against a new process holding the same
// number. That process's counters start near zero, so the delta would
// compute as a large negative number. A start time mismatch means a
// different process, and the entry is treated as absent.
func (s *StateMap) Lookup(pid int, startTicks uint64) (*PIDState, bool) {
	st, ok := s.m[pid]
	if !ok {
		return nil, false
	}
	if st.StartTicks != startTicks {
		// PID reuse. Drop the stale entry so the new process starts
		// with a clean baseline rather than a negative delta.
		delete(s.m, pid)
		return nil, false
	}
	return st, true
}

// Put stores or updates the state for a PID and stamps it with the
// current generation.
func (s *StateMap) Put(pid int, st *PIDState) {
	st.Generation = s.gen
	s.m[pid] = st
}

// Touch stamps an existing entry with the current generation without
// otherwise modifying it. It is used for processes that were filtered
// out, so that an ignored process does not have its state pruned and
// rebuilt on every scan.
func (s *StateMap) Touch(pid int) {
	if st, ok := s.m[pid]; ok {
		st.Generation = s.gen
	}
}

// Prune deletes every entry whose generation is older than the current
// one, and returns the number deleted.
//
// This is what bounds the map. Without it the map grows every time a
// new process appears and never shrinks, so a machine that forks
// constantly would leak indefinitely. With it, the map size tracks the
// number of live processes, which is what makes the exporter safe to
// run for months without a restart.
func (s *StateMap) Prune() int {
	n := 0
	for pid, st := range s.m {
		if st.Generation < s.gen {
			delete(s.m, pid)
			n++
		}
	}
	return n
}

// Len returns the entry count, exported as a gauge so that unbounded
// growth is visible.
func (s *StateMap) Len() int { return len(s.m) }

// Delta computes now minus previous, clamped at zero.
//
// The clamp is a second line of defence behind the start time check. A
// negative delta means either PID reuse the check missed or a counter
// that went backwards, and neither should reach a Prometheus counter.
func Delta(now, prev uint64) uint64 {
	if now < prev {
		return 0
	}
	return now - prev
}

