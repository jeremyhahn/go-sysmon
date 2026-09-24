package pins

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
)

const (
	sha610 = "d23441a48e516b6c34aea4fa41551a30e30af803"
	sha700 = "9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0"
	sha701 = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	shaTag = "9f698171ed81b15d1823a05fc7211befd50c8ae0"
	sha603 = "df4cb1c069e1874edd31b4311f1884172cec0e10"
	shaRC  = "1111111111111111111111111111111111111111"
)

// lsRemote is `git ls-remote --tags` output shaped like actions/checkout's:
// lightweight tags, an annotated tag with its peeled line, a moving major
// tag, a prerelease and a non-version tag.
var lsRemote = shaTag + "\trefs/tags/v6.0.3\n" +
	sha603 + "\trefs/tags/v6.0.3^{}\n" +
	sha610 + "\trefs/tags/v6.1.0\n" +
	sha701 + "\trefs/tags/v7\n" +
	sha700 + "\trefs/tags/v7.0.0\n" +
	sha701 + "\trefs/tags/v7.0.1\n" +
	shaRC + "\trefs/tags/v8.0.0-rc.1\n" +
	sha610 + "\trefs/tags/latest\n"

func checkoutTags(t *testing.T) []Tag {
	t.Helper()
	tags, err := ParseLsRemote([]byte(lsRemote))
	if err != nil {
		t.Fatalf("ParseLsRemote() error = %v", err)
	}
	return tags
}

func TestParseLsRemote_ResolvesAnnotatedTagsToTheirCommit(t *testing.T) {
	tags := checkoutTags(t)
	want := Tag{Name: "v6.0.3", Commit: sha603}
	if tags[0] != want {
		t.Errorf("tags[0] = %+v, want %+v (the peeled commit, not the tag object)", tags[0], want)
	}
	if len(tags) != 7 {
		t.Errorf("len(tags) = %d, want 7 (peeled lines are not tags)", len(tags))
	}
}

func TestParseLsRemote_PeeledLineFirstStillResolves(t *testing.T) {
	tags, err := ParseLsRemote([]byte(sha603 + "\trefs/tags/v6.0.3^{}\n" + shaTag + "\trefs/tags/v6.0.3\n"))
	if err != nil {
		t.Fatalf("ParseLsRemote() error = %v", err)
	}
	if want := []Tag{{Name: "v6.0.3", Commit: sha603}}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %+v, want %+v", tags, want)
	}
}

func TestParseLsRemote_MalformedIsAnError(t *testing.T) {
	for _, out := range []string{"fatal: repository not found\n", sha610 + "\trefs/heads/main\n", "abc\trefs/tags/v1\n"} {
		_, err := ParseLsRemote([]byte(out))
		var ferr *FetchResponseError
		if !errors.As(err, &ferr) {
			t.Errorf("ParseLsRemote(%q) error = %v, want *FetchResponseError", out, err)
		}
	}
}

func TestJudgeAction(t *testing.T) {
	tests := []struct {
		ref, latest string
		want        Status
	}{
		{"v7", "v7.0.1", Current},     // a major tag follows its line
		{"v6", "v7.0.1", Behind},      // ... and falls behind the next one
		{"v7.0", "v7.0.1", Current},   // a minor tag follows its line
		{"v7.0.0", "v7.0.1", Behind},  // an exact tag is compared exactly
		{"v7.0.1", "v7.0.1", Current}, //
		{"v8", "v7.0.1", Ahead},       // no v8 release: only a prerelease
		{"v8.0.0-rc.1", "v7.0.1", Prerelease},
		{sha701, "v7.0.1, pinned commit is v7.0.1", Current},
		{sha610, "v7.0.1, pinned commit is v6.1.0", Behind},
		{sha603, "v7.0.1, pinned commit is v6.0.3", Behind}, // via the peeled line
		{shaRC, "v7.0.1, pinned commit is v8.0.0-rc.1", Prerelease},
	}
	tags := checkoutTags(t)
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			latest, got, err := JudgeAction(tt.ref, tags)
			if err != nil {
				t.Fatalf("JudgeAction(%q) error = %v", tt.ref, err)
			}
			if got != tt.want || latest != tt.latest {
				t.Errorf("JudgeAction(%q) = %q, %q; want %q, %q", tt.ref, latest, got, tt.latest, tt.want)
			}
		})
	}
}

func TestJudgeAction_TagsWithoutAVPrefix(t *testing.T) {
	tags := []Tag{{Name: "1.2.3", Commit: sha700}, {Name: "1.3.0", Commit: sha701}}
	latest, got, err := JudgeAction("1.2.3", tags)
	if err != nil || got != Behind || latest != "v1.3.0" {
		t.Errorf("JudgeAction() = %q, %q, %v; want v1.3.0, behind", latest, got, err)
	}
}

func TestJudgeAction_NoVerdict(t *testing.T) {
	tags := checkoutTags(t)
	tests := map[string]struct {
		ref  string
		tags []Tag
	}{
		"branch":                      {"main", tags},
		"build metadata":              {"v7.0.1+meta", tags},
		"commit of no release":        {"2222222222222222222222222222222222222222", tags},
		"commit of a moving tag only": {sha610, []Tag{{Name: "v7.0.1", Commit: sha701}, {Name: "latest", Commit: sha610}}},
		"no stable release":           {"v1", []Tag{{Name: "v1.0.0-beta", Commit: sha700}, {Name: "v1", Commit: sha700}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, got, err := JudgeAction(tt.ref, tt.tags); err == nil {
				t.Errorf("JudgeAction(%q) = %q, want an error", tt.ref, got)
			}
		})
	}
}

func TestActionItems_FetchesEachRepositoryOnce(t *testing.T) {
	calls := map[string]int{}
	tags := func(_ context.Context, repo string) ([]Tag, error) {
		calls[repo]++
		return checkoutTags(t), nil
	}
	refs := []ActionRef{
		{Repo: "actions/checkout", Ref: "v7"},
		{Repo: "actions/checkout", Ref: "v7"},
		{Repo: "actions/checkout", Ref: "v6"},
		{Repo: "actions/checkout", Ref: sha610},
	}

	report, err := CheckLatest(context.Background(), ActionItems(refs, tags))
	if err != nil {
		t.Fatalf("CheckLatest() error = %v", err)
	}
	if calls["actions/checkout"] != 1 {
		t.Errorf("fetched actions/checkout %d times, want once", calls["actions/checkout"])
	}
	got := map[string]Status{}
	for _, r := range report.Results {
		got[r.Pinned] = r.Status
	}
	want := map[string]Status{"v7": Current, "v6": Behind, sha610: Behind}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts = %v, want %v (one per distinct pin)", got, want)
	}
}

func TestActionItems_OfflineIsNotUpToDate(t *testing.T) {
	offline := syscall.ECONNREFUSED
	tags := func(context.Context, string) ([]Tag, error) { return nil, offline }

	unchecked(t, ActionItems([]ActionRef{{Repo: "actions/checkout", Ref: "v7"}}, tags)[0], offline)
}

func TestWorkflowActions(t *testing.T) {
	root := writeTree(t, map[string]string{
		".github/workflows/ci.yml": `jobs:
  a:
    container: golang:1.27.1-trixie
    steps:
      - uses: actions/checkout@v7
      - uses: github/codeql-action/upload-sarif@v3
      - uses: ./.github/actions/local
      - uses: docker://ubuntu:26.04
      - uses: noref
      - run: echo
`,
		".gitea/workflows/n.yaml": "jobs:\n  b:\n    steps:\n      - uses: actions/checkout@" + sha701 + " # v7.0.1\n",
		"docs/ci.yml":             "jobs:\n  c:\n    steps:\n      - uses: other/thing@v1\n",
	})

	got, err := WorkflowActions(root)
	if err != nil {
		t.Fatalf("WorkflowActions() error = %v", err)
	}
	want := []ActionRef{
		{File: ".gitea/workflows/n.yaml", Line: 4, Repo: "actions/checkout", Ref: sha701},
		{File: ".github/workflows/ci.yml", Line: 5, Repo: "actions/checkout", Ref: "v7"},
		{File: ".github/workflows/ci.yml", Line: 6, Repo: "github/codeql-action", Ref: "v3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkflowActions() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestWorkflowActions_UnparsableWorkflowIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{".github/workflows/ci.yml": "jobs: [unclosed\n"})
	_, err := WorkflowActions(root)
	var serr *SourceError
	if !errors.As(err, &serr) {
		t.Fatalf("WorkflowActions() error = %v, want *SourceError", err)
	}
}
