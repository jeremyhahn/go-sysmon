//go:build !desktop

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
)

// withServeConfig points the serve command at a temporary config file and
// neutralises the flag-backed package variables, which persist across tests and
// would otherwise be treated as supplied when loadServeConfig is called with no
// cobra command.
func withServeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	oldPath, oldAddr, oldInterval := serveConfigPath, serveAddr, serveIntervalMs
	oldCert, oldKey := serveTLSCert, serveTLSKey
	t.Cleanup(func() {
		serveConfigPath, serveAddr, serveIntervalMs = oldPath, oldAddr, oldInterval
		serveTLSCert, serveTLSKey = oldCert, oldKey
	})
	serveConfigPath = path
	serveAddr, serveTLSCert, serveTLSKey = "", "", ""
	serveIntervalMs = 0

	return path
}

// TestLoadServeConfig_RejectsPeerNamedLikeThisNode proves the collision check is
// actually WIRED UP, not merely available. The check itself lives in pkg/config
// and is tested there; what this covers is that serve registers it, which is the
// part that silently stops working if the registration is dropped.
//
// Startup is the path that matters most: the fleet handlers resolve the local
// name before the peer cache, so a node that boots with this config never polls
// the configured host while showing a card that looks like it.
func TestLoadServeConfig_RejectsPeerNamedLikeThisNode(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: aggregator\n      url: http://10.0.0.1:8081\n")

	var clash *config.ErrPeerIsLocalName
	if _, err := loadServeConfig(nil); !errors.As(err, &clash) {
		t.Fatalf("loadServeConfig() = %v, want a *config.ErrPeerIsLocalName", err)
	} else if clash.Name != "aggregator" {
		t.Errorf("ErrPeerIsLocalName.Name = %q, want %q", clash.Name, "aggregator")
	}
}

// TestLoadServeConfig_RejectsCollisionIntroducedByReload covers the other half
// of the wiring: the same check has to survive a reload, and rejecting has to
// leave the running configuration in place rather than reporting a config that
// already went live.
func TestLoadServeConfig_RejectsCollisionIntroducedByReload(t *testing.T) {
	path := withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() with a valid fleet: %v", err)
	}

	// Rename this node onto its own peer. Only the incoming name reveals the
	// collision -- the outgoing "aggregator" clashes with nothing.
	bad := "fleet:\n  local_name: fs\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	var clash *config.ErrPeerIsLocalName
	if _, err := mgr.Reload(); !errors.As(err, &clash) {
		t.Fatalf("Reload() into a collision = %v, want a *config.ErrPeerIsLocalName", err)
	}
	if got := mgr.Current().LocalName(); got != "aggregator" {
		t.Errorf("after a rejected reload LocalName() = %q, want the previous %q", got, "aggregator")
	}
}

// TestLoadServeConfig_AcceptsAnOrdinaryFleet is the no-regression guard: the
// validator must not reject a fleet whose peers are simply other machines.
func TestLoadServeConfig_AcceptsAnOrdinaryFleet(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: gpu-host\n      url: http://10.0.0.2:8081\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() with an ordinary fleet = %v, want it accepted", err)
	}
	if n := len(mgr.Current().Fleet.Peers); n != 2 {
		t.Errorf("peers = %d, want 2", n)
	}
}
