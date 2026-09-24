package server_test

import (
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prometheus build tag has to actually exclude prometheus.
//
// Owner ruling, 2026-09-04: the prometheus code compiles only under a build
// tag; sysmon-server gets it, the desktop and systray builds do not, so a
// realtime-only user carries no time-series dependency (#100).
//
// Nothing about splitting files enforces that. A single import added to any
// untagged file in any package on the path puts pkg/prom back into the desktop
// binary, and everything still compiles, passes and ships -- measured while
// writing this: an import added to an untagged file built cleanly and no test
// noticed. So the guarantee is checked here, in both directions, because
// "excluded" and "included" are each half of what the tag claims.

const promPkg = "github.com/jeremyhahn/go-sysmon/pkg/prom"

// TestPrometheusTagExcludesProm fails if the untagged build can reach pkg/prom.
func TestPrometheusTagExcludesProm(t *testing.T) {
	graph := tagAwareImports(t, nil)
	assertGraphLoaded(t, graph, "untagged")

	for _, from := range []string{"cmd/sysmon", "pkg/server", "pkg/metrics"} {
		if path := reachesPkg(graph, from, promPkg); path != nil {
			t.Errorf("without the prometheus tag, %s still reaches pkg/prom: %s\n\n"+
				"The desktop and systray builds are meant to carry no time-series\n"+
				"dependency at all. One import in one untagged file puts the writer,\n"+
				"the protobuf encoder and snappy back into that binary, and nothing\n"+
				"else in the build says so.",
				from, strings.Join(shorten(path), " -> "))
		}
	}
}

// TestPrometheusTagIncludesProm is the other half. A tag that excluded
// everything in both builds would pass the test above and ship a server with no
// history at all.
//
// It requires cmd/sysmon to import pkg/prom DIRECTLY, which is the wiring --
// the exporter constructed, the querier handed to the server. Reachability
// alone is too weak to be worth asserting: cmd/sysmon reaches pkg/prom through
// pkg/server in the tagged build no matter what the wiring does, so deleting
// serve_prom.go entirely left a reachability check green. Measured, by doing
// exactly that.
func TestPrometheusTagIncludesProm(t *testing.T) {
	graph := tagAwareImports(t, []string{"prometheus"})
	assertGraphLoaded(t, graph, "prometheus")

	var wired bool
	for _, imported := range graph["cmd/sysmon"] {
		if imported == promPkg {
			wired = true
			break
		}
	}
	if !wired {
		t.Error("with the prometheus tag set, cmd/sysmon does not import pkg/prom directly; " +
			"the exporter and the querier are not being constructed, so the server " +
			"would build with history support compiled in and nothing wired to it")
	}

	if path := reachesPkg(graph, "pkg/server", promPkg); path == nil {
		t.Error("with the prometheus tag set, pkg/server does not reach pkg/prom; " +
			"the history endpoints cannot be the real ones")
	}
}

func assertGraphLoaded(t *testing.T, graph map[string][]string, which string) {
	t.Helper()
	if len(graph) < 10 {
		t.Fatalf("%s graph holds %d packages; it did not load, so the assertions prove nothing", which, len(graph))
	}
	if len(graph["cmd/sysmon"]) == 0 {
		t.Fatalf("%s graph has no imports for cmd/sysmon; build constraints are not being read", which)
	}
}

// tagAwareImports parses the first-party import graph as the compiler would see
// it with the given tags, so //go:build lines are honoured rather than ignored.
func tagAwareImports(t *testing.T, tags []string) map[string][]string {
	t.Helper()

	root := repoRoot(t)
	ctx := build.Default
	ctx.BuildTags = tags
	ctx.UseAllFiles = false

	graph := make(map[string][]string)
	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			if entry.Name() == "testdata" || entry.Name() == "node_modules" || entry.Name() == "frontend" {
				return filepath.SkipDir
			}
			pkg, importErr := ctx.ImportDir(path, 0)
			// A directory with no buildable Go files under these tags is a fact
			// about the tags, not an error. Anything else -- an unparsable file,
			// two package clauses -- is a graph this test could not load, and
			// skipping it would let a prom import hide behind the failure.
			var noGo *build.NoGoError
			if errors.As(importErr, &noGo) {
				return nil
			}
			if importErr != nil {
				return importErr
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			graph[filepath.ToSlash(rel)] = pkg.Imports
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", top, err)
		}
	}
	return graph
}

// reachesPkg walks the graph from a module-relative package to a full import
// path, returning the shortest chain or nil.
func reachesPkg(graph map[string][]string, start, target string) []string {
	type step struct {
		pkg  string
		path []string
	}
	const prefix = "github.com/jeremyhahn/go-sysmon/"

	visited := map[string]bool{start: true}
	queue := []step{{pkg: start, path: []string{start}}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, imported := range graph[current.pkg] {
			if imported == target {
				return append(append([]string(nil), current.path...), imported)
			}
			rel, ok := strings.CutPrefix(imported, prefix)
			if !ok || visited[rel] {
				continue
			}
			visited[rel] = true
			queue = append(queue, step{pkg: rel, path: append(append([]string(nil), current.path...), rel)})
		}
	}
	return nil
}

func shorten(path []string) []string {
	out := make([]string, 0, len(path))
	for _, p := range path {
		out = append(out, strings.TrimPrefix(p, "github.com/jeremyhahn/go-sysmon/"))
	}
	return out
}

// repoRoot walks up to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
