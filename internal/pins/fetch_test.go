package pins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveVersion starts a local server answering the version document with
// status and body.
func serveVersion(t *testing.T, status int, body string) GoReleases {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return GoReleases{Client: srv.Client(), URL: srv.URL}
}

func TestGoReleases_Latest(t *testing.T) {
	g := serveVersion(t, http.StatusOK, "go1.27.1\ntime 2026-08-28T16:20:06Z\n")

	got, err := g.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got != "go1.27.1" {
		t.Errorf("Latest() = %q, want go1.27.1", got)
	}
}

func TestGoReleases_RefusesABadAnswer(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
	}{
		"not found":         {http.StatusNotFound, "go1.27.1\n"},
		"server error":      {http.StatusBadGateway, ""},
		"html error page":   {http.StatusOK, "<!doctype html>\n"},
		"empty body":        {http.StatusOK, ""},
		"release candidate": {http.StatusOK, "go1.28rc1\n"},
		"missing go prefix": {http.StatusOK, "1.27.1\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := serveVersion(t, tt.status, tt.body).Latest(context.Background())
			var ferr *FetchResponseError
			if !errors.As(err, &ferr) {
				t.Fatalf("Latest() error = %v, want *FetchResponseError", err)
			}
		})
	}
}

func TestGoReleases_UnreachableIsAnError(t *testing.T) {
	// A closed listener refuses the connection, as an offline host does.
	closed := httptest.NewServer(http.NotFoundHandler())
	g := GoReleases{Client: closed.Client(), URL: closed.URL}
	closed.Close()

	if _, err := g.Latest(context.Background()); err == nil {
		t.Fatal("Latest() against a closed listener error = nil")
	}
}

func TestGoReleases_BadURLIsAnError(t *testing.T) {
	g := GoReleases{Client: http.DefaultClient, URL: "://no-scheme"}
	if _, err := g.Latest(context.Background()); err == nil {
		t.Fatal("Latest() with a malformed URL error = nil")
	}
}

// runReturning is a RunFunc that records its arguments and answers out, err.
func runReturning(out string, err error, gotArgs *[]string) RunFunc {
	return func(_ context.Context, command string, args ...string) ([]byte, error) {
		*gotArgs = append([]string{command}, args...)
		return []byte(out), err
	}
}

func TestGoList_Latest(t *testing.T) {
	var args []string
	g := GoList{Run: runReturning(`{"Path":"golang.org/x/vuln","Version":"v1.8.0","Time":"2026-09-01T00:00:00Z"}`, nil, &args)}

	got, err := g.Latest(context.Background(), "golang.org/x/vuln")
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got != "v1.8.0" {
		t.Errorf("Latest() = %q, want v1.8.0", got)
	}
	if want := "go list -m -json golang.org/x/vuln@latest"; strings.Join(args, " ") != want {
		t.Errorf("ran %q, want %q", strings.Join(args, " "), want)
	}
}

func TestGoList_RefusesABadAnswer(t *testing.T) {
	tests := map[string]string{
		"not json":       "go: downloading...",
		"query error":    `{"Path":"golang.org/x/vuln","Error":{"Err":"no matching versions"}}`,
		"another module": `{"Path":"golang.org/x/mod","Version":"v0.41.0"}`,
		"no version":     `{"Path":"golang.org/x/vuln"}`,
	}
	for name, out := range tests {
		t.Run(name, func(t *testing.T) {
			var args []string
			_, err := GoList{Run: runReturning(out, nil, &args)}.Latest(context.Background(), "golang.org/x/vuln")
			var ferr *FetchResponseError
			if !errors.As(err, &ferr) {
				t.Fatalf("Latest() error = %v, want *FetchResponseError", err)
			}
		})
	}
}

func TestGoList_CommandFailureIsReturned(t *testing.T) {
	var args []string
	cause := &CommandError{Command: "go list", Stderr: "dial tcp: lookup proxy.golang.org: no such host", Cause: errors.New("exit status 1")}
	_, err := GoList{Run: runReturning("", cause, &args)}.Latest(context.Background(), "golang.org/x/vuln")
	if !errors.Is(err, cause) {
		t.Fatalf("Latest() error = %v, want the command's error", err)
	}
}

func TestRun(t *testing.T) {
	out, err := Run(context.Background(), "go", "env", "GOWORK")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// GOWORK=off is how the go command reports a disabled workspace.
	if got := strings.TrimSpace(string(out)); got != "off" {
		t.Errorf("GOWORK = %q, want off: the lookup must not see a workspace", got)
	}
}

func TestRun_FailureCarriesStderr(t *testing.T) {
	_, err := Run(context.Background(), "go", "no-such-subcommand")
	var cerr *CommandError
	if !errors.As(err, &cerr) {
		t.Fatalf("Run() error = %v, want *CommandError", err)
	}
	if !strings.Contains(cerr.Stderr, "no-such-subcommand") {
		t.Errorf("Stderr = %q, want the go command's complaint", cerr.Stderr)
	}
	if !strings.Contains(cerr.Error(), cerr.Stderr) {
		t.Errorf("Error() = %q does not include stderr", cerr.Error())
	}
}

func TestRun_OnlyKnownCommands(t *testing.T) {
	_, err := Run(context.Background(), "sh", "-c", "true")
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Run(sh) error = %v, want ErrUnknownCommand", err)
	}
}

// serveBody starts a local server answering every request with status and
// body, and returns its URL.
func serveBody(t *testing.T, status int, body string) (*http.Client, string) {
	t.Helper()
	g := serveVersion(t, status, body)
	return g.Client, g.URL
}

// closedURL returns a client and the URL of a listener that is already
// closed, which refuses connections as an offline host does.
func closedURL(t *testing.T) (*http.Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.Client(), srv.URL
}

const nodeIndex = `[
 {"version":"v26.10.0","lts":false},
 {"version":"v24.21.0","lts":"Krypton"},
 {"version":"v22.23.3","lts":"Jod"},
 {"version":"v24.9.0","lts":"Krypton"}
]`

func TestNodeReleases_LatestLTS(t *testing.T) {
	client, url := serveBody(t, http.StatusOK, nodeIndex)

	got, err := NodeReleases{Client: client, URL: url}.LatestLTS(context.Background())
	if err != nil {
		t.Fatalf("LatestLTS() error = %v", err)
	}
	// Current (26) is newer but not LTS; 24.21.0 beats 24.9.0 numerically.
	if got != "24.21.0" {
		t.Errorf("LatestLTS() = %q, want 24.21.0", got)
	}
}

func TestNodeReleases_RefusesABadAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
	}{
		"server error": {http.StatusInternalServerError, ""},
		"not json":     {http.StatusOK, "<html>"},
		"no lts":       {http.StatusOK, `[{"version":"v26.10.0","lts":false}]`},
	} {
		t.Run(name, func(t *testing.T) {
			client, url := serveBody(t, tt.status, tt.body)
			_, err := NodeReleases{Client: client, URL: url}.LatestLTS(context.Background())
			var ferr *FetchResponseError
			if !errors.As(err, &ferr) {
				t.Fatalf("LatestLTS() error = %v, want *FetchResponseError", err)
			}
		})
	}
}

func TestNodeReleases_OfflineIsAnError(t *testing.T) {
	client, url := closedURL(t)
	if _, err := (NodeReleases{Client: client, URL: url}).LatestLTS(context.Background()); err == nil {
		t.Fatal("LatestLTS() offline error = nil")
	}
}

func TestDebianReleases_Release(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if _, err := w.Write([]byte("Origin: Debian\nSuite: stable\nVersion: 13.7\nCodename: trixie\nMD5Sum:\n 0123 456 main/Release\n")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := DebianReleases{Client: srv.Client(), URL: srv.URL + "/debian/dists/"}.Release(context.Background(), "stable")
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if want := (DebianRelease{Codename: "trixie", Version: "13.7"}); got != want {
		t.Errorf("Release() = %+v, want %+v", got, want)
	}
	if asked != "/debian/dists/stable/Release" {
		t.Errorf("fetched %q, want /debian/dists/stable/Release", asked)
	}
}

func TestDebianReleases_RefusesABadAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
	}{
		"not found":   {http.StatusNotFound, ""},
		"no codename": {http.StatusOK, "Origin: Debian\nVersion: 13.7\n"},
	} {
		t.Run(name, func(t *testing.T) {
			client, url := serveBody(t, tt.status, tt.body)
			_, err := DebianReleases{Client: client, URL: url}.Release(context.Background(), "stable")
			var ferr *FetchResponseError
			if !errors.As(err, &ferr) {
				t.Fatalf("Release() error = %v, want *FetchResponseError", err)
			}
		})
	}
}

func TestDebianReleases_OfflineIsAnError(t *testing.T) {
	client, url := closedURL(t)
	if _, err := (DebianReleases{Client: client, URL: url}).Release(context.Background(), "stable"); err == nil {
		t.Fatal("Release() offline error = nil")
	}
}

func TestUbuntuReleases_LatestLTSAcrossPages(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"entries":[
			{"version":"26.10","status":"Pre-release Freeze"},
			{"version":"25.10","status":"Current Stable Release"},
			{"version":"24.04","status":"Supported"}],
			"next_collection_link":"` + srv.URL + `/page2"}`
		if r.URL.Path == "/page2" {
			body = `{"entries":[{"version":"26.04","status":"Supported"},{"version":"28.04","status":"Future"}]}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := UbuntuReleases{Client: srv.Client(), URL: srv.URL}.LatestLTS(context.Background())
	if err != nil {
		t.Fatalf("LatestLTS() error = %v", err)
	}
	// 25.10 is released but not LTS; 26.10 and 28.04 are not released.
	if got != "26.04" {
		t.Errorf("LatestLTS() = %q, want 26.04", got)
	}
}

func TestUbuntuReleases_RefusesABadAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
	}{
		"server error":    {http.StatusServiceUnavailable, ""},
		"not json":        {http.StatusOK, "<html>"},
		"no released lts": {http.StatusOK, `{"entries":[{"version":"25.10","status":"Current Stable Release"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			client, url := serveBody(t, tt.status, tt.body)
			_, err := UbuntuReleases{Client: client, URL: url}.LatestLTS(context.Background())
			var ferr *FetchResponseError
			if !errors.As(err, &ferr) {
				t.Fatalf("LatestLTS() error = %v, want *FetchResponseError", err)
			}
		})
	}
}

func TestUbuntuReleases_OfflineIsAnError(t *testing.T) {
	client, url := closedURL(t)
	if _, err := (UbuntuReleases{Client: client, URL: url}).LatestLTS(context.Background()); err == nil {
		t.Fatal("LatestLTS() offline error = nil")
	}
}

func TestGitTags_Tags(t *testing.T) {
	var args []string
	g := GitTags{Run: runReturning(sha701+"\trefs/tags/v7.0.1\n", nil, &args), BaseURL: "https://github.com/"}

	got, err := g.Tags(context.Background(), "actions/checkout")
	if err != nil {
		t.Fatalf("Tags() error = %v", err)
	}
	if want := []Tag{{Name: "v7.0.1", Commit: sha701}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Tags() = %+v, want %+v", got, want)
	}
	if want := "git ls-remote --tags https://github.com/actions/checkout"; strings.Join(args, " ") != want {
		t.Errorf("ran %q, want %q", strings.Join(args, " "), want)
	}
}

func TestGitTags_CommandFailureIsReturned(t *testing.T) {
	var args []string
	cause := &CommandError{Command: "git ls-remote", Stderr: "fatal: unable to access", Cause: errors.New("exit status 128")}
	_, err := GitTags{Run: runReturning("", cause, &args)}.Tags(context.Background(), "actions/checkout")
	if !errors.Is(err, cause) {
		t.Fatalf("Tags() error = %v, want the command's error", err)
	}
}
