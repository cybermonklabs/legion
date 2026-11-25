package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A step write whose run's row another transaction holds waits for it no longer than a write
// waits for any lock another writer holds (the store's lockWait), and is refused 503
// DATABASE_BUSY with Retry-After at that bound, so its client sends it again. workflowstore.PutStep
// locks the run's workflow_nodes row FOR UPDATE after the issue's owner lock, which it takes
// shared, so two writes of one run wait on each other there; without a bound the second held its
// request-pool connection and its admission unit for as long as the first held the row: tester
// round 46 measured 150 s on the real binary, ended only by the holder letting go (INFRA-592).
func TestAStepWriteBehindItsRunsHeldRowIsRefusedBusyAtItsBound(t *testing.T) {
	t.Parallel()
	const bound = time.Second
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createTestIssue(t, handler, "LEG", "A step behind a held row")
	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSessionOf("LEG")))

	send := func() *httptest.ResponseRecorder {
		data, err := json.Marshal(implementerEnd(daemonSessionOf("LEG"), "passed", stepEnd))
		if err != nil {
			t.Fatalf("encode the step's end: %v", err)
		}
		request := httptest.NewRequest(http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+id, bytes.NewReader(data)).
			WithContext(store.WithLockWaitForTest(context.Background(), bound))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer agent-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `select 1 from workflow_nodes where id = $1 for update`, id); err != nil {
		t.Fatalf("hold the run's row: %v", err)
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() { answered <- send() }()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
		t.Fatalf("the step's end never queued behind the row the holder holds")
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-answered:
	case <-time.After(10 * time.Second):
		waited := time.Since(started)
		_ = holder.Rollback(ctx)
		<-answered
		t.Fatalf("the step's end was still waiting on its run's row %s after it was sent, past a write's lock bound of %s: "+
			"a step write waits on a held row until its holder lets go", waited.Round(time.Millisecond), bound)
	}
	waited := time.Since(started)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) ||
		response.Header().Get("Retry-After") == "" {
		t.Fatalf("the step's end behind its run's held row: status=%d Retry-After=%q body=%s, want 503 %s with Retry-After",
			response.Code, response.Header().Get("Retry-After"), response.Body.String(), admission.CodeDatabaseBusy)
	}
	if waited < bound {
		t.Fatalf("the step's end was refused %s after it was sent, inside its bound of %s", waited.Round(time.Millisecond), bound)
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 0); waiting != 0 {
		t.Fatalf("%d sessions still wait behind the held row after the refusal, want none", waiting)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the row go: %v", err)
	}
	if again := send(); again.Code != http.StatusOK {
		t.Fatalf("the step's end sent again once the row was free: status=%d body=%s, want 200", again.Code, again.Body.String())
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].Result != "passed" {
		t.Fatalf("the run reads %+v after its end landed, want one record that passed", steps)
	}
}
