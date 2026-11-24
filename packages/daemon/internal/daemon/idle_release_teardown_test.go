package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
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
// recorded a release of a pod that still stands, armed a backstop over it and forgot its wake
// marks, and left the other roles released in a running pod with nothing to bring them back.
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
	var backstops int
	if err := rig.pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1`, rig.issue.Key).Scan(&backstops); err != nil {
		t.Fatal(err)
	}
	if backstops != 0 {
		t.Fatalf("backstops armed = %d, want none: a backstop belongs to a pod that is gone", backstops)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if rig.declined(t) {
		t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
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
// read-ahead of dispatch://INFRA-3589, D1). The teardown's record writes the release on the mark
// before any step that can fail. Here the teardown's last step fails, so its row runs again. That
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
// runtime has answered, and it can fail too: the runtime has taken the pod, and the mark names no
// release yet. A daemon that stops between the runtime's answer and the record leaves the store
// in the same state, and the daemon that replaces it runs the row again. Here the record fails
// once, and the row runs again after a claim of the pod was taken over or the pod was given work:
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
