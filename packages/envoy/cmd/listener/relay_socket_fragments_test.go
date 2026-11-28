package main

import (
	"bytes"
	"errors"
	"testing"
)

// A data frame's JSON reaches gorilla/websocket in pieces of at most relaySocketWriteFragment
// bytes, in order and whole, whatever its length, and the first piece the writer refuses ends the
// write with the bytes written before it; the upgrader's write buffer is that size, so gorilla
// copies each piece into its buffer and sends it as fragments of that size, never as one fragment
// of its own (gorilla writes a piece past twice its buffer whole). Control: fragments handing p
// whole; the write of one fragment and a byte reaches the writer as one piece past the fragment,
// and the row names it.
func TestFragmentsHandsItsWriterPiecesOfAtMostOneFragment(t *testing.T) {
	t.Parallel()
	if got := newRelaySockets(nil, nil, nil).upgrader.WriteBufferSize; got != relaySocketWriteFragment {
		t.Fatalf("the upgrader's write buffer is %d bytes, want relaySocketWriteFragment (%d), the size of the pieces write hands gorilla", got, relaySocketWriteFragment)
	}
	for _, n := range []int{0, 1, relaySocketWriteFragment - 1, relaySocketWriteFragment, relaySocketWriteFragment + 1, 2 * relaySocketWriteFragment, 2*relaySocketWriteFragment + 1, 700 << 10} {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(i % 251)
		}
		pieces := &pieceRecorder{}
		written, err := fragments{pieces}.Write(p)
		if err != nil || written != n {
			t.Fatalf("a write of %d bytes wrote %d (%v), want all of them", n, written, err)
		}
		if want := (n + relaySocketWriteFragment - 1) / relaySocketWriteFragment; len(pieces.sizes) != want {
			t.Fatalf("a write of %d bytes reached the writer as %d pieces %v, want %d", n, len(pieces.sizes), pieces.sizes, want)
		}
		for _, size := range pieces.sizes {
			if size == 0 || size > relaySocketWriteFragment {
				t.Fatalf("a write of %d bytes reached the writer as pieces %v, want each of 1 to %d bytes", n, pieces.sizes, relaySocketWriteFragment)
			}
		}
		if !bytes.Equal(pieces.data, p) {
			t.Fatalf("a write of %d bytes reached the writer as other bytes, or in another order", n)
		}
	}
	refusing := &pieceRecorder{refuse: 2}
	written, err := fragments{refusing}.Write(make([]byte, 3*relaySocketWriteFragment))
	if !errors.Is(err, errPieceRefused) || written != relaySocketWriteFragment || len(refusing.sizes) != 2 {
		t.Fatalf("a write whose second piece the writer refused wrote %d bytes in %d pieces (%v), want %d bytes, 2 pieces and the refusal", written, len(refusing.sizes), err, relaySocketWriteFragment)
	}
}

var errPieceRefused = errors.New("the writer refused the piece")

// pieceRecorder is a writer that records each piece it is handed, and refuses the refuse-th piece
// when refuse is set, as gorilla's message writer does once its connection fails: 0 bytes and the
// error.
type pieceRecorder struct {
	sizes  []int
	data   []byte
	refuse int
}

func (r *pieceRecorder) Write(p []byte) (int, error) {
	r.sizes = append(r.sizes, len(p))
	if r.refuse > 0 && len(r.sizes) == r.refuse {
		return 0, errPieceRefused
	}
	r.data = append(r.data, p...)
	return len(p), nil
}
