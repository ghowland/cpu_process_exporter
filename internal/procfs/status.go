package procfs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Status holds the fields taken from /proc/<pid>/status. Only the UID
// and the context switch counters are extracted; the memory fields
// duplicate statm and are not read twice.
type Status struct {
	UID        uint32
	GID        uint32
	VolCtxSw   uint64
	InvolCtxSw uint64

	// SwapBytes comes from VmSwap. It costs nothing extra, because
	// this file is already open and parsed.
	SwapBytes uint64
}

// ReadStatus parses /proc/<pid>/status.
//
// The parser stops once every wanted field has been found. The UID
// appears near the top of the file, but the context switch counters are
// the last two lines, so the early exit only helps when context
// switches are not wanted. It is kept because it costs nothing.
func ReadStatus(procPath string, pid int) (*Status, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "status")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	st := &Status{}
	found := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		val := strings.TrimSpace(line[colon+1:])

		switch key {
		case "Uid":
			// Four values: real, effective, saved set, filesystem. The
			// real UID is the owner for grouping purposes.
			if fields := strings.Fields(val); len(fields) > 0 {
				if v, err := strconv.ParseUint(fields[0], 10, 32); err == nil {
					st.UID = uint32(v)
					found++
				}
			}
		case "Gid":
			if fields := strings.Fields(val); len(fields) > 0 {
				if v, err := strconv.ParseUint(fields[0], 10, 32); err == nil {
					st.GID = uint32(v)
					found++
				}
			}
		case "voluntary_ctxt_switches":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				st.VolCtxSw = v
				found++
			}
		case "nonvoluntary_ctxt_switches":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				st.InvolCtxSw = v
				found++
			}
		case "VmSwap":
			// Reported in kilobytes with a kB suffix.
			if fields := strings.Fields(val); len(fields) > 0 {
				if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
					st.SwapBytes = v * 1024
					found++
				}
			}
		}
		if found == 5 {
			break
		}

	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return st, nil
}
