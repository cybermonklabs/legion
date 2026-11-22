package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// closedByClientLine is the one line writeHandlerError writes for a request its client closed.
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

// A route's error is answered as the route's outer layers serve it - the request line, then
// compression - so the line reads what every layer did with the error. Every failure is answered
// its error, a request whose own client closed it among them, so a client still reading never
// takes an empty answer for success. What a closed request changes is the log alone: its line
// says 499, it logs one debug line, and nothing calls it a fault. Every other failure, a cancel
// or a deadline the server made among them, logs its ERROR line as before, and a keyed write's
// handler is answered a 500 the key does not keep.
func TestWriteHandlerErrorAnswersARequestItsClientClosedWithItsErrorAndLogsItAs499(t *testing.T) {
	canceled := fmt.Errorf("list projects: %w", context.Canceled)
	for _, test := range []struct {
		name string
		// closed is whether the request's own context, the one net/http cancels when its client
		// goes, has ended by the time the handler fails.
		closed bool
		err    error
		// keyed answers through the keyed layer's writer, which hides the request's writer.
		keyed bool
		// want is the status on the request line, and fault whether the failure logged its ERROR
		// line. Every row is answered 500 INTERNAL.
		want  int
		fault bool
	}{
		{name: "its client closed it", closed: true, err: canceled, want: accesslog.StatusClientClosed},
		{name: "the server canceled its own context", err: canceled, want: http.StatusInternalServerError, fault: true},
		{name: "its handler's own deadline passed", err: fmt.Errorf("list projects: %w", context.DeadlineExceeded), want: http.StatusInternalServerError, fault: true},
		{name: "a failure of its own", err: errors.New("list projects: connection refused"), want: http.StatusInternalServerError, fault: true},
		{name: "a failure of its own after its client closed it", closed: true, err: errors.New("list projects: connection refused"), want: http.StatusInternalServerError, fault: true},
		{name: "a keyed write whose client closed it", closed: true, keyed: true, err: canceled, want: http.StatusInternalServerError, fault: true},
	} {
		for _, encoding := range []string{"gzip", ""} {
			t.Run(fmt.Sprintf("%s, Accept-Encoding %q", test.name, encoding), func(t *testing.T) {
				logged := captureDebugLog(t)
				ctx, closeRequest := context.WithCancel(context.Background())
				defer closeRequest()
				var keyedStatus int
				handler := accesslog.HandlerFunc("/api/v1/projects", compressAnswers(func(w http.ResponseWriter, r *http.Request) {
					if test.closed {
						closeRequest()
					}
					if !test.keyed {
						(&server{}).writeHandlerError(w, test.err)
						return
					}
					answer := &keyedAnswer{w: w}
					(&server{}).writeHandlerError(answer, test.err)
					keyedStatus = answer.finalStatus()
					answer.deliver()
				}))
				request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil).WithContext(ctx)
				if encoding != "" {
					request.Header.Set("Accept-Encoding", encoding)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)

				lines := accessLines(t, logged)
				if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", test.want)) {
					t.Errorf("request lines %q, want one with status=%d", lines, test.want)
				}
				closed, failures := handlerErrorLines(logged)
				if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"`+codeInternal+`"`) {
					t.Errorf("answered %d %q, want the 500 %s it met, which a client still reading takes as the failure it is", response.Code, response.Body.String(), codeInternal)
				}
				if test.fault {
					if len(failures) != 1 || len(closed) != 0 {
						t.Errorf("logged %q and %q, want one ERROR line and no closed-request line", failures, closed)
					}
				} else if len(failures) != 0 || len(closed) != 1 {
					t.Errorf("logged %q and %q, want no ERROR line and one debug line for a request its client closed", failures, closed)
				}
				if test.keyed && keyedStatus != http.StatusInternalServerError {
					t.Errorf("the keyed layer read %d, want the 500 it releases its key on rather than an answer it keeps", keyedStatus)
				}
			})
		}
	}
}

// The project list a page cancels as it navigates away, as a browser does, while its statement
// waits on the database: the request line says 499 and no ERROR line calls it a server fault,
// while the request is still answered the error it met. The statement waits on a lock this test
// holds, so the cancel lands while the statement runs, which is when the browser's did.
func TestAProjectListItsClientClosedMidStatementIsNoServerFault(t *testing.T) {
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

	ctx, closeRequest := context.WithCancel(context.Background())
	defer closeRequest()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil).WithContext(ctx)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(response, request)
	}()
	// The bound is the test binary's own deadline less its margin, as testwait.Until's is.
	if storetest.WaitForStatementBehind(t, database, lock, projectListQuery, testwait.Bounded(t, time.Hour, 30*time.Second)) == 0 {
		t.Fatal("the project list's statement never queued behind the lock on projects")
	}
	closeRequest()
	testwait.Receive(t, "the project list's handler to return", served)
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}

	var lines []string
	for _, line := range accessLines(t, logged) {
		if strings.Contains(line, " route=/api/v1/projects ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", accesslog.StatusClientClosed)) {
		t.Errorf("request lines %q, want one with status=%d", lines, accesslog.StatusClientClosed)
	}
	closed, failures := handlerErrorLines(logged)
	if len(failures) != 0 || len(closed) != 1 {
		t.Errorf("logged %q and %q, want no ERROR line and one debug line for a request its client closed", failures, closed)
	}
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"`+codeInternal+`"`) {
		t.Errorf("answered %d %q, want the 500 %s it met", response.Code, response.Body.String(), codeInternal)
	}
}

// A client that half-closes its side while its write waits is a client net/http counts as closed:
// its read of the connection meets the end of the stream, which ends the request's context, and
// the cancel rolls the write back. That client still reads, so it must read the error the write
// met, not an empty 200 that says the write succeeded. The write waits on a lock this test holds,
// so the half-close lands while its statement runs.
func TestAHalfClosedClientWhoseWriteRolledBackReadsTheError(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	logged := captureDebugLog(t)
	listener := httptest.NewServer(handler)
	defer listener.Close()
	lock, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the lock's transaction: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(t.Context(), `lock table projects in access exclusive mode`); err != nil {
		t.Fatalf("lock projects: %v", err)
	}

	conn, err := net.Dial("tcp", listener.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	defer conn.Close()
	body := `{"key":"HALF","name":"Half closed"}`
	if _, err := fmt.Fprintf(conn, "POST /api/v1/projects HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nX-Dispatch-User: alice\r\nContent-Length: %d\r\n\r\n%s",
		listener.Listener.Addr(), len(body), body); err != nil {
		t.Fatalf("send the write: %v", err)
	}
	testwait.Until(t, "the project write's insert waiting on the lock", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(t.Context(), database, "%insert into projects%")
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting == 1
	})
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close the client's side: %v", err)
	}
	// The server's read meets the end of the stream and ends the request's context, which cancels
	// the insert; the lock is released only then, so nothing can land first.
	testwait.Until(t, "the half-closed write's insert to stop waiting", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(t.Context(), database, "%insert into projects%")
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting == 0
	})
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the answer on the half-closed connection: %v", err)
	}
	defer answer.Body.Close()
	read, _ := io.ReadAll(answer.Body)
	if answer.StatusCode != http.StatusInternalServerError || !strings.Contains(string(read), `"code":"`+codeInternal+`"`) {
		t.Errorf("the half-closed client read %d %q, want the 500 %s its rolled-back write met, not a success", answer.StatusCode, read, codeInternal)
	}
	var stored int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from projects where key = 'HALF'`).Scan(&stored); err != nil {
		t.Fatalf("count the project: %v", err)
	}
	if stored != 0 {
		t.Fatalf("the cancelled write stored %d projects, want 0: the row's premise is a write the cancel rolled back", stored)
	}
	var lines []string
	for _, line := range accessLines(t, logged) {
		if strings.Contains(line, " route=/api/v1/projects ") && strings.Contains(line, " method=POST ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", accesslog.StatusClientClosed)) {
		t.Errorf("request lines %q, want one with status=%d", lines, accesslog.StatusClientClosed)
	}
	if closed, failures := handlerErrorLines(logged); len(failures) != 0 || len(closed) != 1 {
		t.Errorf("logged %q and %q, want no ERROR line and one debug line", failures, closed)
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
