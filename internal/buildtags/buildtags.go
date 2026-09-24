// Package buildtags finds every build tag the module's Go source is
// constrained on and reports any that no build shape the project ships sets.
//
// A tag that nothing sets is a file that nothing compiles. `go vet` and
// `go test` report success over whatever the tags they were given select, so a
// file behind an unlisted tag -- or behind a listed tag no target passes --
// can stop compiling without any gate noticing. That is how a desktop-only
// test break went unseen (#204). The Makefile vets and tests every shape it
// lists; this package is what keeps that list complete.
package buildtags

import (
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Use is one tag named by one build constraint.
type Use struct {
	Tag  string
	File string
	Line int
}

// Scan returns every tag named by a build constraint in the Go files under
// root, in file and line order.
//
// It walks root the way the go command expands root/...: directories named
// testdata or vendor, directories and files whose names begin with "." or "_",
// and nested modules are not part of the module's packages and are skipped.
// Both the //go:build and the legacy // +build forms are read.
func Scan(root string) ([]Use, error) {
	var uses []Use
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return &SourceError{Path: path, Cause: walkErr}
		}
		if d.IsDir() {
			if path != root && skipDir(path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isGoSource(d.Name()) {
			return nil
		}
		fileUses, err := scanFile(path)
		if err != nil {
			return err
		}
		uses = append(uses, fileUses...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return uses, nil
}

// skipDir reports whether the go command leaves dir out of a ./... pattern.
func skipDir(path, name string) bool {
	if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}

// isGoSource reports whether the go command would consider the file at all.
func isGoSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// scanFile returns the tags named by the build constraints in one file's
// header, which is the only place the go command reads them.
func scanFile(path string) ([]Use, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return nil, &SourceError{Path: path, Cause: err}
	}

	var uses []Use
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) && !constraint.IsPlusBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				return nil, &SourceError{Path: path, Cause: err}
			}
			line := fset.Position(c.Slash).Line
			for _, tag := range tagsOf(expr) {
				uses = append(uses, Use{Tag: tag, File: path, Line: line})
			}
		}
	}
	return uses, nil
}

// tagsOf returns the distinct tags an expression names, in order of first
// appearance. Eval visits both sides of every && and ||, precisely so a
// callback observes every tag, which makes it the parser's own tag walker.
func tagsOf(expr constraint.Expr) []string {
	seen := make(map[string]bool)
	var tags []string
	expr.Eval(func(tag string) bool {
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
		return true
	})
	return tags
}

// Known is the set of tags a build constraint may name: the ones a shipped
// shape sets, and the ones the toolchain sets itself.
type Known map[string]bool

// NewKnown returns the tags the given shapes set, plus every tag the go
// toolchain sets on its own: each GOOS and GOARCH, "unix", "cgo", "gc",
// "gccgo", the release tags (go1.N) and the running toolchain's tool tags.
//
// Each shape is one comma-separated tag list, as passed to -tags.
func NewKnown(shapes []string, platforms Platforms) Known {
	known := make(Known)
	for _, shape := range shapes {
		for _, tag := range strings.Split(shape, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				known[tag] = true
			}
		}
	}
	for _, name := range platforms.OS {
		known[name] = true
	}
	for _, name := range platforms.Arch {
		known[name] = true
	}
	for _, tag := range []string{"unix", "cgo", "gc", "gccgo"} {
		known[tag] = true
	}
	for _, tag := range build.Default.ReleaseTags {
		known[tag] = true
	}
	for _, tag := range build.Default.ToolTags {
		known[tag] = true
	}
	return known
}

// Check returns an *UnknownTagError listing every use of a tag that is not
// known, or nil when there is none.
func Check(uses []Use, known Known) error {
	var unknown []Use
	for _, u := range uses {
		if !known[u.Tag] {
			unknown = append(unknown, u)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return &UnknownTagError{Uses: unknown}
}

// Platforms is the set of operating systems and architectures a toolchain can
// target, as `go tool dist list` prints them.
type Platforms struct {
	OS   []string
	Arch []string
}

// ParsePlatforms reads the output of `go tool dist list`: one GOOS/GOARCH pair
// per line.
func ParsePlatforms(out []byte) (Platforms, error) {
	oses := make(map[string]bool)
	arches := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		goos, goarch, ok := strings.Cut(line, "/")
		if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
			return Platforms{}, &PlatformListError{Line: line}
		}
		oses[goos] = true
		arches[goarch] = true
	}
	if len(oses) == 0 {
		return Platforms{}, &PlatformListError{}
	}
	return Platforms{OS: sortedKeys(oses), Arch: sortedKeys(arches)}, nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
