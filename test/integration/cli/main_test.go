//go:build integration

// Package cli runs every sysmon subcommand and the server's public endpoints
// end to end, against the real host inside the devcontainer.
package cli

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
