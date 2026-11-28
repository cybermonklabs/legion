package daemon

import (
	"context"
	"slices"
	"strings"
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

// lateLanding is outboxDispatch in front of which an act's request lands just before the
// set-back's fence posts: the issue moves to landed at the next sequence, and the fence takes the
// one after. With landed empty, nothing lands, and the fence alone moves the sequence.
type lateLanding struct {
	*outboxDispatch
	landed string
}

func (d *lateLanding) PostMessage(ctx context.Context, key, body string) error {
	if d.landed != "" {
		d.issue.Status, d.issue.LastSeq = d.landed, d.issue.LastSeq+1
	}
	d.issue.LastSeq++
	return d.outboxDispatch.PostMessage(ctx, key, body)
}

// The set-back an act queues before its request (the API's queueSetBack) settles the request from
// Dispatch alone, and never sends it again under its key, whose answer Dispatch keeps for a day:
//   - Dispatch still at the request's sequence, at the status before it: the request may still be
//     in flight, so the row posts a fence, after which no request fenced on that sequence lands, and
//     judges the read after it: a request that landed just before the fence is set back, and one
//     that did not leaves nothing to write;
//   - Dispatch past the request's sequence, at the request's status, a day later and with no key
//     the row could read: the request landed, and its status is set back;
//   - Dispatch past the request's sequence, at the status before it: the request never lands, and
//     nothing is written.
func TestASetBackSettlesItsRequestFromDispatchAlone(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, tc := range []struct {
		name, key, shows, landed string
		seq                      int64
		fences                   int
		writes                   []string
	}{
		{name: "at the request's sequence, it lands before the fence", key: "LEGION-787", shows: "in_progress", landed: "done", seq: 9, fences: 1, writes: []string{"in_progress"}},
		{name: "at the request's sequence, nothing lands", key: "LEGION-788", shows: "in_progress", seq: 9, fences: 1},
		{name: "past the request's sequence, landed, its key long expired", key: "LEGION-789", shows: "done", seq: 10, writes: []string{"in_progress"}},
		{name: "past the request's sequence, never landed", key: "LEGION-790", shows: "in_progress", seq: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putOutboxIssue(t, pool, records, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress"})
			client := &lateLanding{outboxDispatch: &outboxDispatch{issue: dispatch.Issue{Key: tc.key, Status: tc.shows, LastSeq: tc.seq}}, landed: tc.landed}
			runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: &issuelock.Locks{}}
			if err := runner.execute(context.Background(), mustOutboxRow(t, tc.key, record.ReassertStatusWrite("in_progress", "done", 9), time.Now())); err != nil {
				t.Fatalf("execute the row: %v", err)
			}
			if len(client.messages) != tc.fences || tc.fences > 0 && !strings.Contains(client.messages[0], "ends any chance that it lands later") {
				t.Fatalf("messages = %q, want %d fence", client.messages, tc.fences)
			}
			if !slices.Equal(client.statuses, tc.writes) || len(client.acceptanceWrites) != 0 {
				t.Fatalf("Dispatch writes %q and keyed resends %+v, want %q and no resend", client.statuses, client.acceptanceWrites, tc.writes)
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
