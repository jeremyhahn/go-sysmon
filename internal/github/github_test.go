package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testToken = "s3cret-token"

// fakeGitHub is an in-memory repository behind the few endpoints the client
// uses, recording every request.
type fakeGitHub struct {
	mu       sync.Mutex
	issues   []Issue
	labels   []Label
	requests []string
	// dropLabels is how many label applications succeed without applying
	// the label, as GitHub does for a caller it will not let label.
	dropLabels int
	// failPath answers 500 for requests whose path has this suffix.
	failPath string
}

func (f *fakeGitHub) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
			t.Errorf("X-GitHub-Api-Version = %q, want %q", got, apiVersion)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", got)
		}
		if f.failPath != "" && strings.HasSuffix(r.URL.Path, f.failPath) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		f.route(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/", Owner: "jeremyhahn", Repo: "go-sysmon", Token: testToken, HTTP: srv.Client()}
}

func (f *fakeGitHub) route(t *testing.T, w http.ResponseWriter, r *http.Request) {
	const repo = "/repos/jeremyhahn/go-sysmon"
	path := strings.TrimPrefix(r.URL.Path, repo)
	switch {
	case r.Method == http.MethodGet && path == "/issues":
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("issues listed with state=%q, want open", r.URL.Query().Get("state"))
		}
		writeJSON(t, w, page(f.issues, r))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/labels/"):
		name := strings.TrimPrefix(path, "/labels/")
		if !hasLabel(f.labels, name) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeJSON(t, w, Label{Name: name})
	case r.Method == http.MethodPost && path == "/issues":
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		decode(t, r.Body, &req)
		is := Issue{Number: int64(len(f.issues) + 100), Title: req.Title, Body: req.Body}
		is.Labels = f.applyLabels(is.Labels, req.Labels)
		f.issues = append(f.issues, is)
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, is)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
		is := f.issue(strings.TrimSuffix(path, "/labels"))
		if is == nil {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Labels []string `json:"labels"`
		}
		decode(t, r.Body, &req)
		is.Labels = f.applyLabels(is.Labels, req.Labels)
		writeJSON(t, w, is.Labels)
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// applyLabels adds the named labels the repository has to have, unless the
// fake is set to drop them.
func (f *fakeGitHub) applyLabels(have []Label, names []string) []Label {
	if f.dropLabels > 0 {
		f.dropLabels--
		return have
	}
	for _, name := range names {
		if hasLabel(f.labels, name) {
			have = append(have, Label{Name: name})
		}
	}
	return have
}

func (f *fakeGitHub) issue(path string) *Issue {
	n, err := strconv.ParseInt(strings.TrimPrefix(path, "/issues/"), 10, 64)
	if err != nil {
		return nil
	}
	for i := range f.issues {
		if f.issues[i].Number == n {
			return &f.issues[i]
		}
	}
	return nil
}

// page returns the page of items a GitHub list request asks for.
func page[T any](items []T, r *http.Request) []T {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		p = 1
	}
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 30
	}
	start := min((p-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	return append([]T{}, items[start:end]...)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func decode(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Errorf("decode: %v", err)
	}
}

// manyIssues returns n open issues numbered from 1, none carrying marker.
func manyIssues(n int) []Issue {
	issues := make([]Issue, n)
	for i := range issues {
		issues[i] = Issue{Number: int64(i + 1), Body: "unrelated"}
	}
	return issues
}

const marker = "<!-- go-sysmon:check-latest -->"

var readyLabel = []Label{{Name: "task:ready"}}

func TestFindOpen_FindsTheMarkerOnALaterPage(t *testing.T) {
	f := &fakeGitHub{issues: manyIssues(pageSize + 3)}
	f.issues[pageSize+1].Body = "intro\n" + marker + "\nrest"
	c := f.serve(t)

	got, err := c.FindOpen(context.Background(), marker)
	if err != nil {
		t.Fatalf("FindOpen() error = %v", err)
	}
	if want := int64(pageSize + 2); got != want {
		t.Errorf("FindOpen() = %d, want %d", got, want)
	}
}

func TestFindOpen_NoneOpen(t *testing.T) {
	f := &fakeGitHub{issues: manyIssues(pageSize)}
	// A pull request quoting the marker is not the issue.
	f.issues[3].Body = marker
	f.issues[3].PullRequest = &struct{}{}
	c := f.serve(t)

	got, err := c.FindOpen(context.Background(), marker)
	if err != nil || got != 0 {
		t.Fatalf("FindOpen() = %d, %v; want 0, nil", got, err)
	}
	// A full first page means there may be more: the client must ask.
	if len(f.requests) != 2 {
		t.Errorf("requests = %q, want two pages read", f.requests)
	}
}

func TestFindOpen_APIErrorIsReturned(t *testing.T) {
	f := &fakeGitHub{failPath: "/issues"}
	c := f.serve(t)

	_, err := c.FindOpen(context.Background(), marker)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("FindOpen() error = %v, want an *APIError with HTTP 500", err)
	}
}

func TestIssue_HasLabel(t *testing.T) {
	is := Issue{Labels: []Label{{Name: "bug"}, {Name: "task:ready"}}}
	if !is.HasLabel("task:ready") {
		t.Error("HasLabel(task:ready) = false, want true")
	}
	if is.HasLabel("task:blocked") {
		t.Error("HasLabel(task:blocked) = true, want false")
	}
	if (Issue{}).HasLabel("task:ready") {
		t.Error("an unlabelled issue HasLabel(task:ready) = true")
	}
}

func TestRequireLabel(t *testing.T) {
	c := (&fakeGitHub{labels: readyLabel}).serve(t)

	if err := c.requireLabel(context.Background(), "task:ready"); err != nil {
		t.Fatalf("requireLabel() error = %v", err)
	}
}

func TestRequireLabel_MissingLabelIsTyped(t *testing.T) {
	c := (&fakeGitHub{labels: []Label{{Name: "task:blocked"}}}).serve(t)

	err := c.requireLabel(context.Background(), "task:ready")
	var nf *LabelNotFoundError
	if !errors.As(err, &nf) || nf.Label != "task:ready" {
		t.Fatalf("requireLabel() error = %v, want *LabelNotFoundError for task:ready", err)
	}
}

func TestRequireLabel_OtherFailureIsNotANotFound(t *testing.T) {
	c := (&fakeGitHub{labels: readyLabel, failPath: "/labels/task:ready"}).serve(t)

	err := c.requireLabel(context.Background(), "task:ready")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("requireLabel() error = %v, want an *APIError with HTTP 500", err)
	}
}

func TestCreate_FilesAnIssueCarryingItsLabel(t *testing.T) {
	f := &fakeGitHub{labels: readyLabel}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body "+marker, "task:ready")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if len(f.issues) != 1 || f.issues[0].Number != n || !f.issues[0].HasLabel("task:ready") {
		t.Fatalf("issues = %+v, want issue #%d labelled task:ready", f.issues, n)
	}
	want := []string{
		"GET /repos/jeremyhahn/go-sysmon/labels/task:ready",
		"POST /repos/jeremyhahn/go-sysmon/issues",
	}
	if strings.Join(f.requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests = %q, want %q", f.requests, want)
	}
}

func TestCreate_ReappliesALabelThatDidNotStick(t *testing.T) {
	f := &fakeGitHub{labels: readyLabel, dropLabels: 1}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body", "task:ready")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !f.issues[0].HasLabel("task:ready") {
		t.Errorf("issue #%d labels = %+v, want task:ready re-applied", n, f.issues[0].Labels)
	}
	if last := f.requests[len(f.requests)-1]; last != "POST /repos/jeremyhahn/go-sysmon/issues/"+strconv.FormatInt(n, 10)+"/labels" {
		t.Errorf("last request = %q, want the label applied again", last)
	}
}

func TestCreate_LabelThatNeverSticksIsTyped(t *testing.T) {
	f := &fakeGitHub{labels: readyLabel, dropLabels: 2}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body", "task:ready")
	var na *LabelNotAppliedError
	if !errors.As(err, &na) || na.Issue != n || n == 0 {
		t.Fatalf("Create() = %d, %v; want the filed issue and a *LabelNotAppliedError", n, err)
	}
}

func TestCreate_Errors(t *testing.T) {
	tests := map[string]struct {
		fake *fakeGitHub
		// filed is whether an issue exists despite the error.
		filed bool
	}{
		"no such label":       {fake: &fakeGitHub{labels: []Label{{Name: "bug"}}}},
		"labels not readable": {fake: &fakeGitHub{labels: readyLabel, failPath: "/labels/task:ready"}},
		"create refused":      {fake: &fakeGitHub{labels: readyLabel, failPath: "/issues"}},
		"relabel refused":     {fake: &fakeGitHub{labels: readyLabel, dropLabels: 1, failPath: "/100/labels"}, filed: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := tt.fake.serve(t)
			n, err := c.Create(context.Background(), "title", "body", "task:ready")
			if err == nil {
				t.Fatal("Create() error = nil")
			}
			if filed := n != 0; filed != tt.filed {
				t.Errorf("Create() = #%d; want an issue number %v", n, tt.filed)
			}
		})
	}
}

func TestClient_WrongTokenIsAnAPIErrorWithoutTheToken(t *testing.T) {
	c := (&fakeGitHub{}).serve(t)
	c.Token = "wrong-" + testToken

	_, err := c.FindOpen(context.Background(), marker)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("FindOpen() error = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error %q leaks the token", err)
	}
}

func TestClient_LongErrorBodyIsCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 4*maxErrorBody), http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Owner: "o", Repo: "r", Token: testToken, HTTP: srv.Client()}

	_, err := c.FindOpen(context.Background(), marker)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || len(apiErr.Body) != maxErrorBody {
		t.Fatalf("FindOpen() error = %v, want an *APIError with a %d-byte body", err, maxErrorBody)
	}
}

func TestClient_UnreachableIsARequestError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	c := &Client{BaseURL: closed.URL, Owner: "o", Repo: "r", Token: testToken, HTTP: closed.Client()}
	closed.Close()

	_, err := c.FindOpen(context.Background(), marker)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) || errors.Unwrap(err) == nil {
		t.Fatalf("FindOpen() error = %v, want a *RequestError wrapping its cause", err)
	}
}

func TestClient_UndecodableAnswerIsARequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("<html>not json</html>")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Owner: "o", Repo: "r", Token: testToken, HTTP: srv.Client()}

	_, err := c.FindOpen(context.Background(), marker)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("FindOpen() error = %v, want *RequestError", err)
	}
}

func TestClient_MalformedBaseURLIsARequestError(t *testing.T) {
	c := &Client{BaseURL: "://bad", Owner: "o", Repo: "r", Token: testToken, HTTP: http.DefaultClient}

	_, err := c.FindOpen(context.Background(), marker)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("FindOpen() error = %v, want *RequestError", err)
	}
}

func TestClient_UnencodableBodyIsARequestError(t *testing.T) {
	c := &Client{BaseURL: "http://unused.invalid", Owner: "o", Repo: "r", Token: testToken, HTTP: http.DefaultClient}

	err := c.do(context.Background(), http.MethodPost, "/x", map[string]any{"bad": make(chan int)}, nil)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("do() error = %v, want *RequestError", err)
	}
}

func TestRepoPath_EscapesEachSegment(t *testing.T) {
	c := &Client{Owner: "o w", Repo: "r/x"}
	if got, want := c.repoPath("labels", "a b/c"), "/repos/o%20w/r%2Fx/labels/a%20b%2Fc"; got != want {
		t.Errorf("repoPath() = %q, want %q", got, want)
	}
	if got, want := c.repoPath(), "/repos/o%20w/r%2Fx"; got != want {
		t.Errorf("repoPath() = %q, want %q", got, want)
	}
}

func TestErrorMessages(t *testing.T) {
	tests := map[string]error{
		"github: GET /x: HTTP 404: nope":          &APIError{Method: "GET", Path: "/x", Status: 404, Body: "nope"},
		"github: GET /x: HTTP 502":                &APIError{Method: "GET", Path: "/x", Status: 502},
		"github: GET /x: boom":                    &RequestError{Method: "GET", Path: "/x", Cause: errors.New("boom")},
		`github: the repository has no label "a"`: &LabelNotFoundError{Label: "a"},
		`github: issue #3 was filed but label "a" did not stick; label it by hand`: &LabelNotAppliedError{Issue: 3, Label: "a"},
	}
	for want, err := range tests {
		if err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
	}
}
