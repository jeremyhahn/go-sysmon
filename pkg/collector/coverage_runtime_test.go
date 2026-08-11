package collector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(sock)
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
		_, _ = w.Write([]byte("<html>not json</html>"))
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
		_ = json.NewEncoder(w).Encode(dockerInfo{
			ServerVersion: "29.1.3", Driver: "overlay2", DockerRootDir: "/var/lib/docker",
			Images: 3, Containers: 2, ContainersRunning: 1,
		})
	})
	queried := make(chan struct{}, 1)
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
		queried <- struct{}{}
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	select {
	case <-queried:
	case <-time.After(10 * time.Second):
		t.Fatal("/system/df was never queried")
	}

	// The failed query leaves nothing to wait for, so a short deadline is
	// enough to confirm the wait terminates rather than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.WaitForDiskUsage(ctx)

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
	var dfCalls int
	done := make(chan struct{}, 4)

	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dockerInfo{ServerVersion: "29.1.3"})
	})
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, _ *http.Request) {
		dfCalls++
		_ = json.NewEncoder(w).Encode(diskUsageFixture())
		done <- struct{}{}
	})
	serveFakeDaemon(t, mux)

	c := NewRuntimeCollector(quietLogger())

	for range 4 {
		if err := c.Collect(); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.WaitForDiskUsage(ctx)

	if dfCalls != 1 {
		t.Errorf("/system/df was queried %d times across 4 cycles, want 1", dfCalls)
	}
}

// TestGetJSON_CancelledContext verifies a cancelled request surfaces as an
// error rather than blocking or returning a partially decoded value.
func TestGetJSON_CancelledContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dockerInfo{ServerVersion: "29.1.3"})
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
