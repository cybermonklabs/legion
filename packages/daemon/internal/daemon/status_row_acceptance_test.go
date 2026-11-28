package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/issuelock"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A status row queued while the issue stood in accepting with Dispatch at its sign-off's retro —
// the status repair's needs_review — runs after the sign-off's fact has recorded the acceptance.
// Dispatch shows the retro the daemon records, so the row writes nothing: written, it would show
// the issue unaccepted while the daemon records it accepted. The control is the same row while the
// record still holds the issue in accepting, which writes.
func TestAStatusRowWritesNothingOverAnAcceptanceTheRecordHolds(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, tc := range []struct {
		key    string
		at     phase.Phase
		writes []string
	}{
		{key: "LEGION-781", at: phase.Retro},
		{key: "LEGION-782", at: phase.Merging},
		{key: "LEGION-783", at: phase.Accepting, writes: []string{"needs_review"}},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			putOutboxIssue(t, pool, records, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: tc.at, Generation: 1, Status: "retro", DispatchStatus: "retro"})
			client := &outboxDispatch{issue: dispatch.Issue{Key: tc.key, Status: "retro", LastSeq: 9}}
			runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: &issuelock.Locks{}}
			if err := runner.execute(context.Background(), mustOutboxRow(t, tc.key, record.OrdinaryStatusWrite("needs_review", "retro", ""), time.Now())); err != nil {
				t.Fatalf("execute the row: %v", err)
			}
			if !slices.Equal(client.statuses, tc.writes) {
				t.Fatalf("Dispatch writes with the record in %s = %q, want %q", tc.at, client.statuses, tc.writes)
			}
		})
	}
}

// The set-back a close_root queues before its write (the API's queueSetBack) runs after the close's
// fact when nothing dropped it. Dispatch shows the close's done and the record holds the issue done,
// so the row writes nothing: written, it would reopen the closed root, or re-admit its tree. The
// control is the same row while the record still runs the issue, the close's fact refused and its
// write back failed, which writes the old status back.
func TestASetBackWritesNothingOverACloseTheRecordHolds(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, tc := range []struct {
		name, key, status string
		at                phase.Phase
		writes            []string
	}{
		{name: "the close recorded", key: "LEGION-785", at: phase.Done, status: "done"},
		{name: "control: the close refused", key: "LEGION-786", at: phase.Admitted, status: "in_progress", writes: []string{"in_progress"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putOutboxIssue(t, pool, records, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: tc.at, Generation: 1, Status: tc.status, DispatchStatus: "done"})
			client := &outboxDispatch{issue: dispatch.Issue{Key: tc.key, Status: "done", LastSeq: 9}}
			runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: &issuelock.Locks{}}
			if err := runner.execute(context.Background(), mustOutboxRow(t, tc.key, record.ReassertStatusWrite("in_progress", "done", 9), time.Now())); err != nil {
				t.Fatalf("execute the row: %v", err)
			}
			if !slices.Equal(client.statuses, tc.writes) {
				t.Fatalf("Dispatch writes with the record in %s = %q, want %q", tc.at, client.statuses, tc.writes)
			}
		})
	}
}

// A status row of an issue whose acceptance holds its lock waits for the acceptance to end before
// it reads or writes anything of Dispatch, so it never writes between the acceptance's own write
// and its fact. The row is seen blocked on the issue's lock through issuelock.Locks.Waiting, so
// the test times no window: either the row runs while the acceptance holds the lock, which fails
// at once, or it is seen waiting on that lock.
func TestAStatusRowWaitsForAnAcceptanceOfItsIssue(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const key = "LEGION-784"
	putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: key, Phase: phase.Testing, Generation: 1, Status: "testing", DispatchStatus: "in_progress"})
	client := &outboxDispatch{issue: dispatch.Issue{Key: key, Status: "in_progress", LastSeq: 4}}
	waiting := make(chan string, 1)
	locks := &issuelock.Locks{Waiting: func(issue string) { waiting <- issue }}
	runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: locks}

	accepted := locks.Accept(key)
	done := make(chan error, 1)
	go func() {
		done <- runner.execute(context.Background(), mustOutboxRow(t, key, record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now()))
	}()
	select {
	case err := <-done:
		accepted()
		t.Fatalf("the row ran while an acceptance of its issue held the lock: %v, writes %q", err, client.statuses)
	case issue := <-waiting:
		if issue != key {
			accepted()
			t.Fatalf("the row waited on the lock of %s, want %s", issue, key)
		}
	case <-time.After(time.Minute):
		accepted()
		t.Fatal("the row neither ran nor waited on its issue's lock within a minute")
	}
	if len(client.statuses) != 0 {
		accepted()
		t.Fatalf("the row wrote %q before it waited on its issue's lock", client.statuses)
	}
	accepted()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute the row: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the row did not run within a minute of the acceptance's end")
	}
	if want := []string{"testing"}; !slices.Equal(client.statuses, want) {
		t.Fatalf("Dispatch writes = %q, want %q once the acceptance ended", client.statuses, want)
	}
}

// A close_root's own done event, through the handlers a live delivery runs (the mirror, the engine
// and admission), leaves the root in the workflow at the status the record holds, with Dispatch's
// done observed: admission stops at the sequence the engine recorded and writes no done over the
// record. The close's fact then decides, and takes the tree out of the workflow. The control is a
// person's done, which leaves the tree on its event.
func TestACloseRootsOwnDoneEventKeepsTheTreeThroughAdmission(t *testing.T) {
	t.Parallel()
	for _, daemonWrote := range []bool{true, false} {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		w := tickRuntimeIn(t, pool, &tickDispatch{}, "LEGION")
		seedTickIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
			Title: "Root", Tree: "LEGION-1", Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A",
			HandedOver: true, LastDispatchSeq: 1})
		// The root holds its slot and its tree runs, as an admitted root does, so admission's pass
		// leaves it where it stands.
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := record.NewStore().PutSlot(ctx, tx, record.Slot{Issue: "LEGION-1", Index: 0, AdmittedAt: time.Now()}); err != nil {
				return err
			}
			return openTreeTx(ctx, tx, "LEGION", "LEGION-1")
		}); err != nil {
			t.Fatalf("give LEGION-1 its slot and open its tree: %v", err)
		}
		done := intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: "Root", Rank: "A",
			HandedOver: true, DaemonActor: daemonWrote}
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", "done-event", done, w.handlers...); err != nil {
			t.Fatalf("apply the done event: %v", err)
		}
		got := recordOf(t, pool, record.NewStore(), "LEGION-1")
		if !daemonWrote {
			if record.InWorkflow(got) {
				t.Fatalf("after a person's done, LEGION-1 = phase %s status %s, want it out of the workflow", got.Phase, got.Status)
			}
			continue
		}
		if !record.InWorkflow(got) || got.Status != "in_progress" || got.DispatchStatus != "done" || got.LastDispatchSeq != 2 {
			t.Fatalf("after the close's own done event, LEGION-1 = phase %s status %s, Dispatch %s at %d; want it in the workflow at in_progress with done observed at 2",
				got.Phase, got.Status, got.DispatchStatus, got.LastDispatchSeq)
		}
		if result, err := intake.ApplyFact(ctx, pool, "api", "close", intake.CloseRoot{Issue: "LEGION-1", Reason: "moot"}, w.handlers...); err != nil || result.Refusal != nil {
			t.Fatalf("the close = %+v, %v; want it taken", result.Refusal, err)
		}
		if got := recordOf(t, pool, record.NewStore(), "LEGION-1"); record.InWorkflow(got) {
			t.Fatalf("after the close's fact, LEGION-1 = phase %s status %s, want it out of the workflow", got.Phase, got.Status)
		}
	}
}
