//go:build integration

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestEvents_SnapshotStream verifies the original protocol: a reconnect hint,
// then one whole snapshot per collection, each a decodable Snapshot, with or
// without compression.
func TestEvents_SnapshotStream(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	for _, gzip := range []bool{false, true} {
		st := openStream(t, s.URL("/api/events"), streamOptions{gzip: gzip})
		if got := st.header.Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", got)
		}
		if gzip != (st.header.Get("Content-Encoding") == "gzip") {
			t.Errorf("gzip offered %v, Content-Encoding %q", gzip, st.header.Get("Content-Encoding"))
		}
		var previous types.Snapshot
		for i := range 3 {
			ev := st.next(t)
			if ev.name != "snapshot" {
				t.Fatalf("event %d = %q, want snapshot", i, ev.name)
			}
			var snap types.Snapshot
			harness.MustParseJSON(t, ev.data, &snap)
			if i > 0 && !snap.Timestamp.After(previous.Timestamp) {
				t.Errorf("snapshot %d is dated %v, not after the previous %v", i, snap.Timestamp, previous.Timestamp)
			}
			previous = snap
		}
		if st.retry != "3000" {
			t.Errorf("retry = %q, want the advertised 3000ms", st.retry)
		}
	}
}

// deltaClient applies the delta protocol the way the dashboard does, and
// checks every frame it is given can be applied.
type deltaClient struct {
	t    *testing.T
	st   *stream
	seq  uint64
	tree delta.Tree
	// fulls counts whole snapshots, and deltas patches.
	fulls, deltas int
}

// envelope is one frame of the delta protocol.
type envelope struct {
	Seq      uint64          `json:"seq"`
	Snapshot json.RawMessage `json:"snapshot"`
	Patch    *delta.Patch    `json:"patch"`
}

// step reads and applies one frame, returning the tree it leaves.
func (c *deltaClient) step() delta.Tree {
	c.t.Helper()
	ev := c.st.next(c.t)
	var env envelope
	harness.MustParseJSON(c.t, ev.data, &env)
	switch ev.name {
	case "full":
		tree, err := delta.Decode(env.Snapshot)
		if err != nil {
			c.t.Fatalf("full frame %d does not decode: %v", env.Seq, err)
		}
		c.tree = tree
		c.fulls++
	case "delta":
		// A patch is only ever sent against the frame before it: a client
		// that missed one is sent a whole snapshot instead.
		if env.Seq != c.seq+1 {
			c.t.Fatalf("delta frame %d follows frame %d: a patch against a base this client does not hold", env.Seq, c.seq)
		}
		if env.Patch != nil {
			tree, err := delta.Apply(c.tree, env.Patch)
			if err != nil {
				c.t.Fatalf("delta frame %d does not apply: %v", env.Seq, err)
			}
			c.tree = tree
		}
		c.deltas++
	default:
		c.t.Fatalf("event %q on the delta stream", ev.name)
	}
	c.seq = env.Seq
	return c.tree
}

// timestampOf returns a decoded snapshot's timestamp as sent.
func timestampOf(t *testing.T, tree delta.Tree) string {
	t.Helper()
	obj, ok := tree.(map[string]any)
	if !ok {
		t.Fatalf("a snapshot that is not an object: %T", tree)
	}
	ts, _ := obj["timestamp"].(string)
	return ts
}

// parseTime reads a snapshot timestamp.
func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("snapshot timestamp %q: %v", s, err)
	}
	return at
}

// TestEvents_DeltasReconstructTheSnapshotsExactly runs the delta protocol end
// to end: a client applying every frame with pkg/delta must hold, collection
// by collection, exactly the snapshot a whole-snapshot client is sent for the
// same collection.
func TestEvents_DeltasReconstructTheSnapshotsExactly(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	whole := openStream(t, s.URL("/api/events"), streamOptions{})
	c := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{gzip: true})}

	// Each whole snapshot is read until one dated at or after the collection
	// the delta client now holds. A subscriber that fell a collection behind
	// can skip one, so a collection seen only by one side is passed over
	// rather than waited for.
	var latestWhole delta.Tree
	var latestTime time.Time
	matched := 0
	for matched < 5 {
		tree := c.step()
		at := parseTime(t, timestampOf(t, tree))
		for latestWhole == nil || latestTime.Before(at) {
			ev := whole.next(t)
			w, err := delta.Decode([]byte(ev.data))
			if err != nil {
				t.Fatalf("whole snapshot does not decode: %v", err)
			}
			latestWhole, latestTime = w, parseTime(t, timestampOf(t, w))
		}
		if !latestTime.Equal(at) {
			continue
		}
		if !reflect.DeepEqual(tree, latestWhole) {
			t.Fatalf("after frame %d the reconstructed snapshot differs from the one sent whole for %v", c.seq, at)
		}
		matched++
	}
	if c.fulls == 0 || c.deltas == 0 {
		t.Errorf("saw %d full and %d delta frames, want both", c.fulls, c.deltas)
	}
}

// TestEvents_DeltaStreamResyncsPeriodically verifies the whole snapshot sent
// every 60th frame, which bounds how long a client that somehow diverged can
// stay wrong.
func TestEvents_DeltaStreamResyncsPeriodically(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	c := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{})}
	c.step()
	for c.seq%60 != 0 {
		c.step()
	}
	if c.fulls < 2 {
		t.Errorf("frame %d arrived as a patch; every 60th frame is a whole snapshot", c.seq)
	}
}

// TestEvents_ASlowClientStallsNoOne verifies a client that stops reading for
// a while holds up nobody else, and when it resumes is never handed a patch
// against a frame it missed.
func TestEvents_ASlowClientStallsNoOne(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	slow := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{rcvbuf: 4096})}
	slow.step()
	brisk := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{})}
	brisk.step()
	start := brisk.seq
	for brisk.seq < start+10 {
		brisk.step()
	}
	// The slow client resumes: each frame it now reads applies, whatever it
	// missed while it was not reading.
	for range 10 {
		slow.step()
	}
}

// TestEvents_ShutdownSaysGoodbyeOnEveryOpenStream verifies a SIGTERM ends
// every open stream, in both protocols, with a bye that tells the dashboard
// not to reconnect to a service that is going away, and that the service
// then exits cleanly.
func TestEvents_ShutdownSaysGoodbyeOnEveryOpenStream(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	streams := map[string]*stream{
		"snapshot": openStream(t, s.URL("/api/events"), streamOptions{}),
		"delta":    openStream(t, s.URL("/api/events?deltas=1"), streamOptions{gzip: true}),
	}
	for _, st := range streams {
		st.next(t)
	}

	s.Signal(t, syscall.SIGTERM)

	for name, st := range streams {
		// Collections already in flight may come first; the bye follows them.
		ev := st.next(t)
		for i := 0; ev.name != "bye" && i < 10; i++ {
			ev = st.next(t)
		}
		if ev.name != "bye" {
			t.Errorf("%s stream ended with %q, want bye", name, ev.name)
		}
		if r := st.nextResult(t); !errors.Is(r.err, io.EOF) && !errors.Is(r.err, io.ErrUnexpectedEOF) {
			t.Errorf("%s stream after bye: %+v, want the stream to end", name, r)
		}
	}
	if err := s.Wait(t); err != nil {
		t.Errorf("the server on SIGTERM with streams open: %v, want a clean exit", err)
	}
}

// hasProcess reports whether a decoded snapshot lists pid.
func hasProcess(t *testing.T, tree delta.Tree, pid int) bool {
	t.Helper()
	want := fmt.Sprint(pid)
	for _, list := range arraysAt(tree, []string{"processes", "process_list"}) {
		for _, element := range list {
			if obj, ok := element.(map[string]any); ok && fmt.Sprint(obj["pid"]) == want {
				return true
			}
		}
	}
	return false
}

// TestEvents_AProcessComingAndGoingIsReconstructedExactly starts a real
// process and ends it while a delta client and a whole-snapshot client
// watch. The keyed process list gains the process as a new element, then
// loses it, and at every collection both clients seen hold the same snapshot:
// a membership change is carried by the patch, never lost or doubled.
func TestEvents_AProcessComingAndGoingIsReconstructedExactly(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	whole := openStream(t, s.URL("/api/events"), streamOptions{})
	c := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{})}
	var latestWhole delta.Tree
	var latestTime time.Time
	// step applies one frame and checks it against the whole snapshot of the
	// same collection, when the whole-snapshot client was sent it.
	step := func() delta.Tree {
		tree := c.step()
		at := parseTime(t, timestampOf(t, tree))
		for latestWhole == nil || latestTime.Before(at) {
			w, err := delta.Decode([]byte(whole.next(t).data))
			if err != nil {
				t.Fatalf("whole snapshot does not decode: %v", err)
			}
			latestWhole, latestTime = w, parseTime(t, timestampOf(t, w))
		}
		if latestTime.Equal(at) && !reflect.DeepEqual(tree, latestWhole) {
			t.Fatalf("after frame %d the reconstruction differs from the whole snapshot", c.seq)
		}
		return tree
	}
	step()

	proc := exec.Command("sleep", "3600")
	if err := proc.Start(); err != nil {
		t.Fatalf("start a process: %v", err)
	}
	pid := proc.Process.Pid
	// Bounded by frames rather than time: a few collections at most, since the
	// process list is refreshed on the medium tier.
	const maxFrames = 100
	for n := 0; !hasProcess(t, step(), pid); n++ {
		if n == maxFrames {
			t.Fatalf("process %d never appeared in %d frames", pid, maxFrames)
		}
	}

	if err := proc.Process.Kill(); err != nil {
		t.Fatalf("end the process: %v", err)
	}
	var exited *exec.ExitError
	if err := proc.Wait(); !errors.As(err, &exited) {
		t.Fatalf("reap the process: %v", err)
	}
	for n := 0; hasProcess(t, step(), pid); n++ {
		if n == maxFrames {
			t.Fatalf("process %d was still listed %d frames after it ended", pid, maxFrames)
		}
	}
}

// TestEvents_TheHostsKeyedListsHaveUniqueIdentities checks the delta
// protocol's identity table against what this real host reports. A key that
// repeated in a real snapshot -- two interfaces or two processes under one
// identity -- would merge two elements into one on every client.
func TestEvents_TheHostsKeyedListsHaveUniqueIdentities(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	_, body := harness.Get(t, s.URL("/api/snapshot"))
	tree, err := delta.Decode(body)
	if err != nil {
		t.Fatalf("snapshot does not decode: %v", err)
	}
	for path, key := range delta.ArrayKeys() {
		for _, array := range arraysAt(tree, strings.Split(path, ".")) {
			seen := make(map[string]bool, len(array))
			for _, element := range array {
				obj, ok := element.(map[string]any)
				if !ok {
					t.Fatalf("%s holds an element that is not an object", path)
				}
				id := fmt.Sprint(obj[key])
				if seen[id] {
					t.Errorf("%s has two elements with %s %s on this host", path, key, id)
				}
				seen[id] = true
			}
		}
	}
}

// arraysAt returns every array a dotted path names, stepping into arrays at
// each "[]".
func arraysAt(node any, parts []string) [][]any {
	if len(parts) == 0 {
		if array, ok := node.([]any); ok {
			return [][]any{array}
		}
		return nil
	}
	name, through := strings.CutSuffix(parts[0], "[]")
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	child := obj[name]
	if !through {
		return arraysAt(child, parts[1:])
	}
	array, _ := child.([]any)
	var out [][]any
	for _, element := range array {
		out = append(out, arraysAt(element, parts[1:])...)
	}
	return out
}
