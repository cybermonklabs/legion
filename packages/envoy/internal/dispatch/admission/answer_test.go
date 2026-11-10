package admission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// expiredRequest is a request context whose own deadline has passed.
func expiredRequest(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := WithDeadline(context.Background(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

func cancelledRequest() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func assertRetryable(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status=%d body=%s, want %d %s", w.Code, w.Body.String(), status, code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestWriteUnservedTellsTheCausesApartInOrder(t *testing.T) {
	busy := fmt.Errorf("read issue: %w", fmt.Errorf("requests pool: %w: %w", store.ErrPoolBusy, context.DeadlineExceeded))
	commitCutOff := fmt.Errorf("commit the write: %w: %w", store.ErrCommitTimeout, context.DeadlineExceeded)
	statementUnknown := fmt.Errorf("revoke the token: %w", fmt.Errorf("requests pool: %w: %w",
		store.ErrStatementOutcomeUnknown, errors.New("timeout: read tcp: i/o timeout")))
	for _, test := range []struct {
		name   string
		ctx    context.Context
		err    error
		wrote  bool
		status int
		code   string // empty: nothing written
		ok     bool
	}{
		{"the read's deadline passed", expiredRequest(t), context.DeadlineExceeded, false, http.StatusServiceUnavailable, CodeRequestTimeout, true},
		{"the read's deadline passed, whatever the handler failed with", expiredRequest(t), busy, false, http.StatusServiceUnavailable, CodeRequestTimeout, true},
		{"the client went away", cancelledRequest(), context.Canceled, true, 0, "", true},
		{"the client went away while the pool was busy", cancelledRequest(), busy, true, 0, "", true},
		{"the pool was busy", context.Background(), busy, true, http.StatusServiceUnavailable, CodeDatabaseBusy, true},
		// A commit the database never answered may have committed whatever the request is, so
		// it is never offered the retry a busy pool is.
		{"a commit ran out its bound", context.Background(), commitCutOff, true, http.StatusInternalServerError, CodeCommitOutcomeUnknown, true},
		// The statement a pool ran outside a transaction turns on the one fact its caller
		// holds: a write's may have written, a read's commits nothing.
		{"a write's statement outside a transaction", context.Background(), statementUnknown, true, http.StatusInternalServerError, CodeCommitOutcomeUnknown, true},
		{"a read's statement outside a transaction", context.Background(), statementUnknown, false, http.StatusServiceUnavailable, CodeDatabaseBusy, true},
		// The 500 goes to nobody when the client has left, and the ERROR line records it.
		{"the client went away mid-commit", cancelledRequest(), commitCutOff, true, 0, "", true},
		{"another deadline ran out on a live request", context.Background(), fmt.Errorf("call: %w", context.DeadlineExceeded), true, 0, "", false},
		{"anything else", context.Background(), errors.New("boom"), true, 0, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if ok := WriteUnserved(w, test.ctx, test.err, test.wrote); ok != test.ok {
				t.Fatalf("WriteUnserved = %v, want %v", ok, test.ok)
			}
			if test.code == "" {
				if w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatalf("wrote status=%d headers=%v body=%s, want nothing", w.Code, w.Header(), w.Body.String())
				}
				return
			}
			if test.code == CodeCommitOutcomeUnknown {
				if w.Code != test.status || !strings.Contains(w.Body.String(), `"code":"`+test.code+`"`) {
					t.Fatalf("status=%d body=%s, want %d %s", w.Code, w.Body.String(), test.status, test.code)
				}
				if retry := w.Header().Get("Retry-After"); retry != "" {
					t.Fatalf("Retry-After = %q, want none: the call may have committed", retry)
				}
				return
			}
			assertRetryable(t, w, test.status, test.code)
		})
	}
}

// A request whose client went away is answered nothing, and its access-log line says so: 499,
// nginx's client-closed code, where a handler that writes nothing is otherwise logged as the 200
// of a request served. The write that rolled back because its client left was read as a success.
func TestARequestWhoseClientWentAwayIsLogged499(t *testing.T) {
	logged := &bytes.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	handler := accesslog.Handler("/api/v1/issues/{key}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !WriteUnserved(w, r.Context(), fmt.Errorf("patch issue: %w", context.Canceled), !Reads(r.Method)) {
			t.Error("WriteUnserved did not answer a request whose client went away")
		}
	}))
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/TEST-1", nil).WithContext(cancelledRequest())
	handler.ServeHTTP(httptest.NewRecorder(), request)

	line := logged.String()
	if !strings.Contains(line, "msg=\""+accesslog.Message+"\"") || !strings.Contains(line, "status=499") {
		t.Fatalf("the access-log line of a request whose client went away:\n%s\nwant one carrying status=499", line)
	}
}

// A refusal is 429 Too Many Requests, not 503: nothing ran, the server is up, and the client is
// told to slow down and retry after Retry-After.
func TestWriteRefusedAnswersTooManyRequestsOverloaded(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRefused(w, &Refusal{Class: Write, Reason: QueueFull})
	assertRetryable(t, w, http.StatusTooManyRequests, CodeOverloaded)
}

// A download's deadline bounds the work before its answer and stops there
// (WithDeadlineUntilAnswer): until stop its passing ends the context with ErrRequestDeadline, as
// WithDeadline's does and DeadlinePassed reads it; after stop nothing but the parent, or cancel,
// ends it, so the answer's own transfer bound is all the rest of the download waits on.
func TestADeadlineStoppedAtTheAnswerBoundsOnlyTheWorkBeforeIt(t *testing.T) {
	const timeout = 20 * time.Millisecond

	ran, cancelRan, stop := WithDeadlineUntilAnswer(context.Background(), timeout)
	defer cancelRan()
	<-ran.Done()
	if !DeadlinePassed(ran) {
		t.Fatalf("a read that never answered ended with cause %v, want %v", context.Cause(ran), ErrRequestDeadline)
	}
	stop() // the answer began after the deadline: the context stays ended.
	if !DeadlinePassed(ran) {
		t.Fatalf("stopping an ended read's deadline left cause %v, want %v", context.Cause(ran), ErrRequestDeadline)
	}

	answered, cancelAnswered, stopAnswered := WithDeadlineUntilAnswer(context.Background(), timeout)
	defer cancelAnswered()
	stopAnswered()
	time.Sleep(4 * timeout)
	if answered.Err() != nil {
		t.Fatalf("a download whose answer began ended %v within %v of its %v deadline, want it left running",
			context.Cause(answered), 4*timeout, timeout)
	}
	cancelAnswered()
	if !errors.Is(answered.Err(), context.Canceled) || DeadlinePassed(answered) {
		t.Fatalf("the cancelled read ended with %v (cause %v), want the plain cancellation of a request that is over",
			answered.Err(), context.Cause(answered))
	}
}
