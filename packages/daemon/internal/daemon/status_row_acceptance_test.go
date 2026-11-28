package daemon

import (
	"context"
	"encoding/json"
	"net/http"
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

// An unfinished acceptance (record.AcceptanceStatusRow) is settled by sending its request again
// under its key, as Dispatch's keyed layer answers how the first attempt ended:
//   - landed, with the record holding no acceptance or close: the status before the act is
//     written back;
//   - landed, with the record holding the close: nothing is written;
//   - refused: the request never lands, and nothing is written;
//   - committed, with no status change of the daemon's after the request's sequence: the key
//     committed something else (a close's settlement), and nothing is written; with one: written
//     back as landed;
//   - unsettled: the row is attempted again, and nothing is written.
//
// Where Dispatch still stands at the request's sequence, the row posts a fence first, so a fresh
// run of a request that never reached Dispatch is refused rather than landing the very status the
// row undoes.
func TestAnUnfinishedAcceptanceIsSettledUnderItsKey(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	closedBy := func(key string) []json.RawMessage {
		return []json.RawMessage{issueEvent(key, 10, "issue.closed", dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(key)}, nil)}
	}
	refusal := func(code string) error {
		return &dispatch.Error{Status: http.StatusConflict, Code: code, Message: code, Echoed: true}
	}
	for _, tc := range []struct {
		name, key string
		recorded  string
		at        phase.Phase
		seq       int64
		answer    error
		events    func(string) []json.RawMessage
		writes    []string
		fences    int
		unsettled bool
	}{
		{name: "landed, the record holding no close", key: "LEGION-791", recorded: "in_progress", at: phase.Admitted, seq: 10, writes: []string{"in_progress"}},
		{name: "landed, the record holding the close", key: "LEGION-792", recorded: "done", at: phase.Done, seq: 10},
		{name: "refused", key: "LEGION-793", recorded: "in_progress", at: phase.Admitted, seq: 10, answer: refusal("ISSUE_CHANGED")},
		{name: "committed, no status change of the daemon's", key: "LEGION-794", recorded: "in_progress", at: phase.Admitted, seq: 10,
			answer: refusal(dispatch.IdempotencyKeyCommitted)},
		{name: "committed, the daemon's close in the log", key: "LEGION-795", recorded: "in_progress", at: phase.Admitted, seq: 10,
			answer: refusal(dispatch.IdempotencyKeyCommitted), events: closedBy, writes: []string{"in_progress"}},
		{name: "unsettled", key: "LEGION-796", recorded: "in_progress", at: phase.Admitted, seq: 10,
			answer: &dispatch.Error{Status: http.StatusConflict, Code: dispatch.IdempotencyKeyInFlight, Echoed: true}, unsettled: true},
		{name: "Dispatch still at the request's sequence, its fresh run refused after the fence", key: "LEGION-797", recorded: "in_progress", at: phase.Admitted, seq: 9,
			answer: refusal("ISSUE_CHANGED"), fences: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putOutboxIssue(t, pool, records, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: tc.at, Generation: 1, Status: tc.recorded, DispatchStatus: "done"})
			client := &outboxDispatch{issue: dispatch.Issue{Key: tc.key, Status: "done", LastSeq: tc.seq}}
			client.acceptance = func(dispatch.AcceptanceWrite) (dispatch.Issue, error) {
				return dispatch.Issue{Key: tc.key, Status: "done", LastSeq: 10}, tc.answer
			}
			if tc.events != nil {
				client.events = tc.events(tc.key)
			}
			runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: &issuelock.Locks{}}
			payload := record.AcceptanceStatusWrite("done", "in_progress", 9, "legion:close_root:"+tc.key+":k1")
			err := runner.settleAcceptance(context.Background(), record.OutboxRow{ID: 1, Issue: tc.key}, payload, client.issue)
			if tc.unsettled != (err != nil) {
				t.Fatalf("settle = %v, want unsettled %v", err, tc.unsettled)
			}
			if _, permanent := dispatch.PermanentRefusal(err); permanent {
				t.Fatalf("the unsettled answer %v reads as a permanent refusal, which would drop the row", err)
			}
			if len(client.acceptanceWrites) != 1 || client.acceptanceWrites[0] != (dispatch.AcceptanceWrite{Key: payload.Key, Issue: tc.key, Status: "done", Sequence: 9}) {
				t.Fatalf("keyed writes = %+v, want the one request again under its key", client.acceptanceWrites)
			}
			if !slices.Equal(client.statuses, tc.writes) || len(client.messages) != tc.fences {
				t.Fatalf("Dispatch writes %q and %d messages, want %q and %d", client.statuses, len(client.messages), tc.writes, tc.fences)
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
