// Package testfifo plants a fifo where code under test must replace a file without opening it
// for writing, and tells the test, with no clock, whether that code opened it so.
package testfifo

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Fifo is a fifo Plant made, with a reader the test holds on it until the test ends.
type Fifo struct {
	path   string
	reader int
}

// Plant makes a fifo of mode perm at path and holds a reader on it until the test ends. While
// the reader is held, an open of the fifo for writing returns at once instead of waiting for a
// reader, so code that opens it for writing, as os.WriteFile does, answers rather than hangs, and
// the held reader is shown that open (OpenedForWriting).
//
// Plant first runs its own control on a second held reader: shown nothing while no writer has
// opened the fifo, and shown a writer's open and close. A kernel whose held reader shows something
// with no writer, or nothing after one, fails the test there by name, rather than let
// OpenedForWriting give a verdict it cannot see.
func Plant(t *testing.T, path string, perm uint32) *Fifo {
	t.Helper()
	if err := syscall.Mkfifo(path, perm); err != nil {
		t.Fatal(err)
	}
	control := &Fifo{path: path, reader: holdReader(t, path)}
	if shown := control.OpenedForWriting(t); shown != "" {
		t.Fatalf("a reader held on the fifo %s was shown %s before any writer opened it, so a reader held there cannot tell a write's open", path, shown)
	}
	writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if shown := control.OpenedForWriting(t); shown == "" {
		t.Fatalf("a reader held on the fifo %s was shown nothing of a writer's open and close, so a reader held there could see no write's open either", path)
	}
	held := &Fifo{path: path, reader: holdReader(t, path)}
	if shown := held.OpenedForWriting(t); shown != "" {
		t.Fatalf("the reader held on the fifo %s was shown %s before the code under test ran", path, shown)
	}
	return held
}

// OpenedForWriting answers what poll(2) has shown the held reader since Plant opened it: "" for
// nothing, POLLIN while bytes a writer wrote are unread, POLLHUP once a writer that opened the
// fifo closed it, and any other event by its number. Linux shows a reader opened while the fifo
// had no writer no hang-up until a writer opens it (fs/pipe.c, fifo_open and pipe_poll), so a
// hang-up is such an open, even one that wrote nothing. An open of the fifo for reading, as a
// read that refuses what is no regular file makes, shows the held reader nothing.
func (f *Fifo) OpenedForWriting(t *testing.T) string {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(f.reader), Events: unix.POLLIN}}
	// A poll that finds nothing answers EINTR when a signal is pending, as the Go runtime's
	// preemption signal can be, and that says nothing of the fifo, so it polls again.
	for {
		_, err := unix.Poll(fds, 0)
		if err == nil {
			break
		}
		if err != unix.EINTR {
			t.Fatalf("poll the reader held on the fifo %s: %v", f.path, err)
		}
	}
	var shown []string
	if fds[0].Revents&unix.POLLIN != 0 {
		shown = append(shown, "POLLIN")
	}
	if fds[0].Revents&unix.POLLHUP != 0 {
		shown = append(shown, "POLLHUP")
	}
	if rest := fds[0].Revents &^ (unix.POLLIN | unix.POLLHUP); rest != 0 {
		shown = append(shown, fmt.Sprintf("the events %#x", rest))
	}
	return strings.Join(shown, "|")
}

// holdReader opens the fifo at path for reading, which waits for no writer, and closes it when
// the test ends.
func holdReader(t *testing.T, path string) int {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("hold a reader on the fifo %s: %v", path, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}
