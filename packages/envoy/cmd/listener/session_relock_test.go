package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A call that lost its connection under a session's lock releases the lock, waits for the
// connection, and takes the lock again, all inside its own budget (relockAfterReconnect): a
// registration of the session that takes the lock while the call waits for the connection, and
// keeps it past that budget, leaves the relock answering no retry at the budget's end, holding
// nothing, rather than waiting for that holder.
//
// What the row decides is an order, not a time: the budget is a context the row ends itself, once
// it has seen the relock waiting on the other holder (the lock's second user), and the relock must
// then answer no retry while that holder still holds. Every wait is bounded by the test's deadline
// alone (testnats.Remaining).
//
// The control is the same row at 2b8a5d7cbb: the relock took the lock with no bound once the
// connection was back, so it went on waiting for the other holder after its budget had ended, and
// the row fails at the test's deadline.
func TestARelockPastItsBudgetGivesUpRatherThanWaitForAnotherHolder(t *testing.T) {
	proxy := testnats.StartProxy(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	m := &sessionDelivery{}
	const sessionID = "ses_relock_budget"
	unlock := m.locks.lock(sessionID)

	proxy.Cut()
	for client.Connected() {
		time.Sleep(time.Millisecond)
	}
	_, lost := bus.Request(context.Background(), client, func(context.Context) (struct{}, error) { return struct{}{}, nil })
	if !errors.Is(lost, bus.ErrConnectionLost) {
		t.Fatalf("a request while the connection was down answered %v, want ErrConnectionLost", lost)
	}

	budget, end := context.WithCancel(context.Background())
	defer end()
	retried := make(chan bool, 1)
	go func() { retried <- m.relockAfterReconnect(budget, sessionID, &unlock, lost) }()
	// The other holder takes the lock the moment the relock releases it, and keeps it to the end;
	// this returns once the relock has released it.
	other := m.locks.lock(sessionID)
	t.Cleanup(other)
	proxy.Restore()
	// Once the connection is back the relock waits for the lock: it is the lock's second user.
	for lockUsers(&m.locks, sessionID) < 2 {
		select {
		case again := <-retried:
			t.Fatalf("the relock answered %t before it waited for the lock", again)
		default:
		}
		if testnats.Remaining(t) <= 0 {
			t.Fatal("the relock did not wait for the lock by the test's deadline")
		}
		time.Sleep(time.Millisecond)
	}
	end()

	select {
	case again := <-retried:
		if again {
			t.Fatal("the relock reported a retry once its budget had ended, with another holder keeping the lock")
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the relock was still waiting for the other holder at the test's deadline, after its budget had ended")
	}
	// The release the caller makes on its way out releases nothing: the other holder still holds.
	unlock()
	if users := lockUsers(&m.locks, sessionID); users != 1 {
		t.Fatalf("%d callers hold or wait for the lock after the caller's release, want the other holder alone", users)
	}
}
