package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// closedClientSender is one caller the walk below sends every row of the table as: how its
// request is signed, and whether it goes to the rows the keyed layer wraps alone (keyedMethod).
// A person is resolved through the people store, so a closed request of theirs meets the
// database at the identity step; the deployment's shared token is matched in memory, so a
// session's closed request reaches the row's own handler and the reads it makes; and the same
// session under an Idempotency-Key reaches the keyed layer's own read of its key first.
type closedClientSender struct {
	name      string
	keyedOnly bool
	sign      func(request *http.Request, row int)
}

var closedClientSenders = []closedClientSender{
	{
		name: "a person",
		sign: func(request *http.Request, _ int) { request.Header.Set("X-Dispatch-User", "alice") },
	},
	{
		name: "a session under the deployment's token",
		sign: func(request *http.Request, _ int) { request.Header.Set("Authorization", "Bearer agent-token") },
	},
	{
		name:      "a session under the deployment's token, with an Idempotency-Key",
		keyedOnly: true,
		sign: func(request *http.Request, row int) {
			request.Header.Set("Authorization", "Bearer agent-token")
			request.Header.Set(contracts.IdempotencyKeyHeader, fmt.Sprintf("client-closed-probe-%d", row))
		},
	},
}

// closedClientServeLimit bounds how long one closed request may take to be answered: a row that
// waits on something its request's context does not end would otherwise hold the walk until the
// test binary's deadline, and fail it without naming the row.
const closedClientServeLimit = 30 * time.Second

// closedClientRequest is route's probe (readOnlyProbePath, and routeProbeBody for a write) as
// sender sends it, on a context its client has already closed, so whatever the request reaches
// that waits on its context or the database's answer to it meets context.Canceled.
func closedClientRequest(route apiRoute, sender closedClientSender, row int) *http.Request {
	var body io.Reader = http.NoBody
	if route.Method != http.MethodGet {
		body = strings.NewReader(routeProbeBody(route))
	}
	request := httptest.NewRequest(route.Method, readOnlyProbePath(route.Pattern), body)
	if route.Method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	sender.sign(request, row)
	closed, closeClient := context.WithCancel(request.Context())
	closeClient()
	return request.WithContext(closed)
}

// serveClosedClient serves request on handler and answers what it answered and what it panicked
// with, nil where it returned; a row that has not answered within closedClientServeLimit fails
// the test by name.
func serveClosedClient(t *testing.T, name string, handler http.Handler, request *http.Request) (*httptest.ResponseRecorder, any) {
	t.Helper()
	response := httptest.NewRecorder()
	served := make(chan any, 1)
	go func() {
		defer func() { served <- recover() }()
		handler.ServeHTTP(response, request)
	}()
	select {
	case panicked := <-served:
		return response, panicked
	case <-time.After(closedClientServeLimit):
		t.Fatalf("%s had not answered a request its client closed after %s", name, closedClientServeLimit)
		return nil, nil
	}
}

// TestEveryRouteLogsARequestItsClientClosedWithNoErrorLine holds R6 over its class, every row of
// the table (s.routes()): each row is built into its own chain as register builds it
// (routeHandler, with its keyed replay), and sent, by each caller of closedClientSenders, a
// request whose client closed before it was served. A client that leaves is no fault of the
// server's, so no row may write a level=ERROR line for it, and its request line may not read
// 500: the line reads 499 where an answer the close cut off is logged (accesslog's
// StatusClientClosed), and what a row answered before the close could matter keeps its own
// status. Each request is read against the log it wrote while it ran and while any work it
// detached ran after it: exactly one request line, which a row that wrote none fails as well.
//
// The table is the one a deployment serves, test hooks out. It swaps the default logger for
// the walk, so it is not parallel; a row is counted once its request has been read, and the count
// is held to the rows the registered server mounts (its route index), so a walk that skipped a
// row, or walked none, fails.
func TestEveryRouteLogsARequestItsClientClosedWithNoErrorLine(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	logged := captureLog(t)

	replays := served.keyedReplays()
	walked, requests := 0, 0
	var failures []string
	for row, route := range served.routes() {
		name := route.Method + " " + route.Pattern
		handler := served.routeHandler(route, replays[name])
		for _, sender := range closedClientSenders {
			if sender.keyedOnly && !keyedMethod(route.Method) {
				continue
			}
			requests++
			requestsBefore, errorsBefore := len(accessLines(t, logged)), len(errorLines(logged))
			response, panicked := serveClosedClient(t, name+" as "+sender.name, handler, closedClientRequest(route, sender, row))
			idle := make(chan struct{})
			go func() {
				served.detached.wait()
				close(idle)
			}()
			select {
			case <-idle:
			case <-time.After(closedClientServeLimit):
				failures = append(failures, fmt.Sprintf("%s as %s: the work it detached had not ended after %s",
					name, sender.name, closedClientServeLimit))
			}

			requestLines, faults := accessLines(t, logged)[requestsBefore:], errorLines(logged)[errorsBefore:]
			var wrong []string
			if panicked != nil {
				wrong = append(wrong, fmt.Sprintf("panicked with %v", panicked))
			}
			switch {
			case len(requestLines) != 1:
				wrong = append(wrong, fmt.Sprintf("wrote %d request lines, want one (answered %d)", len(requestLines), response.Code))
			case strings.Contains(requestLines[0], "status=500"):
				wrong = append(wrong, "its request line reads status=500: "+requestLines[0])
			}
			for _, line := range faults {
				wrong = append(wrong, "wrote "+line)
			}
			if len(wrong) > 0 {
				failures = append(failures, fmt.Sprintf("%s as %s: %s", name, sender.name, strings.Join(wrong, "; ")))
			}
		}
		walked++
	}

	if mounted := len(served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	if len(failures) > 0 {
		t.Errorf("%d of the %d requests a client closed, over %d rows, wrote an ERROR line or a request line of 500:\n%s",
			len(failures), requests, walked, strings.Join(failures, "\n"))
	}
	t.Logf("walked %d rows of the table with %d requests their clients closed", walked, requests)
}
