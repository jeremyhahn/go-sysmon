//go:build !desktop

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
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
	waitSnapshot(t, m)

	if err := b.SetInterval(500); err != nil {
		t.Errorf("SetInterval(500) error = %v, want nil", err)
	}

	// The reading that existed before the change is still there immediately
	// after it. This used to need a thirty-second wait, because SetInterval
	// REBUILT the monitor on the real SystemCollector and the test had to sit
	// through a genuine first collection -- SMBIOS, every GPU, every disk, the
	// whole process table (issue #34's flake). Adjusting the cadence in place
	// removes the wait because it removes the restart: there is nothing to
	// collect again (#97).
	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot straight after SetInterval: %v", err)
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

// TestSetInterval_AdjustsInPlace pins the behaviour the server has always had
// and the desktop did not: the same control changes the cadence and keeps
// everything else.
//
// It used to stop the monitor, construct a new one and start it, so a user
// changing the polling rate also silently discarded every reading the monitor
// had accumulated -- a second, unnamed action riding on a control called
// "polling interval" (#97).
func TestSetInterval_AdjustsInPlace(t *testing.T) {
	m := newTestMonitor(t, "in-place-host")
	waitSnapshot(t, m)
	sc := collector.NewSystemCollector(slog.Default())
	b := NewMonitorBinding(m, sc)

	before, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot before SetInterval: %v", err)
	}

	if err := b.SetInterval(100); err != nil {
		t.Fatalf("SetInterval(100): %v", err)
	}

	if got := b.monitor(); got != m {
		t.Error("SetInterval replaced the monitor; it must adjust the running one")
	}
	if got := m.Interval(); got != 100*time.Millisecond {
		t.Errorf("Interval() = %v after SetInterval(100), want 100ms", got)
	}

	// No gap: the reading collected before the change is still being served
	// the instant after it, rather than the monitor starting from nothing.
	after, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot straight after SetInterval: %v", err)
	}
	if after == nil {
		t.Fatal("the monitor had no snapshot immediately after an interval change")
	}
	if before != nil && after.Host.Hostname != before.Host.Hostname {
		t.Errorf("the served snapshot changed identity across an interval change: %q -> %q",
			before.Host.Hostname, after.Host.Hostname)
	}
}

// TestStreamToFrontend_SendsDeltasNotSnapshots is #98's contract on the desktop
// side: the window gets one whole snapshot and patches after it.
//
// It emitted the entire snapshot on every tick, which on a ten-node fleet host
// measured 367,126 bytes per frame against a delta averaging 7,842 -- 47 times
// the bytes for identical information, on a machine that already had a correct
// delta hub it never called.
func TestStreamToFrontend_SendsDeltasNotSnapshots(t *testing.T) {
	var mu sync.Mutex
	var names []string
	var sizes []int

	m := newTestMonitor(t, "delta-host")
	waitSnapshot(t, m)
	b := NewMonitorBinding(m, collector.NewSystemCollector(slog.Default()))
	b.ctx = context.Background()
	// Observed per binding, so a stream started by another test cannot be
	// mistaken for this one's -- which it was, until this test failed in the
	// package run and passed alone.
	b.emit = func(_ context.Context, name string, payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, name)
		sizes = append(sizes, len(payload))
	}

	go b.streamToFrontend(context.Background())
	t.Cleanup(func() {
		b.mu.RLock()
		cancel := b.streamCancel
		b.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		enough := len(names) >= 3
		mu.Unlock()
		if enough {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(names) < 3 {
		t.Fatalf("got %d frames in 3s, want at least 3", len(names))
	}
	if names[0] != "full" {
		t.Errorf("first frame is %q, want full -- there is nothing to patch yet", names[0])
	}
	for i, name := range names[1:] {
		if name != "full" {
			continue
		}
		t.Errorf("frame %d is a full snapshot; the whole measurement is being resent every tick", i+1)
	}
	for i, size := range sizes[1:] {
		if size >= sizes[0] {
			t.Errorf("frame %d is %d bytes against a first frame of %d; it is not a patch", i+1, size, sizes[0])
		}
	}
}
