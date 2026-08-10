package collector

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnOnce_LogsOnlyTheFirstTime is the regression test for a permanent
// condition logged on every collection cycle. The SMBIOS warning fired once a
// second and accounted for 85% of a server's log volume.
func TestWarnOnce_LogsOnlyTheFirstTime(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	for i := 0; i < 50; i++ {
		warnOnce(logger, "test:condition", "a permanent condition", "attempt", i)
	}

	got := strings.Count(buf.String(), "a permanent condition")
	if got != 1 {
		t.Errorf("logged %d times, want exactly 1", got)
	}
	// The first call is the one that reports, so its context must survive.
	if !strings.Contains(buf.String(), "attempt=0") {
		t.Errorf("first call's attributes missing; got:\n%s", buf.String())
	}
}

// TestWarnOnce_DistinctKeysEachWarn verifies per-device conditions are not
// collapsed into one another: six unreadable disks must produce six warnings.
func TestWarnOnce_DistinctKeysEachWarn(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	for _, dev := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
		for i := 0; i < 5; i++ {
			warnOnce(logger, "disk:smart:"+dev, "SMART unavailable", "device", dev)
		}
	}

	if got := strings.Count(buf.String(), "SMART unavailable"); got != 3 {
		t.Errorf("logged %d times, want 3 (one per device)", got)
	}
	for _, dev := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
		if !strings.Contains(buf.String(), dev) {
			t.Errorf("device %s never warned; got:\n%s", dev, buf.String())
		}
	}
}

// TestWarnOnce_NilLoggerDoesNotPanic covers a collector constructed without a
// logger.
func TestWarnOnce_NilLoggerDoesNotPanic(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("warnOnce panicked with a nil logger: %v", r)
		}
	}()
	warnOnce(nil, "test:nil-logger", "message")
}

// TestResetWarnOnce_ClearsState verifies the test helper actually resets, so
// cases cannot leak state into one another.
func TestResetWarnOnce_ClearsState(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnOnce(logger, "test:reset", "first")
	resetWarnOnce()
	warnOnce(logger, "test:reset", "first")

	if got := strings.Count(buf.String(), "first"); got != 2 {
		t.Errorf("logged %d times after a reset, want 2", got)
	}
}
