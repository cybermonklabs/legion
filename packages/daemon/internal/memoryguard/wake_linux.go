//go:build linux

package memoryguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// watchModified watches the file at path for the kernel's file-modified event (inotify's
// IN_MODIFY), which cgroup v2 raises on memory.events through cgroup_file_notify whenever one of
// its counters changes. Its channel takes one value for each read of the events the kernel queued
// and holds one value at most, so the events that come while a value waits add none: the file,
// read after a value is taken, holds every change whose event came before it, and a change after
// that has a value of its own. stop ends the watch. The channel closes when the watch ends, and
// the watch's err says why: the error of a read that failed, the closed file's after stop, or
// errWatchIgnored once the kernel drops the watch because the file went away (IN_IGNORED).
//
// The inotify descriptor is non-blocking and held by an os.File, so its reader waits in Go's
// poller, and stop, which closes the file, ends that wait.
func watchModified(path string) (*fileWatch, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, os.NewSyscallError("inotify_init1", err)
	}
	if _, err := unix.InotifyAddWatch(fd, path, unix.IN_MODIFY); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inotify_add_watch %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), "inotify "+path)
	modified := make(chan struct{}, 1)
	w := &fileWatch{modified: modified, stop: func() { _ = file.Close() }}
	go readModified(file, modified, w)
	return w, nil
}

// readModified reads the inotify descriptor's events into modified until the watch ends, and then
// closes modified with w's err saying why. A panic here would end the launcher, PID 1 of the pod,
// which runs this on a goroutine of its own, so it ends the watch instead, as a failed read does,
// with the panic and where it was raised as w's err: the guest watch then logs its wake off line
// and goes on with its scans alone (Guest's Run).
func readModified(file io.Reader, modified chan<- struct{}, w *fileWatch) {
	defer close(modified)
	defer func() {
		if p := recover(); p != nil {
			w.err = errors.New(panicReason("panic", p))
		}
	}()
	// One buffer for every read: room for many events, each of which is at most
	// SizeofInotifyEvent+NAME_MAX+1 bytes, and an event on a watched file carries no name.
	buf := make([]byte, 4096)
	for {
		n, err := file.Read(buf)
		if err != nil {
			w.err = err
			return
		}
		select {
		case modified <- struct{}{}:
		default:
		}
		if watchIgnored(buf[:n]) {
			w.err = errWatchIgnored
			return
		}
	}
}

// watchIgnored is whether the events a read of an inotify descriptor took hold IN_IGNORED, the
// kernel's word that it dropped the watch. Each event is a struct inotify_event: wd (4 bytes), mask
// (4), cookie (4) and len (4), in the machine's byte order, then len bytes of name.
func watchIgnored(events []byte) bool {
	for len(events) >= unix.SizeofInotifyEvent {
		if binary.NativeEndian.Uint32(events[4:8])&unix.IN_IGNORED != 0 {
			return true
		}
		size := unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(events[12:16]))
		if size > len(events) {
			return false
		}
		events = events[size:]
	}
	return false
}
