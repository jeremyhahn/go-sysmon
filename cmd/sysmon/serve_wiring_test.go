//go:build !desktop

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withServeConfig points the serve command at a temporary config file and
// returns the flag-backed package variables -- which persist across tests -- to
// the defaults they were registered with, which is how "no flag was supplied"
// is spelled when loadServeConfig is called with no cobra command.
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

	d := config.Defaults()
	serveConfigPath = path
	serveAddr, serveTLSCert, serveTLSKey = d.Server.Addr, d.Server.TLSCert, d.Server.TLSKey
	serveIntervalMs = d.Server.IntervalMs

	return path
}

// TestLoadServeConfig_UnsuppliedAddrLeavesTheFileInCharge is the regression
// this issue is really about (#17). The flag layer outranks the config file, so
// a flag default that reaches it is not merely redundant -- it wins. The
// aggregator's sysmon.yaml asks for :8081; a serve loop that handed the
// built-in ":8080" to the flag layer would bind that instead and report it as
// having come from a flag nobody typed.
func TestLoadServeConfig_UnsuppliedAddrLeavesTheFileInCharge(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != ":8081" {
		t.Errorf("server.addr = %q, want %q from the config file", got, ":8081")
	}
	if got := mgr.Sources()["server.addr"]; got != config.SourceFile {
		t.Errorf("source = %q, want %q", got, config.SourceFile)
	}
}

// TestLoadServeConfig_SuppliedAddrOutranksTheFile is the other half: a flag the
// operator actually gave still wins, which is what makes `--addr` worth having.
func TestLoadServeConfig_SuppliedAddrOutranksTheFile(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")
	serveAddr = "127.0.0.1:9099"

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != "127.0.0.1:9099" {
		t.Errorf("server.addr = %q, want %q from the flag", got, "127.0.0.1:9099")
	}
	if got := mgr.Sources()["server.addr"]; got != config.SourceFlag {
		t.Errorf("source = %q, want %q", got, config.SourceFlag)
	}
}

// TestLoadServeConfig_EmptyAddrFlagIsNotAValue covers the bad input: `--addr ""`
// names no address, so it must not displace the file and leave normalize to put
// the default back under a "flag" label.
func TestLoadServeConfig_EmptyAddrFlagIsNotAValue(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")
	serveAddr = ""

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != ":8081" {
		t.Errorf("server.addr = %q, want %q from the config file", got, ":8081")
	}
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

// ---- build stamping --------------------------------------------------------

// buildStub is a Snapshotter that reports nothing about itself, so whatever a
// stamped snapshot carries came from the stamper.
type buildStub struct{ err error }

func (b buildStub) Snapshot() (*types.Snapshot, error) {
	if b.err != nil {
		return nil, b.err
	}
	return &types.Snapshot{Host: types.HostInfo{Hostname: "wiring-test"}}, nil
}

// TestEnrichedSnapshotter_StampsTheBuildServingIt is the origin of the fleet
// payload's build identity. The release string has read the same for every
// build this project has produced, so unless the commit and build date are
// stamped here the fleet view has nothing that can tell two builds apart --
// which is #68.
func TestEnrichedSnapshotter_StampsTheBuildServingIt(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() = %v", err)
	}

	snapper := newEnrichedSnapshotter(buildStub{}, mgr, "0.1.0-alpha", "a46f6cf", "2026-09-05T03:26:50Z")
	snap, err := snapper.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() = %v", err)
	}
	if snap.Version != "0.1.0-alpha" {
		t.Errorf("Version = %q, want the release string still stamped", snap.Version)
	}
	if snap.GitCommit != "a46f6cf" || snap.BuildDate != "2026-09-05T03:26:50Z" {
		t.Errorf("build = %q/%q, want a46f6cf/2026-09-05T03:26:50Z",
			snap.GitCommit, snap.BuildDate)
	}
}

// TestEnrichedSnapshotter_CollectionFailureStampsNothing covers the error path:
// a failed collection must surface as an error, not as an empty snapshot
// wearing this node's build. A reader that saw one would believe a reading was
// taken on a build that produced no reading at all.
func TestEnrichedSnapshotter_CollectionFailureStampsNothing(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() = %v", err)
	}

	want := errors.New("collector unavailable")
	snapper := newEnrichedSnapshotter(buildStub{err: want}, mgr, "0.1.0-alpha", "a46f6cf", "2026-09-05T03:26:50Z")
	snap, err := snapper.Snapshot()
	if !errors.Is(err, want) {
		t.Errorf("Snapshot() error = %v, want the collector's own error", err)
	}
	if snap != nil {
		t.Errorf("Snapshot() = %+v, want nil alongside the error", snap)
	}
}
