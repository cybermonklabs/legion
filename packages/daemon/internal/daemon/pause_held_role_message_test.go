package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
// pacing, and the message reaches it once its session holds its topic. The listener answers the
// stopped role as production's has since cybermonklabs/legion#4088 (pauseEnvoy's gone): a publish
// to it is refused 404 `holder_lapsed` with `holder_state` `gone`, which notify reads as
// notify.ErrNoHolder. The row waits through that answer rather than ending undelivered
// (outbox.roleMessage), the listener reports the role's wake beside it, which the test applies,
// and the row reaches the role's session once the wake pacing has launched it. The role launches
// once after the resume, under the pause's held wake with no release, though a release of its pod
// from before the pause stands.
//
// The negative control is roleMessage without its wait on notify.ErrNoHolder for such a row: the
// row ends undelivered at its first attempt, and the test fails where it reads the row after that
// attempt.
func TestARoleMessageThePauseHeldForARoleItStoppedIsDeliveredOnceAfterTheResume(t *testing.T) {
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
	r.envoy.gone(worker)
	r.boot()

	pause := r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	const body = "The checks of your head settled."
	message := r.enqueue(issue, record.RoleMessage{Role: string(worker), Body: body})
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
		t.Fatalf("the role message was published %d times while the project was paused, want it held", len(sent))
	}
	if n := r.count(`select count(*) from outbox where id = $1`, message); n != 1 {
		t.Fatalf("the role message's row is gone while the project is paused, want it held")
	}

	attempts := heldRowAttempts(t, r, message)
	before := len(r.rt.launches())
	r.resumeProject()
	testwait.Eventually(t, "the held role message's first attempt after the resume", func() bool { return attempts() != 0 })
	if attempts() == -1 {
		t.Fatalf("the held role message's row ended at its first attempt, with %d publishes taken for the role; want it waiting through the listener's gone answer for the role the pause stopped",
			len(r.envoy.publishedTo(worker)))
	}
	r.applyFact("listener", "held-role-message-gone", intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(worker), At: r.clock.Now(), Report: "report-of-the-held-role-message"})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
			r.envoy.hold(worker)
		}
	}, func(api.State) {})
	testwait.WithinPolling(t, 6*time.Minute, 200*time.Millisecond, "the held role message to reach the role after its launch", func() bool {
		return len(r.envoy.publishedTo(worker)) > 0 && attempts() == -1
	})
	time.Sleep(time.Second)

	if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != body {
		t.Fatalf("the role message reached the role %d times (%+v), want once with its body", len(sent), sent)
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
// when the second pause begins, so that pause stops nothing of it, but the test makes the row due
// on both clocks while the second pause stands (makeDue), and the second resume marks it with the
// second pause, while the role's newest stop is still the first pause's. The message still reaches
// the role once it is back.
//
// The negative control is heldMessageOwed asking that the pause that held the row be the pause
// that stopped the role (c16617d0's `pause != row.HeldByPause`): once the second resume lets the
// row run, it ends undelivered, and the test fails where it reads the row's mark or the message.
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
	second := r.pauseProject()
	if n := r.makeDue(record.OutboxKindRoleMessage, issue); n != 1 {
		t.Fatalf("%d role message rows of %s made due while the second pause stands, want the held one", n, issue)
	}
	before := len(r.rt.launches())
	r.resumeProject()
	if held := r.heldByPause(message); held != second.ID {
		t.Fatalf("the held role message's row is marked with pause %d after the second resume (-1: the row has ended), want the second pause, %d",
			held, second.ID)
	}
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

// A second pause whose stop of the returning role its resume cancels (R4 of
// dispatch://INFRA-3759). The first pause stops the idle role, whose session reads gone, and a
// role message is held for it. After the first resume the row's publish is answered gone, the
// listener reports the role's wake, and the wake pacing makes the role's launch. The second pause
// begins before that launch is ready, so its stop of the role waits for the ready, and the second
// resume comes first and ends that stop `cancelled`: the role's newest stop, which stopped
// nothing. The row runs again before the role is ready, its publish answered gone once more, and
// is still owed to the role under the first pause's stop, so the message reaches the role once,
// once the role is ready and holds its topic.
//
// The negative control is store.StoppedByAPause reading the claim's newest stop whatever its end:
// the cancelled stop answers no pause, the row's attempt after the second resume ends it
// undelivered, and the test fails where it reads the row after that attempt.
func TestARoleMessageThePauseHeldForARoleItStoppedSurvivesASecondPauseWhoseStopItsResumeCancels(t *testing.T) {
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
	before := len(r.rt.launches())
	r.resumeProject()
	testwait.Eventually(t, "the held role message's first attempt after the first resume", func() bool { return attempts() != 0 })
	if attempts() == -1 {
		t.Fatalf("the held role message's row ended at its first attempt after the first resume, want it waiting for the role")
	}
	r.applyFact("listener", "held-role-message-gone", intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(worker), At: r.clock.Now(), Report: "report-of-the-held-role-message"})
	r.runPacing(before, 1, func(waveLaunch) {}, func(api.State) {})
	testwait.Eventually(t, "the implementer's launch to record its process", func() bool {
		c := r.claim(worker)
		return c.State == supervise.StateLaunching && c.Locator != nil
	})

	second := r.pauseProject()
	stopsEnded := func(end string) int {
		return r.count(`select count(*) from project_pause_stops where pause_id = $1 and claim = $2 and coalesce(ended_as, '') = $3`,
			second.ID, string(worker), end)
	}
	testwait.Eventually(t, "the second pause's stop of the launching implementer, open until its ready", func() bool { return stopsEnded("") == 1 })
	if n := r.makeDue(record.OutboxKindRoleMessage, issue); n != 1 {
		t.Fatalf("%d role message rows of %s made due while the second pause stands, want the held one", n, issue)
	}
	if c := r.claim(worker); c.State != supervise.StateLaunching {
		t.Fatalf("the implementer is %s while the second pause stands, want its launch still waiting for its ready", c.State)
	}
	held := attempts()
	r.resumeProject()
	if cancelled := stopsEnded(string(store.PauseStopCancelled)); cancelled != 1 {
		t.Fatalf("the second resume cancelled %d stops of the implementer, want the second pause's one open stop", cancelled)
	}
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "the held role message's attempt after the second resume, the implementer not ready yet", func() bool {
		n := attempts()
		return n == -1 || n > held
	})
	if attempts() == -1 {
		t.Fatalf("the held role message's row ended at its attempt after the second resume, with %d publishes taken for the role; want it still owed to the role under the first pause's stop: the second pause's cancelled stop stopped nothing",
			len(r.envoy.publishedTo(worker)))
	}
	r.ready(worker)
	r.envoy.hold(worker)
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "the held role message's row to end", func() bool { return attempts() == -1 })
	time.Sleep(time.Second)
	if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != body {
		t.Fatalf("the held role message reached the role %d times across a second pause whose stop its resume cancelled, want once with its body", len(sent))
	}
}

// The held role message's debt at its one statement, heldMessageOwed (R4 of
// dispatch://INFRA-3759), over claim views of a role a pause stopped, the store answering which
// pause stopped it. The row is owed under the pause of the claim's newest stop that ended
// `stopped`, whichever pause held the row, while the claim is not gone or is released with an
// issue that can be woken. The rows of the claim's states are every state supervise declares
// (supervise.States), each owed unless it is gone (supervise.IsGone), launch_uncertain among the
// owed; the rows of its suspensions are every source of a suspension supervise declares, owed
// for a release alone, a root's own exit among the rest. A later stop of the claim that ended
// `cancelled`, `taken_over` or `failed`, or is still open, is passed over, and the row stays owed
// under the first pause's stop; a later stop that ended `stopped` after a release of the issue's
// pod carries the debt under its own pause. It is not owed for a released claim whose issue has
// left the workflow, a row no pause held, or a claim whose newest `stopped` stop is older than a
// release of its issue's pod.
//
// The negative controls are heldMessageOwed's hand list of the states a role comes back from, put
// back without launch_uncertain, where the launch_uncertain case answers 0; and
// store.StoppedByAPause reading the claim's stops oldest first or in no order, where the claim
// stopped again after its pod's release answers 0, since its first stop is older than that
// release.
func TestAHeldRoleMessageIsOwedUntilAnEndEventOfItsDebt(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	runner := &outbox{pool: pool, records: records, trees: outboxTreeStore(t, pool), project: "legion", log: quietLogger()}
	base := time.Now().UTC().Add(-time.Hour)
	pauseOf := func(began, ended time.Time) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, reason, ended_at, ended_by_kind, ended_by_session)
			values ('LEGION', $1, 'operator', 'a pause of the test', $2, 'operator', '') returning id`, began, ended).Scan(&id); err != nil {
			t.Fatalf("record a pause: %v", err)
		}
		return id
	}
	first, second := pauseOf(base, base.Add(10*time.Minute)), pauseOf(base.Add(20*time.Minute), base.Add(30*time.Minute))
	var standing int64
	if err := pool.QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, reason)
		values ('LEGION', $1, 'operator', 'a pause of the test that stands') returning id`, base.Add(40*time.Minute)).Scan(&standing); err != nil {
		t.Fatalf("record the pause that stands: %v", err)
	}
	type role struct {
		token claim.Token
		issue string
	}
	implementer := func(key, status string) role {
		t.Helper()
		putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: key, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: status, Rank: "U", LastDispatchSeq: 5})
		return role{token: mustClaimToken(t, key, claim.RoleImplementer), issue: key}
	}
	// stop records pause's stop of the role, asked at at and ended then as end, or still open where
	// end is empty.
	stop := func(pause int64, of role, end store.PauseStopEnd, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at, ended_at, ended_as)
			values ($1, 'LEGION', $2, $3, 'implementer', $4, case when $5::text = '' then null else $4::timestamptz end, nullif($5::text, ''))`,
			pause, string(of.token), of.issue, at, string(end)); err != nil {
			t.Fatalf("record pause %d's stop of %s: %v", pause, of.token, err)
		}
	}
	release := func(of role, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at) values ('legion', $1, 1, 'implementer', $2)`,
			of.issue, at); err != nil {
			t.Fatalf("record a release of %s's pod: %v", of.issue, err)
		}
	}
	// The first pause stops each role. stopped's pod was released before that stop; cancelled's
	// stop by the second pause was cancelled by its resume; left's issue has left the workflow
	// since; releasedSince's pod was released after the stop. Each role of a later stop has one
	// after the first pause's: takenOver's by the second pause ended `taken_over`, open's by the
	// pause that stands is still open, failedSince's by the second pause ended `failed`, and
	// stoppedAgain's by the second pause ended `stopped` after a release of its pod that came after
	// the first pause's stop. releasedElsewhere's issue has a pod release after the stop that
	// another project's daemon recorded, on the database the two share: that daemon's pod of the
	// issue, not this daemon's, so it ends nothing of this daemon's debt.
	stopped, cancelled := implementer("LEGION-1", "in_progress"), implementer("LEGION-2", "in_progress")
	left, releasedSince := implementer("LEGION-3", "backlog"), implementer("LEGION-4", "in_progress")
	takenOver, open := implementer("LEGION-5", "in_progress"), implementer("LEGION-6", "in_progress")
	failedSince, stoppedAgain := implementer("LEGION-7", "in_progress"), implementer("LEGION-8", "in_progress")
	releasedElsewhere := implementer("LEGION-9", "in_progress")
	for _, of := range []role{stopped, cancelled, left, releasedSince, takenOver, open, failedSince, stoppedAgain, releasedElsewhere} {
		stop(first, of, store.PauseStopStopped, base.Add(5*time.Minute))
	}
	release(stopped, base.Add(-time.Hour))
	stop(second, cancelled, store.PauseStopCancelled, base.Add(30*time.Minute))
	release(releasedSince, base.Add(15*time.Minute))
	stop(second, takenOver, store.PauseStopTakenOver, base.Add(25*time.Minute))
	stop(standing, open, "", base.Add(45*time.Minute))
	stop(second, failedSince, store.PauseStopFailed, base.Add(25*time.Minute))
	release(stoppedAgain, base.Add(15*time.Minute))
	stop(second, stoppedAgain, store.PauseStopStopped, base.Add(25*time.Minute))
	if _, err := pool.Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at) values ('otherproject', $1, 1, 'implementer', $2)`,
		releasedElsewhere.issue, base.Add(15*time.Minute)); err != nil {
		t.Fatalf("record another project's release of %s's pod: %v", releasedElsewhere.issue, err)
	}

	view := func(of role, state supervise.ClaimState, by supervise.SuspendSource) supervise.ClaimView {
		return supervise.ClaimView{Claim: supervise.Claim{Token: of.token, Project: "legion", Tree: of.issue, Issue: of.issue,
			Role: claim.RoleImplementer, State: state, SuspendedBy: by}}
	}
	type owedCase struct {
		name string
		held int64
		view supervise.ClaimView
		want int64
	}
	var cases []owedCase
	// Every state a claim can be in, owed under the first pause unless the claim is gone. The
	// suspended claim of this walk names no source, so it is no release.
	for _, state := range supervise.States() {
		want := first
		if supervise.IsGone(state) {
			want = 0
		}
		cases = append(cases, owedCase{string(state), second, view(stopped, state, ""), want})
	}
	// Every source of a suspension supervise declares, which exports no list of them: owed for a
	// release alone, whose issue here can be woken.
	for _, by := range []supervise.SuspendSource{supervise.SuspendedByUnknown, supervise.SuspendedByOperator,
		supervise.SuspendedByDaemon, supervise.SuspendedByExit, supervise.SuspendedByReleased} {
		want := int64(0)
		if by == supervise.SuspendedByReleased {
			want = first
		}
		cases = append(cases, owedCase{"suspended by " + string(by), second, view(stopped, supervise.StateSuspended, by), want})
	}
	cases = append(cases,
		owedCase{"launching, a later stop of it cancelled", second, view(cancelled, supervise.StateLaunching, ""), first},
		owedCase{"launching, a later stop of it taken over", second, view(takenOver, supervise.StateLaunching, ""), first},
		owedCase{"launching, a later stop of it still open", second, view(open, supervise.StateLaunching, ""), first},
		owedCase{"idle again, a later stop of it failed", second, view(failedSince, supervise.StateIdle, ""), first},
		owedCase{"launching, stopped again after a release of its pod", second, view(stoppedAgain, supervise.StateLaunching, ""), second},
		owedCase{"released, its issue out of the workflow", second, view(left, supervise.StateSuspended, supervise.SuspendedByReleased), 0},
		owedCase{"released, its row held by no pause", 0, view(stopped, supervise.StateSuspended, supervise.SuspendedByReleased), 0},
		owedCase{"released, its pod released after the stop", second, view(releasedSince, supervise.StateSuspended, supervise.SuspendedByReleased), 0},
		owedCase{"released, its issue's pod released after the stop by another project's daemon", second,
			view(releasedElsewhere, supervise.StateSuspended, supervise.SuspendedByReleased), first},
	)
	for _, c := range cases {
		if got, err := runner.heldMessageOwed(ctx, record.OutboxRow{Issue: c.view.Issue, HeldByPause: c.held}, c.view); err != nil || got != c.want {
			t.Errorf("%s: heldMessageOwed = %d, %v; want %d", c.name, got, err, c.want)
		}
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
