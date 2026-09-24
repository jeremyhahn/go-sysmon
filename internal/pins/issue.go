package pins

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const (
	// IssueMarker is written into the body of every issue check-latest
	// files. A later run recognises its own open issue by this marker rather
	// than by the title, which a person may edit.
	IssueMarker = "<!-- go-sysmon:check-latest -->"
	// IssueTitle is the title of a filed issue.
	IssueTitle = "build: bump a pinned toolchain, tool, base image or action to its latest stable release"
	// IssueLabel is the label a filed issue carries, so the sweepers that
	// query by label see it.
	IssueLabel = "task:ready"
)

// Tracker is the issue tracker a lag report is filed in.
type Tracker interface {
	// FindOpen returns the number of an open issue whose body contains
	// marker, or 0 when there is none.
	FindOpen(ctx context.Context, marker string) (int64, error)
	// Create files an issue carrying label and returns its number. It fails
	// unless the label is verifiably on the new issue.
	Create(ctx context.Context, title, body, label string) (int64, error)
}

// Outcome is what filing a report did.
type Outcome string

const (
	// NothingLags means every item is current, so nothing was filed.
	NothingLags Outcome = "nothing-lags"
	// AlreadyOpen means an issue from an earlier run is still open, so no
	// duplicate was filed.
	AlreadyOpen Outcome = "already-open"
	// Filed means a new issue was filed.
	Filed Outcome = "filed"
	// WouldFile means a dry run found an issue should be filed and printed it
	// instead.
	WouldFile Outcome = "would-file"
)

// Filer files a lag report as one issue, and never a second while the first
// is open.
type Filer struct {
	Tracker Tracker
	// DryRun prints the issue that would be filed instead of filing it. The
	// search for an open issue still runs: it is read-only, and it is half of
	// what a dry run has to show.
	DryRun bool
	// RunURL links the issue to the workflow run that found the lag. Optional.
	RunURL string
	// Out receives a line for every decision, and the issue in a dry run.
	Out io.Writer
}

// File files an issue for the lagging items in report, unless nothing lags or
// an issue filed earlier is still open.
func (f Filer) File(ctx context.Context, report Report) (Outcome, error) {
	lag := report.Lagging()
	if len(lag) == 0 {
		fmt.Fprintln(f.Out, "check-latest: every item is at its latest stable release; nothing to file")
		return NothingLags, nil
	}
	open, err := f.Tracker.FindOpen(ctx, IssueMarker)
	if err != nil {
		return "", err
	}
	if open != 0 {
		fmt.Fprintf(f.Out, "check-latest: issue #%d is still open for this; not filing a duplicate\n", open)
		return AlreadyOpen, nil
	}
	body := IssueBody(lag, f.RunURL)
	if f.DryRun {
		fmt.Fprintf(f.Out, "check-latest: dry run; would file this issue with label %s:\n\ntitle: %s\n\n%s\n", IssueLabel, IssueTitle, body)
		return WouldFile, nil
	}
	n, err := f.Tracker.Create(ctx, IssueTitle, body, IssueLabel)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(f.Out, "check-latest: filed issue #%d (%s)\n", n, IssueLabel)
	return Filed, nil
}

// IssueBody renders the body of the issue filed for the lagging results.
func IssueBody(lag []Result, runURL string) string {
	var b strings.Builder
	b.WriteString(IssueMarker + "\n")
	b.WriteString("**Objective.** Every pin at its latest stable release: Go, the dev tools, Node.js LTS, the Debian and Ubuntu base releases, the workflows' actions and the GitHub-hosted runner labels ")
	b.WriteString("(owner rule 2026-09-22: always the latest stable Go, golangci-lint and dev tools).\n\n")
	b.WriteString("**Lagging**, as found by the scheduled check-latest workflow")
	if runURL != "" {
		b.WriteString(" ([run](" + runURL + "))")
	}
	b.WriteString(":\n\n| Item | Pinned | Latest stable | Status |\n|---|---|---|---|\n")
	for _, r := range lag {
		b.WriteString("| " + r.Name + " | " + r.Pinned + " | " + r.Latest + " | " + string(r.Status) + " |\n")
	}
	b.WriteString("\n**Scope.** Bump each lagging pin at its one source, then make every gate pass on the new versions (`make lint`, `make test`, and the container and desktop suites for a base image):\n")
	b.WriteString("- Go: the `go` directive in go.mod.\n")
	b.WriteString("- A tool: its `*_VERSION` in the Makefile.\n")
	b.WriteString("- Node.js: `.node-version` (the latest LTS, not Current).\n")
	b.WriteString("- Debian or Ubuntu: `DEBIAN_RELEASE` or `UBUNTU_RELEASE` in the Makefile.\n")
	b.WriteString("- A hosted runner: `HOSTED_RUNNER_RELEASE` in the Makefile, then every `ubuntu-NN.04[-arm]` label in the workflows.\n")
	b.WriteString("- An action: its `uses:` ref in every workflow, keeping the pin's form (a major tag stays a major tag; a commit SHA moves to the new release's commit, with its `# vX.Y.Z` comment).\n\n")
	b.WriteString("`make check-pins` names every image tag and workflow reference still on the old release.\n\n")
	b.WriteString("**Verification.** `make check-latest` exits 0.\n\n")
	b.WriteString("Filed by `.github/workflows/check-latest.yml`, which files no second issue while this one is open.\n")
	return b.String()
}
