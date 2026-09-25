//go:build integration

// Package prom runs the server build (prometheus,nvml) against an in-process
// stand-in for Prometheus: a remote-write receiver that decodes and checks
// what it is sent the way Prometheus does, and a query API that answers range
// queries from exactly what was written. It covers remote write, the history
// endpoints, the history cadence watch, the scrape endpoint, and an
// aggregator writing its peers' history.
package prom

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
