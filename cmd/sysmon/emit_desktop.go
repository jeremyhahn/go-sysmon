//go:build desktop

package main

import (
	"context"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// emitSnapshot sends a snapshot to the Wails frontend via the
// "sysmon:snapshot" event channel.
//
// Used for the one-off reply to GetSnapshot's callers; the live stream goes
// through emitFrame below.
func emitSnapshot(ctx context.Context, snapshot *types.Snapshot) {
	wailsruntime.EventsEmit(ctx, "sysmon:snapshot", snapshot)
}

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
