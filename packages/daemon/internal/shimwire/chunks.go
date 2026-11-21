package shimwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// The refusals a chunk sequence can earn. Each is a bound the shipped decoder already enforces
// (worker-rpc.ts:246-330); the difference is that the sequence is refused by name rather than
// dropped into a log line, so a caller can say which bound broke.
var (
	// ErrChunkMetadata is a chunk whose own numbers are impossible: the ids, the index and
	// count, or a declared length outside the band a sequence may carry (worker-rpc.ts:248-267).
	ErrChunkMetadata = errors.New("shimwire: invalid rpc chunk metadata")
	// ErrChunkData is a payload that is not canonical base64, or is larger than one chunk may
	// carry (worker-rpc.ts:149-157, 275-278).
	ErrChunkData = errors.New("shimwire: invalid rpc chunk data")
	// ErrChunkSequence is a chunk that does not continue the sequence in progress: a wrong
	// start, a changed header, a gap, or a total that disagrees with the declared length
	// (worker-rpc.ts:280-309, 348-349).
	ErrChunkSequence = errors.New("shimwire: invalid rpc chunk sequence")
	// ErrChunkPayload is a reassembled payload that is not a frame (worker-rpc.ts:312-329).
	ErrChunkPayload = errors.New("shimwire: reassembled rpc chunk payload is not a frame")
	// ErrFrameFitsOneLine is a frame small enough to send plainly. The decoder refuses a
	// sequence declaring less than the plain-frame limit (worker-rpc.ts:262), so chunking one
	// would be sending a sequence no peer accepts.
	ErrFrameFitsOneLine = errors.New("shimwire: frame fits one line and is never chunked")
	// ErrFrameTooLarge is a frame over the reassembly limit (worker-rpc.ts:263).
	ErrFrameTooLarge = errors.New("shimwire: frame exceeds the reassembly limit")
)

// maxChunkCount is the most chunks a sequence may declare: the reassembly limit in full
// payloads (worker-rpc.ts:260).
const maxChunkCount = MaxReassembledBytes / ChunkPayloadBytes

// maxChunkIDBytes bounds the sequence identifier (worker-rpc.ts:251).
const maxChunkIDBytes = 128

// SplitChunks slices one logical frame into the rpc_chunk sequence that carries it: base64 of
// each ChunkPayloadBytes slice, in order, every chunk declaring the whole frame's length
// (fake-omp-rpc.ts:50-65). Only a frame between the plain-frame limit and the reassembly limit
// has a sequence: below it the frame is one line, above it no peer will put it back together.
func SplitChunks(chunkID string, frame []byte) ([]RPCChunk, error) {
	if chunkID == "" || len(chunkID) > maxChunkIDBytes {
		return nil, fmt.Errorf("%w: chunk id of %d bytes", ErrChunkMetadata, len(chunkID))
	}
	if len(frame) < MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameFitsOneLine, len(frame), MaxFrameBytes)
	}
	if len(frame) > MaxReassembledBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, len(frame), MaxReassembledBytes)
	}
	count := (len(frame) + ChunkPayloadBytes - 1) / ChunkPayloadBytes
	chunks := make([]RPCChunk, 0, count)
	for index := range count {
		end := min((index+1)*ChunkPayloadBytes, len(frame))
		chunks = append(chunks, RPCChunk{
			ChunkID:    chunkID,
			Index:      index,
			Count:      count,
			ByteLength: len(frame),
			Data:       base64.StdEncoding.EncodeToString(frame[index*ChunkPayloadBytes : end]),
		})
	}
	return chunks, nil
}

// Reassembler puts one connection's chunk sequences back together, one at a time — the order
// the transport guarantees and the shipped decoder assumes (worker-rpc.ts:246-331). Its zero
// value is ready. It is not safe for concurrent use: one connection, one reader.
type Reassembler struct {
	active     bool
	chunkID    string
	count      int
	byteLength int
	nextIndex  int
	payload    []byte
	// skipping is the id of the sequence whose chunks Push passes over, "" when there is none.
	skipping string
}

// Push adds one chunk. It returns the whole logical frame once the last one lands, nil while
// more are expected, and an error when it drops a sequence, naming the bound a chunk broke: the
// sequence dropped is the one in progress, except where the paragraph below names another.
// Nothing is ever returned truncated.
//
// A shim before this release forwarded a sequence line by line and replayed to its next
// connection what a broken one did not take, so a stream can begin inside a sequence whose head
// went to the broken connection (internal/shim sends and replays each sequence whole). So a chunk
// past index 0 of a sequence that is not the one in progress drops its sequence with one error,
// and every later chunk of that sequence is passed over unread and without one, until a chunk of
// another sequence or a whole frame (Interrupt) ends the skip. Where another sequence was in
// progress, that one error also names it, as abandoned by its sender. A chunk 0 always starts its
// sequence: one it cuts short was abandoned by its sender and is reported as Interrupt reports
// it, while the new one goes on.
//
// The frame is returned as bytes rather than a decoded Frame so nothing is lost on the way
// through: Decode reads it exactly as it reads a plain line.
func (r *Reassembler) Push(chunk RPCChunk) ([]byte, error) {
	if r.skipping != "" {
		if chunk.ChunkID == r.skipping && chunk.Index != 0 {
			return nil, nil
		}
		r.skipping = ""
	}
	if err := chunk.validate(); err != nil {
		r.drop()
		return nil, err
	}
	payload, err := decodeChunkData(chunk.Data)
	if err != nil {
		r.drop()
		return nil, err
	}
	var abandoned error
	if chunk.Index == 0 {
		abandoned = r.Interrupt()
	}
	if !r.active {
		if chunk.Index != 0 {
			return nil, fmt.Errorf("%w: %s", ErrChunkSequence, r.skip(chunk))
		}
		r.active = true
		r.chunkID, r.count, r.byteLength, r.nextIndex = chunk.ChunkID, chunk.Count, chunk.ByteLength, 0
		r.payload = r.payload[:0]
	}
	if r.chunkID != chunk.ChunkID {
		// Only a chunk past index 0 gets here with another id, since a chunk 0 has started its own
		// sequence above: one whose start was not on this stream arrived while another was built.
		abandoned = r.Interrupt()
		return nil, fmt.Errorf("%w, and %s", abandoned, r.skip(chunk))
	}
	if r.count != chunk.Count || r.byteLength != chunk.ByteLength || r.nextIndex != chunk.Index {
		r.drop()
		return nil, fmt.Errorf(
			"%w: expected chunk %d of %q (%d chunks, %d bytes), got chunk %d of %q (%d chunks, %d bytes)",
			ErrChunkSequence, r.nextIndex, r.chunkID, r.count, r.byteLength,
			chunk.Index, chunk.ChunkID, chunk.Count, chunk.ByteLength,
		)
	}
	r.payload = append(r.payload, payload...)
	r.nextIndex++
	if len(r.payload) > r.byteLength {
		declared := r.byteLength
		r.drop()
		return nil, fmt.Errorf("%w: more bytes than the declared %d", ErrChunkSequence, declared)
	}
	if r.nextIndex < r.count {
		// A chunk 0 always ends here — validate holds a sequence to two chunks or more and to a
		// declared length over one chunk's payload — so the sequence it cut short is reported.
		return nil, abandoned
	}
	if len(r.payload) != r.byteLength {
		got, declared := len(r.payload), r.byteLength
		r.drop()
		return nil, fmt.Errorf("%w: %d bytes for a declared %d", ErrChunkSequence, got, declared)
	}
	frame := r.payload
	r.active = false
	// The frame is handed to the caller, so the next sequence starts on its own memory.
	r.payload = nil
	if err := validReassembled(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// Interrupt drops a half-built sequence when something that is not a chunk arrives on the same
// stream: the sender abandoned it, and joining its chunks to whatever comes next would build a
// frame nobody sent (worker-rpc.ts:348-349). It reports what it dropped, and nothing when there
// was nothing to drop. A sequence Push was passing over ends here too, without a word.
func (r *Reassembler) Interrupt() error {
	r.skipping = ""
	if !r.active {
		return nil
	}
	chunkID, index, count := r.chunkID, r.nextIndex, r.count
	r.drop()
	return fmt.Errorf(
		"%w: %q abandoned after %d of %d chunks", ErrChunkSequence, chunkID, index, count,
	)
}

// skip makes Push pass over the rest of chunk's sequence, whose start was not on this stream, and
// names what it dropped.
func (r *Reassembler) skip(chunk RPCChunk) string {
	r.skipping = chunk.ChunkID
	return fmt.Sprintf("%q dropped: its start was not on this stream (chunk %d of %d came first)", chunk.ChunkID, chunk.Index, chunk.Count)
}

func (r *Reassembler) drop() {
	r.active = false
	r.payload = r.payload[:0]
}

// Placement is the rules the shipped decoder applies to where a chunk sits in its sequence, before
// it reads the chunk's declared length (worker-rpc.ts:248-260): an id of one to maxChunkIDBytes
// bytes, a count from two to maxChunkCount, and an index inside that count. It answers
// ErrChunkMetadata naming the rule a chunk breaks, and nil for a chunk at a place the decoder
// takes. The daemon's reader asks it of every chunk first (validate), and the shim's pump asks it
// alone before it holds a chunk with its sequence (shim's chunkHold), so the two never differ on
// where a chunk may sit.
func (c RPCChunk) Placement() error {
	switch {
	case c.ChunkID == "" || len(c.ChunkID) > maxChunkIDBytes:
		return fmt.Errorf("%w: chunk id of %d bytes", ErrChunkMetadata, len(c.ChunkID))
	case c.Count < 2 || c.Count > maxChunkCount:
		return fmt.Errorf("%w: a sequence of %d chunks", ErrChunkMetadata, c.Count)
	case c.Index < 0 || c.Index >= c.Count:
		return fmt.Errorf("%w: chunk %d of %d", ErrChunkMetadata, c.Index, c.Count)
	}
	return nil
}

// validate is every rule the shipped decoder applies to a chunk's own numbers before it looks at
// the sequence (worker-rpc.ts:248-267): its place (Placement), then its declared length.
func (c RPCChunk) validate() error {
	if err := c.Placement(); err != nil {
		return err
	}
	if c.ByteLength < MaxFrameBytes || c.ByteLength > MaxReassembledBytes {
		return fmt.Errorf("%w: a declared length of %d bytes", ErrChunkMetadata, c.ByteLength)
	}
	return nil
}

// decodeChunkData decodes one chunk's payload, refusing anything that is not canonical base64 —
// the shipped decoder re-encodes to check exactly that (worker-rpc.ts:151-157) — and anything
// larger than one chunk may carry (worker-rpc.ts:275-278).
func decodeChunkData(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("%w: empty payload", ErrChunkData)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrChunkData, err)
	}
	if len(payload) > ChunkPayloadBytes {
		return nil, fmt.Errorf(
			"%w: %d bytes, limit %d", ErrChunkData, len(payload), ChunkPayloadBytes,
		)
	}
	return payload, nil
}

// validReassembled is what the bytes have to be once they are whole: the three checks the
// shipped decoder makes before dispatching the frame (worker-rpc.ts:312-329). UTF-8 is checked
// explicitly because Go's JSON scanner, unlike a fatal TextDecoder, does not mind an invalid
// byte inside a string.
func validReassembled(frame []byte) error {
	if !utf8.Valid(frame) {
		return fmt.Errorf("%w: not valid utf-8", ErrChunkPayload)
	}
	if !json.Valid(frame) {
		return fmt.Errorf("%w: not valid json", ErrChunkPayload)
	}
	if trimmed := bytes.TrimLeft(frame, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: not a json object", ErrChunkPayload)
	}
	return nil
}
