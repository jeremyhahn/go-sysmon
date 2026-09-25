//go:build integration

// Package collector runs sysmon against real workloads inside the
// devcontainer: a container runtime's API, processes that are hypervisors,
// container and virtual-machine cgroups, bridges, bonds and VLANs, and memory
// limits. Everything it changes is confined to the container, and most of it
// to a mount or network namespace of the one process under test.
package collector

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }

// awaitSnapshot fetches the server's snapshot until ready accepts one,
// failing the test if none has within await's bound.
func awaitSnapshot(t *testing.T, s *harness.Server, what string, ready func(raw []byte) bool) {
	t.Helper()
	await.Until(t, what, func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL("/api/snapshot"), nil)
		if err != nil {
			t.Fatalf("build the snapshot request: %v", err)
		}
		status, _, body := harness.Do(t, http.DefaultClient, req)
		return status == http.StatusOK && ready(body)
	})
}
