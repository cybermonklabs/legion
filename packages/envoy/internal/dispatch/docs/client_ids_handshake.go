package docs

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"slices"
	"strconv"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/contracts"
)

// This file holds the client id rule's wire handshake (client_ids.go): the first frame of a
// document socket, which names the client id its admission gave the page (clientIDFrame), written
// right after the upgrade's answer (clientIDWriter, clientIDConn).

// hocuspocusStateless is the Hocuspocus message type of a stateless message, the frame that
// carries a socket's client id to its page.
const hocuspocusStateless = 5

// clientIDWriter is a document socket's response writer under ygo's upgrade: the connection the
// upgrade hijacks through it writes frame, the stateless message naming the client id the
// socket's admission gave its page, right after the upgrade's answer (clientIDConn), so it is
// the first frame the page reads.
type clientIDWriter struct {
	http.ResponseWriter
	frame []byte
}

func (w *clientIDWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &clientIDConn{Conn: conn, frame: w.frame}, buffers, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *clientIDWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// clientIDConn is a hijacked connection whose first write, the upgrade's 101 answer, is followed
// in the same write by frame. gorilla writes that answer in one write before it hands the
// connection to any other goroutine (processHeaderWriter), so frame is cleared by it and every
// later write passes straight through. A first write that is no 101 answer upgrades nothing, and
// frame goes nowhere.
type clientIDConn struct {
	net.Conn
	frame []byte
}

func (c *clientIDConn) Write(p []byte) (int, error) {
	frame := c.frame
	if frame == nil {
		return c.Conn.Write(p)
	}
	c.frame = nil
	if !bytes.HasPrefix(p, []byte("HTTP/1.1 101 ")) || !bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		return c.Conn.Write(p)
	}
	written, err := c.Conn.Write(slices.Concat(p, frame))
	return min(written, len(p)), err
}

// clientIDFrame is the websocket frame a server sends carrying the Hocuspocus stateless message
// that names client as the id room's page writes under (DocumentClientIdMessagePrefix).
func clientIDFrame(room string, client crdt.ClientID) []byte {
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(hocuspocusStateless)
		encoder.WriteVarString(contracts.DocumentClientIdMessagePrefix + strconv.FormatUint(uint64(client), 10))
	})
	return append(frameHeader(gws.BinaryMessage, true, int64(len(message)), nil), message...)
}
