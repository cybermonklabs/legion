package main

// An unsubscribe's refollow writes the session consumer's filter under the session's lock, and it
// waits for that lock only until the route's budget ends (sendRequestBudget). A lock held past
// that budget leaves the filter as it was, and the route still answers 200 inside the client's
// attempt. The fences on the same write are session_consumer_filter_fence_test.go's.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lockNotFreeLine is what a write of a session consumer's filter logs when the session's lock was
// not free within the write's budget (storedFilterWrite).
const lockNotFreeLine = "session consumer filter left unchanged: the session's lock was not free within the filter write's budget; the session's next heartbeat sets it"

// An unsubscribe whose session's lock is held past the route's budget answers 200 and leaves the
// consumer's filter as it was. A registration of the session holds that lock across its consumer
// call and its writes, and the test holds it here in the same way. The route's filter write
// (refollow) waits for the lock only until the route's budget of 3.5 s ends (sendRequestBudget,
// storedFilterWrite, lockWithin), because the client reads no answer after its own 5 s attempt.
// The topic is out of the row by then, so the route answers 200 and the listener logs one WARN
// line. The filter keeps the topic until a later filter write. Control: the same unsubscribe sent
// again with the lock free is such a write, and it takes the topic out of the filter.
func TestAnUnsubscribeWhoseSessionLockIsHeldPastTheBudgetAnswers200AndLeavesTheFilter(t *testing.T) {
	t.Parallel()
	url := testnats.URL(t)
	rig := newPromptSessionConsumerRig(t, url, url)
	fake := clocktest.New(time.Unix(0, 0))
	rig.deps.clock = fake
	rig.deps.consumers.clock = fake
	observer, err := bus.ConnectSessionConsumers([]string{url}, 0, nil)
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(observer.Close)
	const sessionID = "ses_refollow_lock_held"
	const kept = "notifications.dispatch.issue.LOCKHELDKEPT.>"
	const dropped = "notifications.dispatch.issue.LOCKHELDDROP.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))
	unsubscribe := `{"session_id":"` + sessionID + `","topics":["` + dropped + `"]}`

	const path = "/v1/interests/unsubscribe"
	post := func() <-chan *httptest.ResponseRecorder {
		return routeAnswer(t, rig.mux.ServeHTTP, httptest.NewRequest(http.MethodPost, path, strings.NewReader(unsubscribe)))
	}
	locks := &rig.deps.consumers.locks
	awaitUsers := func(want int, what string) {
		t.Helper()
		testwait.Until(t, what, func() (string, bool) {
			users := lockUsers(locks, sessionID)
			return fmt.Sprintf("%d callers hold or wait for the lock, want %d", users, want), users == want
		})
	}
	unlock := sync.OnceFunc(locks.lock(sessionID))
	t.Cleanup(unlock)
	deadline := fake.Now().Add(sendRequestBudget)
	answered := post()
	fake.AwaitArmedAt(t, "the held-lock unsubscribe's request budget", deadline)
	awaitUsers(2, "the unsubscribe to wait for the held session lock")
	// The consumer has a budget of its own, but the earlier route deadline must end the wait.
	if armed := fake.Armed(); len(armed) == 0 || !armed[0].Equal(deadline) {
		t.Fatalf("the held-lock unsubscribe armed %v, want the request budget %s to end the wait first", armed, deadline)
	}
	fake.Set(deadline)
	status := testwait.Receive(t, "the unsubscribe to answer at its budget while the session lock is still held", answered).Code
	if status != http.StatusOK {
		t.Fatalf("with the session's lock held, the unsubscribe answered %d, want 200: the topic is out of the row, and only the filter write gave up", status)
	}
	unlock()
	// The wait that gave up takes the lock once it is free and releases it at once (lockWithin),
	// so nothing writes the filter after the test lets the lock go.
	awaitUsers(0, "the held session lock and its canceled waiter to release")
	requireFilter := func(topics ...string) {
		t.Helper()
		ctx, cancel := clock.WithShownDeadline(testwait.Context(t))
		defer cancel()
		info, err := observer.Info(ctx, sessionID)
		if err != nil {
			t.Fatalf("read the consumer of %s: %v", sessionID, err)
		}
		got := slices.Clone(info.Config.FilterSubjects)
		if info.Config.FilterSubject != "" {
			got = append(got, info.Config.FilterSubject)
		}
		slices.Sort(got)
		want := bus.SessionConsumerFilters(append([]string{contracts.LegacyAgentSubject(sessionID)}, topics...))
		if !slices.Equal(got, want) {
			t.Fatalf("the consumer of %s filters on %v, want %v", sessionID, got, want)
		}
	}
	requireFilter(kept, dropped)

	if control := testwait.Receive(t, "the control unsubscribe with the session lock free to answer", post()); control.Code != http.StatusOK {
		t.Fatalf("with the session's lock free, the unsubscribe answered %d (%s), want 200", control.Code, control.Body.String())
	}
	requireFilter(kept)
	// One line for the two unsubscribes: the control's write found the lock free.
	rig.requireOneLogLine(t, "WARN", lockNotFreeLine, sessionID, map[string]any{
		"after": filterAfterUnsubscribe,
		"error": context.DeadlineExceeded.Error(),
	})
}
