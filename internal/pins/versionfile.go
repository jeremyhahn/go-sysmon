package pins

import (
	"os"
	"path/filepath"
	"strings"
)

// ReadVersionFile reads the exact release in a one-line version file such as
// .node-version, the form actions/setup-node's node-version-file reads. A
// leading "v" is accepted and dropped: "v24.21.0" and "24.21.0" are the same
// release. Anything but one exact stable release is an error, since every
// other reference is compared with it.
func ReadVersionFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", &PinFileError{Path: path, Reason: "cannot read", Cause: err}
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(data)), "v")
	if !IsExactRelease(v) {
		return "", &PinFileError{Path: path, Reason: "holds " + quoteShort(v) + ", not one exact stable release such as 24.21.0"}
	}
	return v, nil
}

// ReadPins reads the pins that live in files under root -- the go and
// toolchain directives from go.mod, and Node.js from nodeFile -- and takes
// the Debian, Ubuntu and hosted runner releases as the Makefile passes them.
func ReadPins(root, nodeFile, debian, ubuntu, hostedRunner string) (Pins, error) {
	mod, err := ReadModFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return Pins{}, err
	}
	node, err := ReadVersionFile(filepath.Join(root, filepath.FromSlash(nodeFile)))
	if err != nil {
		return Pins{}, err
	}
	return Pins{
		Go:        mod.Go,
		Toolchain: mod.Toolchain,
		Node:      node,
		NodeFile:  nodeFile,
		Debian:    debian,
		Ubuntu:    ubuntu,

		HostedRunner: hostedRunner,
	}, nil
}
