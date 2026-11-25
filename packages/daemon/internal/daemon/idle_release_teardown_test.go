package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The teardown of an idle release (outbox.releaseIssue): the answer it acts on, and a teardown
// whose step fails after the runtime took the pod, which runs again and finishes. The rig is
// idle_release_test.go's.

// The teardown acts on the answer the runtime took under its pod lock, not on the one this row
// read before it. The two differ: the release's rows run on the issue's lifecycle lane, and a
// notice or a hook prompt runs on a lane of its own, so the wake one of those makes can
// resume a role in between. A teardown that took the first answer and ignored the second
// recorded a release of a pod that still stands and forgot its wake marks, and left the other
// roles released in a running pod with nothing to bring them back.
func TestATeardownTheRuntimeFindsUnauthorizedCancelsTheRelease(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// Under the pod lock, before the store is asked: a re-admission moved the issue's run on,
	// which is a close's business and not a release's.
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if refused := rig.rt.refusals(); refused != 1 {
		t.Fatalf("authorizations refused under the pod lock = %d, want the one the teardown asked for", refused)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the store refused the pod", suspended)
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the pod still stands", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldUnauthorized.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldUnauthorized)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have resumed it into the pod it kept",
			got.State, got.SuspendedBy)
	}
}

// clearRefusedOnce is the daemon's tree store with the teardown's last step refused once: the
// first ClearIssueRelease answers an error, as a store that lost its connection would, and every
// later one runs.
type clearRefusedOnce struct {
	outboxTrees
	refused *bool
}

func (t clearRefusedOnce) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if !*t.refused {
		*t.refused = true
		return errors.New("the pool is gone")
	}
	return t.outboxTrees.ClearIssueRelease(ctx, project, issue)
}

// A release's teardown is one-way once the runtime has taken the pod (the reviewer's second
// read-ahead of dispatch://INFRA-3589, D1). The teardown's record names the release's mark before
// any step that can fail. Here the teardown's last step fails, so its row runs again. That
// attempt finishes the release whatever happened meanwhile: it asks the runtime nothing, cancels
// nothing, and records no second release. An attempt that read the mark again would cancel a
// release whose pod was gone where an operator took a claim over in between, and that cancel would
// resume the role in a pod that is gone. An attempt with nothing new would take the pod a second
// time and record a second release.
func TestATeardownThatFailsAfterItsPodIsGoneFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		takeOver bool
	}{
		{name: "an operator took a claim over between the attempts", takeOver: true},
		{name: "nothing changed between the attempts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			refused := false
			rig.runner.trees = clearRefusedOnce{outboxTrees: rig.st, refused: &refused}
			rig.w.releaseIdlePod(ctx, rig.issue.Key)
			if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key); err != nil {
				t.Fatalf("hold the teardown back: %v", err)
			}
			rig.drain(t)
			if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
				t.Fatalf("the claim = %s by %s and the mark stands = %t; want the release checked and stopped, its teardown to come",
					got.State, got.SuspendedBy, rig.marked(t))
			}
			var teardown record.OutboxRow
			if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
				t.Fatalf("read the teardown row: %v", err)
			}
			if err := rig.runner.execute(ctx, teardown); err == nil || !refused {
				t.Fatalf("the teardown's first attempt = %v, want the refused last step's error", err)
			}
			if _, taken := rig.rt.reads(); taken != 1 || !rig.marked(t) {
				t.Fatalf("after the first attempt the runtime took %d pods and the mark stands = %t; want the pod taken once and the mark left for the next attempt",
					taken, rig.marked(t))
			}
			if tc.takeOver {
				if _, err := rig.pool.Exec(ctx, `update claims set suspended_by = 'operator' where token = $1`, string(rig.token)); err != nil {
					t.Fatalf("take the implementer's claim over as an operator: %v", err)
				}
			}

			if err := rig.runner.execute(ctx, teardown); err != nil {
				t.Fatalf("the teardown's next attempt: %v", err)
			}
			records := rig.records(t)
			if _, taken := rig.rt.reads(); rig.marked(t) || len(records.Releases) != 1 || taken != 1 {
				t.Fatalf("after the next attempt the mark stands = %t, the releases = %+v and the runtime took %d pods; want the release finished once: no mark, one release and one pod taken",
					rig.marked(t), records.Releases, taken)
			}
		})
	}
}

// The runtime's read under the pod's launch turn is the teardown's point of no return: the runtime
// takes the pod on it. So that read commits the release where it acts (store's
// CommitIssueRelease), before the runtime's later steps. Here a later step of the runtime fails
// after the pod is gone, which leaves no record of the release, and the row runs again after a
// claim of the pod was taken over or the pod was given work. That attempt finishes the release: it
// asks the runtime again, records the release once, and cancels nothing. An attempt that read the
// mark again would cancel a release whose pod was gone, and that cancel would resume the role in a
// pod that is gone.
func TestATeardownWhoseRuntimeFailsAfterItTookThePodFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range teardownInterleavings {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.rt.failsAfterAuthorize = 1
			teardown := rig.heldTeardown(t)
			if err := rig.runner.execute(ctx, teardown); err == nil {
				t.Fatal("the teardown's first attempt succeeded, want the runtime's error after it took the pod")
			}
			if records := rig.records(t); len(records.Releases) != 0 || !rig.marked(t) {
				t.Fatalf("after the first attempt the releases = %+v and the mark stands = %t; want no record and the mark left", records.Releases, rig.marked(t))
			}
			tc.between(t, rig)

			if err := rig.runner.execute(ctx, teardown); err != nil {
				t.Fatalf("the teardown's next attempt: %v", err)
			}
			records := rig.records(t)
			asked := rig.rt.teardownsAsked()
			if rig.marked(t) || len(records.Releases) != 1 || !slices.Equal(asked, []string{"ReleaseIssue", "ReleaseIssue"}) {
				t.Fatalf("after the next attempt the mark stands = %t, the releases = %+v and the runtime was asked %v; want the release finished: no mark, one release, and the runtime asked again",
					rig.marked(t), records.Releases, asked)
			}
		})
	}
}

// The record of a release (store.Store.RecordPodRelease) is the teardown's first step once the
// runtime has answered, and it can fail too: the runtime has taken the pod, and no record names
// the mark yet (pod_releases.issue_release_id). A daemon that stops between the runtime's answer
// and the record leaves the store in the same state, and the daemon that replaces it runs the row
// again. Here the record fails once, and the row runs again after a claim of the pod was taken
// over or the pod was given work:
// in the same daemon, and in a daemon restarted from the store alone (idleRig.restarted). The
// runtime's read committed the release (store's CommitIssueRelease), so that attempt finishes
// it: it asks the runtime again, records the release once, and cancels nothing. An attempt that
// read the mark again would cancel a release whose pod was gone, and that cancel would resume the
// role in a pod that is gone.
func TestATeardownWhoseRecordFailsOnceFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range teardownInterleavings {
		for _, restart := range []bool{false, true} {
			name := tc.name
			if restart {
				name += ", and the daemon restarted"
			}
			t.Run(name, func(t *testing.T) {
				rig := newIdleRig(t)
				ctx := context.Background()
				teardown := rig.heldTeardown(t)
				refusePodReleaseRecordOnce(t, rig.pool)
				if err := rig.runner.execute(ctx, teardown); err == nil {
					t.Fatal("the teardown's first attempt succeeded, want the error of its record")
				}
				if _, taken := rig.rt.reads(); taken != 1 || len(rig.records(t).Releases) != 0 || !rig.marked(t) {
					t.Fatalf("after the first attempt the runtime took %d pods, the releases = %+v and the mark stands = %t; want the pod taken once, no record and the mark left",
						taken, rig.records(t).Releases, rig.marked(t))
				}
				tc.between(t, rig)
				runner := rig.runner
				if restart {
					runner = rig.restarted(t)
				}

				if err := runner.execute(ctx, teardown); err != nil {
					t.Fatalf("the teardown's next attempt: %v", err)
				}
				records := rig.records(t)
				asked := rig.rt.teardownsAsked()
				if rig.marked(t) || len(records.Releases) != 1 || len(records.Holds) != 0 ||
					!slices.Equal(asked, []string{"ReleaseIssue", "ReleaseIssue"}) {
					t.Fatalf("after the next attempt the mark stands = %t, the releases = %+v, the holds = %+v and the runtime was asked %v; want the release finished: no mark, one release, no cancel's hold, and the runtime asked again",
						rig.marked(t), records.Releases, records.Holds, asked)
				}
			})
		}
	}
}

// teardownInterleavings are what can come between two attempts of a release's teardown, each of
// which cancels a release that no read of the runtime has committed: an operator's takeover of a
// claim of the pod, and a start of another role queued, which gives the pod work.
var teardownInterleavings = []struct {
	name string
	// between is what happens between the two attempts of the teardown.
	between func(t *testing.T, rig *idleRig)
}{
	{name: "an operator took a claim over", between: func(t *testing.T, rig *idleRig) {
		if _, err := rig.pool.Exec(context.Background(), `update claims set suspended_by = 'operator' where token = $1`,
			string(rig.token)); err != nil {
			t.Fatalf("take the implementer's claim over as an operator: %v", err)
		}
	}},
	{name: "a start of another role was queued", between: func(t *testing.T, rig *idleRig) {
		rig.startQueuedAt(t, rig.outboxID(t)+100, claim.RoleTester)
	}},
}

// heldTeardown runs the rig's idle release up to its teardown and answers the teardown's row. The
// row is held back an hour, so the drain leaves it, and the test runs each of its attempts itself.
func (r *idleRig) heldTeardown(t *testing.T) record.OutboxRow {
	t.Helper()
	ctx := context.Background()
	r.w.releaseIdlePod(ctx, r.issue.Key)
	if _, err := r.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		r.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	r.drain(t)
	if got := r.machine.Claim(); !got.Released() || !r.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t; want the release checked and stopped, its teardown to come",
			got.State, got.SuspendedBy, r.marked(t))
	}
	var teardown record.OutboxRow
	if err := r.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		r.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	return teardown
}

// refusePodReleaseRecordOnce makes the database refuse the next record of a release: a trigger
// on pod_releases raises an error at the first insert, so the record's transaction
// (store.Store.RecordPodRelease) rolls back. The trigger counts on a sequence, which no rollback
// takes back, so every later insert goes through.
func refusePodReleaseRecordOnce(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`create sequence refused_pod_release_records`,
		`create function refuse_pod_release_record_once() returns trigger language plpgsql as $$
		begin
			if nextval('refused_pod_release_records') = 1 then
				raise exception 'the record of the release could not be written';
			end if;
			return new;
		end $$`,
		`create trigger refuse_pod_release_record_once before insert on pod_releases
			for each row execute function refuse_pod_release_record_once()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("refuse the next record of a release: %v", err)
		}
	}
}

// R3's bound on a release (dispatch://INFRA-3589).
// A record of the release that cannot be written is the teardown's error: the mark stands and the
// row runs again, because the record tells the wakes taken under the mark the release they end.
// A release that cannot finish must not run for ever, so the retries end at
// workflow.WakeHeldForRelease after the mark. Here a trigger on pod_releases refuses every record
// (refusePodReleaseRecords), an earlier release of the pod is recorded before this one marks, and
// a CI settlement's wake of the implementer is taken under the mark with its start queued, as the
// workflow's wake leaves one (wakeReleased). Each row runs a first attempt a minute before the
// bound, which fails and leaves the mark, the wake's resume and its start as they were. Then:
//
//   - (a) is the test. The next attempt runs at the bound. It logs one ERROR line naming the issue,
//     the release and the record's error, records no release, clears the mark and finishes its
//     row. The wake's resume pairs with no release, keeps the release it saw as the issue's newest,
//     and is dated to the give-up. Its start is the role's one start: the role's next wake closes
//     no resume and takes none, and the drain resumes the role once. The control of that absence
//     follows it: once a later release of the issue is recorded, the same wake closes the resume
//     and takes its own.
//   - (a2) is the test of the give-up's order. The next attempt runs at the bound and gives up on
//     the record, and the end of the pod's hold under a pause (outbox.podDeleted), its next step,
//     fails once (podEndFaults). The settle is the teardown's last write, in the transaction that
//     clears the mark, so that attempt writes nothing of the give-up: it logs its row's failure and
//     no ERROR line, the mark stands, and the resume and the start are as the wake left them. The
//     attempt after it, a minute later and still past the bound, gives up again and ends as (a)
//     does, with the one ERROR line of the whole run.
//   - (b) is the control of (a) and (a2). The next attempt runs a second before the bound. It fails
//     as the first did: it logs its row's failure and no line of a give-up, the mark stands, the
//     row stays, the resume and the start are as the wake left them, and the role is not resumed
//     in the pod the teardown took.
func TestATeardownWhoseRecordFailsEveryTimeGivesUpAtTheBound(t *testing.T) {
	// holdEndRefused is the error of podEndFaults' refused end of the pod's hold.
	const holdEndRefused = "the pause's hold on the pod cannot be read"
	for _, tc := range []struct {
		name string
		// next is when the next attempt runs, after the release's mark.
		next time.Duration
		// givesUp is whether that attempt gives up on the record.
		givesUp bool
		// holdEndFails is whether that attempt's end of the pod's hold fails once, so the attempt
		// a minute after it is the one that finishes.
		holdEndFails bool
	}{
		{name: "(a) the test: the attempt at the bound gives up on the record", next: workflow.WakeHeldForRelease, givesUp: true},
		{name: "(a2) the test: an attempt at the bound whose next step fails writes nothing of the give-up, and the attempt after it gives up",
			next: workflow.WakeHeldForRelease, givesUp: true, holdEndFails: true},
		{name: "(b) the control: an attempt a second before the bound fails", next: workflow.WakeHeldForRelease - time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			var logs syncBuffer
			rig.runner.log = jsonLogger(&logs)
			earlier, _, err := rig.st.RecordPodRelease(ctx, store.PodRelease{Project: "legion", Issue: rig.issue.Key,
				Generation: 1, Roles: []string{string(claim.RoleImplementer)}, At: time.Now().UTC().Add(-2 * time.Hour)})
			if err != nil {
				t.Fatalf("record the earlier release: %v", err)
			}
			rig.heldTeardown(t)
			refusePodReleaseRecords(t, rig.pool)
			var mark int64
			var markedAt time.Time
			if err := rig.pool.QueryRow(ctx, `select id, marked_at from issue_releases where issue = $1`,
				rig.issue.Key).Scan(&mark, &markedAt); err != nil {
				t.Fatalf("read the release's mark: %v", err)
			}

			// The settlement's wake, taken after the release recorded the claim released and before
			// its teardown: its resume reads the earlier release, and its start waits for the
			// teardown row (record's outboxInOrder).
			takenAt := time.Now().UTC().Truncate(time.Microsecond)
			if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, takenAt, 0); err != nil || !taken {
				t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
			}
			const detail = "head1 failed build"
			start, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
				Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByRelease),
				WakeKind: pods.WakeChecks, WakeDetail: detail}, time.Now().UTC())
			if err != nil {
				t.Fatalf("encode the wake's start: %v", err)
			}
			if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
				values ($1, $2, $3, 0, now(), '', '')`, string(start.Kind), rig.issue.Key, string(start.Payload)); err != nil {
				t.Fatalf("queue the wake's start: %v", err)
			}
			woken := wokenResume{release: earlier, seen: earlier, at: takenAt, clock: takenAt}
			resumes := len(rig.rt.CallsOf("Resume"))

			// attempt runs the teardown row once, with the outbox's clock at after past the mark, and
			// answers the lines that run logged.
			attempt := func(after time.Duration) string {
				t.Helper()
				rig.runner.now = func() time.Time { return markedAt.Add(after) }
				teardownDueAgain(t, rig, false)
				before := len(logs.String())
				if err := rig.runner.RunOnce(ctx); err != nil {
					t.Fatalf("run the teardown: %v", err)
				}
				return logs.String()[before:]
			}
			// failing checks a teardown whose attempts all failed: the row kept with that many
			// attempts, its failure logged once naming cause, the mark standing, no line of a
			// give-up, no record of the release, the wake's resume and its start as the wake left
			// them, and the role not resumed.
			failing := func(attempts int, logged, cause string) {
				t.Helper()
				var kept int
				if err := rig.pool.QueryRow(ctx, `select attempts from outbox where issue = $1 and kind = 'issue_release'`,
					rig.issue.Key).Scan(&kept); err != nil || kept != attempts || !rig.marked(t) {
					t.Fatalf("the teardown row's attempts = %d (%v) and the mark stands = %t; want the row failed %d times and the mark left",
						kept, err, rig.marked(t), attempts)
				}
				failed := loggedLines(t, logged, cause)
				if len(failed) != 1 || failed[0]["msg"] != "outbox row failed" || failed[0]["kind"] != "issue_release" {
					t.Fatalf("the attempt's lines that name %q = %v, want one: the teardown row's failure", cause, failed)
				}
				if given := loggedMessages(t, logs.String(), idleRigRecordGivenUp); len(given) != 0 {
					t.Fatalf("the teardown logged %v, want no line of a teardown that gave up on its record", given)
				}
				if releases := rig.records(t).Releases; len(releases) != 1 || releases[0].ID != earlier {
					t.Fatalf("releases = %+v, want the earlier release %d alone", releases, earlier)
				}
				rig.wantImplementerResume(t, woken)
				if starts := rig.implementerStarts(t); starts != 1 {
					t.Fatalf("the implementer's queued starts = %d, want the wake's one start, waiting for the teardown", starts)
				}
				if got := rig.machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
					t.Fatalf("the claim = %s by %s after %d resumes; want it released and not resumed in the pod the teardown took",
						got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
				}
			}

			failing(1, attempt(workflow.WakeHeldForRelease-time.Minute), podReleasesRefused)
			if tc.holdEndFails {
				rig.runner.trees = &podEndFaults{outboxTrees: rig.st}
			}
			logged := attempt(tc.next)
			if !tc.givesUp {
				failing(2, logged, podReleasesRefused)
				return
			}
			// gaveUp is when the attempt that gives up runs, which is the time the settle writes.
			gaveUp := markedAt.Add(tc.next)
			if tc.holdEndFails {
				failing(2, logged, holdEndRefused)
				gaveUp = gaveUp.Add(time.Minute)
				logged = attempt(tc.next + time.Minute)
			}

			errorLines := loggedLines(t, logged, `"level":"ERROR"`)
			if len(errorLines) != 1 || !strings.HasPrefix(fmt.Sprint(errorLines[0]["msg"]), idleRigRecordGivenUp) ||
				errorLines[0]["issue"] != rig.issue.Key || errorLines[0]["release"] != float64(mark) ||
				!strings.Contains(fmt.Sprint(errorLines[0]["error"]), podReleasesRefused) {
				t.Fatalf("the attempt that gave up logged the ERROR lines %v; want one, %q, naming issue %s, release %d and the record's error %q",
					errorLines, idleRigRecordGivenUp, rig.issue.Key, mark, podReleasesRefused)
			}
			if given := loggedMessages(t, logs.String(), idleRigRecordGivenUp); len(given) != 1 {
				t.Fatalf("the teardown logged %v, want the one ERROR line of the attempt that gave up", given)
			}
			if failed := loggedMessages(t, logged, "outbox row failed"); len(failed) != 0 {
				t.Fatalf("the attempt that gave up logged %v, want no failure of its row", failed)
			}
			var rows int
			if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key).Scan(&rows); err != nil || rows != 0 || rig.marked(t) {
				t.Fatalf("after the attempt that gave up the teardown rows = %d (%v) and the mark stands = %t; want the row finished and the mark cleared",
					rows, err, rig.marked(t))
			}
			if releases := rig.records(t).Releases; len(releases) != 1 || releases[0].ID != earlier {
				t.Fatalf("releases = %+v, want the earlier release %d alone: the give-up writes no pod release", releases, earlier)
			}
			settled := wokenResume{release: 0, seen: earlier, at: gaveUp, clock: gaveUp}
			rig.wantImplementerResume(t, settled)
			if starts := rig.implementerStarts(t); starts != 1 {
				t.Fatalf("the implementer's queued starts = %d, want the wake's one start", starts)
			}

			if closed, taken := rig.implementerWake(t, gaveUp); closed != 0 || taken {
				t.Fatalf("the role's next wake before its start ran closed the resume of release %d and took a wake = %t; want neither: the wake's start is the role's one start",
					closed, taken)
			}
			// The drain runs the wake's start, which resumes the role once, in the new pod. The
			// start then waits to hand the role its task, since the rig's implementer still holds
			// its phase's task, so it can stay queued; it resumes nothing again.
			rig.drain(t)
			if got := rig.machine.Claim(); got.Released() || len(rig.rt.CallsOf("Resume")) != resumes+1 || rig.implementerStarts(t) > 1 {
				t.Fatalf("the claim after the drain = %s by %s after %d resumes, with %d starts queued; want it resumed once, by the wake's one start",
					got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes, rig.implementerStarts(t))
			}
			if closed, taken := rig.implementerWake(t, gaveUp); closed != 0 || taken {
				t.Fatalf("the role's next wake after its start ran closed the resume of release %d and took a wake = %t; want neither",
					closed, taken)
			}
			rig.wantImplementerResume(t, settled)

			// The control of the absence above: a later release of the issue is recorded, so the
			// pod the resume waits for went away again, and the same wake closes the resume and
			// takes its own.
			if _, err := rig.pool.Exec(ctx, `drop trigger refuse_pod_release_records on pod_releases`); err != nil {
				t.Fatalf("let a later release be recorded: %v", err)
			}
			later, _, err := rig.st.RecordPodRelease(ctx, store.PodRelease{Project: "legion", Issue: rig.issue.Key,
				Generation: 1, Roles: []string{string(claim.RoleImplementer)}, At: gaveUp.Add(time.Hour)})
			if err != nil {
				t.Fatalf("record a later release: %v", err)
			}
			if closed, taken := rig.implementerWake(t, gaveUp.Add(2*time.Hour)); closed != later || !taken {
				t.Fatalf("the wake after a later release closed the resume of release %d and took a wake = %t; want the resume closed under release %d and a wake taken",
					closed, taken, later)
			}
		})
	}
}

// idleRigRecordGivenUp is the start of the ERROR line a teardown logs when it gives up on a record
// of its release that could not be written (outbox.recordGivenUp).
const idleRigRecordGivenUp = "outbox: the record of a release could not be written within 15 minutes of its mark"

// podReleasesRefused is the error the trigger of refusePodReleaseRecords raises.
const podReleasesRefused = "pod_releases refuses every record"

// refusePodReleaseRecords makes the database refuse every record of a release: a trigger on
// pod_releases raises podReleasesRefused at each insert, so each record's transaction
// (store.Store.RecordPodRelease) rolls back.
func refusePodReleaseRecords(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`create function refuse_pod_release_records() returns trigger language plpgsql as $$
		begin
			raise exception '` + podReleasesRefused + `';
		end $$`,
		`create trigger refuse_pod_release_records before insert on pod_releases
			for each row execute function refuse_pod_release_records()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("refuse every record of a release: %v", err)
		}
	}
}

// wokenResume is what a test reads of the implementer's resume: the release it pairs with, the
// newest release it saw (pod_resumes.seen_release), its time and its stuck clock.
type wokenResume struct {
	release, seen int64
	at, clock     time.Time
}

// wantImplementerResume checks that the implementer has one resume, open and with no first turn,
// and that it is want.
func (r *idleRig) wantImplementerResume(t *testing.T, want wokenResume) {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `select release_id, seen_release, at, clock_at,
		closed_at is not null or first_turn_at is not null from pod_resumes where issue = $1 and role = 'implementer' order by id`,
		r.issue.Key)
	if err != nil {
		t.Fatalf("read the implementer's resumes: %v", err)
	}
	defer rows.Close()
	var got []wokenResume
	ended := false
	for rows.Next() {
		var resume wokenResume
		var done bool
		if err := rows.Scan(&resume.release, &resume.seen, &resume.at, &resume.clock, &done); err != nil {
			t.Fatalf("read the implementer's resumes: %v", err)
		}
		ended = ended || done
		got = append(got, resume)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the implementer's resumes: %v", err)
	}
	if len(got) != 1 || ended || got[0].release != want.release || got[0].seen != want.seen ||
		!got[0].at.Equal(want.at) || !got[0].clock.Equal(want.clock) {
		t.Fatalf("the implementer's resumes = %+v (one ended: %t), want one open resume %+v", got, ended, want)
	}
}

// implementerStarts is how many starts of the implementer the outbox holds.
func (r *idleRig) implementerStarts(t *testing.T) int {
	t.Helper()
	var starts int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'implementer'`, r.issue.Key).Scan(&starts); err != nil {
		t.Fatalf("count the implementer's starts: %v", err)
	}
	return starts
}

// implementerWake takes a backstop's wake of the implementer at at as the workflow's wake takes
// one (wakeReleased): in one transaction, the pause that holds it (PauseOfWake), the close of a
// resume whose pod went away (ClosePodGoneResume) and the take (TakePodWake). It answers the
// release the close named, 0 where it closed nothing, and whether the take took the wake.
func (r *idleRig) implementerWake(t *testing.T, at time.Time) (closed int64, taken bool) {
	t.Helper()
	ctx := context.Background()
	records, err := r.st.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		pause, _, err := records.PauseOfWake(ctx, tx, r.issue.Key, r.token, 0)
		if err != nil {
			return err
		}
		if closed, err = records.ClosePodGoneResume(ctx, tx, r.issue.Key, claim.RoleImplementer); err != nil {
			return err
		}
		taken, err = records.TakePodWake(ctx, tx, r.issue.Key, claim.RoleImplementer, pods.WakeBackstop, at, pause)
		return err
	}); err != nil {
		t.Fatalf("take the implementer's next wake: %v", err)
	}
	return closed, taken
}

// restarted is the rig's daemon after a restart: a supervisor that restores every claim the store
// holds, as the boot does (supervisor.restore), on the same store and runtime, and an outbox
// runner over it that holds nothing of what the daemon before it did.
func (r *idleRig) restarted(t *testing.T) *outbox {
	t.Helper()
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = r.rt, r.st, api.DaemonAPIVersion
	sup.store = r.st
	claims, err := r.st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims the restarted daemon restores: %v", err)
	}
	if _, err := sup.restore(ctx, claims); err != nil {
		t.Fatalf("restore the claims: %v", err)
	}
	return &outbox{
		pool: r.pool, records: r.issues, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: r.st,
		notices: &outboxPublisher{}, dispatch: &roleHoldersDispatch{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
}
