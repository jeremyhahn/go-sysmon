//go:build !desktop

package main

import (
	"context"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// emitSnapshot is a no-op in non-desktop builds. The streaming goroutine in
// bindings.go still runs so that the monitor subscription is properly drained,
// but without a Wails context there is nowhere to send events.
func emitSnapshot(_ context.Context, _ *types.Snapshot) {}
