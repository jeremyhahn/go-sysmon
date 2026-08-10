//go:build desktop

package main

import (
	"context"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// emitSnapshot sends a snapshot to the Wails frontend via the
// "sysmon:snapshot" event channel.
func emitSnapshot(ctx context.Context, snapshot *types.Snapshot) {
	wailsruntime.EventsEmit(ctx, "sysmon:snapshot", snapshot)
}
