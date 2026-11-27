package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The outbox's delivery wake of a released role around a release (wakeForDelivery,
// store.Store.TakeDeliveryWake): what it does while a release's mark stands, while a cancel's start
// is queued, and when the role's open resume refuses its take. The rig is idle_release_test.go's.

// A wake the role's open resume refuses resumes a claim that is still released all the same, and
// this is the case that makes it necessary: the delivery wake commits its take and then resumes
// the claim in process, so a resume the store refuses, or a daemon that restarts between the two,
// leaves the role released with its resume open. Only a release closes an open resume, and a pod
// that is fully released is never released again, so without this the role's every later wake —
// a delivery, a CI settlement, a role message, its own backstop — would read that resume as a pod
// already on its way and resume nothing, for as long as the issue lives.
//
// The take is made here without the resume behind it, which is what both shapes leave in the
// record: a resume that failed after its take committed, and a daemon that came back up between
// the two and reads the claim released with the resume open. The wake that meets it takes no wake
// of its own — the resume already recorded is still the role's one open resume — and brings the
// claim back. No pause held that resume, so the wake says at Warn that it stands open with no
// process behind it (logRefusedByOpenResume).
func TestAWakeResumesAClaimLeftReleasedUnderAnOpenResume(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}

	taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeDelivery, time.Now().UTC(), 0)
	if err != nil || !taken {
		t.Fatalf("take the wake whose resume never ran = %t, %v; want it taken", taken, err)
	}
	resume, pause, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer)
	if err != nil || !open || pause != 0 {
		t.Fatalf("the open resume after that take = %d, pause %d, %t, %v; want the one it recorded, which no pause held", resume, pause, open, err)
	}
	logs := &testlog.Buffer{}
	rig.runner.log = slog.New(slog.NewJSONHandler(logs, nil))

	woken, _, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken {
		t.Fatalf("the wake meeting the open resume = %t, %v; want the claim woken", woken, err)
	}
	stuck := loggedLines(t, logs.String(), `"msg":"wake: a released pod's resume stands open with no process behind it; this wake resumes it"`)
	if len(stuck) != 1 || stuck[0]["level"] != "WARN" {
		t.Fatalf("the stuck resume's lines = %v, want one at WARN; log:\n%s", stuck, logs.String())
	}
	if held := loggedLines(t, logs.String(), "its pause's held wake"); len(held) != 0 {
		t.Fatalf("the wake logged %v, want no line of a pause's held wake: no pause held this resume", held)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the wake that met the open resume = %s by %s, want it resumed: no release will ever close that resume",
			got.State, got.SuspendedBy)
	}
	// The heal takes no wake of its own: the resume the first take recorded is still the one
	// open, so the records hold one resume of this release rather than two.
	again, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer)
	if err != nil || !open || again != resume {
		t.Fatalf("the open resume after the heal = %d, %t, %v; want the first take's %d", again, open, err, resume)
	}
	// And a wake that meets the open resume of a role whose process is running resumes nothing:
	// that pod is back, which is what the refusal means when the claim is no longer released.
	before := len(rig.rt.CallsOf("Resume"))
	woken, _, err = rig.runner.wakeForDelivery(ctx, rig.token, "a second notice of "+rig.issue.Key, 0)
	if err != nil || woken {
		t.Fatalf("the wake of a running claim = %t, %v; want it to wake nothing", woken, err)
	}
	if after := len(rig.rt.CallsOf("Resume")); after != before {
		t.Fatalf("resumes after the wake of a running claim = %d, want the %d it already had", after, before)
	}
}

// A workflow wake taken while a release stood (R3 of dispatch://INFRA-3589), after the release
// recorded the role, holds the role's one open resume, and its start waits for the release's
// teardown row (record's outboxInOrder). The teardown clears the release's mark before its row
// leaves the outbox, so a delivery's wake can meet that open resume before the start
// runs. That is a normal order of the release (store.Store.TakeDeliveryWake), so the delivery's
// wake says so at Debug and writes no Warn that the resume stands open with no process behind it.
// It resumes the role, as it does for every open resume over a released claim.
func TestADeliveryWakeMeetingAWakeWhoseStartIsQueuedLogsAtDebug(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || rig.marked(t) {
		t.Fatalf("the claim after the release = %s by %s and the mark stands = %t, want it released and the release finished",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build"}, time.Now().UTC().Add(time.Hour)))
	logs := &testlog.Buffer{}
	rig.runner.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	woken, _, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken {
		t.Fatalf("the wake meeting the open resume = %t, %v; want the claim woken", woken, err)
	}
	queued := loggedLines(t, logs.String(), `"msg":"wake: a released role's open resume is a wake whose start is still queued; this wake resumes the role"`)
	if len(queued) != 1 || queued[0]["level"] != "DEBUG" {
		t.Fatalf("the queued wake's lines = %v, want one at DEBUG; log:\n%s", queued, logs.String())
	}
	if stuck := loggedLines(t, logs.String(), "stands open with no process behind it"); len(stuck) != 0 {
		t.Fatalf("the wake logged %v, want no line of a resume with no process behind it: the wake's start is queued", stuck)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the wake = %s by %s, want it resumed", got.State, got.SuspendedBy)
	}
}

// A delivery for a role whose claim a release still running has recorded as `released` takes no
// wake. That release runs to its end whatever wakes the role, and the delivery's wake resumes the
// claim in process, so a resume now would launch the role in the pod the teardown is about to
// take. The row waits 30 seconds instead, as a routine wait, and reads the role again. Once the
// teardown has recorded the release and cleared its mark, the next attempt takes the wake against
// that release and resumes the role, in the new pod.
func TestADeliveryWakeWaitsForTheTeardownOfAReleaseThatRecordedItsRole(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// The teardown is not due yet, so the lane runs the workspace check and the implementer's stop,
	// and the release stands with the claim recorded.
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}

	resumes := len(rig.rt.CallsOf("Resume"))
	earliest := rig.runner.now().Add(30 * time.Second)
	woken, turn, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	latest := rig.runner.now().Add(30 * time.Second)
	at, until := wait.UntilAt(err)
	var routine interface{ Routine() bool }
	if woken || !turn.IsZero() || !until || at.Before(earliest) || at.After(latest) || !errors.As(err, &routine) || !routine.Routine() {
		t.Fatalf("the wake while the release runs = %t, %s, %v; want no wake and a routine wait until 30 seconds on: the release recorded the role",
			woken, turn, err)
	}
	if _, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || open {
		t.Fatalf("an open resume after the wake that waited = %t, %v; want none: the wake took nothing", open, err)
	}
	if got := rig.machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
		t.Fatalf("the claim after the wake that waited = %s by %s, resumes %d; want it released and not resumed in the old pod",
			got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
	}

	// The teardown runs: it records the release and clears the mark.
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("let the teardown run: %v", err)
	}
	rig.drain(t)
	records := rig.records(t)
	if rig.marked(t) || len(records.Releases) != 1 {
		t.Fatalf("the mark stands = %t and the releases = %+v, want the release finished and recorded once", rig.marked(t), records.Releases)
	}

	woken, turn, err = rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken || !turn.IsZero() {
		t.Fatalf("the wake once the teardown finished = %t, %s, %v; want the claim woken", woken, turn, err)
	}
	records = rig.records(t)
	if len(records.Resumes) != 1 || records.Resumes[0].Release != records.Releases[0].ID || records.Resumes[0].Kind != pods.WakeDelivery {
		t.Fatalf("resumes = %+v, want the one delivery wake of release %d", records.Resumes, records.Releases[0].ID)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the wake = %s by %s, want it resumed in the new pod", got.State, got.SuspendedBy)
	}
}

// The class search of dispatch://INFRA-3589 at 45fc14a3, entries 33 and 42. A cancel of a release
// queues a start for each claim it resumes and resumes none itself, so the claim reads `released`
// until that start runs, and its pod never went away (store.CancelIssueRelease). A delivery for
// that role in that time takes no wake: a wake would record a resume of a pod that no release
// took, and the records would count that pod's return. The row waits 30 seconds, as a routine
// wait, while the start is queued. The start resumes the role in the same pod, and the row's next
// attempt finds the claim live and publishes, so the role still gets its delivery.
func TestADeliveryWakeAfterACancelWaitsForTheCancelsStartAndRecordsNoResume(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	// The issue's run moves on, so the teardown cancels the release and queues the claim's resume.
	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	// The lane deletes a row it finished, and the cancel's start waits for the teardown row until
	// then (record's outboxInOrder).
	if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, teardown.ID); err != nil {
		t.Fatalf("finish the teardown row: %v", err)
	}
	if got := rig.machine.Claim(); rig.marked(t) || !got.Released() {
		t.Fatalf("after the teardown the mark stands = %t and the claim = %s by %s; want the release cancelled and the claim released until the cancel's start runs",
			rig.marked(t), got.State, got.SuspendedBy)
	}

	resumes := len(rig.rt.CallsOf("Resume"))
	earliest := rig.runner.now().Add(30 * time.Second)
	woken, turn, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	latest := rig.runner.now().Add(30 * time.Second)
	at, until := wait.UntilAt(err)
	var routine interface{ Routine() bool }
	if woken || !turn.IsZero() || !until || at.Before(earliest) || at.After(latest) || !errors.As(err, &routine) || !routine.Routine() ||
		!strings.Contains(err.Error(), "waits for the start queued for "+rig.issue.Key+"'s implementer") {
		t.Fatalf("the wake while the cancel's start is queued = %t, %s, %v; want no wake and a routine wait of 30 seconds for that start",
			woken, turn, err)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes after the wake that waited = %+v, want none: the release took no pod", records.Resumes)
	}
	if got := rig.machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
		t.Fatalf("the claim after the wake that waited = %s by %s, resumes %d; want it released, for the cancel's start to resume",
			got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
	}

	// The cancel's start runs and resumes the role in the pod that stayed. The row's next attempt
	// finds the claim live: it wakes nothing and hands its delivery over.
	rig.drain(t)
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the cancel's start = %s by %s with the rows %v queued, want it resumed", got.State, got.SuspendedBy, rig.rows(t))
	}
	woken, turn, err = rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || woken || !turn.IsZero() {
		t.Fatalf("the wake once the cancel's start ran = %t, %s, %v; want no wait: the claim is live, and the delivery goes to it", woken, turn, err)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes after the cancel's start = %+v, want none: the release took no pod", records.Resumes)
	}
}

// A tree root's architect starts at the tree's run, which a next change of the root leaves behind
// the root's own generation (record.Issue.GenerationOf). A cancelled release queued the architect's
// start at that run, as the daemon's builder writes it (releaseResume), and the claim reads
// `released` until that start runs. A delivery for the architect in that time looks for the start
// at the run it serves (deliveryWake, store.Store.TakeDeliveryWake). So it takes no wake, records
// no resume, resumes nothing in process and waits as a routine wait. The control is a tester of
// the same root, whose start serves the root's own generation.
func TestADeliveryWakeOfARootsArchitectWaitsForItsStartQueuedAtTheTreesRun(t *testing.T) {
	t.Parallel()
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			rig := newIdleRig(t)
			ctx := context.Background()
			machine := rig.releasedRole(t, role)
			if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
				t.Fatalf("move the root's generation on: %v", err)
			}
			root, err := rig.runner.issue(ctx, rig.issue.Key)
			if err != nil {
				t.Fatalf("read the root: %v", err)
			}
			if root.Tree != rig.issue.Key || root.Generation != 2 || root.TreeGeneration != 1 {
				t.Fatalf("the root after its next change = generation %d, tree's run %d; want 2 and 1: the case's premise",
					root.Generation, root.TreeGeneration)
			}
			row, err := rig.runner.releaseResume(rig.issue.Key, rig.issue.Tree)(role,
				store.IssueRun{Tree: root.Tree, Generation: root.Generation, TreeGeneration: root.TreeGeneration}, store.DroppedWake{})
			if err != nil {
				t.Fatalf("build the cancel's start of the %s: %v", role, err)
			}
			if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
				values ($1, $2, $3, 0, now(), '', '')`, row.Kind, rig.issue.Key, string(row.Payload)); err != nil {
				t.Fatalf("queue the cancel's start of the %s: %v", role, err)
			}

			resumes := len(rig.rt.CallsOf("Resume"))
			woken, turn, err := rig.runner.wakeForDelivery(ctx, mustClaimToken(t, rig.issue.Key, role), "a notice of "+rig.issue.Key, 0)
			var routine interface{ Routine() bool }
			if woken || !turn.IsZero() || !errors.As(err, &routine) || !routine.Routine() ||
				!strings.Contains(err.Error(), "waits for the start queued for "+rig.issue.Key+"'s "+string(role)) {
				t.Fatalf("the wake while the cancel's start of the %s is queued = %t, %s, %v; want no wake and a routine wait for that start",
					role, woken, turn, err)
			}
			if records := rig.records(t); len(records.Resumes) != 0 {
				t.Errorf("resumes after the wake that waited = %+v, want none: the release took no pod", records.Resumes)
			}
			if got := machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
				t.Errorf("the %s after the wake that waited = %s by %s, resumes %d; want it released, for the cancel's start to resume",
					role, got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
			}
		})
	}
}
