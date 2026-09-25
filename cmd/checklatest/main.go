// Command checklatest fails when any pin is not at its latest stable release:
// the go directive in go.mod, each dev tool in the Makefile, Node.js (against
// the latest LTS), the Debian and Ubuntu base releases, every action the
// workflows use, and the GitHub-hosted runner labels. It is online; docs/testing/toolchain.md lists each source.
//
//	make check-latest
//
// Exit status: 0 when every item is current, 1 when any item lags, 2 when any
// item could not be checked. (`go run` and make report any non-zero status as
// a plain failure; the message says which it was.) An item whose latest
// release is unknown is never reported as current.
//
// With -file-issue it files the lag as one task:ready issue on the Gitea
// repository instead of failing, and files none while one is still open; add
// -dry-run to print that issue rather than file it. This is what the daily
// check-latest workflow runs. The token is read from the environment variable
// named by -token-env, never from a flag, so it does not appear in a process
// listing.
//
// Each source is fetched from a URL a flag names, defaulting to the public
// one: -go-releases-url, -node-index-url, -debian-dists-url,
// -ubuntu-series-url, -runner-images-url and -actions-base-url. Pointing them
// at a mirror is how the check runs where the public sources are unreachable.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/gitea"
	"github.com/jeremyhahn/go-sysmon/internal/pins"
)

// Exit statuses.
const (
	exitCurrent   = 0
	exitLagging   = 1
	exitUnchecked = 2
)

// toolFlags collects repeated -tool name=module@version flags.
type toolFlags []pins.Tool

func (t *toolFlags) String() string {
	parts := make([]string, len(*t))
	for i, tool := range *t {
		parts[i] = tool.Name + "=" + tool.Module + "@" + tool.Version
	}
	return strings.Join(parts, " ")
}

func (t *toolFlags) Set(s string) error {
	tool, err := pins.ParseTool(s)
	if err != nil {
		return err
	}
	*t = append(*t, tool)
	return nil
}

// sourceURL is a flag naming where one source is fetched from. It accepts
// only an absolute URL with a host and one of the allowed schemes, so a typo
// is a usage error rather than a check that could not run.
type sourceURL struct {
	url     string
	schemes map[string]bool
}

func (u *sourceURL) String() string { return u.url }

func (u *sourceURL) Set(s string) error {
	parsed, err := url.Parse(s)
	if err != nil {
		return err
	}
	if !u.schemes[parsed.Scheme] || (parsed.Host == "" && parsed.Scheme != "file") || (parsed.Scheme == "file" && parsed.Path == "") {
		return &sourceURLError{URL: s}
	}
	u.url = s
	return nil
}

// sourceURLError reports a source flag that is not a usable URL.
type sourceURLError struct {
	URL string
}

func (e *sourceURLError) Error() string {
	return "checklatest: " + strings.TrimSpace(e.URL) + " is not an absolute URL this source can be fetched from"
}

// httpSchemes are where an HTTP source may be fetched from.
var httpSchemes = map[string]bool{"http": true, "https": true}

// gitSchemes are where git can list an action repository's tags from: a
// server, or a local mirror of the repositories.
var gitSchemes = map[string]bool{"http": true, "https": true, "file": true}

// sourceURLs are where each live source is fetched from.
type sourceURLs struct {
	goReleases   string
	nodeIndex    string
	debianDists  string
	ubuntuSeries string
	runnerImages string
	actionsBase  string
}

func main() {
	o, timeout, err := parseOptions(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	if err != nil {
		// The flag package has already printed the error and the usage.
		os.Exit(exitUnchecked)
	}
	client := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	code := run(ctx, o, newSources(client, o.urls), client)
	cancel()
	os.Exit(code)
}

// parseOptions reads the command line. The API token is read from the
// environment variable -token-env names, through getenv, so it never appears
// in the process's arguments. A malformed command line is reported on stderr,
// with the usage, and returned as an error.
func parseOptions(args []string, getenv func(string) string, stdout, stderr io.Writer) (options, time.Duration, error) {
	fs := flag.NewFlagSet("checklatest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tools toolFlags
	root := fs.String("root", ".", "repository root: go.mod, the Node.js version file and the workflows")
	nodeFile := fs.String("node-file", ".node-version", "file under root holding the Node.js release")
	debian := fs.String("debian", "", "pinned Debian release codename")
	ubuntu := fs.String("ubuntu", "", "pinned Ubuntu release")
	hostedRunner := fs.String("hosted-runner", "", "pinned Ubuntu release of GitHub-hosted runner labels")
	fs.Var(&tools, "tool", "a pinned tool as name=module@version; repeat for each")
	timeout := fs.Duration("timeout", 5*time.Minute, "limit on the whole check")
	fileIssue := fs.Bool("file-issue", false, "file the lag as a Gitea issue instead of failing")
	dryRun := fs.Bool("dry-run", false, "with -file-issue: print the issue instead of filing it")
	api := fs.String("gitea-api", "", "with -file-issue: Gitea API root, https://host/api/v1")
	repo := fs.String("repo", "", "with -file-issue: owner/name of the repository")
	tokenEnv := fs.String("token-env", "GITEA_TOKEN", "with -file-issue: environment variable holding the API token")
	runURL := fs.String("run-url", "", "with -file-issue: workflow run to link from the issue")
	goReleases := &sourceURL{url: pins.GoVersionURL, schemes: httpSchemes}
	nodeIndex := &sourceURL{url: pins.NodeIndexURL, schemes: httpSchemes}
	debianDists := &sourceURL{url: pins.DebianDistsURL, schemes: httpSchemes}
	ubuntuSeries := &sourceURL{url: pins.UbuntuSeriesURL, schemes: httpSchemes}
	runnerImages := &sourceURL{url: pins.RunnerImagesURL, schemes: httpSchemes}
	actionsBase := &sourceURL{url: pins.ActionsBaseURL, schemes: gitSchemes}
	fs.Var(goReleases, "go-releases-url", "Go's latest-release document")
	fs.Var(nodeIndex, "node-index-url", "the Node.js release index")
	fs.Var(debianDists, "debian-dists-url", "the Debian archive's dists directory")
	fs.Var(ubuntuSeries, "ubuntu-series-url", "Launchpad's Ubuntu series list")
	fs.Var(runnerImages, "runner-images-url", "the actions/runner-images README")
	fs.Var(actionsBase, "actions-base-url", "where action repositories are listed from with git ls-remote (http, https or file)")
	if err := fs.Parse(args); err != nil {
		return options{}, 0, err
	}
	return options{
		root: *root, nodeFile: *nodeFile, debian: *debian, ubuntu: *ubuntu, hostedRunner: *hostedRunner,
		tools: tools, fileIssue: *fileIssue, dryRun: *dryRun,
		api: *api, repo: *repo, token: getenv(*tokenEnv), runURL: *runURL,
		urls: sourceURLs{
			goReleases: goReleases.url, nodeIndex: nodeIndex.url, debianDists: debianDists.url,
			ubuntuSeries: ubuntuSeries.url, runnerImages: runnerImages.url, actionsBase: actionsBase.url,
		},
		stdout: stdout, stderr: stderr,
	}, *timeout, nil
}

// newSources returns the live sources of every pin's latest release, fetched
// from urls through client.
func newSources(client *http.Client, urls sourceURLs) sources {
	return sources{
		goLatest:  pins.GoReleases{Client: client, URL: urls.goReleases}.Latest,
		module:    pins.GoList{Run: pins.Run}.Latest,
		nodeLTS:   pins.NodeReleases{Client: client, URL: urls.nodeIndex}.LatestLTS,
		debian:    pins.DebianReleases{Client: client, URL: urls.debianDists}.Release,
		ubuntuLTS: pins.UbuntuReleases{Client: client, URL: urls.ubuntuSeries}.LatestLTS,
		tags:      pins.GitTags{Run: pins.Run, BaseURL: urls.actionsBase}.Tags,
		runners:   pins.RunnerImages{Client: client, URL: urls.runnerImages}.Labels,
	}
}

type options struct {
	root     string
	nodeFile string
	debian   string
	ubuntu   string
	// hostedRunner is the pinned Ubuntu release of hosted runner labels.
	hostedRunner string
	tools        []pins.Tool
	fileIssue    bool
	dryRun       bool
	api          string
	repo         string
	token        string
	runURL       string
	urls         sourceURLs
	stdout       io.Writer
	stderr       io.Writer
}

// sources are where each kind of pin's latest stable release comes from.
type sources struct {
	goLatest  pins.LatestFunc
	module    pins.ModuleLatestFunc
	nodeLTS   pins.LatestFunc
	debian    pins.DebianReleaseFunc
	ubuntuLTS pins.LatestFunc
	tags      pins.TagsFunc
	runners   pins.RunnerLabelsFunc
}

// items builds the list of pins to judge: Go, every tool, Node.js, the Debian
// and Ubuntu base releases, every action the workflows use, and the hosted
// runner labels -- the pinned release on both architectures, and every label
// a workflow names.
func items(o options, p pins.Pins, src sources) ([]pins.Item, error) {
	list := []pins.Item{pins.GoItem(p.Go, src.goLatest)}
	for _, t := range o.tools {
		list = append(list, pins.ToolItem(t, src.module))
	}
	list = append(list,
		pins.NodeItem(p.Node, src.nodeLTS),
		pins.DebianItem(p.Debian, src.debian),
		pins.UbuntuItem(p.Ubuntu, src.ubuntuLTS),
	)
	actions, err := pins.WorkflowActions(o.root)
	if err != nil {
		return nil, err
	}
	list = append(list, pins.ActionItems(actions, src.tags)...)
	runners, err := pins.WorkflowRunners(o.root)
	if err != nil {
		return nil, err
	}
	labels := []string{"ubuntu-" + p.HostedRunner, "ubuntu-" + p.HostedRunner + "-arm"}
	for _, r := range runners {
		labels = append(labels, r.Label)
	}
	return append(list, pins.RunnerItems(labels, src.runners)...), nil
}

// run checks every pin and returns the exit status. client is what a Gitea
// filer calls the API with.
func run(ctx context.Context, o options, src sources, client *http.Client) int {
	// Resolve the filer first: a misconfigured workflow should fail before it
	// spends a minute on the network.
	var filer *pins.Filer
	if o.fileIssue {
		f, err := newFiler(o, client)
		if err != nil {
			fmt.Fprintln(o.stderr, err)
			return exitUnchecked
		}
		filer = f
	}

	p, err := pins.ReadPins(o.root, o.nodeFile, o.debian, o.ubuntu, o.hostedRunner)
	if err != nil {
		fmt.Fprintln(o.stderr, err)
		return exitUnchecked
	}
	list, err := items(o, p, src)
	if err != nil {
		fmt.Fprintln(o.stderr, err)
		return exitUnchecked
	}
	report, checkErr := pins.CheckLatest(ctx, list)
	for _, r := range report.Results {
		fmt.Fprintln(o.stdout, r.String())
	}
	if checkErr != nil {
		fmt.Fprintln(o.stderr, checkErr)
	}

	if filer != nil {
		// What lags is filed even when another item could not be checked;
		// the run still fails so the unchecked item is seen.
		if _, err := filer.File(ctx, report); err != nil {
			fmt.Fprintln(o.stderr, err)
			return exitUnchecked
		}
		if checkErr != nil {
			return exitUnchecked
		}
		return exitCurrent
	}

	if checkErr != nil {
		return exitUnchecked
	}
	if err := report.Err(); err != nil {
		fmt.Fprintln(o.stderr, err)
		return exitLagging
	}
	return exitCurrent
}

// noTokenError is returned when -file-issue is set and the token variable
// named by -token-env is empty. A blank token would not be inert: Gitea answers
// an unauthenticated caller, and the failure would look like a permissions
// problem.
type noTokenError struct{}

func (e *noTokenError) Error() string {
	return "checklatest: -file-issue needs an API token in the variable named by -token-env"
}

// filerFlagsError is returned when -file-issue is set without -gitea-api and
// an owner/name -repo.
type filerFlagsError struct{}

func (e *filerFlagsError) Error() string {
	return "checklatest: -file-issue needs -gitea-api and -repo owner/name"
}

func newFiler(o options, client *http.Client) (*pins.Filer, error) {
	owner, name, ok := strings.Cut(o.repo, "/")
	if o.api == "" || !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, &filerFlagsError{}
	}
	if o.token == "" {
		return nil, &noTokenError{}
	}
	return &pins.Filer{
		Tracker: &gitea.Client{BaseURL: o.api, Owner: owner, Repo: name, Token: o.token, HTTP: client},
		DryRun:  o.dryRun,
		RunURL:  o.runURL,
		Out:     o.stdout,
	}, nil
}
