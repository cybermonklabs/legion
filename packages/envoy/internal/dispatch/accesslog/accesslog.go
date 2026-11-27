// Package accesslog writes Dispatch's one line per served request.
//
// Dispatch kept no record of who called it: when its database saturated nobody could name the
// callers behind the requests it was serving. Handler wraps one route and logs, when the request
// ends, the method, the route pattern (never a path's ids alone), the caller the authentication
// layer resolved and the credential behind it, the address and user agent it came from, the
// status, the time it took and the bytes of body it wrote. A person - a signed-in browser's, a
// personal token's owner, the person a broker-issued token acts for - is named by their login's
// hash (model.LoginHash), never by their address.
//
// The line never carries a token, a cookie, a query string, a request body or a response body.
// The text in it a caller wrote - the method, the path, the session id a bearer names, the
// address a proxy forwarded and the user agent - is each clipped to fieldBytes.
package accesslog

import (
	"bufio"
	"cmp"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The caller kinds the line names, one per way Dispatch authenticates a request.
const (
	// KindUser is a signed-in person, named by their login's hash.
	KindUser = "user"
	// KindSession is an agent session a bearer named in its request's actor.
	KindSession = "session"
	// KindToken is a personal agent token with no session named, by its owner's login's hash.
	KindToken = "token"
	// KindService is a verified Kubernetes service-account token, by its subject.
	KindService = "service"
	// KindBrokerToken is an access token the secrets broker issued, presented under the DPoP
	// scheme and proved against the key it is bound to, by the login's hash of the person its
	// session acts for, or by `service:<service>` for a token that names no person (a machine's,
	// or a job's, whose service is its entry's identity).
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
	// mu guards kind, id, hash and credential alone: Note may run on another goroutine than the
	// handler's while the handler is still writing.
	mu   sync.Mutex
	kind string
	id   string
	// hash is id's login hash (model.LoginHash) when the caller is a person (person), made by Note
	// from the id it was handed, and empty otherwise.
	hash string
	// credential is the owner or subject a bearer's credential proved, kept when the bearer then
	// names a session (which is text the caller wrote), so the line still says whose credential
	// acted: a person's by their login's hash. Empty when the caller is that credential itself, a
	// person, or the shared token.
	credential string
	status     int
	written    int64
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
		rec := &recorder{ResponseWriter: w, kind: KindAnonymous}
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
// `token:<owner's login_hash>`, `service:<subject>`, `broker-token:<person's login_hash>`
// (`broker-token:service:<service>` for a broker token that names no person) or `shared-token`.
// For the first three that session is text the caller wrote, except under a broker-issued token,
// where the token itself names it.
func Note(ctx context.Context, kind, id string) {
	rec, _ := ctx.Value(callerKey{}).(*recorder)
	if rec == nil {
		return
	}
	hash := ""
	if person(kind, id) {
		hash = model.LoginHash(id)
	}
	rec.mu.Lock()
	switch {
	case kind == KindSession && rec.kind == KindSharedToken:
		rec.credential = KindSharedToken
	case kind == KindSession && (rec.kind == KindToken || rec.kind == KindBrokerToken) && rec.hash != "":
		rec.credential = rec.kind + ":" + rec.hash
	case kind == KindSession && (rec.kind == KindService || rec.kind == KindBrokerToken):
		rec.credential = rec.kind + ":" + rec.id
	case kind != KindSession:
		rec.credential = ""
	}
	rec.kind, rec.id, rec.hash = kind, id, hash
	rec.mu.Unlock()
}

// brokerServicePrefix starts the id a broker-issued token that acts for no person is noted by
// (api's server.brokerTokenActor: "service:" and the token's service), which is no login.
const brokerServicePrefix = "service:"

// person reports whether a caller of kind noted as id is a person, whom the line names by their
// login's hash (`login_hash`) in place of `caller`: a signed-in person, a personal token's owner,
// and the person a broker-issued token acts for, but not a broker token's fleet.
func person(kind, id string) bool {
	switch kind {
	case KindUser, KindToken:
		return true
	case KindBrokerToken:
		return !strings.HasPrefix(id, brokerServicePrefix)
	}
	return false
}

// Detached returns ctx with its request's recorder masked, so a caller resolved on it names no
// one on the line. A connection that resolves its caller again on every heartbeat
// (api.whileCallerResolves: the document socket and the live socket) does so on it: the line
// names the caller the request was admitted as, and a heartbeat that resolves only the bearer's
// credential would otherwise replace the session the request named.
func Detached(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey{}, (*recorder)(nil))
}

func (rec *recorder) write(route string, r *http.Request, elapsed time.Duration, aborted bool) {
	rec.mu.Lock()
	kind, id, hash, credential := rec.kind, rec.id, rec.hash, rec.credential
	rec.mu.Unlock()
	status := rec.status
	if status == 0 {
		// A handler that wrote nothing answered 200 with an empty body. One that panicked is
		// logged as 500, though net/http writes no status: it closes the connection (HTTP/1)
		// or resets the stream (HTTP/2).
		status = http.StatusOK
		if aborted {
			status = http.StatusInternalServerError
		}
	}
	who := []any{"caller", clip(id)}
	if hash != "" {
		who = []any{"login_hash", hash}
	}
	attrs := append([]any{
		"method", clip(r.Method),
		"route", route,
		"path", clip(loggedPath(route, r.URL.EscapedPath())),
		"caller_kind", kind,
	}, who...)
	attrs = append(attrs,
		"address", clip(auth.ForwardedAddress(r)),
		"peer", peerAddress(r),
		"user_agent", clip(r.UserAgent()),
		"status", status,
		"elapsed_ms", elapsed.Milliseconds(),
		"bytes", rec.written,
	)
	if credential != "" {
		attrs = append(attrs, "credential", credential)
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

// loginSegments are the route wildcards whose path segment is a person's login
// (`/api/v1/people/{login}/nodes`, `/api/v1/projects/{key}/access/people/{email}`).
var loginSegments = map[string]bool{"{login}": true, "{email}": true}

// loggedPath is the escaped path as the line writes it: each segment that route names a person's
// login in, and each that holds an '@' unescaped whatever the route (an `unmatched` path, a
// method the route does not serve), is written `login_hash:<hash>` (model.LoginHash of the
// segment, unescaped), so the line holds no address; every other segment is as it was sent.
func loggedPath(route, path string) string {
	// A segment unescapes to an '@' only where the path holds an '@' or a '%'.
	if !strings.ContainsAny(path, "@%") && !strings.Contains(route, "{login}") && !strings.Contains(route, "{email}") {
		return path
	}
	pattern, segments := strings.Split(route, "/"), strings.Split(path, "/")
	aligned := len(pattern) == len(segments)
	for i, segment := range segments {
		text, err := url.PathUnescape(segment)
		if err != nil {
			text = segment
		}
		if strings.Contains(text, "@") || aligned && loginSegments[pattern[i]] {
			segments[i] = "login_hash:" + model.LoginHash(text)
		}
	}
	return strings.Join(segments, "/")
}

// clip cuts caller-written text to fieldBytes, dropping a rune the cut splits.
func clip(text string) string {
	if len(text) <= fieldBytes {
		return text
	}
	return strings.ToValidUTF8(text[:fieldBytes], "")
}
