// Copyright 2010 The Go Authors. All rights reserved.
// Copyright 2012-2019 fsnotify Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package inotify

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Event represents a single file system notification.
type Event struct {
	Name string // Relative path to the file or directory.
	Op   Op     // File operation that triggered the event.
}

// Op describes a set of file operations.
type Op uint32

// These are the generalized file operations that can trigger a notification.
const (
	Create Op = 1 << iota
	Write
	Remove
	Rename
	Chmod
	Close_Write
	Close_NoWrite
)

// Watcher watches a set of files, delivering events to a channel.
type Watcher struct {
	Events   chan Event
	Errors   chan error
	mu       sync.Mutex // Map access
	fd       int
	poller   *fdPoller
	watches  map[string]*watch // Map of inotify watches (key: path)
	paths    map[int]string    // Map of watched paths (key: watch descriptor)
	done     chan struct{}     // Channel for sending a "quit message" to the reader goroutine
	doneResp chan struct{}     // Channel to respond to Close
}

// NewWatcher establishes a new watcher with the underlying OS and begins waiting for events.
func NewWatcher() (*Watcher, error) {
	// Create inotify fd
	fd, errno := unix.InotifyInit1(unix.IN_CLOEXEC)
	if fd == -1 {
		return nil, errno
	}
	// Create epoll
	poller, err := newFdPoller(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	w := &Watcher{
		fd:       fd,
		poller:   poller,
		watches:  make(map[string]*watch),
		paths:    make(map[int]string),
		Events:   make(chan Event),
		Errors:   make(chan error),
		done:     make(chan struct{}),
		doneResp: make(chan struct{}),
	}

	go w.readEvents()
	return w, nil
}

func (w *Watcher) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Close removes all watches and closes the events channel.
func (w *Watcher) Close() error {
	if w.isClosed() {
		return nil
	}

	// Send 'close' signal to goroutine, and set the Watcher to closed.
	close(w.done)

	// Wake up goroutine
	w.poller.wake()

	// Wait for goroutine to close
	<-w.doneResp

	return nil
}

// Add starts watching the named file or directory (non-recursively).
func (w *Watcher) Add(name string) error {
	name = filepath.Clean(name)
	if w.isClosed() {
		return errors.New("inotify instance already closed")
	}

	const agnosticEvents = unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
		unix.IN_CREATE | unix.IN_ATTRIB | unix.IN_MODIFY |
		unix.IN_MOVE_SELF | unix.IN_DELETE | unix.IN_DELETE_SELF |
		unix.IN_CLOSE_WRITE | unix.IN_CLOSE_NOWRITE

	var flags uint32 = agnosticEvents

	w.mu.Lock()
	defer w.mu.Unlock()
	watchEntry := w.watches[name]
	if watchEntry != nil {
		flags |= watchEntry.flags | unix.IN_MASK_ADD
	}
	wd, errno := unix.InotifyAddWatch(w.fd, name, flags)
	if wd == -1 {
		return errno
	}

	if watchEntry == nil {
		w.watches[name] = &watch{wd: uint32(wd), flags: flags}
		w.paths[wd] = name
	} else {
		watchEntry.wd = uint32(wd)
		watchEntry.flags = flags
	}

	return nil
}

// Remove stops watching the named file or directory (non-recursively).
func (w *Watcher) Remove(name string) error {
	name = filepath.Clean(name)

	// Fetch the watch.
	w.mu.Lock()
	defer w.mu.Unlock()
	watch, ok := w.watches[name]

	// Remove it from inotify.
	if !ok {
		return fmt.Errorf("can't remove non-existent inotify watch for: %s", name)
	}

	// We successfully removed the watch if InotifyRmWatch doesn't return an
	// error, we need to clean up our internal state to ensure it matches
	// inotify's kernel state.
	delete(w.paths, int(watch.wd))
	delete(w.watches, name)

	// inotify_rm_watch will return EINVAL if the file has been deleted;
	// the inotify will already have been removed.
	// watches and pathes are deleted in ignoreLinux() implicitly and asynchronously
	// by calling inotify_rm_watch() below. e.g. readEvents() goroutine receives IN_IGNORE
	// so that EINVAL means that the wd is being rm_watch()ed or its file removed
	// by another thread and we have not received IN_IGNORE event.
	success, errno := unix.InotifyRmWatch(w.fd, watch.wd)
	if success == -1 {
		// TODO: Perhaps it's not helpful to return an error here in every case.
		// the only two possible errors are:
		// EBADF, which happens when w.fd is not a valid file descriptor of any kind.
		// EINVAL, which is when fd is not an inotify descriptor or wd is not a valid watch descriptor.
		// Watch descriptors are invalidated when they are removed explicitly or implicitly;
		// explicitly by inotify_rm_watch, implicitly when the file they are watching is deleted.
		return errno
	}

	return nil
}

type watch struct {
	wd    uint32 // Watch descriptor (as returned by the inotify_add_watch() syscall)
	flags uint32 // inotify flags of this watch (see inotify(7) for the list of valid flags)
}

// readEvents reads from the inotify file descriptor, converts the
// received events into Event objects and sends them via the Events channel
func (w *Watcher) readEvents() {
	var (
		buf   [unix.SizeofInotifyEvent * 4096]byte // Buffer for a maximum of 4096 raw events
		n     int                                  // Number of bytes read with read()
		errno error                                // Syscall errno
		ok    bool                                 // For poller.wait
	)

	defer close(w.doneResp)
	defer close(w.Errors)
	defer close(w.Events)
	defer unix.Close(w.fd)
	defer w.poller.close()

	for {
		// See if we have been closed.
		if w.isClosed() {
			return
		}

		ok, errno = w.poller.wait()
		if errno != nil {
			select {
			case w.Errors <- errno:
			case <-w.done:
				return
			}
			continue
		}

		if !ok {
			continue
		}

		n, errno = unix.Read(w.fd, buf[:])
		// If a signal interrupted execution, see if we've been asked to close, and try again.
		// http://man7.org/linux/man-pages/man7/signal.7.html :
		// "Before Linux 3.8, reads from an inotify(7) file descriptor were not restartable"
		if errno == unix.EINTR {
			continue
		}

		// unix.Read might have been woken up by Close. If so, we're done.
		if w.isClosed() {
			return
		}

		if n < unix.SizeofInotifyEvent {
			var err error
			if n == 0 {
				// If EOF is received. This should really never happen.
				err = io.EOF
			} else if n < 0 {
				// If an error occurred while reading.
				err = errno
			} else {
				// Read was too short.
				err = errors.New("notify: short read in readEvents()")
			}
			select {
			case w.Errors <- err:
			case <-w.done:
				return
			}
			continue
		}

		var offset uint32
		// We don't know how many events we just read into the buffer
		// While the offset points to at least one whole event...
		for offset <= uint32(n-unix.SizeofInotifyEvent) {
			// Point "raw" to the event in the buffer
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))

			mask := uint32(raw.Mask)
			nameLen := uint32(raw.Len)

			if mask&unix.IN_Q_OVERFLOW != 0 {
				select {
				case w.Errors <- errors.New("inotify queue overflow"):
				case <-w.done:
					return
				}
			}

			// If the event happened to the watched directory or the watched file, the kernel
			// doesn't append the filename to the event, but we would like to always fill the
			// the "Name" field with a valid filename. We retrieve the path of the watch from
			// the "paths" map.
			w.mu.Lock()
			name, ok := w.paths[int(raw.Wd)]
			// IN_DELETE_SELF occurs when the file/directory being watched is removed.
			// This is a sign to clean up the maps, otherwise we are no longer in sync
			// with the inotify kernel state which has already deleted the watch
			// automatically.
			if ok && mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF {
				delete(w.paths, int(raw.Wd))
				delete(w.watches, name)
			}
			w.mu.Unlock()

			if nameLen > 0 {
				// Point "bytes" at the first byte of the filename
				bytes := (*[unix.PathMax]byte)(unsafe.Pointer(&buf[offset+unix.SizeofInotifyEvent]))[:nameLen:nameLen]
				// The filename is padded with NULL bytes. TrimRight() gets rid of those.
				name += "/" + strings.TrimRight(string(bytes[0:nameLen]), "\000")
			}

			event := newEvent(name, mask)
			select {
			case w.Events <- event:
			case <-w.done:
				return
			}

			// Move to the next event in the buffer
			offset += unix.SizeofInotifyEvent + nameLen
		}
	}

}

// newEvent returns an Event based on an inotify mask.
func newEvent(name string, mask uint32) Event {
	e := Event{Name: name}
	if mask&unix.IN_CREATE == unix.IN_CREATE || mask&unix.IN_MOVED_TO == unix.IN_MOVED_TO {
		e.Op |= Create
	}
	if mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF || mask&unix.IN_DELETE == unix.IN_DELETE {
		e.Op |= Remove
	}
	if mask&unix.IN_MODIFY == unix.IN_MODIFY {
		e.Op |= Write
	}
	if mask&unix.IN_MOVE_SELF == unix.IN_MOVE_SELF || mask&unix.IN_MOVED_FROM == unix.IN_MOVED_FROM {
		e.Op |= Rename
	}
	if mask&unix.IN_ATTRIB == unix.IN_ATTRIB {
		e.Op |= Chmod
	}
	if mask&unix.IN_CLOSE_WRITE == unix.IN_CLOSE_WRITE {
		e.Op |= Close_Write
	}
	if mask&unix.IN_CLOSE_NOWRITE == unix.IN_CLOSE_NOWRITE {
		e.Op |= Close_NoWrite
	}
	return e
}

func (op Op) String() string {
	// Use a buffer for efficient string concatenation
	var buffer bytes.Buffer

	if op&Create == Create {
		buffer.WriteString("|CREATE")
	}
	if op&Remove == Remove {
		buffer.WriteString("|REMOVE")
	}
	if op&Write == Write {
		buffer.WriteString("|WRITE")
	}
	if op&Rename == Rename {
		buffer.WriteString("|RENAME")
	}
	if op&Chmod == Chmod {
		buffer.WriteString("|CHMOD")
	}
	if op&Close_Write == Close_Write {
		buffer.WriteString("|CLOSE_WRITE")
	}
	if op&Close_NoWrite == Close_NoWrite {
		buffer.WriteString("|CLOSE_NOWRITE")
	}
	if buffer.Len() == 0 {
		return ""
	}
	return buffer.String()[1:] // Strip leading pipe
}

// String returns a string representation of the event in the form
// "file: REMOVE|WRITE|..."
func (e Event) String() string {
	return fmt.Sprintf("%q: %s", e.Name, e.Op.String())
}
