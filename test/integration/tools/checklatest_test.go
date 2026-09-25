//go:build integration

package tools

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// Exit statuses of checklatest.
const (
	exitCurrent   = 0
	exitLagging   = 1
	exitUnchecked = 2
)

// wantExit fails the test unless r ended with code.
func wantExit(t *testing.T, r harness.Result, code int) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.Code, code, r.Stdout, r.Stderr)
	}
}

// wantContains fails the test unless text contains want.
func wantContains(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s does not contain %q:\n%s", what, want, text)
	}
}

// TestChecklatest_EveryPinCurrent runs the command against a repository whose
// every pin matches what the stand-ins report: every item is judged, each is
// current, and the run exits 0. The action pinned by commit is placed by the
// newest release tag on that commit, read through the peeled annotated tag.
func TestChecklatest_EveryPinCurrent(t *testing.T) {
	w := newWorld(t)

	r := w.checklatest(t, currentRepo(t, w.setupGo), nil)

	wantExit(t, r, exitCurrent)
	want := "go 1.27.1: current\n" +
		"lint v2.14.0: current\n" +
		"node 24.21.0: current\n" +
		"debian trixie: current\n" +
		"ubuntu 26.04: current\n" +
		"actions/checkout v7: current\n" +
		"actions/setup-go " + w.setupGo + ": current\n" +
		"runner ubuntu-26.04: current\n" +
		"runner ubuntu-26.04-arm: current\n"
	if r.Stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.Stdout, want)
	}
}

// TestChecklatest_ALaggingPinFailsWithItsVerdict moves each source on by one
// release, or pins ahead of or before one, and checks the run exits 1 naming
// that item's verdict and the release it was judged against.
func TestChecklatest_ALaggingPinFailsWithItsVerdict(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, w *world)
		repo  func(t *testing.T, w *world) string
		extra []string
		want  string
	}{
		{
			name:  "a go directive on a release candidate",
			setup: func(*testing.T, *world) {},
			repo:  func(t *testing.T, w *world) string { return repoWith(t, "1.28rc1", w.setupGo, "") },
			want:  "go 1.28rc1: prerelease (latest stable is go1.27.1)",
		},
		{
			name:  "a tool pinned to a release candidate",
			setup: func(*testing.T, *world) {},
			extra: []string{"-tool", "lint=" + lintModule + "@v2.15.0-rc.1"},
			want:  "lint v2.15.0-rc.1: prerelease (latest stable is v2.14.0)",
		},
		{
			name:  "an action pinned to a release candidate",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return repoWith(t, "1.27.1", w.setupGo, "actions/checkout@v8.0.0-rc.1")
			},
			want: "actions/checkout v8.0.0-rc.1: prerelease (latest stable is v7.0.2)",
		},
		{
			name:  "an action pinned by the commit of a release candidate",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return repoWith(t, "1.27.1", w.setupGo, "actions/checkout@"+w.checkout["v8.0.0-rc.1"])
			},
			want: "prerelease (latest stable is v7.0.2, pinned commit is v8.0.0-rc.1)",
		},
		{
			name:  "an action pinned to an older exact release",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return repoWith(t, "1.27.1", w.setupGo, "actions/checkout@v7.0.0")
			},
			want: "actions/checkout v7.0.0: behind (latest stable is v7.0.2)",
		},
		{
			name: "a hosted runner on a beta image",
			setup: func(_ *testing.T, w *world) {
				w.up.runners("ubuntu-26.04", "ubuntu-26.04-arm!beta", "ubuntu-24.04-arm")
			},
			want: "runner ubuntu-26.04-arm: prerelease (latest stable is ubuntu-24.04-arm)",
		},
		{
			name:  "ubuntu pinned to an interim release after the LTS",
			setup: func(*testing.T, *world) {},
			extra: []string{"-ubuntu", "26.10"},
			want:  "ubuntu 26.10: ahead (latest stable is 26.04)",
		},
		{
			name:  "go",
			setup: func(_ *testing.T, w *world) { w.up.set("/go/VERSION", reply{body: "go1.27.2\n"}) },
			want:  "go 1.27.1: behind (latest stable is go1.27.2)",
		},
		{
			name:  "a tool",
			setup: func(_ *testing.T, w *world) { w.up.module(lintModule, "v2.14.0", "v2.15.0", "v3.0.0-rc.1") },
			want:  "lint v2.14.0: behind (latest stable is v2.15.0)",
		},
		{
			name:  "a tool pinned past its latest release",
			setup: func(*testing.T, *world) {},
			extra: []string{"-tool", "lint=" + lintModule + "@v2.16.0"},
			want:  "lint v2.16.0: ahead (latest stable is v2.14.0)",
		},
		{
			name: "node",
			setup: func(_ *testing.T, w *world) {
				w.up.set("/node/index.json", reply{body: `[{"version":"v26.3.0","lts":false},{"version":"v24.22.0","lts":"Krypton"}]`})
			},
			want: "node 24.21.0: behind (latest stable is 24.22.0)",
		},
		{
			name: "debian",
			setup: func(_ *testing.T, w *world) {
				w.up.debian("stable", "forky", "14.1")
			},
			want: "debian trixie: behind (latest stable is forky (14.1))",
		},
		{
			name: "debian testing",
			setup: func(_ *testing.T, w *world) {
				w.up.debian("forky", "forky", "")
			},
			extra: []string{"-debian", "forky"},
			want:  "debian forky: prerelease (latest stable is trixie (13.7))",
		},
		{
			name: "ubuntu",
			setup: func(_ *testing.T, w *world) {
				w.up.set("/ubuntu/series/2", reply{body: `{"entries":[{"version":"28.04","status":"Current Stable Release"}]}`})
			},
			want: "ubuntu 26.04: behind (latest stable is 28.04)",
		},
		{
			name: "a hosted runner",
			setup: func(_ *testing.T, w *world) {
				w.up.runners("ubuntu-28.04", "ubuntu-26.04", "ubuntu-26.04-arm", "ubuntu-28.04-arm")
			},
			want: "runner ubuntu-26.04: behind (latest stable is ubuntu-28.04)",
		},
		{
			name: "an action on an older major line",
			setup: func(t *testing.T, w *world) {
				w.mirror.repo(t, "actions/checkout", "v7.0.1", "v7=v7.0.1", "v8.0.0")
			},
			want: "actions/checkout v7: behind (latest stable is v8.0.0)",
		},
		{
			name: "an action pinned by the commit of an older release",
			setup: func(t *testing.T, w *world) {
				w.setupGo = w.mirror.repo(t, "actions/setup-go", "v6.0.0", "v6.1.0")["v6.0.0"]
			},
			want: "(latest stable is v6.1.0, pinned commit is v6.0.0)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(t, w)
			root := currentRepo(t, w.setupGo)
			if tt.repo != nil {
				root = tt.repo(t, w)
			}

			r := w.checklatest(t, root, nil, tt.extra...)

			wantExit(t, r, exitLagging)
			wantContains(t, "stderr", r.Stderr, "not at their latest stable release")
			wantContains(t, "stderr", r.Stderr, tt.want)
		})
	}
}

// TestChecklatest_ASourceWithNoUsableAnswerIsUnchecked breaks each source in
// each way it can fail -- down, answering an error, or answering something
// that is not what it documents -- and checks the run exits 2 naming the item
// and why. An item with no verdict is never reported as current.
func TestChecklatest_ASourceWithNoUsableAnswerIsUnchecked(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, w *world)
		repo  func(t *testing.T, w *world) string
		extra []string
		want  []string
	}{
		{
			name:  "go.dev answers 503",
			setup: func(_ *testing.T, w *world) { w.up.set("/go/VERSION", reply{status: 503, body: "unavailable"}) },
			want:  []string{"pins: go: no verdict", "/go/VERSION: HTTP 503"},
		},
		{
			name:  "go.dev answers a development build",
			setup: func(_ *testing.T, w *world) { w.up.set("/go/VERSION", reply{body: "devel go1.28-abcdef\n"}) },
			want:  []string{"is not a stable Go release"},
		},
		{
			name:  "the Node.js index is not JSON",
			setup: func(_ *testing.T, w *world) { w.up.set("/node/index.json", reply{body: "<html>"}) },
			want:  []string{"pins: node: no verdict", "unreadable index"},
		},
		{
			name: "the Node.js index has no LTS release",
			setup: func(_ *testing.T, w *world) {
				w.up.set("/node/index.json", reply{body: `[{"version":"v26.2.0","lts":false}]`})
			},
			want: []string{"no LTS release in the index"},
		},
		{
			name:  "the Debian archive has no stable suite",
			setup: func(_ *testing.T, w *world) { w.up.set("/debian/dists/stable/Release", reply{status: 404}) },
			want:  []string{"pins: debian: no verdict", "HTTP 404"},
		},
		{
			name: "a Release file with no codename",
			setup: func(_ *testing.T, w *world) {
				w.up.set("/debian/dists/stable/Release", reply{body: "Origin: Debian\n"})
			},
			want: []string{"no Codename field"},
		},
		{
			name:  "stable with no release number",
			setup: func(_ *testing.T, w *world) { w.up.debian("stable", "trixie", "n/a") },
			extra: []string{"-debian", "bookworm"},
			want:  []string{`"n/a" is not a valid Debian release version`},
		},
		{
			name:  "a pinned suite with no release number",
			setup: func(_ *testing.T, w *world) { w.up.debian("bookworm", "bookworm", "old") },
			extra: []string{"-debian", "bookworm"},
			want:  []string{`"old" is not a valid Debian release version`},
		},
		{
			name:  "a source that cuts its answer short",
			setup: func(_ *testing.T, w *world) { w.up.set("/node/index.json", reply{body: `[{"version"`, short: true}) },
			want:  []string{"pins: node: no verdict", "unexpected EOF"},
		},
		{
			name:  "a module pin that is not a version",
			setup: func(*testing.T, *world) {},
			extra: []string{"-tool", "lint=" + lintModule + "@latest"},
			want:  []string{`"latest" is not a valid module version`},
		},
		{
			name:  "an ubuntu pin that is not a release number",
			setup: func(*testing.T, *world) {},
			extra: []string{"-ubuntu", "noble"},
			want:  []string{`"noble" is not a valid Ubuntu release version`},
		},
		{
			name:  "no GA runner of the pinned architecture",
			setup: func(_ *testing.T, w *world) { w.up.runners("ubuntu-26.04", "ubuntu-26.04-arm!beta") },
			want:  []string{"no GA ubuntu-NN.04-arm label"},
		},
		{
			name:  "an action pinned with build metadata",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return repoWith(t, "1.27.1", w.setupGo, "actions/checkout@v7.0.1+build.5")
			},
			want: []string{`"v7.0.1+build.5" is not a valid action release version`},
		},
		{
			name:  "the pinned suite is gone from the archive",
			setup: func(*testing.T, *world) {},
			extra: []string{"-debian", "buster"},
			want:  []string{"/debian/dists/buster/Release: HTTP 404"},
		},
		{
			name:  "Launchpad answers garbage",
			setup: func(_ *testing.T, w *world) { w.up.set("/ubuntu/series/2", reply{body: "{"}) },
			want:  []string{"pins: ubuntu: no verdict", "unreadable series list"},
		},
		{
			name: "Launchpad lists no released LTS",
			setup: func(_ *testing.T, w *world) {
				w.up.set("/ubuntu/series", reply{body: `{"entries":[{"version":"26.04","status":"Obsolete"}]}`})
			},
			want: []string{"no released LTS series"},
		},
		{
			name:  "the runner-images README has no table",
			setup: func(_ *testing.T, w *world) { w.up.set("/runner-images/README.md", reply{body: "# moved\n"}) },
			want:  []string{"pins: runner: no verdict", "no runner label table"},
		},
		{
			name:  "a runner label GitHub does not offer",
			setup: func(_ *testing.T, w *world) { w.up.runners("ubuntu-26.04", "ubuntu-24.04") },
			want:  []string{`runner label "ubuntu-26.04-arm" is not a GitHub-hosted runner`},
		},
		{
			name:  "a module the proxy does not have",
			setup: func(*testing.T, *world) {},
			extra: []string{"-tool", "gone=example.com/tools/gone@v1.0.0"},
			want:  []string{"pins: gone: no verdict", "go list -m -json example.com/tools/gone@latest"},
		},
		{
			name:  "an action repository the mirror does not have",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return writeTree(t, map[string]string{
					"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
					".github/workflows/ci.yml": "jobs:\n  a:\n    runs-on: ubuntu-26.04\n    steps:\n      - uses: actions/cache@v4\n",
				})
			},
			want: []string{"pins: actions/cache: no verdict", "git ls-remote --tags"},
		},
		{
			name:  "an action pinned to a branch",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return writeTree(t, map[string]string{
					"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
					".github/workflows/ci.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@main\n",
				})
			},
			want: []string{`"main" is not a valid action release version`},
		},
		{
			name:  "an action pinned to a commit no release is on",
			setup: func(*testing.T, *world) {},
			repo: func(t *testing.T, w *world) string {
				return writeTree(t, map[string]string{
					"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
					".github/workflows/ci.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@" + strings.Repeat("ab", 20) + "\n",
				})
			},
			want: []string{"is not a valid release commit version"},
		},
		{
			name: "an action with no stable release",
			setup: func(t *testing.T, w *world) {
				w.mirror.repo(t, "actions/checkout", "v8.0.0-rc.1", "v8=v8.0.0-rc.1")
			},
			want: []string{"no stable release tag"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(t, w)
			root := currentRepo(t, w.setupGo)
			if tt.repo != nil {
				root = tt.repo(t, w)
			}

			r := w.checklatest(t, root, nil, tt.extra...)

			wantExit(t, r, exitUnchecked)
			wantContains(t, "stderr", r.Stderr, "could not be checked against their latest stable release")
			for _, want := range tt.want {
				wantContains(t, "stderr", r.Stderr, want)
			}
		})
	}
}

// TestChecklatest_AStalledSourceIsCutOffByTheTimeout checks a source that
// accepts the connection and never answers is bounded by -timeout: the run
// ends with no verdict for that item instead of hanging the workflow.
func TestChecklatest_AStalledSourceIsCutOffByTheTimeout(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{hang: true})

	r := w.checklatest(t, currentRepo(t, w.setupGo), nil, "-timeout", "2s")

	wantExit(t, r, exitUnchecked)
	wantContains(t, "stderr", r.Stderr, "pins: go: no verdict")
	wantContains(t, "stderr", r.Stderr, "context deadline exceeded")
}

// TestChecklatest_ARepositoryItCannotReadIsUnchecked covers the pins read
// before anything is fetched.
func TestChecklatest_ARepositoryItCannotReadIsUnchecked(t *testing.T) {
	tests := map[string]struct {
		files map[string]string
		want  string
	}{
		"no go.mod": {
			files: map[string]string{".node-version": "24.21.0\n"},
			want:  "go.mod: cannot read",
		},
		"a Node.js version that is not one release": {
			files: map[string]string{"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "lts/*\n"},
			want:  `holds "lts/*", not one exact stable release`,
		},
		"a workflow that is not YAML": {
			files: map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
				".github/workflows/ci.yml": "jobs: [\n",
			},
			want: ".github/workflows/ci.yml",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			r := w.checklatest(t, writeTree(t, tt.files), nil)
			wantExit(t, r, exitUnchecked)
			wantContains(t, "stderr", r.Stderr, tt.want)
			if r.Stdout != "" {
				t.Errorf("stdout = %q, want no verdicts from a repository that could not be read", r.Stdout)
			}
		})
	}
}

// TestChecklatest_AMalformedCommandLineExitsTwo checks a bad flag, or a
// source URL that cannot be fetched from, is refused with the usage before
// anything is fetched.
func TestChecklatest_AMalformedCommandLineExitsTwo(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":             {"-no-such-flag"},
		"malformed tool":           {"-tool", "lint"},
		"tool with no version":     {"-tool", "lint=example.com/tools/lint@"},
		"relative source URL":      {"-go-releases-url", "go.dev/VERSION"},
		"actions over ssh":         {"-actions-base-url", "ssh://git@github.com"},
		"HTTP source over file://": {"-node-index-url", "file:///srv/index.json"},
	} {
		t.Run(name, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Checklatest, nil, args...)
			wantExit(t, r, exitUnchecked)
			wantContains(t, "stderr", r.Stderr, "Usage of checklatest")
			if r.Stdout != "" {
				t.Errorf("stdout = %q, want nothing checked", r.Stdout)
			}
		})
	}
}

// fileIssueArgs are the -file-issue arguments against the Gitea stand-in.
func fileIssueArgs(g *giteaStandIn, extra ...string) []string {
	return append([]string{"-file-issue", "-gitea-api", g.api(), "-repo", "o/r", "-run-url", "https://ci.example/runs/42"}, extra...)
}

// tokenEnv is the environment checklatest reads the Gitea token from.
var tokenEnv = []string{"GITEA_TOKEN=" + giteaToken}

// TestChecklatest_FileIssueFilesTheLagOnce checks the daily workflow's path:
// a lag is filed as one labelled issue listing what lags, and a second run
// while it is open files nothing.
func TestChecklatest_FileIssueFilesTheLagOnce(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
	g := newGitea(t)
	root := currentRepo(t, w.setupGo)

	r := w.checklatest(t, root, tokenEnv, fileIssueArgs(g)...)

	wantExit(t, r, exitCurrent)
	wantContains(t, "stdout", r.Stdout, "check-latest: filed issue #1 (task:ready)")
	is, ok := g.issue(1)
	if !ok {
		t.Fatalf("no issue #1 was created; requests: %v", g.requests)
	}
	wantContains(t, "issue body", is.Body, "<!-- go-sysmon:check-latest -->")
	wantContains(t, "issue body", is.Body, "| go | 1.27.1 | go1.27.2 | behind |")
	wantContains(t, "issue body", is.Body, "([run](https://ci.example/runs/42))")
	if len(is.Labels) != 1 || is.Labels[0].Name != "task:ready" {
		t.Errorf("issue labels = %v, want task:ready", is.Labels)
	}

	posted := g.posted()
	again := w.checklatest(t, root, tokenEnv, fileIssueArgs(g)...)
	wantExit(t, again, exitCurrent)
	wantContains(t, "stdout", again.Stdout, "issue #1 is still open for this; not filing a duplicate")
	if g.posted() != posted {
		t.Errorf("the second run posted to Gitea while issue #1 was open")
	}
}

// TestChecklatest_FileIssueFindsAnOpenIssueOnALaterPage checks the search for
// an open issue pages through Gitea's 50-item pages, and that a pull request
// carrying the marker is not mistaken for the issue.
func TestChecklatest_FileIssueFindsAnOpenIssueOnALaterPage(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
	g := newGitea(t)
	for i := 1; i <= pageSize; i++ {
		is := giteaIssue{Number: int64(i), Title: "unrelated " + strconv.Itoa(i)}
		if i == 3 {
			is.Body = "quoting <!-- go-sysmon:check-latest --> in a pull request"
			is.PullRequest = &struct{}{}
		}
		g.issues = append(g.issues, is)
	}
	g.issues = append(g.issues, giteaIssue{Number: 51, Body: "<!-- go-sysmon:check-latest -->\nlag"})

	r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

	wantExit(t, r, exitCurrent)
	wantContains(t, "stdout", r.Stdout, "issue #51 is still open")
	if g.posted() != 0 {
		t.Errorf("filed while issue #51 was open")
	}
}

// TestChecklatest_FileIssueDryRunPrintsTheIssue checks a dry run searches,
// prints the issue it would file, and writes nothing.
func TestChecklatest_FileIssueDryRunPrintsTheIssue(t *testing.T) {
	w := newWorld(t)
	w.up.module(lintModule, "v2.14.0", "v2.15.0")
	g := newGitea(t)

	r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g, "-dry-run")...)

	wantExit(t, r, exitCurrent)
	wantContains(t, "stdout", r.Stdout, "check-latest: dry run; would file this issue with label task:ready")
	wantContains(t, "stdout", r.Stdout, "| lint | v2.14.0 | v2.15.0 | behind |")
	if g.posted() != 0 {
		t.Errorf("a dry run posted to Gitea: %v", g.requests)
	}
}

// TestChecklatest_FileIssueWithNothingLaggingFilesNothing checks a current
// repository is reported as such and nothing is asked of Gitea.
func TestChecklatest_FileIssueWithNothingLaggingFilesNothing(t *testing.T) {
	w := newWorld(t)
	g := newGitea(t)

	r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

	wantExit(t, r, exitCurrent)
	wantContains(t, "stdout", r.Stdout, "nothing to file")
	if len(g.requests) != 0 {
		t.Errorf("Gitea was asked %v with nothing to file", g.requests)
	}
}

// TestChecklatest_FileIssueReappliesALabelThatDidNotStick covers Gitea
// acknowledging a label without applying it: the label is applied again and
// read back, and only a second failure is reported, naming the issue.
func TestChecklatest_FileIssueReappliesALabelThatDidNotStick(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		w := newWorld(t)
		w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
		g := newGitea(t)
		g.dropLabels = 1

		r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

		wantExit(t, r, exitCurrent)
		if is, _ := g.issue(1); len(is.Labels) != 1 {
			t.Errorf("issue #1 labels = %v, want task:ready applied on the second attempt", is.Labels)
		}
	})
	t.Run("twice", func(t *testing.T) {
		w := newWorld(t)
		w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
		g := newGitea(t)
		g.dropLabels = 2

		r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

		wantExit(t, r, exitUnchecked)
		wantContains(t, "stderr", r.Stderr, `issue #1 was filed but label "task:ready" did not stick; label it by hand`)
	})
}

// TestChecklatest_FileIssueReportsGiteaFailures checks each way the tracker
// can fail ends the run with no verdict and the reason.
func TestChecklatest_FileIssueReportsGiteaFailures(t *testing.T) {
	tests := map[string]struct {
		setup func(g *giteaStandIn)
		env   []string
		want  string
	}{
		"the issue list fails": {
			setup: func(g *giteaStandIn) { g.failList = 500 },
			env:   tokenEnv,
			want:  "HTTP 500",
		},
		"the token is refused": {
			setup: func(*giteaStandIn) {},
			env:   []string{"GITEA_TOKEN=wrong"},
			want:  "HTTP 401",
		},
		"the repository has no task:ready label": {
			setup: func(g *giteaStandIn) { g.labels = g.labels[:pageSize] },
			env:   tokenEnv,
			want:  `the repository has no label "task:ready"`,
		},
		"the label list fails": {
			setup: func(g *giteaStandIn) { g.failLabels = 503 },
			env:   tokenEnv,
			want:  "gitea: GET /repos/o/r/labels?limit=50&page=1: HTTP 503",
		},
		"the issue cannot be created": {
			setup: func(g *giteaStandIn) { g.failCreate = 422 },
			env:   tokenEnv,
			want:  "gitea: POST /repos/o/r/issues: HTTP 422",
		},
		"the new issue cannot be read back": {
			setup: func(g *giteaStandIn) { g.failGet = 500 },
			env:   tokenEnv,
			want:  "gitea: GET /repos/o/r/issues/1: HTTP 500",
		},
		"the label cannot be applied again": {
			setup: func(g *giteaStandIn) { g.dropLabels, g.failApply = 1, 500 },
			env:   tokenEnv,
			want:  "gitea: POST /repos/o/r/issues/1/labels: HTTP 500",
		},
		"the issue list is not JSON": {
			setup: func(g *giteaStandIn) { g.garbled = true },
			env:   tokenEnv,
			want:  "gitea: GET /repos/o/r/issues?limit=50&page=1&state=open&type=issues: invalid character",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
			g := newGitea(t)
			tt.setup(g)

			r := w.checklatest(t, currentRepo(t, w.setupGo), tt.env, fileIssueArgs(g)...)

			wantExit(t, r, exitUnchecked)
			wantContains(t, "stderr", r.Stderr, tt.want)
		})
	}
}

// TestChecklatest_FileIssueCutsALongErrorBody checks a Gitea error answered
// with a large body is reported with the start of it, not all of it.
func TestChecklatest_FileIssueCutsALongErrorBody(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
	g := newGitea(t)
	g.failList, g.verbose = 500, true

	r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

	wantExit(t, r, exitUnchecked)
	wantContains(t, "stderr", r.Stderr, "HTTP 500: {\"message\":\"stack frame")
	if n := strings.Count(r.Stderr, "stack frame"); n == 0 || n >= 200 {
		t.Errorf("the error carries %d of the body's 200 frames, want it cut to 512 bytes", n)
	}
}

// TestChecklatest_FileIssueReportsAnUnreachableGitea covers a Gitea that is
// not there, and an API root that is not a URL.
func TestChecklatest_FileIssueReportsAnUnreachableGitea(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
	gone := newGitea(t)
	gone.srv.Close()
	root := currentRepo(t, w.setupGo)

	down := w.checklatest(t, root, tokenEnv, fileIssueArgs(gone)...)
	wantExit(t, down, exitUnchecked)
	wantContains(t, "stderr", down.Stderr, "gitea: GET /repos/o/r/issues")
	wantContains(t, "stderr", down.Stderr, "connection refused")

	bad := w.checklatest(t, root, tokenEnv, "-file-issue", "-gitea-api", "http://gitea.example:port/api/v1", "-repo", "o/r")
	wantExit(t, bad, exitUnchecked)
	wantContains(t, "stderr", bad.Stderr, "gitea: GET /repos/o/r/issues")
	wantContains(t, "stderr", bad.Stderr, "invalid port")
}

// TestChecklatest_FileIssueFilesTheLagEvenWithAnUncheckedItem checks what
// lags is filed although another item had no verdict, and the run still
// fails so the unchecked item is seen.
func TestChecklatest_FileIssueFilesTheLagEvenWithAnUncheckedItem(t *testing.T) {
	w := newWorld(t)
	w.up.set("/go/VERSION", reply{body: "go1.27.2\n"})
	w.up.set("/node/index.json", reply{status: 502})
	g := newGitea(t)

	r := w.checklatest(t, currentRepo(t, w.setupGo), tokenEnv, fileIssueArgs(g)...)

	wantExit(t, r, exitUnchecked)
	wantContains(t, "stdout", r.Stdout, "filed issue #1")
	wantContains(t, "stderr", r.Stderr, "pins: node: no verdict")
}

// TestChecklatest_FileIssueNeedsItsSettings checks a misconfigured workflow
// fails before anything is fetched.
func TestChecklatest_FileIssueNeedsItsSettings(t *testing.T) {
	w := newWorld(t)
	root := currentRepo(t, w.setupGo)
	g := newGitea(t)

	noAPI := w.checklatest(t, root, tokenEnv, "-file-issue", "-repo", "o/r")
	wantExit(t, noAPI, exitUnchecked)
	wantContains(t, "stderr", noAPI.Stderr, "-file-issue needs -gitea-api and -repo owner/name")

	noToken := w.checklatest(t, root, []string{"GITEA_TOKEN="}, fileIssueArgs(g)...)
	wantExit(t, noToken, exitUnchecked)
	wantContains(t, "stderr", noToken.Stderr, "-file-issue needs an API token")
	if noAPI.Stdout != "" || noToken.Stdout != "" {
		t.Errorf("a misconfigured run judged items: %q %q", noAPI.Stdout, noToken.Stdout)
	}
}
