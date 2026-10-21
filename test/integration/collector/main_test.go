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
	"strings"
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

// awaitListing runs "sysmon <args>" until the JSON it prints decodes to a P
// for which listed is true, and returns that P. A guest or container started
// a moment ago is not always in the first listing, so one read cannot tell a
// missing item from a late one; polling waits only for the item and leaves
// every claim about it to the caller.
func awaitListing[P any](t *testing.T, bin, what string, listed func(P) bool, args ...string) P {
	t.Helper()
	var p P
	await.Until(t, what, func(context.Context) bool {
		r := harness.Run(t, bin, nil, args...)
		if r.Code != 0 {
			t.Fatalf("sysmon %s exited %d:\n%s", strings.Join(args, " "), r.Code, r.Output())
		}
		var got P
		harness.MustParseJSON(t, r.Stdout, &got)
		p = got
		return listed(got)
	})
	return p
}
