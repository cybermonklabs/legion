package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The issues of the two rigs below, one pod each: the session of the first runs a subagent, and
// the session of the second, the control, runs nothing.
const (
	idleRigSubagent       = "running-subagent"
	idleRigNothingRunning = "nothing-running"
)

// idleRigHeldPasses is how many passes of the daemon's idle tick the subagent's report holds the
// pod for before the test ends the subagent: more than one, so the hold is seen to stand pass
// after pass for as long as the report does, and not to be read once and dropped.
const idleRigHeldPasses = 4

// A session whose host runs a `task` subagent keeps its pod: R1 of dispatch://INFRA-3276, over
// the rig of TestIdleReleaseAndWake (idle_rig_test.go says where each part comes from). The
// role process reports the subagent as the Oh My Pi extension does, `subagent` among the
// background work of its activity report, through the daemon's own route
// (`POST /legion/v1/claims/activity`) into the claim row the idle rule reads; the
// extension's own test pins on the real binary that a running `task` subagent is reported so
// (packages/pi-legion/extensions/legion-session-activity-omp.test.ts).
//
// The session takes its start task's turn, and then its host starts a subagent between turns,
// which the session reports as soon as it reads it. The pod is made quiet past the idle bound and
// the daemon's own idle tick decides it pass after pass: each pass holds it for `background_work`
// with the detail `subagent`, and the release records count those passes as one streak, read
// through the route the `legion` tool's read_release_records posts to. The claim stays live and
// the pod stands for every one of them. When the subagent ends, the session reports that nothing
// runs, and the next pass that reads the pod quiet releases it: the hold stood exactly as long as
// the report did.
func TestIdleReleaseHoldsForARunningSubagent(t *testing.T) {
	rig := newIdleWakeRig(t, idleRigSubagent)
	issue := rig.issue(idleRigSubagent)
	token := rig.token(issue)
	rig.launchAll()
	rig.awaitStartTask(issue)
	rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: true, Background: []string{"subagent"}})
	rig.awaitReported(token, "subagent")

	held := rig.awaitHeld(issue, idleRigHeldPasses)
	if held.Detail != "subagent" {
		t.Fatalf("the hold of %s is %+v, want the detail subagent: the kinds the session's report named", issue.key, held)
	}
	if !rig.kube.pod(issue.pod) || rig.kube.sandbox(issue.pod) == nil {
		t.Fatalf("the pod of %s is gone after %d passes that held it", issue.key, held.Passes)
	}
	for _, release := range rig.releaseRecords(issue).Releases {
		if release.Issue == issue.key {
			t.Fatalf("a release of %s was recorded (%+v) while its session reported a running subagent", issue.key, release)
		}
	}

	// The subagent ends between turns. The session reports that nothing runs, and the pod is
	// released at the next pass that reads it quiet.
	rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: true})
	rig.awaitReported(token, "")
	rig.awaitReleased(issue)

	records := rig.storedReleaseRecords()
	release := idleRigReleaseOf(t, records, issue)
	streak := false
	for _, hold := range records.Holds {
		if hold.Issue != issue.key || hold.Reason != workflow.IdleHoldBackgroundWork.String() {
			continue
		}
		if hold.Detail != "subagent" {
			t.Errorf("the hold %+v of %s names %q, want subagent: the session reported no other work", hold, issue.key, hold.Detail)
		}
		if hold.At.After(release.At) {
			t.Errorf("the hold %+v of %s was read after its pod was released at %s", hold, issue.key, release.At)
		}
		if hold.ID == held.ID {
			streak = true
			if hold.Passes < held.Passes {
				t.Errorf("the streak of %s counts %d passes, fewer than the %d it had counted while the subagent ran", issue.key, hold.Passes, held.Passes)
			}
		}
	}
	if !streak {
		t.Errorf("the release records of %s no longer hold the streak %+v that kept its pod", issue.key, held)
	}
	rig.kube.requireServed(t)
}

// The control of TestIdleReleaseHoldsForARunningSubagent: the same rig and the same session,
// reporting through the same route after the same turn, with nothing running. Once the pod is
// quiet past the idle bound the daemon's idle tick releases it, and nothing held it for
// background work on the way.
func TestIdleReleaseReleasesASessionWithNothingRunning(t *testing.T) {
	rig := newIdleWakeRig(t, idleRigNothingRunning)
	issue := rig.issue(idleRigNothingRunning)
	token := rig.token(issue)
	rig.launchAll()
	rig.awaitStartTask(issue)
	rig.awaitReported(token, "")

	rig.awaitReleased(issue)

	records := rig.storedReleaseRecords()
	release := idleRigReleaseOf(t, records, issue)
	if len(release.Roles) != 1 || release.Roles[0] != string(issue.role) {
		t.Errorf("the release of %s suspended %v, want its one role %s", issue.key, release.Roles, issue.role)
	}
	for _, hold := range records.Holds {
		if hold.Issue == issue.key && hold.Reason == workflow.IdleHoldBackgroundWork.String() {
			t.Errorf("the pod of %s was held for background work (%+v), want no such hold: its session reported nothing running", issue.key, hold)
		}
	}
	rig.kube.requireServed(t)
}

// awaitStartTask waits until the issue's role has been given its start task, the phase task the
// daemon's boot sends the implementer of an issue at `implementing`. The role process takes its
// turn and reports as soon as it has recorded the task (idleRigRoleProcess.prompted).
func (r *idleWakeRig) awaitStartTask(issue idleRigIssue) {
	r.t.Helper()
	idleRigEventually(r.t, idleRigRecordWait, "the start task of "+issue.key+"'s role", func() bool {
		return len(r.agents.prompts(issue.key, string(issue.role))) > 0
	})
}

// awaitHeld waits until the daemon's idle tick has held the issue's pod for background work over
// at least passes passes of one streak, and answers that streak as the route the `legion` tool's
// read_release_records posts to reads it (releaseRecords). The claim rows are back-dated every
// couple of seconds, as awaitReleased back-dates them, since they are the clock the rule reads
// the idle period against. A claim that stops being live on the way is a release, or a stop,
// that the report should have held off, and fails the test at once.
func (r *idleWakeRig) awaitHeld(issue idleRigIssue, passes int64) api.HoldRecord {
	r.t.Helper()
	token := r.token(issue)
	var held api.HoldRecord
	var quieted time.Time
	idleRigEventually(r.t, idleRigRecordWait, "the pod of "+issue.key+" held for background work over passes of the idle tick", func() bool {
		if time.Since(quieted) > 2*time.Second {
			quieted = time.Now()
			r.quiet()
		}
		if state := r.api.claim(token); !idleRigWorking(state.State) {
			r.t.Fatalf("%s is %s (suspended by %q) while its session reports background work, want it live and its pod kept",
				issue.key, state.State, state.SuspendedBy)
		}
		for _, hold := range r.releaseRecords(issue).Holds {
			if hold.Issue == issue.key && hold.Reason == workflow.IdleHoldBackgroundWork.String() && hold.Passes >= passes {
				held = hold
				return true
			}
		}
		return false
	})
	return held
}

// awaitReported waits until the claim row holds a report of its session's that names kinds as the
// background work running, comma-joined as the store keeps them (store.Store.PutClaimActivity),
// and "" for a report that nothing runs. The role process sends a new one within a second of a
// change of what its plan says runs (idleRigRoleProcess.watchActivity), so a wait on the row is a
// wait on that report.
func (r *idleWakeRig) awaitReported(token claim.Token, kinds string) {
	r.t.Helper()
	idleRigEventually(r.t, idleRigRecordWait, fmt.Sprintf("the report of %s naming background work %q", token, kinds), func() bool {
		var background bool
		var held string
		err := r.pool.QueryRow(context.Background(),
			`select activity_background, activity_kinds from claims where token = $1 and activity_at is not null`,
			string(token)).Scan(&background, &held)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			r.t.Fatalf("read the activity report of %s: %v", token, err)
		}
		return background == (kinds != "") && held == kinds
	})
}

// storedReleaseRecords is every release, resume and hold of the rig's project as the store holds
// them: the read behind the route releaseRecords posts to (store.Store.PodReleaseRecords). It is
// read from the store and not through that route because the route authenticates by a live
// claim, and every claim of a released pod is suspended.
func (r *idleWakeRig) storedReleaseRecords() store.PodReleaseRecords {
	r.t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, r.cfg.PostgresDSN)
	if err != nil {
		r.t.Fatalf("open the store to read the release records: %v", err)
	}
	defer st.Close()
	records, err := st.PodReleaseRecords(ctx, r.project, time.Time{})
	if err != nil {
		r.t.Fatalf("read the release records: %v", err)
	}
	return records
}

// idleRigReleaseOf is the one release records hold of the issue's pod, failing the test when they
// hold none or more than one.
func idleRigReleaseOf(t *testing.T, records store.PodReleaseRecords, issue idleRigIssue) store.PodRelease {
	t.Helper()
	var releases []store.PodRelease
	for _, release := range records.Releases {
		if release.Issue == issue.key {
			releases = append(releases, release)
		}
	}
	if len(releases) != 1 {
		t.Fatalf("the releases of %s are %+v, want the one the idle tick made", issue.key, releases)
	}
	return releases[0]
}
