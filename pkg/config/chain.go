package config

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// entryRole is what a name in a watched directory is to the config path.
type entryRole int

const (
	// roleLink is a symlink the path resolves through, or a name the path
	// needs that does not exist yet. Only its replacement or removal
	// matters: either one can send the path somewhere else.
	roleLink entryRole = iota + 1
	// roleFile is the file the path finally resolves to. Its writes matter
	// too: they are held until the writer closes the file.
	roleFile
)

// watchPlan lists the directories to watch and, in each, the names that
// matter. A config path with no symlinks on it needs one entry: its own
// directory and file name. A ConfigMap mount needs two directories:
//
//	/etc/sysmon:                       sysmon.yaml (link), ..data (link)
//	/etc/sysmon/..2026_09_24_12_00_00: sysmon.yaml (file)
type watchPlan map[string]map[string]entryRole

func (p watchPlan) add(dir, name string, role entryRole) {
	if p[dir] == nil {
		p[dir] = make(map[string]entryRole)
	}
	p[dir][name] = role
}

func (p watchPlan) equal(other watchPlan) bool {
	return maps.EqualFunc(p, other, func(a, b map[string]entryRole) bool { return maps.Equal(a, b) })
}

// maxSymlinkHops bounds resolution the way the kernel bounds it (MAXSYMLINKS):
// a longer chain is a loop, and the load fails on it with ELOOP.
const maxSymlinkHops = 40

// resolvePlan follows the absolute path one component at a time, recording
// every symlink on the way in the directory that holds it, and the file it
// ends at. A component that does not exist ends the walk: its directory is
// watched for it to appear.
//
// Every directory recorded is a real one, reached through already-resolved
// components, so the watches land on exactly the directories the kernel would
// traverse to open the path.
func resolvePlan(path string) watchPlan {
	plan := make(watchPlan)
	rest := splitPath(path)
	prefix := string(filepath.Separator)
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		if name == ".." {
			prefix = filepath.Dir(prefix)
			continue
		}
		entry := filepath.Join(prefix, name)
		info, err := os.Lstat(entry)
		if err != nil {
			plan.add(prefix, name, roleAt(rest))
			return plan
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			if len(rest) == 0 {
				plan.add(prefix, name, roleFile)
				return plan
			}
			prefix = entry
			continue
		}
		plan.add(prefix, name, roleLink)
		hops++
		target, err := os.Readlink(entry)
		if err != nil || hops > maxSymlinkHops {
			return plan
		}
		if filepath.IsAbs(target) {
			prefix = string(filepath.Separator)
		}
		rest = append(splitPath(target), rest...)
	}
	return plan
}

// roleAt is the role of a missing component: the file itself if it is the
// last one, otherwise a name the path passes through.
func roleAt(rest []string) entryRole {
	if len(rest) == 0 {
		return roleFile
	}
	return roleLink
}

// splitPath splits a path into its components, dropping empty and "."
// components. ".." is kept: after a symlink it must be applied to the
// resolved directory, not lexically to the link's text.
func splitPath(path string) []string {
	var parts []string
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// chainWatch keeps an inotify instance's watches on the directories the config
// path currently resolves through. Only the watch loop's goroutine uses it, so
// it needs no synchronisation.
type chainWatch struct {
	in *inotify
	// path is the absolute config path as configured.
	path string
	plan watchPlan
	// wds and dirs map each watched directory to its watch descriptor and
	// back.
	wds  map[string]int32
	dirs map[int32]string
}

func newChainWatch(in *inotify, path string) *chainWatch {
	return &chainWatch{in: in, path: path, plan: make(watchPlan),
		wds: make(map[string]int32), dirs: make(map[int32]string)}
}

// maxRefreshPasses bounds how often refresh re-resolves a path that keeps
// changing while it is being watched. Stopping early loses nothing: the
// changes produce events on directories already watched, and each of those
// refreshes again.
const maxRefreshPasses = 8

// refresh re-resolves the path and moves the watches to match it. It resolves
// once more after adding watches, so a link swapped in a directory that was
// not yet watched is caught rather than missed. On failure it returns the
// directory that could not be watched.
func (c *chainWatch) refresh() (string, error) {
	for range maxRefreshPasses {
		plan := resolvePlan(c.path)
		if dir, err := c.watch(plan); err != nil {
			if changedUnderfoot(err) {
				continue
			}
			return dir, err
		}
		if resolvePlan(c.path).equal(plan) {
			return "", nil
		}
	}
	return "", nil
}

// watch adds a watch for every directory in plan, drops those no longer in it,
// and adopts plan.
func (c *chainWatch) watch(plan watchPlan) (string, error) {
	for dir := range plan {
		if _, ok := c.wds[dir]; ok {
			continue
		}
		wd, err := c.in.add(dir)
		if err != nil {
			return dir, err
		}
		c.wds[dir] = wd
		c.dirs[wd] = dir
	}
	for dir, wd := range c.wds {
		if _, ok := plan[dir]; ok {
			continue
		}
		// Forget the watch first: the kernel confirms its end with an
		// IN_IGNORED that must not be mistaken for the directory going away.
		delete(c.wds, dir)
		delete(c.dirs, wd)
		if err := c.in.remove(wd); err != nil {
			return dir, err
		}
	}
	c.plan = plan
	return "", nil
}

// changedUnderfoot reports whether a failure to watch a directory means the
// path changed between resolving it and watching it: the directory vanished,
// or became a file or a symlink. Resolving again settles it.
func changedUnderfoot(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP)
}

// follow moves the watches to where the path now resolves and reports whether
// the config directory itself is gone, in which case there is nothing left to
// follow. On a failure to watch it returns the directory that failed.
//
// The directory is looked for AFTER the watches move, never before. Checked
// first, a directory removed between the check and the refresh -- rm -r
// deletes the file, the event for that is handled, and the directory goes
// while it is -- passed the check, and the refresh then dropped the watch on
// it for one on its parent. The kernel's IN_DELETE_SELF for the directory
// arrived on the dropped watch and was ignored, and the watcher went on
// silently watching the parent for a directory that was not coming back.
// Checked after, every order is caught: a directory the refresh still found
// keeps its watch, which delivers IN_DELETE_SELF when it goes; one the refresh
// did not find is gone at this check.
func (c *chainWatch) follow() (gone bool, dir string, err error) {
	dir, err = c.refresh()
	if c.configDirGone() {
		return true, "", nil
	}
	return false, dir, err
}

// configDirGone reports whether the directory the config path names has itself
// disappeared. That directory is what an operator pointed the service at; with
// it gone there is nothing left to follow.
func (c *chainWatch) configDirGone() bool {
	_, err := os.Stat(filepath.Dir(c.path))
	return errors.Is(err, fs.ErrNotExist)
}

// classify maps one record to a dirEvent.
func (c *chainWatch) classify(ev rawEvent) dirEvent {
	if ev.mask&unix.IN_Q_OVERFLOW != 0 {
		return eventOverflow
	}
	dir, ok := c.dirs[ev.wd]
	if !ok {
		// A watch already dropped: its last events, and the IN_IGNORED
		// confirming its removal, describe nothing the path uses now.
		return eventNone
	}
	if ev.mask&selfEvents != 0 {
		return eventRelinked
	}
	return classifyEntry(c.plan[dir][ev.name], ev.mask)
}
