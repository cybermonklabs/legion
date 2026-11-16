package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// An operator's spawn — a claim of an issue the daemon keeps no record of — that the pause stops
// owing a launch is launched by the resume itself, once, through the wake pacing
// (projectPause.Resumed). No start row can carry its launch, since the outbox's start reads the
// issue's record, and no wake reaches an issue with no record, so without the resume's own launch
// the claim would stay suspended until an operator resumed it (R4 of dispatch://INFRA-3759: a
// resume launches each session that the pause held something for, once). The spawn's task, which
// its agent never took, is delivered at the launch's ready.
func TestTheResumeLaunchesAnOperatorsSpawnThePauseStoppedOwingALaunch(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	r.boot()
	issue := r.issue(7)
	const task = "Summarize the incident for the operator."
	token := r.d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait.", Task: task})
	if got := r.count(`select count(*) from issues where key = $1`, issue); got != 0 {
		t.Fatalf("records of %s = %d, want none: the operator's spawn is of an issue the daemon keeps no record of", issue, got)
	}

	// The pause meets the spawn launching and holds its stop until the agent is ready; the stop then
	// runs at the ready, before the task is sent, and owes the role the launch that delivers it.
	pause := r.pauseProject()
	testwait.Eventually(t, "the pause's stop of the spawn to be held until its agent is ready", func() bool {
		return r.claim(token).SuspensionHeld
	})
	launched := r.claim(token)
	session := "ses-operator-spawn"
	r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: launched.Generation, Session: session})
	stopped := r.awaitState(token, supervise.StateSuspended, "the held stop runs at the agent's ready")
	if !stopped.Released() || stopped.Pending == nil || stopped.Pending.Task != task || !stopped.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the stopped spawn is %s by %s holding %+v, want it released and holding its task unconfirmed",
			stopped.State, stopped.SuspendedBy, stopped.Pending)
	}
	if got := r.count(`select count(*) from project_pause_stops where pause_id = $1 and claim = $2 and ended_as = 'stopped' and launch_owed`,
		pause.ID, string(token)); got != 1 {
		t.Fatalf("the spawn's stops recorded stopped owing a launch = %d, want 1", got)
	}

	before := len(r.rt.launchesOf(token))
	r.resumeProject()
	if !testwait.Holds(testwait.Bounded(t, 20*time.Second, time.Second), testwait.Poll, func() bool {
		if len(r.rt.launchesOf(token)) > before {
			return true
		}
		for _, wake := range r.state().WakesWaiting {
			if wake.Claim == token {
				r.clock.advanceTo(wake.DueAt)
			}
		}
		return false
	}) {
		c := r.claim(token)
		t.Fatalf("after the resume the operator's spawn is %s by %s, launched %d times since its stop; want it launched once through the pacing",
			c.State, c.SuspendedBy, len(r.rt.launchesOf(token))-before)
	}
	if lines := r.linesOf(supervise.WakeRanMessage, token); len(lines) == 0 || lines[len(lines)-1]["paced"] != true {
		t.Fatalf("the spawn's launch after the resume wrote %v, want a wake line that says paced", lines)
	}

	// Its task is delivered once its agent, on the session it kept, is ready again.
	relaunched := r.claim(token)
	sh := r.dialShim(lastLaunch(t, r.rt.Runtime, token).BootToken)
	testwait.Eventually(t, "the relaunched spawn's shim to say hello", func() bool {
		return r.claim(token).State == supervise.StateShimConnected
	})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: relaunched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: relaunched.Generation, Session: session})
	r.startTurn(sh, task)
	r.awaitState(token, supervise.StateWorking, "the spawn takes its task in a turn")
	if got := len(r.rt.launchesOf(token)) - before; got != 1 {
		t.Fatalf("the spawn was launched %d times after the resume, want once", got)
	}
}
