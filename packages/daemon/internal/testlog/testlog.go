// Package testlog holds the one locked log buffer the daemon's tests share, and the one swap of the
// default logger they make (SetDefault). A test that drives
// real goroutines — a consumer, a supervisor, a process the test started — gives the code under
// test a sink it writes while the test reads what has arrived so far, and the two happen at once:
// the buffer's own mutex is what makes the read safe, so the race detector sees a log a test may
// read at any moment rather than a data race. A test that waits for a line waits on the buffer's
// Stream through testwait.Next, never by reading the log once. A type that needs a reader of its
// own embeds this one rather than copying the body.
package testlog

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// SetDefault makes handler the default logger's until t ends, then puts back the previous default
// logger and the log package's output and flags. slog.SetDefault also points the log package's
// output at handler and clears its flags, and setting the previous default back undoes neither,
// so without the rest every later log line of the binary would go into a finished test's handler.
// Envoy's logtest.SetDefault is the same function.
func SetDefault(t testing.TB, handler slog.Handler) {
	t.Helper()
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
	})
}

// Buffer is a strings.Builder an io.Writer's user and a reader may hold at the same time.
type Buffer struct {
	mu  sync.Mutex
	buf strings.Builder
	// partial is the text written since the last newline, which becomes a line at the next one.
	partial []byte
	lines   testwait.Stream[string]
}

// Write appends p to the buffer, and adds each line p completes to the stream.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rest := p
	for {
		line, after, complete := bytes.Cut(rest, []byte{'\n'})
		b.partial = append(b.partial, line...)
		if !complete {
			break
		}
		b.lines.Add(string(b.partial))
		b.partial, rest = b.partial[:0], after
	}
	return b.buf.Write(p)
}

// Stream is every complete line written to the buffer since it was made, without its newline, in
// the order written: lines written before the first call are on it too, and a line without its
// newline yet waits for it. A test waits for a line with testwait.Next. Reset leaves the stream as
// it is, and a line begun before a Reset arrives whole.
func (b *Buffer) Stream() *testwait.Stream[string] {
	return &b.lines
}

// String returns everything written so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Len is how many bytes have been written so far, for a reader that measures the log rather
// than copying it out to measure what it copied.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Reset drops everything written so far, for a test that reads one phase of its subject at a time.
// The stream keeps its lines: Next's read cursor is how a test reads it one phase at a time.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
