package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// forgottenOwedLaunch is a rig of its own, taken to the shape of the case "with its session
// forgotten" of TestALiveRoundKeepsThePodForTheLaunchAPauseOwesWhetherOrNotItKeepsItsSession. A
// project's pause stopped the implementer, suspended released, and its resume ended the hold of
// the pod a minute before the round opened (recordPause), so the pause still owes the implementer
// a launch (podHolds.owes). The tester runs in the pod. The round opens over the pod and asks the
// tester. The implementer's held start runs. No session store holds its session, so the launch
// forgets the session and spawns a fresh agent in the same try (supervise's startFresh). The
// runtime refuses that spawn, and the retry of the spawn stops inside the runtime until the test
// ends. The tester answers. The rig is answered with the implementer launching, with no session
// and no process (wantForgottenLaunch), and with no machine of the tester in a decision.
func forgottenOwedLaunch(t *testing.T) *podUpdateRig {
	t.Helper()
	const stopped = pauseStoppedRole
	rig := newPodUpdateRig(t, suspendedRole(stopped, supervise.SuspendedByReleased), readyWorker(claim.RoleTester))
	rig.recordPause(store.PausePodHoldResumed, stopped)
	rig.drifts(podUpdateDrift)
	token := rig.tokens[stopped]
	rig.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session",
		token, runtime.ErrSessionFileMissing)})
	rig.rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("the pod's launcher refused the start")})
	let, holding := make(chan struct{}), make(chan struct{}, 1)
	var spawns atomic.Int32
	rig.rt.OnSpawn = func(ctx context.Context) error {
		if spawns.Add(1) == 1 {
			return nil
		}
		holding <- struct{}{}
		select {
		case <-let:
		case <-ctx.Done():
		}
		return nil
	}
	// The held spawn is let go ahead of the supervisor's stop, which waits for the held decision.
	t.Cleanup(func() { close(let) })

	rig.tick()
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.awaitRound(stopped, podUpdateRound)
	m, _ := rig.sup.Machine(token)
	go func() {
		_ = m.Handle(rig.ctx, supervise.RequestRuleResume{Claim: token, Reason: "a wake for the launch the project's pause held"})
	}()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatalf("the %s's launch did not reach the runtime", stopped)
	}
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
