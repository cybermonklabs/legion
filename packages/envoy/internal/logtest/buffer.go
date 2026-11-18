package logtest

import (
	"bytes"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Buffer is the sink the goroutines of the code under test write while the test reads it, such as
// the handler a test installs with SetDefault: the two happen at once, and the buffer's own mutex
// is what makes the read safe, so the race detector sees a log the test may read at any moment
// rather than a data race. A test that waits for a line waits on Stream through testwait.Next,
// never by reading the log once. A type that needs a reader of its own embeds this one rather than
// copying the body.
type Buffer struct {
	mu      sync.Mutex
	written strings.Builder
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
	return b.written.Write(p)
}

// Stream is every complete line written to the buffer since it was made, without its newline, in
// the order written: lines written before the first call are on it too, and a line without its
// newline yet waits for it. A test waits for a line with testwait.Next.
func (b *Buffer) Stream() *testwait.Stream[string] {
	return &b.lines
}

// String is everything logged so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written.String()
}

// Len is how many bytes have been written so far, for a reader that measures the log or marks a
// place in it rather than reading it whole.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written.Len()
}

// Lines is the logged lines that hold substring.
func (b *Buffer) Lines(substring string) string {
	var matched strings.Builder
	for line := range strings.Lines(b.String()) {
		if strings.Contains(line, substring) {
			matched.WriteString(line)
		}
	}
	return matched.String()
}
