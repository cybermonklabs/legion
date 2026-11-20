package main

// The listener's stop waits for the work a handler left behind its answer, and starts none once
// it has begun (awaitAfterAnswer, runAfterAnswer): a registration answered 503 a moment before the
// stop still has its consumer's filter set to the row the bucket holds, and one answered after it
// says by name that it left that filter to the session's next registration. shutdownListener
// ends in os.Exit, so these cases hold the wait it calls to that.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/store"
)

// outrunEveryPlan makes every plan of a registration of sessionID on rig outrun by a writer that
// stores a topic of its own and sets no consumer filter, as a role claim does, so the
// registration is answered 503 and leaves the settle of its consumer's filter behind its answer
// (registerSession). The hook is rig's until the case takes it away.
func outrunEveryPlan(t *testing.T, rig *sessionConsumerRig, sessionID, prefix string) {
	t.Helper()
	passes := 0
	rig.deps.consumers.beforeRegister = func() {
		passes++
		topic := fmt.Sprintf("notifications.dispatch.issue.%s-%d.>", prefix, passes)
		if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
			t.Errorf("store %s under the registration's plan: %v", topic, err)
		}
	}
}

// The stop's wait does not return while the settle a 503 left behind it is running: the settle
// is held inside its fence, and the wait returns only once it is let go, with the consumer then
// filtered on the row the bucket holds.
func TestTheStopWaitsForTheSettleA503LeftBehindIt(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_stop_waits"
	outrunEveryPlan(t, rig, sessionID, "STOPWAITS")
	inside := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() {
			close(inside)
			<-release
		})
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(consumerRegistration(sessionID, "epoch-1", "notifications.team.stopwaits"))))
	if recorder.Code != http.StatusServiceUnavailable {
		close(release)
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-inside:
	case <-time.After(consumerFilterFenceWait):
		close(release)
		t.Fatal("the settle left behind the 503 never read the bucket's row")
	}

	stopped := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
		defer cancel()
		rig.deps.consumers.awaitAfterAnswer(ctx)
		close(stopped)
	}()
	select {
	case <-stopped:
		close(release)
		t.Fatal("the stop's wait returned while the settle left behind the 503 was still running")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(consumerFilterFenceWait):
		t.Fatal("the stop's wait did not return once the settle ended")
	}
	rig.deps.consumers.beforeRegister = nil
	rig.deps.consumers.afterStoredRowRead = nil
	requireFilteredOnStoredRow(t, rig, sessionID)
}

// A registration answered 503 once the stop has begun starts no settle: the work would run on
// the connection the stop drains. It says so by name, leaving the filter to the session's next
// registration, rather than losing the settle without a line.
func TestASettleTheListenerIsTooLateToStartIsLoggedByName(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_settle_too_late"
	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	rig.deps.consumers.awaitAfterAnswer(ctx)

	outrunEveryPlan(t, rig, sessionID, "TOOLATE")
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(consumerRegistration(sessionID, "epoch-1", "notifications.team.toolate"))))
	rig.deps.consumers.beforeRegister = nil
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	const line = "session consumer filter not settled after a registration this listener did not answer: the listener is stopping; the session's next registration sets it"
	requireFilterLineLogged(t, rig, sessionID, line, "the registration answered 503 after the stop started no settle")
}
