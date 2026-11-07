package main

// Every write of a session consumer's filter is built from a read of the interest bucket and
// lands at the server after it. A registration on another listener that stores its row in between
// takes no lock of this process, so the bucket is what orders the two: a write built from the row
// that registration replaced would leave the session answered 200 for a topic its consumer does
// not filter on, and a filter update keeps the consumer's position, so every message published on
// that topic in the window would be skipped for good. The four writes - a registration's confirm,
// a delete's restore, an unsubscribe's refollow and a relay restart - are fenced on the row they
// were built from (fenceFilterWrite), and each case below holds one of them open over a
// registration on another listener and then publishes on the topic that registration added.

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// consumerFilterFenceWait bounds the calls the cases below make themselves: the delete whose
// restore is held open over a registration, and the relay restart one of them drives.
const consumerFilterFenceWait = 10 * time.Second

// A registration's confirm keeps the topic a registration on another listener added while the
// confirm was between its read of the bucket and its consumer call (confirmRegistered,
// repairRegistered).
//
// The order is built rather than waited for. The registration on the other listener sets the
// consumer's filter on its own topics first and then stores its row, and the hooks hold this
// listener's confirm so that its read of the bucket falls between the two: the row it reads is
// the one that registration replaces. The other listener is then let go, and the confirm's write
// is made only once its registration has been answered 200 - its row, its entry and its own
// confirm all behind it - so what that write carries is the older row's filter.
func TestAConfirmKeepsTheTopicARegistrationOnAnotherListenerAddedUnderItsRead(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_confirm_fence"
	const kept = "notifications.dispatch.issue.CONFIRMFENCEKEPT.>"
	const added = "notifications.dispatch.issue.CONFIRMFENCEADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	filtered, stored := make(chan struct{}), make(chan struct{})
	var answered <-chan int
	var onceFilter, onceBefore, onceAfter sync.Once
	b.deps.consumers.afterRegisterFilter = func() {
		onceFilter.Do(func() {
			close(filtered)
			<-stored
		})
	}
	a.deps.consumers.beforeRepairPlan = func() {
		onceBefore.Do(func() {
			answered = postAsync(b, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1", kept, added))
			<-filtered
		})
	}
	a.deps.consumers.afterRepairPlan = func() {
		onceAfter.Do(func() {
			close(stored)
			if status := <-answered; status != http.StatusOK {
				t.Errorf("the registration on the other listener answered %d, want 200", status)
			}
		})
	}
	// The heartbeat whose confirm carries the older row: it follows the topic the session
	// followed before the registration above added one.
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))
	a.deps.consumers.beforeRepairPlan, a.deps.consumers.afterRepairPlan = nil, nil
	b.deps.consumers.afterRegisterFilter = nil

	requireConsumerFilters(t, a, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.CONFIRMFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A delete's restore keeps the topic a registration added while the restore was between its read
// of the bucket and its consumer call. The delete is made on a second listener, as a reaper pass
// or a clean stop on another task makes it: it deletes the record first and the consumer after
// it, and the restore gives the consumer back to the session whose registration landed in between
// (deleteConsumer, restoreRegistered). The hook holds that restore between its read and its write
// while a registration adds a topic, stores its row and creates the consumer filtered on it.
func TestARestoreKeepsTheTopicARegistrationAddedUnderItsRead(t *testing.T) {
	registering := newSessionConsumerRig(t)
	deleting := newSessionConsumerRig(t)
	const sessionID = "ses_restore_fence"
	const kept = "notifications.dispatch.issue.RESTOREFENCEKEPT.>"
	const added = "notifications.dispatch.issue.RESTOREFENCEADD.>"
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait*3)
	defer cancel()
	// The record delete every delete of a consumer follows, and the registration that lands
	// between it and the consumer delete, which is what the restore gives the consumer back for.
	if err := deleting.deps.sessions.Delete(ctx, sessionID); err != nil {
		t.Fatalf("delete the record of %s: %v", sessionID, err)
	}
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-2", kept))

	var once sync.Once
	deleting.deps.consumers.afterRepairPlan = func() {
		once.Do(func() {
			registering.subscribe(t, consumerRegistration(sessionID, "epoch-2", kept, added))
		})
	}
	if err := deleting.deps.consumers.deleteConsumer(ctx, deleting.deps, sessionID); err != nil {
		t.Fatalf("the delete of %s's consumer: %v", sessionID, err)
	}
	deleting.deps.consumers.afterRepairPlan = nil

	requireConsumerFilters(t, registering, sessionID, kept, added)
	registering.store(t, "notifications.dispatch.issue.RESTOREFENCEADD.comment")
	registering.awaitPending(t, sessionID, 1)
}

// An unsubscribe's refollow keeps the topic a registration on another listener added while the
// refollow waited for the session's lock. The unsubscribe removes its own topic from the row and
// then waits at that lock, which the case holds; the other listener's registration takes no lock
// of this process, so it stores its row and sets the consumer's filter on it while the refollow
// waits. The refollow then reads the bucket rather than this listener's cache, which the watch
// stopped below stands for one that has not delivered that row yet, and its write is fenced on
// the row it read.
func TestARefollowKeepsTheTopicARegistrationOnAnotherListenerAddedUnderItsRead(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_refollow_fence"
	const kept = "notifications.dispatch.issue.REFOLLOWFENCEKEPT.>"
	const dropped = "notifications.dispatch.issue.REFOLLOWFENCEDROP.>"
	const added = "notifications.dispatch.issue.REFOLLOWFENCEADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	unlock := a.deps.consumers.locks.lock(sessionID)
	answered := postAsync(a, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	awaitLockUsers(t, &a.deps.consumers.locks, sessionID, 2)
	// From here this listener's watch delivers nothing: its cache holds the row its own
	// unsubscribe wrote and no later one.
	a.harness.registry.StopWatch()

	answer := b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration on the other listener answered delivery %+v, want mode %s", answer.Delivery, deliveryModeConsumer)
	}
	requireConsumerFilters(t, b, sessionID, kept, added)
	row, _, err := b.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(row.Topics, added) {
		t.Fatalf("the bucket's row is %+v (%v), want %s among its topics: the registration above was answered 200 for it", row, err, added)
	}

	unlock()
	if status := <-answered; status != http.StatusOK {
		t.Fatalf("the unsubscribe answered %d, want 200", status)
	}
	requireConsumerFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.REFOLLOWFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A relay restart keeps the topic a registration on another listener added before the restart
// read the session's topics. The restart rebuilds the filter from the stored row and recreates
// the consumer on it (sessionRelays.restart, storedFilter), so it reads the bucket rather than
// this listener's cache, which the watch stopped below stands for one that has not delivered that
// row yet, and its write is fenced on the row it read (settleFilter).
func TestARelayRestartKeepsTheTopicARegistrationOnAnotherListenerAddedUnderItsRead(t *testing.T) {
	a := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_restart_fence"
	const kept = "notifications.dispatch.issue.RESTARTFENCEKEPT.>"
	const added = "notifications.dispatch.issue.RESTARTFENCEADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))
	a.harness.registry.StopWatch()

	answer := b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration on the other listener answered delivery %+v, want mode %s", answer.Delivery, deliveryModeConsumer)
	}
	requireConsumerFilters(t, b, sessionID, kept, added)

	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	_, pull, outcome := a.relays.restart(ctx, &relayRun{sessionID: sessionID})
	if pull == nil {
		t.Fatalf("the restart of %s pulled nothing, outcome %v", sessionID, outcome)
	}
	pull.Stop()

	requireConsumerFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.RESTARTFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// requireConsumerFilters fails the test unless sessionID's consumer filters on its agent subject
// and topics, and nothing else.
func requireConsumerFilters(t *testing.T, rig *sessionConsumerRig, sessionID string, topics ...string) {
	t.Helper()
	want := append([]string{contracts.AgentSubject(sessionID)}, topics...)
	slices.Sort(want)
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("the consumer of %s filters on %v, want %v", sessionID, got, want)
	}
}
