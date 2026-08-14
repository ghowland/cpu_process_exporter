package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads a YAML or JSON file, applies defaults for absent fields,
// and validates the result. A file that fails validation is not
// returned, so a bad file cannot replace a working configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	format := "yaml"
	if strings.EqualFold(filepath.Ext(path), ".json") {
		format = "json"
	}
	return Parse(data, format)
}

// Parse is Load without file access.
func Parse(data []byte, format string) (*Config, error) {
	cfg := Default()
	var err error
	switch format {
	case "json":
		err = json.Unmarshal(data, &cfg)
	default:
		err = yaml.Unmarshal(data, &cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	cfg.normalise()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalise fills values that depend on other values and applies the
// default lists when a section was left unset. A nil list means the
// operator did not mention it and gets the defaults; an explicitly
// empty list means they want nothing.
func (c *Config) normalise() {
	if c.Filter.IgnoreComm == nil {
		c.Filter.IgnoreComm = append([]string{}, DefaultIgnoreComm...)
	}
	if c.Filter.IgnoreCommPrefix == nil {
		c.Filter.IgnoreCommPrefix = append([]string{}, DefaultIgnoreCommPrefix...)
	}
	if c.Filter.IgnoreCommRegex == nil {
		c.Filter.IgnoreCommRegex = append([]string{}, DefaultIgnoreCommRegex...)
	}
	if c.Naming.Rules == nil {
		c.Naming.Rules = append([]NamingRule{}, DefaultNamingRules...)
	}
	if c.Scan.ProcPath == "" {
		c.Scan.ProcPath = "/proc"
	}
	if c.Naming.Fallback == "" {
		c.Naming.Fallback = "comm"
	}
	if c.Server.MetricsPath == "" {
		c.Server.MetricsPath = "/metrics"
	}
	for i := range c.Naming.Rules {
		if c.Naming.Rules[i].From == "" {
			c.Naming.Rules[i].From = "cmdline"
		}
	}
}

// Validate reports every problem found, joined into one error. It never
// mutates the Config.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	s := c.Scan
	if s.Interval.D() < time.Second {
		add("scan.interval must be at least 1s")
	}
	if s.BatchSize < 1 {
		add("scan.batch_size must be at least 1")
	}
	if s.BatchSize > 10000 {
		add("scan.batch_size above 10000 defeats the purpose of batching")
	}
	if s.BatchSleep.D() < 0 {
		add("scan.batch_sleep must not be negative")
	}
	if s.BatchSleep.D() > time.Second {
		add("scan.batch_sleep above 1s will make scans take minutes")
	}
	if s.FDScanEvery < 1 {
		add("scan.fd_scan_every must be at least 1")
	}
	if s.SmapsScanEvery < 1 {
		add("scan.smaps_scan_every must be at least 1")
	}
	if s.GroupRetention.D() < 0 {
		add("scan.group_retention must not be negative")
	}
	if s.ProcPath == "" {
		add("scan.proc_path is empty")
	} else if fi, err := os.Stat(s.ProcPath); err != nil {
		add("scan.proc_path %s is not readable: %v", s.ProcPath, err)
	} else if !fi.IsDir() {
		add("scan.proc_path %s is not a directory", s.ProcPath)
	}

	// A scan that takes longer than the interval causes overruns. This
	// is a rough estimate against a nominal 2000 processes; it warns
	// rather than blocks, because the real process count is unknown at
	// validation time.
	if s.BatchSize > 0 {
		estimate := time.Duration(2000/s.BatchSize) * s.BatchSleep.D()
		if estimate > s.Interval.D() {
			add("scan.batch_sleep %s at batch_size %d would take about %s "+
				"for 2000 processes, which exceeds scan.interval %s",
				s.BatchSleep, s.BatchSize, estimate, s.Interval)
		}
	}

	for i, p := range c.Filter.IgnoreCommRegex {
		if _, err := regexp.Compile(p); err != nil {
			add("filter.ignore_comm_regex[%d]: %v", i, err)
		}
	}
	for i, p := range c.Filter.IgnoreCmdlineRegex {
		if _, err := regexp.Compile(p); err != nil {
			add("filter.ignore_cmdline_regex[%d]: %v", i, err)
		}
	}
	for i, p := range c.Filter.IncludeOnly {
		if _, err := regexp.Compile(p); err != nil {
			add("filter.include_only[%d]: %v", i, err)
		}
	}

	switch c.Naming.Fallback {
	case "comm", "exe_basename", "cmdline_basename":
	default:
		add("naming.fallback must be comm, exe_basename, or cmdline_basename")
	}
	for i, r := range c.Naming.Rules {
		hasMatch := r.MatchComm != "" || r.MatchCommPrefix != "" ||
			r.MatchCommRegex != "" || r.MatchExeRegex != ""
		if !hasMatch {
			add("naming.rules[%d] has no match condition", i)
		}
		if r.Name == "" {
			add("naming.rules[%d].name is empty", i)
		}
		switch r.From {
		case "comm", "exe", "cmdline", "":
		default:
			add("naming.rules[%d].from must be comm, exe, or cmdline", i)
		}
		if r.MatchCommRegex != "" {
			if _, err := regexp.Compile(r.MatchCommRegex); err != nil {
				add("naming.rules[%d].match_comm_regex: %v", i, err)
			}
		}
		if r.MatchExeRegex != "" {
			if _, err := regexp.Compile(r.MatchExeRegex); err != nil {
				add("naming.rules[%d].match_exe_regex: %v", i, err)
			}
		}
		if r.Regex != "" {
			if _, err := regexp.Compile(r.Regex); err != nil {
				add("naming.rules[%d].regex: %v", i, err)
			}
		}
		if r.Regex == "" && strings.Contains(r.Name, "$") {
			add("naming.rules[%d].name references a capture group but no regex is set", i)
		}
	}

	if c.Server.Listen == "" {
		add("server.listen is empty")
	}
	if !strings.HasPrefix(c.Server.MetricsPath, "/") {
		add("server.metrics_path must start with /")
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug, info, warn, or error")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format must be json or text")
	}

	return errors.Join(errs...)
}

// Equal reports whether two configurations are identical, used to
// suppress work after a reload that changed nothing.
func (c *Config) Equal(o *Config) bool {
	if c == nil || o == nil {
		return c == o
	}
	return reflect.DeepEqual(*c, *o)
}

// NamingChanged reports whether a reload alters group naming. Such a
// change rebuilds every group name and breaks every time series, so the
// caller logs it at a higher level than an ordinary reload and resets
// the accumulators.
func (c *Config) NamingChanged(o *Config) bool {
	if c == nil || o == nil {
		return true
	}
	return !reflect.DeepEqual(c.Naming, o.Naming)
}
