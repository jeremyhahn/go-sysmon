// Package gitea is the small part of the Gitea REST API that go-sysmon's own
// automation needs: find an open issue, resolve a label and file an issue
// that verifiably carries it.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// pageSize is the page size requested when listing. Gitea caps pages at its
// MAX_RESPONSE_ITEMS, 50 by default, so a larger request would be cut short
// and read as the last page.
const pageSize = 50

// maxErrorBody bounds how much of an error response is kept for the message.
const maxErrorBody = 512

// Client calls one repository's API as the owner of Token.
type Client struct {
	// BaseURL is the API root, "https://gitea.example/api/v1".
	BaseURL string
	// Owner and Repo name the repository.
	Owner, Repo string
	// Token is an access token. It is sent as an Authorization header and
	// never appears in an error.
	Token string
	HTTP  *http.Client
}

// Label is an issue label.
type Label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Issue is the part of an issue this package reads.
type Issue struct {
	Number int64   `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Labels []Label `json:"labels"`
	// PullRequest is set when the "issue" is a pull request, which the
	// issues endpoint also lists.
	PullRequest *struct{} `json:"pull_request"`
}

// HasLabel reports whether the issue carries the label with id.
func (i Issue) HasLabel(id int64) bool {
	for _, l := range i.Labels {
		if l.ID == id {
			return true
		}
	}
	return false
}

// FindOpen returns the number of the first open issue whose body contains
// marker, or 0 when none does. Pull requests are not issues and never match.
func (c *Client) FindOpen(ctx context.Context, marker string) (int64, error) {
	for page := 1; ; page++ {
		q := url.Values{
			"state": {"open"},
			"type":  {"issues"},
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(pageSize)},
		}
		var issues []Issue
		if err := c.do(ctx, http.MethodGet, c.repoPath("issues")+"?"+q.Encode(), nil, &issues); err != nil {
			return 0, err
		}
		for _, is := range issues {
			if is.PullRequest == nil && strings.Contains(is.Body, marker) {
				return is.Number, nil
			}
		}
		if len(issues) < pageSize {
			return 0, nil
		}
	}
}

// LabelID resolves a repository label name to its id. Labels are ids on the
// wire; a name the repository does not have is a *LabelNotFoundError.
func (c *Client) LabelID(ctx context.Context, name string) (int64, error) {
	for page := 1; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
		var labels []Label
		if err := c.do(ctx, http.MethodGet, c.repoPath("labels")+"?"+q.Encode(), nil, &labels); err != nil {
			return 0, err
		}
		for _, l := range labels {
			if l.Name == name {
				return l.ID, nil
			}
		}
		if len(labels) < pageSize {
			return 0, &LabelNotFoundError{Label: name}
		}
	}
}

// Create files an issue carrying label and returns its number.
//
// Gitea has answered a label request with success without applying the
// label, and an issue without its label is invisible to every sweeper that
// queries by label. So the new issue is read back; a missing label is applied
// once more and read back again, and if it is still missing the result is a
// *LabelNotAppliedError naming the issue that was filed.
func (c *Client) Create(ctx context.Context, title, body, label string) (int64, error) {
	id, err := c.LabelID(ctx, label)
	if err != nil {
		return 0, err
	}
	var created Issue
	req := map[string]any{"title": title, "body": body, "labels": []int64{id}}
	if err := c.do(ctx, http.MethodPost, c.repoPath("issues"), req, &created); err != nil {
		return 0, err
	}
	n := created.Number
	ok, err := c.labelled(ctx, n, id)
	if err != nil || ok {
		return n, err
	}
	var applied []Label
	if err := c.do(ctx, http.MethodPost, c.repoPath("issues", strconv.FormatInt(n, 10), "labels"), map[string]any{"labels": []int64{id}}, &applied); err != nil {
		return n, err
	}
	ok, err = c.labelled(ctx, n, id)
	if err != nil {
		return n, err
	}
	if !ok {
		return n, &LabelNotAppliedError{Issue: n, Label: label}
	}
	return n, nil
}

// labelled re-reads issue n and reports whether it carries label id.
func (c *Client) labelled(ctx context.Context, n, id int64) (bool, error) {
	var is Issue
	if err := c.do(ctx, http.MethodGet, c.repoPath("issues", strconv.FormatInt(n, 10)), nil, &is); err != nil {
		return false, err
	}
	return is.HasLabel(id), nil
}

// repoPath joins path segments under the repository's API path, escaping each.
func (c *Client) repoPath(segments ...string) string {
	p := "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo)
	for _, s := range segments {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// do sends one request with an optional JSON body and decodes a 2xx JSON
// answer into out. Any other status is an *APIError.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return &RequestError{Method: method, Path: path, Cause: err}
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if err != nil {
		return &RequestError{Method: method, Path: path, Cause: err}
	}
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &RequestError{Method: method, Path: path, Cause: err}
	}
	data, readErr := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil && readErr == nil {
		readErr = err
	}
	if readErr != nil {
		return &RequestError{Method: method, Path: path, Cause: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(data) > maxErrorBody {
			data = data[:maxErrorBody]
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &RequestError{Method: method, Path: path, Cause: err}
	}
	return nil
}
