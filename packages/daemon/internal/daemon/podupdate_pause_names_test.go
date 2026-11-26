package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// holdInRuntime is a hook for the fake runtime's calls (fake.Runtime's OnSpawn and OnResume). The
// hook stops each call inside the runtime until the test ends or the call's own context ends.
// reached receives once a call stops there. The test's cleanup lets the held call go. That
// cleanup runs ahead of the supervisor's stop, which waits for the held decision, because a test
// runs its cleanups in the reverse order: make the hold after the rig, which registers its stop.
func holdInRuntime(t *testing.T) (hold func(context.Context) error, reached <-chan struct{}) {
	t.Helper()
	let, holding := make(chan struct{}), make(chan struct{}, 1)
	t.Cleanup(func() { close(let) })
	return func(ctx context.Context) error {
		select {
		case holding <- struct{}{}:
		default:
		}
		select {
		case <-let:
		case <-ctx.Done():
		}
		return nil
	}, holding
}

// heldStartUntilHeld runs the held start of role (heldStartRequest) where holdInRuntime's hold
// stops the launch inside the runtime. The machine's handle of that start does not return while
// the hold stands, so the start runs on its own goroutine, and its error is not read. It waits for
// the launch to reach the runtime (reached, holdInRuntime's channel) until the test binary's
// deadline (testwait.Receive), so a launch a loaded machine makes slow is no failure.
func (r *podUpdateRig) heldStartUntilHeld(role claim.Role, reached <-chan struct{}) {
	r.t.Helper()
	m, _ := r.sup.Machine(r.tokens[role])
	start := r.heldStartRequest(role)
	go func() {
		_ = m.Handle(r.worker(), start)
	}()
	testwait.Receive(r.t, fmt.Sprintf("the %s's launch to reach the runtime", role), reached)
}

// scriptForgottenSession scripts the next launch of role to forget the role's session
// (supervise's startFresh). No session store holds the session, so the resume fails
// (runtime.ErrSessionFileMissing). The launch then forgets the session and spawns a fresh agent in
// the same try. The runtime refuses that spawn, which is a charged launch failure, and the next
// try spawns again. retry runs at the start of that spawn and of each spawn after it, as
// fake.Runtime.OnSpawn does: holdInRuntime's hold keeps the launch in the runtime with no session
// and no process. With a nil retry, those spawns take the runtime's next scripted answers.
func (r *podUpdateRig) scriptForgottenSession(role claim.Role, retry func(context.Context) error) {
	r.t.Helper()
	r.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session",
		r.tokens[role], runtime.ErrSessionFileMissing)})
	r.rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("the pod's launcher refused the start")})
	if retry == nil {
		return
	}
	var spawns atomic.Int32
	r.rt.OnSpawn = func(ctx context.Context) error {
		if spawns.Add(1) == 1 {
			return nil
		}
		return retry(ctx)
	}
}

// forgottenOwedLaunch is a rig of its own, taken to the shape of the case "with its session
// forgotten" of TestALiveRoundKeepsThePodForTheLaunchAPauseOwesWhetherOrNotItKeepsItsSession. A
// project's pause stopped the implementer, suspended released, and its resume ended the hold of
// the pod a minute before the round opened (recordPause), so the pause still owes the implementer
// a launch (podHolds.owes). The tester runs in the pod. others are more members of the round, as
// the rig makes them. The round opens over the pod and asks the tester. The implementer's held
// start runs, and its launch forgets the role's session (scriptForgottenSession): the retry of the
// fresh spawn stops inside the runtime until the test ends (holdInRuntime). The tester answers.
// The rig is answered with the implementer launching, with no session and no process
// (wantForgottenLaunch), and with no machine of the tester in a decision.
func forgottenOwedLaunch(t *testing.T, others ...podUpdateRole) *podUpdateRig {
	t.Helper()
	const stopped = pauseStoppedRole
	members := append([]podUpdateRole{suspendedRole(stopped, supervise.SuspendedByReleased), readyWorker(claim.RoleTester)}, others...)
	rig := newPodUpdateRig(t, members...)
	rig.recordPause(store.PausePodHoldResumed, stopped)
	rig.drifts(podUpdateDrift)
	hold, holding := holdInRuntime(t)
	rig.scriptForgottenSession(stopped, hold)

	rig.tick()
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.awaitRound(stopped, podUpdateRound)
	rig.heldStartUntilHeld(stopped, holding)
	rig.wantForgottenLaunch()

	rig.signal(claim.RoleTester, "a1b2c3d")
	tester, _ := rig.sup.Machine(rig.tokens[claim.RoleTester])
	tester.Wait()
	testwait.Eventually(t, "the tester to be deciding nothing", func() bool { return tester.Deciding() == nil })
	return rig
}

// wantForgottenLaunch holds the implementer, the role the pause stopped (forgottenOwedLaunch), to
// a member of the round that is launching with no session and no process: its launch forgot its
// session, and the retry of the fresh spawn stops inside the runtime.
func (r *podUpdateRig) wantForgottenLaunch() {
	r.t.Helper()
	m, _ := r.sup.Machine(r.tokens[pauseStoppedRole])
	if c := m.View().Claim; c.State != supervise.StateLaunching || c.Locator != nil || c.Session != "" || c.UpdateRound != podUpdateRound {
		r.t.Fatalf("the %s is %s recording session %q, process %+v and round %q, want it launching with no session and no process in round %s",
			pauseStoppedRole, c.State, c.Session, c.Locator, c.UpdateRound, podUpdateRound)
	}
}

// dispatch://INFRA-4109 spec v19 R8: a claim that a project's pause still keeps holds the round
// whether or not it records a session, and the round names it where the round ends without its
// answer. Each case starts from forgottenOwedLaunch: the implementer is launching with no session
// and no process, the pause still owes it a launch, and the tester answered and runs in the pod.
// The round asks no claim with no session (roundAsks), so the round's two sets do not hold the
// implementer. What names it is the set of claims that have not answered (unansweredClaims), or
// the members that a gone pod took work from (lostClaims).
func TestALiveRoundNamesTheLaunchAPauseOwesThatForgotItsSession(t *testing.T) {
	t.Parallel()
	// The bound ends the wait for the implementer. The replacement through the tester's process is
	// made at the bound, and the bound's own line names the implementer as a claim that did not
	// answer. Read through the round's sets alone, the implementer holds nothing, so the answers
	// seem to end the round: no line of a bounded end is written, and the line of the replacement
	// says that no bound made it.
	t.Run("past the bound the replacement names it as unanswered", func(t *testing.T) {
		rig := forgottenOwedLaunch(t)
		rig.clockAt(podUpdateBound)
		rig.awaitReplacement(1, "through the tester's process at the bound")
		rig.wantForgottenLaunch()

		bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
		if len(bounded) != 1 {
			t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
		}
		wantField(t, bounded[0], "answered", claim.RoleTester)
		wantField(t, bounded[0], "unanswered", pauseStoppedRole)
		if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != true {
			t.Errorf("the replacement was logged %+v, want one line that says the bound made it", replaced)
		}
	})

	// The pod goes for a failure of its own before the round is over. The first sweep that reads
	// it gone ends the round and writes one message on the issue. The implementer is a member of
	// the round that did not answer: the round was posted to it while it had a session, and its
	// launch forgot that session after the post. It had a worker, whose unpushed work was on the
	// pod's disk, so the message names it, and it names the tester, which answered, nowhere. Read
	// over the members with a session alone, the only member left is the tester, which answered,
	// so the round ends with nothing lost and the issue is told nothing.
	t.Run("a pod that goes names it as a member that did not answer", func(t *testing.T) {
		rig := forgottenOwedLaunch(t)
		rig.podGone()
		for range 3 {
			rig.tick()
		}
		rig.wantForgottenLaunch()

		messages := rig.messages()
		if len(messages) != 1 {
			t.Fatalf("the issue was written on %d times over three sweeps, want once for the pod that went: %q", len(messages), messages)
		}
		if !strings.Contains(messages[0], string(pauseStoppedRole)) || strings.Contains(messages[0], string(claim.RoleTester)) {
			t.Errorf("the message does not name the %s alone, the one member that had not answered: %s", pauseStoppedRole, messages[0])
		}
		if calls := rig.replacements(); len(calls) != 0 {
			t.Errorf("the pod was replaced %d times, want none: it went before the round was over", len(calls))
		}
	})
}

// dispatch://INFRA-4109 review round 6 (spec R1 and R8): past the bound, a round that passed over
// a claim reads the issue's workspace beside a launch that a project's pause still owes and that
// has not come up. The members are those of forgottenOwedLaunch and a reviewer that the daemon
// suspended before it answered: the implementer is launching with no session and no process, the
// tester answered and runs in the pod, and the round passes over the reviewer, which keeps its
// session. The read of the workspace finds nothing. Before the bound the implementer
// holds the replacement (R8). At the bound the pod is replaced once, through the tester's process.
// The replacement reads the workspace before it is made (R7 of #5285), past the
// bound too, and neither the reviewer the round passed over nor the implementer whose launch the
// pause owes stops the read: the replacement's line names the read the tester made and the reviewer
// the round did not wait for. R8 keeps the pod for the implementer before the bound only, so the
// bound's line names the implementer as unanswered. The read here finds nothing, so nobody is told
// (a read that finds work keeps the pod: round_unread_disk_test.go).
func TestALiveRoundReadsTheWorkspaceBesideTheLaunchAPauseOwesThatForgotItsSession(t *testing.T) {
	t.Parallel()
	const reviewer = claim.RoleReviewer
	rig := forgottenOwedLaunch(t, suspendedRole(reviewer, supervise.SuspendedByDaemon))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead})
	rig.awaitRound(reviewer, podUpdateRound)
	for range 3 {
		rig.tick()
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times before the bound, while the project's pause owes the %s a launch", len(calls), pauseStoppedRole)
	}

	rig.clockAt(podUpdateBound)
	replaced := rig.awaitReplacement(1, "through the tester's process at the bound")
	rig.wantForgottenLaunch()
	if through := replaced[0].Pod.Claim; through != rig.tokens[claim.RoleTester] {
		t.Fatalf("the pod was replaced through %s, want the tester's claim, the one whose process runs in the pod", through)
	}

	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	if lines[0]["workspaceRead"] != "found no work" || lines[0]["workspaceReadBy"] != string(claim.RoleTester) || lines[0]["workspaceHead"] != passedOverHead {
		t.Fatalf("the replacement's line names the read %+v, want one made through the tester that found nothing at %s", lines[0], passedOverHead)
	}
	wantField(t, lines[0], "notWaitedFor", reviewer)
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the read found nothing", messages)
	}

	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	wantField(t, bounded[0], "unanswered", pauseStoppedRole)
}
