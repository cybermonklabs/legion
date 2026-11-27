package main

// An unsubscribe's refollow writes the session consumer's filter under the session's lock, and it
// waits for that lock only until the route's budget ends (sendRequestBudget). A lock held past
// that budget leaves the filter as it was, and the route still answers 200 inside the client's
// attempt. The fences on the same write are session_consumer_filter_fence_test.go's.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lockNotFreeLine is what a write of a session consumer's filter logs when the session's lock was
// not free within the write's budget (storedFilterWrite).
const lockNotFreeLine = "session consumer filter left unchanged: the session's lock was not free within the filter write's budget; the session's next heartbeat sets it"

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: an answer between the route's 3.5 s budget and the client's 5 s attempt.
//
// An unsubscribe whose session's lock is held past the route's budget answers 200 and leaves the
// consumer's filter as it was. A registration of the session holds that lock across its consumer
// call and its writes, and the test holds it here in the same way. The route's filter write
// (refollow) waits for the lock only until the route's budget of 3.5 s ends (sendRequestBudget,
// storedFilterWrite, lockWithin), because the client reads no answer after its own 5 s attempt.
// The topic is out of the row by then, so the route answers 200 and the listener logs one WARN
// line. The filter keeps the topic until a later filter write. Control: the same unsubscribe sent
// again with the lock free is such a write, and it takes the topic out of the filter.
func TestAnUnsubscribeWhoseSessionLockIsHeldPastTheBudgetAnswers200AndLeavesTheFilter(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_refollow_lock_held"
	const kept = "notifications.dispatch.issue.LOCKHELDKEPT.>"
	const dropped = "notifications.dispatch.issue.LOCKHELDDROP.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))
	unsubscribe := `{"session_id":"` + sessionID + `","topics":["` + dropped + `"]}`

	unlock := rig.deps.consumers.locks.lock(sessionID)
	started := time.Now()
	answered := postAsync(rig, "/v1/interests/unsubscribe", unsubscribe)
	// The unsubscribe has removed its topic and waits for the lock to write the filter.
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 2)
	// The test waits for the answer with the lock still held. The wait ends at a minute, far past
	// the route's budget, or earlier at the test's deadline. A write that waits for the lock with
	// no bound gives no answer before the test lets the lock go.
	var status int
	select {
	case status = <-answered:
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		unlock()
		t.Fatalf("the unsubscribe gave no answer while the test held the session's lock, want one once the route's %s ended (sendRequestBudget)", sendRequestBudget)
	}
	took := time.Since(started)
	unlock()
	t.Logf("with the session's lock held, the unsubscribe answered %d after %s", status, took.Round(time.Millisecond))
	if status != http.StatusOK {
		t.Fatalf("with the session's lock held, the unsubscribe answered %d, want 200: the topic is out of the row, and only the filter write gave up", status)
	}
	// The answer comes after the route's budget ends and before the client's attempt ends. The
	// test holds the lock for the whole budget, so an earlier answer did not come from the end of
	// the lock wait's bound. The upper bound is the client's attempt (contracts.ListenerCallTimeout,
	// 5 s), not a margin the test chose. The route's budget is seven tenths of that attempt, so the
	// 1.5 s between the two is the route's own margin. An answer after the attempt reaches no
	// client on any machine, so this bound fails only a route whose answer the client cannot read.
	if took < sendRequestBudget || took >= contracts.ListenerCallTimeout {
		t.Errorf("with the session's lock held, the unsubscribe answered after %s, want once the route's budget ended (sendRequestBudget, %s) and before the client's attempt ended (contracts.ListenerCallTimeout, %s)",
			took.Round(time.Millisecond), sendRequestBudget, contracts.ListenerCallTimeout)
	}
	// The wait that gave up takes the lock once it is free and releases it at once (lockWithin),
	// so nothing writes the filter after the test lets the lock go.
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 0)
	requireFilters(t, rig, sessionID, kept, dropped)

	rig.post(t, "/v1/interests/unsubscribe", unsubscribe)
	requireFilters(t, rig, sessionID, kept)
	// One line for the two unsubscribes: the control's write found the lock free.
	rig.requireOneLogLine(t, "WARN", lockNotFreeLine, sessionID, map[string]any{
		"after": filterAfterUnsubscribe,
		"error": context.DeadlineExceeded.Error(),
	})
}
