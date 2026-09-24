package config

import (
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchDebounce collapses the burst of events an editor produces when it saves
// (write, chmod, rename) into a single reload.
const watchDebounce = 250 * time.Millisecond

// Watch reloads the configuration whenever its file changes on disk, until done
// is closed. It watches the containing directory rather than the file itself so
// that an editor which replaces the file (write to temp, rename over) still
// triggers a reload — watching the inode would silently stop working.
//
// onReload is called after a successful swap with the settings that need a
// restart to take effect. Errors are logged and the running configuration is
// left in place: a syntax error in a half-saved file must not take the daemon's
// settings down with it.
func (m *Manager) Watch(done <-chan struct{}, logger *slog.Logger, onReload func(deferred []string)) error {
	dir := configFileDir(m.Path())
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return &ConfigWatchError{Dir: dir, Err: err}
	}
	if err := w.Add(dir); err != nil {
		// The watcher never started watching, but it holds an inotify
		// descriptor all the same. A failure to release it is reported with
		// the failure to watch, not in place of it.
		if closeErr := w.Close(); closeErr != nil {
			return &ConfigWatchError{Dir: dir, Err: errors.Join(err, closeErr)}
		}
		return &ConfigWatchError{Dir: dir, Err: err}
	}

	target := filepath.Clean(m.Path())

	go func() {
		defer func() {
			if err := w.Close(); err != nil {
				logger.Debug("config: closing watcher", "error", err)
			}
		}()

		var timer *time.Timer
		var timerC <-chan time.Time

		for {
			select {
			case <-done:
				return

			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != target {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(watchDebounce)
				timerC = timer.C

			case <-timerC:
				timerC = nil
				deferred, err := m.Reload()
				// An applier that rejects a setting leaves the reload in
				// force, so it is logged as a partial application rather than
				// discarded as a failed read.
				var applyErr *ErrApply
				if err != nil && !errors.As(err, &applyErr) {
					logger.Warn("config: reload failed, keeping previous settings", "error", err)
					continue
				}
				logger.Info("config: reloaded from disk", "path", m.Path())
				if applyErr != nil {
					logger.Warn("config: reload applied, but some settings were rejected",
						"error", applyErr)
				}
				if onReload != nil {
					onReload(deferred)
				}

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				logger.Debug("config: watcher error", "error", err)
			}
		}
	}()

	return nil
}
