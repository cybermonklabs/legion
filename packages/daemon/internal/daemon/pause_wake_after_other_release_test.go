package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A wake's task names what stopped the session it brings back (stopped_by, decision 16 of
// dispatch://INFRA-3759), and which pause holds the wake reads the same stop (store.PauseOfWake,
// decision 15). A release suspends only the live claims it stops and records their roles
// (store.PodRelease.Roles), so a release of the pod that suspended another role alone, such as the
// idle release of a role that ran in the pod after the resume, did not stop the session of a role
// the pause had stopped before it: the pause did (store.StoppingPauseTx, the one statement the
// update-stalled notice reads too). Here the pause stops the idle implementer, owing it nothing; once
// the project is resumed, the pod's release that suspended the tester alone is recorded; then the
// listener reports a role message to the implementer. The wake's task says that the pause stopped
// the session, and the wake is that pause's held wake, paired with no release.
//
// The negative control is StoppingPauseTx counting every release of the pod, whatever roles it
// suspended: the wake is the tester's release's, its task says stopped_by: release, and the
// resume pairs with that release.
func TestAWakeOfARoleThePauseStoppedSaysThePauseAfterAReleaseOfAnotherRole(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.boot()

	pause := r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	r.resumeProject()
	var stopped time.Time
	if err := r.pool.QueryRow(r.ctx, `select ended_at from project_pause_stops where claim = $1 and pause_id = $2 and ended_as = 'stopped'`,
		string(worker), pause.ID).Scan(&stopped); err != nil {
		t.Fatalf("read pause %d's stop of the implementer: %v", pause.ID, err)
	}
	if c := r.claim(worker); !c.Released() {
		t.Fatalf("the implementer after the resume is %s by %s, want it still stopped: the pause stopped it owing nothing", c.State, c.SuspendedBy)
	}

	// The release of the pod that suspended the tester alone, a minute after the pause's stop.
	released := stopped.Add(time.Minute)
	var release int64
	if err := r.pool.QueryRow(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'tester', $3) returning id`, r.project, issue, released).Scan(&release); err != nil {
		t.Fatalf("record the release of %s's pod that suspended the tester: %v", issue, err)
	}

	r.applyFact("listener", "wake-after-the-testers-release", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker),
		At: r.clock.Now(), Report: "report-after-the-testers-release"})
	if wakes := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer'`, issue); wakes != 1 {
		t.Fatalf("the implementer took %d wakes, want one", wakes)
	}
	var held, paired int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'implementer'`,
		issue).Scan(&held, &paired); err != nil {
		t.Fatalf("read the implementer's wake: %v", err)
	}
	testwait.Eventually(t, "the wake's start to give the implementer its task", func() bool { return r.claim(worker).Pending != nil })
	want := workflow.WakeTask(issue, pods.WakeRoleMessage, string(worker), workflow.StoppedByPause)
	if got := r.claim(worker).Pending.Task; got != want {
		t.Errorf("the implementer's task after the wake = %q, want the task of a wake of a session pause %d stopped, %q: "+
			"the one release since the stop (%d, at %s) suspended the tester alone",
			got, pause.ID, want, release, released.Format(time.RFC3339))
	}
	if held != pause.ID || paired != 0 {
		t.Errorf("the implementer's wake is marked with pause %d and release %d, want pause %d's held wake and no release: release %d suspended the tester alone",
			held, paired, pause.ID, release)
	}
}
