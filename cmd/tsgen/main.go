// Command tsgen regenerates the dashboard's TypeScript wire types and the
// runtime schema they are validated against, from pkg/types/types.go.
//
// Run it through the Makefile rather than by hand:
//
//	make generate
//
// CI runs the same target and fails if the working tree moves, so a change to
// a Go wire type that is not regenerated is caught before it merges instead of
// showing up later as a panel rendering "undefined".
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jeremyhahn/go-sysmon/internal/tsgen"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run parses args and generates the TypeScript, returning the exit status: 0
// when it was written, 1 when generation failed, 2 for a malformed command
// line.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("tsgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "pkg/types/types.go", "Go file declaring the wire types")
	out := fs.String("out", "cmd/sysmon/frontend/src/lib", "directory to write the generated TypeScript into")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := tsgen.Generate(*source, *out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
