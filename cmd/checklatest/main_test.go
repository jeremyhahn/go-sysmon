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
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

// TestNewFiler_NamesWhatIsMissing verifies each misconfiguration is its own
// error type, so the missing flags and the missing token are told apart.
func TestNewFiler_NamesWhatIsMissing(t *testing.T) {
	base := options{api: "http://gitea.invalid/api/v1", repo: "jeremyhahn/go-sysmon", token: "t"}

	noAPI := base
	noAPI.api = ""
	_, err := newFiler(noAPI, http.DefaultClient)
	var flagsErr *filerFlagsError
	if !errors.As(err, &flagsErr) {
		t.Errorf("newFiler(no -gitea-api) error = %v, want a *filerFlagsError", err)
	}

	noToken := base
	noToken.token = ""
	_, err = newFiler(noToken, http.DefaultClient)
	var tokenErr *noTokenError
	if !errors.As(err, &tokenErr) {
		t.Errorf("newFiler(no token) error = %v, want a *noTokenError", err)
	}

	filer, err := newFiler(base, http.DefaultClient)
	if err != nil || filer == nil {
		t.Errorf("newFiler(complete) = %v, %v, want a filer and no error", filer, err)
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

// TestRun_WorkflowRunnerLabelIsChecked verifies a runner label a workflow
// names is judged as well as the pinned release's two labels: a job asking for
// a runner GitHub no longer offers waits forever, so the run has no verdict.
func TestRun_WorkflowRunnerLabelIsChecked(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	o := newOptions(t, &stdout, &stderr)
	workflow := "jobs:\n  a:\n    runs-on: ubuntu-20.04\n    steps:\n      - uses: actions/checkout@v7\n"
	if err := os.WriteFile(filepath.Join(o.root, ".github", "workflows", "ci.yml"), []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := run(context.Background(), o, fakeSources("v2.14.0", nil, &calls), http.DefaultClient); code != exitUnchecked {
		t.Fatalf("run() = %d, want %d; stdout: %s", code, exitUnchecked, stdout.String())
	}
	if !strings.Contains(stderr.String(), `runner label "ubuntu-20.04"`) {
		t.Errorf("stderr = %q, want the unlisted workflow label named", stderr.String())
	}
}

func TestParseOptions_ReadsEveryFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := map[string]string{"MY_TOKEN": "secret"}
	args := []string{
		"-root", "/repo", "-node-file", ".nvmrc", "-debian", "trixie", "-ubuntu", "26.04",
		"-hosted-runner", "26.04", "-tool", "golangci-lint=" + lintModule + "@v2.14.0",
		"-tool", "gosec=github.com/securego/gosec/v2@v2.29.0", "-timeout", "90s",
		"-file-issue", "-dry-run", "-gitea-api", "https://gitea.example/api/v1",
		"-repo", "o/r", "-token-env", "MY_TOKEN", "-run-url", "https://ci.example/run/1",
		"-go-releases-url", "https://mirror.example/go/VERSION",
		"-node-index-url", "https://mirror.example/node/index.json",
		"-debian-dists-url", "http://mirror.example/debian/dists",
		"-ubuntu-series-url", "https://mirror.example/ubuntu/series",
		"-runner-images-url", "https://mirror.example/runner-images/README.md",
		"-actions-base-url", "file:///srv/mirror/actions",
	}

	o, timeout, err := parseOptions(args, func(k string) string { return env[k] }, &stdout, &stderr)
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", timeout)
	}
	want := options{
		root: "/repo", nodeFile: ".nvmrc", debian: "trixie", ubuntu: "26.04", hostedRunner: "26.04",
		tools: []pins.Tool{
			{Name: "golangci-lint", Module: lintModule, Version: "v2.14.0"},
			{Name: "gosec", Module: "github.com/securego/gosec/v2", Version: "v2.29.0"},
		},
		fileIssue: true, dryRun: true, api: "https://gitea.example/api/v1", repo: "o/r",
		token: "secret", runURL: "https://ci.example/run/1",
		urls: sourceURLs{
			goReleases:   "https://mirror.example/go/VERSION",
			nodeIndex:    "https://mirror.example/node/index.json",
			debianDists:  "http://mirror.example/debian/dists",
			ubuntuSeries: "https://mirror.example/ubuntu/series",
			runnerImages: "https://mirror.example/runner-images/README.md",
			actionsBase:  "file:///srv/mirror/actions",
		},
		stdout: &stdout, stderr: &stderr,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("options = %+v, want %+v", o, want)
	}
}

func TestParseOptions_RejectsAMalformedCommandLine(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":   {"-no-such-flag"},
		"malformed tool": {"-tool", "golangci-lint"},
		"bad timeout":    {"-timeout", "soon"},
		// A source URL must be absolute, with a host, on a scheme the
		// source can be fetched over.
		"relative source URL":         {"-go-releases-url", "go.dev/VERSION"},
		"source URL with no host":     {"-node-index-url", "https:///index.json"},
		"unparsable source URL":       {"-debian-dists-url", "http://[::1"},
		"file URL for an HTTP source": {"-ubuntu-series-url", "file:///srv/series"},
		"ftp runner images":           {"-runner-images-url", "ftp://mirror.example/README.md"},
		"ssh actions base":            {"-actions-base-url", "ssh://git@github.com"},
		"file actions base, no path":  {"-actions-base-url", "file://"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if _, _, err := parseOptions(args, func(string) string { return "" }, &stdout, &stderr); err == nil {
				t.Fatal("parseOptions() error = nil, want the malformed command line rejected")
			}
			if !strings.Contains(stderr.String(), "Usage of checklatest") {
				t.Errorf("stderr = %q, want the usage printed", stderr.String())
			}
		})
	}
}

// TestParseOptions_SourcesDefaultToThePublicURLs verifies a command line that
// names no source fetches from the public ones.
func TestParseOptions_SourcesDefaultToThePublicURLs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o, _, err := parseOptions(nil, func(string) string { return "" }, &stdout, &stderr)
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	want := sourceURLs{
		goReleases: pins.GoVersionURL, nodeIndex: pins.NodeIndexURL, debianDists: pins.DebianDistsURL,
		ubuntuSeries: pins.UbuntuSeriesURL, runnerImages: pins.RunnerImagesURL, actionsBase: pins.ActionsBaseURL,
	}
	if o.urls != want {
		t.Errorf("urls = %+v, want the public sources %+v", o.urls, want)
	}
}

// TestNewSources_FetchesFromTheNamedURL verifies a source flag is where the
// source is actually fetched from, not merely parsed.
func TestNewSources_FetchesFromTheNamedURL(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go/VERSION" {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write([]byte("go1.27.9\ntime 2026-09-01T00:00:00Z\n")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(mirror.Close)

	src := newSources(mirror.Client(), sourceURLs{goReleases: mirror.URL + "/go/VERSION", nodeIndex: mirror.URL + "/absent"})
	got, err := src.goLatest(context.Background())
	if err != nil || got != "go1.27.9" {
		t.Errorf("goLatest() = %q, %v; want go1.27.9 from the mirror", got, err)
	}
	var fetchErr *pins.FetchResponseError
	if _, err := src.nodeLTS(context.Background()); !errors.As(err, &fetchErr) || fetchErr.Status != http.StatusNotFound {
		t.Errorf("nodeLTS() error = %v, want the mirror's 404", err)
	}
}

// TestNewSources_WiresEverySource verifies every kind of pin has a live
// source: a missing one would be a nil function the run calls.
func TestNewSources_WiresEverySource(t *testing.T) {
	src := newSources(http.DefaultClient, sourceURLs{})
	wired := map[string]bool{
		"go": src.goLatest != nil, "module": src.module != nil, "node": src.nodeLTS != nil,
		"debian": src.debian != nil, "ubuntu": src.ubuntuLTS != nil, "tags": src.tags != nil,
		"runners": src.runners != nil,
	}
	for name, ok := range wired {
		if !ok {
			t.Errorf("source %s is not wired", name)
		}
	}
}
