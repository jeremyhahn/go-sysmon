package pins

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"syscall"
	"testing"
)

// runnerReadme is shaped like the actions/runner-images README table: a GA
// 26.04 on both architectures, older releases, a preview Ubuntu that is newer
// than anything GA, and a preview Xcode.
const runnerReadme = `# GitHub Actions Runner Images

| Image | Architecture | YAML Label | Included Software |
| --------------------|--------------|---------------------|------------------|
| Ubuntu 28.04 [![beta](https://img.shields.io/badge/beta-0969DA?style=flat)](https://x)<br>![b](https://x) | x64 | ` + "`ubuntu-28.04`" + ` | [x] |
| Ubuntu 26.04<br>![b](https://x) | x64 | ` + "`ubuntu-26.04`" + ` | [x] |
| Ubuntu 26.04 Arm64<br>![b](https://x) | arm64 | ` + "`ubuntu-26.04-arm`" + ` | [x] |
| Ubuntu 24.04<br>![b](https://x) | x64 | ` + "`ubuntu-latest` or `ubuntu-24.04`" + ` | [x] |
| Ubuntu 24.04 Arm64<br>![b](https://x) | arm64 | ` + "`ubuntu-24.04-arm`" + ` | [x] |
| Xcode 27 [![preview](https://img.shields.io/badge/preview-0969DA?style=flat)](https://x) | arm64 | ` + "`xcode-27` or `xcode-27-xlarge`" + ` | [x] |
`

func runnerTable(t *testing.T) []RunnerLabel {
	t.Helper()
	labels, err := ParseRunnerImages([]byte(runnerReadme))
	if err != nil {
		t.Fatalf("ParseRunnerImages() error = %v", err)
	}
	return labels
}

func TestParseRunnerImages(t *testing.T) {
	want := []RunnerLabel{
		{Name: "ubuntu-28.04", Preview: true},
		{Name: "ubuntu-26.04"},
		{Name: "ubuntu-26.04-arm"},
		{Name: "ubuntu-latest"},
		{Name: "ubuntu-24.04"},
		{Name: "ubuntu-24.04-arm"},
		{Name: "xcode-27", Preview: true},
		{Name: "xcode-27-xlarge", Preview: true},
	}
	if got := runnerTable(t); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRunnerImages() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseRunnerImages_NoTableIsAnError(t *testing.T) {
	_, err := ParseRunnerImages([]byte("# moved\n\nSee the new location.\n"))
	var ferr *FetchResponseError
	if !errors.As(err, &ferr) {
		t.Fatalf("ParseRunnerImages() error = %v, want *FetchResponseError", err)
	}
}

func TestJudgeRunner(t *testing.T) {
	tests := []struct {
		label, latest string
		want          Status
	}{
		// The beta 28.04 is newer but not GA, so 26.04 is current.
		{"ubuntu-26.04", "ubuntu-26.04", Current},
		{"ubuntu-26.04-arm", "ubuntu-26.04-arm", Current},
		{"ubuntu-24.04", "ubuntu-26.04", Behind},
		{"ubuntu-24.04-arm", "ubuntu-26.04-arm", Behind},
		{"ubuntu-28.04", "ubuntu-26.04", Prerelease},
	}
	table := runnerTable(t)
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			latest, got, err := JudgeRunner(tt.label, table)
			if err != nil {
				t.Fatalf("JudgeRunner(%q) error = %v", tt.label, err)
			}
			if got != tt.want || latest != tt.latest {
				t.Errorf("JudgeRunner(%q) = %q, %q; want %q, %q", tt.label, latest, got, tt.latest, tt.want)
			}
		})
	}
}

func TestJudgeRunner_UnlistedLabelIsTyped(t *testing.T) {
	_, got, err := JudgeRunner("ubuntu-22.04-arm", runnerTable(t))
	var rerr *RunnerLabelError
	if !errors.As(err, &rerr) || rerr.Label != "ubuntu-22.04-arm" {
		t.Fatalf("JudgeRunner() = %q, %v; want a *RunnerLabelError and no verdict", got, err)
	}
}

func TestJudgeRunner_NoVerdict(t *testing.T) {
	onlyBeta := []RunnerLabel{{Name: "ubuntu-28.04-arm", Preview: true}}
	for name, tt := range map[string]struct {
		label string
		table []RunnerLabel
	}{
		"not a versioned label":      {"ubuntu-latest", runnerTable(t)},
		"no GA label for the arch":   {"ubuntu-28.04-arm", onlyBeta},
		"label with trailing suffix": {"ubuntu-26.04-arm64", runnerTable(t)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, got, err := JudgeRunner(tt.label, tt.table); err == nil {
				t.Errorf("JudgeRunner(%q) = %q, want an error", tt.label, got)
			}
		})
	}
}

func TestRunnerItems_FetchesTheTableOnce(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]RunnerLabel, error) {
		calls++
		return runnerTable(t), nil
	}

	report, err := CheckLatest(context.Background(), RunnerItems([]string{"ubuntu-26.04", "ubuntu-24.04-arm", "ubuntu-26.04"}, fetch))
	if err != nil {
		t.Fatalf("CheckLatest() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("fetched the table %d times, want once", calls)
	}
	got := map[string]Status{}
	for _, r := range report.Results {
		got[r.Pinned] = r.Status
	}
	if want := map[string]Status{"ubuntu-26.04": Current, "ubuntu-24.04-arm": Behind}; !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
}

func TestRunnerItems_OfflineIsNotUpToDate(t *testing.T) {
	offline := syscall.ECONNREFUSED
	fetch := func(context.Context) ([]RunnerLabel, error) { return nil, offline }

	unchecked(t, RunnerItems([]string{"ubuntu-26.04"}, fetch)[0], offline)
}

func TestRunnerImages_Labels(t *testing.T) {
	client, url := serveBody(t, http.StatusOK, runnerReadme)
	got, err := RunnerImages{Client: client, URL: url}.Labels(context.Background())
	if err != nil || len(got) != 8 {
		t.Fatalf("Labels() = %d labels, %v; want 8", len(got), err)
	}
}

func TestRunnerImages_BadAnswerAndOffline(t *testing.T) {
	client, url := serveBody(t, http.StatusNotFound, "")
	var ferr *FetchResponseError
	if _, err := (RunnerImages{Client: client, URL: url}).Labels(context.Background()); !errors.As(err, &ferr) {
		t.Errorf("Labels() on 404 error = %v, want *FetchResponseError", err)
	}
	client, url = closedURL(t)
	if _, err := (RunnerImages{Client: client, URL: url}).Labels(context.Background()); err == nil {
		t.Error("Labels() offline error = nil")
	}
}

func TestWorkflowRunners(t *testing.T) {
	root := writeTree(t, map[string]string{
		".github/workflows/release.yml": `jobs:
  build:
    # ubuntu-22.04 in a comment is not a label
    runs-on: ${{ (github.server_url == 'https://github.com' && matrix.runner) || 'ubuntu-latest' }}
    strategy:
      matrix:
        include:
          - runner: ubuntu-26.04-arm
  other:
    runs-on: [self-hosted, ubuntu-26.04]
`,
	})

	got, err := WorkflowRunners(root)
	if err != nil {
		t.Fatalf("WorkflowRunners() error = %v", err)
	}
	want := []RunnerRef{
		{File: ".github/workflows/release.yml", Line: 8, Label: "ubuntu-26.04-arm"},
		{File: ".github/workflows/release.yml", Line: 10, Label: "ubuntu-26.04"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkflowRunners() = %+v, want %+v", got, want)
	}
}

func TestWorkflowRunners_UnparsableWorkflowIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{".github/workflows/ci.yml": "jobs: [unclosed\n"})
	_, err := WorkflowRunners(root)
	var serr *SourceError
	if !errors.As(err, &serr) {
		t.Fatalf("WorkflowRunners() error = %v, want *SourceError", err)
	}
}

func TestRunnerLabelError(t *testing.T) {
	want := `pins: runner label "ubuntu-22.04-arm" is not a GitHub-hosted runner listed by actions/runner-images`
	if got := (&RunnerLabelError{Label: "ubuntu-22.04-arm"}).Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
