package main

// Every write of a session consumer's filter is built from a read of the interest bucket and
// lands at the server after it. A registration on another listener that stores its row in between
// takes no lock of this process, so the bucket is what orders the two: a write built from the row
// that registration replaced would leave the session answered 200 for a topic its consumer does
// not filter on, at the cost fenceFilterWrite's doc states. The five writes - a registration's
// confirm, a delete's restore, an unsubscribe's refollow, a registration this listener did not
// answer (registerSession's deferred settle, whose cases are
// session_consumer_unanswered_registration_test.go's) and a relay restart - are fenced on the row
// they were built from (fenceFilterWrite), and each case below holds one of them open over a
// registration on another listener and then publishes on the topic that registration added.
//
// Two defences stand in that window, and a case pins one of them at a time. A row stored before
// a write's read is kept by that read being of the bucket and never of this listener's cache,
// which the watch stopped in such a case stands for one that has not delivered the row yet; the
// fence finds nothing moved and corrects nothing. A row stored after that read is kept by the
// fence alone: the read answered the row that registration replaced, so an older row's filter is
// what lands, and only a pass planned from the newest row takes the consumer back. The refollow
// and the relay restart have a case of each order, and a third whose fence can read the bucket
// no further: a fence that reads it corrects a write built from the cache, so only a case
// without one pins that write's own read to the bucket.
//
// The last two cases hold a confirm open over such a registration in every pass rather than in
// one: the fence writes again from the newest row until the row stops moving, and a row that
// never stops outruns its passes, which it reports at WARN and leaves to the session's next
// heartbeat.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
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

	requireFilters(t, a, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.CONFIRMFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A delete's restore keeps the topic a registration added while the restore was between its read
// of the bucket and its consumer call. The delete is made on a second listener, as a reaper pass
// or a clean stop on another task makes it: it deletes the record first and the consumer after
// it, and the restore gives the consumer back to the session whose registration landed in between
// (deleteConsumer, repairRegistered). The hook holds that restore between its read and its write
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

	requireFilters(t, registering, sessionID, kept, added)
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
	requireFilters(t, b, sessionID, kept, added)
	row, _, err := b.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(row.Topics, added) {
		t.Fatalf("the bucket's row is %+v (%v), want %s among its topics: the registration above was answered 200 for it", row, err, added)
	}

	unlock()
	if status := <-answered; status != http.StatusOK {
		t.Fatalf("the unsubscribe answered %d, want 200", status)
	}
	requireFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.REFOLLOWFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// An unsubscribe's refollow keeps that topic even where the fence behind it can read the bucket
// no further: what keeps it is the refollow's own read, which is of the bucket and never of this
// listener's cache (storedTopics), so the filter it writes is the newest row's when it is made
// and not one a corrective pass made right.
//
// The case above cannot say which of the two kept it. A refollow built from the cache writes the
// filter of the row its own unsubscribe left and names that row's revision, and the fence
// reading the bucket after the write finds the row moved and corrects both, leaving the same
// consumer behind. So this case builds that same order - the unsubscribe held at the session's
// lock, this listener's watch stopped, the registration on the other listener answered in that
// window - and closes the connection this listener's registry reads the bucket through once the
// refollow has read it. The fence then judges nothing and says so, and what stands on the
// consumer, and what that line names as the revision written from, are the refollow's own read.
func TestARefollowKeepsTheTopicStoredBeforeItsReadWhereTheFenceReadsNoFurther(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_refollow_unjudged"
	const kept = "notifications.dispatch.issue.REFOLLOWUNJUDGEDKEPT.>"
	const dropped = "notifications.dispatch.issue.REFOLLOWUNJUDGEDDROP.>"
	const added = "notifications.dispatch.issue.REFOLLOWUNJUDGEDADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	unlock := a.deps.consumers.locks.lock(sessionID)
	answered := postAsync(a, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	awaitLockUsers(t, &a.deps.consumers.locks, sessionID, 2)
	// From here this listener's watch delivers nothing: its cache holds the row its own
	// unsubscribe wrote and no later one.
	a.harness.registry.StopWatch()

	b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
	requireFilters(t, b, sessionID, kept, added)
	row, newest, err := b.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(row.Topics, added) {
		t.Fatalf("the bucket's row is %+v (%v), want %s among its topics: the registration above was answered 200 for it", row, err, added)
	}

	var once sync.Once
	a.deps.consumers.afterStoredRowRead = func() {
		// The connection this listener's registry reads the bucket through, closed between the
		// refollow's read and the write built from it and no part of that write, which goes over
		// the session-consumer connection: the fence's read is then the one that cannot be made
		// (store.Registry.RefreshAt), so no pass of its own follows this one.
		once.Do(a.harness.client.Close)
	}
	unlock()
	if status := <-answered; status != http.StatusOK {
		t.Fatalf("the unsubscribe answered %d, want 200", status)
	}
	a.deps.consumers.afterStoredRowRead = nil

	wrote := requireFenceReadFailureLogged(t, a, sessionID)
	requireKeptTheBucketsRow(t, b, sessionID, "refollow", wrote, newest,
		"notifications.dispatch.issue.REFOLLOWUNJUDGEDADD.comment", kept, added)
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
	requireFilters(t, b, sessionID, kept, added)

	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	_, pull, outcome := a.relays.restart(ctx, &relayRun{sessionID: sessionID})
	if pull == nil {
		t.Fatalf("the restart of %s pulled nothing, outcome %v", sessionID, outcome)
	}
	pull.Stop()

	requireFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.RESTARTFENCEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A relay restart keeps that topic where the fence behind it can read the bucket no further
// too, for the reason the refollow above keeps it: the filter the restart creates the consumer
// on is built from the bucket (storedFilter), and the settle after it is a fence that corrects a
// filter built from this listener's cache (settleFilter), so a case whose fence can read the
// bucket is judging that fence rather than the restart's read. The connection the registry reads
// through is closed once the restart has read the row, and the restart's own call is then the
// only write of this loop's the consumer carries.
func TestARelayRestartKeepsTheTopicStoredBeforeItsReadWhereTheFenceReadsNoFurther(t *testing.T) {
	a := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_restart_unjudged"
	const kept = "notifications.dispatch.issue.RESTARTUNJUDGEDKEPT.>"
	const added = "notifications.dispatch.issue.RESTARTUNJUDGEDADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))
	a.harness.registry.StopWatch()

	b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
	requireFilters(t, b, sessionID, kept, added)
	row, newest, err := b.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(row.Topics, added) {
		t.Fatalf("the bucket's row is %+v (%v), want %s among its topics: the registration above was answered 200 for it", row, err, added)
	}

	var once sync.Once
	a.deps.consumers.afterStoredRowRead = func() {
		// The registry's connection, closed between the restart's read of the row and the
		// consumer call built from it, which goes over the session-consumer connection.
		once.Do(a.harness.client.Close)
	}
	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	_, pull, outcome := a.relays.restart(ctx, &relayRun{sessionID: sessionID})
	a.deps.consumers.afterStoredRowRead = nil
	if pull == nil {
		t.Fatalf("the restart of %s pulled nothing, outcome %v", sessionID, outcome)
	}
	pull.Stop()

	wrote := requireFenceReadFailureLogged(t, a.sessionConsumerRig, sessionID)
	requireKeptTheBucketsRow(t, b, sessionID, "restart", wrote, newest,
		"notifications.dispatch.issue.RESTARTUNJUDGEDADD.comment", kept, added)
}

// An unsubscribe's refollow keeps the topic a registration on another listener stored under its
// write. The hook below stores that row and sets the consumer's filter on it after the refollow
// has read the bucket and before its own consumer call, which is where a registration answered
// on another listener lands: the refollow's write then goes over that filter carrying the row it
// replaced, and nothing outside the fence reads the consumer again, so the pass planned from the
// newest row is the whole of the repair (fenceFilterWrite). This listener's watch is left
// running: the row the fence corrects to is the bucket's, whatever the cache holds.
func TestARefollowKeepsTheTopicARegistrationOnAnotherListenerStoredUnderItsWrite(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_refollow_fence_write"
	const kept = "notifications.dispatch.issue.REFOLLOWWRITEKEPT.>"
	const dropped = "notifications.dispatch.issue.REFOLLOWWRITEDROP.>"
	const added = "notifications.dispatch.issue.REFOLLOWWRITEADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	var once sync.Once
	a.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() { b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added)) })
	}
	a.post(t, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	a.deps.consumers.afterStoredRowRead = nil

	requireFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.REFOLLOWWRITEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A relay restart keeps the topic a registration on another listener stored under its write. The
// restart's own call is the fence's first pass, made before the fence runs, so the hook below
// stores that row between the restart's read of the session's topics and that call: what the
// restart leaves on the consumer is the row it replaced, and the pass after it is what sets the
// filter the newest row asks for (sessionRelays.settleFilter).
func TestARelayRestartKeepsTheTopicARegistrationOnAnotherListenerStoredUnderItsWrite(t *testing.T) {
	a := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_restart_fence_write"
	const kept = "notifications.dispatch.issue.RESTARTWRITEKEPT.>"
	const added = "notifications.dispatch.issue.RESTARTWRITEADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	var once sync.Once
	a.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() { b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added)) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	_, pull, outcome := a.relays.restart(ctx, &relayRun{sessionID: sessionID})
	a.deps.consumers.afterStoredRowRead = nil
	if pull == nil {
		t.Fatalf("the restart of %s pulled nothing, outcome %v", sessionID, outcome)
	}
	pull.Stop()

	requireFilters(t, b, sessionID, kept, added)
	b.store(t, "notifications.dispatch.issue.RESTARTWRITEADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A consumer call whose answer did not come back is judged by the fence as a write that may
// have landed: the server applies such a call and only its answer is lost, so an older row's
// filter can stand on the consumer with nothing to read it again (consumerCallAnswered). The
// repair below is held open over a write of the session's row that sets no filter - a role
// claim's - and its consumer call is faulted with the deadline a lost answer reads as. The loop
// reads the bucket, finds the row moved under that call, and makes the consumer what the newest
// row asks for; a loop that took the failure for "nothing landed" would leave the session with
// no consumer at all.
func TestARepairWhoseConsumerCallAnsweredNothingTriesAgainOverTheRowThatMoved(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_lost_answer_fence"
	const kept = "notifications.dispatch.issue.LOSTANSWERKEPT.>"
	const added = "notifications.dispatch.issue.LOSTANSWERADD.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	var faulted sync.Once
	rig.deps.consumers.consumers.SetCreateFault(func(string) error {
		var answer error
		faulted.Do(func() { answer = context.DeadlineExceeded })
		return answer
	})
	t.Cleanup(func() { rig.deps.consumers.consumers.SetCreateFault(nil) })

	// Nothing of this case's own stands between the two passes: the faulted call of the first
	// pass is what sends the fence round again.
	requireRestoreMakesACorrectivePass(t, rig, sessionID, added, nil)
	rig.deps.consumers.consumers.SetCreateFault(nil)

	requireFilters(t, rig, sessionID, kept, added)
	rig.store(t, "notifications.dispatch.issue.LOSTANSWERADD.comment")
	rig.awaitPending(t, sessionID, 1)
}

// A fence whose row stops moving ends on the newest row, however many of its passes the
// registrations on another listener outran. The hook below stores a row and sets the consumer's
// filter from it in every pass but the last, as a registration answered on another listener
// does, so each of this listener's writes carries the row the pass before it read; the last pass
// plans from a row nothing replaces, writes it, and reads the same revision back, which is where
// the loop ends (fenceFilterWrite, filterFenceAttempts).
func TestAFenceWhoseRowStopsMovingEndsOnTheNewestRow(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_fence_converges"
	const kept = "notifications.dispatch.issue.FENCEPASSKEPT.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	topics, moves := []string{kept}, 0
	a.deps.consumers.afterRepairPlan = func() {
		if moves == filterFenceAttempts-1 {
			return
		}
		moves++
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.FENCEPASS%d.>", moves))
		b.subscribe(t, consumerRegistration(sessionID, "epoch-1", topics...))
	}
	// The heartbeat whose confirm those registrations outrun, pass after pass.
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))
	a.deps.consumers.afterRepairPlan = nil

	if moves != filterFenceAttempts-1 {
		t.Fatalf("the row moved under %d of the repair's passes, want %d, one short of the fence's own", moves, filterFenceAttempts-1)
	}
	requireFilters(t, a, sessionID, topics...)
	if logs := a.harness.logs.String(); strings.Contains(logs, fenceExhaustionLine) {
		t.Errorf("a fence that ended on the newest row must report nothing left behind:\n%s", logs)
	}
	b.store(t, "notifications.dispatch.issue.FENCEPASS"+strconv.Itoa(moves)+".comment")
	b.awaitPending(t, sessionID, 1)
}

// A fence out of passes names the session and both revisions at WARN, and the session's next
// heartbeat is what sets the filter it left. The hook below stores a row and sets the consumer's
// filter from it in every pass, so every write this listener makes is outrun and the loop ends
// holding the row of its last plan, one row behind the bucket: that window is what the line
// reports, and what the heartbeat after it closes (fenceFilterWrite).
func TestAFenceOutOfPassesNamesBothRevisionsAndLeavesTheFilterToTheNextHeartbeat(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_fence_exhausted"
	const kept = "notifications.dispatch.issue.FENCEOUTKEPT.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	topics, passes := []string{kept}, 0
	a.deps.consumers.afterRepairPlan = func() {
		passes++
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.FENCEOUT%d.>", passes))
		b.subscribe(t, consumerRegistration(sessionID, "epoch-1", topics...))
	}
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))
	a.deps.consumers.afterRepairPlan = nil

	if passes != filterFenceAttempts {
		t.Fatalf("the repair made %d passes, want the %d the fence allows", passes, filterFenceAttempts)
	}
	// The row of the last pass's plan, which is the one before the row that outran its write.
	requireFilters(t, a, sessionID, topics[:len(topics)-1]...)
	wrote, newest := requireFenceExhaustionLogged(t, a, sessionID)
	if newest <= wrote {
		t.Errorf("the line names revision %d written and %d newest, want a newest above it", wrote, newest)
	}

	// The next heartbeat, which writes the session's row and the filter of it.
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", topics...))
	requireFilters(t, a, sessionID, topics...)
	b.store(t, "notifications.dispatch.issue.FENCEOUT"+strconv.Itoa(passes)+".comment")
	b.awaitPending(t, sessionID, 1)
}

// A corrective pass that finds the consumer already filtering on what the newest row asks ends
// the fence with nothing left behind to report: no write of this loop's stands over a newer
// registration's filter, so there is no window to name (fenceFilterWrite, filterWriteSettled).
//
// The order is built rather than waited for. The restore's first pass writes the row it read,
// and the drive below (requireRestoreMakesACorrectivePass) stores a newer one under that write
// - a role claim's write, which sets no filter - so the fence reads the bucket again. The pass
// after it plans from that newest row, and the hook this case hands that drive lets a
// registration on the other listener set the consumer's filter to that same row first, so this
// pass's own call sends nothing (bus.SessionConsumers.Ensure). A fence reading "sent nothing"
// as a write that did not land names a window the consumer is not in: it filters on the newest
// row, which is what the line it would log says it does not.
func TestACorrectivePassThatFindsTheConsumerAsTheRowAsksReportsNothingLeftBehind(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_fence_already"
	const kept = "notifications.dispatch.issue.FENCEALREADYKEPT.>"
	const added = "notifications.dispatch.issue.FENCEALREADYADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	requireRestoreMakesACorrectivePass(t, a, sessionID, added, func() {
		// The registration on the other listener that sets the consumer's filter to the newest
		// row, before the corrective pass plans from that same row.
		b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
	})

	requireFilters(t, a, sessionID, kept, added)
	if logs := a.harness.logs.String(); strings.Contains(logs, fenceExhaustionLine) {
		t.Errorf("a fence whose corrective pass found the consumer as the newest row asks must report nothing left behind:\n%s", logs)
	}
	b.store(t, "notifications.dispatch.issue.FENCEALREADYADD.comment")
	b.awaitPending(t, sessionID, 1)
}

// A corrective pass that finds the session's record deleted promises no heartbeat: a session
// the bucket holds no record of has none to make, and the delete that took that record takes
// its consumer, so the fence's line - which leaves the filter to the next heartbeat - names a
// repair nothing will do. What the fence reports instead is what the pass found: a filter of
// its own write standing on the consumer of a session that is gone (repairRegistered,
// filterWriteSessionGone).
//
// The order is built rather than waited for. The restore's first pass writes the row it read
// and creates the consumer the delete took, and the drive below
// (requireRestoreMakesACorrectivePass) stores a newer row under that write - a role claim's
// write, which sets no filter - so the fence reads the bucket again. The hook this case hands
// that drive then deletes the session's record before the corrective pass plans, as a reaper
// pass or a clean stop deletes it: neither takes this listener's consumer lock, so both reach
// the bucket while the restore holds it (session.SessionReaper.DeleteConsumer,
// deleteSessionConsumer).
func TestACorrectivePassThatFindsTheRecordDeletedPromisesNoHeartbeat(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_fence_record_gone"
	const kept = "notifications.dispatch.issue.FENCERECGONEKEPT.>"
	const added = "notifications.dispatch.issue.FENCERECGONEADD.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	requireRestoreMakesACorrectivePass(t, rig, sessionID, added, func() {
		// The record delete of a reaper pass or a clean stop, landing between the restore's
		// write and the corrective pass over the row stored under it.
		if err := rig.deps.sessions.Delete(context.Background(), sessionID); err != nil {
			t.Errorf("delete the record of %s under its own repair: %v", sessionID, err)
		}
	})

	if _, err := rig.deps.sessions.Refresh(context.Background(), sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the bucket answers %v for %s's record, want no record at all: the corrective pass must find it deleted", err, sessionID)
	}
	if logs := rig.harness.logs.String(); strings.Contains(logs, fenceExhaustionLine) {
		t.Errorf("the repair of a session whose record is deleted left the filter to the session's next heartbeat; that session has none, and its consumer is being deleted:\n%s", logs)
	}

	// What the pass reports instead, and the consumer it reports it of: the restore created it
	// again before the record was deleted, and it stands filtered on the row that write was
	// built from, for a session the bucket holds no record of.
	if wrote := requireRepairRecordGoneLogged(t, rig, sessionID); wrote == 0 {
		t.Errorf("the line names revision 0 as the one written from, want the revision of the row the restore's own write was built from")
	}
	requireFilters(t, rig, sessionID, kept)
}

// A corrective pass that plans nothing names the window the pass before it left: that pass's
// filter stands on the consumer, and the bucket has already answered a newer row than the one
// it was built from, which is the window a loop out of passes leaves too (fenceFilterWrite).
// The other half of that branch is a pass that plans nothing with no write of this loop's
// behind it, where the fence ends silently: nothing of the loop stands anywhere to report.
//
// The order is built rather than waited for. The restore's first pass creates the consumer the
// delete took, filtered on the row it read, and the drive below
// (requireRestoreMakesACorrectivePass) stores a newer row under that write - a role claim's
// write, which sets no filter - so the fence reads the bucket again. The hook this case hands
// that drive then closes the connection this listener reads the record and the row through,
// before the corrective pass plans: neither read answers, so that pass plans nothing, sends
// nothing and leaves the first pass's filter standing (registeredTarget, registeredReadFailed).
func TestACorrectivePassThatPlansNothingNamesTheWindowTheWriteBeforeItLeft(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_fence_plans_nothing"
	const kept = "notifications.dispatch.issue.FENCEPLANSNOTHINGKEPT.>"
	const added = "notifications.dispatch.issue.FENCEPLANSNOTHINGADD.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept))

	registered, movedTo := requireRestoreMakesACorrectivePass(t, rig, sessionID, added, func() {
		// The connection this listener reads the record and the row through, closed before the
		// corrective pass plans and no part of the consumer calls, which go over the
		// session-consumer connection (session.SessionRegistry.Refresh, store.Registry.RefreshAt).
		rig.harness.client.Close()
	})

	wrote, newest := requireFenceExhaustionLogged(t, rig, sessionID)
	if wrote != registered || newest != movedTo {
		t.Errorf("the line names revision %d written and %d newest, want the %d the first pass read and the %d stored under its write", wrote, newest, registered, movedTo)
	}
	// The window itself: the consumer filters on the topics of the row the first pass read, and
	// the row the bucket holds names one more.
	requireFilters(t, rig, sessionID, kept)
}

// A corrective pass that finds the stream holding no consumer ends the fence with nothing left
// behind to report: the filter an earlier pass of this loop wrote went with the consumer the
// delete took, so no older row's filter stands anywhere, and the window the fence's line names
// - a session answered 200 for a topic its consumer does not filter on - is one no consumer
// can hold open (storedFilterStep, filterWriteSettled).
//
// The order is built rather than waited for. The refollow's first pass writes the row it read,
// and the first hook stores a newer one under that write - a role claim's write, which sets no
// filter - so the fence reads the bucket again. The second hook then deletes the consumer
// before the corrective pass's own call, as a reaper pass or a clean stop deletes it: neither
// takes this listener's consumer lock, so both reach the server while the refollow holds it
// (session.SessionReaper.DeleteConsumer, deleteSessionConsumer).
func TestACorrectivePassThatFindsNoConsumerReportsNothingLeftBehind(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_fence_consumer_gone"
	const kept = "notifications.dispatch.issue.FENCECONSGONEKEPT.>"
	const dropped = "notifications.dispatch.issue.FENCECONSGONEDROP.>"
	const added = "notifications.dispatch.issue.FENCECONSGONEADD.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	passes := 0
	rig.deps.consumers.afterStoredRowRead = func() {
		passes++
		switch passes {
		case 1:
			// The row of a write that sets no filter - a role claim's - stored under the
			// refollow's own write, which is what sends the fence round again.
			if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{added}); err != nil {
				t.Errorf("store %s on the session's row: %v", added, err)
			}
		case 2:
			// The delete of the consumer, landing between the corrective pass's plan and the
			// call it makes from it.
			ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
			defer cancel()
			if err := rig.consumers.Delete(ctx, sessionID); err != nil {
				t.Errorf("delete the consumer of %s under its own refollow: %v", sessionID, err)
			}
		}
	}
	rig.post(t, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	rig.deps.consumers.afterStoredRowRead = nil

	if passes != 2 {
		t.Fatalf("the refollow made %d passes, want the write and the corrective pass over the row stored under it", passes)
	}
	if _, err := rig.consumers.Info(context.Background(), sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the stream answers %v for %s's consumer, want none at all: the corrective pass must find the consumer gone", err, sessionID)
	}
	if logs := rig.harness.logs.String(); strings.Contains(logs, fenceExhaustionLine) {
		t.Errorf("a fence whose corrective pass found no consumer must report nothing left behind: no filter of its own stands anywhere:\n%s", logs)
	}
}

// A fence whose read of the bucket after a write answers no row for the session plans no pass
// from that answer: the filter that write left stands, and the live session goes on receiving
// the topics it names (rowMovedUnder). A pass planned from no row sets the consumer's filter to
// the session's agent subject alone, and the bucket answers no row for two reasons this
// listener cannot tell apart, which cost differently. The row was deleted, as this case builds
// it: the filter standing names topics the session's plugin drops the frames of, and the delete
// of the session's record takes the consumer with it anyway (refollow, deleteConsumer). Or a
// copy behind the bucket's leader has not got the key yet, which takes a deployment of more
// than one replica (bus.KeyValue.Get): emptying the filter there drops every topic a live
// session follows until its next heartbeat, 90 s away.
//
// The hook below deletes the row between the refollow's read and its consumer call, as DELETE
// /v1/interests/<session_id> deletes it before anything deletes the consumer. The session's
// record and consumer stand throughout, which is the shape the lagging copy's answer has too,
// and is why the loop ends on an answer below the revision it wrote rather than passing again.
func TestAFenceWhoseBucketAnswersNoRowLeavesTheFilterItWroteOnALiveConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_fence_row_gone"
	const kept = "notifications.dispatch.issue.FENCEROWGONEKEPT.>"
	const dropped = "notifications.dispatch.issue.FENCEROWGONEDROP.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	var once sync.Once
	rig.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() {
			// The delete DELETE /v1/interests/<session_id> makes of the whole row
			// (store.RoleReleaseAuthority.Force), landing under the refollow's write.
			if _, err := rig.harness.registry.Remove(context.Background(), sessionID, nil, store.RoleReleaseAuthority{Force: true}); err != nil {
				t.Errorf("delete %s's interest row under the refollow's write: %v", sessionID, err)
			}
		})
	}
	rig.post(t, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	rig.deps.consumers.afterStoredRowRead = nil

	if _, _, err := rig.harness.registry.RefreshAt(context.Background(), sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the bucket answers %v for %s's row, want no row at all: the fence judged its write against a row the delete left", err, sessionID)
	}
	requireFilters(t, rig, sessionID, kept)
	rig.store(t, "notifications.dispatch.issue.FENCEROWGONEKEPT.comment")
	rig.awaitPending(t, sessionID, 1)
}

// A write that landed whose read of the bucket after it fails names the session, the revision
// that write was built from and the read's failure: the fence cannot say whether the filter it
// left is an older row's, and that line is all an operator reading the pause in the session's
// delivery has to go on (fenceFilterWrite).
//
// The order is built rather than waited for. The refollow below is held between its read of the
// bucket and its consumer call while a registration on the other listener stores a newer row and
// sets the consumer's filter on it, so what the refollow writes is the filter of the row that
// registration replaced. The connection its registry reads the bucket through is closed in that
// same window, and the write lands all the same: it goes over the session-consumer connection.
// The unsubscribe is answered 200, and the consumer is left filtering on the older row.
func TestAWriteThatLandedWhoseReadOfTheBucketFailsNamesTheSessionTheRevisionAndTheError(t *testing.T) {
	a := newSessionConsumerRig(t)
	b := newSessionConsumerRig(t)
	const sessionID = "ses_fence_read_failed"
	const kept = "notifications.dispatch.issue.FENCEREADKEPT.>"
	const dropped = "notifications.dispatch.issue.FENCEREADDROP.>"
	const added = "notifications.dispatch.issue.FENCEREADADD.>"
	a.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, dropped))

	var once sync.Once
	a.deps.consumers.afterStoredRowRead = func() {
		once.Do(func() {
			b.subscribe(t, consumerRegistration(sessionID, "epoch-1", kept, added))
			// The connection this listener's registry reads the bucket through, and no part of
			// the write below: a connection that went is one of the reads the fence cannot make
			// after a write that landed (store.Registry.RefreshAt).
			a.harness.client.Close()
		})
	}
	a.post(t, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["`+dropped+`"]}`)
	a.deps.consumers.afterStoredRowRead = nil

	// What that write left: the filter of the row the registration above replaced, which is the
	// window the line reports and the fence could not judge.
	requireFilters(t, b, sessionID, kept)
	wrote := requireFenceReadFailureLogged(t, a, sessionID)
	row, newest, err := b.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(row.Topics, added) {
		t.Fatalf("the bucket's row is %+v (%v), want %s among its topics: the registration above was answered 200 for it", row, err, added)
	}
	if wrote == 0 || wrote >= newest {
		t.Errorf("the line names revision %d as the one written from, want one above 0 and below the %d the bucket holds now", wrote, newest)
	}
}

// fenceExhaustionLine is what a fenced filter write logs when it ends on a filter older than the
// row the bucket holds (fenceFilterWrite).
const fenceExhaustionLine = "session consumer filter left on an older interest row than the bucket holds; the session's next heartbeat sets it"

// requireFenceExhaustionLogged fails the test unless the listener logged that line for sessionID,
// and answers the two revisions it names: the one the filter standing was built from, and the one
// the bucket answered after it.
func requireFenceExhaustionLogged(t *testing.T, rig *sessionConsumerRig, sessionID string) (uint64, uint64) {
	t.Helper()
	reported := requireFilterLineLogged(t, rig, sessionID, fenceExhaustionLine,
		"the fence left "+sessionID+" on an older row")
	return reported.Wrote, reported.Newest
}

// fenceReadFailureLine is what a fenced filter write logs when the read of the bucket after a
// write that landed failed, so the fence cannot say whether the filter it left is an older row's
// (fenceFilterWrite).
const fenceReadFailureLine = "session consumer filter written and left unjudged: the interest row could not be read again; the session's next heartbeat sets it"

// requireFenceReadFailureLogged fails the test unless the listener logged that line for
// sessionID naming the failure of the read that judged nothing, and answers the other thing it
// names: the revision the filter standing was built from. The failure is required here rather
// than answered to each caller, since a line that names none is a fence that judged nothing for
// another reason, whose corrective pass a case built on this line cannot see.
func requireFenceReadFailureLogged(t *testing.T, rig *sessionConsumerRig, sessionID string) uint64 {
	t.Helper()
	reported := requireFilterLineLogged(t, rig, sessionID, fenceReadFailureLine,
		"the fence could not read the bucket after "+sessionID+"'s write")
	if reported.Error == "" {
		t.Errorf("the line names no failure, and the read's failure is what says why the fence judged nothing")
	}
	return reported.Wrote
}

// repairRecordGoneLine is what the fence of a repair of a session's consumer logs when a pass
// of it finds the bucket holding no record of that session and a write of an earlier pass
// stands on the consumer (fenceFilterWrite, filterWriteSessionGone).
const repairRecordGoneLine = "listener left the filter of an older interest row on the consumer of a session the bucket holds no record for; the session is gone, so no heartbeat sets that filter and the consumer goes with the delete that took the record, or with the stream's inactive threshold"

// requireRepairRecordGoneLogged fails the test unless the listener logged that line for
// sessionID, and answers the revision it names: the row the filter standing was built from.
func requireRepairRecordGoneLogged(t *testing.T, rig *sessionConsumerRig, sessionID string) uint64 {
	t.Helper()
	return requireFilterLineLogged(t, rig, sessionID, repairRecordGoneLine,
		"the repair of "+sessionID+" found the bucket holding no record of it").Wrote
}

// loggedFilterLine is what one of the lines above names: the session whose consumer it is
// about, the revision the filter standing on that consumer was built from, the revision the
// bucket answered after that write, and the failure of a read that judged nothing. Each line
// names the fields of its own case and leaves the rest zero, which is what its require reads.
type loggedFilterLine struct {
	SessionID string `json:"session_id"`
	Wrote     uint64 `json:"wrote_revision"`
	Newest    uint64 `json:"newest_revision"`
	Error     string `json:"error"`
}

// requireFilterLineLogged answers the fields of the line msg the listener logged for sessionID,
// and fails the test unless it logged one; did names what the listener did and logged nothing
// of, which is that failure's own sentence.
func requireFilterLineLogged(t *testing.T, rig *sessionConsumerRig, sessionID, msg, did string) loggedFilterLine {
	t.Helper()
	logs := rig.harness.logs.String()
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		var reported loggedFilterLine
		if err := json.Unmarshal([]byte(line), &reported); err != nil {
			t.Fatalf("decode the listener's line %s: %v", line, err)
		}
		if reported.SessionID == sessionID {
			return reported
		}
	}
	t.Fatalf("%s and logged nothing:\n%s", did, logs)
	return loggedFilterLine{}
}

// requireKeptTheBucketsRow fails the test unless the write writer names was built from the
// revision the bucket answered when it read, and the consumer it left filters on topics and is
// offered a message stored on message. The three cases whose fence reads the bucket no further
// end this way: with no corrective pass behind it, what the consumer carries is that write's
// own read, which is of the bucket and never of this listener's cache (storedTopics).
func requireKeptTheBucketsRow(t *testing.T, rig *sessionConsumerRig, sessionID, writer string, wrote, newest uint64, message string, topics ...string) {
	t.Helper()
	if wrote != newest {
		t.Errorf("the %s wrote from revision %d, want the %d the bucket held when it read: the fence behind it read no further, so what stands is that write's own read, and a write built from this listener's cache names the row that cache holds", writer, wrote, newest)
	}
	requireFilters(t, rig, sessionID, topics...)
	rig.store(t, message)
	rig.awaitPending(t, sessionID, 1)
}

// requireRestoreMakesACorrectivePass drives the restore of sessionID's consumer that each
// corrective-pass case above is built on, and fails the test unless the fence made the two
// passes those cases read: the write planned from the row the bucket holds, and the corrective
// pass over the row stored under it. A restore that made one pass found nothing moved, and the
// case's own assertions would then be of a window no pass of this loop opened.
//
// The order is built rather than waited for. added is stored on the session's row from inside
// the first pass, as a write that sets no filter - a role claim's - lands under the restore's
// own write, which is what sends the fence round again. underTheCorrectivePass runs before the
// second pass plans, and is where each case puts what it is about; it is nil for a case whose
// second pass the consumer call of the first brings about on its own. Both hooks are off before
// this answers, so nothing a case asserts afterwards drives another pass.
//
// It answers the revision the row held when the restore started, which the first pass's write
// is built from, and the revision the row moved to under that write.
func requireRestoreMakesACorrectivePass(t *testing.T, rig *sessionConsumerRig, sessionID, added string, underTheCorrectivePass func()) (uint64, uint64) {
	t.Helper()
	_, registered, err := rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the revision of %s's row: %v", sessionID, err)
	}

	var moved sync.Once
	var movedTo uint64
	rig.deps.consumers.afterRepairPlan = func() {
		// The row of a write that sets no filter - a role claim's - stored under the restore's
		// own write, which is what sends the fence round again.
		moved.Do(func() {
			if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{added}); err != nil {
				t.Errorf("store %s on the session's row: %v", added, err)
			}
			if _, movedTo, err = rig.harness.registry.RefreshAt(context.Background(), sessionID); err != nil {
				t.Errorf("read the revision the row moved to: %v", err)
			}
		})
	}
	passes := 0
	rig.deps.consumers.beforeRepairPlan = func() {
		passes++
		if passes != 2 || underTheCorrectivePass == nil {
			return
		}
		underTheCorrectivePass()
	}

	// The delete the session reaper or a clean stop makes, whose restore gives the consumer back
	// to a session the bucket still holds a record for (deleteConsumer, repairRegistered).
	ctx, cancel := context.WithTimeout(context.Background(), consumerFilterFenceWait)
	defer cancel()
	if err := rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID); err != nil {
		t.Fatalf("the delete of %s's consumer: %v", sessionID, err)
	}
	rig.deps.consumers.beforeRepairPlan, rig.deps.consumers.afterRepairPlan = nil, nil

	if passes != 2 {
		t.Fatalf("the restore made %d passes, want the write and the corrective pass over the row stored under it", passes)
	}
	return registered, movedTo
}
