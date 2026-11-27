package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// closedByClientLine is the one line closedByClient writes for a request its client closed.
const closedByClientLine = `level=DEBUG msg="dispatch: API request closed by its client"`

// captureDebugLog is captureLog with debug lines kept, so a test reads the closed request's line.
func captureDebugLog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logged
}

// handlerErrorLines are the lines a buffer holds that writeHandlerError wrote for a failure: the
// closed request's debug line, writeHandlerError's own ERROR line, and any ERROR line naming a
// cancel.
func handlerErrorLines(logged *logBuffer) (closed, failures []string) {
	for _, line := range strings.Split(logged.String(), "\n") {
		switch {
		case strings.Contains(line, closedByClientLine):
			closed = append(closed, line)
		case strings.Contains(line, "level=ERROR") &&
			(strings.Contains(line, `msg="dispatch: API handler failed"`) || strings.Contains(line, "context canceled")):
			failures = append(failures, line)
		}
	}
	return closed, failures
}

// errorLines are every level=ERROR line a buffer holds, whoever wrote it, in order.
func errorLines(logged *logBuffer) []string {
	return strings.FieldsFunc(logged.Lines("level=ERROR"), func(r rune) bool { return r == '\n' })
}

// routeLines are the request lines a buffer holds for method and route.
func routeLines(t *testing.T, logged *logBuffer, method, route string) []string {
	t.Helper()
	var lines []string
	for _, line := range accessLines(t, logged) {
		if strings.Contains(line, " method="+method+" ") && strings.Contains(line, " route="+route+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// A search whose client closes it while its meaning statement waits, as the web bar closes each
// search it replaces while a person types, is no server fault: its request line reads 499, it
// writes one debug line and no level=ERROR line, and it is still answered the 500 it met, which a
// client still reading takes for the failure it is. The line names the search's key and shape,
// noted before the statement ran. The statement waits on embeddings, which the test holds
// (holdMeaningStatements), so the close lands while the statement runs, which is when the bar's
// did.
func TestASearchItsClientClosedIsLoggedAs499(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "CLOSED", title, spec)
	processAllPending(t, database, embedder)
	// The query shares no word with the issue, so the hit came from the meaning statement: that is
	// the statement the closed search below waits in.
	if healthy := searchResponse(t, handler, "q=wibbleflorp"); healthy.Degraded != "" || len(healthy.Results) != 1 || healthy.Results[0].ID != issue.Key {
		t.Fatalf("search before the hold = %+v, want the issue %s found by meaning", healthy, issue.Key)
	}

	logged := captureDebugLog(t)
	release := holdMeaningStatements(t, database)
	ctx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(response, aliceSearch(ctx, "/api/v1/search", "q=wibbleflorp"))
	}()
	testwait.Until(t, "the search's meaning statement to wait on embeddings", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%embeddings%")
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting == 1
	})
	closeClient()
	testwait.Receive(t, "the closed search's handler to return", served)
	release()

	lines := routeLines(t, logged, http.MethodGet, "/api/v1/search")
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", accesslog.StatusClientClosed)) {
		t.Fatalf("request lines %q, want one with status=%d", lines, accesslog.StatusClientClosed)
	}
	shape, _ := horizontest.Field(lines[0], "search_shape")
	if _, keyed := horizontest.Field(lines[0], "search_key"); !keyed || shape != "meaning-every" {
		t.Errorf("the closed search's line names no search_key or no search_shape=meaning-every: %s", lines[0])
	}
	if faults := errorLines(logged); len(faults) != 0 {
		t.Errorf("a search its client closed wrote level=ERROR lines:\n%s", strings.Join(faults, "\n"))
	}
	if closed, _ := handlerErrorLines(logged); len(closed) != 1 {
		t.Errorf("logged %q, want one debug line for a request its client closed", closed)
	}
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"`+codeInternal+`"`) {
		t.Errorf("answered %d %q, want the 500 %s it met", response.Code, response.Body.String(), codeInternal)
	}
}

// A cancel the client did not cause leaves the request's own context live, so writeHandlerError
// still answers it 500 with its ERROR line and the request line reads 500: a handler's context
// the server cancelled, a handler's own deadline, and a failure of the handler's own after its
// client closed, which is no cancel at all. A request its own client closed, the last row, is the
// one that reads 499 with the debug line in place of the ERROR line. Each request is served
// through the line's recorder and the compression layer, as a route's error is, so the walk to
// the recorder crosses a layer, with and without a body being compressed.
func TestAServerCancelStillAnswers500WithItsErrorLine(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	for _, test := range []struct {
		name string
		// closed is whether the request's own context, the one net/http cancels when its client
		// goes, has ended by the time the handler fails.
		closed bool
		err    error
		// fault is whether the failure is a server fault: line 500 and its ERROR line, against
		// line 499 and the debug line. Every row is answered 500 INTERNAL.
		fault bool
	}{
		{name: "the server cancelled the handler's context", err: fmt.Errorf("list projects: %w", context.Canceled), fault: true},
		{name: "the handler's own deadline passed", err: fmt.Errorf("list projects: %w", context.DeadlineExceeded), fault: true},
		{name: "a failure of its own after its client closed it", closed: true, err: errors.New("list projects: connection refused"), fault: true},
		{name: "its client closed it", closed: true, err: fmt.Errorf("list projects: %w", context.Canceled)},
	} {
		for _, encoding := range []string{"gzip", ""} {
			t.Run(fmt.Sprintf("%s, Accept-Encoding %q", test.name, encoding), func(t *testing.T) {
				logged := captureDebugLog(t)
				ctx, closeRequest := context.WithCancel(context.Background())
				defer closeRequest()
				handler := accesslog.HandlerFunc("/api/v1/projects", compressAnswers(func(w http.ResponseWriter, r *http.Request) {
					if test.closed {
						closeRequest()
					}
					(&server{}).writeHandlerError(w, test.err)
				}))
				request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil).WithContext(ctx)
				if encoding != "" {
					request.Header.Set("Accept-Encoding", encoding)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)

				want := http.StatusInternalServerError
				if !test.fault {
					want = accesslog.StatusClientClosed
				}
				if lines := accessLines(t, logged); len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", want)) {
					t.Errorf("request lines %q, want one with status=%d", lines, want)
				}
				if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"`+codeInternal+`"`) {
					t.Errorf("answered %d %q, want the 500 %s it met", response.Code, response.Body.String(), codeInternal)
				}
				closed, failures := handlerErrorLines(logged)
				if test.fault {
					if len(failures) != 1 || len(closed) != 0 {
						t.Errorf("logged %q and %q, want one ERROR line and no closed-request line", failures, closed)
					}
				} else if len(failures) != 0 || len(closed) != 1 {
					t.Errorf("logged %q and %q, want no ERROR line and one debug line for a request its client closed", failures, closed)
				}
			})
		}
	}
}

// A keyed write runs its handler on a context its client's close does not end
// (context.WithoutCancel in keyedWrites), so a client that closes while the write waits leaves it
// to finish: the write commits, its key stores the answer, which a repeat under the key is
// replayed, and the request line keeps the 201 the write answered, with no level=ERROR line from
// the close. The insert waits on a lock this test holds on projects, so the close lands while the
// write runs.
func TestAKeyedWriteItsClientClosedFinishesWithNoErrorLine(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	handler, database := newTestHandlerWithStore(t)
	logged := captureDebugLog(t)
	lock, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the lock's transaction: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(t.Context(), `lock table projects in access exclusive mode`); err != nil {
		t.Fatalf("lock projects: %v", err)
	}

	const key, body = "keyed-client-closed", `{"key":"KEYED","name":"Keyed"}`
	keyedCreate := func(ctx context.Context) *http.Request {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/projects", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		return request
	}
	ctx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(response, keyedCreate(ctx))
	}()
	testwait.Until(t, "the keyed write's insert to wait on the lock", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%insert into projects%")
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting == 1
	})
	closeClient()
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	testwait.Receive(t, "the keyed write's handler to return", served)

	if response.Code != http.StatusCreated {
		t.Fatalf("the keyed write answered %d %s, want the 201 of the write it ran to its end", response.Code, response.Body.String())
	}
	var stored int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from projects where key = 'KEYED'`).Scan(&stored); err != nil {
		t.Fatalf("count the project: %v", err)
	}
	if stored != 1 {
		t.Fatalf("the keyed write stored %d projects, want the 1 it committed", stored)
	}
	lines := routeLines(t, logged, http.MethodPost, "/api/v1/projects")
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", http.StatusCreated)) {
		t.Errorf("request lines %q, want one with the write's status=%d", lines, http.StatusCreated)
	}
	if faults := errorLines(logged); len(faults) != 0 {
		t.Errorf("a keyed write its client closed wrote level=ERROR lines:\n%s", strings.Join(faults, "\n"))
	}

	repeat := httptest.NewRecorder()
	handler.ServeHTTP(repeat, keyedCreate(context.Background()))
	if repeat.Code != http.StatusCreated || repeat.Header().Get(contracts.IdempotencyReplayedHeader) != "?1" || repeat.Body.String() != response.Body.String() {
		t.Fatalf("the repeat under the key answered %d %s (%s: %q), want the stored 201 replayed",
			repeat.Code, repeat.Body.String(), contracts.IdempotencyReplayedHeader, repeat.Header().Get(contracts.IdempotencyReplayedHeader))
	}
}

// No line a request writes carries a value of a header that names its caller: the header
// identity's email, a bearer token, a cookie. The header identity's case is the one that writes
// an ERROR line: its email holds a NUL, which the people table refuses, so the person's record
// fails and writeHandlerError logs that failure, the path CodeQL's go/clear-text-logging traced
// from the header to the ERROR line. The bearer's and the cookie's are refused 401 with the
// request line alone. Each case first finds its request line, so a run that logged nothing
// cannot pass.
func TestARequestLogsNoValueOfAHeaderThatNamesItsCaller(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	const canary = "c4815-credential-canary"
	handler, _ := newTestHandlerWithStore(t)
	for _, test := range []struct {
		name, header, value string
		// failed is whether the request fails with 500 and its ERROR line.
		failed bool
	}{
		{name: "the header identity's email", header: "X-Dispatch-User", value: canary + "\x00@example.com", failed: true},
		{name: "a bearer token", header: "Authorization", value: "Bearer dsp_" + canary},
		{name: "a session cookie", header: "Cookie", value: "dispatch_session=" + canary},
	} {
		t.Run(test.name, func(t *testing.T) {
			logged := captureDebugLog(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if lines := accessLines(t, logged); len(lines) != 1 || !strings.Contains(lines[0], " route=/api/v1/projects ") {
				t.Fatalf("request lines %q, want the request's one line", lines)
			}
			_, failures := handlerErrorLines(logged)
			if test.failed && (response.Code != http.StatusInternalServerError || len(failures) != 1) {
				t.Fatalf("answered %d and logged %q, want 500 and its one ERROR line", response.Code, failures)
			}
			if !test.failed && response.Code != http.StatusUnauthorized {
				t.Fatalf("answered %d %q, want 401", response.Code, response.Body.String())
			}
			for _, line := range strings.Split(logged.String(), "\n") {
				if strings.Contains(line, canary) {
					t.Errorf("a log line carries the %s header's value: %q", test.header, line)
				}
			}
		})
	}
}
