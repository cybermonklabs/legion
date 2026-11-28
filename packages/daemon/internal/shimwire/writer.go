package shimwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrEmbeddedNewline is a line carrying a newline of its own, which would split one frame into
// two on the wire. JSON escapes its own newlines, so this is a caller's bug, not a peer's.
var ErrEmbeddedNewline = errors.New("shimwire: a frame line may not contain a newline")

// Writer writes whole frames to a stream, one newline-terminated line each.
//
// The bug the shipped writer exists for is a partial write: a ~350 KiB rpc_chunk line against a
// unix socket buffer of roughly 200 KiB, where the bytes the kernel refused were dropped and
// every chunked agent_end reached the daemon truncated (socket-writer.ts:19-25). Its queue and
// its drain handler are the netpoller's job here — an io.Writer either writes everything or
// reports why it could not, and a net.Conn parks the goroutine until the peer drains. What is
// left for this type is the part Go does not give: one frame is one Write, while the frame holds
// the writer's one slot, so two goroutines sharing a stream can never interleave their lines or
// lose a newline between them.
type Writer struct {
	// slot is a one-slot semaphore rather than a mutex. So a frame that waits for the writer
	// still answers to its own context (WriteFrameContext).
	slot chan struct{}
	dst  io.Writer
	buf  bytes.Buffer
	json *json.Encoder
}

// ContextWriter is a destination whose write a context can end. A peer that stops reading holds
// a plain Write until the destination's own bound. A frame bounded by its caller's context cannot
// wait that long.
type ContextWriter interface {
	WriteContext(ctx context.Context, p []byte) (int, error)
}

// NewWriter writes frames to dst.
func NewWriter(dst io.Writer) *Writer {
	w := &Writer{slot: make(chan struct{}, 1), dst: dst}
	w.json = newFrameEncoder(&w.buf)
	return w
}

// newFrameEncoder is the encoder every frame line is encoded with, the Writer's and FrameLen's:
// JSON.stringify does not escape <, > or &, and a prompt message is arbitrary task text, so a Go
// peer writes the same bytes the shipped one does.
func newFrameEncoder(dst io.Writer) *json.Encoder {
	encoder := json.NewEncoder(dst)
	encoder.SetEscapeHTML(false)
	return encoder
}

// FrameLen is the length of the line WriteFrame writes for f, its newline included, which a peer
// reads no longer than MaxFrameBytes. It encodes f into a counter that keeps only the length, so
// a frame is measured without a second copy of what it carries beside the encoder's own buffer.
func FrameLen(f Frame) (int, error) {
	var line byteCount
	if err := newFrameEncoder(&line).Encode(f); err != nil {
		return 0, fmt.Errorf("encode the %s frame: %w", f.FrameType(), err)
	}
	return int(line), nil
}

// byteCount is an io.Writer that keeps nothing of what it is written but its length.
type byteCount int

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

// WriteFrame encodes f and writes it as one line.
func (w *Writer) WriteFrame(f Frame) error {
	return w.WriteFrameContext(context.Background(), f)
}

// WriteFrameContext is WriteFrame bounded by ctx. It waits for the writer or for ctx, and a frame
// whose context ends first writes nothing. Once it holds the writer, it writes through the
// destination's WriteContext where the destination has one, so ctx bounds the write too.
func (w *Writer) WriteFrameContext(ctx context.Context, f Frame) error {
	if err := w.take(ctx); err != nil {
		return fmt.Errorf("the %s frame waited for the writer: %w", f.FrameType(), err)
	}
	defer w.release()
	w.buf.Reset()
	// Encode terminates the value with the newline that frames it.
	if err := w.json.Encode(f); err != nil {
		return fmt.Errorf("encode the %s frame: %w", f.FrameType(), err)
	}
	return w.writeHeld(ctx)
}

// WriteLine writes an already-encoded frame as one line. The newline is this writer's. It waits
// for the writer with no bound, so the wait cannot fail (take).
func (w *Writer) WriteLine(line []byte) error {
	w.slot <- struct{}{}
	defer w.release()
	w.buf.Reset()
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	return w.writeHeld(context.Background())
}

// take waits for the writer's slot, or for ctx to end, and then returns ctx's cause. A context
// that has already ended takes no slot, even a free one, so its frame is never written.
func (w *Writer) take(ctx context.Context) error {
	if ctx.Err() == nil {
		select {
		case w.slot <- struct{}{}:
			return nil
		case <-ctx.Done():
		}
	}
	return context.Cause(ctx)
}

// release gives the writer's slot back to the next frame.
func (w *Writer) release() { <-w.slot }

// writeHeld sends the buffered line — newline included — after refusing the two shapes that
// would desynchronise the peer's framing rather than reach it as one frame. The caller holds the
// writer's slot.
func (w *Writer) writeHeld(ctx context.Context) error {
	line := w.buf.Bytes()
	if i := bytes.IndexByte(line[:len(line)-1], '\n'); i >= 0 {
		return fmt.Errorf("%w (at byte %d of %d)", ErrEmbeddedNewline, i, len(line)-1)
	}
	if len(line) > MaxFrameBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrLineTooLong, len(line), MaxFrameBytes)
	}
	if dst, ok := w.dst.(ContextWriter); ok {
		_, err := dst.WriteContext(ctx, line)
		return err
	}
	_, err := w.dst.Write(line)
	return err
}
