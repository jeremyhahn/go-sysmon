package pins

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// tracker is an in-memory Tracker.
type tracker struct {
	open      int64
	findErr   error
	createN   int64
	createErr error
	created   []string // titles filed
	bodies    []string
	labels    []string
}

func (tr *tracker) FindOpen(_ context.Context, marker string) (int64, error) {
	if marker != IssueMarker {
		return 0, errors.New("tracker: searched for the wrong marker")
	}
	return tr.open, tr.findErr
}

func (tr *tracker) Create(_ context.Context, title, body, label string) (int64, error) {
	if tr.createErr != nil {
		return 0, tr.createErr
	}
	tr.created = append(tr.created, title)
	tr.bodies = append(tr.bodies, body)
	tr.labels = append(tr.labels, label)
	return tr.createN, nil
}

var laggingReport = Report{Results: []Result{
	{Name: "go", Pinned: "1.27.1", Latest: "go1.27.1", Status: Current},
	{Name: "golangci-lint", Pinned: "v2.13.2", Latest: "v2.14.0", Status: Behind},
}}

func TestFiler_FilesOneLabelledIssue(t *testing.T) {
	tr := &tracker{createN: 240}
	var out bytes.Buffer

	got, err := Filer{Tracker: tr, RunURL: "https://gitea.example/r/actions/runs/7", Out: &out}.File(context.Background(), laggingReport)
	if err != nil {
		t.Fatalf("File() error = %v", err)
	}
	if got != Filed {
		t.Errorf("File() = %q, want %q", got, Filed)
	}
	if len(tr.created) != 1 || tr.created[0] != IssueTitle || tr.labels[0] != "task:ready" {
		t.Fatalf("filed %q with labels %q, want one %q with task:ready", tr.created, tr.labels, IssueTitle)
	}
	body := tr.bodies[0]
	for _, want := range []string{IssueMarker, "| golangci-lint | v2.13.2 | v2.14.0 | behind |", "https://gitea.example/r/actions/runs/7"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "| go |") {
		t.Errorf("body lists a current item:\n%s", body)
	}
	if !strings.Contains(out.String(), "filed issue #240") {
		t.Errorf("output = %q, want the issue number", out.String())
	}
}

func TestFiler_NoDuplicateWhileOneIsOpen(t *testing.T) {
	tr := &tracker{open: 231}
	var out bytes.Buffer

	got, err := Filer{Tracker: tr, Out: &out}.File(context.Background(), laggingReport)
	if err != nil {
		t.Fatalf("File() error = %v", err)
	}
	if got != AlreadyOpen || len(tr.created) != 0 {
		t.Errorf("File() = %q and filed %q, want %q and nothing filed", got, tr.created, AlreadyOpen)
	}
	if !strings.Contains(out.String(), "#231") {
		t.Errorf("output = %q, want the open issue named", out.String())
	}
}

func TestFiler_NothingLagsFilesNothing(t *testing.T) {
	tr := &tracker{findErr: errors.New("tracker: must not be asked")}
	current := Report{Results: laggingReport.Results[:1]}

	got, err := Filer{Tracker: tr, Out: &bytes.Buffer{}}.File(context.Background(), current)
	if err != nil || got != NothingLags || len(tr.created) != 0 {
		t.Errorf("File() = %q, %v, filed %q; want %q, nil, nothing", got, err, tr.created, NothingLags)
	}
}

func TestFiler_DryRunPrintsTheIssueAndFilesNothing(t *testing.T) {
	tr := &tracker{}
	var out bytes.Buffer

	got, err := Filer{Tracker: tr, DryRun: true, Out: &out}.File(context.Background(), laggingReport)
	if err != nil {
		t.Fatalf("File() error = %v", err)
	}
	if got != WouldFile || len(tr.created) != 0 {
		t.Errorf("File() = %q and filed %q, want %q and nothing filed", got, tr.created, WouldFile)
	}
	for _, want := range []string{"dry run", "task:ready", "title: " + IssueTitle, "| golangci-lint | v2.13.2 | v2.14.0 | behind |"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestFiler_DryRunStillHonoursAnOpenIssue(t *testing.T) {
	tr := &tracker{open: 231}

	got, err := Filer{Tracker: tr, DryRun: true, Out: &bytes.Buffer{}}.File(context.Background(), laggingReport)
	if err != nil || got != AlreadyOpen {
		t.Errorf("File() = %q, %v; want %q", got, err, AlreadyOpen)
	}
}

func TestFiler_TrackerErrorsAreReturned(t *testing.T) {
	findErr := errors.New("tracker: search failed")
	createErr := errors.New("tracker: create failed")
	for name, tr := range map[string]*tracker{
		"search": {findErr: findErr},
		"create": {createErr: createErr},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Filer{Tracker: tr, Out: &bytes.Buffer{}}.File(context.Background(), laggingReport)
			if !errors.Is(err, findErr) && !errors.Is(err, createErr) {
				t.Errorf("File() = %q, %v; want the tracker's error", got, err)
			}
		})
	}
}

func TestIssueBody_WithoutRunURL(t *testing.T) {
	body := IssueBody(laggingReport.Lagging(), "")
	if strings.Contains(body, "[run]") {
		t.Errorf("body links a run it was not given:\n%s", body)
	}
	if !strings.HasPrefix(body, IssueMarker) {
		t.Errorf("body does not start with the marker:\n%s", body)
	}
}
