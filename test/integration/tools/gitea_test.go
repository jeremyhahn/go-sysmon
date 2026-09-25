//go:build integration

package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// giteaToken is the only token the Gitea stand-in accepts.
const giteaToken = "secret"

// giteaIssue is an issue as the stand-in holds it.
type giteaIssue struct {
	Number      int64        `json:"number"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	Labels      []giteaLabel `json:"labels"`
	PullRequest *struct{}    `json:"pull_request,omitempty"`
}

type giteaLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// giteaStandIn serves the part of the Gitea API checklatest's filer uses, for
// the repository o/r, paging lists 50 at a time as Gitea does.
type giteaStandIn struct {
	srv *httptest.Server

	mu     sync.Mutex
	issues []giteaIssue
	labels []giteaLabel
	// dropLabels is how many label applications to acknowledge without
	// applying: Gitea has answered success and not applied the label.
	dropLabels int
	// failList, failLabels, failCreate, failGet and failApply answer that
	// request with the status when set.
	failList, failLabels, failCreate, failGet, failApply int
	// garbled answers the issue list with a body that is not JSON.
	garbled bool
	// verbose makes a failure's body longer than any message should carry.
	verbose bool
	// requests records each request as "METHOD path".
	requests []string
}

// pageSize is Gitea's default page cap, which the client pages by.
const pageSize = 50

func newGitea(t *testing.T) *giteaStandIn {
	t.Helper()
	g := &giteaStandIn{}
	// Enough labels that task:ready is on the second page.
	for i := 1; i <= pageSize; i++ {
		g.labels = append(g.labels, giteaLabel{ID: int64(i), Name: "area:" + strconv.Itoa(i)})
	}
	g.labels = append(g.labels, giteaLabel{ID: 77, Name: "task:ready"})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/r/issues", g.listIssues)
	mux.HandleFunc("GET /api/v1/repos/o/r/labels", g.listLabels)
	mux.HandleFunc("POST /api/v1/repos/o/r/issues", g.create)
	mux.HandleFunc("GET /api/v1/repos/o/r/issues/{n}", g.get)
	mux.HandleFunc("POST /api/v1/repos/o/r/issues/{n}/labels", g.label)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		if r.Header.Get("Authorization") != "token "+giteaToken {
			http.Error(w, `{"message":"token is required"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// api is the API root checklatest is given.
func (g *giteaStandIn) api() string { return g.srv.URL + "/api/v1" }

// posted reports how many requests created or labelled an issue.
func (g *giteaStandIn) posted() int {
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
func (g *giteaStandIn) issue(n int64) (giteaIssue, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, is := range g.issues {
		if is.Number == n {
			return is, true
		}
	}
	return giteaIssue{}, false
}

func page[T any](r *http.Request, all []T) []T {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		p = 1
	}
	start := (p - 1) * pageSize
	if start >= len(all) {
		return []T{}
	}
	return all[start:min(start+pageSize, len(all))]
}

// fail answers with status and a Gitea-style error body.
func (g *giteaStandIn) fail(w http.ResponseWriter, status int) {
	msg := "database is locked"
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

func (g *giteaStandIn) listIssues(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failList != 0 {
		g.fail(w, g.failList)
		return
	}
	if g.garbled {
		if _, err := w.Write([]byte("<html>maintenance</html>")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, page(r, g.issues))
}

func (g *giteaStandIn) listLabels(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failLabels != 0 {
		g.fail(w, g.failLabels)
		return
	}
	writeJSON(w, http.StatusOK, page(r, g.labels))
}

func (g *giteaStandIn) labelByID(id int64) (giteaLabel, bool) {
	for _, l := range g.labels {
		if l.ID == id {
			return l, true
		}
	}
	return giteaLabel{}, false
}

// applyLabels adds ids to issue i unless a drop is pending.
func (g *giteaStandIn) applyLabels(i int, ids []int64) {
	if g.dropLabels > 0 {
		g.dropLabels--
		return
	}
	for _, id := range ids {
		if l, ok := g.labelByID(id); ok {
			g.issues[i].Labels = append(g.issues[i].Labels, l)
		}
	}
}

func (g *giteaStandIn) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title  string  `json:"title"`
		Body   string  `json:"body"`
		Labels []int64 `json:"labels"`
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
	g.issues = append(g.issues, giteaIssue{Number: n, Title: req.Title, Body: req.Body})
	g.applyLabels(len(g.issues)-1, req.Labels)
	writeJSON(w, http.StatusCreated, g.issues[len(g.issues)-1])
}

// index finds the issue the path names.
func (g *giteaStandIn) index(r *http.Request) (int, bool) {
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

func (g *giteaStandIn) get(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.index(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if g.failGet != 0 {
		g.fail(w, g.failGet)
		return
	}
	writeJSON(w, http.StatusOK, g.issues[i])
}

func (g *giteaStandIn) label(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels []int64 `json:"labels"`
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
