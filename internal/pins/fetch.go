package pins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// GoVersionURL is where the Go project publishes its latest stable release,
// one version per line with the newest first ("go1.27.1\ntime ...").
const GoVersionURL = "https://go.dev/VERSION?m=text"

// maxBody bounds how much of any release document is read. The largest,
// Node.js's index.json, is a few hundred kilobytes.
const maxBody = 8 << 20

// get fetches url and returns its body, refusing any status but 200.
func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err := resp.Body.Close(); err != nil && readErr == nil {
		readErr = err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &FetchResponseError{Source: url, Status: resp.StatusCode}
	}
	if readErr != nil {
		return nil, readErr
	}
	return body, nil
}

// GoReleases fetches the latest stable Go release over HTTP.
type GoReleases struct {
	Client *http.Client
	URL    string
}

// Latest returns the first line of the version document, which must be an
// exact stable Go release.
func (g GoReleases) Latest(ctx context.Context) (string, error) {
	body, err := get(ctx, g.Client, g.URL)
	if err != nil {
		return "", err
	}
	first, _, _ := bytes.Cut(body, []byte("\n"))
	v := strings.TrimSpace(string(first))
	if !IsGoRelease(v) || !strings.HasPrefix(v, "go") {
		return "", &FetchResponseError{Source: g.URL, Detail: "first line " + quoteShort(v) + " is not a stable Go release"}
	}
	return v, nil
}

// NodeIndexURL is the Node.js release index: every release, newest first,
// each with its LTS codename or false.
const NodeIndexURL = "https://nodejs.org/dist/index.json"

// NodeReleases fetches the latest Node.js LTS release.
type NodeReleases struct {
	Client *http.Client
	URL    string
}

// LatestLTS returns the newest release on an LTS line, "24.21.0".
//
// This is LTS, not Current, deliberately: Current (the newest major, 26 while
// 24 is LTS) is a line that changes incompatibly within months and that
// Node.js does not recommend for production. The newest LTS line is always
// the Active LTS one; older LTS lines are in Maintenance, and their releases
// are never the newest LTS release.
func (n NodeReleases) LatestLTS(ctx context.Context) (string, error) {
	body, err := get(ctx, n.Client, n.URL)
	if err != nil {
		return "", err
	}
	var releases []struct {
		Version string          `json:"version"`
		LTS     json.RawMessage `json:"lts"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", &FetchResponseError{Source: n.URL, Detail: "unreadable index: " + err.Error()}
	}
	latest := ""
	for _, r := range releases {
		// lts is false for a Current release and a codename string for LTS.
		if len(r.LTS) == 0 || r.LTS[0] != '"' {
			continue
		}
		v := strings.TrimPrefix(r.Version, "v")
		if IsExactRelease(v) && (latest == "" || semver.Compare("v"+v, "v"+latest) > 0) {
			latest = v
		}
	}
	if latest == "" {
		return "", &FetchResponseError{Source: n.URL, Detail: "no LTS release in the index"}
	}
	return latest, nil
}

// DebianDistsURL is the root of the Debian archive's suites.
const DebianDistsURL = "https://deb.debian.org/debian/dists"

// DebianReleases reads suites' Release files from the Debian archive.
type DebianReleases struct {
	Client *http.Client
	URL    string
}

// Release returns the codename and version a suite's Release file declares.
func (d DebianReleases) Release(ctx context.Context, suite string) (DebianRelease, error) {
	url := strings.TrimSuffix(d.URL, "/") + "/" + suite + "/Release"
	body, err := get(ctx, d.Client, url)
	if err != nil {
		return DebianRelease{}, err
	}
	var r DebianRelease
	// The fields come first, before the checksum lists; a line that does not
	// start with a field name ends the header.
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || strings.HasPrefix(line, " ") {
			continue
		}
		switch key {
		case "Codename":
			r.Codename = strings.TrimSpace(value)
		case "Version":
			r.Version = strings.TrimSpace(value)
		}
	}
	if !debianCodename.MatchString(r.Codename) {
		return DebianRelease{}, &FetchResponseError{Source: url, Detail: "no Codename field"}
	}
	return r, nil
}

// UbuntuSeriesURL lists every Ubuntu series with its version and status.
const UbuntuSeriesURL = "https://api.launchpad.net/devel/ubuntu/series"

// UbuntuReleases finds the latest Ubuntu LTS release from Launchpad.
type UbuntuReleases struct {
	Client *http.Client
	URL    string
}

// ubuntuLTS matches an LTS release number: April of an even year.
var ubuntuLTS = regexp.MustCompile(`^[0-9]{1}[02468]\.04$`)

// ubuntuReleased are the Launchpad statuses of a published, supported series.
var ubuntuReleased = map[string]bool{
	"Current Stable Release": true,
	"Supported":              true,
}

// LatestLTS returns the newest released LTS, "26.04". A series in
// development or frozen for release is not released, and an obsolete one is
// not supported.
func (u UbuntuReleases) LatestLTS(ctx context.Context) (string, error) {
	latest := ""
	for next := u.URL; next != ""; {
		body, err := get(ctx, u.Client, next)
		if err != nil {
			return "", err
		}
		var page struct {
			Entries []struct {
				Version string `json:"version"`
				Status  string `json:"status"`
			} `json:"entries"`
			Next string `json:"next_collection_link"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return "", &FetchResponseError{Source: next, Detail: "unreadable series list: " + err.Error()}
		}
		for _, e := range page.Entries {
			if ubuntuReleased[e.Status] && ubuntuLTS.MatchString(e.Version) && e.Version > latest {
				latest = e.Version
			}
		}
		next = page.Next
	}
	if latest == "" {
		return "", &FetchResponseError{Source: u.URL, Detail: "no released LTS series"}
	}
	return latest, nil
}

// RunFunc runs one of the fixed commands this package uses ("go" or "git")
// and returns its standard output. It is what a test replaces.
type RunFunc func(ctx context.Context, command string, args ...string) ([]byte, error)

// GoList resolves a module's latest version with `go list -m`, so it honours
// the same GOPROXY, GOPRIVATE and GONOSUMDB settings as `go install` does when
// `make tools` installs the pin.
type GoList struct {
	Run RunFunc
}

// commands are the programs Run may start, with the environment each needs.
// Nothing else can be run: the program is never caller-supplied.
var commands = map[string][]string{
	// No workspace, and module-mode resolution, so a query answers the same
	// way anywhere in the tree.
	"go": {"GOWORK=off", "GOFLAGS=-mod=mod"},
	// Never prompt for credentials: an unreachable or private repository is
	// an error, not a hang.
	"git": {"GIT_TERMINAL_PROMPT=0"},
}

// Run runs one of the fixed commands on PATH, returning standard output. A
// failure carries the command's standard error, which is where go and git
// explain themselves.
func Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	env, ok := commands[command]
	if !ok {
		return nil, &CommandError{Command: command, Cause: ErrUnknownCommand}
	}
	// #nosec G204 -- the program is one of the fixed names in commands; args
	// reach it as argv with no shell, and callers pass only fixed queries plus
	// a module path or action repository taken from the repository's pins.
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &CommandError{Command: command + " " + strings.Join(args, " "), Stderr: strings.TrimSpace(stderr.String()), Cause: err}
	}
	return out, nil
}

// Latest returns the version `go list -m -json module@latest` reports. The
// go command's @latest is the newest release, excluding retracted versions,
// and falls back to a prerelease only when the module has no release at all.
func (g GoList) Latest(ctx context.Context, module string) (string, error) {
	out, err := g.Run(ctx, "go", "list", "-m", "-json", module+"@latest")
	if err != nil {
		return "", err
	}
	var info struct {
		Path    string
		Version string
		Error   *struct{ Err string }
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", &FetchResponseError{Source: "go list -m " + module, Detail: "unreadable output: " + err.Error()}
	}
	if info.Error != nil {
		return "", &FetchResponseError{Source: "go list -m " + module, Detail: info.Error.Err}
	}
	if info.Path != module || info.Version == "" {
		return "", &FetchResponseError{Source: "go list -m " + module, Detail: "reported " + quoteShort(info.Path+"@"+info.Version)}
	}
	return info.Version, nil
}

// quoteShort quotes s for an error message, cut to a readable length.
func quoteShort(s string) string {
	const limit = 80
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return strconv.Quote(s)
}

// ActionsBaseURL is where the workflows' actions are released from.
const ActionsBaseURL = "https://github.com"

// GitTags lists an action repository's tags with `git ls-remote`, which needs
// no API token and has no rate limit.
type GitTags struct {
	Run RunFunc
	// BaseURL is where repositories are cloned from, "https://github.com".
	BaseURL string
}

// Tags returns the tags of repo, "actions/checkout".
func (g GitTags) Tags(ctx context.Context, repo string) ([]Tag, error) {
	out, err := g.Run(ctx, "git", "ls-remote", "--tags", strings.TrimSuffix(g.BaseURL, "/")+"/"+repo)
	if err != nil {
		return nil, err
	}
	return ParseLsRemote(out)
}
