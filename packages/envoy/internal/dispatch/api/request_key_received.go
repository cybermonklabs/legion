package api

import (
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/contracts"
)

// receivingBodies is every keyed request whose body this process is reading now, by whom its key
// belongs to (requestKeyLogin) and the key: how much of each body has arrived. A client holds each
// attempt of a write to a silence bound, and an upload's client sees its body go out only as its
// connection takes it, into buffers that hold megabytes ahead of the line; so while it waits it
// asks GET /api/v1/request-keys/{key}/received whether the body still arrives (envoy-client's
// #askReceived, the page's askReceived), and only a number that grew restarts its bound. The
// answer is this process's own: a body another process reads is answered 404 here, which tells the
// client nothing, and its bound runs on as it did before the route.
type receivingBodies struct {
	mu     sync.Mutex
	bodies map[receivingKey][]*receivingBody
}

// receivingKey is whose key, and which.
type receivingKey struct{ login, key string }

// receivingBody is one request's body as it arrives: the bytes read off it so far, of length, its
// Content-Length (-1 where it declared none).
type receivingBody struct {
	read   atomic.Int64
	length int64
}

// start counts body as login's key's from now until stop is called, and answers the body that
// counts: the one the request must read from.
func (b *receivingBodies) start(login, key string, body io.ReadCloser, length int64) (counted io.ReadCloser, stop func()) {
	at := receivingKey{login: login, key: key}
	receiving := &receivingBody{length: length}
	b.mu.Lock()
	if b.bodies == nil {
		b.bodies = map[receivingKey][]*receivingBody{}
	}
	b.bodies[at] = append(b.bodies[at], receiving)
	b.mu.Unlock()
	stop = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		left := slices.DeleteFunc(b.bodies[at], func(other *receivingBody) bool { return other == receiving })
		if len(left) == 0 {
			delete(b.bodies, at)
			return
		}
		b.bodies[at] = left
	}
	return countedBody{ReadCloser: body, into: receiving}, stop
}

// received is how much has arrived of the body login's key began receiving last, and its length;
// ok is false where the key is receiving none here.
func (b *receivingBodies) received(login, key string) (read, length int64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bodies := b.bodies[receivingKey{login: login, key: key}]
	if len(bodies) == 0 {
		return 0, 0, false
	}
	latest := bodies[len(bodies)-1]
	return latest.read.Load(), latest.length, true
}

// countedBody is a request body that adds every byte read off it to its receivingBody.
type countedBody struct {
	io.ReadCloser
	into *receivingBody
}

func (c countedBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.into.read.Add(int64(n))
	return n, err
}

// requestKeyReceived is GET /api/v1/request-keys/{key}/received's answer: how many bytes of the
// body have arrived, and how many it declared (-1 for none).
type requestKeyReceived struct {
	Received int64 `json:"received"`
	Length   int64 `json:"length"`
}

// getRequestKeyReceived answers how much of the body of the caller's own keyed request under {key}
// this process has read so far, while it reads it: 404 KEY_NOT_RECEIVING once the body has
// arrived whole or ended, before it began, under another caller's key, or in another process.
func (s *server) getRequestKeyReceived(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !requestKeyPattern.MatchString(key) {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, contracts.IdempotencyKeyInvalid,
			"the key must be 1 to %d letters, digits, '.', '_', ':' or '-'", contracts.IdempotencyKeyMax))
		return
	}
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	read, length, ok := s.receiving.received(requestKeyLogin(actor, human), key)
	if !ok {
		writeError(w, "KEY_NOT_RECEIVING", http.StatusNotFound,
			"this Dispatch process is reading no body of yours under that key")
		return
	}
	WriteJSON(w, http.StatusOK, requestKeyReceived{Received: read, Length: length})
}
