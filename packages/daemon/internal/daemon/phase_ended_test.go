package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// endedByTheWait is the phase-ended message the admission pass queues when it stops a parent's
// tester in the middle of testing because a Dispatch child of it is open (workflow's
// StopForChildren, dispatch://INFRA-3320 R10).
var endedByTheWait = record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.AwaitingChildren,
	Reason: "the admission pass stopped the tester's testing phase, while these Dispatch children of LEGION-208 were open: LEGION-209 (todo)"}

// A released role's woken session claims its role only after the row's next attempt has
// published, so that attempt meets a role no live session holds: the row publishes with keep, the
// listener keeps the message for the role's next holder, which the woken session is, and the row
// ends with the listener as the message's keeper, the pod woken once. Published with keep: false,
// the message would meet the no-holder answer and the row would end with it kept nowhere, and the
// stopped worker would go on with the phase the wait ended.
func TestAPhaseEndedMessageForAReleasedPodIsKeptForItsWokenSession(t *testing.T) {
	keptAcrossTheWake(t, endedByTheWait, claim.RoleTester, endedByTheWait.Message("LEGION-208"))
}

// A phase-ended message to a released role waits out the resume the wake pacing parks, as a hook
// prompt does (TestAHookPromptWaitsOutTheParkedWakeOfItsReleasedPod): the row reads the claim
// again at the wake's turn or parkedWakeBound on, whichever comes first, as a routine wait, and
// publishes nothing while the park holds, and the attempt after the turn has launched the resume
// publishes the message with keep, once, and ends the row.
func TestAPhaseEndedMessageWaitsOutTheParkedWakeOfItsReleasedPod(t *testing.T) {
	keptAfterTheParkedWake(t, endedByTheWait, claim.RoleTester, endedByTheWait.Message("LEGION-208"))
}

// An operator's resume of a released role whose resume the wake pacing parked launches the claim
// at once (supervise's dropWakeFor), and the turn the row read goes with the dropped wake: the
// row, which reads the claim again at the bound rather than sleeping to that turn, publishes the
// message with keep, once, at its next attempt, and ends, as a hook prompt does
// (TestAHookPromptFollowsAnOperatorsResumeOfItsParkedWakeWithinTheBound).
func TestAPhaseEndedMessageFollowsAnOperatorsResumeOfItsParkedWakeWithinTheBound(t *testing.T) {
	keptAfterAnOperatorsResume(t, endedByTheWait, claim.RoleTester, endedByTheWait.Message("LEGION-208"))
}

// A relaunch the wake pacing parks over a process the claim still records holds no phase-ended
// row, as it holds no hook prompt (TestARelaunchParkedOverALiveProcessHoldsNoRow): the claim's live
// session can take a publish now, so the row's first attempt publishes the message to the
// stopped worker's topic and ends.
func TestAPhaseEndedMessageToARelaunchParkedOverALiveProcessPublishesAtOnce(t *testing.T) {
	ctx := context.Background()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	parkedOverItsProcess(t, sup, "LEGION-208", claim.RoleTester)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", endedByTheWait, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the row's first attempt: %v", err)
	}
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleTester))
	attempts, delivered := publisher.snapshot()
	if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic || delivered[0].message != endedByTheWait.Message("LEGION-208") {
		t.Fatalf("the first attempt made %d publishes and delivered %+v, want the message delivered to %s: the relaunch waits over a live session",
			attempts, delivered, topic)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished by its first attempt", left)
	}
}

// The three answers of the confirmed publish route are other failures, and keep the ordinary retry:
// a holder that did not confirm the message in time can take it on the next attempt, a holder that
// does not advertise the delivery mode can be replaced by another session on the role, and a
// forward the listener's own NATS grant denies goes through once an operator changes that grant.
// Only a role with no live holder ends the row.
func TestAPhaseEndedMessageRetriesEveryFailureButNoHolder(t *testing.T) {
	ctx := context.Background()
	row := mustOutboxRow(t, "LEGION-208", endedByTheWait, time.Now())
	row.ID = 61
	publisher := &outboxPublisher{}
	// The row reads the claim of the tester's step from the record (outbox.workerToken), which
	// holds no phases row of LEGION-208, so the claim is the tester's own token.
	runner := &outbox{project: "legion", pool: isolatedOutboxPool(t), records: record.NewStore(), notices: publisher, log: quietLogger()}
	const denial = `envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to "notifications.agent.ses_t"`
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "a holder that did not confirm it", err: notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses_t"}},
		{name: "a holder that does not advertise the delivery mode", err: notify.ErrRefusedByHolder{Holder: "ses_t"}},
		{name: "a forward the listener's grant denies", err: notify.ErrPublishDenied{Message: denial}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher.err = fmt.Errorf("publish notice to the tester's topic: %w", tc.err)
			err := runner.execute(ctx, row)
			if err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("the phase-ended message answered %v = %v, want the answer as a failure, retried", tc.err, err)
			}
		})
	}
	publisher.err = fmt.Errorf("publish notice to the tester's topic: %w: no holder", notify.ErrNoHolder)
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the phase-ended message no session holds = %v, want the row finished", err)
	}
}
