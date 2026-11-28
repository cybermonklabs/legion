package shimwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// The bug socket-writer.ts exists for (socket-writer.ts:19-25): an ~350 KiB rpc_chunk line
// against a unix socket buffer of roughly 200 KiB. A writer that let the transport take only
// what fits would deliver a truncated, unparseable frame. Here the reader deliberately waits
// until the buffer is full before it starts draining.
func TestALineLargerThanTheSocketBufferIsDeliveredWhole(t *testing.T) {
	line := chunkLine(t)
	listener, err := net.Listen("unix", testtmp.Socket(t, "w.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	written := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			written <- acceptErr
			return
		}
		defer conn.Close()
		written <- NewWriter(conn).WriteLine(line)
	}()

	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Let the writer fill the kernel buffer and park before anything is read from it.
	time.Sleep(100 * time.Millisecond)
	got, err := NewReader(conn).ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if writeErr := <-written; writeErr != nil {
		t.Fatalf("WriteLine: %v", writeErr)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("read %d bytes, want the whole %d-byte line", len(got), len(line))
	}
}

// Two goroutines share one stream in the shim (the wrapped process's stdout and the daemon's own
// answers). Two frames must never interleave on the wire, and neither may lose its newline.
func TestConcurrentFramesNeverInterleaveOnTheWire(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	writer := NewWriter(client)

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		reader := NewReader(server)
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			lines <- string(line)
		}
	}()

	var writers sync.WaitGroup
	for w := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 20 {
				frame := Response{
					ID:      fmt.Sprintf("w%d-%d", w, i),
					Command: TypePrompt,
					Success: true,
					// Long enough that a partial write would land inside another frame.
					Data: json.RawMessage(`"` + strings.Repeat("x", 4096) + `"`),
				}
				if err := writer.WriteFrame(frame); err != nil {
					t.Errorf("WriteFrame: %v", err)
					return
				}
			}
		}()
	}
	writers.Wait()
	client.Close()

	seen := map[string]bool{}
	for line := range lines {
		frame, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("Decode(%.80q): %v", line, err)
		}
		response, ok := frame.(Response)
		if !ok {
			t.Fatalf("Decode(%.80q) = %#v, want a Response", line, frame)
		}
		seen[response.ID] = true
	}
	if len(seen) != 8*20 {
		t.Fatalf("read %d distinct frames, want %d", len(seen), 8*20)
	}
}

// A newline inside a line would split one frame into two on the wire. JSON escapes its own, so a
// caller that hands one over has a bug worth naming rather than a stream to desynchronise.
func TestALineCarryingItsOwnNewlineIsRefused(t *testing.T) {
	var sink bytes.Buffer
	err := NewWriter(&sink).WriteLine([]byte("{\"type\":\"agent_start\"}\n{\"type\":\"agent_end\"}"))
	if !errors.Is(err, ErrEmbeddedNewline) {
		t.Fatalf("WriteLine = %v, want ErrEmbeddedNewline", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("wrote %d bytes for a refused line, want none", sink.Len())
	}
}

// The plain-frame limit is the peer's read limit too (worker-rpc.ts:8-12): a frame over it never
// arrives, so it is refused at the source and chunked instead.
func TestAFrameOverThePlainFrameLimitIsRefusedRatherThanSent(t *testing.T) {
	var sink bytes.Buffer
	err := NewWriter(&sink).WriteLine(bytes.Repeat([]byte("x"), MaxFrameBytes))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("WriteLine = %v, want ErrLineTooLong", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("wrote %d bytes for a refused line, want none", sink.Len())
	}
}

// Every frame is one line terminated by exactly one newline — the framing the reader on the
// other side splits on (socket-writer.ts:51).
func TestEachFrameIsOneNewlineTerminatedLine(t *testing.T) {
	var sink bytes.Buffer
	writer := NewWriter(&sink)
	for _, frame := range []Frame{Shutdown{}, AgentStart{DeliveryID: "d1"}, AgentEnd{}} {
		if err := writer.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame(%#v): %v", frame, err)
		}
	}
	want := "{\"type\":\"shutdown\"}\n{\"type\":\"agent_start\",\"deliveryId\":\"d1\"}\n{\"type\":\"agent_end\"}\n"
	if sink.String() != want {
		t.Fatalf("wrote %q, want %q", sink.String(), want)
	}
}

// A frame that waits for the writer while another frame holds it answers to its own context. A
// wait that no context can end holds a request past its bound. It waits as long as the peer
// leaves the other frame unread.
func TestAFrameWaitingForTheWriterEndsWithItsContext(t *testing.T) {
	dst := &heldWriter{entered: make(chan struct{}), release: make(chan struct{})}
	writer := NewWriter(dst)
	first := returnOf(func() error { return writer.WriteFrame(AgentStart{DeliveryID: "d1"}) })
	select {
	case <-dst.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first write did not reach its destination")
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiting := newDoneWatch(ctx)
	second := returnOf(func() error { return writer.WriteFrameContext(waiting, AgentEnd{}) })
	// The context is ended only once the second write waits on it. So no check made before the
	// wait can end the write in its place.
	select {
	case <-waiting.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not wait on its context while another frame held the writer")
	}
	cancel()
	err := awaitReturn(t, second, "the write did not return after its context ended")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFrameContext after its context ended = %v, want context.Canceled", err)
	}
	if got := dst.lines(); len(got) != 1 {
		t.Fatalf("the destination saw %d writes while the first held the writer, want only the first: %q", len(got), got)
	}

	close(dst.release)
	if err := awaitReturn(t, first, "the first write did not return once its destination took it"); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := writer.WriteFrame(Shutdown{}); err != nil {
		t.Fatalf("WriteFrame after the ended wait: %v", err)
	}
	want := []string{"{\"type\":\"agent_start\",\"deliveryId\":\"d1\"}\n", "{\"type\":\"shutdown\"}\n"}
	if got := dst.lines(); !slices.Equal(got, want) {
		t.Fatalf("the destination saw %q, want %q", got, want)
	}
}

// heldWriter is a destination whose first write waits until the test closes release. It keeps
// each write it took, whole.
type heldWriter struct {
	entered, release chan struct{}
	mu               sync.Mutex
	writes           []string
}

func (w *heldWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	first := len(w.writes) == 1
	w.mu.Unlock()
	if first {
		close(w.entered)
		<-w.release
	}
	return len(p), nil
}

func (w *heldWriter) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

// doneWatch is a context that says when its Done is first asked for. A frame asks for it as it
// starts to wait for the writer.
type doneWatch struct {
	context.Context
	asked chan struct{}
	once  sync.Once
}

func newDoneWatch(ctx context.Context) *doneWatch {
	return &doneWatch{Context: ctx, asked: make(chan struct{})}
}

func (c *doneWatch) Done() <-chan struct{} {
	c.once.Do(func() { close(c.asked) })
	return c.Context.Done()
}

// returnOf runs call on a goroutine of its own and hands back what it returns.
func returnOf(call func() error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- call() }()
	return result
}

// awaitReturn is what call returned. The wait only ends a test that hangs, and failing says what
// did not happen.
func awaitReturn(t *testing.T, result <-chan error, failure string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
		return nil
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
