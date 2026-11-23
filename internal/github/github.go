// Package github is the small part of the GitHub REST API that go-sysmon's own
// automation needs: find an open issue and file an issue that verifiably
// carries its label.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DefaultBaseURL is the API root of github.com.
const DefaultBaseURL = "https://api.github.com"

// apiVersion is the REST API version every request asks for, so a change to
// GitHub's default cannot change what the client reads.
const apiVersion = "2022-11-28"

// pageSize is the page size requested when listing: the most GitHub serves.
const pageSize = 100

// maxErrorBody bounds how much of an error response is kept for the message.
const maxErrorBody = 512

// Client calls one repository's API as the owner of Token.
type Client struct {
	// BaseURL is the API root, DefaultBaseURL on github.com.
	BaseURL string
	// Owner and Repo name the repository.
	Owner, Repo string
	// Token is an access token. It is sent as an Authorization header and
	// never appears in an error.
	Token string
	HTTP  *http.Client
}

// Label is an issue label. GitHub names labels on the wire.
type Label struct {
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

// HasLabel reports whether the issue carries the label called name.
func (i Issue) HasLabel(name string) bool {
	return hasLabel(i.Labels, name)
}

// hasLabel reports whether labels holds the label called name.
func hasLabel(labels []Label, name string) bool {
	for _, l := range labels {
		if l.Name == name {
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
			"state":    {"open"},
			"page":     {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(pageSize)},
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

// Create files an issue carrying label and returns its number.
//
// The label must already exist: a name the repository does not have is a
// *LabelNotFoundError and nothing is filed. GitHub drops a label it will not
// apply without failing the request, and an issue without its label is
// invisible to everything that queries by label. So the label is checked on
// the issue GitHub returns; a missing one is applied once more and checked
// again, and if it is still missing the result is a *LabelNotAppliedError
// naming the issue that was filed.
func (c *Client) Create(ctx context.Context, title, body, label string) (int64, error) {
	if err := c.requireLabel(ctx, label); err != nil {
		return 0, err
	}
	var created Issue
	req := map[string]any{"title": title, "body": body, "labels": []string{label}}
	if err := c.do(ctx, http.MethodPost, c.repoPath("issues"), req, &created); err != nil {
		return 0, err
	}
	n := created.Number
	if created.HasLabel(label) {
		return n, nil
	}
	var applied []Label
	if err := c.do(ctx, http.MethodPost, c.repoPath("issues", strconv.FormatInt(n, 10), "labels"), map[string]any{"labels": []string{label}}, &applied); err != nil {
		return n, err
	}
	if !hasLabel(applied, label) {
		return n, &LabelNotAppliedError{Issue: n, Label: label}
	}
	return n, nil
}

// requireLabel fails unless the repository has the label called name. A
// missing label is a *LabelNotFoundError.
func (c *Client) requireLabel(ctx context.Context, name string) error {
	var l Label
	err := c.do(ctx, http.MethodGet, c.repoPath("labels", name), nil, &l)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return &LabelNotFoundError{Label: name}
	}
	return err
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
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
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
