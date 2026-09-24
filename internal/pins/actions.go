package pins

import (
	"bufio"
	"bytes"
	"context"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"
)

// ActionRef is one `uses: owner/repo[/path]@ref` in a workflow.
type ActionRef struct {
	File string
	Line int
	// Repo is the repository the action is released from, "actions/checkout".
	Repo string
	// Ref is what it is pinned to: a release tag ("v7", "v7.0.1") or a
	// commit SHA.
	Ref string
}

// WorkflowActions returns every action the workflows under root use, in file
// order. Local actions (./path) and docker:// steps are not released
// actions and are not returned.
func WorkflowActions(root string) ([]ActionRef, error) {
	var refs []ActionRef
	err := walkFiles(root, func(p, name string) scanFunc {
		if !isWorkflow(p, name) {
			return nil
		}
		return func(file string, data []byte) error {
			doc, err := parseYAML(file, data)
			if err != nil || doc == nil {
				return err
			}
			workflowSteps(doc, func(int, string) {}, func(_, uses *yaml.Node, _ []map[string]string) {
				if ref, ok := parseUses(uses.Value); ok {
					ref.File, ref.Line = file, uses.Line
					refs = append(refs, ref)
				}
			})
			return nil
		}
	})
	return refs, err
}

// parseUses splits "owner/repo/path@ref". It reports false for a local
// action, which has no release.
func parseUses(uses string) (ActionRef, bool) {
	action, ref, ok := strings.Cut(uses, "@")
	if !ok || strings.HasPrefix(action, ".") {
		return ActionRef{}, false
	}
	parts := strings.SplitN(action, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ActionRef{}, false
	}
	return ActionRef{Repo: parts[0] + "/" + parts[1], Ref: ref}, true
}

// Tag is one tag of a repository and the commit it points at. An annotated
// tag's commit is its peeled target, which is what a SHA pin names.
type Tag struct {
	Name   string
	Commit string
}

// TagsFunc returns the tags of an action's repository, "actions/checkout".
type TagsFunc func(ctx context.Context, repo string) ([]Tag, error)

// ParseLsRemote reads `git ls-remote --tags` output into tags, resolving each
// annotated tag to the commit its "^{}" line names.
func ParseLsRemote(out []byte) ([]Tag, error) {
	direct := map[string]string{}
	peeled := map[string]string{}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, "\t")
		name, isTag := strings.CutPrefix(ref, "refs/tags/")
		if !ok || !isTag || !isSHA(sha) {
			return nil, &FetchResponseError{Source: "git ls-remote", Detail: "unexpected line " + quoteShort(line)}
		}
		if base, isPeeled := strings.CutSuffix(name, "^{}"); isPeeled {
			peeled[base] = sha
			continue
		}
		if _, seen := direct[name]; !seen {
			names = append(names, name)
		}
		direct[name] = sha
	}
	if err := sc.Err(); err != nil {
		return nil, &FetchResponseError{Source: "git ls-remote", Detail: err.Error()}
	}
	tags := make([]Tag, 0, len(names))
	for _, n := range names {
		commit, ok := peeled[n]
		if !ok {
			commit = direct[n]
		}
		tags = append(tags, Tag{Name: n, Commit: commit})
	}
	return tags, nil
}

// commitSHA matches a full commit SHA, the only safe SHA pin.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isSHA(s string) bool { return commitSHA.MatchString(s) }

// releaseVersion returns a tag name as a canonical semantic version, adding
// the "v" some repositories leave off, or "" when it is not one exact
// version. Moving major tags ("v7") are not exact versions.
func releaseVersion(name string) string {
	v := name
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if semver.Canonical(v) != v {
		return ""
	}
	return v
}

// latestRelease returns the newest stable release among tags.
func latestRelease(tags []Tag) string {
	latest := ""
	for _, t := range tags {
		v := releaseVersion(t.Name)
		if v != "" && semver.Prerelease(v) == "" && (latest == "" || semver.Compare(v, latest) > 0) {
			latest = v
		}
	}
	return latest
}

// JudgeAction judges one action pin against the repository's tags. It returns
// the latest stable release as it is reported.
//
// A tag pin is compared at its own precision, which is the convention's
// point: "v7" follows the v7 line and is Current while the latest release is
// any v7.x.y, "v7.0" while it is any v7.0.y. A commit SHA pin is the newest
// release tag pointing at that commit, compared exactly.
func JudgeAction(ref string, tags []Tag) (string, Status, error) {
	latest := latestRelease(tags)
	if latest == "" {
		return "", "", &FetchResponseError{Source: "action tags", Detail: "no stable release tag"}
	}
	if isSHA(ref) {
		pinned := ""
		for _, t := range tags {
			if v := releaseVersion(t.Name); t.Commit == ref && v != "" && (pinned == "" || semver.Compare(v, pinned) > 0) {
				pinned = v
			}
		}
		if pinned == "" {
			return "", "", &VersionError{Kind: "release commit", Value: ref}
		}
		reported := latest + ", pinned commit is " + pinned
		if semver.Prerelease(pinned) != "" {
			return reported, Prerelease, nil
		}
		return reported, classify(semver.Compare(pinned, latest)), nil
	}
	v := ref
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) || semver.Build(v) != "" {
		return "", "", &VersionError{Kind: "action release", Value: ref}
	}
	if semver.Prerelease(v) != "" {
		return latest, Prerelease, nil
	}
	truncate := precisions[strings.Count(v, ".")]
	return latest, classify(semver.Compare(v, truncate(latest))), nil
}

// precisions truncates a version to the precision of a pin with that many
// dots: its major line for "v7", its minor line for "v7.0", itself for
// "v7.0.1".
var precisions = map[int]func(string) string{
	0: semver.Major,
	1: semver.MajorMinor,
	2: func(v string) string { return v },
}

// ActionItems returns one Item per distinct action pin, fetching each
// repository's tags once however many pins share it.
func ActionItems(refs []ActionRef, tags TagsFunc) []Item {
	type fetched struct {
		tags []Tag
		err  error
	}
	cache := map[string]*fetched{}
	fetch := func(ctx context.Context, repo string) ([]Tag, error) {
		if f, ok := cache[repo]; ok {
			return f.tags, f.err
		}
		t, err := tags(ctx, repo)
		cache[repo] = &fetched{tags: t, err: err}
		return t, err
	}

	var items []Item
	seen := map[string]bool{}
	for _, r := range refs {
		key := r.Repo + "@" + r.Ref
		if seen[key] {
			continue
		}
		seen[key] = true
		repo, ref := r.Repo, r.Ref
		items = append(items, Item{Name: repo, Pinned: ref, Check: func(ctx context.Context) (string, Status, error) {
			t, err := fetch(ctx, repo)
			if err != nil {
				return "", "", err
			}
			return JudgeAction(ref, t)
		}})
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Pinned, b.Pinned)
	})
	return items
}
