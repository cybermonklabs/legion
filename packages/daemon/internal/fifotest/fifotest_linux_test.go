//go:build linux

package fifotest

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A plain open of a fifo waits for a writer; the watch reads that wait from the kernel, answers it,
// and its own open for writing lets the waiting open go on. The verdict waits for the watch with no
// span of its own: a loaded machine only makes the open slower to start.
func TestAWatchSeesAPlainOpenOfItsFifoAndLetsItGoOn(t *testing.T) {
	Require(t)
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		file, err := os.Open(path)
		if err == nil {
			err = file.Close()
		}
		opened <- err
	}()
	watch := &Watch{Path: path}
	giveUp := time.After(testwait.Bounded(t, 1<<60, 30*time.Second))
	poll := time.NewTicker(testwait.Poll)
	defer poll.Stop()
	for seen := false; !seen; {
		select {
		case <-poll.C:
			var err error
			if seen, err = watch.Opened(); err != nil {
				t.Fatalf("Opened: %v", err)
			}
		case <-giveUp:
			t.Fatalf("the watch never answered the plain open of %s, to 30 s before the test binary's deadline", path)
		}
	}
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("the plain open the watch answered ended with %v, want it to go on and close", err)
		}
	case <-time.After(testwait.Bounded(t, 1<<60, 10*time.Second)):
		t.Fatal("the plain open the watch answered still waits: the watch's own open for writing did not let it go on")
	}
}

// Code that opens the fifo with O_NONBLOCK is its reader without waiting in the open, as a correct
// check of the fifo is, and the watch does not take it for a plain open however many times it is
// asked: the rule is two calls in a row with a thread asleep in a plain openat, not a reader alone.
func TestAWatchTakesNoReaderThatDidNotWaitForAPlainOpen(t *testing.T) {
	Require(t)
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(reader) }()
	watch := &Watch{Path: path}
	for call := 1; call <= 3; call++ {
		seen, err := watch.Opened()
		if err != nil {
			t.Fatalf("Opened, call %d: %v", call, err)
		}
		if seen {
			t.Fatalf("Opened answered a plain open at call %d with only a reader that did not wait holding %s", call, path)
		}
	}
}

// Where the kernel keeps no record of what each thread is asleep in, as gVisor keeps none, the
// watch answers ErrUnsupported rather than a clean false, and so does the check Require makes.
func TestAWatchAnswersUnsupportedWhereTheKernelKeepsNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tasks, record string
	}{
		{name: "no record beside any thread", tasks: tasks, record: "no-such-record"},
		{name: "no list of threads", tasks: filepath.Join(t.TempDir(), "no-such-tasks"), record: record},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousTasks, previousRecord := tasks, record
			tasks, record = tc.tasks, tc.record
			t.Cleanup(func() { tasks, record = previousTasks, previousRecord })
			if seen, err := (&Watch{Path: filepath.Join(t.TempDir(), "fifo")}).Opened(); seen || !errors.Is(err, ErrUnsupported) {
				t.Errorf("Opened = %v, %v; want false, ErrUnsupported", seen, err)
			}
			if err := supported(); !errors.Is(err, ErrUnsupported) {
				t.Errorf("supported = %v, want ErrUnsupported", err)
			}
		})
	}
}
