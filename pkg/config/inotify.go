package config

import (
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// watchMask is the set of inotify events each watched directory subscribes to.
// IN_EXCL_UNLINK drops events from a file after it is unlinked from the
// directory, so a writer still holding a replaced file cannot hold or arm a
// reload of the file that took its name. IN_ONLYDIR and IN_DONT_FOLLOW make
// the kernel watch exactly the directory that was resolved: a path that has
// since become a symlink or a file is refused rather than followed somewhere
// the watcher did not intend.
const watchMask = unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_CREATE | unix.IN_MOVED_TO |
	unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_EXCL_UNLINK | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW

// selfEvents are the masks that describe a watched directory itself rather
// than an entry in it: it was deleted, moved or unmounted, or its watch ended.
const selfEvents = unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_UNMOUNT | unix.IN_IGNORED

// readBufferSize holds many events of the longest possible name. The kernel
// returns only whole events, and fails a read too small for the next one.
const readBufferSize = 64 * (unix.SizeofInotifyEvent + unix.NAME_MAX + 1)

// rawEvent is one inotify record: the watch it came from, what happened, and
// the entry it happened to (empty for an event about the directory itself).
type rawEvent struct {
	wd   int32
	mask uint32
	name string
}

// inotify is one inotify instance, holding any number of directory watches.
type inotify struct {
	f *os.File
}

// newInotify creates an inotify instance. The descriptor is non-blocking so
// that reads park in the Go runtime's poller rather than an OS thread, and a
// Close from another goroutine wakes a pending read.
func newInotify() (*inotify, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, os.NewSyscallError("inotify_init1", err)
	}
	return &inotify{f: os.NewFile(uintptr(fd), "inotify")}, nil
}

// add watches dir and returns the watch descriptor. Watching a directory that
// is already watched returns its existing descriptor.
func (in *inotify) add(dir string) (int32, error) {
	var wd int
	err := in.control(func(fd int) error {
		var err error
		wd, err = unix.InotifyAddWatch(fd, dir, watchMask)
		if err != nil {
			return os.NewSyscallError("inotify_add_watch", err)
		}
		return nil
	})
	return int32(wd), err
}

// remove ends the watch wd. A watch the kernel has already dropped -- its
// directory was deleted -- is reported as EINVAL; that is the state remove was
// asked to reach, so it is not an error.
func (in *inotify) remove(wd int32) error {
	return in.control(func(fd int) error {
		_, err := unix.InotifyRmWatch(fd, uint32(wd))
		if err != nil && !errors.Is(err, unix.EINVAL) {
			return os.NewSyscallError("inotify_rm_watch", err)
		}
		return nil
	})
}

// control runs op against the raw descriptor, which stays valid for the call.
func (in *inotify) control(op func(fd int) error) error {
	raw, err := in.f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Control(func(fd uintptr) { opErr = op(int(fd)) }); err != nil {
		return err
	}
	return opErr
}

// read delivers events until the instance is closed or done is. It returns
// nil when stopped by either, and the read error otherwise.
func (in *inotify) read(out chan<- rawEvent, done <-chan struct{}) error {
	buf := make([]byte, readBufferSize)
	for {
		n, err := in.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		for _, ev := range parseEvents(buf[:n]) {
			select {
			case out <- ev:
			case <-done:
				return nil
			}
		}
	}
}

// close releases the instance and every watch it holds.
func (in *inotify) close() error { return in.f.Close() }

// parseEvents decodes a buffer of inotify records. The kernel writes them in
// native byte order: wd int32, mask, cookie and len uint32, then len bytes of
// NUL-padded name.
func parseEvents(buf []byte) []rawEvent {
	var events []rawEvent
	for off := 0; off+unix.SizeofInotifyEvent <= len(buf); {
		wd := int32(binary.NativeEndian.Uint32(buf[off:]))
		mask := binary.NativeEndian.Uint32(buf[off+4:])
		nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
		start := off + unix.SizeofInotifyEvent
		end := start + nameLen
		if end > len(buf) {
			// The kernel never splits a record; a short one is not an event.
			break
		}
		events = append(events, rawEvent{wd: wd, mask: mask, name: entryName(buf[start:end])})
		off = end
	}
	return events
}

// entryName returns the name in a record's NUL-padded name field.
func entryName(field []byte) string {
	for i, b := range field {
		if b == 0 {
			return string(field[:i])
		}
	}
	return string(field)
}
