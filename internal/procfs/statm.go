package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Statm is the parsed content of /proc/<pid>/statm, in pages. It is a
// seven-field single-line file and is cheaper to read and parse than
// the equivalent memory fields of status.
type Statm struct {
	SizePages     uint64
	ResidentPages uint64
	SharedPages   uint64
	TextPages     uint64
	DataPages     uint64
}

// ReadStatm parses /proc/<pid>/statm.
func ReadStatm(procPath string, pid int) (*Statm, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "statm")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(data))
	if len(f) < 6 {
		return nil, fmt.Errorf("procfs: %s: expected 7 fields, got %d", path, len(f))
	}
	u := func(i int) uint64 {
		v, _ := strconv.ParseUint(f[i], 10, 64)
		return v
	}
	return &Statm{
		SizePages:     u(0),
		ResidentPages: u(1),
		SharedPages:   u(2),
		TextPages:     u(3),
		DataPages:     u(5),
	}, nil
}

