// Command buildtags fails when a build constraint in the module names a tag
// that no shipped build shape sets.
//
// Run it through the Makefile, which passes the shape list it also vets and
// tests:
//
//	make check-build-tags
//
// A file behind a tag no shape sets is compiled by no gate: `go vet` and
// `go test` succeed over the files their tags select and say nothing about the
// rest (#204).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jeremyhahn/go-sysmon/internal/buildtags"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, distList))
}

// distList returns the toolchain's own platform list, so a constraint on any
// real GOOS or GOARCH is accepted without this command carrying a copy that
// would drift.
func distList() ([]byte, error) {
	return exec.Command("go", "tool", "dist", "list").Output()
}

// run parses args, checks every build constraint under -root against the
// shapes and the platforms listPlatforms reports, and returns the exit
// status: 0 when every tag is set by a shape, 1 when one is not or the check
// could not run, 2 for a malformed command line.
func run(args []string, stdout, stderr io.Writer, listPlatforms func() ([]byte, error)) int {
	fs := flag.NewFlagSet("buildtags", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "module root to scan")
	shapes := fs.String("shapes", "", "whitespace-separated build shapes, each a comma-separated tag list as passed to -tags")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	n, err := check(*root, strings.Fields(*shapes), listPlatforms)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "build tags: all %d constraint tag uses are set by a build shape\n", n)
	return 0
}

// check returns how many constraint tag uses there are under root, or the
// error that says which are set by no shape.
func check(root string, shapes []string, listPlatforms func() ([]byte, error)) (int, error) {
	out, err := listPlatforms()
	if err != nil {
		return 0, &buildtags.PlatformListError{Cause: err}
	}
	platforms, err := buildtags.ParsePlatforms(out)
	if err != nil {
		return 0, err
	}
	uses, err := buildtags.Scan(root)
	if err != nil {
		return 0, err
	}
	if err := buildtags.Check(uses, buildtags.NewKnown(shapes, platforms)); err != nil {
		return 0, err
	}
	return len(uses), nil
}
