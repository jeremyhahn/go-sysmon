//go:build prometheus

// Tagged because the real history handlers exist only in the prometheus build (#100).
package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestTree_HistoryIsAnsweredByTheAggregatorThatRecordsIt: each aggregator
// records its own direct peers (topology A at every level, #190), so the root
// relays a history request for a node two levels down to the child that
// records it, and passes the child's answer on as it came. The root's own
// Prometheus is never asked.
func TestTree_HistoryIsAnsweredByTheAggregatorThatRecordsIt(t *testing.T) {
	rootProm, midProm := newFakePrometheus(t), newFakePrometheus(t)
	querier := func(f *fakePrometheus) HistoryQuerier {
		return prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: f.srv.URL, Timeout: 5 * time.Second}
		})
	}

	a1 := startTreeNode(t, namedSnapshotter{name: "a1"})
	mid := startTreeNodeWithHistory(t, namedSnapshotter{name: "mid"}, querier(midProm), a1)
	waitFor(t, "mid sees a1", func() bool { return reachableCount(fleetOf(t, mid, "")) == 2 })
	root := startTreeNodeWithHistory(t, namedSnapshotter{name: "root"}, querier(rootProm), mid)
	waitFor(t, "root sees a1", func() bool { return reachableCount(fleetOf(t, root, "")) == 3 })

	var h types.History
	if code := getJSON(t, root.srv, "/api/history?metric=cpu&node=a1", &h); code != http.StatusOK {
		t.Fatalf("history for a1 through root = %d", code)
	}
	if h.Node != "a1" || !h.Enabled || len(h.Series) == 0 {
		t.Errorf("relayed history = %+v; want mid's answer for a1", h)
	}
	if q := midProm.seen.Get("query"); !strings.Contains(q, `instance="a1"`) {
		t.Errorf("mid's Prometheus was asked %q; want instance a1", q)
	}
	if rootProm.seen != nil {
		t.Errorf("root's own Prometheus was asked %q for a node it does not record", rootProm.seen.Get("query"))
	}

	// mid itself is root's own peer: root answers it from its own history.
	if code := getJSON(t, root.srv, "/api/history?metric=cpu&node=mid", &h); code != http.StatusOK {
		t.Fatalf("history for mid = %d", code)
	}
	if q := rootProm.seen.Get("query"); !strings.Contains(q, `instance="mid"`) {
		t.Errorf("root's Prometheus was asked %q for its own peer mid", q)
	}

	// A malformed range is refused where it is relayed to, and the refusal
	// comes back as it was given.
	if rec := get(t, root.srv, "/api/history?metric=cpu&node=a1&start=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad range relayed = %d, want the child's 400", rec.Code)
	}

	// The child that records a1 stops answering: the relay fails at the
	// gateway, naming no address, and the root does not answer from its own
	// Prometheus instead.
	mid.ts.Close()
	rootProm.seen = nil
	// A range not asked before: an identical relay within the last second is
	// answered from the one before it (relayReuse), which is what it is for.
	rec := get(t, root.srv, fmt.Sprintf("/api/history?metric=cpu&node=a1&end=%d", time.Now().UnixMilli()))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("history through a down child = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("relay error carries an address: %s", rec.Body.String())
	}
	if rootProm.seen != nil {
		t.Errorf("root answered a1's history from its own Prometheus: %q", rootProm.seen.Get("query"))
	}
}

// TestTree_HistoryIsRelayedByAParentWithNoHistoryOfItsOwn: a parent that
// records nothing itself still routes a subtree node's history to the child
// that records it. The relay used to sit behind the parent's own "is history
// enabled" check, so the parent answered {"enabled":false} for a node whose
// history was one hop away.
func TestTree_HistoryIsRelayedByAParentWithNoHistoryOfItsOwn(t *testing.T) {
	midProm := newFakePrometheus(t)
	a1 := startTreeNode(t, namedSnapshotter{name: "a1"})
	mid := startTreeNodeWithHistory(t, namedSnapshotter{name: "mid"}, prom.NewQuerier(func() prom.QueryOptions {
		return prom.QueryOptions{BaseURL: midProm.srv.URL, Timeout: 5 * time.Second}
	}), a1)
	waitFor(t, "mid sees a1", func() bool { return reachableCount(fleetOf(t, mid, "")) == 2 })
	root := startTreeNode(t, namedSnapshotter{name: "root"}, mid)
	waitFor(t, "root sees a1", func() bool { return reachableCount(fleetOf(t, root, "")) == 3 })

	var h types.History
	if code := getJSON(t, root.srv, "/api/history?metric=cpu&node=a1", &h); code != http.StatusOK {
		t.Fatalf("history for a1 through a parent with no history = %d", code)
	}
	if !h.Enabled || h.Node != "a1" || len(h.Series) == 0 {
		t.Errorf("relayed history = %+v; want mid's answer for a1", h)
	}
}
