//go:build prometheus

package server

import (
	"context"
	"errors"
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
	start, end, step, err := historyWindow(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	series, err := s.readHistory(r.Context(), metric, node, start, end, step)
	if err != nil {
		// A request this server will not serve is the caller's mistake, and
		// the message is this package's own: it says what to ask for instead,
		// and discloses nothing.
		var badRange *prom.ErrInvalidRange
		var badNode *prom.ErrInvalidInstance
		if errors.As(err, &badRange) || errors.As(err, &badNode) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A database that does not publish its lookback cannot have a
		// counter drawn from it: the rate window is measured against that
		// lookback (pkg/prom/lookback.go). It is said as such -- a fact about
		// the database, not a failure of this request -- and without the
		// database's address, which is kept to the log as below.
		var unpublished *prom.ErrLookbackUnpublished
		if errors.As(err, &unpublished) {
			slog.Warn("server: counter history unavailable", "metric", metric.Key, "node", node, "error", err)
			http.Error(w, "counter history needs the time-series database's query lookback, "+
				"which it does not publish; see the server log", http.StatusBadGateway)
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

// readHistory runs one metric's range query for one node.
//
// A counter is read in two steps. The first measures the longest gap between
// its samples over the range and the windows reaching back before it, from
// the samples actually stored (prom.Querier.RateGap); the second takes its
// rate over a window sized from that gap and the step, so every window holds
// the two samples rate() needs whatever cadence the series was written at --
// by this node, by a peer, or under settings since changed. A range with no
// two samples close enough to measure has nothing to take a rate over, and is
// answered as the empty range it is.
func (s *Server) readHistory(ctx context.Context, metric prom.Metric, node string,
	start, end time.Time, step time.Duration) ([]prom.RangeSeries, error) {
	// One database and one deadline for the whole read. A counter is several
	// requests -- the lookback, the gap measurement, the rate -- and a reload
	// between them must not send the rate to a different database from the
	// one its window was measured on; nor may a timeout per request let them
	// add up past the server's own write deadline, so the answer could never
	// be sent.
	// The deadline is also held inside the server's own write budget: a
	// configured timeout longer than it would keep the read going past the
	// point where even a 502 can be sent.
	history := s.history.Pinned()
	deadline := drillInBudget()
	if timeout := history.Timeout(); timeout > 0 {
		deadline = min(deadline, timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	if err := prom.ValidateRange(start, end, step); err != nil {
		return nil, err
	}
	var gap time.Duration
	if metric.Counter {
		selector, err := metric.Selector(node)
		if err != nil {
			return nil, err
		}
		// The measurement reads before start as the rate query does, and is
		// held to the same allowance.
		gap, err = history.RateGap(ctx, selector, start, end, step, maxHistoryWindow-end.Sub(start))
		if err != nil {
			return nil, err
		}
		if gap == 0 {
			return []prom.RangeSeries{}, nil
		}
	}

	expr, err := metric.Expr(node, step, gap)
	if err != nil {
		return nil, err
	}
	// A counter reads its rate window before start as well, and that window
	// grows with the step -- which the caller chooses. Without counting it, a
	// one-hour range at a one-year step would pass the bound on the range and
	// have Prometheus scan a year of samples: the work that bound exists to cap.
	lookback, err := metric.Lookback(step, gap)
	if err != nil {
		return nil, err
	}
	if metric.Counter {
		// The rate is gated on the counter's presence with an instant
		// selector, which reads one database lookback before start; the
		// read reaches back whichever is further. Cached: RateGap has just
		// read it.
		dbLookback, err := history.Lookback(ctx)
		if err != nil {
			return nil, err
		}
		lookback = max(lookback, dbLookback)
	}
	// Compared against what the range leaves of the allowance rather than
	// added to it: historyWindow has already held the range to the bound, so
	// the subtraction cannot overflow, while a step near the largest duration
	// makes a lookback that the addition would wrap negative -- and pass.
	if lookback > maxHistoryWindow-end.Sub(start) {
		return nil, &prom.ErrInvalidRange{
			Message: "the range and its rate window exceed " + maxHistoryWindow.String() +
				", the most that may be read at once; narrow the range or the step",
		}
	}
	return history.QueryRange(ctx, expr, start, end, step)
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
	return s.fleet != nil && s.fleet.Has(name)
}

// historyEnabled reports whether this node can answer a range. It is checked
// per request rather than at construction because prometheus.query_url is
// reloadable: turning history on must not need a restart, which would drop
// every connected browser to gain a setting.
func (s *Server) historyEnabled() bool {
	return s.history != nil && s.history.Enabled()
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
