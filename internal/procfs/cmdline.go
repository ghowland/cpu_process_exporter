package procfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ReadCmdline reads /proc/<pid>/cmdline and splits it on NUL.
//
// An empty result identifies a kernel thread, which has no argv. This
// is the primary kernel-thread test, because it needs no ancestry walk
// and no extra read.
func ReadCmdline(procPath string, pid int) ([]string, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "cmdline")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	// The content is NUL-separated and usually NUL-terminated, so the
	// trailing empty field is trimmed before splitting.
	s := strings.TrimRight(string(data), "\x00")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// ReadExe resolves the /proc/<pid>/exe symlink.
//
// It fails for kernel threads, which have no executable, and for
// processes owned by another user when unprivileged. Both are expected,
// so the caller treats an error as an absent value rather than a
// failure.
func ReadExe(procPath string, pid int) (string, error) {
	path := filepath.Join(procPath, strconv.Itoa(pid), "exe")
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	// A deleted executable renders as "/path/to/bin (deleted)".
	if i := strings.Index(target, " (deleted)"); i > 0 {
		target = target[:i]
	}
	return target, nil
}

