package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A move into accepting starts no worker: accepting's role is the architect, so its row starts
// nobody (effectsInto), and the phase-finished notice of the step before it is what tells the
// architect that the issue waits for its acceptance (roles/architect-root.md: "the pass of the
// step before it moves the issue to `accepting` ... That step's notice of it comes to you for the
// root"). Main's #5161 lets an owner's plan put `acceptance:owner` right after the implementer or
// the tester (plan/validate.go: "the tester, the reviewer and the acceptance in any order after
// the implementer"), so that notice can come from either of them.
//
// R5 of dispatch://INFRA-3686 makes a notice low only where its reader's act can wait for the
// reader's next turn, and the spec's Design gives the reason a worker's phase-finished is low: "the
// daemon has already started the next phase". Into accepting the daemon started no phase, and a
// low notice wakes no released pod and starts no turn (go/architect-common.md), so the issue would
// wait in accepting until something else starts the architect's turn; on main every notice starts
// one. So the notice of a move into accepting is med for the architect, whichever step's pass made
// the move, as the reviewer's approval's is. The first case, a reviewer's approval, is the
// control: it passes at the head where the other two fail.
//
// The urgency is read as the daemon's publish reads it (daemon's noticeUrgency): texts.UrgencyOf
// with the notice's role, phase, to, change and hold.
func TestAMoveIntoAcceptingIsMedForTheArchitect(t *testing.T) {
	type later struct {
		trigger TriggerKind
		target  phase.Phase
	}
	for _, c := range []struct {
		name   string
		steps  []string
		moves  []TriggerKind
		review bool
		then   []later
		from   claim.Role
	}{
		{name: "the plan implementer reviewer acceptance:owner, after the reviewer's approval",
			steps: []string{"implementer", "reviewer", "acceptance:owner"}, moves: []TriggerKind{TriggerImplementationReady}, review: true,
			from: claim.RoleReviewer},
		{name: "the plan implementer acceptance:owner, after the implementer",
			steps: []string{"implementer", "acceptance:owner"}, moves: []TriggerKind{TriggerImplementationReady}, from: claim.RoleImplementer},
		{name: "the plan implementer tester acceptance:owner, after the tester's pass",
			steps: []string{"implementer", "tester", "acceptance:owner"}, moves: []TriggerKind{TriggerImplementationReady, TriggerTesterPassed},
			from: claim.RoleTester},
		// A merger's own move back out of merging into accepting (request_backward_move) starts no
		// worker either: the architect, which holds accepting, is the one that acts next.
		{name: "the plan implementer reviewer acceptance:owner, after the merger's move back",
			steps: []string{"implementer", "reviewer", "acceptance:owner"}, moves: []TriggerKind{TriggerImplementationReady}, review: true,
			then: []later{{TriggerSignOff, ""}, {TriggerRetroCompleted, ""}, {TriggerBackward, phase.Accepting}}, from: claim.RoleMerger},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, c.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
			for _, trigger := range c.moves {
				move(t, pool, engine, root.Key, trigger, "", record.PhaseRow{})
			}
			if c.review {
				seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
				reviewApprovedNow(t, pool, engine, root.Key)
			}
			for _, step := range c.then {
				move(t, pool, engine, root.Key, step.trigger, step.target, record.PhaseRow{})
			}
			if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Accepting || got.Hold != nil {
				t.Fatalf("premise: %s stands in %s (hold %+v), want accepting", root.Key, got.Phase, got.Hold)
			}

			var into []record.Notice
			for _, notice := range noticesOf(t, pool, root.Key, record.NoticeKind(texts.PhaseFinished)) {
				if notice.To == phase.Accepting && notice.Role == c.from {
					into = append(into, notice)
				}
			}
			if len(into) != 1 {
				t.Fatalf("%s's outbox holds %d phase-finished notices of the %s's move into accepting, want 1: %+v", root.Key, len(into), c.from, into)
			}
			notice := into[0]
			got := texts.UrgencyOf(texts.Kind(notice.Kind), claim.RoleArchitect,
				texts.Variant{Role: notice.Role, Phase: notice.Phase, To: notice.To, Change: notice.Change, Hold: notice.Hold})
			t.Logf("the %s's phase-finished out of %s into %s goes to the architect at %s", notice.Role, notice.Phase, notice.To, got)
			if got != texts.Med {
				t.Fatalf("the %s's phase-finished that moves %s into accepting goes to the architect at %s, want med: "+
					"the daemon started no phase for it, the architect's sign-off is the next act, and a low notice starts no turn "+
					"and wakes no released pod, so nothing tells the architect the issue waits for its acceptance", notice.Role, root.Key, got)
			}
		})
	}
}
