package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
)

// ConfigParseError reports a config file that exists but could not be read or
// decoded. It is deliberately distinct from a missing file, which is not an
// error: defaults plus environment are a valid configuration.
type ConfigParseError struct {
	Path string
	Err  error
}

func (e *ConfigParseError) Error() string {
	return fmt.Sprintf("config: cannot load %s: %v", e.Path, e.Err)
}

func (e *ConfigParseError) Unwrap() error { return e.Err }

// ConfigUnreadableError reports a config path that could not be examined at all:
// a component of the path is not a directory, or the process may not traverse
// it. It is deliberately distinct from both siblings -- a missing file is
// benign, and a parse failure means the bytes were read but did not decode --
// because this case is the one where settings exist and cannot be seen.
type ConfigUnreadableError struct {
	Path string
	Err  error
}

func (e *ConfigUnreadableError) Error() string {
	return fmt.Sprintf("config: cannot read %s: %v", e.Path, e.Err)
}

func (e *ConfigUnreadableError) Unwrap() error { return e.Err }

// ConfigEmptyError reports a config file that exists but holds nothing: zero
// bytes, or only whitespace. The file watcher refuses it rather than reloading,
// because a tool rewriting the file in place passes through exactly this state
// -- truncated, not yet written -- and an empty file decodes to every default,
// silently dropping alert thresholds and fleet peers. Startup and an explicit
// SIGHUP still read an empty file as "run on defaults".
type ConfigEmptyError struct {
	Path string
}

func (e *ConfigEmptyError) Error() string {
	return fmt.Sprintf("config: %s is empty; keeping the running configuration until it has contents", e.Path)
}

// ConfigWatchError reports that the config file's directory could not be
// watched for changes: the watcher could not be created, or the directory could
// not be added to it. Dir is the directory, since that, not the file, is what
// is watched.
type ConfigWatchError struct {
	Dir string
	Err error
}

func (e *ConfigWatchError) Error() string {
	return fmt.Sprintf("config: cannot watch %s: %v", e.Dir, e.Err)
}

func (e *ConfigWatchError) Unwrap() error { return e.Err }

// DuplicatePeerError reports two fleet peers claiming the same name. The name is
// a peer's identity throughout the fleet subsystem, so a duplicate cannot be
// resolved to one host: it is a mistake in the config file that only the
// operator can settle. URLs holds the two conflicting addresses, in the order
// they appear in the file, so the message points at the lines to fix.
type DuplicatePeerError struct {
	Name string
	URLs []string
}

func (e *DuplicatePeerError) Error() string {
	return fmt.Sprintf("config: fleet peer name %q is used by more than one peer (%s); names must be unique",
		e.Name, strings.Join(e.URLs, ", "))
}

// PeerCredentialsError reports a fleet peer URL carrying embedded credentials.
// Authenticated peers are not supported, so this is an operator mistake rather
// than a configuration to accept and hide. Failing the load means the secret is
// never stored, never rendered into the settings view and never sent anywhere;
// redacting it at the point of display would leave it live in memory and one
// future code path away from leaking again.
//
// The message names the peer and its host ONLY. It must never carry the URL or
// the userinfo, because it goes to logs -- which is exactly the kind of place a
// credential is not supposed to end up.
type PeerCredentialsError struct {
	Name string
	Host string
}

func (e *PeerCredentialsError) Error() string {
	return fmt.Sprintf("config: fleet peer %q (%s) has credentials embedded in its url; "+
		"authenticated peers are not supported -- configure it with a url that carries no credentials",
		e.Name, e.Host)
}

// PeerURLError reports a fleet peer URL the poller could never use: one that does
// not parse, or that parses but is not an absolute http/https URL with a host.
// Both are caught at load, which turns what would otherwise be a connection
// failure repeated on every poll -- against a node the dashboard shows as
// merely unreachable -- into one clear error where the mistake was made.
//
// Reason explains what is wrong without reproducing the URL. A malformed or
// unusable value can still contain a credential, and this text reaches logs; in
// particular the *url.Error wrapper formats the whole URL into its own message,
// so only its inner cause is ever reported.
type PeerURLError struct {
	Name   string
	Reason string
}

func (e *PeerURLError) Error() string {
	return fmt.Sprintf("config: fleet peer %q has an unusable url: %s", e.Name, e.Reason)
}

// PeerNameError reports a fleet peer whose name cannot be its fleet-wide
// identity. A peer's name is also the Prometheus instance label its history is
// stored under, so it is held to nodename.Rule at load: a name that breaks it
// would otherwise surface as a history request that fails, or a chart that is
// simply empty, long after the file was written.
//
// Host is the URL's host and port, never the URL, for the reason
// PeerCredentialsError gives.
type PeerNameError struct {
	Name string
	Host string
	Err  error
}

func (e *PeerNameError) Error() string {
	return fmt.Sprintf("config: fleet peer %s (%s) has an unusable name: %s; a peer's name is its fleet-wide "+
		"identity and the Prometheus instance label its history is stored under", nodename.Quote(e.Name), e.Host, nodename.Rule)
}

func (e *PeerNameError) Unwrap() error { return e.Err }

// PeerUnnamedError reports a fleet peer with no `name:` (owner ruling
// 2026-09-23, under #120). A peer's name is its fleet-wide identity -- its
// card, its /api/fleet/{name}/... routes and its Prometheus instance label --
// and it is published on unauthenticated endpoints. Defaulting it to the URL's
// host published the peer's address as its routing key, so an unnamed peer is
// refused at load and on reload, and a reload that introduces one keeps the
// running configuration.
//
// The peer is identified by Position, its 1-based place in fleet.peers, and
// never by its URL or host: this is the one peer error whose entry has nothing
// else that may be quoted.
type PeerUnnamedError struct {
	Position int
}

func (e *PeerUnnamedError) Error() string {
	return "config: fleet peer " + strconv.Itoa(e.Position) + " in fleet.peers has no name; " +
		"add name: set to that node's fleet.local_name (its hostname unless set), " +
		"which is also the instance label its history is stored under"
}

// LocalNameError reports a name for this node that cannot be its fleet-wide
// identity. It is held to nodename.Rule for the same reason a peer name is:
// this node's history is written under it as the Prometheus instance label,
// and an aggregator's peer entry for this node must carry the same name.
//
// FromHostname marks the fallback: fleet.local_name was unset and the host's
// own name broke the rule, so the fix is to set fleet.local_name rather than
// to rename the machine.
type LocalNameError struct {
	Name         string
	FromHostname bool
	Err          error
}

func (e *LocalNameError) Error() string {
	if e.FromHostname {
		return fmt.Sprintf("config: fleet.local_name is not set and this host's name %s cannot stand in for it "+
			"(%s); set fleet.local_name", nodename.Quote(e.Name), nodename.Rule)
	}
	return fmt.Sprintf("config: fleet.local_name %s is unusable: %s; it is this node's fleet-wide identity "+
		"and the Prometheus instance label its history is stored under", nodename.Quote(e.Name), nodename.Rule)
}

func (e *LocalNameError) Unwrap() error { return e.Err }

// PeerIsLocalNameError reports a fleet peer configured with this node's own name.
//
// The name is the identity: the fleet handlers check the local name before
// consulting the peer cache, so such a peer renders as a second same-named card
// and answers with the LOCAL machine's snapshot and configuration when
// selected. The real host is never polled, and the operator sees a node that
// looks monitored and is not. Like a duplicate peer name, this is a collision
// only the operator can resolve.
type PeerIsLocalNameError struct {
	Name string
}

func (e *PeerIsLocalNameError) Error() string {
	return fmt.Sprintf("config: fleet peer %q is also this node's own name; "+
		"give the peer a different name or set fleet.local_name to something else", e.Name)
}

// ApplyError reports that a reload was published but one or more runtime
// components rejected the new settings. The configuration is already live for
// everything that accepted it, so this is a partial-application warning rather
// than a reason to discard the reload.
type ApplyError struct {
	Reasons []string
}

func (e *ApplyError) Error() string {
	return "config: reload applied with errors: " + strings.Join(e.Reasons, "; ")
}

// PrometheusURLError reports a prometheus URL that cannot be used. Key names the
// setting so the message points at the line to fix; the URL itself is never
// quoted, since the same string may be the thing that must not be echoed.
type PrometheusURLError struct {
	Key    string
	Reason string
}

func (e *PrometheusURLError) Error() string {
	return fmt.Sprintf("config: %s is unusable: %s", e.Key, e.Reason)
}

// PrometheusCredentialsError reports a prometheus URL carrying embedded
// credentials.
//
// This mirrors PeerCredentialsError, and for the same reason: the settings view
// has no authentication of its own, so a secret accepted here would live in
// memory and be one code path away from being rendered. Sending it to a
// Prometheus that a whole fleet writes to would spread it further than a peer
// URL ever could. Only the host is named -- never the URL.
type PrometheusCredentialsError struct {
	Key  string
	Host string
}

func (e *PrometheusCredentialsError) Error() string {
	return fmt.Sprintf("config: %s must not carry credentials (host %s); "+
		"put them in a reverse proxy or an authenticating sidecar instead", e.Key, e.Host)
}

// BMCURLError reports a bmc.redfish_url the drive-slot collector cannot use.
// The URL itself is never quoted, for the reason PrometheusURLError gives.
type BMCURLError struct {
	Reason string
}

func (e *BMCURLError) Error() string {
	return "config: bmc.redfish_url is unusable: " + e.Reason
}

// BMCCredentialsError reports a bmc.redfish_url carrying embedded credentials.
// The password belongs in bmc.password_file, where it is read per poll and
// never held in the config; only the host is named, never the URL.
type BMCCredentialsError struct {
	Host string
}

func (e *BMCCredentialsError) Error() string {
	return fmt.Sprintf("config: bmc.redfish_url must not carry credentials (host %s); "+
		"set bmc.username and bmc.password_file instead", e.Host)
}

// PrometheusLabelError reports an entry in prometheus.external_labels that
// Prometheus would refuse, or that collides with a label go-sysmon sets itself.
//
// It is an error rather than a silently dropped entry: a label an operator
// wrote down and did not get leaves them looking at a database that does not
// have it, with nothing anywhere saying why. The value is never quoted -- only
// the name is a mistake worth naming, and the value may be anything.
type PrometheusLabelError struct {
	Name   string
	Reason string
}

// RetiredConfigKeyError reports a key this build no longer has a field for, naming
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
type RetiredConfigKeyError struct {
	Path     string
	Key      string
	Guidance string
}

func (e *RetiredConfigKeyError) Error() string {
	return fmt.Sprintf("%s sets %s, which this build no longer has: %s",
		e.Path, e.Key, e.Guidance)
}

func (e *PrometheusLabelError) Error() string {
	return fmt.Sprintf("config: prometheus.external_labels %q is unusable: %s", e.Name, e.Reason)
}

// HistoryCadenceError reports a configuration that would write history more
// slowly than the time-series database's query lookback allows.
//
// A series with no sample for longer than the lookback is stale: the database
// shows it as absent between samples, so a gauge charts as isolated dots, and
// a counter's rate cannot be told from one whose node was down. A node that
// writes history at such a cadence records something no chart can draw, so it
// is refused rather than accepted and silently useless. Key names the
// settings that add up to the cadence.
type HistoryCadenceError struct {
	Key        string
	IntervalMs int
	LookbackMs int
}

func (e *HistoryCadenceError) Error() string {
	return fmt.Sprintf("config: %s is %dms; while remote_write_url is set it must be at most %dms, "+
		"the time-series database's query lookback, or the history written is stale between samples",
		e.Key, e.IntervalMs, e.LookbackMs)
}

// IntervalBelowFloorError reports a collection interval faster than
// MinIntervalMs.
type IntervalBelowFloorError struct {
	Key        string
	IntervalMs int
}

func (e *IntervalBelowFloorError) Error() string {
	return fmt.Sprintf("config: %s is %dms; it must be at least %dms", e.Key, e.IntervalMs, MinIntervalMs)
}

// HeadroomTargetError reports a fleet.headroom_target outside (0, 1]. The
// target is the utilisation a node's headroom is measured up to, as a
// fraction: zero or less leaves no node any headroom, and more than one
// promises capacity past a node's whole.
type HeadroomTargetError struct {
	Value float64
}

func (e *HeadroomTargetError) Error() string {
	return fmt.Sprintf("config: fleet.headroom_target %s is out of range; it is a utilisation fraction, "+
		"greater than 0 and at most 1 (0.8 is 80%%)", strconv.FormatFloat(e.Value, 'g', -1, 64))
}

// TrustedProxyError reports a server.trusted_proxies entry that is not a CIDR
// prefix. It stops startup: a list the server cannot read in full would trust
// a different set of proxies than the file names.
type TrustedProxyError struct {
	CIDR string
	Err  error
}

func (e *TrustedProxyError) Error() string {
	return fmt.Sprintf("config: server.trusted_proxies entry %q is not a CIDR prefix "+
		"(write 10.0.0.0/8, or 192.0.2.10/32 for one host): %v", e.CIDR, e.Err)
}

func (e *TrustedProxyError) Unwrap() error { return e.Err }
