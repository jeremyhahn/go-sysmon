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
	"os"
	"os/exec"
	"strings"

	"github.com/jeremyhahn/go-sysmon/internal/buildtags"
)

func main() {
	root := flag.String("root", ".", "module root to scan")
	shapes := flag.String("shapes", "", "whitespace-separated build shapes, each a comma-separated tag list as passed to -tags")
	flag.Parse()

	if err := run(*root, strings.Fields(*shapes)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root string, shapes []string) error {
	// The toolchain's own list, so a constraint on any real GOOS or GOARCH is
	// accepted without this command carrying a copy that would drift.
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		return &buildtags.PlatformListError{Cause: err}
	}
	platforms, err := buildtags.ParsePlatforms(out)
	if err != nil {
		return err
	}

	uses, err := buildtags.Scan(root)
	if err != nil {
		return err
	}
	if err := buildtags.Check(uses, buildtags.NewKnown(shapes, platforms)); err != nil {
		return err
	}
	fmt.Printf("build tags: all %d constraint tag uses are set by a build shape\n", len(uses))
	return nil
}
