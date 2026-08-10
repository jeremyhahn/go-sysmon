//go:build !desktop

package main

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- helpers --------------------------------------------------------------

// newTestMonitor creates and starts a monitor backed by a static snapshot.
func newTestMonitor(t *testing.T, hostname string) *monitor.Monitor {
	t.Helper()
	stub := &monitorStub{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: hostname}},
	}
	m, err := monitor.New(stub, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck
	return m
}

// monitorStub is a minimal monitor.Snapshotter.
type monitorStub struct {
	snapshot *types.Snapshot
	err      error
}

func (s *monitorStub) Snapshot() (*types.Snapshot, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

// waitSnapshot blocks until the monitor has at least one snapshot or the
// timeout expires.
func waitSnapshot(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := m.Snapshot(); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("monitor never produced a snapshot within 500 ms")
}

// ---- NewMonitorBinding ----------------------------------------------------

func TestNewMonitorBinding_NotNil(t *testing.T) {
	m := newTestMonitor(t, "binding-host")
	b := NewMonitorBinding(m, nil)
	if b == nil {
		t.Fatal("NewMonitorBinding returned nil")
	}
}

func TestNewMonitorBinding_StoresMonitor(t *testing.T) {
	m := newTestMonitor(t, "store-host")
	b := NewMonitorBinding(m, nil)
	// Verify the binding is usable – GetSnapshot delegates to the monitor.
	waitSnapshot(t, m)
	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot after NewMonitorBinding: %v", err)
	}
	if snap.Host.Hostname != "store-host" {
		t.Errorf("Hostname = %q, want %q", snap.Host.Hostname, "store-host")
	}
}

// ---- GetVersion -----------------------------------------------------------

func TestGetVersion_ContainsExpectedKeys(t *testing.T) {
	m := newTestMonitor(t, "ver-host")
	b := NewMonitorBinding(m, nil)
	v := b.GetVersion()

	for _, key := range []string{"version", "gitCommit", "buildDate"} {
		if _, ok := v[key]; !ok {
			t.Errorf("GetVersion() missing key %q", key)
		}
	}
}

func TestGetVersion_ValuesAreStrings(t *testing.T) {
	m := newTestMonitor(t, "ver-host2")
	b := NewMonitorBinding(m, nil)
	v := b.GetVersion()

	// Values must be non-empty strings (the package-level vars are "dev" /
	// "unknown" in test builds).
	for key, val := range v {
		if val == "" {
			t.Errorf("GetVersion()[%q] is empty", key)
		}
	}
}

// ---- GetSnapshot ----------------------------------------------------------

func TestGetSnapshot_ReturnsData(t *testing.T) {
	m := newTestMonitor(t, "snap-host")
	waitSnapshot(t, m)
	b := NewMonitorBinding(m, nil)

	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot() error = %v, want nil", err)
	}
	if snap == nil {
		t.Fatal("GetSnapshot() returned nil snapshot")
	}
	if snap.Host.Hostname != "snap-host" {
		t.Errorf("Hostname = %q, want %q", snap.Host.Hostname, "snap-host")
	}
}

func TestGetSnapshot_MonitorNotStarted(t *testing.T) {
	stub := &monitorStub{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "h"}},
	}
	m, err := monitor.New(stub, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	// Do NOT start the monitor – no snapshot is available yet.
	b := NewMonitorBinding(m, nil)

	_, err = b.GetSnapshot()
	if err == nil {
		t.Fatal("GetSnapshot() error = nil, want MonitorNotRunningError")
	}
	var notRunning *types.MonitorNotRunningError
	if !errors.As(err, &notRunning) {
		t.Errorf("error type = %T, want *types.MonitorNotRunningError", err)
	}
}

// ---- SetInterval ----------------------------------------------------------

func TestSetInterval_ValidValue(t *testing.T) {
	m := newTestMonitor(t, "interval-host")
	sc := collector.NewSystemCollector(slog.Default())
	b := NewMonitorBinding(m, sc)

	if err := b.SetInterval(500); err != nil {
		t.Errorf("SetInterval(500) error = %v, want nil", err)
	}
	// Let the new monitor produce a snapshot then confirm the binding is live.
	deadline := time.Now().Add(2 * time.Second)
	var snap *types.Snapshot
	var err error
	for time.Now().Before(deadline) {
		snap, err = b.GetSnapshot()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetSnapshot after SetInterval: %v", err)
	}
	if snap == nil {
		t.Fatal("GetSnapshot returned nil after SetInterval")
	}
}

func TestSetInterval_ZeroIsInvalid(t *testing.T) {
	m := newTestMonitor(t, "interval-zero-host")
	b := NewMonitorBinding(m, nil)

	err := b.SetInterval(0)
	if err == nil {
		t.Fatal("SetInterval(0) error = nil, want InvalidIntervalError")
	}
	var invalid *types.InvalidIntervalError
	if !errors.As(err, &invalid) {
		t.Errorf("error type = %T, want *types.InvalidIntervalError", err)
	}
}

func TestSetInterval_NegativeIsInvalid(t *testing.T) {
	m := newTestMonitor(t, "interval-neg-host")
	b := NewMonitorBinding(m, nil)

	err := b.SetInterval(-100)
	if err == nil {
		t.Fatal("SetInterval(-100) error = nil, want InvalidIntervalError")
	}
	var invalid *types.InvalidIntervalError
	if !errors.As(err, &invalid) {
		t.Errorf("error type = %T, want *types.InvalidIntervalError", err)
	}
}

func TestSetInterval_ReplacesMonitor(t *testing.T) {
	m := newTestMonitor(t, "replace-host")
	waitSnapshot(t, m)
	sc := collector.NewSystemCollector(slog.Default())
	b := NewMonitorBinding(m, sc)

	// Change interval; the internal monitor should be replaced.
	if err := b.SetInterval(100); err != nil {
		t.Fatalf("SetInterval(100): %v", err)
	}

	// The new monitor should produce snapshots within a reasonable time.
	deadline := time.Now().Add(2 * time.Second)
	var snap *types.Snapshot
	var err error
	for time.Now().Before(deadline) {
		snap, err = b.GetSnapshot()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetSnapshot after SetInterval: %v", err)
	}
	if snap == nil {
		t.Fatal("GetSnapshot returned nil after SetInterval")
	}
}
