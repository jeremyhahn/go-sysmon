package gitea

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

// fakeGitea is an in-memory repository behind the few endpoints the client
// uses, recording every request.
type fakeGitea struct {
	mu       sync.Mutex
	issues   []Issue
	labels   []Label
	requests []string
	// dropLabels is how many label applications succeed without applying
	// the label, as Gitea has been seen to do.
	dropLabels int
	// failPath answers 500 for requests whose path has this suffix.
	failPath string
}

func (f *fakeGitea) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "token "+testToken {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if f.failPath != "" && strings.HasSuffix(r.URL.Path, f.failPath) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		f.route(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/api/v1/", Owner: "jeremyhahn", Repo: "go-sysmon", Token: testToken, HTTP: srv.Client()}
}

func (f *fakeGitea) route(t *testing.T, w http.ResponseWriter, r *http.Request) {
	const repo = "/api/v1/repos/jeremyhahn/go-sysmon"
	path := strings.TrimPrefix(r.URL.Path, repo)
	switch {
	case r.Method == http.MethodGet && path == "/issues":
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("issues listed with state=%q, want open", r.URL.Query().Get("state"))
		}
		writeJSON(t, w, page(f.issues, r))
	case r.Method == http.MethodGet && path == "/labels":
		writeJSON(t, w, page(f.labels, r))
	case r.Method == http.MethodPost && path == "/issues":
		var req struct {
			Title  string  `json:"title"`
			Body   string  `json:"body"`
			Labels []int64 `json:"labels"`
		}
		decode(t, r.Body, &req)
		is := Issue{Number: int64(len(f.issues) + 100), Title: req.Title, Body: req.Body}
		is.Labels = f.applyLabels(is.Labels, req.Labels)
		f.issues = append(f.issues, is)
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, is)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/issues/"):
		is := f.issue(path)
		if is == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, is)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
		is := f.issue(strings.TrimSuffix(path, "/labels"))
		var req struct {
			Labels []int64 `json:"labels"`
		}
		decode(t, r.Body, &req)
		is.Labels = f.applyLabels(is.Labels, req.Labels)
		writeJSON(t, w, is.Labels)
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// applyLabels adds ids to have, unless the fake is set to drop them.
func (f *fakeGitea) applyLabels(have []Label, ids []int64) []Label {
	if f.dropLabels > 0 {
		f.dropLabels--
		return have
	}
	for _, id := range ids {
		for _, l := range f.labels {
			if l.ID == id {
				have = append(have, l)
			}
		}
	}
	return have
}

func (f *fakeGitea) issue(path string) *Issue {
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

// page returns the page of items a Gitea list request asks for.
func page[T any](items []T, r *http.Request) []T {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		p = 1
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 {
		limit = 30
	}
	start := min((p-1)*limit, len(items))
	end := min(start+limit, len(items))
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

func TestFindOpen_FindsTheMarkerOnALaterPage(t *testing.T) {
	f := &fakeGitea{issues: manyIssues(pageSize + 3)}
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
	f := &fakeGitea{issues: manyIssues(pageSize)}
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
	f := &fakeGitea{failPath: "/issues"}
	c := f.serve(t)

	_, err := c.FindOpen(context.Background(), marker)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("FindOpen() error = %v, want an *APIError with HTTP 500", err)
	}
}

func TestLabelID(t *testing.T) {
	labels := make([]Label, pageSize+2)
	for i := range labels {
		labels[i] = Label{ID: int64(i + 1), Name: "label-" + strconv.Itoa(i)}
	}
	labels[pageSize+1].Name = "task:ready"
	c := (&fakeGitea{labels: labels}).serve(t)

	got, err := c.LabelID(context.Background(), "task:ready")
	if err != nil {
		t.Fatalf("LabelID() error = %v", err)
	}
	if want := int64(pageSize + 2); got != want {
		t.Errorf("LabelID() = %d, want %d", got, want)
	}
}

func TestLabelID_MissingLabelIsTyped(t *testing.T) {
	c := (&fakeGitea{labels: []Label{{ID: 1, Name: "task:blocked"}}}).serve(t)

	_, err := c.LabelID(context.Background(), "task:ready")
	var nf *LabelNotFoundError
	if !errors.As(err, &nf) || nf.Label != "task:ready" {
		t.Fatalf("LabelID() error = %v, want *LabelNotFoundError for task:ready", err)
	}
}

func TestCreate_FilesAnIssueCarryingItsLabel(t *testing.T) {
	f := &fakeGitea{labels: []Label{{ID: 7, Name: "task:ready"}}}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body "+marker, "task:ready")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if len(f.issues) != 1 || f.issues[0].Number != n || !f.issues[0].HasLabel(7) {
		t.Fatalf("issues = %+v, want issue #%d labelled 7", f.issues, n)
	}
	// The label is verified by reading the issue back, not taken on trust.
	if last := f.requests[len(f.requests)-1]; last != "GET /api/v1/repos/jeremyhahn/go-sysmon/issues/"+strconv.FormatInt(n, 10) {
		t.Errorf("last request = %q, want the issue re-read", last)
	}
}

func TestCreate_ReappliesALabelThatDidNotStick(t *testing.T) {
	f := &fakeGitea{labels: []Label{{ID: 7, Name: "task:ready"}}, dropLabels: 1}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body", "task:ready")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !f.issues[0].HasLabel(7) {
		t.Errorf("issue #%d labels = %+v, want task:ready re-applied", n, f.issues[0].Labels)
	}
}

func TestCreate_LabelThatNeverSticksIsTyped(t *testing.T) {
	f := &fakeGitea{labels: []Label{{ID: 7, Name: "task:ready"}}, dropLabels: 2}
	c := f.serve(t)

	n, err := c.Create(context.Background(), "title", "body", "task:ready")
	var na *LabelNotAppliedError
	if !errors.As(err, &na) || na.Issue != n || n == 0 {
		t.Fatalf("Create() = %d, %v; want the filed issue and a *LabelNotAppliedError", n, err)
	}
}

func TestCreate_Errors(t *testing.T) {
	tests := map[string]*fakeGitea{
		"no such label":       {labels: []Label{{ID: 1, Name: "bug"}}},
		"create refused":      {labels: []Label{{ID: 7, Name: "task:ready"}}, failPath: "/issues"},
		"relabel refused":     {labels: []Label{{ID: 7, Name: "task:ready"}}, dropLabels: 1, failPath: "/100/labels"},
		"issue not readable":  {labels: []Label{{ID: 7, Name: "task:ready"}}, failPath: "/100"},
		"labels not readable": {failPath: "/labels"},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			c := f.serve(t)
			if _, err := c.Create(context.Background(), "title", "body", "task:ready"); err == nil {
				t.Fatal("Create() error = nil")
			}
		})
	}
}

func TestClient_WrongTokenIsAnAPIErrorWithoutTheToken(t *testing.T) {
	c := (&fakeGitea{}).serve(t)
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

func TestClient_UnreachableIsARequestError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	c := &Client{BaseURL: closed.URL, Owner: "o", Repo: "r", Token: testToken, HTTP: closed.Client()}
	closed.Close()

	_, err := c.FindOpen(context.Background(), marker)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("FindOpen() error = %v, want *RequestError", err)
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

	_, err := c.LabelID(context.Background(), "task:ready")
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("LabelID() error = %v, want *RequestError", err)
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

func TestErrorMessages(t *testing.T) {
	tests := map[string]error{
		"gitea: GET /x: HTTP 404: nope":                                           &APIError{Method: "GET", Path: "/x", Status: 404, Body: "nope"},
		"gitea: GET /x: HTTP 502":                                                 &APIError{Method: "GET", Path: "/x", Status: 502},
		`gitea: the repository has no label "a"`:                                  &LabelNotFoundError{Label: "a"},
		`gitea: issue #3 was filed but label "a" did not stick; label it by hand`: &LabelNotAppliedError{Issue: 3, Label: "a"},
	}
	for want, err := range tests {
		if err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
	}
}
