//go:build !prometheus

package server

import (
	"net/http"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The history endpoints exist in every build and answer the same shape.
//
// A binary without prometheus support answers Enabled=false, exactly as a node
// with no query URL configured does -- NOT 404. The dashboard hides the date
// picker on a JSON envelope saying history is unavailable; on a 404 it would be
// looking at a failure, and the picker's absence would stop being a decision
// and become a symptom (owner ruling 2026-09-04, #100).
//
// The two builds are therefore indistinguishable to a client that has no
// history, which is the point: "this node keeps no history" and "this build
// cannot" are the same answer to the only question the dashboard asks.
func (s *Server) handleHistoryMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, types.HistoryCatalog{Enabled: false})
}

// handleHistory answers the same way for the same reason.
func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, types.History{Enabled: false})
}
