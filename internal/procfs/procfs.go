package procfs

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// ListPIDs returns every numeric directory name under /proc.
//
// Readdirnames is used rather than ReadDir so that no stat is performed
// per entry. On a machine with two thousand processes, ReadDir would
// issue two thousand extra syscalls per scan for information that is
// never used: a numeric name under /proc is a process directory by
// definition.
func ListPIDs(procPath string) ([]int, error) {
	f, err := os.Open(procPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	pids := make([]int, 0, len(names))
	for _, n := range names {
		// A fast reject before the parse: every process directory name
		// begins with a digit, and nothing else under /proc does.
		if len(n) == 0 || n[0] < '0' || n[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(n)
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// IsVanished reports whether an error means the process exited between
// the directory listing and the read.
//
// A process exiting mid-scan is the normal case at any real turnover
// rate, not an error condition, so the scanner skips such processes
// silently rather than logging. An exporter that logged on every
// process exit would be worse than useless.
func IsVanished(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A read of a /proc file for a process that exits mid-read returns
	// ESRCH as an EIO or a short read on some kernels.
	return errors.Is(err, syscall.EIO)
}

// IsDenied reports whether an error means the exporter lacks permission
// for this file.
//
// The scanner records the omission rather than reporting a zero value,
// because zero and unknown are different facts: a group that does no
// I/O and a group whose I/O cannot be seen must not look identical.
func IsDenied(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM)
}

