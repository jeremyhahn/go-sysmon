// Command checkpins fails when any version reference in the repository
// disagrees with the pin it repeats. It is offline, and part of `make lint`:
//
//	make check-pins
//
// Each version is set in one place: Go in go.mod, Node.js in .node-version,
// the Debian and Ubuntu base releases and the GitHub-hosted runner release in
// the Makefile. Everything else reads
// that source (setup-go's go-version-file, setup-node's node-version-file) or
// repeats it, and a repeated version drifts silently: the image builds on one
// release while CI tests another (#183).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jeremyhahn/go-sysmon/internal/pins"
)

func main() {
	root := flag.String("root", ".", "repository root to scan")
	nodeFile := flag.String("node-file", ".node-version", "file under root holding the Node.js release")
	debian := flag.String("debian", "", "Debian release codename every Debian-based image uses")
	ubuntu := flag.String("ubuntu", "", "Ubuntu release every Ubuntu image uses")
	hostedRunner := flag.String("hosted-runner", "", "Ubuntu release of every versioned GitHub-hosted runner label")
	flag.Parse()
	os.Exit(run(*root, *nodeFile, *debian, *ubuntu, *hostedRunner, os.Stdout, os.Stderr))
}

// run checks the pins under root and returns the exit status: 0 when every
// reference agrees with its pin, 1 otherwise.
func run(root, nodeFile, debian, ubuntu, hostedRunner string, stdout, stderr io.Writer) int {
	p, err := pins.ReadPins(root, nodeFile, debian, ubuntu, hostedRunner)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	n, err := pins.CheckAgreement(root, p)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "pins: all %d version references agree (go %s, node %s, debian %s, ubuntu %s, hosted runners %s)\n",
		n, p.Go, p.Node, p.Debian, p.Ubuntu, p.HostedRunner)
	return 0
}
