//go:build desktop

package main

import (
	"context"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// emitFrame sends one delta-protocol frame to the Wails frontend.
//
// The payload is the JSON envelope the hub already encoded, passed as a string
// so Wails hands the frontend exactly those bytes rather than re-marshalling a
// decoded copy of them -- which would undo the saving this exists for and, for
// a patch, would turn a tree of operations into an object the applier does not
// recognise.
func emitFrame(ctx context.Context, name string, payload []byte) {
	wailsruntime.EventsEmit(ctx, "sysmon:"+name, string(payload))
}
