// Package nodename defines what a fleet node's name may look like.
//
// A node's name is its identity across the whole fleet: it titles the node's
// card, routes /api/fleet/{name}/..., keys the poller's cache and -- the reason
// this package exists -- is the Prometheus `instance` label the node's history
// is written and queried under. A name that cannot be that label is not a name
// the fleet can use, so the rule lives in one place and is applied both when
// the configuration loads and when a history query is built.
//
// It is a leaf package rather than part of pkg/prom because pkg/config must
// apply it, and pkg/prom carries the remote-write encoder that the desktop
// build deliberately does not link.
package nodename

import (
	"regexp"
	"strings"
)

// Rule states the constraint in words, for error messages that have to tell an
// operator what to write instead. It describes pattern and must change with it.
const Rule = "a node name must start with a letter or digit and contain only " +
	"letters, digits, '.', '_', ':' and '-', at most 253 characters"

// pattern is what a node name may look like. Fleet names are hostnames or short
// labels, so this is deliberately narrower than PromQL would allow: with the
// value constrained to characters that carry no meaning inside a matcher, a
// selector built from it cannot be turned into a different query.
var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

// ErrInvalid reports a name that breaks Rule.
type ErrInvalid struct {
	Name string
}

func (e *ErrInvalid) Error() string {
	return "nodename: " + Quote(e.Name) + " is not a usable node name: " + Rule
}

// Validate reports whether name can identify a node, returning *ErrInvalid when
// it cannot.
func Validate(name string) error {
	if !pattern.MatchString(name) {
		return &ErrInvalid{Name: name}
	}
	return nil
}

// Quote renders an untrusted name for a log line or an error message: bounded
// in length, with control characters, quotes and backslashes neutralised so the
// value cannot forge a line or break out of its quotes.
func Quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return `"` + strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return '.'
		}
		return r
	}, s) + `"`
}
