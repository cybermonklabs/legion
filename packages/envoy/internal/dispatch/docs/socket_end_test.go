package docs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

// wireConn is a connection whose writes land on wire, in order.
type wireConn struct {
	net.Conn
	wire *bytes.Buffer
}

func (c wireConn) Write(p []byte) (int, error) { return c.wire.Write(p) }

// opaqueContext is a context the standard library cannot see into: it hides every value, so a
// child of it learns of its end only from a goroutine of its own (context.WithCancelCause), not in
// its cancel.
type opaqueContext struct{ context.Context }

func (opaqueContext) Value(any) any { return nil }

// An ended socket's connection lets out the rest of the message going out when the end came and
// the close frame, and nothing else, whatever the writes split: before the end a frame whose
// header two writes carry goes out whole; a frame of the message in flight whose header the end
// splits goes out; a room update split over two writes, a message begun after the end and a ping
// are dropped whole; and the close frame goes out. Every write is answered as written, and a
// message the page begins after the end is refused. The end is a revoke's, which cancels the
// socket's own context, or the API's, which cancels the request's: here through a context the
// standard library cannot see into, so the socket's own context learns of it only later, and the
// connection must read the request's itself. The same writes with no end all go out.
func TestAnEndedSocketsConnectionLetsOutOnlyTheMessageGoingOutAndTheClose(t *testing.T) {
	frame := func(opcode byte, final bool, payload []byte) []byte {
		return append(frameHeader(opcode, final, int64(len(payload)), nil), payload...)
	}
	upgrade := []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n")
	before := frame(gws.BinaryMessage, true, []byte("a frame before the end"))
	first := frame(gws.BinaryMessage, false, bytes.Repeat([]byte("a"), 300))
	middle := frame(continuationFrame, false, bytes.Repeat([]byte("b"), 70000))
	last := frame(continuationFrame, true, []byte("c"))
	update := frame(gws.BinaryMessage, true, []byte("a room update after the end"))
	begun := frame(gws.BinaryMessage, false, []byte("a message begun after the end"))
	begunEnd := frame(continuationFrame, true, []byte("its end"))
	ping := frame(gws.PingMessage, true, nil)
	closing := frame(gws.CloseMessage, true, binary.BigEndian.AppendUint16(nil, uint16(contracts.DocumentAdmissionRevokedCloseCode)))

	for _, c := range []struct {
		name string
		// end ends the socket, given the cancels of its own context and of the request's.
		end  func(own, request context.CancelCauseFunc)
		want [][]byte
	}{
		{"a revoke, which cancels the socket's own context", func(own, _ context.CancelCauseFunc) { own(errSchemaRevoked) },
			[][]byte{upgrade, before, first, middle, last, closing}},
		{"the API's end of the request, through a context the standard library cannot see into", func(_, request context.CancelCauseFunc) { request(context.Canceled) },
			[][]byte{upgrade, before, first, middle, last, closing}},
		{"no end", nil, [][]byte{upgrade, before, first, middle, update, last, begun, begunEnd, ping, closing}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var wire bytes.Buffer
			base, endRequest := context.WithCancelCause(context.Background())
			defer endRequest(nil)
			request := opaqueContext{base}
			own, endOwn := context.WithCancelCause(request)
			defer endOwn(nil)
			end := newSocketEnd(own, request)
			conn := &socketEndConn{Conn: wireConn{wire: &wire}, end: end}
			write := func(p []byte) {
				t.Helper()
				if n, err := conn.Write(p); n != len(p) || err != nil {
					t.Fatalf("a write of %d bytes answered %d, %v; want every byte written", len(p), n, err)
				}
			}
			write(upgrade)
			write(before[:1])
			write(before[1:])
			write(first)
			// The middle frame's header is ten bytes; the end comes after the first five.
			write(middle[:5])
			if end.refusesMessage() {
				t.Fatal("the connection refuses the page's messages before the end")
			}
			if c.end != nil {
				c.end(endOwn, endRequest)
			}
			write(middle[5:])
			write(update[:3])
			write(update[3:])
			write(last)
			write(begun)
			write(begunEnd)
			write(ping)
			write(closing)
			if want := bytes.Join(c.want, nil); !bytes.Equal(wire.Bytes(), want) {
				t.Fatalf("the connection let out %d bytes, want %d: %s", wire.Len(), len(want), describeWire(wire.Bytes(), want))
			}
			frames, ended := end.framesWrittenBeforeEnd()
			if ended != (c.end != nil) || (ended && frames != 2) {
				t.Fatalf("framesWrittenBeforeEnd = %d, %t; want 2 frames let out before an end (the frame before it and the message's first), ended %t",
					frames, ended, c.end != nil)
			}
			if refuses := end.refusesMessage(); refuses != (c.end != nil) {
				t.Fatalf("after the writes the connection refuses the page's messages = %t, want %t", refuses, c.end != nil)
			}
		})
	}
}

// describeWire says where got first differs from want.
func describeWire(got, want []byte) string {
	at := 0
	for at < min(len(got), len(want)) && got[at] == want[at] {
		at++
	}
	return fmt.Sprintf("they differ from byte %d", at)
}

// endBetween reads before's bytes and then after's, and calls end once, the first time it is read
// past before. The filter reads a frame's bytes only as it reaches that frame, so every frame of
// before is decided before the end, and every frame of after once it has come.
type endBetween struct {
	before, after *bytes.Reader
	end           func()
	ended         bool
}

func (r *endBetween) Read(p []byte) (int, error) {
	if r.before.Len() > 0 {
		return r.before.Read(p)
	}
	if !r.ended {
		r.ended = true
		r.end()
	}
	return r.after.Read(p)
}

// A page can send an edit as a message of its own after a non-final frame. gorilla takes such a
// frame as a new message whenever the filter has handed it nothing of the open one, so the filter
// must refuse an edit begun after the end in every mode it can be in, not only when no message is
// open. Each sequence goes through the filter over an ended socket, and ygo must read no message
// holding the edit: a FIN frame alone; a non-final frame refused after the end, then the edit as
// a FIN binary frame, or as a FIN text frame; and a non-final awareness frame sent before the end,
// then the edit as a FIN binary frame after it. The last sequence with no end at all is the
// control: there the edit reaches ygo, so the row's search for it does find it when it is there.
func TestAnEndedSocketsFilterRefusesEveryMessageThePageBeginsAfterTheEnd(t *testing.T) {
	edit := documentMessage(limitRoom, 0, []byte("EDIT-AFTER-END"))
	editFrame := clientFrame(gws.BinaryMessage, true, edit)
	opening := clientFrame(gws.BinaryMessage, false, documentMessage(limitRoom, 0, []byte("an opening")))
	awarenessOpen := clientFrame(gws.BinaryMessage, false, awarenessMessageOf(limitRoom, noAwarenessChange))
	for _, c := range []struct {
		name          string
		before, after [][]byte
		ends          bool
	}{
		{"a FIN edit after the end", nil, [][]byte{editFrame}, true},
		{"a refused non-final frame, then a FIN binary edit", nil, [][]byte{opening, editFrame}, true},
		{"a refused non-final frame, then a FIN text edit", nil, [][]byte{opening, clientFrame(gws.TextMessage, true, edit)}, true},
		{"a non-final awareness frame before the end, then a FIN binary edit after it", [][]byte{awarenessOpen}, [][]byte{editFrame}, true},
		{"the same with no end (control)", [][]byte{awarenessOpen}, [][]byte{editFrame}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			own, endOwn := context.WithCancelCause(context.Background())
			defer endOwn(nil)
			source := &endBetween{
				before: bytes.NewReader(bytes.Join(c.before, nil)),
				after:  bytes.NewReader(bytes.Join(c.after, nil)),
				end:    func() {},
			}
			if c.ends {
				source.end = func() { endOwn(errSchemaRevoked) }
			}
			conn := newAwarenessLimitConn(&scriptedSocket{frames: source}, limitRoom, nil, newSocketEnd(own, context.Background()))
			reached := false
			for _, message := range filtered(t, conn) {
				if bytes.Contains(message.payload, []byte("EDIT-AFTER-END")) {
					reached = true
				}
			}
			if reached == c.ends {
				t.Fatalf("the edit reached ygo = %t; want %t (socket ended: %t)", reached, !c.ends, c.ends)
			}
		})
	}
}

// gorillaMessages is what gorilla's reader makes of a stream of a client's frames: the payload of
// each data message whose final frame it holds whole, unmasked. A frame cut short, and a message
// whose final frame never comes, are no message: gorilla's ReadMessage returns none for them, and
// ygo applies none. Control frames are no message either.
func gorillaMessages(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var messages [][]byte
	var open []byte
	inMessage := false
	for rest := stream; ; {
		if len(rest) < 2 {
			return messages
		}
		length, header := uint64(rest[1]&0x7f), 2
		switch length {
		case 126:
			if len(rest) < 4 {
				return messages
			}
			length, header = uint64(binary.BigEndian.Uint16(rest[2:4])), 4
		case 127:
			if len(rest) < 10 {
				return messages
			}
			length, header = binary.BigEndian.Uint64(rest[2:10]), 10
		}
		if rest[1]&0x80 == 0 {
			t.Fatalf("the filtered stream holds an unmasked frame, which a websocket server refuses: % x", rest[:header])
		}
		if uint64(len(rest)) < uint64(header)+4+length {
			return messages
		}
		key, payload := rest[header:header+4], rest[header+4:uint64(header)+4+length]
		opcode, final := rest[0]&0x0f, rest[0]&0x80 != 0
		rest = rest[uint64(header)+4+length:]
		switch {
		case opcode == gws.BinaryMessage || opcode == gws.TextMessage:
			open, inMessage = open[:0], true
		case opcode == continuationFrame && inMessage:
		default:
			continue
		}
		for i, b := range payload {
			open = append(open, b^key[i%4])
		}
		if final {
			messages = append(messages, bytes.Clone(open))
			inMessage = false
		}
	}
}

// R1 of INFRA-5142: no message the page completes after the socket's end is applied, whenever it
// began. A page can hold a message open before the end and complete it after: its edit, typed
// after the end, is in the final frame. Each sequence holds part of one 164-byte document message
// open before the end - its first byte, which leaves the filter deciding what the message is, or
// its 38-byte Hocuspocus header, which the filter has already passed on to gorilla, or all of it,
// so that the final frame after the end carries no byte at all - and after the end sends a ping and
// the final frame. The same holds of a text message, which the filter passes to gorilla unread,
// and of an awareness message, which it holds until its end. gorilla must complete no message from
// what the filter hands it. The same sequences with no end are the controls: there gorilla reads
// the whole message.
func TestAnEndedSocketsFilterCompletesNoMessageAfterTheEnd(t *testing.T) {
	edit := documentMessage(limitRoom, 0, []byte(strings.Repeat("EDIT-TYPED-AFTER-THE-END ", 5)+"!"))
	if len(edit) != 164 {
		t.Fatalf("the edit is %d bytes; the row holds open parts of a 164-byte message", len(edit))
	}
	awareness := awarenessMessageOf(limitRoom, noAwarenessChange)
	ping := clientFrame(gws.PingMessage, true, nil)
	held := func(opcode byte, message []byte, at int) (before, after [][]byte) {
		return [][]byte{clientFrame(opcode, false, message[:at])},
			[][]byte{ping, clientFrame(continuationFrame, true, message[at:])}
	}
	for _, c := range []struct {
		name    string
		opcode  byte
		message []byte
		at      int
	}{
		{"one byte held open (the filter deciding)", gws.BinaryMessage, edit, 1},
		{"the 38-byte header held open (already passed to gorilla)", gws.BinaryMessage, edit, 38},
		{"the whole message held open, its final frame empty", gws.BinaryMessage, edit, len(edit)},
		{"one byte of a text message held open (passed to gorilla unread)", gws.TextMessage, edit, 1},
		{"an awareness message held open", gws.BinaryMessage, awareness, len(awareness) - 1},
	} {
		for _, ends := range []bool{true, false} {
			name := c.name + ", no end (control)"
			if ends {
				name = c.name + ", completed after the end"
			}
			t.Run(name, func(t *testing.T) {
				own, endOwn := context.WithCancelCause(context.Background())
				defer endOwn(nil)
				before, after := held(c.opcode, c.message, c.at)
				source := &endBetween{
					before: bytes.NewReader(bytes.Join(before, nil)),
					after:  bytes.NewReader(bytes.Join(after, nil)),
					end:    func() {},
				}
				if ends {
					source.end = func() { endOwn(errSchemaRevoked) }
				}
				conn := newAwarenessLimitConn(&scriptedSocket{frames: source}, limitRoom, nil, newSocketEnd(own, context.Background()))
				stream, err := io.ReadAll(conn)
				if err != nil {
					t.Fatalf("read the filtered stream: %v", err)
				}
				messages := gorillaMessages(t, stream)
				switch {
				case ends && len(messages) > 0:
					t.Fatalf("gorilla completed %d message(s) from what the filter handed it after the end, the first of %d bytes: the room took a message the page completed after the end", len(messages), len(messages[0]))
				case !ends && len(messages) != 1:
					t.Fatalf("with no end gorilla completed %d message(s); want the one message the page sent", len(messages))
				case !ends && bytes.Equal(c.message, edit) && !bytes.Equal(messages[0], edit):
					t.Fatalf("with no end gorilla completed a %d-byte message; want the 164-byte edit, whole", len(messages[0]))
				}
			})
		}
	}
}
