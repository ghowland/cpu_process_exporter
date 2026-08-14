// Package config holds every configuration struct, its defaults, its
// validation, and the reload watcher.
package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so that a value such as 15s parses from
// both YAML and JSON. The standard library does not parse a duration
// string into time.Duration directly.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		var n int64
		if err2 := value.Decode(&n); err2 != nil {
			return err
		}
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	p, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(p)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		p, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(p)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Config is the complete effective configuration after defaults.
type Config struct {
	Scan   ScanConfig   `yaml:"scan"   json:"scan"`
	Filter FilterConfig `yaml:"filter" json:"filter"`
	Naming NamingConfig `yaml:"naming" json:"naming"`
	Server ServerConfig `yaml:"server" json:"server"`
	Log    LogConfig    `yaml:"log"    json:"log"`
}

// ScanConfig controls how often the scanner runs and how hard it works
// while running. BatchSize and BatchSleep are what keep the exporter
// off the top of the process list: the scan yields between batches so
// the scheduler does not treat it as a CPU-bound task.
type ScanConfig struct {
	Interval    Duration `yaml:"interval"        json:"interval"`
	BatchSize   int      `yaml:"batch_size"      json:"batch_size"`
	BatchSleep  Duration `yaml:"batch_sleep"     json:"batch_sleep"`
	FDScanEvery int      `yaml:"fd_scan_every"   json:"fd_scan_every"`

	// ReadSmaps collects proportional set size, which is the only
	// correct group memory footprint. Resident set size counts each
	// shared page once per member, so a group sum over-counts.
	//
	// The read walks page tables and is comparable in cost to the
	// descriptor directory walk, so it runs on its own schedule.
	ReadSmaps      bool `yaml:"read_smaps"       json:"read_smaps"`
	SmapsScanEvery int  `yaml:"smaps_scan_every" json:"smaps_scan_every"`

	ReadIO         bool     `yaml:"read_io"         json:"read_io"`
	ReadStatus     bool     `yaml:"read_status"     json:"read_status"`
	CacheCmdline   bool     `yaml:"cache_cmdline"   json:"cache_cmdline"`
	GroupRetention Duration `yaml:"group_retention" json:"group_retention"`
	ProcPath       string   `yaml:"proc_path"       json:"proc_path"`
}

// FilterConfig removes processes that carry no useful signal. Kernel
// threads have no address space of their own, so their memory readings
// are meaningless, and their names are numerous and machine-specific,
// which makes them a cardinality problem as well as a noise problem.
type FilterConfig struct {
	IgnoreKernelThreads bool     `yaml:"ignore_kernel_threads" json:"ignore_kernel_threads"`
	IgnoreComm          []string `yaml:"ignore_comm"           json:"ignore_comm"`
	IgnoreCommPrefix    []string `yaml:"ignore_comm_prefix"    json:"ignore_comm_prefix"`
	IgnoreCommRegex     []string `yaml:"ignore_comm_regex"     json:"ignore_comm_regex"`
	IgnoreCmdlineRegex  []string `yaml:"ignore_cmdline_regex"  json:"ignore_cmdline_regex"`
	IgnoreUsers         []string `yaml:"ignore_users"          json:"ignore_users"`
	IncludeOnly         []string `yaml:"include_only"          json:"include_only"`
}

// NeedsCmdline reports whether any rule requires reading
// /proc/<pid>/cmdline before the filter can decide. When false, the
// scanner applies the filter after reading stat and before every other
// read, so an ignored process costs one small read rather than several.
func (f FilterConfig) NeedsCmdline() bool {
	return len(f.IgnoreCmdlineRegex) > 0 || f.IgnoreKernelThreads || len(f.IncludeOnly) > 0
}

// NamingConfig turns a process into a group name. The rules are the
// cardinality control: a rule that extracts a PID, a UUID, or a
// timestamp creates one series per process instance.
type NamingConfig struct {
	Fallback     string       `yaml:"fallback"      json:"fallback"`
	ResolveUsers bool         `yaml:"resolve_users" json:"resolve_users"`
	Rules        []NamingRule `yaml:"rules"         json:"rules"`
}

// NamingRule is one match-and-extract step. Rules are evaluated in
// order. A rule that matches but whose extraction regex does not match
// falls through to the next rule, which is what lets a jar-first then
// main-class cascade work for Java.
type NamingRule struct {
	MatchComm       string `yaml:"match_comm"        json:"match_comm"`
	MatchCommPrefix string `yaml:"match_comm_prefix" json:"match_comm_prefix"`
	MatchCommRegex  string `yaml:"match_comm_regex"  json:"match_comm_regex"`
	MatchExeRegex   string `yaml:"match_exe_regex"   json:"match_exe_regex"`
	From            string `yaml:"from"              json:"from"`
	Regex           string `yaml:"regex"             json:"regex"`
	Name            string `yaml:"name"              json:"name"`
}

type ServerConfig struct {
	Listen      string `yaml:"listen"       json:"listen"`
	MetricsPath string `yaml:"metrics_path" json:"metrics_path"`
}

type LogConfig struct {
	Level  string `yaml:"level"  json:"level"`
	Format string `yaml:"format" json:"format"`
}
