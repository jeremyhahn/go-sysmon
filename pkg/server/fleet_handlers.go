package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// handleFleet serves the aggregated fleet view. A node with no peers answers
// with Enabled=false rather than 404, so the dashboard can tell "this build has
// no fleet support" from "this node is standalone".
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	view := types.Fleet{Enabled: false}
	if s.fleet != nil {
		view = s.fleet.View()
	}
	writeJSON(w, view)
}

// handleFleetSnapshot serves one node's full snapshot from the aggregator's
// cache, which is what lets the dashboard render any peer's detail without the
// browser making a cross-origin request.
func (s *Server) handleFleetSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	peer, ok := s.fleet.PeerSnapshot(name)
	if !ok {
		http.Error(w, "no snapshot for node "+name, http.StatusNotFound)
		return
	}
	// The envelope is served even when the peer is unreachable. Answering 5xx
	// would throw away the last good reading, which is often the most useful
	// thing on the page when a host has just gone; answering with a bare
	// snapshot would present it as current. The client gets both the data and
	// the fact that it is stale, and decides how to show it.
	writeJSON(w, peer)
}

// handleConfig serves the effective configuration and each value's origin. It
// is read-only by design: the service holds elevated capabilities on some hosts
// and carries no authentication of its own, so configuration is changed on
// disk and reloaded, never through the dashboard.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeJSON(w, types.ConfigView{})
		return
	}
	view := s.cfg.View()
	if s.mon != nil {
		view = withRunningInterval(view, s.mon.Interval())
	}
	writeJSON(w, view)
}

// withRunningInterval replaces the configured polling interval with the one the
// monitor is actually using.
//
// POST /api/interval changes the monitor directly -- it is what the dashboard's
// refresh-rate selector calls -- so the configured value goes stale the moment
// anyone picks a different rate. Reporting it here would leave GET /api/config
// and GET /api/interval giving two different accounts of one running service,
// which is the confusion the settings view exists to end. The source becomes
// "runtime" so the pair stays self-consistent and an operator can see that the
// number came from the dashboard rather than the file.
func withRunningInterval(view types.ConfigView, running time.Duration) types.ConfigView {
	if running <= 0 {
		return view
	}
	ms := strconv.Itoa(int(running.Milliseconds()))
	for i, e := range view.Entries {
		if e.Key != "server.interval_ms" {
			continue
		}
		// When they agree there is no override to report, and the configured
		// provenance is the more informative answer.
		if e.Value != ms {
			view.Entries[i].Value = ms
			view.Entries[i].Source = config.SourceRuntime
		}
		break
	}
	return view
}

// handleFleetConfig serves one node's effective configuration. Without it the
// settings view would show the aggregator's own settings while a peer is
// selected, which reads as that peer's configuration and is wrong.
func (s *Server) handleFleetConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	if name == s.fleet.LocalName() {
		s.handleConfig(w, r)
		return
	}
	view, err := s.fleet.PeerConfig(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, view)
}

// There is deliberately no reload endpoint. The service carries no
// authentication of its own and is designed to sit behind an authenticating
// proxy, so no HTTP path changes its configuration: settings are edited on disk
// and picked up by SIGHUP or the file watcher, which deliver everything a
// reload endpoint would have. POST /api/interval remains the one runtime-state
// endpoint, and it retunes a transient refresh rate rather than configuration.
