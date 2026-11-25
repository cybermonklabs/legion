package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ownerHoldRig is LEGION-1 standing at place of its task a, whose steps are words (`role` or
// `role:key`), with its owner's claim working and an outbox that publishes to a holder.
type ownerHoldRig struct {
	pool    *store.Pool
	records record.Store
	rt      *fake.Runtime
	owner   claim.Token
	runner  *outbox
	pub     *holderPublisher
	logged  *syncBuffer
	now     time.Time
}

func newOwnerHoldRig(t *testing.T, at phase.Phase, place record.TaskPlace, words ...string) *ownerHoldRig {
	t.Helper()
	ctx := context.Background()
	r := &ownerHoldRig{pool: isolatedOutboxPool(t), records: record.NewStore(), owner: mustClaimToken(t, "LEGION-1", claim.RoleArchitect),
		now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), pub: &holderPublisher{}, logged: &syncBuffer{}}
	issue := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION",
		Title: "LEGION-1", Phase: at, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", LastDispatchSeq: 1}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return putPlannedIssue(t, ctx, tx, r.records, issue, place, r.now, words...)
	}); err != nil {
		t.Fatalf("seed LEGION-1: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	r.rt = rt
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	r.runner = &outbox{log: slog.New(slog.NewJSONHandler(r.logged, nil)), pool: r.pool, dispatchProject: "LEGION", records: r.records, notices: r.pub,
		supervisor: sup, project: "legion", now: time.Now}
	return r
}

// putPlannedIssue writes issue in tx and the plan its architect gave at given: version 1, its one
// task a running at place, whose steps are words (`role` or `role:key`), each of its built-in
// role's type, with their agents named as a plan's are (record.NameAgents).
func putPlannedIssue(t *testing.T, ctx context.Context, tx pgx.Tx, records record.Store, issue record.Issue, place record.TaskPlace, given time.Time,
	words ...string) error {
	t.Helper()
	version := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: mustClaimToken(t, issue.Key, claim.RoleArchitect), GivenAt: given}
	task := record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.",
		State: record.TaskRunning, Place: place}
	roles := stepplan.BuiltinRoles()
	steps := make([]record.PlanStep, 0, len(words))
	for i, word := range words {
		role, key, _ := strings.Cut(word, ":")
		steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: "a", Position: i + 1, Role: role, AgentKey: key,
			Type: roles[role].Type})
	}
	if err := records.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	return records.PutPlan(ctx, tx, version, []record.PlanTask{task}, record.NameAgents(nil, nil, nil, steps))
}

// started counts a start of LEGION-1's place kind/at and leaves its run open, as a start does
// (record.Store.RaiseStepStarts), and ends it where ended (ended).
func (r *ownerHoldRig) started(t *testing.T, kind string, at int, ended bool) {
	t.Helper()
	ctx := context.Background()
	place := record.StepPlace{Task: "a", Kind: kind, Place: at}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.records.RaiseStepStarts(ctx, tx, "LEGION-1", place, func(starts int64) record.StepRecord {
			return record.StepRecord{Task: "a", Kind: kind, Step: at, Agent: string(r.owner), Generation: 1, StartedAt: new(r.now)}
		})
		return err
	}); err != nil {
		t.Fatalf("start LEGION-1's %s %d: %v", kind, at, err)
	}
	if ended {
		r.ended(t, kind, at)
	}
}

// ended ends the open run of LEGION-1's place kind/at, as an end does (record.Store.EndStepStart).
func (r *ownerHoldRig) ended(t *testing.T, kind string, at int) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := r.records.EndStepStart(ctx, tx, "LEGION-1", record.StepPlace{Task: "a", Kind: kind, Place: at})
		return err
	}); err != nil {
		t.Fatalf("end LEGION-1's %s %d: %v", kind, at, err)
	}
}

// told runs one row of LEGION-1 holding payload, a notice or a merge-forward message, through the
// executor as the runner does, and deletes the row where the executor finished it, as the runner
// does. It answers the executor's error.
func (r *ownerHoldRig) told(t *testing.T, payload record.OutboxPayload) error {
	t.Helper()
	row := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", payload, r.now))
	err := r.runner.execute(context.Background(), row)
	if err == nil {
		if _, err := r.pool.Exec(context.Background(), "delete from outbox where id = $1", row.ID); err != nil {
			t.Fatalf("finish row %d: %v", row.ID, err)
		}
	}
	return err
}

// published is the topic each notice was published to, in order.
func (r *ownerHoldRig) published() []string {
	_, delivered := r.pub.snapshot()
	topics := make([]string, 0, len(delivered))
	for _, d := range delivered {
		topics = append(topics, d.topic)
	}
	return topics
}

// staleUnread fails the test unless logged holds one line of a stale owner-step, the line the
// executor logs for an owner-step that finished undelivered (ownerStepStaleLine, the code's own, so
// that no test here holds its words): the executor decided the step no longer runs. What the slot
// was asked is each test's own check (slotCalls), so no attribute of the line that depends on the
// order of the executor's reads is asserted.
func staleUnread(t *testing.T, logged, of string) {
	t.Helper()
	if lines := loggedMessages(t, logged, ownerStepStaleLine); len(lines) != 1 {
		t.Fatalf("the log holds %d lines of %s, want one:\n%s", len(lines), of, logged)
	}
}

// A stale owner-step is not delivered: the notice names the run it tells, and once that run has
// ended it finishes unpublished with the log line of a step that no longer runs, asking the runtime
// nothing. The owner's retro of `implementer:owner`: its notice while the retro runs is published,
// once the slot's gh files hold the implement App, the App of the owner's implementer step, which
// the retro is done as; the same notice once the retro ended is not. A role at two steps,
// `implementer:owner reviewer implementer:owner` standing at step 3: the notice of step 1, whose
// run ended, is not delivered, and the notice of step 3 is.
//
// The control: the hold that judged the App alone published the retro's notice after the retro
// ended (the architect's App is what the slot holds wherever the owner does no step), and the
// notice of step 1 at step 3 (both steps are the implementer's).
func TestAStaleOwnerStepIsNotDelivered(t *testing.T) {
	t.Parallel()
	retro := record.NoticeStep{Task: "a", Kind: "retro", Place: 0, Starts: 1}

	t.Run("the owner's retro, running then ended", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "implementer:owner")
		r.started(t, "retro", 0, false)
		notice := record.Notice{Kind: record.OwnerStepNotice, Phase: phase.Retro, Reason: "LEGION-1 retro. Task: a.", OwnerStep: &retro}
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retro's notice while it runs = %v", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the retro's notice while it runs published %v, want one", got)
		}
		r.ended(t, "retro", 0)
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retro's notice after it ended = %v, want it finished", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the notices published = %v after the retro ended, want the one of the running retro alone", got)
		}
		staleUnread(t, r.logged.String(), "the retro's notice after it ended")
		if got, want := slotCalls(r.rt, r.owner), []string{"WriteSlotCredential", "SlotHoldsCredential implementer"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the retro's notices asked the runtime %q of the owner's slot, want the running retro's %q, and nothing of the ended retro's", got, want)
		}
	})

	t.Run("a role at two steps", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 3}, "implementer:owner", "reviewer", "implementer:owner")
		r.started(t, "step", 1, true)
		r.started(t, "step", 3, false)
		r.rt.ScriptSlotHolds(nil)
		first := record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer, Phase: phase.Implementing,
			Reason: "Step 1 of 3: implementer:owner.", OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 1}}
		if err := r.told(t, first); err != nil {
			t.Fatalf("the notice of step 1 at step 3 = %v, want it finished", err)
		}
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the notice of step 1 at step 3 published %v, want nothing", got)
		}
		third := first
		third.Reason, third.OwnerStep = "Step 3 of 3: implementer:owner.", &record.NoticeStep{Task: "a", Kind: "step", Place: 3, Starts: 1}
		if err := r.told(t, third); err != nil {
			t.Fatalf("the notice of step 3 = %v", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the notice of step 3 published %v, want one", got)
		}
	})
}

// R20: no notice waits on a credential the owner's slot already holds. The owner's acceptance of
// the default steps acts as the review App, the App of the owner's own role, so with the pod on the
// image before this release (its launcher cannot say what the slot holds,
// runtime.ErrAwaitsPodUpdate) the acceptance's owner-step is published after one ask and no write,
// and the reviewer's phase-finished queued after it is published too, held behind nothing. A step
// whose App the slot does not hold, the owner's `implementer:owner`, still waits for the slot.
//
// The control: the hold that asked the slot of every owner-step held the acceptance's notice on
// the pod's update round, and the phase-finished behind it.
func TestAnOwnersStepWhoseAppTheSlotHoldsWaitsForNothing(t *testing.T) {
	t.Parallel()
	t.Run("the acceptance of the default steps", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 4}, "implementer", "tester", "reviewer", "acceptance:owner")
		r.started(t, "step", 4, false)
		r.rt.ScriptSlotHolds(runtime.ErrAwaitsPodUpdate)
		if err := r.told(t, record.Notice{Kind: record.OwnerStepNotice, Role: "acceptance", Phase: phase.Accepting, Reason: "Step 4 of 4: acceptance:owner.",
			OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 4, Starts: 1}}); err != nil {
			t.Errorf("the acceptance's owner-step on a pod of the image before = %v, want it published", err)
		}
		if err := r.told(t, record.Notice{Kind: "phase-finished", Role: claim.RoleReviewer, Phase: phase.Reviewing, Summary: "approved"}); err != nil {
			t.Errorf("the reviewer's phase-finished after it = %v, want it published", err)
		}
		if got := r.published(); len(got) != 2 {
			t.Fatalf("the notices published = %v, want the owner-step and the phase-finished", got)
		}
		if calls := slotCalls(r.rt, r.owner); len(calls) != 1 || calls[0] != "SlotHoldsCredential acceptance" {
			t.Fatalf("the acceptance's owner-step asked the runtime %q of the owner's slot, want one ask and no write", calls)
		}
	})

	t.Run("a step of another App", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, "implementer:owner", "reviewer")
		r.started(t, "step", 1, false)
		r.rt.ScriptSlotHolds(runtime.ErrAwaitsPodUpdate)
		err := r.told(t, record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer, Phase: phase.Implementing, Reason: "Step 1 of 2: implementer:owner.",
			OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 1}})
		if !errors.Is(err, errNoticeWaits) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
			t.Fatalf("the implementer's owner-step on a pod of the image before = %v, want it held for the slot", err)
		}
	})
}

// A step done as the owner's own App waits until the slot's gh files hold that App: after the
// owner's `implementer:owner` step the write-back of the owner's own App has run, but the kubelet
// has not synced it into the pod yet, so the slot's files still hold the implement App
// (runtime.ErrSlotCredentialPending). The owner's acceptance is then held as a notice held for its
// architect, and published nothing, with no write of the slot; once the files hold the review App
// it is published.
//
// The control: the hold that told a step of the owner's own App without asking the slot published
// the acceptance while the owner's gh still acted as the implement App.
func TestAnOwnersStepOfItsOwnAppWaitsForTheSlotToHoldIt(t *testing.T) {
	t.Parallel()
	r := newOwnerHoldRig(t, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, "implementer:owner", "acceptance:owner")
	r.started(t, "step", 1, true)
	r.started(t, "step", 2, false)
	r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
	notice := record.Notice{Kind: record.OwnerStepNotice, Role: "acceptance", Phase: phase.Accepting, Reason: "Step 2 of 2: acceptance:owner.",
		OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 2, Starts: 1}}
	row := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", notice, r.now))
	if err := r.runner.execute(context.Background(), row); !errors.Is(err, errNoticeWaits) || !errors.Is(err, runtime.ErrSlotCredentialPending) {
		t.Fatalf("the acceptance's owner-step while the slot still holds the implement App = %v, want a notice held for its architect", err)
	}
	if got := r.published(); len(got) != 0 {
		t.Fatalf("the acceptance's owner-step published %v while the slot still holds the implement App, want nothing", got)
	}
	r.rt.ScriptSlotHolds(nil)
	row.LeaseToken = leaseOutboxRow(t, r.pool, row.ID)
	if err := r.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the acceptance's owner-step once the slot holds the review App = %v", err)
	}
	if got := r.published(); len(got) != 1 {
		t.Fatalf("the acceptance's owner-step published %v once the slot holds the review App, want one", got)
	}
	if calls := slotCalls(r.rt, r.owner); len(calls) != 2 || calls[0] != "SlotHoldsCredential acceptance" || calls[1] != "SlotHoldsCredential acceptance" {
		t.Fatalf("the acceptance's owner-step asked the runtime %q of the owner's slot, want two asks and no write", calls)
	}
}

// The end of an owner's step writes the owner's slot back to its own App once, and the next notice
// to the owner waits for that write. At the reviewer's step of `implementer:owner reviewer`, once
// the owner's implementer step ended, a notice queued after the slot_credential row that end queued
// waits as a notice held for its architect and publishes nothing; the row asks one write of the
// slot, which the runtime makes for the App it judges, the architect's; then the notice publishes.
// Where the owner's next step of another App runs already (step 3 of
// `implementer:owner reviewer implementer:owner`), the row writes nothing: that step's owner-step
// writes its App, so the slot is written once at that end too.
//
// The control: the write-back that waited for nothing let the notice publish before the write,
// and wrote the slot for step 3's App, which step 3's owner-step wrote again.
func TestTheNextNoticeToTheOwnerWaitsForItsSlotWrittenBack(t *testing.T) {
	t.Parallel()
	t.Run("the notice after the write-back", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, "implementer:owner", "reviewer")
		r.started(t, "step", 1, true)
		slot := r.slotCredentialRow(t)
		notice := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "phase-finished", Role: claim.RoleReviewer,
			Phase: phase.Reviewing, Summary: "approved"}, r.now))
		if err := r.runner.execute(context.Background(), notice); !errors.Is(err, errNoticeWaits) {
			t.Fatalf("the notice queued after the write-back = %v, want a notice held for its architect", err)
		}
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the notice published %v before the write-back ran, want nothing", got)
		}
		r.ran(t, slot, fmt.Sprintf("row %d", slot.ID))
		if calls := slotCalls(r.rt, r.owner); len(calls) != 1 || calls[0] != "WriteSlotCredential" {
			t.Fatalf("the write-back asked the runtime %q of the owner's slot, want one write", calls)
		}
		notice.LeaseToken = leaseOutboxRow(t, r.pool, notice.ID)
		r.ran(t, notice, fmt.Sprintf("row %d", notice.ID))
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the notice after the write-back published %v, want one", got)
		}
	})

	t.Run("the owner's next step of another App", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 3}, "implementer:owner", "reviewer", "implementer:owner")
		r.started(t, "step", 1, true)
		r.started(t, "step", 3, false)
		r.writtenBack(t)
		if calls := slotCalls(r.rt, r.owner); len(calls) != 0 {
			t.Fatalf("the write-back with step 3 running asked the runtime %q, want nothing: step 3's owner-step writes its App", calls)
		}
		r.rt.ScriptSlotHolds(nil)
		if err := r.told(t, record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer, Phase: phase.Implementing,
			Reason: "Step 3 of 3: implementer:owner.", OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 3, Starts: 1}}); err != nil {
			t.Fatalf("step 3's owner-step = %v", err)
		}
		if calls := slotCalls(r.rt, r.owner); len(calls) != 2 || calls[0] != "WriteSlotCredential" || calls[1] != "SlotHoldsCredential implementer" {
			t.Fatalf("step 3's owner-step asked the runtime %q, want one write and the check of the implementer's App", calls)
		}
	})
}

// stands moves LEGION-1 to at, its task a running at place, held by nothing, as a move of the walk
// writes the two (record.Store.MoveTask, PutIssue).
func (r *ownerHoldRig) stands(t *testing.T, at phase.Phase, place record.TaskPlace) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := r.records.Issue(ctx, tx, "LEGION-1")
		if err != nil {
			return err
		}
		if err := r.records.MoveTask(ctx, tx, "LEGION-1", 1, "a", place, record.TaskRunning); err != nil {
			return err
		}
		issue.Phase, issue.Hold = at, nil
		return r.records.PutIssue(ctx, tx, *issue)
	}); err != nil {
		t.Fatalf("move LEGION-1 to %s at %+v: %v", at, place, err)
	}
}

// slotCredentialRow is one supervise slot_credential of the owner's claim, the write of its slot
// back to its own App that the end of an act the owner did as another App queues, leased as the
// runner leases a row before it executes it.
func (r *ownerHoldRig) slotCredentialRow(t *testing.T) record.OutboxRow {
	t.Helper()
	return leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", record.SuperviseRequest{Op: "slot_credential", Tree: "LEGION-1",
		Role: claim.RoleArchitect, Claim: r.owner, Generation: 1}, r.now))
}

// writtenBack runs one slot_credential row of the owner's claim (slotCredentialRow) and deletes it
// once the executor finished it, as the runner does.
func (r *ownerHoldRig) writtenBack(t *testing.T) {
	t.Helper()
	r.ran(t, r.slotCredentialRow(t), "the slot_credential row")
}

// The owner's retro of a task whose last step of the type implementation is the owner's is done as
// that step's role (record.OwnerActsAs), so through its retro the owner's gh acts as the App that
// opened the pull request: the one App GitHub lets resolve the pull request's review threads, which
// the retro resolves where a reviewer accepted a bot's thread and named it to the owner. On
// `implementer:owner reviewer` at the retro, the retro's owner-step has the runtime write the
// owner's slot, and while the slot's gh files do not hold the implement App it publishes nothing
// and waits as a notice held for its architect; once they hold it, it is published. A write-back of
// the owner's own App queued at the end of step 1 that runs only now, during the retro, writes
// nothing, so it does not undo the App the owner was told its retro holds; once the retro has ended
// and the issue stands in merging, the write-back the retro's end queues writes the slot, for the
// App the runtime judges then, the owner's own.
//
// The controls: the retro of `acceptance:owner` alone, a task with no step of the type
// implementation, which the owner does as itself, asks whether the slot holds the owner's own App
// and writes nothing; and at the retro of `implementer reviewer`, which its implementer does, a
// write-back of the owner's slot writes it, as wherever the owner does no step.
func TestTheOwnersRetroOfItsOwnImplementationIsDoneAsTheImplementApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	retro := record.NoticeStep{Task: "a", Kind: "retro", Place: 0, Starts: 1}
	notice := record.Notice{Kind: record.OwnerStepNotice, Phase: phase.Retro, Reason: "LEGION-1 retro. Task: a.", OwnerStep: &retro}
	asked := func(t *testing.T, r *ownerHoldRig, want []string, while string) {
		t.Helper()
		if got := slotCalls(r.rt, r.owner); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s the runtime was asked %q of the owner's slot, want %q", while, got, want)
		}
	}

	t.Run("implementer:owner reviewer", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "implementer:owner", "reviewer")
		r.started(t, "step", 1, true)
		r.started(t, "retro", 0, false)
		r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
		row := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", notice, r.now))
		if err := r.runner.execute(ctx, row); !errors.Is(err, errNoticeWaits) || !errors.Is(err, runtime.ErrSlotCredentialPending) {
			t.Fatalf("the retro's owner-step while the slot does not hold the implement App = %v, want a notice held for its architect", err)
		}
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the retro's owner-step published %v while the slot does not hold the implement App, want nothing", got)
		}
		want := []string{"WriteSlotCredential", "SlotHoldsCredential implementer"}
		asked(t, r, want, "while the slot does not hold the implement App,")

		r.rt.ScriptSlotHolds(nil)
		row.LeaseToken = leaseOutboxRow(t, r.pool, row.ID)
		r.ran(t, row, "the retro's owner-step once the slot holds the implement App")
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the retro's owner-step published %v once the slot holds the implement App, want one", got)
		}
		want = append(want, "WriteSlotCredential", "SlotHoldsCredential implementer")
		asked(t, r, want, "once the slot holds the implement App,")

		r.writtenBack(t)
		asked(t, r, want, "after a write-back of step 1's end ran during the retro, which must write nothing,")

		r.ended(t, "retro", 0)
		r.stands(t, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
		r.writtenBack(t)
		asked(t, r, append(want, "WriteSlotCredential"), "after the retro's write-back ran in merging,")
	})

	// hold puts LEGION-1, standing in its retro, on a hold from it, as an escalation does.
	hold := func(t *testing.T, r *ownerHoldRig) {
		t.Helper()
		if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			issue, err := r.records.Issue(ctx, tx, "LEGION-1")
			if err != nil {
				return err
			}
			issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Retro, Reason: record.HoldEscalated}
			return r.records.PutIssue(ctx, tx, *issue)
		}); err != nil {
			t.Fatalf("hold LEGION-1 from its retro: %v", err)
		}
	}

	// A hold of the retro gives the owner no acting role (record.OwnerActsAs), and the owner does no
	// step or retro then: complete_step refuses its report. So the retro's owner-step run during the
	// hold finishes undelivered, writing and asking nothing, as a held step's does. Once the hold's
	// retry puts the issue back in the retro, the retry tells the retro again (workflow's restart,
	// an owner-step of the same run), which is told once the slot holds the implement App. While the
	// executor compared the role of the owner's place with the retro's, this case finished undelivered
	// by that comparison alone (the retro is done as the implementer, and the held issue's owner acts
	// as nothing), so the acceptance:owner twin below, whose retro is done as the owner's own App, was
	// the case that showed the hold check; with that comparison gone, both finish undelivered by the
	// hold check alone.
	t.Run("implementer:owner reviewer, its retro held, then retried", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "implementer:owner", "reviewer")
		r.started(t, "step", 1, true)
		r.started(t, "retro", 0, false)
		hold(t, r)
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retro's owner-step while the issue is held = %v, want it finished", err)
		}
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the retro's owner-step published %v while the issue is held, want nothing", got)
		}
		staleUnread(t, r.logged.String(), "the held retro's owner-step")
		asked(t, r, nil, "for the retro's owner-step while the issue is held,")

		r.stands(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		r.rt.ScriptSlotHolds(nil)
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retry's owner-step of the retro = %v", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the retry's owner-step of the retro published %v, want one", got)
		}
		asked(t, r, []string{"WriteSlotCredential", "SlotHoldsCredential implementer"}, "for the retry's owner-step of the retro,")
	})

	// The same holds of a retro the owner does as itself, of `acceptance:owner` alone, whose App is
	// the owner's own: the hold, not the App, ends what the owner may do. While the issue is held
	// the retro's owner-step finishes undelivered and asks the runtime nothing, and once the hold's
	// retry puts the issue back in the retro the retry's owner-step is told after one ask.
	t.Run("acceptance:owner alone, its retro held, then retried", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "acceptance:owner")
		r.started(t, "step", 1, true)
		r.started(t, "retro", 0, false)
		hold(t, r)
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retro's owner-step while the issue is held = %v, want it finished", err)
		}
		if got := r.published(); len(got) != 0 {
			t.Fatalf("the retro's owner-step published %v while the issue is held, want nothing", got)
		}
		staleUnread(t, r.logged.String(), "the held retro's owner-step")
		asked(t, r, nil, "for the retro's owner-step while the issue is held,")

		r.stands(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retry's owner-step of the retro = %v", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the retry's owner-step of the retro published %v, want one", got)
		}
		asked(t, r, []string{"SlotHoldsCredential architect"}, "for the retry's owner-step of the retro,")
	})

	t.Run("control: acceptance:owner alone", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "acceptance:owner")
		r.started(t, "step", 1, true)
		r.started(t, "retro", 0, false)
		if err := r.told(t, notice); err != nil {
			t.Fatalf("the retro's owner-step = %v", err)
		}
		if got := r.published(); len(got) != 1 {
			t.Fatalf("the retro's owner-step published %v, want one", got)
		}
		asked(t, r, []string{"SlotHoldsCredential architect"}, "for the retro the owner does as itself,")
	})

	t.Run("control: the retro of a fresh implementer", func(t *testing.T) {
		r := newOwnerHoldRig(t, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, "implementer", "reviewer")
		r.writtenBack(t)
		asked(t, r, []string{"WriteSlotCredential"}, "for a write-back of the owner's slot at its implementer's retro,")
	})
}
