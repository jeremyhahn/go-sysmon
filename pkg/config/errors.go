package config

import (
	"fmt"
	"strings"
)

// ErrConfigParse reports a config file that exists but could not be read or
// decoded. It is deliberately distinct from a missing file, which is not an
// error: defaults plus environment are a valid configuration.
type ErrConfigParse struct {
	Path string
	Err  error
}

func (e *ErrConfigParse) Error() string {
	return fmt.Sprintf("config: cannot load %s: %v", e.Path, e.Err)
}

func (e *ErrConfigParse) Unwrap() error { return e.Err }

// ErrConfigUnreadable reports a config path that could not be examined at all:
// a component of the path is not a directory, or the process may not traverse
// it. It is deliberately distinct from both siblings -- a missing file is
// benign, and a parse failure means the bytes were read but did not decode --
// because this case is the one where settings exist and cannot be seen.
type ErrConfigUnreadable struct {
	Path string
	Err  error
}

func (e *ErrConfigUnreadable) Error() string {
	return fmt.Sprintf("config: cannot read %s: %v", e.Path, e.Err)
}

func (e *ErrConfigUnreadable) Unwrap() error { return e.Err }

// ErrDuplicatePeer reports two fleet peers claiming the same name. The name is
// a peer's identity throughout the fleet subsystem, so a duplicate cannot be
// resolved to one host: it is a mistake in the config file that only the
// operator can settle. URLs holds the two conflicting addresses, in the order
// they appear in the file, so the message points at the lines to fix.
type ErrDuplicatePeer struct {
	Name string
	URLs []string
}

func (e *ErrDuplicatePeer) Error() string {
	return fmt.Sprintf("config: fleet peer name %q is used by more than one peer (%s); names must be unique",
		e.Name, strings.Join(e.URLs, ", "))
}

// ErrPeerCredentials reports a fleet peer URL carrying embedded credentials.
// Authenticated peers are not supported, so this is an operator mistake rather
// than a configuration to accept and hide. Failing the load means the secret is
// never stored, never rendered into the settings view and never sent anywhere;
// redacting it at the point of display would leave it live in memory and one
// future code path away from leaking again.
//
// The message names the peer and its host ONLY. It must never carry the URL or
// the userinfo, because it goes to logs -- which is exactly the kind of place a
// credential is not supposed to end up.
type ErrPeerCredentials struct {
	Name string
	Host string
}

func (e *ErrPeerCredentials) Error() string {
	return fmt.Sprintf("config: fleet peer %q (%s) has credentials embedded in its url; "+
		"authenticated peers are not supported -- configure it with a url that carries no credentials",
		e.Name, e.Host)
}

// ErrPeerURL reports a fleet peer URL the poller could never use: one that does
// not parse, or that parses but is not an absolute http/https URL with a host.
// Both are caught at load, which turns what would otherwise be a connection
// failure repeated on every poll -- against a node the dashboard shows as
// merely unreachable -- into one clear error where the mistake was made.
//
// Reason explains what is wrong without reproducing the URL. A malformed or
// unusable value can still contain a credential, and this text reaches logs; in
// particular the *url.Error wrapper formats the whole URL into its own message,
// so only its inner cause is ever reported.
type ErrPeerURL struct {
	Name   string
	Reason string
}

func (e *ErrPeerURL) Error() string {
	return fmt.Sprintf("config: fleet peer %q has an unusable url: %s", e.Name, e.Reason)
}

// ErrPeerIsLocalName reports a fleet peer configured with this node's own name.
//
// The name is the identity: the fleet handlers check the local name before
// consulting the peer cache, so such a peer renders as a second same-named card
// and answers with the LOCAL machine's snapshot and configuration when
// selected. The real host is never polled, and the operator sees a node that
// looks monitored and is not. Like a duplicate peer name, this is a collision
// only the operator can resolve.
type ErrPeerIsLocalName struct {
	Name string
}

func (e *ErrPeerIsLocalName) Error() string {
	return fmt.Sprintf("config: fleet peer %q is also this node's own name; "+
		"give the peer a different name or set fleet.local_name to something else", e.Name)
}

// ErrApply reports that a reload was published but one or more runtime
// components rejected the new settings. The configuration is already live for
// everything that accepted it, so this is a partial-application warning rather
// than a reason to discard the reload.
type ErrApply struct {
	Reasons []string
}

func (e *ErrApply) Error() string {
	return "config: reload applied with errors: " + strings.Join(e.Reasons, "; ")
}

// ErrPrometheusURL reports a prometheus URL that cannot be used. Key names the
// setting so the message points at the line to fix; the URL itself is never
// quoted, since the same string may be the thing that must not be echoed.
type ErrPrometheusURL struct {
	Key    string
	Reason string
}

func (e *ErrPrometheusURL) Error() string {
	return fmt.Sprintf("config: %s is unusable: %s", e.Key, e.Reason)
}

// ErrPrometheusCredentials reports a prometheus URL carrying embedded
// credentials.
//
// This mirrors ErrPeerCredentials, and for the same reason: the settings view
// has no authentication of its own, so a secret accepted here would live in
// memory and be one code path away from being rendered. Sending it to a
// Prometheus that a whole fleet writes to would spread it further than a peer
// URL ever could. Only the host is named -- never the URL.
type ErrPrometheusCredentials struct {
	Key  string
	Host string
}

func (e *ErrPrometheusCredentials) Error() string {
	return fmt.Sprintf("config: %s must not carry credentials (host %s); "+
		"put them in a reverse proxy or an authenticating sidecar instead", e.Key, e.Host)
}

// ErrPrometheusLabel reports an entry in prometheus.external_labels that
// Prometheus would refuse, or that collides with a label go-sysmon sets itself.
//
// It is an error rather than a silently dropped entry: a label an operator
// wrote down and did not get leaves them looking at a database that does not
// have it, with nothing anywhere saying why. The value is never quoted -- only
// the name is a mistake worth naming, and the value may be anything.
type ErrPrometheusLabel struct {
	Name   string
	Reason string
}

// ErrRetiredConfigKey reports a key this build no longer has a field for, naming
// what replaced it.
//
// It exists because the strict decode in Loader.Load cannot tell a typo from a
// key we removed under the operator. Both arrive as viper's "invalid keys:
// alerts.swap_pct", which says the key is wrong and not what to write instead --
// and for a rename we initiated, the operator did nothing wrong and deserves the
// answer rather than a puzzle.
//
// Accepting and ignoring the old key was the alternative, and it is the one thing
// that strict decode exists to prevent: the file would go on stating a threshold
// the daemon no longer applies.
type ErrRetiredConfigKey struct {
	Path     string
	Key      string
	Guidance string
}

func (e *ErrRetiredConfigKey) Error() string {
	return fmt.Sprintf("%s sets %s, which this build no longer has: %s",
		e.Path, e.Key, e.Guidance)
}

func (e *ErrPrometheusLabel) Error() string {
	return fmt.Sprintf("config: prometheus.external_labels %q is unusable: %s", e.Name, e.Reason)
}
