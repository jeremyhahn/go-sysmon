package pins

import (
	"context"
	"strings"
)

// Tool is one dev tool pinned to a module version.
type Tool struct {
	// Name is how the tool is reported, "golangci-lint".
	Name string
	// Module is the module the tool is built from, which is what has
	// releases: "github.com/golangci/golangci-lint/v2".
	Module string
	// Version is the pinned module version, "v2.13.2".
	Version string
}

// ParseTool parses the "name=module@version" form the Makefile passes.
func ParseTool(s string) (Tool, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return Tool{}, &ToolSpecError{Spec: s}
	}
	module, ver, ok := strings.Cut(rest, "@")
	if !ok || module == "" || ver == "" {
		return Tool{}, &ToolSpecError{Spec: s}
	}
	return Tool{Name: name, Module: module, Version: ver}, nil
}

// Result is the verdict on one pinned item.
type Result struct {
	Name   string
	Pinned string
	Latest string
	Status Status
}

// String renders a result as one report line.
func (r Result) String() string {
	s := r.Name + " " + r.Pinned + ": " + string(r.Status)
	if r.Status != Current {
		s += " (latest stable is " + r.Latest + ")"
	}
	return s
}

// Report is the verdict on every item a check reached.
type Report struct {
	Results []Result
}

// Lagging returns the results that are not Current.
func (r Report) Lagging() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status != Current {
			out = append(out, res)
		}
	}
	return out
}

// Err returns a *LagError when any item is not at its latest stable release,
// and nil otherwise.
func (r Report) Err() error {
	if lag := r.Lagging(); len(lag) > 0 {
		return &LagError{Results: lag}
	}
	return nil
}

// LatestFunc returns the latest stable release of one thing.
type LatestFunc func(ctx context.Context) (string, error)

// ModuleLatestFunc returns the latest version of a module.
type ModuleLatestFunc func(ctx context.Context, module string) (string, error)

// Item is one pinned thing to judge against its latest stable release.
type Item struct {
	Name   string
	Pinned string
	// Check fetches the latest stable release and judges the pin against it.
	// An error means no verdict.
	Check func(ctx context.Context) (latest string, status Status, err error)
}

// versionItem is an Item whose latest release is one fetched string, judged by
// compare.
func versionItem(name, pinned string, latest LatestFunc, compare func(pinned, latest string) (Status, error)) Item {
	return Item{Name: name, Pinned: pinned, Check: func(ctx context.Context) (string, Status, error) {
		l, err := latest(ctx)
		if err != nil {
			return "", "", err
		}
		status, err := compare(pinned, l)
		return l, status, err
	}}
}

// GoItem judges the go directive against the latest stable Go release.
func GoItem(directive string, latest LatestFunc) Item {
	return versionItem(GoName, directive, latest, CompareGo)
}

// ToolItem judges a tool's pinned module version against the module's latest.
func ToolItem(t Tool, latest ModuleLatestFunc) Item {
	return versionItem(t.Name, t.Version, func(ctx context.Context) (string, error) {
		return latest(ctx, t.Module)
	}, CompareModule)
}

// NodeItem judges the pinned Node.js release against the latest Active LTS
// release.
func NodeItem(pinned string, latestLTS LatestFunc) Item {
	return versionItem(NodeName, pinned, latestLTS, CompareRelease)
}

// UbuntuItem judges the pinned Ubuntu release against the latest LTS.
func UbuntuItem(pinned string, latestLTS LatestFunc) Item {
	return versionItem(UbuntuName, pinned, latestLTS, CompareUbuntu)
}

// Names items are reported under.
const (
	GoName     = "go"
	NodeName   = "node"
	DebianName = "debian"
	UbuntuName = "ubuntu"
)

// CheckLatest judges every item. The Report holds a verdict for each item that
// reached one. When any did not, the error is an *UncheckedError naming them:
// an item with no verdict is never reported as current.
func CheckLatest(ctx context.Context, items []Item) (Report, error) {
	var report Report
	var failures []*ItemError
	for _, it := range items {
		latest, status, err := it.Check(ctx)
		if err != nil {
			failures = append(failures, &ItemError{Item: it.Name, Cause: err})
			continue
		}
		report.Results = append(report.Results, Result{Name: it.Name, Pinned: it.Pinned, Latest: latest, Status: status})
	}
	if len(failures) > 0 {
		return report, &UncheckedError{Failures: failures}
	}
	return report, nil
}
