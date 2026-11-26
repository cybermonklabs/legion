package daemon

// The Legion reviewer's point 1 on #5285 at 0259f8af (R7 of #5285): the moment a
// keep of a pod records is the moment after which a turn of the issue owes a read, once that turn
// has ended. A turn that started before the read, and that the reading the keep was decided on did
// not show, must not fall before that moment: its work may not be in the read, and no later read
// would be made for it. So the sweep's walk decides only whether the keep holds the pod; the read
// that may end the keep is a step after the walk, which reads the claims as their machines hold
// them then, and the keep's moment is taken before that reading and moved before the start of any
// turn that runs once the read is made. The same holds for the keep a round makes at its read.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// keptRoles are the two roles of the kept pod in each case here.
var keptRoles = []claim.Role{claim.RoleImplementer, claim.RoleTester}

// The tester's turn ends, so the read of the kept pod is due. The sweep reads its claims, and the
// implementer's turn starts before the sweep reads the workspace: the snapshot shows no turn. The
// read of the workspace the implementer's turn owes is made once that turn has ended.
func TestAKeptPodIsReadAgainAfterATurnTheSweepsSnapshotMissed(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.answerRound(podUpdateDrift, keptRoles...)
	rig.roundEndsKept("over a workspace that holds an unpushed commit", keptRoles...)
	rig.turn(claim.RoleTester, nil)

	snapshot, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the sweep's snapshot: %v", err)
	}
	rig.clock.advance(time.Minute)
	rig.envoyTurn(claim.RoleImplementer)
	rig.s.reconcilePodUpdates(rig.ctx, snapshot)
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: rig.tokens[claim.RoleImplementer]})

	ended := rig.workspaceReads()
	for range roundSweeps {
		rig.tick()
	}
	if reads := rig.workspaceReads() - ended; reads != 1 {
		t.Fatalf("the kept pod's workspace was read %d times after the implementer's turn ended, want once: that turn started before the sweep's read, which cannot have seen its work", reads)
	}
}

// The implementer's turn starts while the round's own read runs, at the moment the read was taken.
// The round keeps the pod over the commit it found, and the read that turn owes is made once it has
// ended.
func TestAKeptPodIsReadAgainAfterATurnThatStartedAsItsRoundRead(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.answerRound(podUpdateDrift, keptRoles...)
	var started bool
	rig.rt.OnWorkspaceWork(func() {
		if !started {
			started = true
			rig.envoyTurn(claim.RoleImplementer)
		}
	})
	rig.roundEndsKept("over a workspace that holds an unpushed commit", keptRoles...)
	rig.rt.OnWorkspaceWork(nil)
	if !started {
		t.Fatal("the round read no workspace, so this case proves nothing")
	}
	if reads := rig.workspaceReads(); reads != 1 {
		t.Fatalf("the round read the workspace %d times, want once", reads)
	}

	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: rig.tokens[claim.RoleImplementer]})
	for range roundSweeps {
		rig.tick()
	}
	if reads := rig.workspaceReads(); reads != 2 {
		t.Fatalf("the kept pod's workspace was read %d times after the implementer's turn ended, want once: that turn ran as the round read", reads-1)
	}
}

// The Legion reviewer's point 12: a round's keep over a read that could not be made is read again
// once pods.DeclineUnreadRetry has passed since that read, with no turn of the issue between, and
// not before: one read at the hour, and none at the sweeps after it while it still fails.
func TestAKeptPodWhoseReadFailedIsReadAgainAfterTheRetry(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.FailWorkspaceWork(podUpdateIssue, errRoundRead)
	rig.answerRound(podUpdateDrift, keptRoles...)
	rig.roundEndsKept("over a read that cannot be made", keptRoles...)
	reads := rig.workspaceReads()

	rig.clock.advance(pods.DeclineUnreadRetry - time.Minute)
	for range roundSweeps {
		rig.tick()
	}
	if more := rig.workspaceReads() - reads; more != 0 {
		t.Fatalf("the kept pod was read %d times before %s passed with no turn, want none", more, pods.DeclineUnreadRetry)
	}
	rig.clock.advance(time.Minute)
	for range roundSweeps {
		rig.tick()
	}
	if more := rig.workspaceReads() - reads; more != 1 {
		t.Fatalf("the kept pod was read %d times once %s passed, want once: the read moves the keep's moment", more, pods.DeclineUnreadRetry)
	}
}

// The Legion reviewer's point 12: the holder the boot binds (plan.launchRounds, daemon.run) and
// prepareSandbox hands the Sandbox runtime as its RoundPodEnds, LaunchTakes and LaunchTook forwards
// each call to what it was bound to, and before it is bound keeps every pod, records nothing,
// refuses nothing and tells nobody.
func TestTheLaunchHolderForwardsEachHookItIsBoundTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	taken := runtime.TakenPod{Issue: podUpdateIssue, PodUID: podUpdatePod, Err: errRoundRead}
	holder := &launchRounds{}
	if ends, err := holder.ends(ctx, "token", podUpdatePod); ends || err != nil {
		t.Fatalf("an unbound holder's ends = %t, %v; want the pod kept", ends, err)
	}
	if err := holder.takes(ctx, taken); err != nil {
		t.Fatalf("an unbound holder's takes = %v, want nothing refused", err)
	}
	holder.took(ctx, taken)

	var asked, recorded, told int
	refused := errors.New("the store refuses the record")
	holder.bind(func(context.Context, claim.Token, string) (bool, error) { asked++; return true, nil },
		func(context.Context, runtime.TakenPod) error { recorded++; return refused },
		func(context.Context, runtime.TakenPod) { told++ })
	if ends, err := holder.ends(ctx, "token", podUpdatePod); !ends || err != nil || asked != 1 {
		t.Fatalf("the bound holder's ends = %t, %v after %d readings; want the bound reading's answer, once", ends, err, asked)
	}
	if err := holder.takes(ctx, taken); !errors.Is(err, refused) || recorded != 1 {
		t.Fatalf("the bound holder's takes = %v after %d records; want the bound record's error, once", err, recorded)
	}
	if holder.took(ctx, taken); told != 1 {
		t.Fatalf("the bound holder told %d times, want once", told)
	}
}
