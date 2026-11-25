// Package accesslog writes Dispatch's one line per served request.
//
// Dispatch kept no record of who called it: when its database saturated nobody could name the
// callers behind the requests it was serving. Handler wraps one route and logs, when the request
// ends, the method, the route pattern (never a path's ids alone), the caller the authentication
// layer resolved and the credential behind it, the address and user agent it came from, the
// status, the time it took and the bytes of body it wrote.
//
// The line never carries a token, a cookie, a query string, a request body or a response body.
// A search's line carries the keyed hash of the search in place of its query string (NoteSearch):
// the key is one the server makes when it starts and never writes out, so the hash tells repeats
// of one search apart from other searches and gives a reader no way back to the text. The text in
// it a caller wrote - the method, the path, the session id a bearer names, the address a proxy
// forwarded and the user agent - is each clipped to fieldBytes.
package accesslog

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// The caller kinds the line names, one per way Dispatch authenticates a request.
const (
	// KindUser is a signed-in person, named by their lowercase email.
	KindUser = "user"
	// KindSession is an agent session a bearer named in its request's actor.
	KindSession = "session"
	// KindToken is a personal agent token with no session named, by its owner's email.
	KindToken = "token"
	// KindService is a verified Kubernetes service-account token, by its subject.
	KindService = "service"
	// KindBrokerToken is an access token the secrets broker issued, presented under the DPoP
	// scheme and proved against the key it is bound to, by the person its session acts for.
	KindBrokerToken = "broker-token"
	// KindSharedToken is the deployment's shared agent token with no session named: it proves
	// no owner, so the line names none.
	KindSharedToken = "shared-token"
	// KindAnonymous is a request no credential was resolved for: a public route, or one
	// refused before its handler ran.
	KindAnonymous = "anonymous"
)

// Message is the line every served request writes. Match it whole (msg="dispatch: request"):
// the previous-shared-token warning's message starts with the same words.
const Message = "dispatch: request"

// fieldBytes bounds the caller-written text the line keeps (the request path and a named
// session's id), as the previous-token log bounds the caller fields it writes
// (auth.previousTokenCallerBytes).
const fieldBytes = 256

// StatusClientClosed is the status the line gives a request whose client closed it before its
// answer (MarkClosedByClient): nginx's 499. No answer carries it: the request is still answered
// with the status its handler wrote, which reaches the client only where it half-closed and
// still reads.
const StatusClientClosed = 499

// callerKey carries the request's recorder, which the authentication layer names the caller on.
// A nil *recorder under it masks the request's recorder (Detached).
type callerKey struct{}

// recorder is one request's response writer and its line's facts. It wraps the server's writer
// rather than replacing it, and carries through the three interfaces Dispatch's handlers take it
// for: Flusher for the body drain's flush of an answer (api.drainingWriter), Hijacker for the
// websocket upgrade, and ReaderFrom so an artifact download still streams through the server's own
// copy.
type recorder struct {
	http.ResponseWriter
	// mu guards kind, id, credential, searchKey and searchShape: Note and NoteSearch may run on
	// another goroutine than the handler's while the handler is still writing.
	mu   sync.Mutex
	kind string
	id   string
	// credential is the owner or subject a bearer's credential proved, kept when the bearer then
	// names a session (which is text the caller wrote), so the line still says whose credential
	// acted. Empty when the caller is that credential itself, a person, or the shared token.
	credential string
	// searchKey and searchShape are a search's keyed hash and the shape of the statement it chose
	// (NoteSearch), empty on every other request.
	searchKey   string
	searchShape string
	status      int
	written     int64
	// request is the request's own context as net/http gave it, which net/http cancels when the
	// connection stops reading or writing (MarkClosedByClient), and which no layer of Dispatch
	// cancels.
	request context.Context
	// closedByClient is set by MarkClosedByClient, on the handler's goroutine as status is.
	closedByClient bool
}

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(body []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	written, err := rec.ResponseWriter.Write(body)
	rec.written += int64(written)
	return written, err
}

// ReadFrom keeps the server's own copy (sendfile, and its buffer reuse) for a handler that
// streams a body with io.Copy, such as an artifact download.
func (rec *recorder) ReadFrom(body io.Reader) (int64, error) {
	from, ok := rec.ResponseWriter.(io.ReaderFrom)
	if !ok {
		// Copy through Write, which counts, without handing io.Copy this ReadFrom again.
		return io.Copy(writerOnly{rec}, body)
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	written, err := from.ReadFrom(body)
	rec.written += written
	return written, err
}

// writerOnly hides every method of its writer but Write.
type writerOnly struct{ io.Writer }

func (rec *recorder) Flush() {
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack serves the websocket upgrade, which writes its own answer on the connection it takes.
// The line records the upgrade as the 101 it is; the frames that follow are not its bytes.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err == nil && rec.status == 0 {
		rec.status = http.StatusSwitchingProtocols
	}
	return conn, buffered, err
}

// Unwrap gives http.ResponseController the server's own writer, so a handler that moves its
// connection's deadlines reaches it through this wrapper.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Handler serves one route and logs the request when it ends. route is the route's own pattern
// (`/api/v1/issues/{key}`), mounted once, so the line groups a route's requests together whatever
// ids their paths carry. A handler that panics (an artifact download whose store fails mid-stream
// aborts with http.ErrAbortHandler) still writes its line, marked aborted=true, as the panic
// passes on to net/http. The time the line records is read on the real clock (HandlerOn).
func Handler(route string, next http.Handler) http.Handler {
	return HandlerOn(route, next, nil)
}

// HandlerOn is Handler timing each request on clk, nil being clock.Real(): a server that times
// work of its own inside the request on a clock a test drives (api's Deps.Clock, which the answers
// read's sessionanswers.Latency shares in a test) times the request's line on that clock too, so
// the line and that work read one clock.
func HandlerOn(route string, next http.Handler, clk clock.Clock) http.Handler {
	clk = cmp.Or[clock.Clock](clk, clock.Real())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := clk.Now()
		rec := &recorder{ResponseWriter: w, kind: KindAnonymous, request: r.Context()}
		completed := false
		defer func() { rec.write(route, r, clk.Now().Sub(start), !completed) }()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), callerKey{}, rec)))
		completed = true
	})
}

// HandlerFunc is Handler for a handler function, which is how every Dispatch route is written.
func HandlerFunc(route string, next http.HandlerFunc) http.HandlerFunc {
	return HandlerFuncOn(route, next, nil)
}

// HandlerFuncOn is HandlerOn for a handler function.
func HandlerFuncOn(route string, next http.HandlerFunc, clk clock.Clock) http.HandlerFunc {
	handler := HandlerOn(route, next, clk)
	return handler.ServeHTTP
}

// Note records who the server took this request's sender for. The authentication layer calls it
// wherever it resolves a caller, and the last resolution wins: a bearer is noted by what its
// token proved and then by the session it named, which is the caller a reader wants. A session
// named after a credential of its own keeps that credential as the line's `credential`:
// `token:<owner>`, `service:<subject>`, `broker-token:<person>` or `shared-token`. For the first
// three that session is text the caller wrote, except under a broker-issued token, where the
// token itself names it.
func Note(ctx context.Context, kind, id string) {
	rec, _ := ctx.Value(callerKey{}).(*recorder)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	switch {
	case kind == KindSession && rec.kind == KindSharedToken:
		rec.credential = KindSharedToken
	case kind == KindSession && (rec.kind == KindToken || rec.kind == KindService || rec.kind == KindBrokerToken):
		rec.credential = rec.kind + ":" + rec.id
	case kind != KindSession:
		rec.credential = ""
	}
	rec.kind, rec.id = kind, id
	rec.mu.Unlock()
}

// Detached returns ctx with its request's recorder masked, so a caller resolved on it names no
// one on the line. A connection that resolves its caller again on every heartbeat
// (api.whileCallerResolves: the document socket and the live socket) does so on it: the line
// names the caller the request was admitted as, and a heartbeat that resolves only the bearer's
// credential would otherwise replace the session the request named.
func Detached(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey{}, (*recorder)(nil))
}

// NoteSearch records a search on its request's line: key, the keyed hash of the normalized
// request, which the line names as search_key, and shape, the statement the search chose, as
// search_shape. The search calls it when it chooses a statement, before the statement runs, so a
// search that then fails or whose client closes it still names both; a later call replaces the
// earlier, as a broadened second run replaces the first run's shape. The key is the caller's to
// make: this package writes it as it is given, so it must be one no reader can reverse.
func NoteSearch(ctx context.Context, key, shape string) {
	rec, _ := ctx.Value(callerKey{}).(*recorder)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.searchKey, rec.searchShape = key, shape
	rec.mu.Unlock()
}

// MarkClosedByClient reports whether the client of the request that w answers closed it before
// its answer, and when it did, gives the request's line StatusClientClosed in place of the status
// the request is answered with, whether that status was written before the close or after it.
// It changes nothing the client is sent. It reads the request's own context, which only the client
// ends while the handler runs. net/http cancels it when the connection fails a read or a write:
// the client closed the connection, half-closed its side (a read that meets the end of the
// stream), or reset the stream, or a deadline set on the connection ran out. A client that gives
// up waiting - a page at its own deadline, a proxy at its timeout - closes the same way, so a 499
// says the client left, and its line's elapsed_ms says how long it waited. No Dispatch route
// leaves a deadline set on its connection past the end of the body while its handler works (api's
// readFor), and Dispatch shuts down without closing a connection under a request (cmd/dispatch's
// shutdown). A cancel that Dispatch makes - a handler's own context, the server's lifetime -
// leaves it live. w is the writer a handler was given: the walk follows each layer's Unwrap, as
// http.ResponseController does, and a writer this package did not wrap, or one that hides what it
// wraps (api.keyedAnswer, whose answer a key keeps), reports false.
func MarkClosedByClient(w http.ResponseWriter) bool {
	for {
		switch layer := w.(type) {
		case *recorder:
			if !errors.Is(layer.request.Err(), context.Canceled) {
				return false
			}
			layer.closedByClient = true
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = layer.Unwrap()
		default:
			return false
		}
	}
}

func (rec *recorder) write(route string, r *http.Request, elapsed time.Duration, aborted bool) {
	rec.mu.Lock()
	kind, id, credential := rec.kind, rec.id, rec.credential
	searchKey, searchShape := rec.searchKey, rec.searchShape
	rec.mu.Unlock()
	status := rec.status
	switch {
	case rec.closedByClient:
		status = StatusClientClosed
	case status == 0:
		// A handler that wrote nothing answered 200 with an empty body. One that panicked is
		// logged as 500, though net/http writes no status: it closes the connection (HTTP/1)
		// or resets the stream (HTTP/2).
		status = http.StatusOK
		if aborted {
			status = http.StatusInternalServerError
		}
	}
	attrs := []any{
		"method", clip(r.Method),
		"route", route,
		"path", clip(r.URL.EscapedPath()),
		"caller_kind", kind,
		"caller", clip(id),
		"address", clip(auth.ForwardedAddress(r)),
		"peer", peerAddress(r),
		"user_agent", clip(r.UserAgent()),
		"status", status,
		"elapsed_ms", elapsed.Milliseconds(),
		"bytes", rec.written,
	}
	if credential != "" {
		attrs = append(attrs, "credential", credential)
	}
	if searchKey != "" {
		attrs = append(attrs, "search_key", searchKey)
	}
	if searchShape != "" {
		attrs = append(attrs, "search_shape", searchShape)
	}
	if aborted {
		attrs = append(attrs, "aborted", true)
	}
	slog.Info(Message, attrs...)
}

// peerAddress is the connection's own address without its port: the hop that reached Dispatch,
// which a caller cannot forge as it can an X-Forwarded-For header on a direct path.
func peerAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return clip(r.RemoteAddr)
}

// clip cuts caller-written text to fieldBytes, dropping a rune the cut splits.
func clip(text string) string {
	if len(text) <= fieldBytes {
		return text
	}
	return strings.ToValidUTF8(text[:fieldBytes], "")
}
