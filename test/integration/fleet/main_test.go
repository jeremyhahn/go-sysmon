//go:build integration

// Package fleet runs fleets of real sysmon processes: trees of aggregators,
// dead and misbehaving peers, and TLS between them.
package fleet

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
