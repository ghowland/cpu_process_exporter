package procfs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IO holds the byte counters from /proc/<pid>/io.
type IO struct {
	ReadBytes  uint64
	WriteBytes uint64
	SyscR      uint64
	SyscW      uint64
}

// ReadIO parses /proc/<pid>/io.
//
// This file requires a matching UID or CAP_SYS_PTRACE. An unprivileged
// exporter gets a permission error for every process it does not own,
// which is the normal case rather than an exceptional one. The caller
// distinguishes unknown from zero by checking the error rather than by
// reading a zero value, because reporting zero would be a lie.
func ReadIO(procPath string, pid int) (*IO, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "io")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	io := &IO{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		val, err := strconv.ParseUint(strings.TrimSpace(line[colon+1:]), 10, 64)
		if err != nil {
			continue
		}
		switch line[:colon] {
		case "read_bytes":
			io.ReadBytes = val
		case "write_bytes":
			io.WriteBytes = val
		case "syscr":
			io.SyscR = val
		case "syscw":
			io.SyscW = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return io, nil
}

