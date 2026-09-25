//go:build integration

// Package server runs `sysmon serve` end to end as a client and a reverse
// proxy see it: every API endpoint and its refusals, the security headers,
// TLS, the event stream in both of its protocols, alerts raised by the real
// host, and a graceful shutdown with a stream open. It also embeds pkg/server
// in a TLS application of its own, the way the library is meant to be used.
package server

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
