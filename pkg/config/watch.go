package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// watchDebounce collapses a burst of completed changes -- a create followed by
// its close, an editor's rename and chmod -- into a single reload. It is not
// what keeps a partly written file from being loaded: that is waiting for the
// writer to close it (see transitions).
const watchDebounce = 250 * time.Millisecond

// watchStallWarning is how long a writer may hold the config file open without
// changing it before the watcher says so. The kernel reports a write as
// finished only by its close, so a writer that finishes and keeps the file
// open cannot be told from one that is still writing, and its file is never
// published. It is not waited on in silence either: after this long without a
// change the watcher warns that the reload is held, and how to apply it now.
const watchStallWarning = 30 * time.Second

// watchTiming holds the watcher's two periods, so tests can shorten them.
type watchTiming struct {
	// debounce coalesces completed changes into one reload.
	debounce time.Duration
	// stall is how long a held write may go unchanged before a warning.
	stall time.Duration
}

// Watch reloads the configuration whenever its file changes on disk, until done
// is closed.
//
// It watches directories rather than the file itself, so an editor or
// config-management tool that replaces the file (write to a temp file, rename
// over) still triggers a reload; watching the inode would silently stop
// working after the first such save.
//
// The directories are the ones the config path resolves through. A path with
// symlinks on it -- a Kubernetes ConfigMap mount, where the kubelet updates
// the volume by swapping the ..data symlink onto a new directory, or a link to
// a file kept elsewhere -- is followed link by link: replacing any link on the
// way reloads the file it now leads to, and the watches move with it.
//
// A file written in place -- truncated, then written -- is reloaded only once
// its writer closes it. A reload taken in between would read a file that is
// empty or cut short, and a file cut at a line boundary parses: it would
// publish the new values for the lines already written and defaults for the
// rest. An empty file is refused outright (ConfigEmptyError), which covers a
// writer that truncates, closes and writes later. A writer that finishes but
// keeps the file open is never reloaded -- the close is the kernel's only
// signal that a write is complete -- and the watcher warns once the file has
// gone watchStallWarning without a change, so the hold is never silent.
//
// Opens are not watched. The kernel reports a reader's open exactly as a
// writer's, so an open cannot mean "the previous writer finished": taking it
// that way would let anyone reading the file mid-write publish it half
// written.
//
// onReload is called after a successful swap with the settings that need a
// restart to take effect. Errors are logged and the running configuration is
// left in place: a file that cannot be used must not take the daemon's
// settings down with it.
func (m *Manager) Watch(done <-chan struct{}, logger *slog.Logger, onReload func(deferred []string)) error {
	return m.watch(done, logger, onReload, watchTiming{debounce: watchDebounce, stall: watchStallWarning})
}

// watch is Watch with its periods supplied.
func (m *Manager) watch(done <-chan struct{}, logger *slog.Logger, onReload func(deferred []string),
	timing watchTiming) error {
	dir := configFileDir(m.Path())
	path, err := filepath.Abs(m.Path())
	if err != nil {
		return &ConfigWatchError{Dir: dir, Err: err}
	}
	if _, err := os.Stat(dir); err != nil {
		return &ConfigWatchError{Dir: dir, Err: err}
	}
	in, err := newInotify()
	if err != nil {
		return &ConfigWatchError{Dir: dir, Err: err}
	}
	chain := newChainWatch(in, path)
	if failed, err := chain.refresh(); err != nil {
		// The instance never started watching, but it holds a descriptor all
		// the same. A failure to release it is reported with the failure to
		// watch, not in place of it.
		if closeErr := in.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return &ConfigWatchError{Dir: failed, Err: err}
	}

	events := make(chan rawEvent)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		readErr <- in.read(events, done)
	}()

	go m.watchLoop(chain, events, readErr, done, logger, onReload, timing)
	return nil
}

// watchLoop turns inotify events into reloads until done is closed, the config
// directory goes away, or reading events fails.
func (m *Manager) watchLoop(chain *chainWatch, events <-chan rawEvent, readErr <-chan error,
	done <-chan struct{}, logger *slog.Logger, onReload func(deferred []string), timing watchTiming) {
	defer func() {
		if err := chain.in.close(); err != nil {
			logger.Debug("config: closing the watch", "error", err)
		}
		// Closing the descriptor ends the reader; draining lets it finish a
		// send it may be blocked in, so no goroutine outlives the watch.
		for range events {
		}
		logger.Debug("config: stopped watching the config file", "path", m.Path())
	}()

	debounce := time.NewTimer(timing.debounce)
	debounce.Stop()
	var fire <-chan time.Time

	// stall runs while a write is held, restarted by each change to the
	// file, so it fires only once the writer has gone quiet with the file
	// still open.
	stall := time.NewTimer(timing.stall)
	stall.Stop()
	var stalled <-chan time.Time

	var state watchState
	for {
		select {
		case <-done:
			return

		case raw, ok := <-events:
			if !ok {
				if err := <-readErr; err != nil {
					logger.Warn("config: watching the config file failed, changes are no longer reloaded",
						"path", m.Path(), "error", err)
				}
				return
			}
			ev := chain.classify(raw)
			action := state.on(ev, logger)
			if movesWatches[ev] {
				gone, dir, err := chain.follow()
				if gone {
					logger.Warn("config: the config directory is gone, changes are no longer watched",
						"path", m.Path())
					return
				}
				if err != nil {
					logger.Warn("config: cannot watch a directory the config path resolves through, changes there are not seen",
						"dir", dir, "error", err)
				}
			}
			switch action {
			case reloadArm:
				debounce.Reset(timing.debounce)
				fire = debounce.C
				stall.Stop()
				stalled = nil
			case reloadHold:
				debounce.Stop()
				fire = nil
				stall.Reset(timing.stall)
				stalled = stall.C
			}

		case <-fire:
			fire = nil
			m.reloadFromWatch(logger, onReload)

		case <-stalled:
			stalled = nil
			logger.Warn("config: file is still open for writing and has not changed; reload waits until it is closed",
				"path", m.Path(), "unchanged_for", timing.stall,
				"apply_now", "systemctl reload go-sysmon")
		}
	}
}

// reloadFromWatch performs one watcher-triggered reload and reports it.
func (m *Manager) reloadFromWatch(logger *slog.Logger, onReload func(deferred []string)) {
	deferred, err := m.reload(emptyIsIncomplete)
	// An applier that rejects a setting leaves the reload in force, so it is
	// logged as a partial application rather than discarded as a failed read.
	var applyErr *ApplyError
	if err != nil && !errors.As(err, &applyErr) {
		logger.Warn("config: reload failed, keeping previous settings", "error", err)
		return
	}
	logger.Info("config: reloaded from disk", "path", m.Path())
	if applyErr != nil {
		logger.Warn("config: reload applied, but some settings were rejected", "error", applyErr)
	}
	if onReload != nil {
		onReload(deferred)
	}
}

// dirEvent is what happened to the config path, reduced to what the reload
// decision needs.
type dirEvent int

const (
	eventNone dirEvent = iota
	// eventWritten: the file was modified (written or truncated).
	eventWritten
	// eventClosed: a writer closed the file (IN_CLOSE_WRITE).
	eventClosed
	// eventReplaced: a new entry took the file's name (IN_CREATE, IN_MOVED_TO).
	eventReplaced
	// eventRemoved: the file's name went away (IN_DELETE, IN_MOVED_FROM).
	eventRemoved
	// eventRelinked: a symlink the path resolves through was replaced or
	// removed, or a directory it resolves through went away.
	eventRelinked
	// eventOverflow: the kernel dropped events (IN_Q_OVERFLOW).
	eventOverflow
)

// fileEvents maps the masks that matter on the file itself to events.
var fileEvents = []struct {
	mask  uint32
	event dirEvent
}{
	{unix.IN_CLOSE_WRITE, eventClosed},
	{unix.IN_MODIFY, eventWritten},
	{unix.IN_CREATE, eventReplaced},
	{unix.IN_MOVED_TO, eventReplaced},
	{unix.IN_DELETE, eventRemoved},
	{unix.IN_MOVED_FROM, eventRemoved},
}

// linkChanges are the masks that change what a link on the path points at:
// only a new entry under its name, or its removal. A symlink is never written
// in place.
const linkChanges = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_DELETE | unix.IN_MOVED_FROM

// roleEvents maps each role an entry can play on the path to how its events
// are read.
var roleEvents = map[entryRole]func(mask uint32) dirEvent{
	roleFile: func(mask uint32) dirEvent {
		for _, f := range fileEvents {
			if mask&f.mask != 0 {
				return f.event
			}
		}
		return eventNone
	},
	roleLink: func(mask uint32) dirEvent {
		if mask&linkChanges != 0 {
			return eventRelinked
		}
		return eventNone
	},
}

// classifyEntry maps an event on a named entry to a dirEvent, by the role the
// entry plays on the path. An entry with no role is not part of it.
func classifyEntry(role entryRole, mask uint32) dirEvent {
	if events, ok := roleEvents[role]; ok {
		return events(mask)
	}
	return eventNone
}

// movesWatches lists the events after which the path may resolve somewhere
// else, so the watches are moved to match before the reload.
var movesWatches = map[dirEvent]bool{
	eventReplaced: true,
	eventRemoved:  true,
	eventRelinked: true,
	eventOverflow: true,
}

// reloadAction is what the watcher does with its pending reload after an event.
type reloadAction int

const (
	// reloadKeep leaves the pending reload, if any, as it is.
	reloadKeep reloadAction = iota
	// reloadArm (re)starts the debounce: as far as the events show the file is
	// complete, and a reload follows unless another event intervenes.
	reloadArm
	// reloadHold cancels any pending reload: a writer has the file open, and
	// the reload waits for it to close.
	reloadHold
)

// watchState tracks whether the config file is mid-write. Only the watch
// loop's goroutine touches it, so it needs no synchronisation.
type watchState struct {
	writing bool
}

// transitions maps each kind of event to its effect on the state and on the
// pending reload.
var transitions = map[dirEvent]func(s *watchState, logger *slog.Logger) reloadAction{
	// Data was written, or the file truncated. The writer closes the file
	// when it is done, and eventClosed arrives then.
	eventWritten: func(s *watchState, logger *slog.Logger) reloadAction {
		if !s.writing {
			s.writing = true
			logger.Debug("config: file is being written, reload waits until it is closed")
		}
		return reloadHold
	},
	// A writer closed the file it had open for writing: the write is complete.
	eventClosed: settled,
	// A new entry took the name. A rename brings a complete file. A create by
	// open(2) is followed by eventWritten as soon as anything is written,
	// which holds the reload until the close; one that stalls before its
	// first write is seen empty and refused.
	eventReplaced: settled,
	// The name was removed or renamed away. With no file the service runs on
	// defaults, as it would have started.
	eventRemoved: settled,
	// A link on the path now leads somewhere else. Whatever it leads to was
	// complete when the link was swapped in -- the kubelet writes the new
	// directory in full first -- so it is reloaded; a write still under way
	// there holds the reload as usual.
	eventRelinked: settled,
	// The kernel's queue overflowed and events were lost, so whether a write
	// is in progress is unknown. Reloading is the only way to converge on the
	// file as it now is. The queue holds 16384 events by default; the
	// directories of one config path do not produce that many between two
	// reads.
	eventOverflow: settled,
}

// settled records that no write is in progress and arms a reload.
func settled(s *watchState, _ *slog.Logger) reloadAction {
	s.writing = false
	return reloadArm
}

// on applies ev to the state and returns what to do with the pending reload.
func (s *watchState) on(ev dirEvent, logger *slog.Logger) reloadAction {
	if apply, ok := transitions[ev]; ok {
		return apply(s, logger)
	}
	return reloadKeep
}
