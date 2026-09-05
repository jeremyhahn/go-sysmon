//go:build !prometheus

package server

// HistoryQuerier and MetricsExporter exist in this build so Config keeps the
// same fields, and are `any` because nothing in this build can construct one:
// pkg/prom is not linked (#100).
//
// The fields remain rather than disappearing behind a tag of their own so that
// a caller compiles against one server.Config whichever way the binary is
// built, and a nil is the only value either field can hold here.
type HistoryQuerier = any

// MetricsExporter is the same, for the remote-write side.
type MetricsExporter = any
