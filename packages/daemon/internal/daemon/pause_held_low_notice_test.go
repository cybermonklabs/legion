package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A `low` notice that a project's pause held launches its reader at the resume
// (dispatch://INFRA-3686's Design, which keeps R4 of dispatch://INFRA-3759: the resume launches each
// reader it held a text for, whatever the text's urgency). Where the low rule and the held-row
// release meet, the held row wins (lowRule): the notice wakes its released architect with the
// pause's held wake, as a `med` notice would, and is still published at `low`.
//
// The notice is a checks-red notice of an issue in testing, `low` to the architect, queued while
// the project is paused for an architect an idle release took before the pause, so the pause owes
// that architect no launch and nothing else of the resume brings it back. The negative control is
// lowRule without its held-row exception: the row then takes the low rule, waits an hour for the
// architect's next ready and wakes nothing, and the architect is never launched.
func TestALowNoticeAPauseHeldLaunchesItsReaderAtTheResume(t *testing.T) {
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.Gates.Design = config.DesignGateOff.Steps() })
	issue := r.issue(1)
	r.seed(r.root(issue, phase.Testing), r.running(issue, claim.RoleTester, supervise.StateIdle),
		r.released(issue, claim.RoleArchitect))
	// The idle release that took the architect's pod before the pause, which a wake no pause held
	// would pair with.
	if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'architect', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("record a release of %s's pod: %v", issue, err)
	}
	r.boot()

	pause := r.pauseProject()
	r.awaitState(r.token(issue, claim.RoleTester), supervise.StateSuspended, "the pause stops the idle tester")
	notice, err := record.NoticeOf(texts.ChecksRedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChecksRed, Issue: issue},
		Phase: phase.Testing, ConflictsWith: "main", Head: "c0de000000000000000000000000000000000001",
		PullRequest: texts.PullRequestRef(liveRepo, 42), Why: string(record.ConflictCheckNotGreen)})
	if err != nil {
		t.Fatalf("build the checks-red notice: %v", err)
	}
	if got := noticeUrgency(notice, claim.RoleArchitect); got != texts.Low {
		t.Fatalf("the checks-red notice of an issue in testing goes to the architect at %q, want low: this case proves nothing", got)
	}
	row := r.enqueue(issue, notice)
	r.resumeProject()
	if got := r.heldByPause(row); got != pause.ID {
		t.Fatalf("the resume marked the held low notice with pause %d, want %d", got, pause.ID)
	}

	testwait.Eventually(t, "the held low notice to wake its released architect", func() bool {
		return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'architect' and kind = $2`, issue, pods.WakeDelivery) == 1
	})
	var held *int64
	var release int64
	if err := r.pool.QueryRow(r.ctx, `select pause_id, release_id from pod_resumes where issue = $1 and role = 'architect' and kind = $2`,
		issue, pods.WakeDelivery).Scan(&held, &release); err != nil {
		t.Fatalf("read the notice's wake: %v", err)
	}
	if held == nil || *held != pause.ID || release != 0 {
		t.Fatalf("the held low notice's wake is marked with pause %v and release %d, want pause %d and no release", held, release, pause.ID)
	}
}

// A `low` role message that a project's pause held for a role that pause stopped is published at
// the resume and launches that role, as a `med` one does (the held row wins over the low rule; R4
// of dispatch://INFRA-3759, which the spec's Design keeps). The listener reports no wake for a
// `low` message with no sender (its wakesItsRole), so the daemon raises the role's wake itself on
// the listener's answer that no session holds the role (outbox.wakeHeldRole), and the workflow's
// wake of the role is the pause's held wake.
//
// The message is a CI settlement of an issue in testing to its implementer, which does not work
// that phase, so it is `low` to it. The pause stops the idle implementer. Nothing in this test
// stands in for the listener's report: the wake the test waits for is the daemon's own. The role
// is launched once, under the pause's held wake with no release, and the message reaches its
// session once it holds its topic, at `low`.
//
// The negative control is roleMessage without its wakeHeldRole call: the row waits through the
// listener's answer, nobody wakes the role, and the test fails where it waits for the wake.
func TestALowRoleMessageAPauseHeldForARoleItStoppedLaunchesThatRoleAtTheResume(t *testing.T) {
	r := newPauseDaemonRig(t, nil)
	issue := r.issue(1)
	worker := r.token(issue, claim.RoleImplementer)
	r.seed(r.root(issue, phase.Testing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
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

	testwait.Eventually(t, "the daemon to wake the role its pause stopped, for the held low role message", func() bool {
		return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer' and kind = $2`, issue, pods.WakeRoleMessage) == 1
	})
	r.runPacing(before, 1, func(launch waveLaunch) {
		if launch.claim == worker {
			r.ready(worker)
			r.envoy.hold(worker)
		}
	}, func(api.State) {})
	testwait.WithinPolling(t, 6*time.Minute, 200*time.Millisecond, "the held low role message to reach the role after its launch", func() bool {
		return len(r.envoy.publishedTo(worker)) > 0 && r.count(`select count(*) from outbox where id = $1`, message) == 0
	})
	time.Sleep(time.Second)

	sent := r.envoy.publishedTo(worker)
	var payload struct {
		Kind string `json:"kind"`
	}
	if len(sent) != 1 || sent[0].urgency != string(texts.Low) || json.Unmarshal([]byte(sent[0].payload), &payload) != nil ||
		payload.Kind != string(texts.CISettled) {
		t.Fatalf("the role message reached the role %d times (%+v), want once, a ci-settled at low", len(sent), sent)
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
	var held, release int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(pause_id, 0), release_id from pod_resumes where issue = $1 and role = 'implementer'`,
		issue).Scan(&held, &release); err != nil {
		t.Fatalf("read the implementer's wake: %v", err)
	}
	if held != pause.ID || release != 0 {
		t.Fatalf("the implementer's wake is marked with pause %d and release %d, want pause %d and no release", held, release, pause.ID)
	}
}
