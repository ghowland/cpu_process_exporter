// Package namer derives the group name from a process. The rules are
// the cardinality control: a rule that extracts an unbounded value,
// such as a PID or a container ID, creates one series per process
// instance and defeats the point of grouping.
package namer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/example/process_exporter/internal/config"
)

// maxNameLen bounds a derived label value.
const maxNameLen = 128

// compiledRule is one rule with its patterns compiled.
type compiledRule struct {
	matchComm       string
	matchCommPrefix string
	matchCommRegex  *regexp.Regexp
	matchExeRegex   *regexp.Regexp
	from            string
	extract         *regexp.Regexp
	template        string
}

// matches reports whether the rule's match conditions are satisfied,
// before extraction is attempted.
func (r *compiledRule) matches(in Input) bool {
	if r.matchComm != "" && in.Comm != r.matchComm {
		return false
	}
	if r.matchCommPrefix != "" && !strings.HasPrefix(in.Comm, r.matchCommPrefix) {
		return false
	}
	if r.matchCommRegex != nil && !r.matchCommRegex.MatchString(in.Comm) {
		return false
	}
	if r.matchExeRegex != nil {
		if in.Exe == "" || !r.matchExeRegex.MatchString(in.Exe) {
			return false
		}
	}
	return true
}

// Namer applies the naming rules. It is compiled once and is then
// read-only and safe for concurrent use.
type Namer struct {
	rules    []*compiledRule
	fallback string
}

// New compiles a NamingConfig.
func New(cfg config.NamingConfig) (*Namer, error) {
	n := &Namer{fallback: cfg.Fallback}
	if n.fallback == "" {
		n.fallback = "comm"
	}

	for i, r := range cfg.Rules {
		cr := &compiledRule{
			matchComm:       r.MatchComm,
			matchCommPrefix: r.MatchCommPrefix,
			from:            r.From,
			template:        r.Name,
		}
		if cr.from == "" {
			cr.from = "cmdline"
		}
		var err error
		if r.MatchCommRegex != "" {
			cr.matchCommRegex, err = regexp.Compile(r.MatchCommRegex)
			if err != nil {
				return nil, fmt.Errorf("namer: rules[%d].match_comm_regex: %w", i, err)
			}
		}
		if r.MatchExeRegex != "" {
			cr.matchExeRegex, err = regexp.Compile(r.MatchExeRegex)
			if err != nil {
				return nil, fmt.Errorf("namer: rules[%d].match_exe_regex: %w", i, err)
			}
		}
		if r.Regex != "" {
			cr.extract, err = regexp.Compile(r.Regex)
			if err != nil {
				return nil, fmt.Errorf("namer: rules[%d].regex: %w", i, err)
			}
		}
		n.rules = append(n.rules, cr)
	}
	return n, nil
}

// Input is what the namer examines.
type Input struct {
	Comm    string
	Exe     string
	Cmdline []string
}

// Name derives the group name.
//
// Rules are evaluated in order; the first that both matches and
// successfully extracts wins. A rule that matches but whose extraction
// regex does not match falls through to the next rule, which is what
// lets a jar-first then main-class cascade work for Java: the first
// rule catches "-jar app.jar" and the second catches a bare main class,
// and a JVM invoked either way gets a useful name.
func (n *Namer) Name(in Input) string {
	for _, r := range n.rules {
		if !r.matches(in) {
			continue
		}
		if name, ok := n.applyRule(r, in); ok {
			return Sanitize(name)
		}
	}
	return Sanitize(n.fallbackName(in))
}

// applyRule evaluates one rule and reports whether it produced a name.
func (n *Namer) applyRule(r *compiledRule, in Input) (string, bool) {
	// A rule with no extraction regex emits its template literally.
	// This is how the container shims collapse to one group: match the
	// prefix, discard the container ID that follows, emit a constant.
	if r.extract == nil {
		return r.template, true
	}

	text := sourceText(r.from, in)
	if text == "" {
		return "", false
	}
	m := r.extract.FindStringSubmatchIndex(text)
	if m == nil {
		return "", false
	}
	out := r.extract.ExpandString(nil, r.template, text, m)
	if len(out) == 0 {
		return "", false
	}
	return string(out), true
}

// fallbackName returns the name when no rule matched.
func (n *Namer) fallbackName(in Input) string {
	switch n.fallback {
	case "exe_basename":
		if in.Exe != "" {
			return filepath.Base(in.Exe)
		}
		if len(in.Cmdline) > 0 {
			return filepath.Base(in.Cmdline[0])
		}
	case "cmdline_basename":
		if len(in.Cmdline) > 0 {
			return filepath.Base(in.Cmdline[0])
		}
		if in.Exe != "" {
			return filepath.Base(in.Exe)
		}
	}
	if in.Comm != "" {
		return in.Comm
	}
	if in.Exe != "" {
		return filepath.Base(in.Exe)
	}
	if len(in.Cmdline) > 0 {
		return filepath.Base(in.Cmdline[0])
	}
	return "unknown"
}

// sourceText selects the text an extraction regex runs against.
func sourceText(from string, in Input) string {
	switch from {
	case "comm":
		return in.Comm
	case "exe":
		return in.Exe
	default:
		if len(in.Cmdline) == 0 {
			return ""
		}
		return strings.Join(in.Cmdline, " ")
	}
}

// Sanitize makes a derived name safe as a Prometheus label value.
//
// The character set is restricted so that a process which embeds
// arbitrary bytes in its argv cannot produce an unreadable or
// unqueryable label. The length cap bounds the memory cost of a rule
// that accidentally captures a whole command line. An empty result
// becomes "unknown", because a label value must not be empty.
func Sanitize(s string) string {
	if s == "" {
		return "unknown"
	}
	if len(s) > maxNameLen {
		s = s[:maxNameLen]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '.', c == '/', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}

