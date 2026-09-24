package pins

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// RunnerImagesURL is the README of actions/runner-images, whose table lists
// every GitHub-hosted runner image with its YAML labels and a badge on those
// in beta or preview.
const RunnerImagesURL = "https://raw.githubusercontent.com/actions/runner-images/main/README.md"

// hostedUbuntuLabel matches a versioned GitHub-hosted Ubuntu runner label,
// "ubuntu-26.04" or "ubuntu-26.04-arm", and captures the release and the arm
// suffix. ubuntu-latest is deliberately not matched: on the Gitea plane it is
// a self-hosted label, which is company infrastructure, not a pin.
var hostedUbuntuLabel = regexp.MustCompile(`\bubuntu-([0-9]{2}\.[0-9]{2})(-arm)?\b`)

// RunnerLabel is one label in the runner-images table.
type RunnerLabel struct {
	Name string
	// Preview is true for an image badged beta or preview: not GA.
	Preview bool
}

// RunnerLabelsFunc returns the runner-images table's labels.
type RunnerLabelsFunc func(ctx context.Context) ([]RunnerLabel, error)

// RunnerRef is one hosted Ubuntu runner label in a workflow.
type RunnerRef struct {
	File  string
	Line  int
	Label string
}

// previewBadge matches the badge runner-images puts on an image that is not
// generally available.
var previewBadge = regexp.MustCompile(`img\.shields\.io/badge/(beta|preview)-`)

// backticked matches a label in the table's YAML Label column.
var backticked = regexp.MustCompile("`([^`]+)`")

// ParseRunnerImages reads the labels from the runner-images README table:
// rows whose first cell names the image (with any badge) and whose third cell
// lists its labels in backticks.
func ParseRunnerImages(readme []byte) ([]RunnerLabel, error) {
	var labels []RunnerLabel
	for _, line := range strings.Split(string(readme), "\n") {
		cells := strings.Split(line, "|")
		// "| image | arch | labels | software |" splits into 6 cells.
		if len(cells) < 5 || strings.TrimSpace(cells[0]) != "" {
			continue
		}
		preview := previewBadge.MatchString(cells[1])
		for _, m := range backticked.FindAllStringSubmatch(cells[3], -1) {
			labels = append(labels, RunnerLabel{Name: m[1], Preview: preview})
		}
	}
	if len(labels) == 0 {
		return nil, &FetchResponseError{Source: "runner-images README", Detail: "no runner label table"}
	}
	return labels, nil
}

// RunnerImages fetches the runner-images README.
type RunnerImages struct {
	Client *http.Client
	URL    string
}

// Labels returns every label the README lists.
func (r RunnerImages) Labels(ctx context.Context) ([]RunnerLabel, error) {
	body, err := get(ctx, r.Client, r.URL)
	if err != nil {
		return nil, err
	}
	return ParseRunnerImages(body)
}

// JudgeRunner judges one hosted Ubuntu label against the table. The latest is
// the newest GA ubuntu-NN.04 label of the same architecture. A label the
// table does not list is a *RunnerLabelError: nothing can be said about a
// runner GitHub does not offer, and a job on it would wait forever.
func JudgeRunner(label string, table []RunnerLabel) (string, Status, error) {
	m := hostedUbuntuLabel.FindStringSubmatch(label)
	if m == nil || m[0] != label {
		return "", "", &VersionError{Kind: "hosted Ubuntu runner label", Value: label}
	}
	idx := slices.IndexFunc(table, func(l RunnerLabel) bool { return l.Name == label })
	if idx < 0 {
		return "", "", &RunnerLabelError{Label: label}
	}
	arm := m[2]
	latest := ""
	for _, l := range table {
		c := hostedUbuntuLabel.FindStringSubmatch(l.Name)
		if l.Preview || c == nil || c[0] != l.Name || c[2] != arm || !ubuntuLTS.MatchString(c[1]) {
			continue
		}
		if latest == "" || c[1] > latest {
			latest = c[1]
		}
	}
	if latest == "" {
		return "", "", &FetchResponseError{Source: "runner-images README", Detail: "no GA ubuntu-NN.04" + arm + " label"}
	}
	reported := "ubuntu-" + latest + arm
	if table[idx].Preview {
		return reported, Prerelease, nil
	}
	status, err := CompareUbuntu(m[1], latest)
	return reported, status, err
}

// RunnerItems returns one Item per distinct hosted runner label, fetching the
// table once however many labels there are.
func RunnerItems(labels []string, fetch RunnerLabelsFunc) []Item {
	var (
		table   []RunnerLabel
		err     error
		fetched bool
	)
	get := func(ctx context.Context) ([]RunnerLabel, error) {
		if !fetched {
			table, err = fetch(ctx)
			fetched = true
		}
		return table, err
	}
	var items []Item
	seen := map[string]bool{}
	for _, label := range labels {
		if seen[label] {
			continue
		}
		seen[label] = true
		items = append(items, Item{Name: "runner", Pinned: label, Check: func(ctx context.Context) (string, Status, error) {
			t, err := get(ctx)
			if err != nil {
				return "", "", err
			}
			return JudgeRunner(label, t)
		}})
	}
	slices.SortStableFunc(items, func(a, b Item) int { return strings.Compare(a.Pinned, b.Pinned) })
	return items
}

// WorkflowRunners returns every versioned hosted Ubuntu runner label in the
// workflows under root, wherever it appears: a runs-on value, a matrix
// entry, or a quoted label inside a runs-on expression.
func WorkflowRunners(root string) ([]RunnerRef, error) {
	var refs []RunnerRef
	err := walkFiles(root, func(p, name string) scanFunc {
		if !isWorkflow(p, name) {
			return nil
		}
		return func(file string, data []byte) error {
			doc, err := parseYAML(file, data)
			if err != nil || doc == nil {
				return err
			}
			eachScalar(doc, func(n *yaml.Node) {
				for _, label := range hostedUbuntuLabel.FindAllString(n.Value, -1) {
					refs = append(refs, RunnerRef{File: file, Line: n.Line, Label: label})
				}
			})
			return nil
		}
	})
	return refs, err
}

// eachScalar calls visit for every scalar value (not mapping key) under n.
func eachScalar(n *yaml.Node, visit func(*yaml.Node)) {
	switch n.Kind {
	case yaml.ScalarNode:
		visit(n)
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			eachScalar(n.Content[i], visit)
		}
	default:
		for _, c := range n.Content {
			eachScalar(c, visit)
		}
	}
}
