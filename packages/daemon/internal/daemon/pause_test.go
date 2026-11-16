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

// A claim of another project's issue is an operator's spawn the daemon keeps no issue record of,
// so the resume launches it only for the work the pause held for it, as it launches every such
// spawn (resumeOwedSpawns), and only through the wake pacing: one the pause met launching with its
// task untaken is stopped owing a launch and launched once after the resume, and one the pause
// found ready with nothing held is stopped owing nothing and stays stopped (R4 of
// dispatch://INFRA-3759: each other stopped session stays stopped until its next wake).
func TestTheResumeLaunchesAClaimOfAnotherProjectsIssueOnlyForTheWorkThePauseHeld(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	r.boot()
	const task = "Summarize the incident for the operator."
	owed := r.d.spawn(api.SpawnRequest{Tree: "OTHERPROJECT-7", Issue: "OTHERPROJECT-7", Role: claim.RoleArchitect,
		Prompt: "Reply ready and wait.", Task: task})
	idle := r.d.spawn(api.SpawnRequest{Tree: "OTHERPROJECT-8", Issue: "OTHERPROJECT-8", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	testwait.Eventually(t, "the idle spawn to launch", func() bool { return len(r.rt.launchesOf(idle)) == 1 })
	up := func(token claim.Token, session string) {
		t.Helper()
		launched := r.claim(token)
		r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
		r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
			SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion})
		r.handle(token, supervise.RequestReady{Claim: token, Generation: launched.Generation, Session: session})
	}
	up(idle, "ses-other-idle")
	testwait.Eventually(t, "the idle spawn's agent to be ready", func() bool {
		state := r.claim(idle).State
		return state == supervise.StateReady || state == supervise.StateIdle
	})

	// The pause stops the ready spawn at once, and holds the stop of the launching one until its
	// agent is ready, before its task is sent.
	pause := r.pauseProject()
	r.awaitState(idle, supervise.StateSuspended, "the pause stops the ready spawn of another project's issue")
	testwait.Eventually(t, "the pause's stop of the launching spawn to be held until its agent is ready", func() bool {
		return r.claim(owed).SuspensionHeld
	})
	up(owed, "ses-other-owed")
	r.awaitState(owed, supervise.StateSuspended, "the held stop runs at the agent's ready")
	for token, launchOwed := range map[claim.Token]bool{owed: true, idle: false} {
		if got := r.count(`select count(*) from project_pause_stops where pause_id = $1 and claim = $2 and ended_as = 'stopped' and launch_owed = $3`,
			pause.ID, string(token), launchOwed); got != 1 {
			t.Fatalf("the stops of %s recorded stopped owing a launch %t = %d, want 1", token, launchOwed, got)
		}
	}

	owedBefore, idleBefore := len(r.rt.launchesOf(owed)), len(r.rt.launchesOf(idle))
	r.resumeProject()
	if !testwait.Holds(testwait.Bounded(t, 20*time.Second, time.Second), testwait.Poll, func() bool {
		if len(r.rt.launchesOf(owed)) > owedBefore {
			return true
		}
		for _, wake := range r.state().WakesWaiting {
			if wake.Claim == owed {
				r.clock.advanceTo(wake.DueAt)
			}
		}
		return false
	}) {
		c := r.claim(owed)
		t.Fatalf("after the resume the spawn owed a launch is %s by %s; want it launched once through the pacing", c.State, c.SuspendedBy)
	}
	if lines := r.linesOf(supervise.WakeRanMessage, owed); len(lines) == 0 || lines[len(lines)-1]["paced"] != true {
		t.Fatalf("the owed spawn's launch after the resume wrote %v, want a wake line that says paced", lines)
	}
	time.Sleep(time.Second)
	if got := len(r.rt.launchesOf(owed)) - owedBefore; got != 1 {
		t.Fatalf("the spawn owed a launch was launched %d times after the resume, want once", got)
	}
	if c := r.claim(idle); c.State != supervise.StateSuspended || len(r.rt.launchesOf(idle)) != idleBefore {
		t.Fatalf("after the resume the spawn owed nothing is %s, launched %d times since its stop; want it stopped and not launched",
			c.State, len(r.rt.launchesOf(idle))-idleBefore)
	}
	for _, wake := range r.state().WakesWaiting {
		if wake.Claim == idle {
			t.Fatalf("the spawn owed nothing waits in the pacing for %+v, want no launch of it", wake)
		}
	}
}
