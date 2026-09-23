package server

import (
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// snapshotDecorator is the one place a served snapshot is given the fields
// that belong to the service rather than the hardware: the build serving it,
// the name the node writes its history under, and the alerts its thresholds
// produce.
//
// The server installs it on its monitor, so every path that serves a snapshot
// -- /api/snapshot, both event stream protocols, the fleet's view of this
// node, /metrics and the exporter -- reads the same decorated value, whether
// the server was built by the CLI or embedded by another program. These fields
// used to be attached by the CLI's own snapshotter, so an embedded server
// served none of them and /api/snapshot and /api/events could disagree.
type snapshotDecorator struct {
	// key is everything the decoration is derived from. Two servers built
	// over one monitor with the same key describe the same node the same
	// way and share one decorator -- and one alert evaluator, so a
	// condition's duration is not restarted by the second server.
	key decorationKey

	version   string
	gitCommit string
	buildDate string
	// name resolves this node's fleet-wide name, which is also its history
	// instance label. A function, so a reload of fleet.local_name is read.
	name func() string
	// thresholds supplies the alert rules. A function, so a reload of the
	// alerts section takes effect at the next collection.
	thresholds func() config.Alerts
	eval       *alert.Evaluator
}

var _ monitor.Decorator = (*snapshotDecorator)(nil)

// decorationKey identifies what a snapshotDecorator is derived from.
type decorationKey struct {
	version, gitCommit, buildDate string
	cfg                           *config.Manager
	fleet                         *fleet.Poller
}

// decorationKeyOf is the key a decorator built for s would have.
func decorationKeyOf(s *Server) decorationKey {
	return decorationKey{
		version: s.version, gitCommit: s.gitCommit, buildDate: s.buildDate,
		cfg: s.cfg, fleet: s.fleet,
	}
}

// newSnapshotDecorator builds the decorator for s. Without a configuration
// manager the thresholds are the built-in defaults, which are the thresholds
// a configuration file that sets none would give.
func newSnapshotDecorator(s *Server) *snapshotDecorator {
	thresholds := func() config.Alerts { return config.Defaults().Alerts }
	if s.cfg != nil {
		thresholds = func() config.Alerts { return s.cfg.Current().Alerts }
	}
	return &snapshotDecorator{
		key:        decorationKeyOf(s),
		version:    s.version,
		gitCommit:  s.gitCommit,
		buildDate:  s.buildDate,
		name:       s.localName,
		thresholds: thresholds,
		eval:       alert.New(),
	}
}

// Identify stamps the build and the node's name.
func (d *snapshotDecorator) Identify(snap *types.Snapshot) {
	snap.Version = d.version
	snap.GitCommit = d.gitCommit
	snap.BuildDate = d.buildDate
	snap.NodeName = d.name()
}

// Evaluate attaches the alerts the current thresholds produce, as of when the
// reading was taken. Rates are the change between two readings over the time
// between them, so the time must be the readings' own: evaluating a reading
// published a minute before this server was built at the build time would
// divide a minute's change by a moment. The monitor dates every reading it
// publishes; the clock is used only for one that somehow is not.
func (d *snapshotDecorator) Evaluate(snap *types.Snapshot) {
	at := snap.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	snap.Alerts = d.eval.Evaluate(d.thresholds(), snap, at)
}

// installDecorator makes sure s's monitor is decorated, installing s's own
// decorator if it has none, and returns the decorator that was already there
// when it did not install -- nil when it did.
//
// A reload hook is registered only for a decorator that was installed, since
// a configuration manager keeps its hooks for good. A reload that lands
// between the installation and the registration is caught by one restamp
// after registering, which reads whatever configuration is current by then.
func (s *Server) installDecorator() monitor.Decorator {
	if s.mon == nil {
		// Tolerated as newDeltaHub tolerates it: nothing is served.
		return nil
	}
	d := newSnapshotDecorator(s)
	for {
		if cur := s.mon.Decorator(); cur != nil {
			return cur
		}
		if err := s.mon.SetDecorator(d); err == nil {
			break
		}
		// Another server installed one between the two calls; look again.
	}
	if s.cfg != nil {
		s.cfg.OnReload(func(*config.Config) error {
			if s.mon.Decorator() == monitor.Decorator(d) {
				s.mon.Restamp()
			}
			return nil
		})
		s.mon.Restamp()
	}
	return nil
}

// decorate installs s's decorator on its monitor or shares the one there.
//
// A server with the same key describes the same node the same way and shares
// it. A different one is refused with DecoratorInstalledError: one monitor
// describes one node, and two descriptions would each silently overwrite the
// other's build, name, thresholds and alert history for every reader.
func (s *Server) decorate() error {
	cur := s.installDecorator()
	if cur == nil {
		return nil
	}
	if shared, ok := cur.(*snapshotDecorator); ok && shared.key == decorationKeyOf(s) {
		return nil
	}
	return &types.DecoratorInstalledError{}
}
