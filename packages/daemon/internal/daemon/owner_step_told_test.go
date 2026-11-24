package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ownerHoldLaunch and ownerHoldSession are the launch and the session the owner of newToldRig is
// told its step in.
const (
	ownerHoldLaunch  uint64 = 1
	ownerHoldSession        = "ses_owner"
)

// stepOneTold is the owner-step of step 1 of LEGION-1's task a, `implementer:owner`, in the run
// its first start began, as that start queues it (workflow's tellOwner).
func stepOneTold() record.Notice {
	return record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer, Phase: phase.Implementing,
		Reason: "Step 1 of 2: implementer:owner.", OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 1}}
}

// newToldRig is LEGION-1 standing at step 1 of `implementer:owner reviewer`, its run open, and the
// owner's claim at launch ownerHoldLaunch in session ownerHoldSession, its slot holding the step's
// App (relaunched).
func newToldRig(t *testing.T) *ownerHoldRig {
	t.Helper()
	r := newOwnerHoldRig(t, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, "implementer:owner", "reviewer")
	r.started(t, "step", 1, false)
	r.relaunched(t, ownerHoldLaunch, ownerHoldSession)
	return r
}

// relaunched is the owner's claim of LEGION-1 running at launch in session, its slot holding the
// step's App: the claim the outbox's supervisor holds, which a publish reads, and the claim the
// claims table holds, as the daemon persists the claim a launch's ready comes from
// (claimDurableState), which the workflow reads.
func (r *ownerHoldRig) relaunched(t *testing.T, launch uint64, session string) {
	t.Helper()
	ctx := context.Background()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	if _, _, err := sup.Create(ctx, supervise.Claim{Token: r.owner, Project: "legion", Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		State: supervise.StateWorking, Generation: launch, Session: session}, ""); err != nil {
		t.Fatalf("run the owner's claim at launch %d in session %s: %v", launch, session, err)
	}
	rt.ScriptSlotHolds(nil)
	r.rt, r.runner.supervisor = rt, sup
	if _, err := r.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-1', 'LEGION-1', 'architect', $2, $3, '', 'working', 0, 0, 0, 0)
		on conflict (token) do update set generation = excluded.generation, session = excluded.session`,
		string(r.owner), int64(launch), session); err != nil {
		t.Fatalf("record the owner's claim at launch %d in session %s: %v", launch, session, err)
	}
}

// ready applies the owner's claim saying it is ready at launch, under id, through the workflow:
// a launch's own ready (paced false), or the one the outbox's start applies for a launch that
// still runs (paced true).
func (r *ownerHoldRig) ready(t *testing.T, id string, launch uint64, paced bool) {
	t.Helper()
	engine := wakeEngine(r.records, nil, func() time.Time { return r.now })
	if _, err := intake.ApplyFact(context.Background(), r.pool, "supervise", id,
		intake.ClaimReady{Issue: "LEGION-1", Role: claim.RoleArchitect, Launch: launch, Paced: paced}, engine); err != nil {
		t.Fatalf("apply the owner's ready %s: %v", id, err)
	}
}

// queuedOwnerSteps is every owner-step of LEGION-1 the outbox holds, in row order.
func (r *ownerHoldRig) queuedOwnerSteps(t *testing.T) []record.Notice {
	t.Helper()
	var steps []record.Notice
	for _, notice := range queuedNoticesOf(t, r.pool, "LEGION-1") {
		if notice.Kind == record.OwnerStepNotice {
			steps = append(steps, notice)
		}
	}
	return steps
}

// ownerStepsPublished is how many owner-steps the outbox has published.
func (r *ownerHoldRig) ownerStepsPublished() int {
	_, delivered := r.pub.snapshot()
	published := 0
	for _, d := range delivered {
		if notice, ok := d.payload.(record.Notice); ok && notice.Kind == record.OwnerStepNotice {
			published++
		}
	}
	return published
}

// tell runs notice through the executor (told) and fails the test on its error, or unless the
// owner-steps published then number want.
func (r *ownerHoldRig) tell(t *testing.T, notice record.Notice, want int) {
	t.Helper()
	if err := r.told(t, notice); err != nil {
		t.Fatalf("tell the owner a %s %q: %v", notice.Kind, notice.Reason, err)
	}
	if got := r.ownerStepsPublished(); got != want {
		t.Fatalf("the outbox has published %d owner-steps, want %d", got, want)
	}
}

// tellQueued runs every notice of LEGION-1 the outbox holds, the catch-up a ready queued and the
// owner-step among them, in row order, as the runner runs them: each publishes, and its row is
// gone. The owner-steps published then number want.
func (r *ownerHoldRig) tellQueued(t *testing.T, want int) {
	t.Helper()
	queued := queuedNoticesOf(t, r.pool, "LEGION-1")
	if _, err := r.pool.Exec(context.Background(), `delete from outbox where kind = $1 and issue = 'LEGION-1'`,
		string(record.OutboxKindNotice)); err != nil {
		t.Fatalf("take LEGION-1's queued notices: %v", err)
	}
	for _, notice := range queued {
		if err := r.told(t, notice); err != nil {
			t.Fatalf("tell the owner a %s %q: %v", notice.Kind, notice.Reason, err)
		}
	}
	if got := r.ownerStepsPublished(); got != want {
		t.Fatalf("the outbox has published %d owner-steps, want %d", got, want)
	}
}

// An owner's launch tells its step again only to a session that was not told it. The owner of
// LEGION-1, at step 1 `implementer:owner`, is told the step at its launch 1 in its session: the
// outbox publishes the step's owner-step to it. Then:
//
//   - a ready of that same launch, the one the outbox's start applies for an architect that still
//     runs, tells nothing more: the session holds the notice;
//   - the next launch in the same session, a relaunch that resumed it, tells nothing more either;
//   - the next launch in a fresh session, the session told it lost, is told the step once, and a
//     ready of that launch after the notice was published to it tells nothing more;
//   - a step whose owner-step was never published is told at the launch;
//   - the operator's retry of the owner tells it the step once again, whatever the session holds,
//     and the relaunch's ready adds nothing to it, before or after it publishes;
//   - a relaunch into a fresh session while the owner-step is published, the listener forwarding
//     it to the fresh session the outbox's read of the claim did not yet name, is told nothing
//     more at that launch's ready: the mark names the session the listener named
//     (notify.HTTPPublisher.PublishTo);
//   - a fresh session in the launch told the step, one the launch started after its resume of the
//     session told failed, is told the step once: a mark that names a session is decided by it.
//
// The control is the catch-up as it stood (workflow's claimReady at its parent commit): it told
// the step again at every ready where no owner-step was still queued, so the first two cases, the
// ready after the fresh session's notice, and the last ready of the retry each queued one more;
// the mark of the claim the outbox read before the publish, which named the session the
// relaunch left, so the sixth case's ready told the fresh session the step a second time; and the
// launch of the mark deciding a ready of that launch whatever session it ran in, so the last
// case's fresh session was told nothing.
func TestALaunchTellsTheOwnersStepAgainOnlyToASessionNotToldIt(t *testing.T) {
	t.Parallel()
	t.Run("a ready of the launch told the step", func(t *testing.T) {
		r := newToldRig(t)
		r.tell(t, stepOneTold(), 1)
		r.ready(t, "start-running:1", ownerHoldLaunch, true)
		if queued := r.queuedOwnerSteps(t); len(queued) != 0 {
			t.Fatalf("a ready of the launch the step was told in queued %+v, want nothing: that session holds the notice", queued)
		}
	})

	t.Run("the next launch of the session told the step", func(t *testing.T) {
		r := newToldRig(t)
		r.tell(t, stepOneTold(), 1)
		r.relaunched(t, ownerHoldLaunch+1, ownerHoldSession)
		r.ready(t, "ready:2", ownerHoldLaunch+1, false)
		if queued := r.queuedOwnerSteps(t); len(queued) != 0 {
			t.Fatalf("the relaunch that resumed the session told the step queued %+v, want nothing: the session holds the notice", queued)
		}
	})

	t.Run("a fresh session after the loss of the one told the step", func(t *testing.T) {
		r := newToldRig(t)
		r.tell(t, stepOneTold(), 1)
		r.relaunched(t, ownerHoldLaunch+1, "ses_fresh")
		r.ready(t, "ready:2", ownerHoldLaunch+1, false)
		queued := r.queuedOwnerSteps(t)
		if len(queued) != 1 || queued[0].Role != claim.RoleImplementer || queued[0].OwnerStep == nil || *queued[0].OwnerStep != *stepOneTold().OwnerStep {
			t.Fatalf("the fresh session's launch queued %+v, want the owner-step of step 1's run once", queued)
		}
		r.tellQueued(t, 2)
		r.ready(t, "start-running:2", ownerHoldLaunch+1, true)
		if queued := r.queuedOwnerSteps(t); len(queued) != 0 {
			t.Fatalf("a ready of the fresh session's launch after it was told the step queued %+v, want nothing", queued)
		}
	})

	t.Run("a step never told", func(t *testing.T) {
		r := newToldRig(t)
		r.ready(t, "ready:1", ownerHoldLaunch, false)
		if queued := r.queuedOwnerSteps(t); len(queued) != 1 {
			t.Fatalf("the launch of an owner never told its step queued %+v, want its owner-step once", queued)
		}
	})

	t.Run("the operator's retry", func(t *testing.T) {
		r := newToldRig(t)
		r.tell(t, stepOneTold(), 1)
		engine := wakeEngine(r.records, nil, func() time.Time { return r.now })
		result, err := intake.ApplyFact(context.Background(), r.pool, "api", "retry:1",
			intake.RestartPhase{Issue: "LEGION-1", Role: claim.RoleArchitect, Claim: r.owner}, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("the operator's retry of the owner = %+v, %v; want it taken", result.Refusal, err)
		}
		if queued := r.queuedOwnerSteps(t); len(queued) != 1 {
			t.Fatalf("the operator's retry queued %+v, want one owner-step", queued)
		}
		r.relaunched(t, ownerHoldLaunch+1, ownerHoldSession)
		r.ready(t, "ready:2", ownerHoldLaunch+1, false)
		if queued := r.queuedOwnerSteps(t); len(queued) != 1 {
			t.Fatalf("the retry's relaunch with its owner-step queued holds %+v, want that one", queued)
		}
		r.tellQueued(t, 2)
		r.ready(t, "start-running:2", ownerHoldLaunch+1, true)
		if queued := r.queuedOwnerSteps(t); len(queued) != 0 {
			t.Fatalf("a ready of the retried launch after it was told the step queued %+v, want nothing", queued)
		}
	})

	t.Run("a relaunch into a fresh session during the publish", func(t *testing.T) {
		r := newToldRig(t)
		r.runner.notices = &namingPublisher{holderPublisher: r.pub, holder: "ses_fresh"}
		r.tell(t, stepOneTold(), 1)
		r.relaunched(t, ownerHoldLaunch+1, "ses_fresh")
		r.ready(t, "ready:2", ownerHoldLaunch+1, false)
		if queued := r.queuedOwnerSteps(t); len(queued) != 0 {
			t.Fatalf("the ready of the fresh session the listener forwarded the step to queued %+v, want nothing: that session holds it", queued)
		}
	})

	t.Run("a fresh session in the launch told the step", func(t *testing.T) {
		r := newToldRig(t)
		r.tell(t, stepOneTold(), 1)
		// The launch's resume of the session failed, and the same launch started a fresh one.
		r.relaunched(t, ownerHoldLaunch, "ses_fresh")
		r.ready(t, "ready:1-fresh", ownerHoldLaunch, false)
		if queued := r.queuedOwnerSteps(t); len(queued) != 1 {
			t.Fatalf("the fresh session of the launch the step was told in queued %+v, want the owner-step once: the session told it is gone", queued)
		}
	})
}

// namingPublisher is the outbox's publisher with the listener naming holder as the session it
// forwarded each publish to (notify.HTTPPublisher.PublishTo), whatever the claim records: a
// session that took the role while the publish was made.
type namingPublisher struct {
	*holderPublisher
	holder string
}

func (p *namingPublisher) PublishTo(ctx context.Context, topic, message string, payload any, key string) (string, error) {
	return p.holder, p.Publish(ctx, topic, message, payload, key)
}
