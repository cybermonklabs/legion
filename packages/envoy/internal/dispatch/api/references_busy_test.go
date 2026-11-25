package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's references read that cannot get a request-pool connection within the pool's acquire
// bound is the server's busy refusal, 503 DATABASE_BUSY with Retry-After, which its client sends
// again, and not the read's own bound, 503 REFERENCES_TIMEOUT with none: the pool's busy error
// wraps context.DeadlineExceeded, which closureBoundHit reads as the read's bound, so the refusal
// checks store.IsPoolBusy first. The read runs on the real server and its real request pool,
// filled by the one connection the test holds, and its error is answered by the route's own
// classification (writeClosureRefusal).
func TestAReferenceClosureOnABusyRequestPoolIsRefusedDatabaseBusy(t *testing.T) {
	t.Parallel()
	const bound = 300 * time.Millisecond
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{requestPoolSize: 1, acquireBound: bound})
	held, err := database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold the request pool's one connection: %v", err)
	}
	defer held.Release()

	started := time.Now()
	_, err = served.boundedClosure(context.Background(), model.Actor{}, "CORE-1")
	if err == nil {
		t.Fatal("the read on a full request pool answered no error, want the pool's busy refusal")
	}
	if took := time.Since(started); took > bound+time.Second {
		t.Fatalf("the read on a full request pool failed after %v, want within its acquire bound %v", took, bound)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/CORE-1/references", nil)
	recorder := httptest.NewRecorder()
	served.writeClosureRefusal(recorder, request, err)
	assertRetryable(t, recorder.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
}
