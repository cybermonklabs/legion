// edge_server.go is the broker's edge server: its handler chain, its access log and its idle
// timeout, which main serves on the socket BROKER_EDGE_LISTEN_ADDR binds.
package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/metrics"
)

// edgeRequestMessage is the line the broker writes at INFO for each request its edge answers
// (edgeAccessLog): the edge's access log.
const edgeRequestMessage = "broker: edge request"

// edgeForwardedForHeader is the header the edge's access log names as x_forwarded_for: what the
// edge's load balancer appends the client's address and its own to.
const edgeForwardedForHeader = "X-Forwarded-For"

// edgeIdleTimeout is how long the edge's server keeps an idle connection open for its next
// request. Google's external Application Load Balancer, the one caller of the edge, keeps its
// connection to a backend idle for 600 seconds, fixed, and its documentation says a backend's own
// keepalive must be greater than that, or the backend can close a connection the balancer still
// sends a request on; its own example for nginx is keepalive_timeout 620s
// (https://docs.cloud.google.com/load-balancing/docs/https/request-distribution#timeout-keepalive-backends).
// The same page says the balancer does not retry a POST, and both login routes are POST, so a
// connection the broker closed under it is an answer of 502 to a person's machine. 620 seconds is
// the edge's alone: the API's server keeps its two minutes (newEdgeServer).
const edgeIdleTimeout = 620 * time.Second

// newEdgeServer is the edge's own server: edgeHandler over mux, on the read and write timeouts of
// apiServer, the API port's server, since what the edge serves is the API's own handlers, and on
// edgeIdleTimeout in place of the API's idle timeout.
func newEdgeServer(mux http.Handler, answers *metrics.Counter, log *slog.Logger, apiServer *http.Server) *http.Server {
	return &http.Server{
		Handler:           edgeHandler(mux, answers, log),
		ReadHeaderTimeout: apiServer.ReadHeaderTimeout,
		ReadTimeout:       apiServer.ReadTimeout,
		WriteTimeout:      apiServer.WriteTimeout,
		IdleTimeout:       edgeIdleTimeout,
	}
}

// edgeHandler is what the edge's server serves: mux, the edge's routes (api.RegisterEdge), behind
// its answer recorder (answer.EdgeMiddleware), which wraps the mux itself so it reads each answer's
// route; the request deadline around that; and, outermost, the edge's access log on log, so every
// request a handler of the edge is handed writes its line, the 404 and 405 the mux answers and a
// refusal before any route among them, with the status the broker answered with.
func edgeHandler(mux http.Handler, answers *metrics.Counter, log *slog.Logger) http.Handler {
	return edgeAccessLog(withRequestDeadline(answer.EdgeMiddleware(mux, answers), requestDeadline), log)
}

// edgeAccessLog serves next, then writes one line on log at INFO under edgeRequestMessage for the
// request: an access log's line, which no proxy of the deployment's own writes for the edge, since
// nothing but the load balancer stands in front of it. The line names the method; the path, never
// its query string; the protocol; the status the broker answered with, read once next has
// returned, which a client can still miss when the load balancer's or the server's own timeout
// ends the exchange first; the User-Agent; and X-Forwarded-For, each of its header lines in order
// joined by ", ".
// It names nothing else of the request: not the query string, the Referer, the body, the
// Authorization header, the DPoP proof or a cookie, any of which can carry a credential or a
// person's login.
//
// Each text field the caller wrote is cut to answer.FieldBytes, so the line stays one Cloud
// Logging entry whatever the caller sends: the method, the path and the User-Agent to their first
// bytes (answer.Clip), and X-Forwarded-For to its last (clipTail), which hold the entries the load
// balancer appends after whatever the caller wrote. A handler that panics still writes its line,
// marked aborted=true, before the panic goes on to net/http. An answer net/http writes itself
// before any handler runs, such as its 400 for a malformed request line or its 431 for headers
// over its limit, reaches no handler and writes no line.
func edgeAccessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answered := &edgeAnswer{ResponseWriter: w}
		completed := false
		defer func() { answered.log(log, r, !completed) }()
		next.ServeHTTP(answered, r)
		completed = true
	})
}

// edgeAnswer is one edge request's response writer as its access log reads it: the status the
// broker answered with. It overrides WriteHeader alone of the writer's own methods: the edge's
// answer recorder, always inside it (edgeHandler), sets each answer's status with WriteHeader
// before the answer's first byte, so a Write of its own would only pass the handlers' bytes on,
// and code scanning reads every such pass-through as one more place a caller's text is written
// (go/reflected-xss). It adds Flush, which the answer recorder takes the writer for, and Unwrap
// for http.ResponseController. The edge's routes neither take the connection nor stream a file,
// so it carries no Hijack and no ReadFrom: the answer recorder copies a body through Write
// without the second.
type edgeAnswer struct {
	http.ResponseWriter
	// status is the answer's final status, 0 until one has gone out.
	status int
}

// WriteHeader records the answer's first final status: a 1xx informational answer but 101 is
// passed on and not recorded, since the final status still follows it.
func (a *edgeAnswer) WriteHeader(status int) {
	if a.status == 0 && (status >= http.StatusOK || status == http.StatusSwitchingProtocols) {
		a.status = status
	}
	a.ResponseWriter.WriteHeader(status)
}

// Flush sends what has been written, its status 200 when none was set, as net/http's writer does.
func (a *edgeAnswer) Flush() {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if flusher, ok := a.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap gives http.ResponseController the server's own writer.
func (a *edgeAnswer) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// log writes r's line on log once its handler has returned, or panicked when aborted.
func (a *edgeAnswer) log(log *slog.Logger, r *http.Request, aborted bool) {
	status := a.status
	if status == 0 {
		// A handler that wrote nothing answered 200 with no body, which net/http sends once it
		// returns. One that panicked sent nothing - net/http closes the connection or resets the
		// stream - and is the 500 the answer recorder counts it as.
		status = http.StatusOK
		if aborted {
			status = http.StatusInternalServerError
		}
	}
	fields := []any{
		"method", answer.Clip(r.Method),
		"path", answer.Clip(r.URL.EscapedPath()),
		"protocol", r.Proto,
		"status", status,
		"user_agent", answer.Clip(r.UserAgent()),
		"x_forwarded_for", clipTail(strings.Join(r.Header.Values(edgeForwardedForHeader), ", ")),
	}
	if aborted {
		fields = append(fields, "aborted", true)
	}
	log.Info(edgeRequestMessage, fields...)
}

// clipTail keeps text of at most answer.FieldBytes as it came, and cuts longer text to its last
// FieldBytes, dropping every invalid UTF-8 run in the bytes it keeps (strings.ToValidUTF8), a rune
// the cut splits among them: answer.Clip from the other end.
func clipTail(text string) string {
	if len(text) <= answer.FieldBytes {
		return text
	}
	return strings.ToValidUTF8(text[len(text)-answer.FieldBytes:], "")
}
