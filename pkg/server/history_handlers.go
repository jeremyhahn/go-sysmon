package server

import (
	"errors"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// maxHistoryWindow bounds how far back one request may ask.
//
// It is not a retention policy -- Prometheus owns that -- but a bound on what a
// single unauthenticated request can make the server and the database do. A
// year at the resolution the picker offers is a query the receiver will chew on
// for a long time, and this endpoint is reachable by anything that can route to
// the host.
const maxHistoryWindow = 90 * 24 * time.Hour

// handleHistoryMetrics advertises what history is available.
//
// A node with no query URL answers Enabled=false rather than 404, the same way
// the fleet endpoint does, so the dashboard can tell "this node keeps no
// history" from "this build has no history support" and hide the date picker
// instead of showing a broken one.
func (s *Server) handleHistoryMetrics(w http.ResponseWriter, _ *http.Request) {
	if !s.historyEnabled() {
		writeJSON(w, types.HistoryCatalog{Enabled: false})
		return
	}
	catalog := prom.Catalog()
	out := types.HistoryCatalog{Enabled: true, Metrics: make([]types.HistoryMetric, 0, len(catalog))}
	for _, m := range catalog {
		out.Metrics = append(out.Metrics, types.HistoryMetric{
			Key: m.Key, Label: m.Label, Unit: m.Unit, SplitBy: m.SplitBy,
		})
	}
	writeJSON(w, out)
}

// handleHistory answers one date range for one metric on one node.
//
// The query is built here from a fixed catalog rather than taken from the
// request. Forwarding an expression would make this an open PromQL proxy for
// everything in the time-series database, on an endpoint with no authentication
// of its own -- see the note in pkg/prom/catalog.go.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !s.historyEnabled() {
		writeJSON(w, types.History{Enabled: false})
		return
	}

	q := r.URL.Query()
	metric, err := prom.Lookup(q.Get("metric"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	node := q.Get("node")
	if node == "" {
		node = s.localName()
	}
	// The node must be one this dashboard knows: itself, or a configured peer.
	// A syntactically valid name is not enough. Several nodes write into one
	// Prometheus, and this endpoint has no authentication of its own, so
	// forwarding any well-formed name would let anyone able to reach one node's
	// dashboard read the history of every machine in the database -- including
	// hosts this one is not meant to show.
	if !s.knownNode(node) {
		http.Error(w, (&prom.ErrUnknownNode{Node: node}).Error(), http.StatusBadRequest)
		return
	}
	expr, err := metric.Expr(node)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start, end, step, err := historyWindow(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	series, err := s.history.QueryRange(r.Context(), expr, start, end, step)
	if err != nil {
		// A range this server will not serve is the caller's mistake, and the
		// message is this package's own: it says what to ask for instead, and
		// discloses nothing.
		var badRange *prom.ErrInvalidRange
		if errors.As(err, &badRange) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Everything else is the receiver's answer. 502 says the dashboard
		// reached go-sysmon and go-sysmon could not reach Prometheus, which is
		// the distinction an operator needs -- but the detail stays in the log.
		// The underlying errors carry the query_url and whatever text the
		// receiver returned, and this endpoint is unauthenticated: the address
		// of the fleet's time-series database is reconnaissance, the same
		// reason the settings view redacts it.
		slog.Warn("server: history query failed", "metric", metric.Key, "node", node, "error", err)
		http.Error(w, "cannot read history from prometheus; see the server log",
			http.StatusBadGateway)
		return
	}

	writeJSON(w, types.History{
		Enabled: true,
		Node:    node,
		Metric:  metric.Key,
		Label:   metric.Label,
		Unit:    metric.Unit,
		StartMs: start.UnixMilli(),
		EndMs:   end.UnixMilli(),
		StepMs:  step.Milliseconds(),
		Series:  toHistorySeries(metric, series),
	})
}

// knownNode reports whether name is a node this dashboard is meant to show:
// itself, or one of its configured fleet peers.
//
// The configured peer list is preferred over the poller's view, because a peer
// that is currently unreachable is still a node whose past this dashboard
// should answer for -- and its history is exactly what someone asks for when it
// has just gone.
func (s *Server) knownNode(name string) bool {
	if name == "" {
		return false
	}
	if name == s.localName() {
		return true
	}
	if s.cfg != nil {
		for _, p := range s.cfg.Current().Fleet.Peers {
			if p.Name == name {
				return true
			}
		}
		return false
	}
	if s.fleet != nil {
		for _, n := range s.fleet.View().Nodes {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// historyEnabled reports whether this node can answer a range. It is checked
// per request rather than at construction because prometheus.query_url is
// reloadable: turning history on must not need a restart, which would drop
// every connected browser to gain a setting.
func (s *Server) historyEnabled() bool {
	return s.history != nil && s.history.Enabled()
}

// localName is the name this node publishes to the fleet, which is also the
// instance label its samples are written under. Without a fleet it falls back
// to the configured local name, and finally to the hostname, so the two can
// never disagree about which series belong to this machine.
func (s *Server) localName() string {
	if s.fleet != nil {
		return s.fleet.LocalName()
	}
	if s.cfg != nil {
		return s.cfg.Current().LocalName()
	}
	// Neither is required. server.Config makes History independent of the
	// fleet and of the configuration manager, so an embedded caller can wire
	// up history alone -- and an empty name is not a node, so every history
	// request for the local node was refused as unknown. The same resolution
	// the configuration makes, from a zero Config, gives the hostname and
	// never an empty string.
	return (&config.Config{}).LocalName()
}

// historyWindow reads start, end and step from the query string.
//
// start and end are Unix milliseconds, matching every other timestamp this API
// serves. A missing step is chosen for the window rather than rejected: the
// caller asked for a date range, not for a resolution, and picking one is the
// server's job precisely because the ceiling that makes it necessary is the
// server's to know.
func historyWindow(q map[string][]string) (start, end time.Time, step time.Duration, err error) {
	endMs, err := msParam(q, "end", time.Now().UnixMilli())
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	startMs, err := msParam(q, "start", endMs-time.Hour.Milliseconds())
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}

	start, end = time.UnixMilli(startMs), time.UnixMilli(endMs)
	if !end.After(start) {
		return time.Time{}, time.Time{}, 0, &prom.ErrInvalidRange{Message: "end must be after start"}
	}
	if window := end.Sub(start); window > maxHistoryWindow {
		return time.Time{}, time.Time{}, 0, &prom.ErrInvalidRange{
			Message: "the range is " + window.Round(time.Hour).String() +
				"; at most " + maxHistoryWindow.String() + " may be requested at once",
		}
	}

	stepMs, err := msParam(q, "step", 0)
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	if stepMs <= 0 {
		return start, end, prom.StepFor(end.Sub(start)), nil
	}
	return start, end, time.Duration(stepMs) * time.Millisecond, nil
}

// msParam reads one integer-millisecond parameter, defaulting when absent.
func msParam(q map[string][]string, name string, fallback int64) (int64, error) {
	values := q[name]
	if len(values) == 0 || values[0] == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		return 0, &prom.ErrInvalidRange{Message: name + " must be an integer number of milliseconds"}
	}
	return v, nil
}

// toHistorySeries reduces Prometheus' label sets to the one label that
// distinguishes the lines of this metric from each other, and orders them by
// it.
//
// The full label set is deliberately not passed through. Everything else on it
// is either constant across the response (instance, job) or an operator's
// external labels, and a chart legend has no use for either -- while forwarding
// them would publish the fleet's Prometheus labelling to any browser that can
// reach the dashboard.
func toHistorySeries(m prom.Metric, series []prom.RangeSeries) []types.HistorySeries {
	out := make([]types.HistorySeries, 0, len(series))
	for _, s := range series {
		name := ""
		if m.SplitBy != "" {
			name = s.Labels[m.SplitBy]
		}
		points := make([]types.HistoryPoint, 0, len(s.Points))
		for _, p := range s.Points {
			points = append(points, types.HistoryPoint{TimeMs: p.TimeMs, Value: p.Value})
		}
		out = append(out, types.HistorySeries{Name: name, Points: points})
	}
	// Prometheus does not promise an order, and an unordered legend that
	// reshuffles between requests is the same defect the fleet list had.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
