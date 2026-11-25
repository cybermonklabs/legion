package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergeForwardHead is the head the merge forward of these tests is asked of, and mergeForwardLanded
// the head its merge leaves.
const (
	mergeForwardHead   = "c0de"
	mergeForwardLanded = "d00d"
)

// askedOf records what a merge forward asked in at leaves on LEGION-1: the implementer's phases row
// running step 1 of the plan under implementer, and the open pull request #42 at the head that
// conflicted, holding the ask of implementer (record.MergeForward.Agent), not landed.
func (r *ownerHoldRig) askedOf(t *testing.T, implementer claim.Token, at phase.Phase) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-1", Role: claim.RoleImplementer, Claim: implementer, Entry: 1,
			StepTask: "a", StepPlace: 1}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: "LEGION-1", Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-1",
			State: record.PullRequestOpen, HeadSHA: mergeForwardHead, Failing: []string{},
			MergeForward: &record.MergeForward{Head: mergeForwardHead, Base: "main", Phase: at, Agent: implementer, Why: record.ConflictCheckNotGreen, AskedAt: r.now}})
	}); err != nil {
		t.Fatalf("record the merge forward asked of LEGION-1 in %s: %v", at, err)
	}
}

// lands records the merge forward's landing on LEGION-1's pull request, as the synchronize that
// moved its head does (workflow's mergeForwardLanded): the new head, and whether the landing asked
// the reviewer to read the merge.
func (r *ownerHoldRig) lands(t *testing.T, reviewAsked bool) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := r.records.PullRequest(ctx, tx, "LEGION-1")
		if err != nil || pr == nil {
			return errors.Join(err, errors.New("LEGION-1 records no pull request"))
		}
		landed := *pr.MergeForward
		landed.Landed, landed.ReviewAsked = mergeForwardLanded, reviewAsked
		pr.MergeForward, pr.HeadSHA = &landed, mergeForwardLanded
		return r.records.PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("land LEGION-1's merge forward: %v", err)
	}
}

// mergeForwardAsk is the ask of a merge forward in at to the implementer's claim token.
func mergeForwardAsk(token claim.Token, at phase.Phase) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: claim.RoleImplementer, Claim: token, Step: record.MergeForwardAsk, Head: mergeForwardHead, Base: "main",
		Phase: at, PullRequest: 42, Why: record.ConflictCheckNotGreen}
}

// mergeForwardReview is the review the landing in at asks of the reviewer's claim token.
func mergeForwardReview(token claim.Token, at phase.Phase) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: claim.RoleReviewer, Claim: token, Step: record.MergeForwardReview, Head: mergeForwardHead,
		Landed: mergeForwardLanded, Base: "main", Phase: at, PullRequest: 42}
}

// waits fails the test unless err is a wait of the row on its backoff.
func waits(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("%s = %v, want a wait", what, err)
	}
}

// A merge forward is an act as the implementer, and the owner's slot holds the App of the role the
// act is done as. On `implementer:owner reviewer`, standing at the reviewer's step, another agent's,
// a base that moved asks the owner to merge it forward. The ask waits while the owner's slot does
// not hold the implement App: the executor has the runtime write the slot, which it judges through
// the daemon's ActsAs, and asks whether the slot's gh files hold the implement App; once they do,
// the ask is delivered, once. A write-back that runs while the ask is open writes nothing, so it
// does not undo the App the owner was told it acts as. Once the merge forward landed the owner acts
// as nothing again, and the write-back the landing queues writes the slot.
//
// The control: on `implementer reviewer` the ask goes to the implementer's own claim, and the
// owner's slot is never written or asked.
func TestAMergeForwardAskedOfTheOwnerWaitsForTheImplementApp(t *testing.T) {
	t.Parallel()
	reviewer := record.TaskPlace{Kind: record.PlaceStep, Position: 2}

	t.Run("implementer:owner reviewer", func(t *testing.T) {
		t.Parallel()
		r := newOwnerHoldRig(t, phase.Reviewing, reviewer, "implementer:owner", "reviewer")
		r.started(t, "step", 1, true)
		r.askedOf(t, r.owner, phase.Reviewing)
		r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
		ask := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", mergeForwardAsk(r.owner, phase.Reviewing), r.now))
		waits(t, r.runner.execute(context.Background(), ask), "the ask while the owner's slot does not hold the implement App")
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the ask published %v while the owner's slot does not hold the implement App, want nothing", got)
		}
		want := []string{"WriteSlotCredential", "SlotHoldsCredential implementer"}
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the ask asked the runtime %q of the owner's slot, want %q", got, want)
		}

		r.rt.ScriptSlotHolds(nil)
		ask.LeaseToken = leaseOutboxRow(t, r.pool, ask.ID)
		r.ran(t, ask, "the ask once the owner's slot holds the implement App")
		if got := r.published(); len(got) != 1 || got[0] != notify.RoleTopicPrefix+string(r.owner) {
			t.Fatalf("the ask published %v once the owner's slot holds the implement App, want one to the owner's topic", got)
		}
		want = append(want, "WriteSlotCredential", "SlotHoldsCredential implementer")
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the ask asked the runtime %q of the owner's slot, want %q", got, want)
		}

		r.writtenBack(t)
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("a write-back while the ask is open asked the runtime %q of the owner's slot, want nothing more than %q", got, want)
		}
		r.lands(t, false)
		r.writtenBack(t)
		want = append(want, "WriteSlotCredential")
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the landing's write-back asked the runtime %q of the owner's slot, want %q", got, want)
		}
	})

	t.Run("control: implementer reviewer", func(t *testing.T) {
		t.Parallel()
		r := newOwnerHoldRig(t, phase.Reviewing, reviewer, "implementer", "reviewer")
		implementer := mustClaimToken(t, "LEGION-1", claim.RoleImplementer)
		if _, _, err := r.runner.supervisor.Create(context.Background(), supervise.Claim{Token: implementer, Project: "legion", Tree: "LEGION-1",
			Issue: "LEGION-1", Role: claim.RoleImplementer, State: supervise.StateWorking}, ""); err != nil {
			t.Fatalf("create the implementer's claim: %v", err)
		}
		r.askedOf(t, implementer, phase.Reviewing)
		r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
		if err := r.told(t, mergeForwardAsk(implementer, phase.Reviewing)); err != nil {
			t.Fatalf("the ask to the implementer = %v, want it delivered", err)
		}
		if got := r.published(); len(got) != 1 || got[0] != notify.RoleTopicPrefix+string(implementer) {
			t.Fatalf("the ask published %v, want one to the implementer's topic", got)
		}
		if got := slotCalls(r.rt, r.owner); len(got) != 0 {
			t.Fatalf("the ask to the implementer asked the runtime %q of the owner's slot, want nothing", got)
		}
	})
}

// The review a landed merge forward asks of the reviewer is an act as the reviewer, and GitHub
// refuses an approval from the pull request's author, the implement App. On `implementer:owner
// reviewer:owner` the review goes to the owner's claim. In the owner's retro, which the owner does
// as its implementer step, the slot holds the implement App, so the review waits and asks the
// runtime nothing. Once the retro ended and the issue stands in merging, the review waits for the
// slot's gh files to hold the reviewer's App, which is the owner's own, so it writes nothing; the
// retro's write-back writes the slot, and the review is delivered once. The same holds of the review
// a merge forward asked of the owner in merging asks: the ask holds the implement App, and the
// review reaches the owner only after the landing's write-back.
func TestTheReviewOfAMergeForwardReachesTheOwnerOnceItsSlotHoldsTheReviewApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	held := func(t *testing.T, r *ownerHoldRig, review record.OutboxRow) {
		t.Helper()
		before := len(r.published())
		r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
		review.LeaseToken = leaseOutboxRow(t, r.pool, review.ID)
		waits(t, r.runner.execute(ctx, review), "the review while the write-back has not landed")
		if got := r.published(); len(got) != before {
			t.Fatalf("the review published %v before the write-back landed, want nothing more than %d", got, before)
		}
		if got := slotCalls(r.rt, r.owner); len(got) == 0 || got[len(got)-1] != "SlotHoldsCredential reviewer" {
			t.Fatalf("the review asked the runtime %q of the owner's slot, want an ask for the reviewer's App last", got)
		}
		r.writtenBack(t)
		r.rt.ScriptSlotHolds(nil)
		review.LeaseToken = leaseOutboxRow(t, r.pool, review.ID)
		if err := r.runner.execute(ctx, review); err != nil {
			t.Fatalf("the review once the write-back landed = %v", err)
		}
		if got := r.published(); len(got) != before+1 || got[before] != notify.RoleTopicPrefix+string(r.owner) {
			t.Fatalf("the review published %v once the write-back landed, want one more, to the owner's topic", got)
		}
	}

	t.Run("landed in the owner's retro", func(t *testing.T) {
		t.Parallel()
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "implementer:owner", "reviewer:owner")
		r.started(t, "step", 1, true)
		r.started(t, "step", 2, true)
		r.started(t, "retro", 0, false)
		r.askedOf(t, r.owner, phase.Retro)
		r.lands(t, true)
		review := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", mergeForwardReview(r.owner, phase.Retro), r.now))
		r.rt.ScriptSlotHolds(nil)
		waits(t, r.runner.execute(ctx, review), "the review during the owner's retro")
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the review published %v during the owner's retro, want nothing", got)
		}
		if got := slotCalls(r.rt, r.owner); len(got) != 0 {
			t.Fatalf("the review during the owner's retro asked the runtime %q of the owner's slot, want nothing", got)
		}
		r.ended(t, "retro", 0)
		r.stands(t, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
		held(t, r, review)
	})

	t.Run("asked of the owner in merging", func(t *testing.T) {
		t.Parallel()
		r := newOwnerHoldRig(t, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge}, "implementer:owner", "reviewer:owner")
		r.started(t, "step", 1, true)
		r.started(t, "step", 2, true)
		r.started(t, "retro", 0, true)
		r.askedOf(t, r.owner, phase.Merging)
		r.rt.ScriptSlotHolds(nil)
		if err := r.told(t, mergeForwardAsk(r.owner, phase.Merging)); err != nil {
			t.Fatalf("the ask in merging = %v", err)
		}
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != "WriteSlotCredential,SlotHoldsCredential implementer" {
			t.Fatalf("the ask in merging asked the runtime %q of the owner's slot, want a write and an ask for the implementer's App", got)
		}
		r.lands(t, true)
		review := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", mergeForwardReview(r.owner, phase.Merging), r.now))
		held(t, r, review)
	})
}

// landsThroughTheWorkflow lands the merge forward of LEGION-1's pull request through the workflow
// itself: the synchronize that moves the head off the head the merge forward was asked of
// (workflow's mergeForwardLanded), which records the landing and queues what it tells.
func (r *ownerHoldRig) landsThroughTheWorkflow(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	engine := workflow.New(r.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	if _, err := intake.ApplyFact(ctx, r.pool, "github", "synchronize-the-merge", intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 42,
		Branch: "legion/LEGION-1", Base: "main", HeadSHA: mergeForwardLanded, Before: mergeForwardHead, Sender: "legion-implementer[bot]"}, engine); err != nil {
		t.Fatalf("apply the synchronize that lands LEGION-1's merge forward: %v", err)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := r.records.PullRequest(ctx, tx, "LEGION-1")
		if err == nil && (pr == nil || pr.MergeForward == nil || pr.MergeForward.Landed != mergeForwardLanded) {
			err = fmt.Errorf("the record holds the pull request %+v", pr)
		}
		return err
	}); err != nil {
		t.Fatalf("the synchronize did not land the merge forward, so this case proves nothing: %v", err)
	}
}

// queuedRow is the one row of LEGION-1's outbox whose payload matches, leased as the runner leases
// a row before it executes it.
func (r *ownerHoldRig) queuedRow(t *testing.T, what string, matches func(record.OutboxPayload) bool) record.OutboxRow {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "select id, kind, payload from outbox where issue = 'LEGION-1' order by id")
	if err != nil {
		t.Fatalf("read LEGION-1's outbox: %v", err)
	}
	defer rows.Close()
	var found []record.OutboxRow
	for rows.Next() {
		row := record.OutboxRow{Issue: "LEGION-1"}
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Payload); err != nil {
			t.Fatalf("read a row of LEGION-1's outbox: %v", err)
		}
		row.Kind = record.OutboxKind(kind)
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode row %d: %v", row.ID, err)
		}
		if matches(payload) {
			found = append(found, row)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read LEGION-1's outbox: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("LEGION-1's outbox holds %d rows of %s, want one", len(found), what)
	}
	row := found[0]
	row.LeaseToken = leaseOutboxRow(t, r.pool, row.ID)
	return row
}

// ran executes row as the runner does and deletes it once the executor finished it.
func (r *ownerHoldRig) ran(t *testing.T, row record.OutboxRow, what string) {
	t.Helper()
	if err := r.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("%s = %v", what, err)
	}
	if _, err := r.pool.Exec(context.Background(), "delete from outbox where id = $1", row.ID); err != nil {
		t.Fatalf("finish row %d: %v", row.ID, err)
	}
}

// While the owner's slot acts as the implementer for a merge forward asked of it, nothing asks the
// owner for an act as another role. The owner's review step of `implementer:owner reviewer:owner`
// and its acceptance of `implementer:owner acceptance:owner`, told while the ask is open, wait as a
// notice held for its architect, with no stale line, since their runs go on. The merge then lands
// through the workflow (landsThroughTheWorkflow), which queues the write-back of the owner's slot.
// Once the write-back ran, each notice is told once. No landed is queued: the owner was asked the
// merge as the agent of the task's implementation step and pushed it itself.
func TestTheOwnersStepsWaitWhileItsSlotActsForAMergeForward(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		words []string
		role  claim.Role
	}{
		{name: "the owner's review step", at: phase.Reviewing, words: []string{"implementer:owner", "reviewer:owner"}, role: claim.RoleReviewer},
		{name: "the owner's acceptance", at: phase.Accepting, words: []string{"implementer:owner", "acceptance:owner"}, role: claim.RoleAcceptance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := newOwnerHoldRig(t, tc.at, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, tc.words...)
			r.started(t, "step", 1, true)
			r.started(t, "step", 2, false)
			r.askedOf(t, r.owner, tc.at)
			r.stepTwoRaised(t, tc.role)
			r.rt.ScriptSlotHolds(nil)
			notice := record.Notice{Kind: record.OwnerStepNotice, Role: tc.role, Phase: tc.at, Reason: "Step 2 of 2.",
				OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 2, Starts: 1}}
			row := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", notice, r.now))
			if err := r.runner.execute(ctx, row); !errors.Is(err, errNoticeWaits) {
				t.Fatalf("the owner-step of step 2 while the merge forward is open = %v, want a notice held for its architect", err)
			}
			if got := r.published(); len(got) != 0 {
				t.Fatalf("the owner-step of step 2 published %v while the merge forward is open, want nothing", got)
			}
			if lines := loggedMessages(t, r.logged.String(), ownerStepStaleLine); len(lines) != 0 {
				t.Fatalf("the owner-step of step 2 logged %d stale lines while the merge forward is open, want none: its run goes on", len(lines))
			}

			r.landsThroughTheWorkflow(t)
			r.ran(t, r.queuedRow(t, "the write-back of the owner's slot", func(payload record.OutboxPayload) bool {
				request, ok := payload.(record.SuperviseRequest)
				return ok && request.Op == "slot_credential" && request.Claim == r.owner
			}), "the write-back the landing queued")
			row.LeaseToken = leaseOutboxRow(t, r.pool, row.ID)
			if err := r.runner.execute(ctx, row); err != nil {
				t.Fatalf("the owner-step of step 2 once the merge forward landed = %v", err)
			}
			if got := r.published(); len(got) != 1 {
				t.Fatalf("the owner-step of step 2 published %v once the merge forward landed, want one", got)
			}
			var landed int
			if err := r.pool.QueryRow(ctx, "select count(*) from outbox where kind = $1 and payload->>'step' = $2",
				string(record.OutboxKindMergeForward), string(record.MergeForwardLanded)).Scan(&landed); err != nil || landed != 0 {
				t.Fatalf("the landing queued %d landed messages (%v), want none: the owner pushed the merge itself", landed, err)
			}
		})
	}
}

// stepTwoRaised records the phases row step 2's start raised under the owner's claim (workflow's
// startOwnersStep) on role's row, which a message to that step's worker is addressed by.
func (r *ownerHoldRig) stepTwoRaised(t *testing.T, role claim.Role) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-1", Role: role, Claim: r.owner, Entry: 1, StepTask: "a", StepPlace: 2})
	}); err != nil {
		t.Fatalf("record the phases row of step 2: %v", err)
	}
}

// The landed of a merge forward that another agent pushed reaches the owner at its own step, by the
// step's role and the owner's claim (record.Issue.StepWorker), as a fresh worker of that step is
// told. It asks the owner to go on with that step at the new head, so it waits for the owner's slot
// to hold the App of that step's role. On `implementer reviewer:owner` at the owner's review step,
// the ask went to the fresh implementer; the merge lands through the workflow, which queues the
// landed to the owner's claim as the reviewer and no write-back of the owner's slot. While the
// slot's gh files do not hold the review App the landed waits; once they do, it is told.
func TestTheLandedReachesTheOwnerAtItsOwnStepOnceItsSlotHoldsTheStepsApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newOwnerHoldRig(t, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, "implementer", "reviewer:owner")
	r.started(t, "step", 2, false)
	r.askedOf(t, mustClaimToken(t, "LEGION-1", claim.RoleImplementer), phase.Reviewing)
	r.stepTwoRaised(t, claim.RoleReviewer)
	r.landsThroughTheWorkflow(t)
	var writeBacks int
	if err := r.pool.QueryRow(ctx, "select count(*) from outbox where kind = $1 and payload->>'op' = 'slot_credential'",
		string(record.OutboxKindSupervise)).Scan(&writeBacks); err != nil || writeBacks != 0 {
		t.Fatalf("the landing queued %d write-backs (%v), want none: the ask was the fresh implementer's", writeBacks, err)
	}
	var message record.MergeForwardMessage
	landed := r.queuedRow(t, "the landed to the owner's review step", func(payload record.OutboxPayload) bool {
		queued, ok := payload.(record.MergeForwardMessage)
		if ok && queued.Step == record.MergeForwardLanded {
			message = queued
			return true
		}
		return false
	})
	if message.Claim != r.owner || message.Role != claim.RoleReviewer {
		t.Fatalf("the landing queued the landed %+v, want it to the owner's claim %s as the reviewer", message, r.owner)
	}
	r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
	waits(t, r.runner.execute(ctx, landed), "the landed to the owner's review step while its slot's files hold another App")
	if got := r.published(); len(got) != 0 {
		t.Fatalf("the landed published %v while the slot's files hold another App, want nothing", got)
	}
	r.rt.ScriptSlotHolds(nil)
	landed.LeaseToken = leaseOutboxRow(t, r.pool, landed.ID)
	r.ran(t, landed, "the landed to the owner's review step once its slot holds the review App")
	if got := r.published(); len(got) != 1 {
		t.Fatalf("the landed published %v once the slot holds the review App, want one", got)
	}
}
