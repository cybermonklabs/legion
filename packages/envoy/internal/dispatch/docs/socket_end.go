package docs

import (
	"bytes"
	"context"
	"net"
	"sync"

	gws "github.com/gorilla/websocket"
)

// A document socket's request ends in three ways, each of which ends a context: a re-check's
// revoke cancels the socket's own context (revokeUnless), and the API's heartbeat cancels the
// request's context when the credential the socket was opened with expires (whileCallerResolves,
// errCredentialExpired, which closes with the admission-revoked code too) or no longer resolves
// (which closes with no frame). From that moment the page is owed no more of the document, and the
// room takes nothing more from it. ygo acts on the end from the socket's context watcher, a
// goroutine the cancel only wakes: until it runs, the room's broadcast goes on writing every
// update to the socket, and ygo's read loop goes on applying what the page sends. With the watcher
// scheduled 30 ms late, an edit made after the revoke reached the revoked page (INFRA-5142).
//
// ygo offers no hook on what it writes to a peer or reads from one, so the socket's end is kept
// on the connection the websocket upgrade hijacks, where the awareness filter already reads what
// the page sends (awareness_limit.go): socketEnd decides, under its own lock, each frame ygo
// writes and each message the page sends, and reads both contexts as it decides. A cancel sets
// its own context's error before it returns, so every decision taken after a cancel has returned
// is taken as ended, whichever of the two contexts it cancelled: the socket's own context is a
// child of the request's, but a child learns of its parent's end in the parent's cancel only when
// every context between them is one of the standard library's; reading the request's context
// itself takes no such turn.

// socketEnd is one document socket's end, as its connection keeps it: once own or request has
// ended, the connection lets out only the rest of the message already going out and a close
// frame, and ygo applies no message the page completes from then on, whenever it began.
//
// Writes: it reads the frames ygo writes, whole or split over writes, and decides each as its
// header completes. Before the end every frame goes out. After it, a frame goes out only if it is
// a close frame, or the next frame of a data message that went out before the end and has not
// finished - the message the close waits for, which gorilla's WriteControl never cuts into. Every
// other frame - a room update, an awareness message, a sync answer - is dropped whole, header and
// payload, and the write that carried it is answered as written, so gorilla and ygo go on to the
// close as they would have. The connection's first write is the upgrade's 101 answer, which the
// upgrader makes in one write before any frame (processHeaderConn reads the same), and goes out
// as it is.
//
// Reads: the awareness filter asks it twice for each message the page sends (refusesMessage).
// It asks as each data frame arrives that could begin a message of gorilla's, whatever message the
// filter holds open, and a message begun after the end reaches ygo as nothing at all
// (awareness_limit.go, readFrame). And it asks at each message's completion: the filter takes the
// last byte of the message's final frame off the page before it asks, and hands that byte to ygo
// only if the socket has not ended (awareness_limit.go, Read). So a message the page began before
// the end and completes after it - its first bytes passed to gorilla, or held by the filter - is
// never applied either: gorilla never holds it whole.
type socketEnd struct {
	// own is the socket's own context, which ygo serves it on and a revoke cancels; request is the
	// request's context as ServeHTTP received it, which the API cancels.
	own, request context.Context
	mu           sync.Mutex
	// ended is set by the first decision that finds either context ended.
	ended bool
	// upgraded is set once the upgrade's answer has gone out; every byte after it is a frame.
	upgraded bool
	// header is the header of the frame being read, as far as the writes have carried it; its
	// bytes wait until it is whole and decided.
	header []byte
	// left is how many payload bytes of the frame in hand are still to come, and passing whether
	// they go out.
	left    int64
	passing bool
	// inMessage is set while a data message that went out has frames still to come.
	inMessage bool
	// frames counts the frames that went out, and framesBeforeEnd is that count when the end was
	// first seen: every frame counted before it was decided before the cancel, and every frame
	// after it is the end's to answer for, so a test reads that boundary here rather than off a
	// clock (framesWrittenBeforeEnd).
	frames          int
	framesBeforeEnd int
	// parts is decide's answer, reused from one write to the next: a connection takes one write
	// at a time (socketEndConn), which clears it once it is done with it.
	parts [][]byte
}

// newSocketEnd is the end of the socket served on own, a child of request.
func newSocketEnd(own, request context.Context) *socketEnd {
	return &socketEnd{own: own, request: request}
}

// noteEnd marks the socket ended once either context has, keeping the count of the frames let
// out before; e.mu is held. A decision that found both contexts live was taken before either
// cancel, so the frames counted when the end is first seen are exactly those.
func (e *socketEnd) noteEnd() {
	if !e.ended && (e.own.Err() != nil || e.request.Err() != nil) {
		e.ended = true
		e.framesBeforeEnd = e.frames
	}
}

// framesWrittenBeforeEnd is how many frames the connection had let out when the socket ended, and
// whether it has.
func (e *socketEnd) framesWrittenBeforeEnd() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noteEnd()
	return e.framesBeforeEnd, e.ended
}

// refusesMessage answers whether a message the page begins, or completes, from now on is to reach
// ygo as nothing: it is once the socket has ended. The awareness filter asks it as each message
// begins (readFrame) and as each completes (Read). A nil end refuses nothing (a connection no
// ServeHTTP made).
func (e *socketEnd) refusesMessage() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noteEnd()
	return e.ended
}

// decide reads p, the next bytes ygo's connection writes, and returns what of them goes out:
// whole when every byte of p goes out as it is, else the parts that do, in order, which may
// start with header bytes an earlier write left waiting.
func (e *socketEnd) decide(p []byte) ([][]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.upgraded {
		e.upgraded = true
		return nil, true
	}
	e.noteEnd()
	e.parts = e.parts[:0]
	whole := len(e.header) == 0
	// headerAt is where in p the header being read began, or -1 when an earlier write began it.
	headerAt := -1
	for i := 0; i < len(p); {
		if e.left > 0 {
			n := int(min(int64(len(p)-i), e.left))
			if e.passing {
				e.parts = append(e.parts, p[i:i+n])
			} else {
				whole = false
			}
			e.left -= int64(n)
			i += n
			continue
		}
		if len(e.header) == 0 {
			headerAt = i
		}
		need := 2
		if len(e.header) >= 2 {
			need = frameHeaderLength(e.header)
		}
		n := min(need-len(e.header), len(p)-i)
		e.header = append(e.header, p[i:i+n]...)
		i += n
		if len(e.header) < 2 || len(e.header) < frameHeaderLength(e.header) {
			continue
		}
		if e.passing = e.admits(e.header[0]); !e.passing {
			whole = false
		} else if e.frames++; headerAt >= 0 {
			e.parts = append(e.parts, p[headerAt:i])
		} else {
			e.parts = append(e.parts, bytes.Clone(e.header))
		}
		e.left = max(framePayloadLength(e.header), 0)
		e.header, headerAt = e.header[:0], -1
	}
	if len(e.header) > 0 {
		whole = false
	}
	return e.parts, whole
}

// admits decides the frame whose header begins with first, and keeps track of the data message
// the frames that go out are part of.
func (e *socketEnd) admits(first byte) bool {
	opcode, final := first&0x0f, first&finalFrame != 0
	switch {
	case !e.ended:
	case opcode == gws.CloseMessage:
		return true
	case opcode == continuationFrame && e.inMessage:
	default:
		return false
	}
	if opcode == continuationFrame || opcode == gws.TextMessage || opcode == gws.BinaryMessage {
		e.inMessage = !final
	}
	return true
}

// socketEndConn is a document socket's connection under ygo's writes: each write puts on the
// connection underneath what end lets out of it. ygo writes from one goroutine at a time and
// gorilla serialises that writer against WriteControl, so the frames arrive here in the order
// they go out.
type socketEndConn struct {
	net.Conn
	end *socketEnd
}

func (c *socketEndConn) Write(p []byte) (int, error) {
	parts, whole := c.end.decide(p)
	// The parts are the caller's bytes; none is kept past this write.
	defer clear(parts)
	if whole {
		return c.Conn.Write(p)
	}
	if len(parts) > 0 {
		buffers := net.Buffers(parts)
		if _, err := buffers.WriteTo(c.Conn); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
