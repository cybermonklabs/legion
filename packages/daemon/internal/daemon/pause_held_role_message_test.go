package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// R4 of dispatch://INFRA-3759: a resume launches each session the pause held something for,
// once, and what was held for it is delivered to it then. A role message of the daemon's own that
// the pause held for a role the pause stopped is such held work: the role was live when the pause
// stopped it, so without the pause it would have had the message. Main's rule that a role message
// to a released role is not worth a wake (outbox.roleMessage) is for a role an idle release took,
// not for one the pause stopped (the architect's decision on review of 7385a210).
//
// So after the resume the role is woken once, as the pause's wake with no release, at the wake
// pacing, and the message reaches it once its session holds its topic. The test models the Envoy
// listener as dispatch://INFRA-3759's rig does: a role's topic is held from the moment its launch
// is made, and where the daemon publishes the message to the stopped role before it launches it,
// the publish is refused and the listener reports the role's wake, which the test applies. It
// pins the outcome, not the path: either the daemon wakes the role for the row, or it publishes
// and the listener's report wakes it.
func TestARoleMessageThePauseHeldForARoleItStoppedIsDeliveredOnceAfterTheResume(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.boot()

	pause := r.pauseProject()
	testwait.Eventually(t, "the pause to stop the idle implementer", func() bool { return r.claim(worker).Released() })
	const body = "The checks of your head settled."
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: body})
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
		t.Fatalf("the role message was published %d times while the project was paused, want it held", len(sent))
	}
	if n := r.count(`select count(*) from outbox where id = $1`, message); n != 1 {
		t.Fatalf("the role message's row is gone while the project is paused, want it held")
	}

	before := len(r.rt.launches())
	r.resumeProject()
	// The role comes back for the message: the daemon launches it for the row, or the row's
	// publish is refused for the stopped role and the listener reports the role's wake.
	var reported bool
	testwait.WithinPolling(t, 2*time.Minute, 200*time.Millisecond,
		"the held role message to bring back the role the pause stopped (a launch of it, or a publish the listener refuses and reports)",
		func() bool {
			if len(r.rt.launches()) > before || len(r.state().WakesWaiting) > 0 {
				return true
			}
			var attempts int
			if err := r.pool.QueryRow(r.ctx, `select coalesce(max(attempts), 0) from outbox where id = $1`, message).Scan(&attempts); err != nil {
				t.Fatalf("read the role message's row: %v", err)
			}
			if attempts > 0 && !reported {
				reported = true
				r.applyFact("listener", "held-role-message-publish", intake.ListenerWake{Kind: intake.ListenerWakeRole,
					Role: string(worker), At: r.clock.Now(), Report: "report-of-the-held-role-message"})
			}
			return false
		})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
			r.envoy.hold(worker)
		}
	}, func(api.State) {})
	testwait.WithinPolling(t, 6*time.Minute, 200*time.Millisecond, "the held role message to reach the role after its launch", func() bool {
		return len(r.envoy.publishedTo(worker)) > 0 && r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	time.Sleep(time.Second)

	if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != body {
		t.Fatalf("the role message reached the role %d times (%+v), want once with its body", len(sent), sent)
	}
	if launched := len(r.rt.launchesOf(worker)); launched < 1 {
		t.Fatalf("the implementer was not launched after the resume")
	}
	var launchedAfter int
	for _, launch := range r.rt.launches()[before:] {
		if launch.claim == worker {
			launchedAfter++
		}
	}
	if launchedAfter != 1 {
		t.Fatalf("the implementer launched %d times after the resume, want once", launchedAfter)
	}
	if wakes := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer'`, issue); wakes != 1 {
		t.Fatalf("the implementer took %d wakes after the resume, want one", wakes)
	}
	var held, release int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'implementer'`,
		issue).Scan(&held, &release); err != nil {
		t.Fatalf("read the implementer's wake: %v", err)
	}
	if held != pause.ID || release != 0 {
		t.Fatalf("the implementer's wake is marked with pause %d and release %d, want pause %d and no release", held, release, pause.ID)
	}
}

// The boundary of the rule above: a role that an idle release took before the pause was not
// stopped by the pause, and main's rule for a role message to a released role still holds for it
// (outbox.roleMessage, cybermonklabs/legion#4088): the message the pause held finishes undelivered
// once the resume lets its row run, and launches nothing.
func TestARoleMessageThePauseHeldForARoleReleasedBeforeThePauseKeepsMainsRule(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.released(issue, claim.RoleImplementer))
	if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'implementer', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("record a release of %s's pod: %v", issue, err)
	}
	r.boot()

	r.pauseProject()
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: "The checks of your head settled."})
	time.Sleep(time.Second)
	if n := r.count(`select count(*) from outbox where id = $1`, message); n != 1 {
		t.Fatalf("the role message's row is gone while the project is paused, want it held")
	}
	before := len(r.rt.launches())
	r.resumeProject()
	testwait.Eventually(t, "the held role message's row to finish after the resume", func() bool {
		return r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	time.Sleep(2 * time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
		t.Fatalf("the role message was published %d times to a role released before the pause, want none", len(sent))
	}
	if launched := len(r.rt.launches()) - before; launched != 0 || len(r.state().WakesWaiting) != 0 {
		t.Fatalf("the held role message launched %d roles and left %d wakes waiting, want none", launched, len(r.state().WakesWaiting))
	}
	if wakes := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer'`, issue); wakes != 0 {
		t.Fatalf("the implementer took %d wakes, want none", wakes)
	}
}

// The gap between a woken role's ready and its role claim (R4 of dispatch://INFRA-3759). An agent
// says it is ready before its plugin claims the role's Envoy topic, which it does at the session's
// first turn or at the first-turn bound (supervise.SaidReady), and until then the listener answers
// a publish to the role as it answers the stopped session: 404 holder_lapsed gone
// (notify.ErrNoHolder). A role message the pause held for the role it stopped, whose row runs in
// that gap, still reaches the role once its session holds the topic. The test lets the row run once
// in the gap: the role ready, its topic not yet held.
func TestARoleMessageThePauseHeldForARoleItStoppedSurvivesTheGapBetweenItsReadyAndItsRoleClaim(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.envoy.gone(worker)
	r.boot()

	r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	const body = "The checks of your head settled."
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: body})
	attempts := heldRowAttempts(t, r, message)
	before := len(r.rt.launches())
	r.resumeProject()
	testwait.Eventually(t, "the held role message's first attempt after the resume", func() bool { return attempts() != 0 })
	r.applyFact("listener", "held-role-message-gone", intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(worker), At: r.clock.Now(), Report: "report-of-the-held-role-message"})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
		}
	}, func(api.State) {})
	atReady := attempts()
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "the row's next attempt after the role's ready, its topic not yet claimed", func() bool {
		n := attempts()
		return n == -1 || n > atReady
	})
	r.envoy.hold(worker)
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "the held role message's row to end", func() bool { return attempts() == -1 })
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != body {
		t.Fatalf("the held role message reached the role %d times once its row ran between the role's ready and its role claim, want once with its body", len(sent))
	}
}

// A second pause that begins before the role the first pause stopped is back (R4 of
// dispatch://INFRA-3759). The first resume marks the held role message with the first pause; its
// row runs, the listener answers gone, and the row waits for the role. The role is still released
// when the second pause begins, so that pause stops nothing of it, but the row falls due while the
// second pause stands and the second resume marks it with the second pause, while the role's
// newest stop is still the first pause's. The message still reaches the role once it is back.
func TestARoleMessageThePauseHeldForARoleItStoppedSurvivesASecondPauseBeforeTheRoleIsBack(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.envoy.gone(worker)
	r.boot()

	r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the first pause stops the idle implementer")
	const body = "The checks of your head settled."
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: body})
	attempts := heldRowAttempts(t, r, message)
	r.resumeProject()
	testwait.Eventually(t, "the held role message's first attempt after the first resume", func() bool { return attempts() != 0 })
	if attempts() == -1 {
		t.Fatalf("the held role message's row ended at its first attempt after the first resume, want it waiting for the role")
	}
	r.pauseProject()
	// The waiting row falls due while the second pause stands: on the wall clock the outbox writes
	// its next attempt by, and on the test clock the second resume reads it at.
	time.Sleep(5 * time.Second)
	r.clock.advance(2 * time.Minute)
	before := len(r.rt.launches())
	r.resumeProject()
	r.applyFact("listener", "held-role-message-gone-2", intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(worker), At: r.clock.Now(), Report: "report-of-the-held-role-message-2"})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
			r.envoy.hold(worker)
		}
	}, func(api.State) {})
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "the held role message's row to end", func() bool { return attempts() == -1 })
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != body {
		t.Fatalf("the held role message reached the role %d times across a second pause, want once with its body", len(sent))
	}
}

// heldRowAttempts reads the attempts of outbox row id, and -1 once the row has ended.
func heldRowAttempts(t *testing.T, r *pauseDaemonRig, id int64) func() int {
	return func() int {
		var n int
		if err := r.pool.QueryRow(r.ctx, `select coalesce(max(attempts), -1) from outbox where id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read outbox row %d: %v", id, err)
		}
		return n
	}
}
