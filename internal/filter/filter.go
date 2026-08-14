// Package filter removes processes that carry no useful signal.
package filter

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/example/process_exporter/internal/config"
)

// Filter decides whether a process is exported. It is compiled once
// from configuration and is then read-only and safe for concurrent use.
type Filter struct {
	ignoreKernel bool

	ignoreComm   map[string]bool
	ignorePrefix []string
	ignoreRegex  []*regexp.Regexp
	ignoreCmd    []*regexp.Regexp
	ignoreUsers  map[string]bool
	includeOnly  []*regexp.Regexp

	needsCmdline bool
}

// New compiles a FilterConfig. Invalid patterns are rejected here
// rather than silently matching nothing at scan time.
func New(cfg config.FilterConfig) (*Filter, error) {
	f := &Filter{
		ignoreKernel: cfg.IgnoreKernelThreads,
		ignoreComm:   make(map[string]bool, len(cfg.IgnoreComm)),
		ignoreUsers:  make(map[string]bool, len(cfg.IgnoreUsers)),
		ignorePrefix: append([]string{}, cfg.IgnoreCommPrefix...),
	}
	for _, c := range cfg.IgnoreComm {
		f.ignoreComm[c] = true
	}
	for _, u := range cfg.IgnoreUsers {
		f.ignoreUsers[u] = true
	}
	for i, p := range cfg.IgnoreCommRegex {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("filter: ignore_comm_regex[%d] %q: %w", i, p, err)
		}
		f.ignoreRegex = append(f.ignoreRegex, re)
	}
	for i, p := range cfg.IgnoreCmdlineRegex {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("filter: ignore_cmdline_regex[%d] %q: %w", i, p, err)
		}
		f.ignoreCmd = append(f.ignoreCmd, re)
	}
	for i, p := range cfg.IncludeOnly {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("filter: include_only[%d] %q: %w", i, p, err)
		}
		f.includeOnly = append(f.includeOnly, re)
	}

	f.needsCmdline = cfg.NeedsCmdline()
	return f, nil
}

// NeedsCmdline reports whether the filter requires the command line
// before it can decide.
//
// When false, the scanner applies the filter after reading stat and
// before every other read, so an ignored process costs one small read
// rather than five reads and a directory walk. A configuration using
// ignore_cmdline_regex pays for cmdline on every process, which is
// documented so an operator can choose to avoid it.
func (f *Filter) NeedsCmdline() bool { return f.needsCmdline }

// Input is what the filter examines. Cmdline and User may be empty when
// the filter does not need them.
type Input struct {
	Comm     string
	Cmdline  []string
	User     string
	PPID     int
	IsKernel bool
}

// Keep reports whether the process is exported, and when it is not, the
// rule that excluded it. The reason is used for debug logging only and
// never becomes a metric label.
func (f *Filter) Keep(in Input) (bool, string) {
	// include_only runs first. When it is set, a process must match at
	// least one pattern to survive, and every rule below still applies
	// to what does.
	if len(f.includeOnly) > 0 {
		joined := in.Comm
		if len(in.Cmdline) > 0 {
			joined = strings.Join(in.Cmdline, " ")
		}
		hit := false
		for _, re := range f.includeOnly {
			if re.MatchString(in.Comm) || re.MatchString(joined) {
				hit = true
				break
			}
		}
		if !hit {
			return false, "include_only"
		}
	}

	if f.ignoreKernel && in.IsKernel {
		return false, "kernel_thread"
	}
	if f.ignoreComm[in.Comm] {
		return false, "ignore_comm"
	}
	for _, p := range f.ignorePrefix {
		if strings.HasPrefix(in.Comm, p) {
			return false, "ignore_comm_prefix"
		}
	}
	for _, re := range f.ignoreRegex {
		if re.MatchString(in.Comm) {
			return false, "ignore_comm_regex"
		}
	}
	if len(f.ignoreCmd) > 0 && len(in.Cmdline) > 0 {
		joined := strings.Join(in.Cmdline, " ")
		for _, re := range f.ignoreCmd {
			if re.MatchString(joined) {
				return false, "ignore_cmdline_regex"
			}
		}
	}
	if in.User != "" && f.ignoreUsers[in.User] {
		return false, "ignore_users"
	}
	return true, ""
}

// IsKernelThread reports whether a process is a kernel thread.
//
// Two signals are used, either sufficient: an empty command line, since
// kernel threads have no argv, and a parent of PID 2, which is kthreadd.
// Full ancestry walking is not performed, because the direct children
// are the overwhelming majority and the walk would cost one read per
// generation for every process on the machine.
func IsKernelThread(cmdline []string, ppid int) bool {
	return len(cmdline) == 0 || ppid == 2
}

