package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// droppedForward is the merge forward of main onto c0de, asked of the implementer, that a move of
// LEGION-208 into awaiting_children dropped before it landed (record.PhaseEnded.MergeForward).
var droppedForward = record.DroppedMergeForward{Head: "c0de", Base: "main", Role: claim.RoleImplementer}

// droppedForwardBody is the body of the role message the implementer of LEGION-208 is sent when a
// move into awaiting_children that stops no phase of its own drops droppedForward
// (dispatch://INFRA-3320 R10): the merge forward named, the begun and not-begun rule, when a merge
// forward is its to make again, and when the issue's next task comes. internal/admit's
// TestParentWaitsHoldsEveryPhaseStart asserts the same words (pwDroppedBody).
const droppedForwardBody = "LEGION-208: the issue moved into awaiting_children, where it waits on its open Dispatch children." +
	" The daemon dropped the merge forward of main onto head c0de that it asked of you for LEGION-208: if you have begun that merge, finish it, push it and do nothing more; if you have not begun it, begin none." +
	" The daemon does not ask for that merge forward again: in a later task of the issue, merge main forward only once GitHub reads its pull request CONFLICTING." +
	" The daemon gives the issue its next task once its children are finished."

// endedDroppingTheForward is the phase-ended message the admission pass queues when it stops the
// tester of LEGION-208 in the middle of testing and drops droppedForward: the form for a worker
// other than the implementer, which writes and pushes nothing more. internal/record's
// TestAPhaseEndedMessageIntoAwaitingChildrenGivesTheWaitsOwnWords reads that form word for word,
// so the text published here is the one its Message gives.
var endedDroppingTheForward = record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.AwaitingChildren,
	Reason: endedByTheWait.Reason, MergeForward: &droppedForward}

// endedTheImplementersPhase is the phase-ended message the admission pass queues when it stops the
// implementer of LEGION-208 in the middle of implementing and drops no merge forward: the plain
// form, which tells it to commit and push what it has (record.PhaseEnded.Message).
var endedTheImplementersPhase = record.PhaseEnded{Role: claim.RoleImplementer, From: phase.Implementing, To: phase.AwaitingChildren,
	Reason: "the admission pass stopped the implementer's implementing phase, while these Dispatch children of LEGION-208 were open: LEGION-209 (todo)"}

// waitingParentsAsk is the ask a confirmed conflict in testing queued for the implementer of
// LEGION-208 before the admission pass stopped its tester into awaiting_children: the merge
// forward of main onto sha-1, asked because the base's rules were unread
// (classify.ConflictNeedsMerge's reason).
var waitingParentsAsk = record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1",
	Base: "main", Phase: phase.Testing, PullRequest: 86, Why: record.ConflictRulesUnread}

// waitingParentRunner is an outbox runner over LEGION-208 as the admission pass leaves it once it
// stopped the tester into awaiting_children: the record in awaiting_children holding the start of
// testing, and its pull request #86 open at head sha-1 with no merge forward, since the move
// dropped the one the conflict asked for (workflow's clearMergeForward). It drains on the caller's
// goroutine and logs to the buffer it answers, as mergeForwardRunner's does.
func waitingParentRunner(t *testing.T, publisher notify.Publisher) (*outbox, *supervisor, *store.Pool, *syncBuffer) {
	t.Helper()
	runner, sup, pool, logs := mergeForwardRunner(t, publisher, nil)
	waited := time.Now().UTC().Add(-time.Minute)
	putOutboxIssue(t, pool, runner.records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingChildren,
		Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5, ChildrenWaitedAt: &waited,
		ChildrenWait: &record.ChildrenWait{Start: phase.Testing, Task: "Continue Workflow. Issue: LEGION-208. Phase: testing.",
			Reason: endedByTheWait.Reason, Since: waited}})
	return runner, sup, pool, logs
}

// The daemon's side of a parent's move into awaiting_children that drops a merge forward before
// it landed (dispatch://INFRA-3320 R10, and its check
// dispatch://INFRA-3320/artifact/verification-md#b-k12, whose command runs this test beside
// internal/admit's of the same name): the outbox executor delivers the rows that move writes.
// The implementer's role message is a role message as the outbox delivers every one
// (outbox.roleMessage): to a released implementer it ends with no publish and no wake, as
// TestOutboxRoleMessageToAReleasedRoleEndsWithoutAWake reads of any role message, and to a claim
// not released it is published to the implementer's role topic with its body whole. The
// phase-ended message to a released role takes its delivery wake as the plain one does
// (TestAPhaseEndedMessageForAReleasedPodIsKeptForItsWokenSession): the implementer's plain form,
// and the tester's words for a worker other than the implementer. The ask a confirmed conflict
// queued for the implementer and the pass's move left in the outbox finishes undelivered at its
// attempt, since the record holds no merge forward any more (workflow.MergeForwardAt), with no
// publish and no wake of the released implementer.
func TestParentWaitsHoldsEveryPhaseStart(t *testing.T) {
	t.Parallel()
	t.Run("the implementer's role message naming the dropped merge forward ends at a released implementer with no publish and no wake", func(t *testing.T) {
		ctx := context.Background()
		publisher := &outboxPublisher{}
		runner, sup, pool := hookRunner(t, publisher)
		implementer := releasedClaim(t, sup, claim.RoleImplementer)
		token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
			record.RoleMessage{Role: string(token), Body: droppedForwardBody}, time.Now().Add(-time.Minute)))

		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("drain the role message: %v", err)
		}

		if published := publisher.publishes(); len(published) != 0 {
			t.Fatalf("publishes = %#v, want none: the released implementer is not told", published)
		}
		if left := outboxRows(t, pool); left != 0 {
			t.Fatalf("outbox rows left = %d, want the message finished", left)
		}
		if held := implementer.Claim(); !held.Released() {
			t.Fatalf("the implementer is %s by %s, want it still released: the message is no wake", held.State, held.SuspendedBy)
		}
		if resumes := podResumes(t, pool, "LEGION-208"); resumes != 0 {
			t.Fatalf("recorded wakes = %d, want none: the message is no wake", resumes)
		}
	})

	t.Run("the implementer's role message naming the dropped merge forward reaches a claim not released whole", func(t *testing.T) {
		ctx := context.Background()
		publisher := &outboxPublisher{}
		runner, sup, pool := hookRunner(t, publisher)
		startedClaim(t, sup, claim.RoleImplementer)
		token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
			record.RoleMessage{Role: string(token), Body: droppedForwardBody}, time.Now().Add(-time.Minute)))

		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("drain the role message: %v", err)
		}

		topic := notify.RoleTopicPrefix + string(token)
		if published := publisher.publishes(); len(published) != 1 || published[0].topic != topic || published[0].message != droppedForwardBody {
			t.Fatalf("publishes = %#v, want the one message to %s, its body whole:\n\t%q", published, topic, droppedForwardBody)
		}
		if left := outboxRows(t, pool); left != 0 {
			t.Fatalf("outbox rows left = %d, want the message finished", left)
		}
	})

	t.Run("the phase-ended message naming the dropped merge forward wakes the released tester and tells it to write and push nothing more", func(t *testing.T) {
		keptAcrossTheWake(t, endedDroppingTheForward, claim.RoleTester, endedDroppingTheForward.Message("LEGION-208"))
	})

	t.Run("the plain phase-ended message wakes the released implementer's pod and reaches its woken session", func(t *testing.T) {
		keptAcrossTheWake(t, endedTheImplementersPhase, claim.RoleImplementer, endedTheImplementersPhase.Message("LEGION-208"))
	})

	t.Run("the implementer's merge-forward ask still queued once the pass dropped the merge forward finishes undelivered, waking no role", func(t *testing.T) {
		publisher := &holderPublisher{}
		runner, sup, pool, logs := waitingParentRunner(t, publisher)
		implementer := releasedClaim(t, sup, claim.RoleImplementer)
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", waitingParentsAsk, time.Now().Add(-time.Minute)))

		drainAt(t, runner, 0, "the ask's attempt")

		if attempts, _ := publisher.snapshot(); attempts != 0 || outboxRows(t, pool) != 0 {
			t.Fatalf("the ask made %d publishes and %d rows are left, want the row finished with nothing published", attempts, outboxRows(t, pool))
		}
		if lines := loggedLines(t, logs.String(), mergeForwardLeftLine); len(lines) != 1 || lines[0]["why"] != "the record holds no merge forward" {
			t.Fatalf("the ask logged %v, want one line naming why: the record holds no merge forward", lines)
		}
		if held := implementer.Claim(); !held.Released() {
			t.Fatalf("the implementer is %s by %s, want it still released: the ask woke no role", held.State, held.SuspendedBy)
		}
		if resumes := podResumes(t, pool, "LEGION-208"); resumes != 0 {
			t.Fatalf("recorded wakes = %d, want none: the ask woke no role", resumes)
		}
	})
}
