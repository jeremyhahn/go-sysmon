//go:build integration

package tools

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// lintModule is the module the synthetic repositories pin a tool from. It is
// served by the module proxy stand-in and by nothing on the network.
const lintModule = "example.com/tools/lint"

// writeTree writes files, keyed by slash-separated path, under a fresh
// directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// reply is how the upstream stand-in answers one path.
type reply struct {
	status int
	body   string
	// hang answers nothing until the client gives up, like a source that
	// accepted the connection and stalled.
	hang bool
	// short promises more body than it sends, like a connection dropped
	// mid-answer.
	short bool
}

// upstream stands in for every HTTP source checklatest reads -- go.dev,
// nodejs.org, the Debian archive, Launchpad, the runner-images README -- and
// for a Go module proxy. A path with no reply is a 404.
type upstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]reply
}

// newUpstream starts the stand-in answering as the public sources would on a
// day when the synthetic repository is current.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{routes: map[string]reply{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		rep, ok := u.routes[r.URL.Path]
		u.mu.Unlock()
		switch {
		case !ok:
			http.NotFound(w, r)
		case rep.hang:
			<-r.Context().Done()
		default:
			if rep.short {
				w.Header().Set("Content-Length", strconv.Itoa(len(rep.body)+100))
			}
			if rep.status != 0 {
				w.WriteHeader(rep.status)
			}
			if _, err := io.WriteString(w, rep.body); err != nil {
				t.Errorf("upstream %s: %v", r.URL.Path, err)
			}
		}
	}))
	t.Cleanup(u.srv.Close)

	u.set("/go/VERSION", reply{body: "go1.27.1\ntime 2026-09-02T17:00:00Z\n"})
	u.set("/node/index.json", reply{body: `[
		{"version":"v26.2.0","lts":false},
		{"version":"v24.21.0","lts":"Krypton"},
		{"version":"v24.20.0","lts":"Krypton"},
		{"version":"v22.21.0","lts":"Jod"}]`})
	u.debian("stable", "trixie", "13.7")
	u.debian("trixie", "trixie", "13.7")
	// Launchpad pages its series list; the newest LTS is on the second page.
	u.set("/ubuntu/series", reply{body: `{"entries":[
		{"version":"24.04","status":"Supported"},
		{"version":"25.10","status":"Current Stable Release"}],
		"next_collection_link":"` + u.srv.URL + `/ubuntu/series/2"}`})
	u.set("/ubuntu/series/2", reply{body: `{"entries":[
		{"version":"26.04","status":"Supported"},
		{"version":"26.10","status":"Active Development"}]}`})
	u.runners("ubuntu-26.04", "ubuntu-26.04-arm", "ubuntu-24.04")
	u.module(lintModule, "v2.13.0", "v2.14.0")
	return u
}

// set replaces the reply for path.
func (u *upstream) set(path string, r reply) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.routes[path] = r
}

// debian publishes a suite's Release file.
func (u *upstream) debian(suite, codename, version string) {
	body := "Origin: Debian\nLabel: Debian\nSuite: " + suite + "\nCodename: " + codename + "\n"
	if version != "" {
		body += "Version: " + version + "\n"
	}
	u.set("/debian/dists/"+suite+"/Release", reply{body: body + "Date: Sat, 06 Sep 2026 09:00:00 UTC\nSHA256:\n 0123 12 main/Release\n"})
}

// runners publishes the runner-images table with one row per label, GA
// unless the label ends in "!beta", and a beta row for ubuntu-28.04 that must
// never count as the latest.
func (u *upstream) runners(labels ...string) {
	var b strings.Builder
	b.WriteString("# Available images\n\n| Image | Architecture | YAML Label | Included Software |\n")
	b.WriteString("| ----- | ------------ | ---------- | ----------------- |\n")
	for _, l := range labels {
		image := "Ubuntu"
		if name, beta := strings.CutSuffix(l, "!beta"); beta {
			l, image = name, "Ubuntu ![beta](https://img.shields.io/badge/beta-yellow)"
		}
		b.WriteString("| " + image + " | x64 | `" + l + "` | [list](x) |\n")
	}
	b.WriteString("| Ubuntu 28.04 ![beta](https://img.shields.io/badge/beta-yellow) | x64 | `ubuntu-28.04` | [list](x) |\n")
	u.set("/runner-images/README.md", reply{body: b.String()})
}

// module publishes versions of a module on the proxy: the list, and each
// version's .info and .mod, which is everything `go list -m @latest` reads.
func (u *upstream) module(path string, versions ...string) {
	base := "/goproxy/" + path + "/@v/"
	u.set(base+"list", reply{body: strings.Join(versions, "\n") + "\n"})
	for _, v := range versions {
		u.set(base+v+".info", reply{body: `{"Version":"` + v + `","Time":"2026-09-01T00:00:00Z"}`})
		u.set(base+v+".mod", reply{body: "module " + path + "\n\ngo 1.24\n"})
	}
}

// urlFlags points every checklatest source at the stand-in, and the actions
// at the git mirrors under actions.
func (u *upstream) urlFlags(actions string) []string {
	return []string{
		"-go-releases-url", u.srv.URL + "/go/VERSION",
		"-node-index-url", u.srv.URL + "/node/index.json",
		"-debian-dists-url", u.srv.URL + "/debian/dists",
		"-ubuntu-series-url", u.srv.URL + "/ubuntu/series",
		"-runner-images-url", u.srv.URL + "/runner-images/README.md",
		"-actions-base-url", "file://" + actions,
	}
}

// goEnv makes `go list -m` resolve modules through the stand-in proxy alone,
// into a module cache of the test's own, with no checksum database and no
// toolchain download.
func (u *upstream) goEnv(t *testing.T) []string {
	t.Helper()
	cache := t.TempDir()
	// The go command writes its module cache read-only; make it writable
	// again so the temporary directory can be removed.
	t.Cleanup(func() {
		err := filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o700)
		})
		if err != nil {
			t.Errorf("make the module cache removable: %v", err)
		}
	})
	return []string{
		"GOPROXY=" + u.srv.URL + "/goproxy",
		"GOSUMDB=off",
		"GONOPROXY=", "GONOSUMDB=", "GOPRIVATE=",
		"GOMODCACHE=" + cache,
		"GOTOOLCHAIN=local",
	}
}

// mirror is a directory of bare git repositories laid out as owner/repo, which
// is what checklatest's -actions-base-url names as a file URL. git ls-remote
// reads it with git's own transport, so the tags are listed exactly as they
// would be from github.com, peeled annotated tags included.
type mirror struct {
	dir string
	env []string
}

func newMirror(t *testing.T) *mirror {
	t.Helper()
	home := t.TempDir()
	return &mirror{dir: t.TempDir(), env: append(os.Environ(),
		"HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)}
}

// git runs git in dir.
func (m *mirror) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = m.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo publishes repo ("actions/checkout") with tags, in order. A tag "a=b"
// is a lightweight tag a on the commit tag b names, the way a moving major
// tag follows its release; any other tag gets a commit of its own and is
// annotated, so ls-remote lists it with a peeled ^{} line. It returns each
// tag's commit.
func (m *mirror) repo(t *testing.T, repo string, tags ...string) map[string]string {
	t.Helper()
	work := t.TempDir()
	m.git(t, work, "init", "-q", "-b", "main")
	commits := map[string]string{}
	for _, tag := range tags {
		if name, target, alias := strings.Cut(tag, "="); alias {
			m.git(t, work, "tag", name, target)
			commits[name] = commits[target]
			continue
		}
		m.git(t, work, "commit", "-q", "--allow-empty", "-m", "release "+tag)
		m.git(t, work, "tag", "-a", tag, "-m", tag)
		commits[tag] = m.git(t, work, "rev-parse", "HEAD")
	}
	// A repository published again replaces the earlier one, as a push of
	// new tags would.
	bare := filepath.Join(m.dir, filepath.FromSlash(repo))
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	m.git(t, work, "clone", "-q", "--bare", work, bare)
	return commits
}

// currentRepo is a repository whose every pin is current against the default
// stand-ins: Go 1.27.1, Node.js 24.21.0, actions/checkout on its v7 line, and
// actions/setup-go pinned by the commit of its latest release.
func currentRepo(t *testing.T, setupGoSHA string) string {
	t.Helper()
	return repoWith(t, "1.27.1", setupGoSHA, "")
}

// repoWith is currentRepo with its go directive, and one more step using
// extraUses when that is set.
func repoWith(t *testing.T, goDirective, setupGoSHA, extraUses string) string {
	t.Helper()
	extra := ""
	if extraUses != "" {
		extra = "      - uses: " + extraUses + "\n"
	}
	return writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo " + goDirective + "\n",
		".node-version": "v24.21.0\n",
		".github/workflows/ci.yml": `jobs:
  build:
    runs-on: ubuntu-26.04
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@` + setupGoSHA + ` # v6.0.0
      - uses: ./.github/actions/local
  arm:
    runs-on: ubuntu-26.04-arm
    steps:
      - uses: actions/checkout@v7
` + extra,
	})
}

// world is everything one checklatest run talks to.
type world struct {
	up     *upstream
	mirror *mirror
	// setupGo is the commit actions/setup-go's latest release is on.
	setupGo string
	// checkout is the commit of each actions/checkout tag.
	checkout map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{up: newUpstream(t), mirror: newMirror(t)}
	// 7.0.2 is tagged without the "v" some repositories leave off.
	w.checkout = w.mirror.repo(t, "actions/checkout", "v6.0.0", "v7.0.0", "v7.0.1", "v7=v7.0.1", "7.0.2", "v8.0.0-rc.1")
	w.setupGo = w.mirror.repo(t, "actions/setup-go", "v5.9.0", "v6.0.0", "v6=v6.0.0")["v6.0.0"]
	return w
}

// checklatest runs the command against root with every source in w, the
// pins the synthetic repositories use, and extra arguments.
func (w *world) checklatest(t *testing.T, root string, env []string, extra ...string) harness.Result {
	t.Helper()
	args := append([]string{
		"-root", root, "-debian", "trixie", "-ubuntu", "26.04", "-hosted-runner", "26.04",
		"-tool", "lint=" + lintModule + "@v2.14.0", "-timeout", "60s",
	}, w.up.urlFlags(w.mirror.dir)...)
	return harness.Run(t, harness.Bins.Checklatest, append(w.up.goEnv(t), env...), append(args, extra...)...)
}
