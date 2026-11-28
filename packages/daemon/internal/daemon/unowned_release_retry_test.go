package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// podEndFaults is the release's store with the end of a pause's hold on the pod refused once
// (outbox.podDeleted), the first act after the runtime's answer of a teardown that records no
// release. Every other call reaches the real store.
type podEndFaults struct {
	outboxTrees
	refused bool
}

func (s *podEndFaults) EndPausePodHold(ctx context.Context, project, issue string, end store.PausePodHoldEnd, at time.Time) (bool, error) {
	if !s.refused {
		s.refused = true
		return false, errors.New("the pause's hold on the pod cannot be read")
	}
	return s.outboxTrees.EndPausePodHold(ctx, project, issue, end, at)
}

// holdTeardownBack holds the issue's teardown row back an hour, so the outbox's drain leaves it
// and the test runs each of its attempts itself (teardownDueAgain makes the row due).
func holdTeardownBack(t *testing.T, rig *idleRig) {
	t.Helper()
	if _, err := rig.pool.Exec(context.Background(), `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
}

// teardownDueAgain makes the issue's teardown row due again, as its backoff leaves it. died
// leaves the row as a daemon that died in the failed attempt leaves it: with no attempt counted,
// since only a run's retry counts one (record.QueuedSupervise.Attempts).
func teardownDueAgain(t *testing.T, rig *idleRig, died bool) {
	t.Helper()
	if _, err := rig.pool.Exec(context.Background(), `update outbox set next_at = now() - interval '1 minute',
		attempts = case when $2 then 0 else attempts end where issue = $1 and kind = 'issue_release'`, rig.issue.Key, died); err != nil {
		t.Fatalf("make the teardown due again: %v", err)
	}
}

// retriedTeardownRecorded checks what the retried teardown left: its mark cleared, so it finished,
// and want releases recorded.
func retriedTeardownRecorded(t *testing.T, rig *idleRig, want int) {
	t.Helper()
	if rig.marked(t) {
		t.Fatal("the retried teardown left its mark behind, so it never finished")
	}
	if releases := rig.records(t).Releases; len(releases) != want {
		t.Fatalf("releases recorded after the retry = %+v, want %d", releases, want)
	}
}

// A retried teardown of the release of a pod no claim owns records what the release did, as
// store.ReleaseTeardown's rule has it, whatever the retry itself finds: an attempt that took the
// pod and failed before its record, counted or with its daemon dead in it, leaves the retry no
// Sandbox to take, and the release is recorded once; an attempt that found no Sandbox and failed
// after the runtime's answer leaves nothing noted, and its retry records none. The first fails in
// the runtime, after the runtime's first write (releaseRuntime.failsAfterAuthorize), so the mark
// says `reached` and no record names it. Its retry asks the runtime again, finds the Sandbox gone,
// and records the release because the mark's note says so (#5372's path). The second fails at the
// end of the pod's hold (podEndFaults), the first step of a teardown that records nothing.
func TestARetriedTeardownOfAPodNoClaimOwnsRecordsWhatTheReleaseDid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// foundNothing is a runtime that holds no Sandbox of the issue from the first attempt on
		// (releaseRuntime.sandboxGone), whose teardown fails at the end of the pod's hold;
		// otherwise the first attempt takes the pod and its runtime fails after its first write, and
		// the Sandbox is gone from then on.
		foundNothing bool
		// died leaves the failed attempt as a daemon that died in it leaves its row: not counted.
		died bool
		// failure is the error the failed attempt's row logs.
		failure string
		// note is what the mark says of the teardown after the failed attempt.
		note store.ReleaseTeardown
		// records is how many releases the retry leaves recorded, and line the one line it logs.
		records int
		line    string
	}{
		{name: "an attempt that took the pod and failed before its record",
			failure: "the Secrets of the issue's Sandbox could not be deleted", note: store.TeardownReached, records: 1, line: idleRigUnownedTeardown},
		{name: "an attempt that took the pod and whose daemon died before its record", died: true,
			failure: "the Secrets of the issue's Sandbox could not be deleted", note: store.TeardownReached, records: 1, line: idleRigUnownedTeardown},
		{name: "an attempt that found no Sandbox and failed after the runtime's answer", foundNothing: true,
			failure: "the pause's hold on the pod cannot be read", note: store.TeardownNotReached, records: 0, line: idleRigUnownedFoundNothing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			var logs syncBuffer
			rig.runner.log = jsonLogger(&logs)
			gone := func() {
				rig.rt.mu.Lock()
				defer rig.rt.mu.Unlock()
				rig.rt.sandboxGone = true
			}
			faults := &podEndFaults{outboxTrees: rig.st}
			if tc.foundNothing {
				rig.runner.trees = faults
			} else {
				rig.rt.mu.Lock()
				rig.rt.failsAfterAuthorize = 1
				rig.rt.mu.Unlock()
			}
			rig.stoppedImplementer(t, false)
			if queued := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(ctx) }); !slices.Contains(queued, "issue_release") {
				t.Fatalf("queued rows = %v, want the release's teardown among them", queued)
			}
			if tc.foundNothing {
				gone()
			}

			if err := rig.runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the teardown once: %v", err)
			}
			// The attempt failed where the row says: before the record, or at the end of the pod's
			// hold of a teardown that records nothing. Either way no release is recorded yet.
			failed := loggedMessages(t, logs.String(), "outbox row failed")
			if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0]["error"]), tc.failure) || faults.refused != tc.foundNothing {
				t.Fatalf("the failed attempt's lines = %v and the end of the pod's hold was refused = %t; want one failure naming %q, refused only for a teardown that found nothing",
					failed, faults.refused, tc.failure)
			}
			if releases := rig.records(t).Releases; len(releases) != 0 {
				t.Fatalf("releases recorded by the failed attempt = %+v, want none: it failed before any record", releases)
			}
			var attempts int
			var note string
			if err := rig.pool.QueryRow(ctx, `select o.attempts, r.teardown from outbox o join issue_releases r on r.issue = o.issue
				where o.issue = $1 and o.kind = 'issue_release'`, rig.issue.Key).Scan(&attempts, &note); err != nil {
				t.Fatalf("read the failed teardown and its mark: %v", err)
			}
			if attempts != 1 || store.ReleaseTeardown(note) != tc.note {
				t.Fatalf("after the failed attempt the teardown has %d attempts and its mark says %q, want 1 and %q", attempts, note, tc.note)
			}
			// The Sandbox is gone before the retry, so the retry's runtime takes nothing.
			gone()
			teardownDueAgain(t, rig, tc.died)
			rig.drain(t)

			retriedTeardownRecorded(t, rig, tc.records)
			// The retry asked the runtime again: no record named the mark, so it did not read the
			// mark as a release that took its pod (store.ReleaseDecision.PodTaken).
			asked := rig.rt.teardownsAsked()
			if !slices.Equal(asked, []string{"ReleaseIssue", "ReleaseIssue"}) {
				t.Fatalf("the runtime was asked %v, want the release's teardown twice: the retry found no record and asked again", asked)
			}
			t.Logf("the failed attempt logged %q and left the note %q and no record; the retry asked the runtime %v and recorded %d releases",
				failed[0]["error"], note, asked, tc.records)
			for _, line := range []string{idleRigUnownedTeardown, idleRigUnownedFoundNothing} {
				want := 0
				if line == tc.line {
					want = 1
				}
				if got := loggedMessages(t, logs.String(), line); len(got) != want {
					t.Fatalf("the teardown logged %d lines %q, want %d", len(got), line, want)
				}
			}
		})
	}
}

// clearFaults is the release's store with the clear of a release's mark refused once
// (ClearIssueRelease), the last act of a teardown, after its record. Every other call reaches the
// real store.
type clearFaults struct {
	outboxTrees
	refused bool
}

func (s *clearFaults) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if !s.refused {
		s.refused = true
		return errors.New("the release's mark cannot be cleared")
	}
	return s.outboxTrees.ClearIssueRelease(ctx, project, issue)
}

// A teardown that recorded its release and then failed to clear the mark — a refused write, or a
// daemon that died between the two, which leaves the store as the refusal does — runs again over
// the standing mark, and the release is still recorded once: the record names the mark, and the
// second write takes the first one's (store.RecordPodRelease). Both release rules record their
// release this way: the retry reads the mark as a release that took its pod, because a record
// names the mark (store.ReleaseDecision.PodTaken), so it asks the runtime nothing, and its record
// takes the first one's. The release line follows the record, so the retry logs it no second
// time. A record whose write fails before workflow.WakeHeldForRelease after the mark is the
// teardown's error (outbox.recordPodRelease, outbox.recordGivenUp): that attempt stops before its
// later steps, so it records nothing, logs no release line and no warning of its own, never
// reaches the clear and leaves the mark, and the row's failure line names the record's error once.
// The next attempt records the release and logs the release line, and its clear fails, so the
// attempt after it takes that record and logs the line no second time.
func TestATeardownRetriedAfterItsRecordRecordsTheReleaseOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	queueUnowned := func(t *testing.T, rig *idleRig) {
		rig.stoppedImplementer(t, false)
		rig.w.releaseUnownedPods(ctx)
	}
	for _, release := range []struct {
		name  string
		queue func(t *testing.T, rig *idleRig)
		// line is the release line the teardown logs once it has recorded the release.
		line         string
		refuseRecord bool
	}{
		{
			name:  "the idle release",
			queue: func(_ *testing.T, rig *idleRig) { rig.w.releaseIdlePod(ctx, rig.issue.Key) },
			line:  idleRigIdleTeardown,
		},
		{
			name:  "the release of a pod no claim owns",
			queue: queueUnowned,
			line:  idleRigUnownedTeardown,
		},
		{
			name:         "the release of a pod no claim owns whose record is refused once",
			queue:        queueUnowned,
			line:         idleRigUnownedTeardown,
			refuseRecord: true,
		},
	} {
		t.Run(release.name, func(t *testing.T) {
			rig := newIdleRig(t)
			var logs syncBuffer
			rig.runner.log = jsonLogger(&logs)
			faults := &clearFaults{outboxTrees: rig.st}
			rig.runner.trees = faults
			if queued := rig.queuedBy(t, func() { release.queue(t, rig) }); !slices.Contains(queued, "issue_release") {
				t.Fatalf("queued rows = %v, want the release's teardown among them", queued)
			}
			if release.refuseRecord {
				// Refuse the real store's insert until the first attempt has run.
				if _, err := rig.pool.Exec(ctx, `alter table pod_releases add constraint test_refuses_release_record check (false)`); err != nil {
					t.Fatalf("refuse the release's record: %v", err)
				}
			}
			rig.drain(t)
			if release.refuseRecord {
				if faults.refused {
					t.Fatal("the teardown reached the clear of its mark, though its record failed")
				}
				if !rig.marked(t) {
					t.Fatal("the release's mark is gone though its teardown's record failed")
				}
				if releases := rig.records(t).Releases; len(releases) != 0 {
					t.Fatalf("releases recorded after the refused write = %+v, want none", releases)
				}
				if got := loggedMessages(t, logs.String(), release.line); len(got) != 0 {
					t.Fatalf("the teardown whose record failed logged %d lines %q, want none: the line follows the record", len(got), release.line)
				}
				// The record's error is logged once, by the row's failure, and by no line of its own.
				refused := loggedLines(t, logs.String(), "test_refuses_release_record")
				if len(refused) != 1 || refused[0]["msg"] != "outbox row failed" || refused[0]["kind"] != "issue_release" {
					t.Fatalf("the lines that name the refused record = %v, want one: the teardown row's failure", refused)
				}
				if _, err := rig.pool.Exec(ctx, `alter table pod_releases drop constraint test_refuses_release_record`); err != nil {
					t.Fatalf("allow the retry to record the release: %v", err)
				}
				teardownDueAgain(t, rig, false)
				rig.drain(t)
			}
			if !faults.refused {
				t.Fatal("the teardown never reached the clear of its mark")
			}
			var mark int64
			if err := rig.pool.QueryRow(ctx, `select id from issue_releases where issue = $1`, rig.issue.Key).Scan(&mark); err != nil {
				t.Fatalf("the mark is gone though its clear was refused: %v", err)
			}
			if releases := rig.records(t).Releases; len(releases) != 1 || releases[0].Release != mark {
				t.Fatalf("releases recorded before the retry = %+v, want the one the attempt that reached the clear wrote, naming mark %d", releases, mark)
			}

			teardownDueAgain(t, rig, false)
			rig.drain(t)

			retriedTeardownRecorded(t, rig, 1)
			if got := loggedMessages(t, logs.String(), release.line); len(got) != 1 {
				t.Fatalf("the teardown and its retries logged %d lines %q, want 1", len(got), release.line)
			}
		})
	}
}
