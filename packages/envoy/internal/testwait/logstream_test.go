package testwait_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A captured log is a Stream's chief producer: the buffer adds each line a write completes, from
// the buffer's first write on, so a line that came before the test asked for the stream is still
// on it, a partial line waits for its newline, and one write that holds two lines gives two.
func TestTheLogBufferStreamsEachCompleteLine(t *testing.T) {
	var log logtest.Buffer
	write(t, &log, "written before the stream was asked for\n")
	stream := log.Stream()
	if again := log.Stream(); again != stream {
		t.Fatal("Stream returned a second stream; a buffer has one")
	}
	write(t, &log, "a partial ")
	write(t, &log, "line\nthe second line of one write\nthe third line of it\n")
	write(t, &log, "\n")
	// Had the partial line been streamed before its newline, the second line read would be it.
	for _, want := range []string{
		"written before the stream was asked for",
		"a partial line",
		"the second line of one write",
		"the third line of it",
		"",
	} {
		if got := testwait.Next(t, fmt.Sprintf("the line %q", want), stream, func(string) bool { return true }); got != want {
			t.Fatalf("the stream's next line is %q, want %q", got, want)
		}
	}
	// The stream is beside the buffer's other readers, which still read every line.
	if got, want := log.Lines("line of"), "the second line of one write\nthe third line of it\n"; got != want {
		t.Fatalf("Lines(%q) is %q, want %q", "line of", got, want)
	}
}

// Writers on goroutines of their own each write whole lines while the test reads: every line
// arrives whole, once.
func TestTheLogBufferStreamsTheLinesOfConcurrentWriters(t *testing.T) {
	const writers, each = 4, 50
	var log logtest.Buffer
	var wrote sync.WaitGroup
	for writer := range writers {
		wrote.Go(func() {
			for line := range each {
				fmt.Fprintf(&log, "writer %d line %d\n", writer, line)
			}
		})
	}
	unread := map[string]bool{}
	for writer := range writers {
		for line := range each {
			unread[fmt.Sprintf("writer %d line %d", writer, line)] = true
		}
	}
	for range writers * each {
		line := testwait.Next(t, "the next line a writer completes", log.Stream(), func(string) bool { return true })
		if !unread[line] {
			t.Fatalf("the stream gave %q, which is no line a writer wrote, or one it gave before", line)
		}
		delete(unread, line)
	}
	wrote.Wait()
}

func write(t *testing.T, log *logtest.Buffer, text string) {
	t.Helper()
	if _, err := log.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
}
