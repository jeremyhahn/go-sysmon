package server_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The owner's ruling of 2026-09-04: "the poller does its job, updates a state,
// and every API request and stream serves from that state until the next poll.
// No endpoint may query fresh host state per request."
//
// The audit for #99 found no violation: every handler reads Monitor.Snapshot(),
// which is an atomic pointer load of the last completed poll, and pkg/server
// does not import a collector at all. This test is what keeps that true.
//
// It is here rather than in a document because the same rule was already
// written down for the frontend bundle -- "a change to the frontend is not
// complete until dist/ is rebuilt and committed with it" -- and main still
// spent a day carrying a bundle that did not match its sources (#55). Prose
// does not enforce. A failing check does.

const modulePrefix = "github.com/jeremyhahn/go-sysmon/"

// TestServerNeverReachesACollector fails if pkg/server can reach pkg/collector
// through any chain of first-party imports.
//
// Direct imports are not enough to check. The rule is about what the request
// path can DO, and a package two hops away can read the host just as easily as
// one imported here -- the transitive graph is the honest question.
func TestServerNeverReachesACollector(t *testing.T) {
	graph := firstPartyImports(t)

	const (
		from = "pkg/server"
		to   = "pkg/collector"
	)
	if path := reaches(graph, from, to); path != nil {
		t.Errorf("%s can reach %s: %s\n\n"+
			"Every API request and stream must serve the state the poller last\n"+
			"published (Monitor.Snapshot, an atomic pointer load). A collector on\n"+
			"the request path means an endpoint can read the host per request,\n"+
			"which is the thing this rule forbids: two clients would measure the\n"+
			"machine twice for one poll, and a slow read would block a response\n"+
			"that has a perfectly good reading available.",
			from, to, strings.Join(path, " -> "))
	}
}

// TestGraphIsNotEmpty guards the guard.
//
// Every assertion above is of the form "X cannot reach Y". A graph that failed
// to load -- a moved directory, a parse error swallowed, a wrong root -- makes
// every such assertion vacuously true, and the check would report a pass while
// reading nothing. That is the failure this repository keeps finding in other
// gates, and it is cheap to refuse here.
func TestGraphIsNotEmpty(t *testing.T) {
	graph := firstPartyImports(t)

	if len(graph) < 10 {
		t.Fatalf("loaded %d packages; the import graph did not load, so the guard above proves nothing", len(graph))
	}
	if _, ok := graph["pkg/server"]; !ok {
		t.Fatal("pkg/server is not in the graph; the guard above is reading the wrong tree")
	}
	if _, ok := graph["pkg/collector"]; !ok {
		t.Fatal("pkg/collector is not in the graph; a rename would make the guard above vacuous")
	}
	// pkg/server must reach something, or the walk is not following edges.
	if len(graph["pkg/server"]) == 0 {
		t.Fatal("pkg/server imports nothing; the parser is not reading import blocks")
	}
}

// firstPartyImports parses every package under pkg/ and cmd/ and returns the
// import graph, keyed by module-relative path.
//
// Test files are excluded deliberately: a test may import whatever it needs to
// build a fixture, and forbidding that would be a rule about tests rather than
// about the request path.
func firstPartyImports(t *testing.T) map[string][]string {
	t.Helper()

	root := moduleRoot(t)
	graph := make(map[string][]string)

	for _, top := range []string{"pkg", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() || entry.Name() == "testdata" || entry.Name() == "node_modules" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			imports, readErr := packageImports(path)
			if readErr != nil {
				return readErr
			}
			if imports != nil {
				graph[filepath.ToSlash(rel)] = imports
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return graph
}

// packageImports returns the first-party imports of the Go files in dir, or nil
// when dir holds no Go source.
func packageImports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	found := false
	fset := token.NewFileSet()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found = true
		// ImportsOnly: the guard is about the import graph, and parsing whole
		// bodies for every package would make this the slowest test in the
		// suite for nothing.
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if after, ok := strings.CutPrefix(path, modulePrefix); ok {
				seen[after] = true
			}
		}
	}
	if !found {
		return nil, nil
	}

	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// reaches returns the import chain from start to target, or nil when there is
// none. Breadth-first, so the path reported is the shortest one -- the most
// useful thing to read when the guard fails.
func reaches(graph map[string][]string, start, target string) []string {
	type step struct {
		pkg  string
		path []string
	}

	visited := map[string]bool{start: true}
	queue := []step{{pkg: start, path: []string{start}}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range graph[current.pkg] {
			if next == target {
				return append(append([]string(nil), current.path...), next)
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			queue = append(queue, step{pkg: next, path: append(append([]string(nil), current.path...), next)})
		}
	}
	return nil
}

// moduleRoot walks up from the test's directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
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
			t.Fatal("no go.mod above the test directory; cannot locate the module root")
		}
		dir = parent
	}
}
