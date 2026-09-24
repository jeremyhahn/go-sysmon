package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// serveFakeDaemon starts an HTTP server on a unix socket in a temporary
// directory and points runtime discovery at it, so the collector's real
// transport is exercised without a container runtime installed.
func serveFakeDaemon(t *testing.T, mux *http.ServeMux) string {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}

	srv := &http.Server{Handler: mux} //nolint:gosec // test server, no timeouts needed
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting down the fake daemon: %v", err)
		}
		if err := await.Recv(t, "the fake daemon to stop serving", served); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the fake daemon stopped with %v, want http.ErrServerClosed", err)
		}
		// Closing a unix listener normally unlinks its socket already.
		if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("removing %s: %v", sock, err)
		}
	})

	original := runtimeSocketPaths
	runtimeSocketPaths = []struct {
		path   string
		engine string
	}{{sock, "docker"}}
	t.Cleanup(func() { runtimeSocketPaths = original })

	return sock
}

// TestRuntimeCollect_InfoRejected verifies a daemon that answers the socket but
// refuses /info is reported as unavailable rather than as an empty-but-present
// runtime, which would render as a runtime with zero containers.
func TestRuntimeCollect_InfoRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil: a refusing daemon is not a collector failure", err)
	}
	if info := c.Info(); info.Available {
		t.Errorf("Available = true after /info was refused: %+v", info)
	}
}

// TestRuntimeCollect_MalformedInfo verifies a daemon returning something that
// is not JSON is handled the same way as a refusal.
func TestRuntimeCollect_MalformedInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("<html>not json</html>")); err != nil {
			t.Errorf("writing /info: %v", err)
		}
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if info := c.Info(); info.Available {
		t.Errorf("Available = true despite an undecodable /info response: %+v", info)
	}
}

// TestRuntimeCollect_DiskUsageRejected verifies engine details still arrive
// when the expensive /system/df query fails. Image inventory is optional; the
// daemon summary is not.
func TestRuntimeCollect_DiskUsageRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(dockerInfo{
			ServerVersion: "29.1.3", Driver: "overlay2", DockerRootDir: "/var/lib/docker",
			Images: 3, Containers: 2, ContainersRunning: 1,
		}); err != nil {
			t.Errorf("encoding /info: %v", err)
		}
	})
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	// The failed query releases the wait with its own error; the bound only
	// fails a regression that would hang.
	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()
	var statusErr *runtimeStatusError
	if err := c.WaitForDiskUsage(ctx); !errors.As(err, &statusErr) {
		t.Errorf("WaitForDiskUsage() = %v, want the /system/df status error", err)
	}
	if c.diskUsage.Load() != nil {
		t.Errorf("disk usage recorded from a /system/df query that failed: %+v", c.diskUsage.Load())
	}

	info := c.Info()
	if !info.Available || info.Version != "29.1.3" {
		t.Errorf("engine details lost when /system/df failed: %+v", info)
	}
	if len(info.Images) != 0 {
		t.Errorf("got %d images, want none: /system/df failed", len(info.Images))
	}
}

// TestRuntimeCollect_DiskUsageIsThrottled verifies the expensive query is not
// repeated on every cycle. It takes seconds against a real daemon, so running
// it per-tick would stall collection.
func TestRuntimeCollect_DiskUsageIsThrottled(t *testing.T) {
	var dfCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(dockerInfo{ServerVersion: "29.1.3"}); err != nil {
			t.Errorf("encoding /info: %v", err)
		}
	})
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, _ *http.Request) {
		dfCalls.Add(1)
		if err := json.NewEncoder(w).Encode(diskUsageFixture()); err != nil {
			t.Errorf("encoding /system/df: %v", err)
		}
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	for range 4 {
		if err := c.Collect(); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()
	if err := c.WaitForDiskUsage(ctx); err != nil {
		t.Fatalf("WaitForDiskUsage() = %v", err)
	}

	if calls := dfCalls.Load(); calls != 1 {
		t.Errorf("/system/df was queried %d times across 4 cycles, want 1", calls)
	}
}

// TestGetJSON_CancelledContext verifies a cancelled request surfaces as an
// error rather than blocking or returning a partially decoded value.
func TestGetJSON_CancelledContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(dockerInfo{ServerVersion: "29.1.3"}); err != nil {
			t.Errorf("encoding /info: %v", err)
		}
	})
	sock := serveFakeDaemon(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out dockerInfo
	err := getJSON(ctx, unixHTTPClient(sock, time.Second), "/info", &out)
	if err == nil {
		t.Fatal("getJSON() error = nil, want an error for a cancelled context")
	}
}
