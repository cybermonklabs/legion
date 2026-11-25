package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A project's resume launches each reader it held a text for, whatever the text's urgency (R4 of
// dispatch://INFRA-3759, which dispatch://INFRA-3686's Design keeps): a role message a pause held
// for a role that pause stopped stays owed to the role until a debt's end event
// (outbox.heldMessageOwed), and a release of the pod is one only where it suspended the role. A
// release suspends only the live claims it stops and records their roles (store.PodRelease.Roles),
// so one that suspended the pod's other roles alone left the stopped role as the pause's stop left
// it (store.StoppedByAPause, which reads a pause's stop through the same condition as the wake and
// the update-stalled notice). Here the pause stops the idle implementer, a release of the pod that
// suspended the tester alone is recorded after that stop, and a low CI settlement the pause held is
// queued for the implementer. At the resume the daemon wakes the implementer for that message,
// launches it once under the pause's held wake, and the message reaches its session once, at low.
//
// The negative control is StoppedByAPause counting every release of the pod after the stop,
// whatever roles it suspended: the debt has ended, the low message to a released role finishes
// undelivered, and nothing launches the implementer.
func TestAResumeLaunchesARoleThePauseStoppedForItsHeldLowMessageAfterAReleaseOfAnotherRole(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Testing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
	r.envoy.gone(worker)
	r.boot()

	pause := r.pauseProject()
	r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
	var stopped time.Time
	if err := r.pool.QueryRow(r.ctx, `select ended_at from project_pause_stops where claim = $1 and pause_id = $2 and ended_as = 'stopped'`,
		string(worker), pause.ID).Scan(&stopped); err != nil {
		t.Fatalf("read pause %d's stop of the implementer: %v", pause.ID, err)
	}
	// The release of the pod that suspended the tester alone, a minute after the pause's stop.
	var release int64
	if err := r.pool.QueryRow(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'tester', $3) returning id`, r.project, issue, stopped.Add(time.Minute)).Scan(&release); err != nil {
		t.Fatalf("record the release of %s's pod that suspended the tester: %v", issue, err)
	}

	urgency := texts.UrgencyOf(texts.CISettled, claim.RoleImplementer, texts.Variant{Phase: phase.Testing})
	if urgency != texts.Low {
		t.Fatalf("a CI settlement of an issue in testing goes to the implementer at %q, want low: this case proves nothing", urgency)
	}
	settled, err := record.NewRoleMessage(string(worker), texts.CISettledRecord{RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: issue},
		PullRequest: texts.PullRequestRef(liveRepo, 42), Head: "c0de", Phase: phase.Testing, Passed: 3, ListPassed: texts.ListPassed(liveRepo, 42)}, urgency)
	if err != nil {
		t.Fatalf("build the CI settlement: %v", err)
	}
	message := r.enqueue(issue, settled)
	before := len(r.rt.launches())
	r.resumeProject()
	if got := r.heldByPause(message); got != pause.ID {
		t.Fatalf("the resume marked the held low role message with pause %d, want %d", got, pause.ID)
	}

	testwait.Eventually(t, "the daemon to wake the implementer the pause stopped, for the held low role message, though release "+
		"of the tester alone came after the stop", func() bool {
		return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer' and kind = $2`, issue, pods.WakeRoleMessage) == 1
	})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
			r.envoy.hold(worker)
		}
	}, func(api.State) {})
	testwait.WithinPolling(t, 6*time.Minute, 200*time.Millisecond, "the held low role message to reach the implementer after its launch", func() bool {
		return len(r.envoy.publishedTo(worker)) > 0 && r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	time.Sleep(time.Second)

	sent := r.envoy.publishedTo(worker)
	var payload struct {
		Kind string `json:"kind"`
	}
	if len(sent) != 1 || sent[0].urgency != string(texts.Low) || json.Unmarshal([]byte(sent[0].payload), &payload) != nil ||
		payload.Kind != string(texts.CISettled) {
		t.Fatalf("the role message reached the implementer %d times (%+v), want once, a ci-settled at low", len(sent), sent)
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
	var held, paired int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'implementer'`,
		issue).Scan(&held, &paired); err != nil {
		t.Fatalf("read the implementer's wake: %v", err)
	}
	if held != pause.ID || paired != 0 {
		t.Fatalf("the implementer's wake is marked with pause %d and release %d, want pause %d and no release: release %d suspended the tester alone",
			held, paired, pause.ID, release)
	}
}
