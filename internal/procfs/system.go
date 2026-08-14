// Package procfs holds the low-level readers for each /proc file. Each
// reader is a stateless function, so the scanner controls when and how
// often each is called.
package procfs

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// System holds the constants that convert raw /proc values into real
// units. They are read once at start rather than assumed, because the
// clock tick rate is a kernel build option.
type System struct {
	ClockTicks int64
	PageSize   int64
	BootTime   float64
	ProcPath   string

	// SmapsRollup reports whether the cheap single-read form is
	// available. Detected once at start, because probing per process
	// would cost one stat per process per scan.
	SmapsRollup bool
}

// NewSystem reads the system constants. It fails only when /proc is
// absent or unreadable, which means the exporter cannot function.
func NewSystem(procPath string) (*System, error) {
	if procPath == "" {
		procPath = "/proc"
	}
	bt, err := bootTime(procPath)
	if err != nil {
		return nil, fmt.Errorf("procfs: read boot time: %w", err)
	}
	return &System{
		ClockTicks:  clockTicks(),
		PageSize:    int64(os.Getpagesize()),
		BootTime:    bt,
		ProcPath:    procPath,
		SmapsRollup: HaveSmapsRollup(procPath),
	}, nil
}

// TicksToSeconds converts a CPU tick count to seconds.
func (s *System) TicksToSeconds(ticks uint64) float64 {
	if s.ClockTicks <= 0 {
		return 0
	}
	return float64(ticks) / float64(s.ClockTicks)
}

// PagesToBytes converts a page count to bytes.
func (s *System) PagesToBytes(pages uint64) uint64 {
	return pages * uint64(s.PageSize)
}

// StartTimeUnix converts a process start time in ticks since boot to
// unix seconds.
func (s *System) StartTimeUnix(startTicks uint64) float64 {
	return s.BootTime + s.TicksToSeconds(startTicks)
}

// clockTicks returns sysconf(_SC_CLK_TCK). Go has no sysconf binding,
// so the value is obtained from getconf when available. Every Linux
// kernel in practical use has 100, which is the fallback.
func clockTicks() int64 {
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

// bootTime parses the btime line from /proc/stat, which gives the unix
// time at which the system booted.
func bootTime(procPath string) (float64, error) {
	f, err := os.Open(filepath.Join(procPath, "stat"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[6:]), 64)
		if err != nil {
			return 0, err
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("btime not found in %s/stat", procPath)
}

// UserCache resolves UIDs to names. Container-created UIDs have no
// passwd entry on the host, so an unresolvable UID renders as uid:NNNN
// rather than failing.
type UserCache struct {
	mu       sync.RWMutex
	byUID    map[uint32]string
	loadedAt time.Time
}

// NewUserCache builds the cache.
func NewUserCache() *UserCache {
	c := &UserCache{byUID: make(map[uint32]string)}
	c.Refresh()
	return c
}

// Name returns the user name for a UID, or uid:NNNN when unknown. A
// successful lookup for a UID absent from the initial load is cached,
// so a newly created account resolves without a reload.
func (c *UserCache) Name(uid uint32) string {
	c.mu.RLock()
	name, ok := c.byUID[uid]
	c.mu.RUnlock()
	if ok {
		return name
	}

	fallback := "uid:" + strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil && u.Username != "" {
		fallback = u.Username
	}

	c.mu.Lock()
	c.byUID[uid] = fallback
	c.mu.Unlock()
	return fallback
}

// Refresh rebuilds the cache from /etc/passwd, called on configuration
// reload.
func (c *UserCache) Refresh() {
	m := make(map[uint32]string)

	f, err := os.Open("/etc/passwd")
	if err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, ":")
			if len(parts) < 3 {
				continue
			}
			uid, err := strconv.ParseUint(parts[2], 10, 32)
			if err != nil {
				continue
			}
			m[uint32(uid)] = parts[0]
		}
		_ = f.Close()
	}

	c.mu.Lock()
	c.byUID = m
	c.loadedAt = time.Now()
	c.mu.Unlock()
}
