package agentwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A frame is a payload's length as 4 big-endian bytes, then the payload: one JSON document. The
// length comes first so a reader knows a frame's size before it reads any of it, and refuses one
// past its limit at the header, holding none of it in memory; a newline-delimited stream would
// have to read up to the limit before it could tell. JSON never needs a delimiter of its own
// inside a frame, so any payload travels as it is.

// frameHeader is the size of a frame's length prefix.
const frameHeader = 4

// FrameTooLargeError is a frame whose header names more bytes than its reader's or writer's limit.
// Its payload was not read.
type FrameTooLargeError struct {
	Size  int
	Limit int
}

func (e FrameTooLargeError) Error() string {
	return fmt.Sprintf("a frame of %d bytes is past its limit of %d", e.Size, e.Limit)
}

// WriteFrame writes payload as one frame, refusing a payload past limit before it writes any of
// it.
func WriteFrame(w io.Writer, payload []byte, limit int) error {
	if len(payload) > limit {
		return FrameTooLargeError{Size: len(payload), Limit: limit}
	}
	frame := make([]byte, frameHeader+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[frameHeader:], payload)
	_, err := w.Write(frame)
	return err
}

// ReadFrame reads one frame of at most limit bytes. A frame whose header names more is a
// FrameTooLargeError, and nothing past its header is read. A stream that ends inside a frame is
// io.ErrUnexpectedEOF, and one that ends before it is io.EOF.
func ReadFrame(r io.Reader, limit int) ([]byte, error) {
	var header [frameHeader]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if uint64(size) > uint64(limit) {
		return nil, FrameTooLargeError{Size: int(size), Limit: limit}
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payload, nil
}
