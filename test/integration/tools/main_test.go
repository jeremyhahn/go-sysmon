//go:build integration

// Package tools runs the development tools as `make` does -- checkpins,
// checklatest, buildtags and tsgen -- against this repository and against
// synthetic ones. Every service checklatest reaches is an in-process stand-in:
// the release documents, a Go module proxy, git mirrors of the action
// repositories and the Gitea API.
package tools

import (
	"os"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
