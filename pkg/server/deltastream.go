package server

import (
	"context"
	"log/slog"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
)

// DeltaStream is the delta protocol without HTTP.
//
// The browser gets this stream over server-sent events; the desktop window gets
// it over the Wails event bridge. Both need the same thing -- one whole
// snapshot, then patches, with a sequence number and the rule that a patch is
// only ever sent to a client that has the frame before it -- and only one of
// them needs a socket.
//
// This exists because the desktop did not have it. It emitted the entire
// snapshot on every tick: measured on a ten-node fleet host, 367,126 bytes per
// frame against a delta averaging 7,842 -- 47 times the bytes, for identical
// information, on a machine that already had a correct delta hub it never
// called (#98).
type DeltaStream struct {
	hub *deltaHub
}

// NewDeltaStream returns a stream over mon's snapshots. Nothing runs until Run
// is called: a stream nobody is watching costs a goroutine and a diff per tick,
// and the desktop window can be closed.
func NewDeltaStream(mon *monitor.Monitor, log *slog.Logger) *DeltaStream {
	return &DeltaStream{hub: newDeltaHub(mon, log)}
}

// Emit delivers one frame. name is "full" for a whole snapshot or "delta" for a
// patch against the frame before it; payload is the JSON envelope, already
// encoded, carrying the sequence number.
type Emit func(name string, payload []byte)

// Run subscribes and delivers frames through emit until ctx is cancelled or the
// monitor stops. It blocks, so callers run it in a goroutine.
//
// A caller that stops consuming must cancel ctx: the subscription is what keeps
// the hub's publishing loop alive, and an abandoned Run would keep diffing for
// a window nobody is looking at.
func (d *DeltaStream) Run(ctx context.Context, emit Emit) {
	if emit == nil {
		return
	}
	client := newClientStream(d.hub)
	defer client.close()

	// The frame already published, so a window opening between ticks is not
	// blank until the next one.
	if name, payload, ok := client.initial(); ok {
		emit(name, payload)
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-client.done():
			// The monitor stopped. Nothing further is emitted: the last frame
			// delivered is the last measurement that happened, and inventing a
			// closing frame would put a reading on screen that no collection
			// produced.
			return

		case frame := <-client.frames():
			// A closed subscription wins over a queued frame, exactly as it
			// does on the socket: both can be ready at once and Go picks
			// between them at random, so without this a frame published before
			// the stop could be rendered after the stream was already over.
			if client.ended() {
				return
			}
			name, payload, skip := client.next(frame)
			if skip {
				continue
			}
			emit(name, payload)
		}
	}
}

// Shutdown stops the publishing loop and releases every subscriber.
func (d *DeltaStream) Shutdown() { d.hub.shutdown() }
