package config

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// record encodes one inotify event the way the kernel writes it: wd, mask,
// cookie and len in native byte order, then the name padded with NULs to len.
func record(wd int32, mask uint32, name string, padTo int) []byte {
	buf := make([]byte, unix.SizeofInotifyEvent+padTo)
	binary.NativeEndian.PutUint32(buf[0:], uint32(wd))
	binary.NativeEndian.PutUint32(buf[4:], mask)
	binary.NativeEndian.PutUint32(buf[12:], uint32(padTo))
	copy(buf[unix.SizeofInotifyEvent:], name)
	return buf
}

func TestParseEvents_DecodesEveryRecord(t *testing.T) {
	var buf []byte
	buf = append(buf, record(1, unix.IN_MODIFY, "sysmon.yaml", 16)...)
	buf = append(buf, record(2, unix.IN_MOVED_TO, "..data", 16)...)
	buf = append(buf, record(-1, unix.IN_Q_OVERFLOW, "", 0)...)

	want := []rawEvent{
		{wd: 1, mask: unix.IN_MODIFY, name: "sysmon.yaml"},
		{wd: 2, mask: unix.IN_MOVED_TO, name: "..data"},
		{wd: -1, mask: unix.IN_Q_OVERFLOW},
	}
	if got := parseEvents(buf); !slices.Equal(got, want) {
		t.Errorf("parseEvents() = %v, want %v", got, want)
	}
}

// A record whose name runs past the buffer is not an event. The kernel never
// writes one; the parser must stop rather than read out of bounds.
func TestParseEvents_TruncatedRecordIsDropped(t *testing.T) {
	whole := record(1, unix.IN_CLOSE_WRITE, "sysmon.yaml", 16)
	buf := append(slices.Clone(whole), whole[:unix.SizeofInotifyEvent+4]...)
	if got := parseEvents(buf); len(got) != 1 || got[0].name != "sysmon.yaml" {
		t.Errorf("parseEvents() with a trailing short record = %v, want only the whole one", got)
	}
	if got := parseEvents(whole[:unix.SizeofInotifyEvent-1]); len(got) != 0 {
		t.Errorf("parseEvents() of a partial header = %v, want none", got)
	}
}

// The NUL padding is trimmed, so a name that shares a prefix with another is
// still its own name.
func TestEntryName_TrimsPaddingOnly(t *testing.T) {
	for field, want := range map[string]string{
		"sysmon.yaml\x00\x00\x00": "sysmon.yaml",
		"sysmon.yaml.swp\x00":     "sysmon.yaml.swp",
		"unpadded":                "unpadded",
		"":                        "",
	} {
		if got := entryName([]byte(field)); got != want {
			t.Errorf("entryName(%q) = %q, want %q", field, got, want)
		}
	}
}

func TestWatchState_HoldsWhileWritingAndArmsWhenSettled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var s watchState

	steps := []struct {
		ev      dirEvent
		want    reloadAction
		writing bool
	}{
		{eventWritten, reloadHold, true},
		{eventWritten, reloadHold, true},
		{eventClosed, reloadArm, false},
		{eventWritten, reloadHold, true},
		{eventReplaced, reloadArm, false},
		{eventWritten, reloadHold, true},
		{eventRemoved, reloadArm, false},
		{eventWritten, reloadHold, true},
		// A link on the path now leads to another file: whatever write was
		// under way was to the file it no longer leads to.
		{eventRelinked, reloadArm, false},
		{eventWritten, reloadHold, true},
		// Lost events leave the write state unknown; the reload converges on
		// the file as it now is.
		{eventOverflow, reloadArm, false},
		{eventNone, reloadKeep, false},
	}
	for i, step := range steps {
		if got := s.on(step.ev, logger); got != step.want {
			t.Errorf("step %d: on(%v) = %v, want %v", i, step.ev, got, step.want)
		}
		if s.writing != step.writing {
			t.Errorf("step %d: after %v writing = %v, want %v", i, step.ev, s.writing, step.writing)
		}
	}
}

func TestClassifyEntry_ByRole(t *testing.T) {
	cases := []struct {
		role entryRole
		mask uint32
		want dirEvent
	}{
		{roleFile, unix.IN_MODIFY, eventWritten},
		{roleFile, unix.IN_CLOSE_WRITE, eventClosed},
		{roleFile, unix.IN_CREATE, eventReplaced},
		{roleFile, unix.IN_MOVED_TO, eventReplaced},
		{roleFile, unix.IN_DELETE, eventRemoved},
		{roleFile, unix.IN_MOVED_FROM, eventRemoved},
		{roleLink, unix.IN_MOVED_TO, eventRelinked},
		{roleLink, unix.IN_CREATE, eventRelinked},
		{roleLink, unix.IN_DELETE, eventRelinked},
		{roleLink, unix.IN_MOVED_FROM, eventRelinked},
		// A symlink is never written in place, and a directory reached
		// through one being written to says nothing about the path.
		{roleLink, unix.IN_MODIFY, eventNone},
		{roleLink, unix.IN_CLOSE_WRITE, eventNone},
		// An entry that is not on the path.
		{0, unix.IN_CLOSE_WRITE, eventNone},
	}
	for _, c := range cases {
		if got := classifyEntry(c.role, c.mask); got != c.want {
			t.Errorf("classifyEntry(%v, %#x) = %v, want %v", c.role, c.mask, got, c.want)
		}
	}
}

func TestInotifyAdd_RefusesAPathThatIsNotADirectory(t *testing.T) {
	in, err := newInotify()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := in.close(); err != nil {
			t.Error(err)
		}
	}()

	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, link} {
		if _, err := in.add(path); err == nil {
			t.Errorf("add(%s) succeeded, want IN_ONLYDIR|IN_DONT_FOLLOW to refuse it", path)
		} else if !changedUnderfoot(err) {
			t.Errorf("add(%s) = %v, want an error refresh treats as the path having changed", path, err)
		}
	}
}

// Removing a watch the kernel already dropped -- its directory was deleted --
// reaches the state asked for, so it is not an error.
func TestInotifyRemove_AlreadyDroppedWatch(t *testing.T) {
	in, err := newInotify()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := in.close(); err != nil {
			t.Error(err)
		}
	}()
	dir := t.TempDir()
	wd, err := in.add(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.remove(wd); err != nil {
		t.Fatalf("remove() = %v", err)
	}
	if err := in.remove(wd); err != nil {
		t.Errorf("remove() of an already removed watch = %v, want nil", err)
	}
}

// TestWatchLoop_ReadFailureIsReportedAndStops covers the watch's descriptor
// failing on read. The loop must say at warning level that changes are no
// longer reloaded and stop, rather than spin or fall silent while looking
// alive. A directory descriptor stands in for the broken one: reading it fails
// with EISDIR.
func TestWatchLoop_ReadFailureIsReportedAndStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysmon.yaml")
	mgr, err := NewManager(NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := &inotify{f: f}

	done := make(chan struct{})
	defer close(done)
	events := make(chan rawEvent)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		readErr <- in.read(events, done)
	}()

	records := make(chan slog.Record, 16)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		mgr.watchLoop(newChainWatch(in, path), events, readErr, done, slog.New(channelHandler{records}), nil,
			watchTiming{debounce: watchDebounce, stall: watchStallWarning})
	}()

	await.Recv(t, "the watch loop to stop after the read failed", stopped)
	var warned bool
	for len(records) > 0 {
		if r := <-records; r.Level == slog.LevelWarn && r.Message == "config: watching the config file failed, changes are no longer reloaded" {
			warned = true
		}
	}
	if !warned {
		t.Error("a failed read stopped the watch without a warning")
	}
}

// channelHandler hands every log record to a channel.
type channelHandler struct{ records chan slog.Record }

func (h channelHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h channelHandler) Handle(_ context.Context, r slog.Record) error {
	h.records <- r.Clone()
	return nil
}
func (h channelHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h channelHandler) WithGroup(string) slog.Handler      { return h }
