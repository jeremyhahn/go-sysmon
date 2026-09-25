//go:build integration

// Package config runs `sysmon serve` against real configuration files and
// environments: what it refuses to start on, which layer each setting comes
// from, reloads by SIGHUP and by the file watcher, logging, the systemd
// notification protocol, and how it shuts down.
package config

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
