//go:build linux

package memoryguard

import (
	"strings"
	"testing"
	"time"
)

// panickingReader is an inotify descriptor whose read panics.
type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("a test's read panics") }

// A panic in the reader of the wake's file-modified events ends that watch as a failed read does:
// its channel closes with the panic and where it was raised as its err, which the guest watch logs
// as its wake off line before it goes on with its scans alone, and the panic does not leave the
// reader's goroutine, which would end the launcher, PID 1 of the pod.
func TestTheWakeReaderEndsItsWatchOnAPanicWithThePanicAsItsError(t *testing.T) {
	modified := make(chan struct{}, 1)
	w := &fileWatch{modified: modified}
	done := make(chan struct{})
	go func() {
		defer close(done)
		readModified(panickingReader{}, modified, w)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reader did not return after its read panicked")
	}
	if _, open := <-modified; open {
		t.Fatal("the watch's channel took a value after the reader's panic, want it closed")
	}
	want := "panic: a test's read panics at "
	if w.err == nil || !strings.HasPrefix(w.err.Error(), want) || !strings.Contains(w.err.Error(), "wake_linux_test.go:") {
		t.Fatalf("the watch's err is %v, want one starting %q that names this test's file", w.err, want)
	}
}
