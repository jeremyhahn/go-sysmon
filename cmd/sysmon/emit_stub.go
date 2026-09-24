//go:build !desktop

package main

import "context"

// emitFrame is a no-op in non-desktop builds. The streaming goroutine in
// bindings.go still runs so that the monitor subscription is properly drained,
// but without a Wails context there is nowhere to send frames.
func emitFrame(_ context.Context, _ string, _ []byte) {}
