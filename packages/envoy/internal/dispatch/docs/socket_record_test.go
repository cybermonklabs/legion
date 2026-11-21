package docs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
)

// What the server writes to one document socket, recorded on the server's end and read back as
// the frames its page reads. The revoke rows of schema_admission_test.go read a revoked page's
// end off this record rather than off the page's socket, and size the document they seed from
// the socket buffers the kernel grants, which the helpers here read back.

// serverSendBufferRequest is the send buffer the tests ask the kernel for on every connection the
// server accepts: stock Linux's default net.core.wmem_max. A stock kernel caps a SO_SNDBUF
// request at that value and grants twice what it kept; a kernel whose cap is raised grants twice
// this request. Both therefore grant 425984 bytes, so no host runs this test on a buffer of its
// own choosing, and the test reads back what it was granted rather than trusting either (an
// explicit SO_SNDBUF also turns the kernel's own send-buffer tuning off, so the grant is all the
// socket ever holds).
const serverSendBufferRequest = 212992

// setSendBuffer asks for serverSendBufferRequest on tcp and returns the SO_SNDBUF the kernel
// granted, which is how many bytes that socket holds for a page that reads nothing.
func setSendBuffer(tcp *net.TCPConn) (int, error) {
	if err := tcp.SetWriteBuffer(serverSendBufferRequest); err != nil {
		return 0, err
	}
	return socketBuffer(tcp, syscall.SO_SNDBUF)
}

// socketBuffer reads back tcp's buffer of option, syscall.SO_SNDBUF or syscall.SO_RCVBUF: the
// bytes the kernel granted, which is not the number a request asked for.
func socketBuffer(tcp *net.TCPConn, option int) (int, error) {
	raw, err := tcp.SyscallConn()
	if err != nil {
		return 0, err
	}
	var granted int
	var readErr error
	if err := raw.Control(func(fd uintptr) {
		granted, readErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, option)
	}); err != nil {
		return 0, err
	}
	return granted, readErr
}

// sendBufferGrant is the send buffer the kernel grants a connection the server accepts, measured
// on a loopback connection of this test's own: a test's document is sized from it before it is
// seeded - the slow page's to fit it whole, since a write the buffer cannot take blocks and a
// blocked write has put no bytes on the wire for the test to read, and the in-flight page's to
// overrun it.
func sendBufferGrant(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen to measure the send buffer the kernel grants: %v", err)
	}
	defer listener.Close()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("connect to measure the send buffer the kernel grants: %v", err)
	}
	defer dialed.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept to measure the send buffer the kernel grants: %v", err)
	}
	defer accepted.Close()
	tcp, ok := accepted.(*net.TCPConn)
	if !ok {
		t.Fatalf("the loopback listener accepted a %T, want a TCP connection whose send buffer this can set", accepted)
	}
	granted, err := setSendBuffer(tcp)
	if err != nil {
		t.Fatalf("set and read back a send buffer of %d bytes: %v", serverSendBufferRequest, err)
	}
	return granted
}

// receiveBufferControl is a net.Dialer's Control that asks for request as the socket's SO_RCVBUF
// before it connects, so the receive window the connection opens with is the one that buffer
// allows.
func receiveBufferControl(request int) func(network, address string, raw syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var setErr error
		if err := raw.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, request)
		}); err != nil {
			return err
		}
		return setErr
	}
}

// receiveBufferGrant is the receive buffer the kernel grants a page that asks for request before it
// connects (receiveBufferControl), measured on a loopback connection of this test's own before
// the document is seeded, as sendBufferGrant measures the server's send buffer.
func receiveBufferGrant(t *testing.T, request int) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen to measure the receive buffer the kernel grants: %v", err)
	}
	defer listener.Close()
	dialed, err := (&net.Dialer{Control: receiveBufferControl(request)}).Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("connect asking for a receive buffer of %d bytes: %v", request, err)
	}
	defer dialed.Close()
	tcp, ok := dialed.(*net.TCPConn)
	if !ok {
		t.Fatalf("the loopback dial made a %T, want a TCP connection whose receive buffer this can read", dialed)
	}
	granted, err := socketBuffer(tcp, syscall.SO_RCVBUF)
	if err != nil {
		t.Fatalf("read back the receive buffer of %d bytes asked for: %v", request, err)
	}
	return granted
}

// recordingServer is a document service served over HTTP through a recordingListener, from which
// httptest's Start takes the server's URL, so a test can read exactly what the server wrote to a
// page it dialled. It closes when the test ends.
type recordingServer struct {
	*httptest.Server
	accepted *recordingListener
}

func newRecordingServer(t *testing.T, service *Service) *recordingServer {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(service.ServeHTTP))
	accepted := &recordingListener{Listener: server.Listener}
	server.Listener = accepted
	server.Start()
	t.Cleanup(server.Close)
	return &recordingServer{Server: server, accepted: accepted}
}

// dialUnreadPage is dialPage on a connection that asks for receiveBuffer as its SO_RCVBUF before
// it connects (receiveBufferControl), which bounds what the page's kernel takes off the wire while
// the page reads nothing, so the server's write of a document bigger than its buffers stops part
// way. It returns the page's socket, which must be accepted and closes when the test ends, and
// the server's end of it, which records what the server writes to that page.
func (s *recordingServer) dialUnreadPage(t *testing.T, artifactID string, query url.Values, receiveBuffer int) (*gws.Conn, *serverSocket) {
	t.Helper()
	dialer := gws.Dialer{NetDialContext: (&net.Dialer{Control: receiveBufferControl(receiveBuffer)}).DialContext}
	address := "ws" + strings.TrimPrefix(s.URL, "http") + "/ws/doc/" + artifactID + "?" + query.Encode()
	connection, response, err := dialer.Dial(address, http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("connect a page presenting %v with a receive buffer of %d bytes: response=%#v err=%v", query, receiveBuffer, response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	// ygo sends a joining peer the room's state only in answer to that peer's own sync step 1
	// (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go, reearth/ygo#342), so a page that
	// asks for nothing is owed nothing and the writes below would never fill its buffer. The
	// token follows it, as a provider sends it once its socket opens, so the room's answer to it
	// is queued behind the document: a frame after the answer's SyncStep2 says the server has
	// moved on from writing the document (waitForTheDocumentOnTheWire).
	sendSyncStep1(t, connection, artifactID)
	sendToken(t, connection, artifactID, query.Get("schema_version"))
	socket := s.accepted.find(connection.LocalAddr().String())
	if socket == nil {
		t.Fatalf("no accepted connection has the page's local address %s, so nothing records what the server writes to it", connection.LocalAddr())
	}
	return connection, socket
}

// recordingListener hands every accepted connection out as a serverSocket, so a test can find
// the one belonging to a page it dialled and read exactly what the server wrote to it. gorilla's
// Hijack hands ygo that same connection.
type recordingListener struct {
	net.Listener
	mu       sync.Mutex
	accepted []*serverSocket
}

func (l *recordingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		if _, err := setSendBuffer(tcp); err != nil {
			_ = tcp.Close()
			return nil, err
		}
	}
	socket := &serverSocket{Conn: connection}
	l.mu.Lock()
	l.accepted = append(l.accepted, socket)
	l.mu.Unlock()
	return socket, nil
}

// find is the accepted connection whose remote address is remote, a page's local address, or nil
// when no connection has it.
func (l *recordingListener) find(remote string) *serverSocket {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, socket := range l.accepted {
		if socket.RemoteAddr().String() == remote {
			return socket
		}
	}
	return nil
}

// socketWrite is one Write to a server socket: when it began, when it returned (zero while it has
// not), and the bytes the connection accepted of it - the n it returned - which are the bytes that
// went on the wire.
type socketWrite struct {
	startedAt  time.Time
	returnedAt time.Time
	bytes      []byte
}

// serverSocket is the server's end of one accepted connection, recording every write and when
// the server first closed it. The writes are recorded in the order they began, which is the order
// their bytes went out in: ygo writes to a peer from one goroutine of its own, and gorilla
// serialises that writer against the WriteControl a revoke's close frame goes out on.
type serverSocket struct {
	net.Conn
	mu       sync.Mutex
	writes   []socketWrite
	closedAt time.Time
}

func (s *serverSocket) Write(payload []byte) (int, error) {
	s.mu.Lock()
	at := len(s.writes)
	s.writes = append(s.writes, socketWrite{startedAt: time.Now()})
	s.mu.Unlock()
	n, err := s.Conn.Write(payload)
	returnedAt := time.Now()
	s.mu.Lock()
	s.writes[at].returnedAt = returnedAt
	s.writes[at].bytes = append([]byte(nil), payload[:n]...)
	s.mu.Unlock()
	return n, err
}

func (s *serverSocket) Close() error {
	now := time.Now()
	s.mu.Lock()
	if s.closedAt.IsZero() {
		s.closedAt = now
	}
	s.mu.Unlock()
	return s.Conn.Close()
}

// record is every write this connection has taken so far and the instant the server first closed
// it, zero while it is still open.
func (s *serverSocket) record() ([]socketWrite, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]socketWrite(nil), s.writes...), s.closedAt
}

// streamOf is the byte stream a socket's writes put on the wire: the bytes each write was
// accepted for, in the order the writes began.
func streamOf(writes []socketWrite) []byte {
	var stream []byte
	for _, write := range writes {
		stream = append(stream, write.bytes...)
	}
	return stream
}

// The websocket opcodes a server may write (RFC 6455, section 5.2); every other value of those
// four bits is reserved, and a stream that reads one is not a stream of whole frames.
const (
	opcodeContinuation byte = 0x0
	opcodeText         byte = 0x1
	opcodeBinary       byte = 0x2
	opcodeClose        byte = 0x8
	opcodePing         byte = 0x9
	opcodePong         byte = 0xa
)

// websocketFrame is one frame at the head of a server's byte stream: its opcode, the payload
// bytes the stream holds of it, the payload length its header declares - which is what the server
// wrote, whole or part way out - and whether the stream held the whole frame. A frame whose
// header is itself cut short declares nothing, so its length is the bytes it holds.
type websocketFrame struct {
	opcode  byte
	payload []byte
	length  int
	whole   bool
}

// maxFrameLength bounds the payload length a header may declare, far above any message ygo
// writes: a longer one is a header no server wrote, not a message to make room for.
const maxFrameLength = 1 << 31

// nextServerFrame reads the frame at the head of a server's stream, which must not be empty, and
// returns it with the bytes after it. A stream that ends inside the frame yields the payload
// bytes it holds, whole false and no bytes after. It errors on a header no server wrote: a
// reserved opcode or reserved bit, which is what a frame cut in half and resumed reads as, the
// mask only a client sets, or a payload length past maxFrameLength.
func nextServerFrame(stream []byte) (websocketFrame, []byte, error) {
	opcode := stream[0] & 0x0f
	switch opcode {
	case opcodeContinuation, opcodeText, opcodeBinary, opcodeClose, opcodePing, opcodePong:
	default:
		return websocketFrame{}, nil, fmt.Errorf("a frame whose first byte is %#02x carries the reserved opcode %#x", stream[0], opcode)
	}
	if stream[0]&0x70 != 0 {
		return websocketFrame{}, nil, fmt.Errorf("a frame whose first byte is %#02x sets a reserved bit", stream[0])
	}
	if len(stream) < 2 {
		return websocketFrame{opcode: opcode}, nil, nil
	}
	if stream[1]&0x80 != 0 {
		return websocketFrame{}, nil, fmt.Errorf("a frame whose second byte is %#02x is masked, which a server never does", stream[1])
	}
	length, header := uint64(stream[1]&0x7f), 2
	switch length {
	case 126:
		if header = 4; len(stream) < header {
			return websocketFrame{opcode: opcode}, nil, nil
		}
		length = uint64(binary.BigEndian.Uint16(stream[2:4]))
	case 127:
		if header = 10; len(stream) < header {
			return websocketFrame{opcode: opcode}, nil, nil
		}
		length = binary.BigEndian.Uint64(stream[2:10])
	}
	if length > maxFrameLength {
		return websocketFrame{}, nil, fmt.Errorf("a frame declaring %d payload bytes, past the %d no message reaches", length, maxFrameLength)
	}
	body := stream[header:]
	if length > uint64(len(body)) {
		return websocketFrame{opcode: opcode, payload: body, length: int(length)}, nil, nil
	}
	return websocketFrame{opcode: opcode, payload: body[:length], length: int(length), whole: true}, body[length:], nil
}

// closeFrameCode reads payload as one whole close frame of a server's and returns the code it
// carries. Anything else - a data frame, a frame cut short, a frame with bytes after it, a close
// with no code - is not one.
func closeFrameCode(payload []byte) (int, bool) {
	frame, after, err := nextServerFrame(payload)
	if err != nil || !frame.whole || frame.opcode != opcodeClose || len(after) > 0 || len(frame.payload) < 2 {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(frame.payload)), true
}

// serverStreamEnd is how the bytes a server wrote to one socket end, which is the end its page
// reads off that socket.
type serverStreamEnd string

const (
	// endsOnTheRevokedClose: the stream's last frame is the admission-revoked close frame, which
	// the page reads as that code.
	endsOnTheRevokedClose serverStreamEnd = "on the admission-revoked close frame"
	// endsAtACut: the stream stops with no close frame, at a frame boundary or inside a frame,
	// which the page reads as 1006.
	endsAtACut serverStreamEnd = "at a cut, which the page reads as 1006"
)

// serverStream is what the bytes a server wrote to one document socket amount to: how they end,
// the frames of the room's document in them, and the frame they stop inside, if they stop inside
// one. ygo sends a page the document in answer to the page's sync step 1, as sync Updates of at
// most stateFrameBytes a frame and then the SyncStep2 that ends the answer: documentFrames
// counts those frames read whole, documentBytes is the payload they carry, answered says the
// SyncStep2 is among them, and framesAfter counts the whole frames after it, which say the server
// has moved on from writing the document. cutLength is the payload a frame the stream stops
// inside declares and cutHeld how much of it the stream holds, both 0 when it stops at a frame's
// end.
type serverStream struct {
	end            serverStreamEnd
	documentFrames int
	documentBytes  int
	answered       bool
	framesAfter    int
	cutLength      int
	cutHeld        int
}

// readServerStream reads the bytes a server wrote to one document socket - its HTTP 101 response
// and the websocket frames after it. It errors on anything the page could not read as an end its
// provider reconnects on: a frame header no server wrote, a close frame carrying any other code,
// or a byte after a close frame.
func readServerStream(stream []byte) (serverStream, error) {
	head := bytes.Index(stream, []byte("\r\n\r\n"))
	if head < 0 {
		return serverStream{}, fmt.Errorf("the %d bytes the server wrote hold no whole HTTP response head, so the socket never became a websocket", len(stream))
	}
	read := serverStream{end: endsAtACut}
	for rest := stream[head+4:]; len(rest) > 0; {
		offset := len(stream) - len(rest)
		frame, after, err := nextServerFrame(rest)
		if err != nil {
			return serverStream{}, fmt.Errorf("%d bytes in: %w", offset, err)
		}
		if !frame.whole {
			read.cutLength, read.cutHeld = frame.length, len(frame.payload)
			return read, nil
		}
		if frame.opcode == opcodeClose {
			if len(frame.payload) < 2 {
				return serverStream{}, fmt.Errorf("the close frame %d bytes in carries %d bytes, too few for a code", offset, len(frame.payload))
			}
			if code := int(binary.BigEndian.Uint16(frame.payload)); code != contracts.DocumentAdmissionRevokedCloseCode {
				return serverStream{}, fmt.Errorf("the close frame %d bytes in carries code %d, want the admission-revoked %d", offset, code, contracts.DocumentAdmissionRevokedCloseCode)
			}
			if len(after) > 0 {
				return serverStream{}, fmt.Errorf("%d bytes follow the close frame %d bytes in", len(after), offset)
			}
			read.end = endsOnTheRevokedClose
			return read, nil
		}
		if read.answered {
			read.framesAfter++
		} else if document, last := documentFrame(frame.payload); frame.opcode == opcodeBinary && document {
			read.documentFrames++
			read.documentBytes += frame.length
			read.answered = last
		}
		rest = after
	}
	return read, nil
}

// documentFrame reads payload, one whole binary message of a document socket's server, as a
// room's frame (decodeRoomFrame). It reports whether that message carries the room's document - a
// sync Update or a SyncStep2 - and whether it is that SyncStep2, which ends ygo's answer to a
// page's sync step 1.
func documentFrame(payload []byte) (document, last bool) {
	frame, err := decodeRoomFrame(payload)
	if err != nil || frame.tag != hocuspocusSync {
		return false, false
	}
	switch frame.sync {
	case ygsync.MsgUpdate:
		return true, false
	case ygsync.MsgSyncStep2:
		return true, true
	}
	return false, false
}

// serverFrame is one unmasked, final server frame of opcode carrying payload, with the length
// encoding that payload's size calls for.
func serverFrame(opcode byte, payload []byte) []byte {
	frame := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, byte(n))
	case n < 1<<16:
		frame = binary.BigEndian.AppendUint16(append(frame, 126), uint16(n))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 127), uint64(n))
	}
	return append(frame, payload...)
}

// readServerStream is what the two revoke tests of schema_admission_test.go read a socket's record
// off, so these are the streams it has to tell apart: the two ends the provider reconnects on, and
// three a revoked page could read as neither. The document in each crosses in frames as ygo cuts
// it - Updates, then the SyncStep2 that ends the answer - among frames that are not the document,
// which the reader must not count: the sync step 1 the server asks with and its answer to the
// page's token. The frames after the SyncStep2 are how the slow-page test knows the server has
// finished writing the document, and the frame a stream stops inside is the one the in-flight test
// watches go out.
func TestTheServerStreamReaderTellsARevokedPagesEndsApart(t *testing.T) {
	const head = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n"
	// hocuspocus is one Hocuspocus message of the document "document" of type kind
	// (hocuspocusSync, or 2 for authentication), body after its type.
	hocuspocus := func(kind uint64, body func(*encoding.Encoder)) []byte {
		return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
			encoder.WriteVarString("document")
			encoder.WriteVarUint(kind)
			body(encoder)
		})
	}
	// syncMessage is a Hocuspocus sync message of ygo's sync type kind carrying size bytes.
	syncMessage := func(kind uint64, size int) []byte {
		return hocuspocus(hocuspocusSync, func(encoder *encoding.Encoder) {
			encoder.WriteVarUint(kind)
			encoder.WriteVarBytes(bytes.Repeat([]byte("d"), size))
		})
	}
	update, step2 := syncMessage(ygsync.MsgUpdate, 60<<10), syncMessage(ygsync.MsgSyncStep2, 8<<10)
	updateFrame, step2Frame := serverFrame(opcodeBinary, update), serverFrame(opcodeBinary, step2)
	step1Frame := serverFrame(opcodeBinary, syncMessage(ygsync.MsgSyncStep1, 1))
	tokenAnswer := serverFrame(opcodeBinary, hocuspocus(2, func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(2) // Authenticated.
		encoder.WriteVarString("read-write")
	}))
	revokedClose := serverFrame(opcodeClose, binary.BigEndian.AppendUint16(nil, uint16(contracts.DocumentAdmissionRevokedCloseCode)))
	stream := func(parts ...[]byte) []byte {
		return bytes.Join(append([][]byte{[]byte(head)}, parts...), nil)
	}
	for _, c := range []struct {
		name    string
		stream  []byte
		want    serverStream
		wantErr string
	}{
		{
			name:   "the document's frames and the token's answer, ended by the admission-revoked close",
			stream: stream(step1Frame, updateFrame, updateFrame, step2Frame, tokenAnswer, revokedClose),
			want: serverStream{end: endsOnTheRevokedClose, documentFrames: 3, documentBytes: 2*len(update) + len(step2),
				answered: true, framesAfter: 1},
		},
		{
			name:   "a stream cut inside a frame of the document, which the page reads as 1006",
			stream: stream(step1Frame, updateFrame, updateFrame[:len(updateFrame)-1000]),
			want: serverStream{end: endsAtACut, documentFrames: 1, documentBytes: len(update),
				cutLength: len(update), cutHeld: len(update) - 1000},
		},
		{
			name:    "a close frame carrying another code",
			stream:  stream(updateFrame, serverFrame(opcodeClose, binary.BigEndian.AppendUint16(nil, 1000))),
			wantErr: "carries code 1000",
		},
		{
			name:    "a byte after the close frame",
			stream:  stream(updateFrame, revokedClose, []byte{0x81}),
			wantErr: "follow the close frame",
		},
		{
			name:    "a reserved opcode, which is what a frame cut in half and resumed reads as",
			stream:  stream(updateFrame, serverFrame(0x3, nil)),
			wantErr: "reserved opcode",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			read, err := readServerStream(c.stream)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("readServerStream = %+v, %v; want an error naming %q", read, err, c.wantErr)
				}
				return
			}
			if err != nil || read != c.want {
				t.Fatalf("readServerStream = %+v, %v; want %+v, no error", read, err, c.want)
			}
		})
	}
}
