package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// lineConn is a daemon connection that takes its first limit lines whole and refuses every later
// one, as a connection whose peer has hung up does. The outbox writes each line with one Write
// (shimwire.Writer), so lines is exactly what the daemon's reader would have read.
type lineConn struct {
	net.Conn // nil: the outbox calls Write and Close alone
	limit    int
	lines    []string
	closed   bool
}

func (c *lineConn) Write(p []byte) (int, error) {
	if len(c.lines) == c.limit {
		return 0, syscall.EPIPE
	}
	c.lines = append(c.lines, strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func (c *lineConn) Close() error {
	c.closed = true
	return nil
}

// outboxShim is a shim with nothing but its outbox and the log the outbox writes to.
func outboxShim() (*shim, *strings.Builder) {
	var logged strings.Builder
	s := &shim{log: log.New(&logged, "", 0)}
	s.out.log = s.log
	return s, &logged
}

// attachConn attaches a connection that breaks after limit lines, as connect does once the
// daemon has acked.
func attachConn(s *shim, limit int) *lineConn {
	conn := &lineConn{limit: limit}
	s.out.attach(conn, shimwire.NewWriter(conn))
	return conn
}

// plain is one of OMP's frames, which the pump forwards as one line.
func plain(n int) []byte {
	return fmt.Appendf(nil, `{"type":"fake_frame","n":%d}`, n)
}

// bundleResult is a frame over the line limit, which toDaemonChunked sends as an rpc_chunk
// sequence, with the line it encodes to and the number of chunks that carry it.
func bundleResult(t *testing.T) (frame shimwire.Frame, line []byte, chunks int) {
	t.Helper()
	frame = shimwire.BundleWorkspaceResult{ID: "bundle-1", OK: true, SizeBytes: 1 << 20, Bundle: bytes.Repeat([]byte{0xa5}, 1<<20)}
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode the bundle result: %v", err)
	}
	chunks = (len(line) + shimwire.ChunkPayloadBytes - 1) / shimwire.ChunkPayloadBytes
	if len(line) < shimwire.MaxFrameBytes || chunks < 3 {
		t.Fatalf("the bundle result is %d bytes in %d chunks, want a sequence of three chunks or more", len(line), chunks)
	}
	return frame, line, chunks
}

// reassembled reads lines as the daemon's reader would on a stream of its own: one sequence,
// from its chunk 0 and in order, back into the frame it carries.
func reassembled(t *testing.T, lines []string) []byte {
	t.Helper()
	var r shimwire.Reassembler
	for i, line := range lines {
		frame, err := shimwire.Decode([]byte(line))
		chunk, ok := frame.(shimwire.RPCChunk)
		if err != nil || !ok {
			t.Fatalf("line %d is %.80s, want an rpc_chunk", i, line)
		}
		if chunk.Index != i {
			t.Fatalf("line %d is chunk %d of %d; want the sequence from its chunk 0, in order", i, chunk.Index, chunk.Count)
		}
		whole, err := r.Push(chunk)
		if err != nil {
			t.Fatalf("the daemon's reader refused chunk %d: %v", chunk.Index, err)
		}
		if whole != nil {
			if i != len(lines)-1 {
				t.Fatalf("the sequence ended at line %d of %d", i, len(lines))
			}
			return whole
		}
	}
	t.Fatalf("the %d lines never completed their sequence", len(lines))
	return nil
}

// A connection that breaks under a chunk of a sequence never carried that frame whole, and the
// next connection's reader holds nothing of it: so the whole sequence, from its chunk 0, waits
// for the next connection and is replayed in order, ahead of anything newer. A connection that
// breaks again while the sequence is replayed leaves it whole for the one after.
func TestASequenceABrokenConnectionCutIsReplayedWholeFromItsFirstChunk(t *testing.T) {
	s, _ := outboxShim()
	frame, line, chunks := bundleResult(t)

	first := attachConn(s, 3) // a plain line, chunks 0 and 1, and it breaks under chunk 2
	s.out.send(plain(1))
	s.toDaemonChunked(frame)
	s.out.send(plain(2))
	if !first.closed {
		t.Fatal("the broken connection was left open; closing it is what tells the daemon's reader")
	}

	second := attachConn(s, 2) // chunks 0 and 1 of the replay, and it breaks under chunk 2
	if !second.closed {
		t.Fatal("the connection that broke mid-replay was left open")
	}

	third := attachConn(s, math.MaxInt)
	if got := len(third.lines); got != chunks+1 {
		t.Fatalf("the replay is %d lines, want %d: the sequence's %d chunks and the plain line behind them: %.80q", got, chunks+1, chunks, third.lines)
	}
	if got := reassembled(t, third.lines[:chunks]); !bytes.Equal(got, line) {
		t.Fatalf("the replayed sequence carries %d bytes, want the %d of the bundle result", len(got), len(line))
	}
	if got := third.lines[chunks]; got != string(plain(2)) {
		t.Fatalf("the replay ends with %.80s, want the plain line sent after the sequence", got)
	}
	s.out.send(plain(3))
	if got := third.lines[len(third.lines)-1]; got != string(plain(3)) {
		t.Fatalf("a frame after the replay went to %.80s, want the attached connection", got)
	}
}

// The backlog keeps the newest backlogLimit lines, but it drops frames, never part of one: a
// sequence whose head the limit would cut goes whole, so no connection is handed the rest of a
// frame its reader can only throw away. Lines ahead of a sequence go first, one at a time, and
// the drop is logged once.
func TestATrimmedBacklogDropsAWholeSequenceNeverItsHeadAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		over int  // lines sent beyond backlogLimit
		kept bool // whether the sequence is still in the backlog
	}{
		{"the two lines ahead of the sequence dropped", 2, true},
		{"then the sequence, whole", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, logged := outboxShim()
			frame, line, chunks := bundleResult(t)
			s.out.send(plain(0))
			s.out.send(plain(1))
			s.toDaemonChunked(frame)
			last := backlogLimit + tc.over - chunks // plain lines 2 to last-1 bring the total there
			for n := 2; n < last; n++ {
				s.out.send(plain(n))
			}

			replay := attachConn(s, math.MaxInt).lines
			var rest []string
			if tc.kept {
				if len(replay) < chunks {
					t.Fatalf("the replay is %d lines, want the sequence's %d first", len(replay), chunks)
				}
				if got := reassembled(t, replay[:chunks]); !bytes.Equal(got, line) {
					t.Fatalf("the kept sequence carries %d bytes, want the %d of the bundle result", len(got), len(line))
				}
				rest = replay[chunks:]
			} else {
				rest = replay
			}
			if len(rest) != last-2 {
				t.Fatalf("the replay holds %d lines after what was kept of the sequence, want %d plain lines; it starts %.80q", len(rest), last-2, replay[:min(len(replay), 2)])
			}
			for i, got := range rest {
				if want := string(plain(i + 2)); got != want {
					t.Fatalf("replayed line %d is %.80s, want %s", i, got, want)
				}
			}
			if got := strings.Count(logged.String(), "dropping oldest frames beyond the last 1000 lines"); got != 1 {
				t.Fatalf("the trim was logged %d times, want once; log:\n%s", got, logged.String())
			}
		})
	}
}

// ompChunkID is the id of the sequence ompSequence writes.
const ompChunkID = "omp-agent-end"

// ompSequence is a frame of Oh My Pi's over the line limit, a long turn's agent_end, with the
// rpc_chunk lines Oh My Pi writes for it (shimwire.SplitChunks), one line each.
func ompSequence(t *testing.T) (frame []byte, lines [][]byte) {
	t.Helper()
	frame = fmt.Appendf(nil, `{"type":"agent_end","messages":[{"role":"assistant","content":%q}]}`, strings.Repeat("x", 3<<20))
	chunks, err := shimwire.SplitChunks(ompChunkID, frame)
	if err != nil {
		t.Fatalf("chunk Oh My Pi's agent_end: %v", err)
	}
	for _, chunk := range chunks {
		line, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("encode an rpc_chunk frame: %v", err)
		}
		lines = append(lines, line)
	}
	return frame, lines
}

// pumpOf runs s's pump on a pipe that stands in for Oh My Pi's stdout. It returns the end Oh My
// Pi writes to, and the channel the pump closes once it has forwarded all that was written
// before that end was closed.
func pumpOf(t *testing.T, s *shim) (stdout *os.File, pumped <-chan struct{}) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the pipe for Oh My Pi's stdout: %v", err)
	}
	done := make(chan struct{})
	go s.pump(r, done)
	t.Cleanup(func() {
		_ = w.Close()
		<-done
	})
	return w, done
}

// writeLines writes lines to Oh My Pi's stdout, one line each.
func writeLines(t *testing.T, stdout *os.File, lines ...[]byte) {
	t.Helper()
	for _, line := range lines {
		if _, err := stdout.Write(append(bytes.Clone(line), '\n')); err != nil {
			t.Fatalf("write to Oh My Pi's stdout: %v", err)
		}
	}
}

// Oh My Pi writes a frame over the line limit as an rpc_chunk sequence, one chunk per line. The
// pump hands the sequence to the outbox as one frame once its last chunk is read, so a connection
// that breaks under it leaves the whole sequence, from its chunk 0, for the next connection, in
// order with the lines around it: never its tail alone, which the daemon's reader can only drop.
func TestASequenceOhMyPiWritesIsReplayedWholeFromItsFirstChunkAfterABreak(t *testing.T) {
	s, _ := outboxShim()
	frame, sequence := ompSequence(t)

	first := attachConn(s, 3) // a plain line and chunks 0 and 1, and it breaks under chunk 2
	stdout, pumped := pumpOf(t, s)
	writeLines(t, stdout, plain(1))
	writeLines(t, stdout, sequence...)
	writeLines(t, stdout, plain(2))
	_ = stdout.Close()
	<-pumped
	if !first.closed {
		t.Fatal("the broken connection was left open; closing it is what tells the daemon's reader")
	}
	if got := first.lines[0]; got != string(plain(1)) {
		t.Fatalf("the first connection took %.80s first, want the plain line written before the sequence", got)
	}

	second := attachConn(s, math.MaxInt)
	if got := len(second.lines); got != len(sequence)+1 {
		t.Fatalf("the replay is %d lines, want %d: the sequence's %d chunks and the plain line behind them: %.80q", got, len(sequence)+1, len(sequence), second.lines)
	}
	if got := reassembled(t, second.lines[:len(sequence)]); !bytes.Equal(got, frame) {
		t.Fatalf("the replayed sequence carries %d bytes, want the %d of Oh My Pi's agent_end", len(got), len(frame))
	}
	if got := second.lines[len(sequence)]; got != string(plain(2)) {
		t.Fatalf("the replay ends with %.80s, want the plain line written after the sequence", got)
	}
}

// The shim's own chunked frame, a bundle result (toDaemonChunked), can be sent while Oh My Pi is
// halfway through writing a sequence. The two go out one after the other, each whole, and neither
// lands between two chunks of the other: the daemon's reader builds one sequence at a time, and a
// chunk 0 in the middle of one abandons it.
func TestABundleResultSentWhileOhMyPiWritesASequenceNeverLandsBetweenItsChunks(t *testing.T) {
	s, _ := outboxShim()
	ompFrame, sequence := ompSequence(t)
	bundle, bundleLine, bundleChunks := bundleResult(t)
	conn := attachConn(s, math.MaxInt)

	stdout, pumped := pumpOf(t, s)
	writeLines(t, stdout, plain(1))
	// Eight chunks of some 350 KiB each are far more than a pipe holds (64 KiB on Linux), so when
	// the last write returns the pump has read the first of them: a pump that forwarded each line
	// as it read it would have sent them ahead of the bundle result.
	writeLines(t, stdout, sequence[:8]...)
	s.toDaemonChunked(bundle)
	writeLines(t, stdout, sequence[8:]...)
	writeLines(t, stdout, plain(2))
	_ = stdout.Close()
	<-pumped

	lines := conn.lines
	if want := len(sequence) + bundleChunks + 2; len(lines) != want {
		t.Fatalf("the connection took %d lines, want %d: two plain lines and the %d and %d chunks of the two sequences", len(lines), want, len(sequence), bundleChunks)
	}
	if lines[0] != string(plain(1)) || lines[len(lines)-1] != string(plain(2)) {
		t.Fatalf("the connection took %.80s first and %.80s last, want the two plain lines around the sequences", lines[0], lines[len(lines)-1])
	}
	between := lines[1 : len(lines)-1]
	head, err := shimwire.Decode([]byte(between[0]))
	chunk, ok := head.(shimwire.RPCChunk)
	if err != nil || !ok {
		t.Fatalf("the line after the first plain one is %.80s, want an rpc_chunk", between[0])
	}
	ompLines, bundleLines := between[:len(sequence)], between[len(sequence):]
	if chunk.ChunkID != ompChunkID {
		bundleLines, ompLines = between[:bundleChunks], between[bundleChunks:]
	}
	if got := reassembled(t, ompLines); !bytes.Equal(got, ompFrame) {
		t.Fatalf("Oh My Pi's sequence carries %d bytes, want the %d of its agent_end", len(got), len(ompFrame))
	}
	if got := reassembled(t, bundleLines); !bytes.Equal(got, bundleLine) {
		t.Fatalf("the bundle result's sequence carries %d bytes, want the %d of the bundle result", len(got), len(bundleLine))
	}
}

// The pump reads each of Oh My Pi's lines once, for the hold and the pane's summary both, and a
// line one of whose other members does not decode is still read as its type says: summarized when
// the pane shows that type, and sent alone, as one frame, whatever the type.
func TestThePumpReadsALinesTypeWhateverItsOtherMembersHold(t *testing.T) {
	for _, tc := range []struct {
		name, line, summary string
	}{
		{"an agent_start whose index is a string", `{"type":"agent_start","index":"one"}`, "agent_start"},
		{"an rpc_chunk whose count is a string", `{"type":"rpc_chunk","chunkId":"c","index":0,"count":"two"}`, ""},
		{"an agent_start cut short", `{"type":"agent_start"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, logged := outboxShim() // no connection: what the pump hands the outbox waits in its backlog
			stdout, pumped := pumpOf(t, s)
			writeLines(t, stdout, []byte(tc.line), []byte(tc.line))
			_ = stdout.Close()
			<-pumped
			want := ""
			if tc.summary != "" {
				want = strings.Repeat(tc.summary+"\n", 2)
			}
			if got := logged.String(); got != want {
				t.Fatalf("the pane's log is %q, want %q", got, want)
			}
			if !slices.Equal(s.out.frames, []int{1, 1}) || !slices.EqualFunc(s.out.backlog, [][]byte{[]byte(tc.line), []byte(tc.line)}, bytes.Equal) {
				t.Fatalf("the outbox holds frames of %v lines, want the two lines as one frame each", s.out.frames)
			}
		})
	}
}

// A chunk line whose place the daemon's reader refuses (shimwire's RPCChunk.Placement: an id empty
// or over its bound, a count under two or over the most a sequence may declare, an index under
// zero or at or past its count) starts no sequence the pump holds: it goes to the outbox as it is
// read, a frame of its own after what is held, as every line went before the pump held sequences.
// So a child that writes such lines, which no splitter does, makes the shim hold none of them,
// while a sequence of as many chunks as one may declare is still held whole.
func TestAChunkLineTheDaemonRefusesByItsPlaceGoesAloneAndIsNeverHeld(t *testing.T) {
	chunk := func(id string, index, count int) []byte {
		return fmt.Appendf(nil, `{"type":"rpc_chunk","chunkId":%q,"index":%d,"count":%d,"byteLength":%d,"data":""}`,
			id, index, count, shimwire.MaxFrameBytes)
	}
	// The most chunks a sequence may declare: the reassembly limit in full payloads.
	ceiling := shimwire.MaxReassembledBytes / shimwire.ChunkPayloadBytes
	// Each line but a group's last continues the one before it, so a hold that took them would keep
	// the whole group: past the ceiling, more chunks than any sequence may have.
	var refused [][]byte
	for index := range ceiling + 1 {
		refused = append(refused, chunk("over-the-ceiling", index, ceiling+1))
	}
	overLongID := strings.Repeat("i", 129)
	refused = append(refused,
		chunk("past-its-count", 3, 3), chunk("past-its-count", 4, 3),
		chunk("under-two", 0, 1), chunk("under-two", 1, 1),
		chunk("under-zero", -1, 3), chunk("under-zero", 0, 0),
		chunk("", 0, 3), chunk("", 1, 3),
		chunk(overLongID, 0, 3), chunk(overLongID, 1, 3),
	)
	for _, line := range refused {
		frame, err := shimwire.Decode(line)
		decoded, ok := frame.(shimwire.RPCChunk)
		if err != nil || !ok {
			t.Fatalf("%s decodes to %T, %v; want an rpc_chunk", line, frame, err)
		}
		var reader shimwire.Reassembler
		if _, err := reader.Push(decoded); !errors.Is(err, shimwire.ErrChunkMetadata) {
			t.Fatalf("the daemon's reader answers %s with %v, want ErrChunkMetadata: the case needs a chunk it refuses by its place", line, err)
		}
	}
	var atTheCeiling [][]byte
	for index := range ceiling {
		atTheCeiling = append(atTheCeiling, chunk("at-the-ceiling", index, ceiling))
	}

	s, _ := outboxShim() // no connection: what the pump hands the outbox waits in its backlog
	stdout, pumped := pumpOf(t, s)
	writeLines(t, stdout, plain(0))
	writeLines(t, stdout, refused...)
	writeLines(t, stdout, atTheCeiling...)
	writeLines(t, stdout, plain(1))
	_ = stdout.Close()
	<-pumped

	want := []int{1}
	for range refused {
		want = append(want, 1)
	}
	want = append(want, len(atTheCeiling), 1)
	if !slices.Equal(s.out.frames, want) {
		t.Fatalf("the outbox holds frames of %v lines; want the plain line, each of the %d refused chunks alone, the %d chunks at the ceiling as one frame, then the plain line", s.out.frames, len(refused), len(atTheCeiling))
	}
	written := slices.Concat([][]byte{plain(0)}, refused, atTheCeiling, [][]byte{plain(1)})
	if !slices.EqualFunc(s.out.backlog, written, bytes.Equal) {
		t.Fatal("the outbox holds other lines than the pump read, or in another order")
	}
}
