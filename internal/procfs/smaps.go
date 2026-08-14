package procfs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Smaps holds the proportional memory figures.
//
// Proportional set size divides each shared page by the number of
// processes sharing it, so a group sum is the true physical footprint.
// Resident set size counts every shared page in full for each member,
// so a group sum over-counts and can exceed physical memory.
type Smaps struct {
	PSSBytes     uint64
	SwapPSSBytes uint64
}

// HaveSmapsRollup reports whether /proc/self/smaps_rollup exists.
//
// smaps_rollup arrived in kernel 4.14 and is one read of about twenty
// lines. The fallback, smaps, has one entry per memory mapping, which
// for a JVM can be hundreds of entries and thousands of lines. The
// check runs once at start rather than per process.
func HaveSmapsRollup(procPath string) bool {
	_, err := os.Stat(filepath.Join(procPath, "self", "smaps_rollup"))
	return err == nil
}

// ReadSmaps reads the proportional figures for one process.
//
// It requires the same privilege as /proc/<pid>/io: a matching UID or
// CAP_SYS_PTRACE. An unprivileged exporter gets a permission error for
// processes it does not own, which the caller reports as coverage
// rather than treating as a failure.
func ReadSmaps(procPath string, pid int, useRollup bool) (*Smaps, error) {
	name := "smaps"
	if useRollup {
		name = "smaps_rollup"
	}
	path := filepath.Join(procPath, strconv.Itoa(pid), name)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := &Smaps{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8192), 256*1024)

	for sc.Scan() {
		line := sc.Text()
		// Both files report in kilobytes with a "kB" suffix. smaps
		// repeats these keys per mapping, so the values accumulate;
		// smaps_rollup reports each once.
		switch {
		case strings.HasPrefix(line, "Pss:"):
			out.PSSBytes += parseKB(line[4:])
		case strings.HasPrefix(line, "SwapPss:"):
			out.SwapPSSBytes += parseKB(line[8:])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseKB extracts a kilobyte value and returns bytes.
func parseKB(s string) uint64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0
	}
	return v * 1024
}

