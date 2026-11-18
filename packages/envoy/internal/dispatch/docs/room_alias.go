package docs

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"

	gws "github.com/gorilla/websocket"
)

// A document socket asks for a document by its id, and the server decides the room it edits in
// (R5 of dispatch://INFRA-1253): a document converted into its issue's room is edited in that
// room, whose name is the issue's tree artifact's id. The Hocuspocus framing names the document at
// the head of every message, both ways (HocuspocusFraming, docs/service.go), and the browser's
// provider routes every message it reads by that name, so a socket answered under the room's name
// would sync nothing. ygo serves the socket under the room's name (ServeHTTP hands it the request
// so), so every message it writes names the room and every message it reads is the room's; the
// socket's frames are renamed on the connection the websocket upgrade hijacks, where the
// awareness filter sits (awareness_limit.go): from the name the socket asked for to the room's on
// the way in, and back on the way out. A canonical uuid is 36 bytes, so the two names are the
// same length and a renamed frame keeps its length and its header.

// roomAliasWriter is the response writer of a document socket ServeHTTP admitted to a room under
// another name than the one it asked for: the connection ygo's websocket upgrade hijacks through
// it is a roomAliasConn. It wraps the awareness filter's writer, so the frames it reads are the
// ones that filter wrote, and the frames it writes reach the socket through that filter's
// connection. Hijack resets the hijacked reader onto the alias, as the awareness filter's does onto
// itself, so gorilla reads the socket through it whichever reader it takes.
type roomAliasWriter struct {
	http.ResponseWriter
	asked, room string
}

func (w *roomAliasWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// What the reader holds and has not handed on is the socket's first bytes, which the reset
	// below drops, so the alias is given them to read first. Peek of what the reader already
	// holds reads nothing and cannot fail.
	var pending []byte
	if held := buffers.Reader.Buffered(); held > 0 {
		head, _ := buffers.Reader.Peek(held)
		pending = bytes.Clone(head)
	}
	aliased := newRoomAliasConn(conn, w.asked, w.room, pending)
	buffers.Reader.Reset(aliased)
	return aliased, buffers, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *roomAliasWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// roomAliasConn is a document socket's connection under ygo's websocket reader and writer, for a
// socket admitted to a room under another name: every message the socket sends that names the
// document it asked for reaches ygo naming the room, and every message ygo writes naming the room
// reaches the socket naming that document.
type roomAliasConn struct {
	net.Conn
	src io.Reader
	// in renames what the socket sends and out what ygo writes; each runs on its side's one
	// goroutine (ygo's reader, and gorilla's writer, which takes one write at a time).
	in, out *frameRenamer
	// read is what in has renamed and ygo's reader has yet to take; buffer is the read's scratch.
	read, buffer []byte
	// written is the write's scratch.
	written []byte
}

// newRoomAliasConn is the alias over conn, reading pending - what the upgrade's hijack held of
// the socket already - before conn itself.
func newRoomAliasConn(conn net.Conn, asked, room string, pending []byte) *roomAliasConn {
	var source io.Reader = conn
	if len(pending) > 0 {
		source = io.MultiReader(bytes.NewReader(pending), conn)
	}
	return &roomAliasConn{
		Conn: conn, src: source,
		in:  &frameRenamer{from: []byte(asked), to: []byte(room)},
		out: &frameRenamer{from: []byte(room), to: []byte(asked), response: true},
	}
}

// Read hands ygo the socket's frames, each message that names the document asked for renamed to
// the room. It reads the socket until it has bytes for ygo, as the socket's own reads would block
// until a frame arrives: a message's name is held back until the whole of it has arrived.
func (c *roomAliasConn) Read(p []byte) (int, error) {
	for len(c.read) == 0 {
		if cap(c.buffer) < len(p) {
			c.buffer = make([]byte, max(len(p), 4096))
		}
		n, err := c.src.Read(c.buffer[:len(p)])
		c.read = c.in.rename(c.read[:0], c.buffer[:n])
		if err != nil && len(c.read) == 0 {
			return 0, err
		}
	}
	n := copy(p, c.read)
	c.read = c.read[n:]
	return n, nil
}

// Write hands the socket what ygo wrote, each message that names the room renamed to the document
// the socket asked for: the upgrade's answer as it is, then every frame. A message's name ygo
// writes across two writes is held back until the whole of it is written.
func (c *roomAliasConn) Write(p []byte) (int, error) {
	c.written = c.out.rename(c.written[:0], p)
	if len(c.written) > 0 {
		if _, err := c.Conn.Write(c.written); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// frameRenamer renames the document a Hocuspocus message names in a stream of websocket frames,
// from one name to another of the same length (roomAliasConn). A message opens with its
// document's name as a varstring: one byte of length, for a name under 128 bytes, then the name.
// The first frame of a binary message carries it at the start of its payload, which the awareness
// filter keeps whole on the way in (awarenessLimitConn.decide) and gorilla writes in one frame on
// the way out; a name that differs is passed through as it came.
type frameRenamer struct {
	from, to []byte
	// response is set while the stream begins with an HTTP response, the upgrade's answer, which
	// passes through as it is up to the blank line that ends it; ended counts the bytes of that
	// blank line's "\r\n\r\n" matched so far.
	response bool
	ended    int
	// header holds the frame header being read, of want bytes once its first two are in.
	header [maxFrameHeader]byte
	have   int
	// payload is the payload bytes of the frame in hand not yet read, offset how many of them
	// were, key the frame's masking key where it is masked, and named whether its payload opens a
	// message whose name may be renamed.
	payload, offset int64
	key             [4]byte
	masked, named   bool
	// held is the bytes of the name being read, held back until it is whole.
	held []byte
}

// rename appends to out what in carries, renamed, and returns out. What is held back of a name is
// appended on a later call, once the name is whole or its frame ends.
func (f *frameRenamer) rename(out, in []byte) []byte {
	for len(in) > 0 {
		switch {
		case f.response:
			at := 0
			for at < len(in) && f.ended < 4 {
				if in[at] == "\r\n\r\n"[f.ended] {
					f.ended++
				} else if in[at] == '\r' {
					f.ended = 1
				} else {
					f.ended = 0
				}
				at++
			}
			out = append(out, in[:at]...)
			in = in[at:]
			f.response = f.ended < 4
		case f.payload > 0:
			n := int(min(f.payload, int64(len(in))))
			out = f.payloadBytes(out, in[:n])
			in = in[n:]
		default:
			out, in = f.headerBytes(out, in)
		}
	}
	return out
}

// headerBytes reads a frame header off in, passes it to out, and starts the frame's payload.
func (f *frameRenamer) headerBytes(out, in []byte) ([]byte, []byte) {
	want := 2
	if f.have >= 2 {
		want = frameHeaderLength(f.header[:2])
	}
	for f.have < want && len(in) > 0 {
		f.header[f.have] = in[0]
		f.have++
		in = in[1:]
		if f.have == 2 {
			want = frameHeaderLength(f.header[:2])
		}
	}
	if f.have < want {
		return out, in
	}
	out = append(out, f.header[:want]...)
	opcode := f.header[0] & 0x0f
	f.payload = max(framePayloadLength(f.header[:want]), 0)
	f.offset = 0
	f.masked = f.header[1]&maskedFrame != 0
	if f.masked {
		copy(f.key[:], f.header[want-4:want])
	}
	f.named = opcode == gws.BinaryMessage
	f.have = 0
	if f.payload == 0 {
		f.named = false
	}
	return out, in
}

// payloadBytes passes part of the frame's payload to out, holding back the name a message opens
// with until it is whole, and renaming it then where it is the name renamed from.
func (f *frameRenamer) payloadBytes(out, part []byte) []byte {
	nameEnd := int64(1 + len(f.from))
	for len(part) > 0 {
		if !f.named || f.offset >= nameEnd {
			out = append(out, part...)
			f.offset += int64(len(part))
			f.payload -= int64(len(part))
			part = nil
			break
		}
		take := int(min(nameEnd-f.offset, int64(len(part))))
		f.held = append(f.held, part[:take]...)
		part = part[take:]
		f.offset += int64(take)
		f.payload -= int64(take)
		if f.offset == nameEnd {
			out = f.releaseName(out)
		}
	}
	if f.payload == 0 && len(f.held) > 0 {
		// The frame ended inside what a name would take: it names no document this renames.
		out = append(out, f.held...)
		f.held = f.held[:0]
	}
	if f.payload == 0 {
		f.named = false
	}
	return out
}

// releaseName appends the held name to out: renamed to f.to where it is f.from, as it came
// otherwise, each byte masked again under the frame's key where the frame is masked.
func (f *frameRenamer) releaseName(out []byte) []byte {
	plain := func(index int) byte {
		if f.masked {
			return f.held[index] ^ f.key[index%4]
		}
		return f.held[index]
	}
	same := plain(0) == byte(len(f.from))
	for index := 1; same && index < len(f.held); index++ {
		same = plain(index) == f.from[index-1]
	}
	if same {
		for index := 1; index < len(f.held); index++ {
			named := f.to[index-1]
			if f.masked {
				named ^= f.key[index%4]
			}
			f.held[index] = named
		}
	}
	out = append(out, f.held...)
	f.held = f.held[:0]
	return out
}
