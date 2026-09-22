package fleet

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// bodyBuffers keeps one read buffer per peer between aggregation cycles.
//
// Peers answer /api/snapshot chunked, with no Content-Length, so a reader that
// starts empty every cycle grows its buffer by doubling until the whole body
// fits: for a 700 KB snapshot that is a dozen allocations and copies adding up
// to several times the body, and every byte of it is garbage three seconds
// later. Measured on the deployed fleet's real snapshots, that growth was more
// than half of everything an aggregation cycle allocated (#76).
//
// A peer's snapshot is about the same size from one poll to the next, so the
// buffer that held the last one holds the next one without growing. Keeping it
// per peer, rather than in a shared sync.Pool, matters twice over: a pool is
// emptied by the garbage collector, which on the aggregator runs about once per
// cycle, and a pooled buffer sized for a 60 KB peer handed to a 700 KB one
// grows anyway.
//
// A buffer is taken out of the map while a fetch uses it and put back after, so
// two fetches of the same peer can never share one: the second simply gets a
// fresh buffer. The map is a sync.Map because each key is written by one
// goroutine at a time and read by nobody else -- the disjoint-key case it is
// built for -- and it keeps the poll goroutines off a shared lock.
type bodyBuffers struct {
	m sync.Map // peer name -> *bytes.Buffer
}

// maxRetainedRatio bounds how much larger than the body it last held a buffer
// may be and still be kept. A peer whose snapshot shrinks -- its process count
// falls, or it is replaced by a smaller machine under the same name -- would
// otherwise pin its largest-ever buffer for the life of the process. Dropping
// it costs one cycle of regrowth, sized to what the peer sends now.
const maxRetainedRatio = 4

// decode reads body in full into the buffer kept for peer, bounded by limit,
// and unmarshals it into v.
//
// The buffer is held for the whole of the decode and only then put back, so it
// cannot be handed to another fetch while its bytes are still being read. The
// decoded value does not alias it: json.Unmarshal copies every string it
// stores.
//
// Unmarshal rather than a json.Decoder over the body is what lets the buffer be
// reused at all -- a Decoder reads into a buffer of its own and grows it by
// doubling, which is the cost this type exists to remove.
//
// The buffer goes back to the map whether or not the read succeeded, subject to
// the same size rule as any other: a read cut short by a timeout leaves little
// in it and it is dropped, which costs that peer one cycle of regrowth.
func (b *bodyBuffers) decode(peer string, body io.Reader, limit int64, v any) error {
	buf := b.take(peer)
	defer b.keep(peer, buf)

	if _, err := buf.ReadFrom(io.LimitReader(body, limit)); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), v)
}

// take removes and returns peer's buffer, emptied, or a new one.
func (b *bodyBuffers) take(peer string) *bytes.Buffer {
	if v, ok := b.m.LoadAndDelete(peer); ok {
		buf := v.(*bytes.Buffer)
		buf.Reset()
		return buf
	}
	return new(bytes.Buffer)
}

// keep stores buf as peer's buffer unless it has become disproportionately
// larger than what it holds.
func (b *bodyBuffers) keep(peer string, buf *bytes.Buffer) {
	if buf.Cap() > maxRetainedRatio*buf.Len() {
		return
	}
	b.m.Store(peer, buf)
}

// forget drops the buffers of peers that are no longer configured. A nil keep
// drops them all.
func (b *bodyBuffers) forget(keep map[string]bool) {
	b.m.Range(func(k, _ any) bool {
		if name := k.(string); !keep[name] {
			b.m.Delete(name)
		}
		return true
	})
}
