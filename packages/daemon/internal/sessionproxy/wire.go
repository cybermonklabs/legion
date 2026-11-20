package sessionproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// The codes of the Postgres messages a client sends before its startup message, which carry no
// type byte: a request for an SSL or a GSS encryption, which the database answers with one byte,
// and a cancel request, which it ends without a word.
const (
	sslRequest    = 80877103
	gssencRequest = 80877104
	cancelRequest = 80877102
)

// maxErrorBody bounds the body of an error the proxy keeps to read: a database's error is a few
// hundred bytes, and one longer than this is read as no word at all.
const maxErrorBody = 16 << 10

// wire is what the proxy reads of the Postgres messages that cross one served connection, to tell
// whether the database's end of that connection is the store's fault (servedUpstream): how the
// messages are framed and of what type each is, which of the requests that come before a startup
// the client made, and, of an error the database sends, its severity and SQLSTATE. It reads no
// other field of any message, and nothing at all after an encryption the database accepted, which
// leaves the rest unreadable (opaque).
type wire struct {
	mu             sync.Mutex
	client, server frame
	// started is whether the client sent its startup message, after which every message of its is
	// typed; owed is how many one-byte answers the database owes to its encryption requests.
	started bool
	owed    int
	opaque  bool
	// cancel is whether the connection is a cancel request, and terminated whether the client's
	// last message was a Terminate, the end of a session in order: either way the database's end
	// of the connection is its answer.
	cancel, terminated bool
	// fatal is whether the database sent an error of severity FATAL or PANIC, which it ends the
	// connection with: its own word on why the connection ended.
	fatal bool
}

// frame is one direction's message in progress: its header so far, then how much of its body is
// still to come, and of an error the body itself.
type frame struct {
	head [8]byte
	n    int
	kind byte
	left int
	body []byte
}

// fromClient reads b, bytes the client sent, before they go to the database.
func (w *wire) fromClient(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := &w.client
	for len(b) > 0 && !w.opaque {
		if f.left > 0 {
			skip := min(f.left, len(b))
			f.left, b = f.left-skip, b[skip:]
			continue
		}
		// A typed message's header is its type and its length; an untyped one's, before the
		// startup, its length and its code.
		need := 5
		if !w.started {
			need = 8
		}
		took := copy(f.head[f.n:need], b)
		f.n, b = f.n+took, b[took:]
		if f.n < need {
			return
		}
		f.n = 0
		if w.started {
			f.kind, f.left = f.head[0], int(binary.BigEndian.Uint32(f.head[1:5]))-4
			w.terminated = f.kind == 'X'
		} else {
			f.left = int(binary.BigEndian.Uint32(f.head[0:4])) - 8
			switch code := binary.BigEndian.Uint32(f.head[4:8]); {
			case code == sslRequest || code == gssencRequest:
				w.owed++
			case code == cancelRequest:
				w.cancel = true
			case code>>16 == 3:
				w.started = true
			default:
				w.opaque = true
			}
		}
		if f.left < 0 {
			w.opaque = true
		}
	}
}

// fromServer reads b, bytes the database sent, before they go to the client, and answers the
// SQLSTATE of a FATAL or PANIC error that b completes, with ok set; ok is false for none.
func (w *wire) fromServer(b []byte) (code string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := &w.server
	for len(b) > 0 && !w.opaque {
		if f.left > 0 {
			take := min(f.left, len(b))
			if f.kind == 'E' {
				f.body = append(f.body, b[:take]...)
			}
			f.left, b = f.left-take, b[take:]
			if f.left == 0 && f.kind == 'E' {
				if c, fatal := w.readError(f.body); fatal {
					code, ok = c, true
				}
				f.body = nil
			}
			continue
		}
		if f.n == 0 && w.owed > 0 {
			switch b[0] {
			case 'N':
				w.owed, b = w.owed-1, b[1:]
				continue
			case 'S', 'G':
				w.opaque = true
				return code, ok
			}
			// A server that predates encryption answers the request with an error message.
			w.owed = 0
		}
		took := copy(f.head[f.n:5], b)
		f.n, b = f.n+took, b[took:]
		if f.n < 5 {
			return code, ok
		}
		f.n = 0
		f.kind, f.left = f.head[0], int(binary.BigEndian.Uint32(f.head[1:5]))-4
		if f.left < 0 || f.kind == 'E' && f.left > maxErrorBody {
			w.opaque = true
			return code, ok
		}
		if f.left == 0 && f.kind == 'E' {
			if c, fatal := w.readError(nil); fatal {
				code, ok = c, true
			}
		}
	}
	return code, ok
}

// readError reads an error's fields: its SQLSTATE, and whether its severity, the untranslated one
// a server from 9.6 on sends or else the one it translates, is FATAL or PANIC, which marks the
// connection as ended by the database's own word.
func (w *wire) readError(body []byte) (code string, fatal bool) {
	var severity, translated string
	for len(body) > 0 && body[0] != 0 {
		field := body[0]
		end := bytes.IndexByte(body[1:], 0)
		if end < 0 {
			break
		}
		value := string(body[1 : 1+end])
		body = body[2+end:]
		switch field {
		case 'C':
			code = value
		case 'V':
			severity = value
		case 'S':
			translated = value
		}
	}
	if severity == "" {
		severity = translated
	}
	if severity == "FATAL" || severity == "PANIC" {
		w.fatal = true
		return code, true
	}
	return code, false
}

// databaseEnded is whether err, how a read or a write on the database's side of the connection
// failed, is the database ending the connection with no word of its own on why: a reset or any
// other failure, and an end of file too, unless the client had ended the session in order or the
// connection was a cancel request, which the database answers by ending it, or unless what crossed
// could not be read. An end after a FATAL error is not, as that error already said why
// (fromServer).
func (w *wire) databaseEnded(err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.fatal || w.cancel || w.terminated:
		return false
	case errors.Is(err, io.EOF):
		return !w.opaque
	default:
		return true
	}
}
