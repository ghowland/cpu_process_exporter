package procfs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CountFDs counts the entries in /proc/<pid>/fd.
//
// This is the most expensive read in the system. The kernel must walk
// the process file descriptor table and produce a directory entry for
// each open descriptor, so a process holding ten thousand descriptors
// costs more than a hundred stat reads. The scanner therefore calls
// this every Nth scan rather than every scan; descriptor counts change
// slowly enough that a refresh every minute is adequate, while CPU
// needs every scan to be useful.
//
// Readdirnames is used rather than ReadDir, because the names alone are
// sufficient for a count and stat-ing each entry would multiply the
// cost by the number of descriptors.
func CountFDs(procPath string, pid int) (int, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "fd")
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	total := 0
	for {
		names, err := f.Readdirnames(1024)
		total += len(names)
		if err != nil {
			// io.EOF ends the walk. Any other error means the process
			// exited mid-read, in which case the partial count is
			// returned rather than discarded.
			if len(names) == 0 && total == 0 {
				return 0, err
			}
			return total, nil
		}
		if len(names) == 0 {
			return total, nil
		}
	}
}

// MaxFDs reads the soft limit on open files from /proc/<pid>/limits.
//
// The value rarely changes, so it is read on the same schedule as the
// descriptor count. A limit of "unlimited" is returned as -1, which the
// aggregator treats as unknown rather than folding into the sum.
func MaxFDs(procPath string, pid int) (int, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "limits")
	f, err := os.Open(path)
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Max open files") {
			continue
		}
		// Columns are fixed width: name, soft, hard, units. The soft
		// limit is the first value after the name.
		rest := strings.Fields(line[len("Max open files"):])
		if len(rest) == 0 {
			return -1, nil
		}
		if rest[0] == "unlimited" {
			return -1, nil
		}
		v, err := strconv.Atoi(rest[0])
		if err != nil {
			return -1, nil
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, nil
}

