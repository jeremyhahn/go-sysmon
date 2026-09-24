package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/pins"
)

const lintModule = "github.com/golangci/golangci-lint/v2"

// writeRepo creates a repository with every pin current and one workflow
// using actions/checkout@v7.
func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.27.1\n",
		".node-version":            "24.21.0\n",
		".github/workflows/ci.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v7\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeSources answers every pin as current, except golangci-lint, whose
// latest is lintLatest. A non-nil goErr stands in for an unreachable go.dev;
// calls counts the Go lookups.
func fakeSources(lintLatest string, goErr error, calls *int) sources {
	return sources{
		goLatest: func(context.Context) (string, error) {
			*calls++
			return "go1.27.1", goErr
		},
		module: func(_ context.Context, module string) (string, error) {
			if module != lintModule {
				return "", errors.New("fake: unknown module " + module)
			}
			return lintLatest, nil
		},
		nodeLTS: func(context.Context) (string, error) { return "24.21.0", nil },
		debian: func(_ context.Context, suite string) (pins.DebianRelease, error) {
			return pins.DebianRelease{Codename: "trixie", Version: "13.7"}, nil
		},
		ubuntuLTS: func(context.Context) (string, error) { return "26.04", nil },
		tags: func(context.Context, string) ([]pins.Tag, error) {
			return []pins.Tag{{Name: "v7.0.1", Commit: "3d3c42e5aac5ba805825da76410c181273ba90b1"}}, nil
		},
		runners: func(context.Context) ([]pins.RunnerLabel, error) {
			return []pins.RunnerLabel{{Name: "ubuntu-26.04"}, {Name: "ubuntu-26.04-arm"}}, nil
		},
	}
}

func newOptions(t *testing.T, stdout, stderr *bytes.Buffer) options {
	t.Helper()
	return options{
		root:     writeRepo(t),
		nodeFile: ".node-version",
		debian:   "trixie",
		ubuntu:   "26.04",

		hostedRunner: "26.04",
		tools:        []pins.Tool{{Name: "golangci-lint", Module: lintModule, Version: "v2.14.0"}},
		stdout:       stdout,
		stderr:       stderr,
	}
}

// fakeGitea serves an empty issue list, one task:ready label, and records the
// issues filed.
type fakeGitea struct {
	mu     sync.Mutex
	posted []string
}

func (f *fakeGitea) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Errorf("encode: %v", err)
			}
		}
		base := "/api/v1/repos/jeremyhahn/go-sysmon"
		labels := []map[string]any{{"id": 7, "name": "task:ready"}}
		switch r.Method + " " + strings.TrimPrefix(r.URL.Path, base) {
		case "GET /issues":
			reply([]any{})
		case "GET /labels":
			reply(labels)
		case "POST /issues":
			f.posted = append(f.posted, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 300, "labels": labels})
		case "GET /issues/300":
			reply(map[string]any{"number": 300, "labels": labels})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRun_CurrentPinsExitZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0

	code := run(context.Background(), newOptions(t, &stdout, &stderr), fakeSources("v2.14.0", nil, &calls), http.DefaultClient)

	if code != exitCurrent {
		t.Fatalf("run() = %d, want %d; stderr: %s", code, exitCurrent, stderr.String())
	}
	want := "go 1.27.1: current\ngolangci-lint v2.14.0: current\nnode 24.21.0: current\n" +
		"debian trixie: current\nubuntu 26.04: current\nactions/checkout v7: current\n" +
		"runner ubuntu-26.04: current\nrunner ubuntu-26.04-arm: current\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_StalePinExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0

	code := run(context.Background(), newOptions(t, &stdout, &stderr), fakeSources("v2.15.0", nil, &calls), http.DefaultClient)

	if code != exitLagging {
		t.Fatalf("run() = %d, want %d", code, exitLagging)
	}
	if !strings.Contains(stderr.String(), "golangci-lint v2.14.0: behind (latest stable is v2.15.0)") {
		t.Errorf("stderr = %q, want the lagging tool named", stderr.String())
	}
}

func TestRun_OfflineExitsTwoNotZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0

	code := run(context.Background(), newOptions(t, &stdout, &stderr), fakeSources("v2.14.0", syscall.ENETUNREACH, &calls), http.DefaultClient)

	if code != exitUnchecked {
		t.Fatalf("run() = %d, want %d", code, exitUnchecked)
	}
	if strings.Contains(stdout.String(), "go 1.27.1: current") {
		t.Errorf("stdout = %q reports an unchecked item as current", stdout.String())
	}
	if !strings.Contains(stderr.String(), "go: no verdict") {
		t.Errorf("stderr = %q, want the unchecked item named", stderr.String())
	}
}

func TestRun_MissingModFileExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	o := newOptions(t, &stdout, &stderr)
	o.root = t.TempDir()

	if code := run(context.Background(), o, fakeSources("v2.14.0", nil, &calls), http.DefaultClient); code != exitUnchecked {
		t.Fatalf("run() = %d, want %d", code, exitUnchecked)
	}
}

func TestRun_FilerMisconfigurationFailsBeforeChecking(t *testing.T) {
	tests := map[string]func(*options){
		"no token": func(o *options) { o.api, o.repo = "http://gitea.invalid/api/v1", "jeremyhahn/go-sysmon" },
		"no api":   func(o *options) { o.repo, o.token = "jeremyhahn/go-sysmon", "t" },
		"repo not owner/name": func(o *options) {
			o.api, o.repo, o.token = "http://gitea.invalid/api/v1", "go-sysmon", "t"
		},
		"repo with extra path": func(o *options) {
			o.api, o.repo, o.token = "http://gitea.invalid/api/v1", "a/b/c", "t"
		},
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			o := newOptions(t, &stdout, &stderr)
			o.fileIssue = true
			configure(&o)

			code := run(context.Background(), o, fakeSources("v2.14.0", nil, &calls), http.DefaultClient)
			if code != exitUnchecked || calls != 0 {
				t.Errorf("run() = %d after %d checks, want %d before any", code, calls, exitUnchecked)
			}
			if !strings.Contains(stderr.String(), "-file-issue needs") {
				t.Errorf("stderr = %q, want the missing setting named", stderr.String())
			}
		})
	}
}

func fileOptions(t *testing.T, srv *httptest.Server, stdout, stderr *bytes.Buffer) options {
	o := newOptions(t, stdout, stderr)
	o.fileIssue = true
	o.api = srv.URL + "/api/v1"
	o.repo = "jeremyhahn/go-sysmon"
	o.token = "t"
	return o
}

func TestRun_FileIssueFilesALagAndExitsZero(t *testing.T) {
	g := &fakeGitea{}
	srv := g.serve(t)
	var stdout, stderr bytes.Buffer
	calls := 0

	code := run(context.Background(), fileOptions(t, srv, &stdout, &stderr), fakeSources("v2.15.0", nil, &calls), srv.Client())

	if code != exitCurrent {
		t.Fatalf("run() = %d, want %d; stderr: %s", code, exitCurrent, stderr.String())
	}
	if len(g.posted) != 1 || !strings.Contains(stdout.String(), "filed issue #300") {
		t.Errorf("posted %q, stdout %q; want one issue filed", g.posted, stdout.String())
	}
}

func TestRun_DryRunFilesNothing(t *testing.T) {
	g := &fakeGitea{}
	srv := g.serve(t)
	var stdout, stderr bytes.Buffer
	calls := 0
	o := fileOptions(t, srv, &stdout, &stderr)
	o.dryRun = true

	code := run(context.Background(), o, fakeSources("v2.15.0", nil, &calls), srv.Client())

	if code != exitCurrent || len(g.posted) != 0 {
		t.Fatalf("run() = %d and posted %q, want %d and nothing posted", code, g.posted, exitCurrent)
	}
	if !strings.Contains(stdout.String(), "would file this issue") {
		t.Errorf("stdout = %q, want the issue printed", stdout.String())
	}
}

func TestRun_FileIssueStillFailsForAnUncheckedItem(t *testing.T) {
	g := &fakeGitea{}
	srv := g.serve(t)
	var stdout, stderr bytes.Buffer
	calls := 0
	o := fileOptions(t, srv, &stdout, &stderr)
	o.tools = append(o.tools, pins.Tool{Name: "gosec", Module: "github.com/securego/gosec/v2", Version: "v2.29.0"})

	code := run(context.Background(), o, fakeSources("v2.15.0", nil, &calls), srv.Client())

	if code != exitUnchecked {
		t.Fatalf("run() = %d, want %d", code, exitUnchecked)
	}
	if len(g.posted) != 1 {
		t.Errorf("posted %q, want the known lag filed anyway", g.posted)
	}
}

func TestRun_TrackerFailureExitsTwo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	var stdout, stderr bytes.Buffer
	calls := 0

	code := run(context.Background(), fileOptions(t, srv, &stdout, &stderr), fakeSources("v2.15.0", nil, &calls), srv.Client())

	if code != exitUnchecked || !strings.Contains(stderr.String(), "HTTP 503") {
		t.Fatalf("run() = %d, stderr %q; want %d and the API error", code, stderr.String(), exitUnchecked)
	}
}

func TestToolFlags(t *testing.T) {
	var f toolFlags
	if err := f.Set("gosec=github.com/securego/gosec/v2@v2.29.0"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, want := f.String(), "gosec=github.com/securego/gosec/v2@v2.29.0"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	var serr *pins.ToolSpecError
	if err := f.Set("gosec"); !errors.As(err, &serr) {
		t.Errorf("Set(gosec) error = %v, want *pins.ToolSpecError", err)
	}
	if len(f) != 1 {
		t.Errorf("a rejected flag was kept: %v", f)
	}
}

func TestRun_UnreadableWorkflowExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	o := newOptions(t, &stdout, &stderr)
	if err := os.WriteFile(filepath.Join(o.root, ".github", "workflows", "bad.yml"), []byte("jobs: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := run(context.Background(), o, fakeSources("v2.14.0", nil, &calls), http.DefaultClient); code != exitUnchecked {
		t.Fatalf("run() = %d, want %d", code, exitUnchecked)
	}
}
