package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

// TestEndpoints_NoListIsEverNull is #151 at the server's edge: every JSON
// endpoint the dashboard reads, in the state where its lists are most likely
// to be nil -- a standalone node with no fleet, no history, no configuration
// manager, and a snapshot with nothing in it -- sends [] for every list.
//
// It runs in both build shapes, since the history handlers are compiled from a
// different file in each.
func TestEndpoints_NoListIsEverNull(t *testing.T) {
	mon := startedMonitor(t, idleSnapshotter{}, 10*time.Millisecond)
	awaitPublications(t, mon, 1)
	srv, err := NewWithConfig(Config{Monitor: mon, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	tests := []struct {
		path string
		like any
	}{
		{"/api/snapshot", types.Snapshot{}},
		{"/api/fleet", types.Fleet{}},
		{"/api/config", types.ConfigView{}},
		{"/api/history/metrics", types.HistoryCatalog{}},
		{"/api/history?metric=cpu", types.History{}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			nulls, err := typestest.NullSlices(rec.Body.Bytes(), tt.like)
			if err != nil {
				t.Fatalf("NullSlices: %v", err)
			}
			if len(nulls) != 0 {
				t.Errorf("lists encoded as null: %v\nbody: %s", nulls, rec.Body.String())
			}
		})
	}
}

// TestWriteJSON_NormalisesItsCopyNotTheCallersValue: the response carries []
// for a nil list, and the value the caller passed in is not written to --
// writeJSON takes it by value, and the walk runs over that copy.
func TestWriteJSON_NormalisesItsCopyNotTheCallersValue(t *testing.T) {
	view := types.History{Enabled: true}

	rec := httptest.NewRecorder()
	writeJSON(rec, view)

	nulls, err := typestest.NullSlices(rec.Body.Bytes(), view)
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(nulls) != 0 {
		t.Errorf("lists encoded as null: %v", nulls)
	}
	if view.Series != nil {
		t.Error("writeJSON wrote to the caller's value rather than its own copy")
	}
}
