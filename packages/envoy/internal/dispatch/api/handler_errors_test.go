package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/testwait"
)

// handlerFailureLines are the lines a buffer holds that call a request's failure a server fault:
// writeHandlerError's own ERROR line, and any ERROR line naming a cancel.
func handlerFailureLines(logged *logBuffer) []string {
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "level=ERROR") &&
			(strings.Contains(line, `msg="dispatch: API handler failed"`) || strings.Contains(line, "context canceled")) {
			lines = append(lines, line)
		}
	}
	return lines
}

// A route's error is answered as the route's outer layers serve it - the request line, then
// compression - so the line reads what every layer did with the error. A request whose own
// client closed it is answered nothing, its line says 499 and nothing calls it a fault; every
// other failure, a cancel or a deadline the server made among them, is a 500 with its ERROR line
// as before, and a keyed write's handler is answered a 500 the key does not keep.
func TestWriteHandlerErrorAnswersARequestItsClientClosedWithNothing(t *testing.T) {
	canceled := fmt.Errorf("list projects: %w", context.Canceled)
	for _, test := range []struct {
		name string
		// closed is whether the request's own context, the one net/http cancels when its client
		// goes, has ended by the time the handler fails.
		closed bool
		err    error
		// keyed answers through the keyed layer's writer, which hides the request's writer.
		keyed bool
		// want is the status on the request line, and fault whether a 500 and its ERROR line
		// were written.
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
				logged := captureLog(t)
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
				failures := handlerFailureLines(logged)
				wroteFault := response.Code == http.StatusInternalServerError || strings.Contains(response.Body.String(), "internal server error")
				if test.fault {
					if len(failures) != 1 || !wroteFault {
						t.Errorf("answered %d %q and logged %q, want a 500 and one ERROR line", response.Code, response.Body.String(), failures)
					}
				} else if len(failures) != 0 || wroteFault || response.Body.Len() != 0 {
					t.Errorf("answered %d %q and logged %q, want no answer and no ERROR line for a request nobody is left to read", response.Code, response.Body.String(), failures)
				}
				if test.keyed && keyedStatus != http.StatusInternalServerError {
					t.Errorf("the keyed layer read %d, want the 500 it releases its key on rather than an answer it keeps", keyedStatus)
				}
			})
		}
	}
}

// The project list a page cancels as it navigates away, as a browser does, while its statement
// waits on the database: the request line says 499, no ERROR line calls it a server fault, and no
// 500 is written. The statement waits on a lock this test holds, so the cancel lands while the
// statement runs, which is when the browser's did.
func TestAProjectListItsClientClosedMidStatementIsNoServerFault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	logged := captureLog(t)
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
	testwait.Until(t, "the project list's statement waiting on the lock", func() (int, bool) {
		var waiting int
		if err := database.Pool.QueryRow(t.Context(),
			`select count(*) from pg_stat_activity where wait_event_type = 'Lock' and query = $1`,
			projectListQuery).Scan(&waiting); err != nil {
			t.Fatalf("read the waiting statements: %v", err)
		}
		return waiting, waiting == 1
	})
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
	if failures := handlerFailureLines(logged); len(failures) != 0 {
		t.Errorf("logged %q, want no ERROR line for a request its client closed", failures)
	}
	if response.Code == http.StatusInternalServerError || strings.Contains(response.Body.String(), "internal server error") {
		t.Errorf("answered %d %q, want no 500 for a request nobody is left to read", response.Code, response.Body.String())
	}
}
