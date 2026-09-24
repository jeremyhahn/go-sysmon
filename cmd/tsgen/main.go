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
	"os"

	"github.com/jeremyhahn/go-sysmon/internal/tsgen"
)

func main() {
	source := flag.String("source", "pkg/types/types.go", "Go file declaring the wire types")
	out := flag.String("out", "cmd/sysmon/frontend/src/lib", "directory to write the generated TypeScript into")
	flag.Parse()

	if err := tsgen.Generate(*source, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
