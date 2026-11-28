package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The head a fix's working copy is made at, and the fix's task as the start executor hands it to
// the implementer's claim: phase implementing, run 1, fix 1.
const (
	copyFixHead = "c0de000000000000000000000000000000000004"
	copyFixTask = "Fix the CI check k-ci, which failed at " + copyFixHead + "."
	// copyMoveHead is the head a task moves the shared working copy to before it is sent.
	copyMoveHead = "c0de000000000000000000000000000000000005"
)

// fixTaskOf is fix 1's task for token's claim, made in a working copy of its own at copyFixHead.
func fixTaskOf(token claim.Token) supervise.RequestDeliver {
	return supervise.RequestDeliver{Claim: token, Task: copyFixTask, ID: "outbox:7", Phase: phase.Implementing, Generation: 1,
		Fix: 1, FixFrom: copyFixHead}
}

// movingFixTaskOf is fixTaskOf that also moves the shared working copy to copyMoveHead first.
func movingFixTaskOf(token claim.Token) supervise.RequestDeliver {
	task := fixTaskOf(token)
	task.MoveTo = copyMoveHead
	return task
}

// inTurn makes role's claim ready on the rig's issue and puts it in a turn: of task, delivered to
// it first, or of no task at all — a turn an Envoy message starts — where task names no claim.
func (r *issueRig) inTurn(role claim.Role, task supervise.RequestDeliver) claim.Token {
	r.t.Helper()
	token, machine := r.ready(role, fake.NewConn())
	if task.Task != "" {
		task.Claim = token
		if err := machine.Handle(r.ctx, task); err != nil {
			r.t.Fatalf("deliver %s's task: %v", role, err)
		}
		machine.Wait()
	}
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		r.t.Fatalf("%s's turn start: %v", role, err)
	}
	machine.Wait()
	if view := machine.View(); view.State != supervise.StateWorking {
		r.t.Fatalf("%s's claim is %s, want it working: the case's own premise", role, view.State)
	}
	return token
}

// wantSentAtOnce holds that task went out at once on conn, after one more adoption of a working
// copy than the adoptions made before it, under role's App and naming want.
func (r *issueRig) wantSentAtOnce(conn *fake.Conn, task string, adoptions int, role claim.Role, want runtime.Adoption) {
	r.t.Helper()
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].Message != task {
		r.t.Fatalf("prompts = %+v, want %q sent at once", prompts, task)
	}
	made := r.rt.CallsOf("AdoptWorkingCopy")
	if len(made) != adoptions+1 || made[adoptions].Identity.Name != "legion-"+string(role)+"[bot]" || made[adoptions].Adoption != want {
		r.t.Fatalf("adoptions = %+v, want the %d made before and one more, under %s's App, naming %+v", made, adoptions, role, want)
	}
}

// wantHeldUntilTheTurnEnds holds that task waits on conn, unsent and adopting nothing, while the
// turn of the claim inTurn is open, and goes once that turn ends: the hold is the turn's.
func (r *issueRig) wantHeldUntilTheTurnEnds(conn *fake.Conn, task string, adoptions int, inTurn claim.Token) {
	r.t.Helper()
	if prompts := conn.Prompts(); len(prompts) != 0 {
		r.t.Fatalf("prompted %+v while %s's turn on the shared copy was open; want the task held", prompts, inTurn)
	}
	if got := r.adoptions(); got != adoptions {
		r.t.Fatalf("adoptions = %d while %s's turn on the shared copy was open, want the %d made before it", got, inTurn, adoptions)
	}
	r.sup.post(inTurn, supervise.StreamTurnEnd{Claim: inTurn})
	testwait.Eventually(r.t, "the held task to be sent once the turn ended", func() bool {
		prompts := conn.Prompts()
		return len(prompts) == 1 && prompts[0].Message == task
	})
}

// A fix works a working copy of its own (the shim's FixDir), so its task's adoption takes nothing
// from a role in a turn in the issue's shared working copy: the implementer's fix's task is adopted
// and sent at once while the reviewer works, as a failed CI check starts the fix at once. The
// control is two tasks on the shared copy, the reviewer working and a tester's task pending: the
// tester's task waits for the reviewer's turn, as it always has, and goes when that turn ends.
func TestAFixTaskWaitsForNoReviewerTurn(t *testing.T) {
	t.Parallel()

	t.Run("the fix's task beside the reviewer's turn", func(t *testing.T) {
		t.Parallel()
		rig := newIssueRig(t)
		rig.inTurn(claim.RoleReviewer, supervise.RequestDeliver{Task: "review the head", ID: "outbox:1", Phase: phase.Reviewing, Generation: 1})
		adoptions := rig.adoptions()
		conn := fake.NewConn()
		implementer, fixing := rig.ready(claim.RoleImplementer, conn)

		if err := fixing.Handle(rig.ctx, fixTaskOf(implementer)); err != nil {
			t.Fatalf("deliver the fix's task: %v", err)
		}
		fixing.Wait()

		rig.wantSentAtOnce(conn, copyFixTask, adoptions, claim.RoleImplementer, runtime.Adoption{FixFrom: copyFixHead})
	})

	t.Run("control: a tester's task on the shared copy waits for the reviewer's turn", func(t *testing.T) {
		t.Parallel()
		rig := newIssueRig(t)
		reviewer := rig.inTurn(claim.RoleReviewer, supervise.RequestDeliver{Task: "review the head", ID: "outbox:1", Phase: phase.Reviewing, Generation: 1})
		adoptions := rig.adoptions()
		conn := fake.NewConn()
		tester, waiting := rig.ready(claim.RoleTester, conn)

		if err := waiting.Handle(rig.ctx, supervise.RequestDeliver{Claim: tester, Task: "test the branch", ID: "outbox:2",
			Phase: phase.Testing, Generation: 1}); err != nil {
			t.Fatalf("deliver the tester's task: %v", err)
		}
		waiting.Wait()

		rig.wantHeldUntilTheTurnEnds(conn, "test the branch", adoptions, reviewer)
	})
}

// The fix's turn writes in the fix's own working copy and nothing in the shared one, so a tester's
// task on the shared copy is adopted and sent at once while the implementer works its fix. The
// controls are the implementer in a turn of a task of no fix, which works the shared copy, and the
// implementer in a turn of no task at all, an Envoy message's, which counts as the shared copy's:
// in each the tester's task waits for that turn, as it always has, and goes when it ends.
func TestATesterTaskWaitsForNoFixTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		task  func(claim.Token) supervise.RequestDeliver
		waits bool
	}{
		{name: "the tester's task beside the fix's turn", task: fixTaskOf},
		{name: "control: the tester's task beside a turn of the implementer's task of no fix", waits: true,
			task: func(token claim.Token) supervise.RequestDeliver {
				return supervise.RequestDeliver{Claim: token, Task: "implement the plan", ID: "outbox:1", Phase: phase.Implementing, Generation: 1}
			}},
		{name: "control: the tester's task beside a turn of the implementer that serves no task", waits: true,
			task: func(claim.Token) supervise.RequestDeliver { return supervise.RequestDeliver{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newIssueRig(t)
			implementer := rig.inTurn(claim.RoleImplementer, tc.task(""))
			adoptions := rig.adoptions()
			conn := fake.NewConn()
			tester, waiting := rig.ready(claim.RoleTester, conn)

			if err := waiting.Handle(rig.ctx, supervise.RequestDeliver{Claim: tester, Task: "test the branch", ID: "outbox:2",
				Phase: phase.Testing, Generation: 1}); err != nil {
				t.Fatalf("deliver the tester's task: %v", err)
			}
			waiting.Wait()

			if tc.waits {
				rig.wantHeldUntilTheTurnEnds(conn, "test the branch", adoptions, implementer)
				return
			}
			rig.wantSentAtOnce(conn, "test the branch", adoptions, claim.RoleTester, runtime.Adoption{})
		})
	}
}

// The turn hold follows what an adoption rewrites, not which task it serves. A fix's task that also
// moves the shared working copy to a head (MoveTo) rewrites the shared copy, so it waits for the
// reviewer's turn there and goes when that turn ends, its adoption still naming the fix's head; and
// a fix's task that names no head of a copy of its own (no FixFrom) adopts the shared copy as every
// task of no fix does, so it waits too. A claim serving a task that moves the shared copy counts as
// in the shared copy, while its send is in flight and while its turn runs, so a tester's task waits
// for it. The control is a fix's task that names only its own copy's head: it is sent at once
// beside the reviewer's turn.
func TestAFixTaskThatMovesTheSharedCopyWaitsForTheReviewersTurn(t *testing.T) {
	t.Parallel()
	review := supervise.RequestDeliver{Task: "review the head", ID: "outbox:1", Phase: phase.Reviewing, Generation: 1}
	test := func(tester claim.Token) supervise.RequestDeliver {
		return supervise.RequestDeliver{Claim: tester, Task: "test the branch", ID: "outbox:2", Phase: phase.Testing, Generation: 1}
	}

	for _, tc := range []struct {
		name string
		task func(claim.Token) supervise.RequestDeliver
		want runtime.Adoption
	}{
		{name: "the fix's task that moves the shared copy", task: movingFixTaskOf,
			want: runtime.Adoption{FixFrom: copyFixHead, MoveTo: copyMoveHead}},
		{name: "a fix's task that names no copy of its own", want: runtime.Adoption{},
			task: func(token claim.Token) supervise.RequestDeliver {
				task := fixTaskOf(token)
				task.FixFrom = ""
				return task
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newIssueRig(t)
			reviewer := rig.inTurn(claim.RoleReviewer, review)
			adoptions := rig.adoptions()
			conn := fake.NewConn()
			implementer, fixing := rig.ready(claim.RoleImplementer, conn)

			if err := fixing.Handle(rig.ctx, tc.task(implementer)); err != nil {
				t.Fatalf("deliver the fix's task: %v", err)
			}
			fixing.Wait()

			rig.wantHeldUntilTheTurnEnds(conn, copyFixTask, adoptions, reviewer)
			made := rig.rt.CallsOf("AdoptWorkingCopy")
			if len(made) != adoptions+1 || made[adoptions].Adoption != tc.want || made[adoptions].Identity.Name != "legion-implementer[bot]" {
				t.Fatalf("adoptions = %+v, want the %d made before and one more, under the implementer's App, naming %+v", made, adoptions, tc.want)
			}
		})
	}

	t.Run("a tester's task beside the turn of a fix's task that moved the shared copy", func(t *testing.T) {
		t.Parallel()
		rig := newIssueRig(t)
		implementer := rig.inTurn(claim.RoleImplementer, movingFixTaskOf(""))
		adoptions := rig.adoptions()
		conn := fake.NewConn()
		tester, waiting := rig.ready(claim.RoleTester, conn)

		if err := waiting.Handle(rig.ctx, test(tester)); err != nil {
			t.Fatalf("deliver the tester's task: %v", err)
		}
		waiting.Wait()

		rig.wantHeldUntilTheTurnEnds(conn, "test the branch", adoptions, implementer)
	})

	t.Run("a tester's task beside the send in flight of a fix's task that moves the shared copy", func(t *testing.T) {
		t.Parallel()
		rig := newIssueRig(t)
		gated := newGatedPrompt()
		implementer, fixing := rig.ready(claim.RoleImplementer, gated)
		if err := fixing.Handle(rig.ctx, movingFixTaskOf(implementer)); err != nil {
			t.Fatalf("deliver the fix's task: %v", err)
		}
		// The fix's send is held inside its prompt: its adoption, which moved the shared copy, has
		// run, and its turn has not started.
		select {
		case <-gated.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the fix's send never reached its prompt")
		}
		adoptions := rig.adoptions()
		if view := fixing.View(); view.State != supervise.StateReady || !view.SendInFlight {
			t.Fatalf("the implementer is %s with its send in flight = %v, want it ready with a send in flight", view.State, view.SendInFlight)
		}
		conn := fake.NewConn()
		tester, waiting := rig.ready(claim.RoleTester, conn)

		if err := waiting.Handle(rig.ctx, test(tester)); err != nil {
			t.Fatalf("deliver the tester's task: %v", err)
		}
		waiting.Wait()
		if prompts := conn.Prompts(); len(prompts) != 0 {
			t.Fatalf("the tester was prompted %+v while the fix's send that moves the shared copy was in flight; want its task held", prompts)
		}

		// The fix's agent takes the prompt and its turn starts: the tester's task is held by the turn
		// now, and goes when it ends.
		close(gated.release)
		fixing.Wait()
		if err := fixing.Handle(rig.ctx, supervise.StreamTurnStart{Claim: implementer}); err != nil {
			t.Fatalf("the fix's turn start: %v", err)
		}
		rig.wantHeldUntilTheTurnEnds(conn, "test the branch", adoptions, implementer)
	})

	t.Run("control: the fix's task that names only its own copy's head", func(t *testing.T) {
		t.Parallel()
		rig := newIssueRig(t)
		rig.inTurn(claim.RoleReviewer, review)
		adoptions := rig.adoptions()
		conn := fake.NewConn()
		implementer, fixing := rig.ready(claim.RoleImplementer, conn)

		if err := fixing.Handle(rig.ctx, fixTaskOf(implementer)); err != nil {
			t.Fatalf("deliver the fix's task: %v", err)
		}
		fixing.Wait()

		rig.wantSentAtOnce(conn, copyFixTask, adoptions, claim.RoleImplementer, runtime.Adoption{FixFrom: copyFixHead})
	})
}
