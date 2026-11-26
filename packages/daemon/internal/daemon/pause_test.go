package daemon

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// An operator's spawn — a claim of an issue the daemon keeps no record of — that the pause stops
// owing a launch is launched by the resume itself, once, through the wake pacing
// (projectPause.Resumed). No start row can carry its launch, since the outbox's start reads the
// issue's record, and no wake reaches an issue with no record, so without the resume's own launch
// the claim would stay suspended until an operator resumed it (R4 of dispatch://INFRA-3759: a
// resume launches each session that the pause held something for, once). The spawn's task, which
// its agent never took, is delivered at the launch's ready.
func TestTheResumeLaunchesAnOperatorsSpawnThePauseStoppedOwingALaunch(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	r.boot()
	issue := r.issue(7)
	const task = "Summarize the incident for the operator."
	// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
	r.board.place(issue, "todo", "", false)
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
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
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
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
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
//
// The same holds where the database the daemon shares with another project's daemon holds that
// daemon's record of the issue (the issues table holds every project's issues, under its
// project). That record is not this daemon's (projectRecords.Issue): a start row built from it
// would be keyed by an issue whose rows the other project's daemon runs, so the stop queues none,
// and the resume finds the spawn as one with no record of its own and launches it. A message to
// the spawn the resume left stopped wakes it no more than a message to any operator's spawn does
// (workflow's wakeReleased): it takes no resume of the role, which would refuse every later wake
// of it, and queues no start keyed by the other project's issue.
func TestTheResumeLaunchesAClaimOfAnotherProjectsIssueOnlyForTheWorkThePauseHeld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// recorded seeds another project's record of each spawned issue.
		recorded bool
	}{
		{name: "no record of the issues"},
		{name: "another project's daemon records the issues", recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseDaemonRig(t, nil)
			r.boot()
			const owedIssue, idleIssue = "OTHERPROJECT-7", "OTHERPROJECT-8"
			if tc.recorded {
				for _, key := range []string{owedIssue, idleIssue} {
					recorded := r.root(key, phase.Planning)
					recorded.Project = "OTHERPROJECT"
					if err := store.Transact(r.ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(r.ctx, tx, recorded) }); err != nil {
						t.Fatalf("record %s as the OTHERPROJECT daemon's issue: %v", key, err)
					}
				}
			}
			startsOf := func(issue string) int {
				t.Helper()
				return r.count(`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, issue)
			}
			const task = "Summarize the incident for the operator."
			// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
			r.board.place(owedIssue, "todo", "", false)
			r.board.place(idleIssue, "todo", "", false)
			owed := r.d.spawn(api.SpawnRequest{Tree: owedIssue, Issue: owedIssue, Role: claim.RoleArchitect,
				Prompt: "Reply ready and wait.", Task: task})
			idle := r.d.spawn(api.SpawnRequest{Tree: idleIssue, Issue: idleIssue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
			testwait.Eventually(t, "the idle spawn to launch", func() bool { return len(r.rt.launchesOf(idle)) == 1 })
			up := func(token claim.Token, session string) {
				t.Helper()
				launched := r.claim(token)
				r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
				r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
					SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
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
			if got := startsOf(owedIssue); got != 0 {
				t.Errorf("start rows keyed %s after the stop = %d, want none: this daemon's outbox runs no row of another project's issue", owedIssue, got)
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
			if got := startsOf(owedIssue); got != 0 {
				t.Fatalf("start rows keyed %s after the resume = %d, want none", owedIssue, got)
			}

			// A message to the stopped spawn's session, and one to its role token, each reach the
			// listener's wake, which finds the claim released under this daemon's token.
			idleRows := r.count(`select count(*) from outbox where issue = $1`, idleIssue)
			for n, wake := range []intake.ListenerWake{
				{Kind: intake.ListenerWakeAgent, Session: "ses-other-idle"},
				{Kind: intake.ListenerWakeRole, Role: string(idle)},
			} {
				wake.At, wake.Report = r.clock.Now(), fmt.Sprintf("report-idle-%d", n)
				r.applyFact("listener", fmt.Sprintf("wake-idle-%d", n), wake)
			}
			if got := r.count(`select count(*) from outbox where issue = $1`, idleIssue) - idleRows; got != 0 {
				t.Errorf("the message wakes of the stopped spawn wrote %d outbox rows keyed %s, want none: this daemon's outbox runs no row of another project's issue",
					got, idleIssue)
			}
			if got := r.count(`select count(*) from pod_resumes where project = $1 and issue = $2`, r.project, idleIssue); got != 0 {
				t.Errorf("the message wakes of the stopped spawn recorded %d resumes of it, want none", got)
			}
			if c := r.claim(idle); c.State != supervise.StateSuspended {
				t.Errorf("after its message wakes the spawn owed nothing is %s, want it still stopped", c.State)
			}
		})
	}
}

// An update round's bound counts the pause's hold of its pod on to the let-out of the last launch
// the pause held for a role of the pod (decision 12 of dispatch://INFRA-3759, and the let-out of
// decision 14): a role whose launch waits its turn in the wake pacing can answer no round, so a
// round that counted from the hold's end would replace the pod, and delete its disk with that
// role's unpushed work, before the role could take a turn. Pod P holds an idle worker and an
// architect in a turn, both in a round opened ten minutes before the pause, five minutes short of
// the rig's grace; 84 other issues hold an idle worker each. The pause stops every worker and
// holds P's pod, while P's architect runs its turn on in it. Each other worker is given a held
// wake (a listener's report, as k2's are). P's worker is given one too, or, in the second and the
// third case, a hook's prompt the pause holds, which wakes it only when the row runs after the
// resume (wakeForDelivery) and is marked with the pause all the same: the resume marks every row
// its pause holds (record.OutboxRow.HeldByPause, store.Store.TakePodWake), the third case's row,
// already due when the pause began, as well as the second's. P's row is held back, under a lease
// of the test's, until every other worker has reserved its turn, so the pacing lets P's worker
// out last, 84 turns of 7.5 seconds after the resume. The others' wakes carry a role's message, so
// they wait in the pacing's first queue (dispatch://INFRA-5682 R1), and so does P's held wake. A
// hook's prompt carries none, and its launch would wait in the second queue and be let out at
// that queue's first turn, three turns in: so in those two cases the listener reports a message
// for P's worker once its launch waits, which moves it to the back of the first queue, behind
// every other worker's. The resume ends P's hold, since P's architect is live in the pod.
//
// The round's pod keeps a process through P's architect, whose turn runs through the pause, as
// k4's pod 6 does: a role the rig relaunches runs in no pod (the fake runtime records a tmux pane),
// so a round whose only process in its pod was a relaunched role would wait for a process and
// replace nothing.
//
// Before the fix the round's bound fell five minutes after the hold's end, and P's pod was
// replaced while P's worker still waited in the pacing; and before the resume marked the rows its
// pause held, so did the third case's, whose wake read the pause that stood at the row's due time
// and found none.
func TestARoundsBoundCountsTheHeldPodOnToTheLetOutOfItsLastHeldLaunch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// hook is whether P's worker is woken by a held hook's prompt rather than a held wake.
		hook bool
		// dueBefore is whether that hook's prompt was already due when the pause began.
		dueBefore bool
	}{
		{name: "a held wake"},
		{name: "a held hook prompt", hook: true},
		{name: "a held hook prompt due before the pause", hook: true, dueBefore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const others = 84
			r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.AdmissionCap = others + 10 })
			// The update rounds' sweep runs beside the machines (overrides.orphanSweep), reading the
			// test clock at each pass. Each pass asks the fake runtime of every pod, which records each
			// call, so a second keeps that record small over 85 pods.
			r.o.orphanSweep = time.Second
			P := r.issue(1)
			worker := func(issue string) claim.Token { return r.token(issue, claim.RoleImplementer) }
			pausedAt := r.clock.Now().UTC()
			opened := pausedAt.Add(-10 * time.Minute)
			left := opened.Add(pauseRigPodUpdateGrace).Sub(pausedAt)
			r.seed(r.root(P, phase.Implementing), inPodRound(r.running(P, claim.RoleImplementer, supervise.StateIdle), opened),
				inPodRound(r.running(P, claim.RoleArchitect, supervise.StateWorking), opened))
			for n := 2; n <= others+1; n++ {
				r.seed(r.root(r.issue(n), phase.Implementing), r.running(r.issue(n), claim.RoleImplementer, supervise.StateIdle))
			}
			r.boot()

			pause := r.pauseProject()
			testwait.WithinPolling(t, 3*time.Minute, 100*time.Millisecond, "the pause to stop every worker", func() bool {
				return r.count(`select count(*) from claims where project = $1 and role = 'implementer' and state = 'suspended' and suspended_by = 'released'`,
					r.project) == others+1
			})
			if !r.openPodHold(P) {
				t.Fatal("no pause holds P's pod once the pause stopped its worker")
			}
			for n := 2; n <= others+1; n++ {
				r.applyFact("listener", fmt.Sprintf("wake-%d", n), intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker(r.issue(n))),
					At: r.clock.Now(), Report: fmt.Sprintf("report-%d", n)})
			}
			// The resume makes every held row claimable at once, and their lanes race for the
			// pacing's turns: P's row is leased to nobody, so it runs only once the test lets the
			// lease lapse.
			var held int64
			holdUntil := r.later().Add(time.Hour)
			if tc.hook {
				held = r.enqueue(P, record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing,
					To: phase.Implementing, Count: 1, Counts: map[string]int{phase.Pair(phase.Reviewing, phase.Implementing): 1}, Prompt: "Read the review."})
				if tc.dueBefore {
					if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = $2 where id = $1`, held, pausedAt.Add(-time.Hour)); err != nil {
						t.Fatalf("make P's hook prompt due before the pause: %v", err)
					}
				}
				// A row under a live lease is no row the pause holds, and the resume marks the rows
				// its pause holds on the daemon's clock, the test clock, where the lanes read a lease
				// on the wall clock. So the test clock is moved past the lease: the resume finds P's
				// row unleased and held, and marks it with the pause, as it marks every row its pause
				// holds (record.Postgres.MarkPauseHeldRows), while the lanes find it leased.
				wall := time.Now().UTC()
				holdUntil = wall.Add(5 * time.Minute)
				r.clock.advanceTo(wall.Add(10 * time.Minute))
			} else {
				r.applyFact("listener", "wake-1", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker(P)),
					At: r.clock.Now(), Report: "report-1"})
				if err := r.pool.QueryRow(r.ctx, `select id from outbox where kind = 'supervise' and issue = $1 and payload->>'op' = 'start'
					and payload->>'role' = 'implementer'`, P).Scan(&held); err != nil {
					t.Fatalf("read the held start of P's worker: %v", err)
				}
			}
			if _, err := r.pool.Exec(r.ctx, `update outbox set lease_token = 'held back by the test', lease_until = $2 where id = $1`,
				held, holdUntil); err != nil {
				t.Fatalf("hold P's row back: %v", err)
			}

			before, resumedAt := len(r.rt.launches()), r.clock.Now()
			r.resumeProject()
			testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "every other worker's launch to reserve its turn", func() bool {
				return len(r.rt.launches())-before+len(r.state().WakesWaiting) == others
			})
			if got := r.count(`select count(*) from project_pause_pod_holds where issue = $1 and ended_why = 'resumed'`, P); got != 1 {
				t.Fatalf("P's holds the resume ended = %d, want the one the pause put on it, ended because P's architect is live", got)
			}
			if _, err := r.pool.Exec(r.ctx, `update outbox set lease_until = $2 where id = $1`, held, r.dueNow()); err != nil {
				t.Fatalf("let P's row's lease lapse: %v", err)
			}
			// The outbox's sweep, every pauseRigOutboxSweep of the wall clock, wakes P's lane.
			testwait.WithinPolling(t, 2*time.Minute, 200*time.Millisecond, "P's worker's launch to reserve the last turn", func() bool {
				return len(r.rt.launches())-before+len(r.state().WakesWaiting) == others+1
			})
			if tc.hook {
				r.applyFact("listener", "wake-1", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker(P)),
					At: r.clock.Now(), Report: "report-1"})
				testwait.WithinPolling(t, time.Minute, 100*time.Millisecond, "the message to move P's worker's launch behind every other worker's", func() bool {
					waiting := r.state().WakesWaiting
					last := slices.MaxFunc(waiting, func(a, b api.WakeWaiting) int { return a.DueAt.Compare(b.DueAt) })
					return len(waiting) > 1 && last.Claim == worker(P)
				})
			}

			var letOut time.Time
			r.runPacing(before, others+1, func(launch waveLaunch) {
				if launch.claim == worker(P) {
					letOut = launch.at
					// Every sweep that read the clock before this turn has run.
					r.awaitFullSweep()
				}
				if replaced := r.replacedPods(P); replaced != 0 {
					t.Fatalf("P's pod was replaced %d times by %s after the resume, at the launch of %s, while P's worker had not yet been let out of the pacing",
						replaced, r.clock.Now().Sub(resumedAt), launch.claim)
				}
			}, func(api.State) {})
			if got := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer' and pause_id = $2`, P, pause.ID); got != 1 {
				t.Fatalf("P's worker's resumes held by pause %d = %d, want its wake marked with the pause", pause.ID, got)
			}
			if !letOut.After(resumedAt.Add(left)) {
				t.Fatalf("P's worker was let out %s after the resume, want it past the %s the round had left of its grace at the pause",
					letOut.Sub(resumedAt), left)
			}
			if c := r.claim(r.token(P, claim.RoleArchitect)); !c.InPodRound() {
				t.Fatalf("P's architect is in round %q once P's worker was let out, want the round still open", c.UpdateRound)
			}

			// The roles get what was left of their grace once the pacing let P's worker out.
			r.clock.advanceTo(letOut.Add(left - time.Minute))
			r.awaitFullSweep()
			if replaced := r.replacedPods(P); replaced != 0 {
				t.Fatalf("P's pod was replaced %d times a minute before the bound moved to the let-out, want none", replaced)
			}
			r.clock.advanceTo(letOut.Add(left))
			testwait.Eventually(t, "the replacement of P's pod at the bound the let-out moved", func() bool { return r.replacedPods(P) == 1 })
		})
	}
}

// A wake a pause held is marked with its pause (decision 15 of dispatch://INFRA-3759), and so is
// the wake of a delivery the pause held. A notice for an architect the pause stopped waits as its
// outbox row, and wakes the architect only when it runs after the resume (wakeForDelivery). The
// pause holds the row whatever its due time, and the resume marks it with the pause
// (record.OutboxRow.HeldByPause), so the wake is a held wake of that pause, which pairs with no
// release (store.Store.TakePodWake), as a wake the workflow takes while the project is paused is:
// for a notice that fell due while the pause stood, and for one already due when the pause began,
// the backlog of an outage. Before the first fix the wake was recorded as the wake of the issue's
// latest release, with no pause, and the release records counted the pause's wait as that
// release's; before the second, the wake of a notice due before the pause still was, since the
// wake read the pause that stood at the row's due time.
func TestANoticeThePauseHeldWakesItsArchitectWithThatPause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// dueBefore is how long before the pause began the notice was due, 0 for a notice that fell
		// due while the pause stood.
		dueBefore time.Duration
	}{
		{name: "a notice due while the pause stood"},
		{name: "a notice already due when the pause began", dueBefore: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseDaemonRig(t, nil)
			issue := r.issue(1)
			architect := r.token(issue, claim.RoleArchitect)
			r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleArchitect, supervise.StateIdle))
			// A release of the issue's pod from before the pause, which a wake no pause held would
			// pair with.
			if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
				values ($1, $2, 1, 'architect', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
				t.Fatalf("record a release of %s's pod: %v", issue, err)
			}
			r.boot()

			pause := r.pauseProject()
			r.awaitState(architect, supervise.StateSuspended, "the pause stops the idle architect")
			notice := r.enqueue(issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"})
			if tc.dueBefore > 0 {
				if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = $2 where id = $1`, notice, pause.Since.Add(-tc.dueBefore)); err != nil {
					t.Fatalf("make the notice due before the pause: %v", err)
				}
			}
			r.resumeProject()
			testwait.Eventually(t, "the held notice to wake its architect", func() bool {
				return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'architect' and kind = $2`, issue, pods.WakeDelivery) == 1
			})
			var held *int64
			var release int64
			if err := r.pool.QueryRow(r.ctx, `select pause_id, release_id from pod_resumes where issue = $1 and role = 'architect' and kind = $2`,
				issue, pods.WakeDelivery).Scan(&held, &release); err != nil {
				t.Fatalf("read the notice's wake: %v", err)
			}
			if held == nil || *held != pause.ID || release != 0 {
				t.Fatalf("the held notice's wake is marked with pause %v and release %d, want pause %d and no release", held, release, pause.ID)
			}
		})
	}
}

// A role the pause stopped owing nothing stays stopped after the resume until its next wake. A role
// message the pause held for it is published once the resume lets its row run, and the outbox
// wakes nothing for a role message, so the Envoy listener's report of the publish is the role's
// wake, through the workflow, after the pause has ended. That wake finds no open pause and reads no
// row's mark, and is that pause's held wake all the same: the role's newest stop is the pause's
// and newer than the issue's newest release and the role's last ready, which is what kept the role
// down (store.PodRecords.PauseOfWake, store.StoppingPauseTx). So its resume is a role message's
// wake that names the pause and pairs with no release, as the wake of a delivery the pause held
// does (TestANoticeThePauseHeldWakesItsArchitectWithThatPause), and the task its start row gives
// the role says that the pause stopped its session (workflow.WakeTask). Before the first fix the
// wake read the open pause alone, found none, and was recorded as the wake of the issue's latest
// release. Before the second the outbox took a delivery's wake of the role for the held message
// and resumed it with no task, and the listener's report then took no wake.
func TestAListenersWakeAfterTheResumeOfARoleThePauseStoppedIsThatPausesHeldWake(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	// A release of the issue's pod from before the pause, which a wake no pause held would pair
	// with.
	if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'implementer', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("record a release of %s's pod: %v", issue, err)
	}
	r.boot()

	pause := r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: "The checks of your head settled."})
	// The listener takes the message once the row runs, and keeps it for the stopped role.
	r.envoy.hold(worker)
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
		t.Fatalf("the role message was published %d times while the project was paused, want it held", len(sent))
	}
	r.resumeProject()
	testwait.Eventually(t, "the held role message to be published after the resume", func() bool {
		return len(r.envoy.publishedTo(worker)) == 1 && r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	if c := r.claim(worker); !c.Released() {
		t.Errorf("the implementer after the publish is %s by %s, want it still stopped: the pause stopped it owing nothing, and a role message wakes nothing itself",
			c.State, c.SuspendedBy)
	}

	r.applyFact("listener", "wake-after-resume", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker),
		At: r.clock.Now(), Report: "report-after-resume"})
	if wakes := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer'`, issue); wakes != 1 {
		t.Fatalf("the implementer took %d wakes after the resume, want one", wakes)
	}
	var kind string
	var held, release int64
	if err := r.pool.QueryRow(r.ctx, `select kind, coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'implementer'`,
		issue).Scan(&kind, &held, &release); err != nil {
		t.Fatalf("read the implementer's wake: %v", err)
	}
	if kind != pods.WakeRoleMessage || held != pause.ID || release != 0 {
		t.Fatalf("the implementer's wake after the resume is a %s wake marked with pause %d and release %d, want a %s wake of pause %d and no release",
			kind, held, release, pods.WakeRoleMessage, pause.ID)
	}
	want := workflow.WakeTask(pods.WakeRoleMessage, string(worker), workflow.StoppedByPause)
	testwait.Eventually(t, "the wake's start to give the implementer its task", func() bool { return r.claim(worker).Pending != nil })
	if got := r.claim(worker).Pending.Task; got != want {
		t.Fatalf("the implementer's task after the wake = %q, want the task of a role message's wake of a session the pause stopped, %q", got, want)
	}
}

// The bound of a held role message's wait through the listener's gone answer for the role the
// pause stopped (TestARoleMessageThePauseHeldForARoleItStoppedIsDeliveredOnceAfterTheResume): it
// waits only while that role can come back for it (outbox.heldMessageOwed). An operator's
// suspension of the stopped role while the project is paused takes the suspension over from the
// pause's release (supervise.SuspendedByOperator), and nothing of the daemon's brings that role
// back, so once the resume lets the row run it takes main's path: it is published, the listener
// answers that the role's session is gone, and the row ends undelivered, with no launch and no
// wake.
//
// The negative control is the wait counted whatever the claim's state: the row waits for a role
// that is not coming back, and the test fails where it waits for the row to end.
func TestAHeldRoleMessageForARoleAnOperatorSuspendedDuringThePauseEndsUndelivered(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.envoy.gone(worker)
	r.boot()

	r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: "The checks of your head settled."})
	if status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(worker)+"/suspend", nil, true); status != http.StatusOK {
		t.Fatalf("the operator's suspension of the implementer = %d; body %s", status, body)
	}
	if c := r.claim(worker); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the implementer after the operator's suspension is %s by %s, want suspended by the operator", c.State, c.SuspendedBy)
	}
	before := len(r.rt.launches())
	r.resumeProject()
	testwait.Eventually(t, "the held role message for the role the operator suspended to end after the resume", func() bool {
		return r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	time.Sleep(time.Second)

	if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
		t.Fatalf("the role message was taken %d times for a role the operator suspended, want none", len(sent))
	}
	if launched := len(r.rt.launches()) - before; launched != 0 || len(r.state().WakesWaiting) != 0 {
		t.Fatalf("the held role message launched %d roles and left %d wakes waiting, want none", launched, len(r.state().WakesWaiting))
	}
	if wakes := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer'`, issue); wakes != 0 {
		t.Fatalf("the implementer took %d wakes, want none", wakes)
	}
	if c := r.claim(worker); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the implementer after the resume is %s by %s, want still suspended by the operator", c.State, c.SuspendedBy)
	}
}

// The wake of a role the pause stopped owing nothing is that pause's whichever path takes it
// (store.PodRecords.PauseOfWake), a delivery's as a listener's: the pause, not a release, kept the
// role down. A notice queued after the resume carries no pause's mark, and its wake of the stopped
// architect reads the pause that stopped it, so the resume names that pause and pairs with no
// release, as the listener's wake of such a role does
// (TestAListenersWakeAfterTheResumeOfARoleThePauseStoppedIsThatPausesHeldWake). Before the fix the
// delivery wake read its row's mark alone, found none, and was recorded as the wake of the release
// from before the pause.
func TestANoticeQueuedAfterTheResumeWakesAnArchitectThePauseStoppedWithThatPause(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	architect := r.token(issue, claim.RoleArchitect)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleArchitect, supervise.StateIdle))
	// A release of the issue's pod from before the pause, which a wake no pause held would pair
	// with.
	if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'architect', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("record a release of %s's pod: %v", issue, err)
	}
	r.boot()

	pause := r.pauseProject()
	r.awaitState(architect, supervise.StateSuspended, "the pause stops the idle architect")
	r.resumeProject()
	if c := r.claim(architect); !c.Released() {
		t.Fatalf("the architect after the resume is %s by %s, want it still stopped: the pause stopped it owing nothing", c.State, c.SuspendedBy)
	}
	r.enqueue(issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"})
	testwait.Eventually(t, "the notice queued after the resume to wake its architect", func() bool {
		return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'architect' and kind = $2`, issue, pods.WakeDelivery) == 1
	})
	var held, release int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'architect' and kind = $2`,
		issue, pods.WakeDelivery).Scan(&held, &release); err != nil {
		t.Fatalf("read the notice's wake: %v", err)
	}
	if held != pause.ID || release != 0 {
		t.Fatalf("the wake of the notice queued after the resume is marked with pause %d and release %d, want pause %d and no release",
			held, release, pause.ID)
	}
}

// The resume launches an operator's spawn of another project's root architect that the pause
// stopped owing a launch (resumeOwedSpawns), and that launch's ready reads the root's record by
// key (workflow's claimReady). On a database shared with another project's daemon that record is
// that daemon's, and it is no record of this daemon's: the daemon's records answer it as none
// (projectRecords.Issue). A catch-up built from it would be a row keyed by an issue whose rows the
// other project's daemon runs, and its drop of the root's queued catch-ups would delete that
// daemon's own. So after the resume the relaunched spawn's ready queues no row keyed by the other
// project's issue and deletes none of that daemon's rows.
func TestTheResumedSpawnOfAnotherProjectsRootArchitectTouchesNoRowOfThatProjectAtItsReady(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	r.boot()
	const issue = "OTHERPROJECT-9"
	recorded := r.root(issue, phase.Planning)
	recorded.Project = "OTHERPROJECT"
	if err := store.Transact(r.ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(r.ctx, tx, recorded) }); err != nil {
		t.Fatalf("record %s as the OTHERPROJECT daemon's issue: %v", issue, err)
	}
	const task = "Summarize the incident for the operator."
	// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
	r.board.place(issue, "todo", "", false)
	token := r.d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait.", Task: task})
	pause := r.pauseProject()
	testwait.Eventually(t, "the pause's stop of the spawn to be held until its agent is ready", func() bool {
		return r.claim(token).SuspensionHeld
	})
	launched := r.claim(token)
	const session = "ses-other-root-architect"
	r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: launched.Generation, Session: session})
	r.awaitState(token, supervise.StateSuspended, "the held stop runs at the agent's ready")
	if got := r.count(`select count(*) from project_pause_stops where pause_id = $1 and claim = $2 and ended_as = 'stopped' and launch_owed`,
		pause.ID, string(token)); got != 1 {
		t.Fatalf("the spawn's stops recorded stopped owing a launch = %d, want 1", got)
	}

	// The other project's daemon has a catch-up of its own architect of the root queued.
	theirs := r.enqueue(issue, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "OTHERPROJECT's own catch-up of its root",
		CatchUp: &record.CatchUp{Generation: 1, Launch: 1, Gates: []record.CatchUpGate{}, Issues: []record.CatchUpIssue{}}})
	rowsOf := func() map[int64]string {
		t.Helper()
		rows, err := r.pool.Query(r.ctx, `select id, kind || '/' || coalesce(payload->>'kind', payload->>'op', '') from outbox where issue = $1`, issue)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[int64]string{}
		for rows.Next() {
			var id int64
			var what string
			if err := rows.Scan(&id, &what); err != nil {
				t.Fatal(err)
			}
			found[id] = what
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	before := rowsOf()

	launches := len(r.rt.launchesOf(token))
	r.resumeProject()
	if !testwait.Holds(testwait.Bounded(t, 20*time.Second, time.Second), testwait.Poll, func() bool {
		if len(r.rt.launchesOf(token)) > launches {
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
		t.Fatalf("after the resume the spawn owed a launch is %s by %s; want it launched once through the pacing", c.State, c.SuspendedBy)
	}
	relaunched := r.claim(token)
	r.dialShim(lastLaunch(t, r.rt.Runtime, token).BootToken)
	testwait.Eventually(t, "the relaunched spawn's shim to say hello", func() bool {
		return r.claim(token).State == supervise.StateShimConnected
	})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: relaunched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: relaunched.Generation, Session: session})
	testwait.Eventually(t, "the relaunched spawn's agent to be ready", func() bool {
		state := r.claim(token).State
		return state == supervise.StateReady || state == supervise.StateWorking || state == supervise.StateIdle
	})
	// The ready's fact is applied after the machine records the ready, in a transaction of its
	// own; two seconds is ample for it, and what this test reads is the absence of its rows.
	time.Sleep(2 * time.Second)

	after := rowsOf()
	if _, kept := after[theirs]; !kept {
		t.Errorf("the other project's daemon's catch-up row %d keyed %s is gone after the resumed spawn's ready: this daemon deleted a row of another project's issue",
			theirs, issue)
	}
	for id, what := range after {
		if _, had := before[id]; !had {
			t.Errorf("the resumed spawn's ready queued outbox row %d (%s) keyed %s, want none: this daemon's outbox runs no row of another project's issue, and that project's daemon runs every one",
				id, what, issue)
		}
	}
}

// A fact of a claim of another project's issue that the resume launched again reads the issue's
// record by key, as its ready does: the claim's failure (workflow's claimFailed), the loss of the
// session it recorded (sessionLost), a fault of the session store at its start
// (sessionStoreFault) and a memory guard's kill of one of its processes (memoryGuardKill). On a
// database shared with another project's daemon that record is that daemon's, and this daemon's
// records answer it as no record (projectRecords.Issue). So each fact is applied as a fact of a
// claim of an issue the daemon keeps no record of: it tells nobody and writes nothing of the
// other project's record, it queues no row keyed by that project's issue and deletes none of that
// daemon's, and a memory guard's kill is logged as one that names an issue the daemon does not
// record. The spawn is of the root's architect, as the operator's spawn of a tree's root makes
// one, so its failure is one that would tell the tree's architect and the controller were the
// record this daemon's.
func TestAFactOfTheResumedSpawnOfAnotherProjectsIssueTouchesNoRowOfThatProject(t *testing.T) {
	t.Parallel()
	const issue = "OTHERPROJECT-9"
	const role = claim.RoleArchitect
	for _, tc := range []struct {
		name string
		fact func(token claim.Token) intake.Fact
		// logged is the line the daemon writes of the fact, "" for none.
		logged string
	}{
		{name: "its claim failed", fact: func(claim.Token) intake.Fact {
			return intake.ClaimFailed{Issue: issue, Role: role, Reason: "the claim's start budget ran out"}
		}},
		{name: "its session was lost", fact: func(claim.Token) intake.Fact {
			return intake.SessionLost{Issue: issue, Role: role, Detail: "/sessions/ses-other-root-architect.jsonl is in no session store"}
		}},
		{name: "the session store faulted at its start", fact: func(claim.Token) intake.Fact {
			return intake.SessionStoreFault{Issue: issue, Role: role, Detail: "the session store refused the connection"}
		}},
		{name: "a memory guard killed one of its processes", fact: func(token claim.Token) intake.Fact {
			return intake.MemoryGuardKill{Issue: issue, Token: token, PodUID: "pod-other-9", Report: shimwire.LauncherMemoryKill{
				Guard: "launcher", Sequence: 1, PID: 4242, Executable: "go", SizeBytes: 6 << 30, ReadingBytes: 7 << 30, LimitBytes: 8 << 30}}
		}, logged: "workflow: a memory guard kill names an issue the daemon does not record, so no role is told"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseDaemonRig(t, nil)
			r.boot()
			recorded := r.root(issue, phase.Planning)
			recorded.Project = "OTHERPROJECT"
			if err := store.Transact(r.ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(r.ctx, tx, recorded) }); err != nil {
				t.Fatalf("record %s as the OTHERPROJECT daemon's issue: %v", issue, err)
			}
			// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
			r.board.place(issue, "todo", "", false)
			token := r.d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: role, Prompt: "Reply ready and wait.",
				Task: "Summarize the incident for the operator."})
			r.pauseProject()
			testwait.Eventually(t, "the pause's stop of the spawn to be held until its agent is ready", func() bool {
				return r.claim(token).SuspensionHeld
			})
			launched := r.claim(token)
			const session = "ses-other-root-architect"
			r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
			r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
				SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
			r.handle(token, supervise.RequestReady{Claim: token, Generation: launched.Generation, Session: session})
			r.awaitState(token, supervise.StateSuspended, "the held stop runs at the agent's ready")
			launches := len(r.rt.launchesOf(token))
			r.resumeProject()
			if !testwait.Holds(testwait.Bounded(t, 20*time.Second, time.Second), testwait.Poll, func() bool {
				if len(r.rt.launchesOf(token)) > launches {
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
				t.Fatalf("after the resume the spawn owed a launch is %s by %s; want it launched once through the pacing", c.State, c.SuspendedBy)
			}

			// The other project's daemon has a catch-up of its own queued for the issue.
			theirs := r.enqueue(issue, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "OTHERPROJECT's own catch-up of its root",
				CatchUp: &record.CatchUp{Generation: 1, Launch: 1, Gates: []record.CatchUpGate{}, Issues: []record.CatchUpIssue{}}})
			before, held := r.outboxRows(issue), r.recordOf(issue)
			mark := r.mark()
			event := "fact-of-" + string(token)
			r.applyFact("supervise", event, tc.fact(token))

			after := r.outboxRows(issue)
			if _, kept := after[theirs]; !kept {
				t.Errorf("the other project's daemon's catch-up row %d keyed %s is gone after the fact: this daemon deleted a row of another project's issue",
					theirs, issue)
			}
			for id, what := range after {
				if _, had := before[id]; !had {
					t.Errorf("the fact queued outbox row %d (%s) keyed %s, want none: this daemon's outbox runs no row of another project's issue, and that project's daemon runs every one",
						id, what, issue)
				}
			}
			if now := r.recordOf(issue); now == nil || !reflect.DeepEqual(*now, *held) {
				t.Errorf("OTHERPROJECT's record of %s after the fact = %+v, want it as that daemon wrote it: %+v", issue, now, held)
			}
			if got := r.count(`select count(*) from processed_events where source = 'supervise' and event_id = $1`, event); got != 1 {
				t.Errorf("the fact's processed marks = %d, want 1: it is applied as a fact of an issue the daemon keeps no record of", got)
			}
			if tc.logged == "" {
				return
			}
			lines := r.linesAfter(mark, tc.logged)
			if len(lines) != 1 || lines[0]["issue"] != issue {
				t.Errorf("the daemon wrote %v of the fact, want one %q naming %s", lines, tc.logged, issue)
			}
		})
	}
}

// A role-holder change of an operator's spawn of another project's issue — its agent's ready, and
// the pause's stop of it — touches no role_holders_publish row of that issue. That row is keyed by
// the issue, which the other project's daemon records, so only that daemon's outbox runs it and
// this daemon's never does (record's DueRows reads the rows of its own project's keys): a row this
// daemon writes there is a row of another project's issue, and a write that meets that daemon's
// own row while that daemon runs it changes the lease that daemon holds. The spawn's stop by the
// pause is a claim write that changes the role's holder (the store's putClaim), so it is where the
// pause meets this row.
func TestThePausesStopOfASpawnOfAnotherProjectsIssueTouchesNoRoleHoldersRowOfThatProject(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	r.boot()
	const issue = "OTHERPROJECT-9"
	recorded := r.root(issue, phase.Planning)
	recorded.Project = "OTHERPROJECT"
	if err := store.Transact(r.ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(r.ctx, tx, recorded) }); err != nil {
		t.Fatalf("record %s as the OTHERPROJECT daemon's issue: %v", issue, err)
	}
	holderRows := func() (int, string) {
		t.Helper()
		var n int
		var lease *string
		if err := r.pool.QueryRow(r.ctx, `select count(*), max(lease_token) from outbox where kind = 'role_holders_publish' and issue = $1`,
			issue).Scan(&n, &lease); err != nil {
			t.Fatal(err)
		}
		if lease == nil {
			return n, ""
		}
		return n, *lease
	}
	// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
	r.board.place(issue, "todo", "", false)
	token := r.d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	testwait.Eventually(t, "the spawn to launch", func() bool { return len(r.rt.launchesOf(token)) == 1 })
	launched := r.claim(token)
	const session = "ses-other-architect"
	r.handle(token, supervise.StreamHello{Claim: token, Generation: launched.Generation})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: launched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: launched.Generation, Session: session})
	testwait.Eventually(t, "the spawn's agent to be ready", func() bool {
		state := r.claim(token).State
		return state == supervise.StateReady || state == supervise.StateIdle
	})
	if n, _ := holderRows(); n != 0 {
		t.Errorf("the spawn's ready left %d role_holders_publish rows keyed %s, want none: this daemon's outbox runs no row of another project's issue",
			n, issue)
	}

	// The other project's daemon is running its own publication of the issue: it holds the row's
	// lease when the pause stops this daemon's spawn.
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where kind = 'role_holders_publish' and issue = $1`, issue); err != nil {
		t.Fatal(err)
	}
	const theirLease = "otherproject-lease"
	var theirs int64
	if err := r.pool.QueryRow(r.ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
		values ('role_holders_publish', $1, jsonb_build_object('issue', $1::text), 0, now(), '', $2, now() + interval '5 minutes')
		returning id`, issue, theirLease).Scan(&theirs); err != nil {
		t.Fatalf("queue the other project's daemon's role-holders row of %s: %v", issue, err)
	}
	before := r.outboxRows(issue)
	r.pauseProject()
	r.awaitState(token, supervise.StateSuspended, "the pause stops the ready spawn")
	// The stop's claim write and its publication are one transaction, committed before the claim
	// reads suspended; a second of settling is for nothing else.
	time.Sleep(time.Second)

	if n, lease := holderRows(); n != 1 || lease != theirLease {
		t.Errorf("after the pause's stop of the spawn the role_holders_publish rows keyed %s = %d with lease %q, want the other project's daemon's one row with its lease %q",
			issue, n, lease, theirLease)
	}
	after := r.outboxRows(issue)
	if _, kept := after[theirs]; !kept {
		t.Errorf("the other project's daemon's role-holders row %d keyed %s is gone after the pause's stop", theirs, issue)
	}
	for id, what := range after {
		if _, had := before[id]; !had {
			t.Errorf("the pause's stop of the spawn queued outbox row %d (%s) keyed %s, want none", id, what, issue)
		}
	}
}
