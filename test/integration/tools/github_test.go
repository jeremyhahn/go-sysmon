//go:build integration

package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// githubToken is the only token the GitHub stand-in accepts.
const githubToken = "secret"

// githubIssue is an issue as the stand-in holds it.
type githubIssue struct {
	Number      int64         `json:"number"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	Labels      []githubLabel `json:"labels"`
	PullRequest *struct{}     `json:"pull_request,omitempty"`
}

type githubLabel struct {
	Name string `json:"name"`
}

// githubStandIn serves the part of the GitHub REST API checklatest's filer
// uses, for the repository o/r under a GitHub Enterprise-style /api/v3 root,
// paging lists by per_page as GitHub does.
type githubStandIn struct {
	srv *httptest.Server

	mu     sync.Mutex
	issues []githubIssue
	labels []string
	// dropLabels is how many label applications to acknowledge without
	// applying: GitHub drops a label it will not let the caller set, and
	// still answers success.
	dropLabels int
	// failList, failLabel, failCreate and failApply answer that request with
	// the status when set.
	failList, failLabel, failCreate, failApply int
	// garbled answers the issue list with a body that is not JSON.
	garbled bool
	// verbose makes a failure's body longer than any message should carry.
	verbose bool
	// requests records each request as "METHOD path".
	requests []string
}

// pageSize is the largest page GitHub serves, which the client asks for.
const pageSize = 100

func newGitHub(t *testing.T) *githubStandIn {
	t.Helper()
	g := &githubStandIn{labels: []string{"area:tools", "task:ready"}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/o/r/issues", g.listIssues)
	mux.HandleFunc("GET /api/v3/repos/o/r/labels/{name}", g.getLabel)
	mux.HandleFunc("POST /api/v3/repos/o/r/issues", g.create)
	mux.HandleFunc("POST /api/v3/repos/o/r/issues/{n}/labels", g.label)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+githubToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Api-Version") == "" {
			http.Error(w, `{"message":"no API version requested"}`, http.StatusBadRequest)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// api is the API root checklatest is given.
func (g *githubStandIn) api() string { return g.srv.URL + "/api/v3" }

// posted reports how many requests created or labelled an issue.
func (g *githubStandIn) posted() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.HasPrefix(r, "POST ") {
			n++
		}
	}
	return n
}

// issue returns the issue numbered n, or false.
func (g *githubStandIn) issue(n int64) (githubIssue, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, is := range g.issues {
		if is.Number == n {
			return is, true
		}
	}
	return githubIssue{}, false
}

// page returns the page of all a list request asks for, by its page and
// per_page parameters. GitHub serves 30 by default and at most 100.
func page[T any](r *http.Request, all []T) []T {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		p = 1
	}
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 30
	}
	perPage = min(perPage, pageSize)
	start := (p - 1) * perPage
	if start >= len(all) {
		return []T{}
	}
	return all[start:min(start+perPage, len(all))]
}

// fail answers with status and a GitHub-style error body.
func (g *githubStandIn) fail(w http.ResponseWriter, status int) {
	msg := "Server Error"
	if g.verbose {
		msg = strings.Repeat("stack frame ", 200)
	}
	http.Error(w, `{"message":"`+msg+`"}`, status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (g *githubStandIn) listIssues(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failList != 0 {
		g.fail(w, g.failList)
		return
	}
	if g.garbled {
		if _, err := w.Write([]byte("<html>unicorn</html>")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, page(r, g.issues))
}

func (g *githubStandIn) getLabel(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failLabel != 0 {
		g.fail(w, g.failLabel)
		return
	}
	name := r.PathValue("name")
	if !slices.Contains(g.labels, name) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, githubLabel{Name: name})
}

// applyLabels adds the named labels the repository has to issue i, unless a
// drop is pending.
func (g *githubStandIn) applyLabels(i int, names []string) {
	if g.dropLabels > 0 {
		g.dropLabels--
		return
	}
	for _, name := range names {
		if slices.Contains(g.labels, name) {
			g.issues[i].Labels = append(g.issues[i].Labels, githubLabel{Name: name})
		}
	}
}

func (g *githubStandIn) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failCreate != 0 {
		g.fail(w, g.failCreate)
		return
	}
	n := int64(len(g.issues) + 1)
	g.issues = append(g.issues, githubIssue{Number: n, Title: req.Title, Body: req.Body})
	g.applyLabels(len(g.issues)-1, req.Labels)
	writeJSON(w, http.StatusCreated, g.issues[len(g.issues)-1])
}

// index finds the issue the path names.
func (g *githubStandIn) index(r *http.Request) (int, bool) {
	n, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil {
		return 0, false
	}
	for i, is := range g.issues {
		if is.Number == n {
			return i, true
		}
	}
	return 0, false
}

func (g *githubStandIn) label(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.index(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if g.failApply != 0 {
		g.fail(w, g.failApply)
		return
	}
	g.applyLabels(i, req.Labels)
	writeJSON(w, http.StatusOK, g.issues[i].Labels)
}
