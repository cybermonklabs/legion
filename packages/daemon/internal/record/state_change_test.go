// These tests live in the external test package so that they can read the state route's own
// projection (internal/projection), which imports this package: each state event is held to the
// view GET /legion/v1/state serves of the same store before and after the write.
package record_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

const (
	// changeProject is the Dispatch project of every issue these tests write, and so the issue
	// column of every state row they queue.
	changeProject = "LEGION"
	// changeRun is the run id the publishing store of these tests stamps on its rows.
	changeRun = "5e0c8a7d-2b4f-4d61-9c3e-8f1a6b2d7e94"
)

// routeFields is the four fields Dispatch observes of an issue, by field, each as the state route
// serves it at the issue: the JSON value, or null where the route serves none.
type routeFields map[record.StateField]string

// changeRig is one migrated store, and the record store that writes through it with the state
// events on (record.Postgres.PublishState).
type changeRig struct {
	st      *store.Store
	records *record.Postgres
}

func newChangeRig(t *testing.T) *changeRig {
	t.Helper()
	records := record.NewStore()
	records.PublishState(changeRun, nil)
	return &changeRig{st: testpg.CloneStore(t, "legion_record_test"), records: records}
}

// moved is what one transaction did: the route's view of its issue before and after it, and the
// state changes of that issue it queued, in the order it queued them.
type moved struct {
	before, after routeFields
	changes       []record.StateChange
}

// commit runs write in a transaction of its own and commits it, whatever write answers: a write
// the store refused (PutIssue's checks of the record it is handed) is committed too, so the test
// reads what such a write leaves in a transaction that goes on to commit.
func (r *changeRig) commit(t *testing.T, key string, write func(ctx context.Context, tx pgx.Tx)) moved {
	t.Helper()
	return r.run(t, key, write, true)
}

// rollBack runs write in a transaction of its own and rolls it back.
func (r *changeRig) rollBack(t *testing.T, key string, write func(ctx context.Context, tx pgx.Tx)) moved {
	t.Helper()
	return r.run(t, key, write, false)
}

func (r *changeRig) run(t *testing.T, key string, write func(ctx context.Context, tx pgx.Tx), commit bool) moved {
	t.Helper()
	return r.runOver(t, []string{key}, write, commit)[key]
}

// runOver runs write in a transaction of its own, which it commits or rolls back, and answers what
// that one transaction did to each issue of keys.
func (r *changeRig) runOver(t *testing.T, keys []string, write func(ctx context.Context, tx pgx.Tx), commit bool) map[string]moved {
	t.Helper()
	ctx := context.Background()
	before, queued := map[string]routeFields{}, map[string]int{}
	for _, key := range keys {
		before[key], queued[key] = r.view(t, key), len(r.changesOf(t, key))
	}
	tx, err := r.st.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	write(ctx, tx)
	if commit {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	} else if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	got := make(map[string]moved, len(keys))
	for _, key := range keys {
		got[key] = moved{before: before[key], after: r.view(t, key), changes: r.changesOf(t, key)[queued[key]:]}
	}
	return got
}

// put is a write of issue through the publishing store, which a step expects to succeed.
func (r *changeRig) put(t *testing.T, issue record.Issue) func(context.Context, pgx.Tx) {
	return func(ctx context.Context, tx pgx.Tx) {
		t.Helper()
		if err := r.records.PutIssue(ctx, tx, issue); err != nil {
			t.Fatalf("put %s: %v", issue.Key, err)
		}
	}
}

// view is what the state route serves of key, field by field: the route's projection over the
// store (projection.Project, as the daemon's state source runs it, in a repeatable-read
// transaction), the issue's view encoded as the route encodes it, and each field read off that
// encoding — the slot as its index — null where the view carries none, and every field null for an
// issue the view does not hold.
func (r *changeRig) view(t *testing.T, key string) routeFields {
	t.Helper()
	ctx := context.Background()
	fields := routeFields{record.StateFieldPhase: "null", record.StateFieldTree: "null", record.StateFieldSlot: "null", record.StateFieldWaitingOn: "null"}
	var state api.State
	if err := pgx.BeginTxFunc(ctx, r.st.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(ctx, projection.Inputs{Tx: tx, Store: record.NewStore(), Project: changeProject, Now: time.Now().UTC()})
		return err
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	issue, served := state.Issues[key]
	if !served {
		return fields
	}
	encoded, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("encode the view of %s: %v", key, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode the view of %s: %v", key, err)
	}
	for field, name := range map[record.StateField]string{record.StateFieldPhase: "phase", record.StateFieldTree: "tree", record.StateFieldWaitingOn: "waitingOn"} {
		if value, ok := wire[name]; ok {
			fields[field] = canonical(t, value)
		}
	}
	if slot, ok := wire["slot"]; ok {
		var view struct {
			Index json.RawMessage `json:"index"`
		}
		if err := json.Unmarshal(slot, &view); err != nil {
			t.Fatalf("decode the slot of %s: %v", key, err)
		}
		fields[record.StateFieldSlot] = canonical(t, view.Index)
	}
	return fields
}

// changesOf is every state change of key the outbox holds, in the order it was queued, each read
// back through the outbox's own decoder from a row whose issue column is the bare project key.
func (r *changeRig) changesOf(t *testing.T, key string) []record.StateChange {
	t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select kind, issue, payload from outbox
		where kind = $1 and payload->>'issue' = $2 order by id`, string(record.OutboxKindStateChange), key)
	if err != nil {
		t.Fatalf("read the state changes of %s: %v", key, err)
	}
	defer rows.Close()
	var changes []record.StateChange
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("read a state change of %s: %v", key, err)
		}
		if row.Issue != changeProject {
			t.Fatalf("a state change of %s is queued under %q, want the bare project key %s: the project's state lane", key, row.Issue, changeProject)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode a state change of %s: %v", key, err)
		}
		changes = append(changes, payload.(record.StateChange))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the state changes of %s: %v", key, err)
	}
	return changes
}

// canonical is raw as one canonical JSON text, so two encodings of one value — the route's, and
// the one Postgres stores a payload's jsonb in — compare equal.
func canonical(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %s: %v", raw, err)
	}
	return string(out)
}

// assertMoved holds one transaction's state changes of key to the route's view of key. A
// transaction that moves no field of want queues none, and the route's view of every field is
// what it was. Otherwise it queues exactly one change, of the project, from the run, naming the
// fields of want in want's order, each from the value the route served before the transaction to
// the value it serves after it; every field want does not name the route still serves as it did.
// It answers the change's values, field by field: where each named field went from, and to.
func assertMoved(t *testing.T, key string, got moved, want ...record.StateField) (from, to routeFields) {
	t.Helper()
	for _, field := range []record.StateField{record.StateFieldPhase, record.StateFieldTree, record.StateFieldSlot, record.StateFieldWaitingOn} {
		if slices.Contains(want, field) == (got.before[field] == got.after[field]) {
			t.Fatalf("the route serves %s's %s as %s before and %s after the write, want it moved exactly when the test names it (%v): the test's premise",
				key, field, got.before[field], got.after[field], want)
		}
	}
	if len(want) == 0 {
		if len(got.changes) != 0 {
			t.Fatalf("a write that moved nothing Dispatch observes of %s queued %+v, want no state change", key, got.changes)
		}
		return nil, nil
	}
	if len(got.changes) != 1 {
		t.Fatalf("the write of %s queued %d state changes (%+v), want one naming %v", key, len(got.changes), got.changes, want)
	}
	change := got.changes[0]
	if change.Project != changeProject || change.Run != changeRun || change.Issue != key {
		t.Fatalf("the state change of %s names project %q, run %q and issue %q, want %s, %s and %s", key, change.Project, change.Run, change.Issue, changeProject, changeRun, key)
	}
	named := make([]record.StateField, 0, len(change.Changes))
	from, to = routeFields{}, routeFields{}
	for _, field := range change.Changes {
		named = append(named, field.Field)
		if field.Role != "" {
			t.Fatalf("the %s change of %s names the role %q, want none: only a claim change names one", field.Field, key, field.Role)
		}
		from[field.Field], to[field.Field] = canonical(t, field.Old), canonical(t, field.New)
	}
	if !slices.Equal(named, want) {
		t.Fatalf("the state change of %s names %v, want %v, in that order", key, named, want)
	}
	for _, field := range want {
		if from[field] != got.before[field] || to[field] != got.after[field] {
			t.Fatalf("the %s change of %s goes from %s to %s, want the route's own values, %s before the write and %s after it",
				field, key, from[field], to[field], got.before[field], got.after[field])
		}
	}
	return from, to
}

// slotMoved holds one transaction's state changes of key to one change naming the slot alone
// (assertMoved), from the index from to the index to, each as its JSON: null for no slot.
func slotMoved(t *testing.T, key string, got moved, from, to string) {
	t.Helper()
	gotOld, gotNew := assertMoved(t, key, got, record.StateFieldSlot)
	if gotOld[record.StateFieldSlot] != from || gotNew[record.StateFieldSlot] != to {
		t.Fatalf("the slot change of %s goes from %s to %s, want %s to %s", key, gotOld[record.StateFieldSlot], gotNew[record.StateFieldSlot], from, to)
	}
}

// changeIssue is a recorded root of the project in phase p.
func changeIssue(key string, p phase.Phase) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: changeProject, Title: key, Phase: p, Generation: 1, Status: "in_progress",
		Rank: "A", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// A write of an issue queues, in its own transaction, one state change naming each field Dispatch
// observes that the write moved (record.ObservationOf): its phase, its tree and what it waits on
// from a person, in that order, each from the value the state route served before the write to the
// value it serves after it, as the route encodes them — a time at the microsecond Postgres stores
// it at, not the nanosecond the write was handed. A first write names each field it gives a value
// from null, and no field it leaves null. A write that moves none of them queues nothing, whatever
// else it writes — a status, a rank, the detail of an answer, the sequence a wait began at — and so
// does a write the store refused, a write that rolled back, and every write of a store whose state
// events are off.
func TestAnIssueWriteQueuesOneStateEventNamingEachFieldItMoved(t *testing.T) {
	r := newChangeRig(t)
	since := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

	t.Run("a first insert names old null for each field it gives a value", func(t *testing.T) {
		root := changeIssue("LEGION-1", phase.Admitted)
		from, _ := assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)), record.StateFieldPhase, record.StateFieldTree)
		if from[record.StateFieldPhase] != "null" || from[record.StateFieldTree] != "null" {
			t.Fatalf("the first write of %s names its old values %v, want null for each", root.Key, from)
		}
		// An issue that names no tree is served none, so its first write names its phase alone.
		treeless := changeIssue("LEGION-2", phase.Planning)
		treeless.Tree = ""
		assertMoved(t, treeless.Key, r.commit(t, treeless.Key, r.put(t, treeless)), record.StateFieldPhase)
		// A root recorded in awaiting_person names what it waits on from null as well.
		waiting := changeIssue("LEGION-3", phase.AwaitingPerson)
		waiting.Awaiting = &record.Awaiting{Ask: "ask-3", Since: record.EventMark{Seq: 4, At: since}}
		from, _ = assertMoved(t, waiting.Key, r.commit(t, waiting.Key, r.put(t, waiting)), record.StateFieldPhase, record.StateFieldTree, record.StateFieldWaitingOn)
		if from[record.StateFieldWaitingOn] != "null" {
			t.Fatalf("the first write of %s names its old waitingOn %s, want null", waiting.Key, from[record.StateFieldWaitingOn])
		}
	})

	t.Run("phase", func(t *testing.T) {
		issue := changeIssue("LEGION-10", phase.Admitted)
		r.commit(t, issue.Key, r.put(t, issue))
		issue.Phase = phase.Planning
		_, to := assertMoved(t, issue.Key, r.commit(t, issue.Key, r.put(t, issue)), record.StateFieldPhase)
		if to[record.StateFieldPhase] != `"planning"` {
			t.Fatalf("the phase change of %s goes to %s, want \"planning\"", issue.Key, to[record.StateFieldPhase])
		}
	})

	t.Run("tree set and cleared", func(t *testing.T) {
		issue := changeIssue("LEGION-20", phase.Implementing)
		issue.Tree = ""
		r.commit(t, issue.Key, r.put(t, issue))
		issue.Tree = "LEGION-19"
		_, to := assertMoved(t, issue.Key, r.commit(t, issue.Key, r.put(t, issue)), record.StateFieldTree)
		if to[record.StateFieldTree] != `"LEGION-19"` {
			t.Fatalf("the tree set on %s goes to %s, want \"LEGION-19\"", issue.Key, to[record.StateFieldTree])
		}
		issue.Tree = ""
		_, to = assertMoved(t, issue.Key, r.commit(t, issue.Key, r.put(t, issue)), record.StateFieldTree)
		if to[record.StateFieldTree] != "null" {
			t.Fatalf("the tree cleared on %s goes to %s, want null", issue.Key, to[record.StateFieldTree])
		}
	})

	t.Run("entering and leaving awaiting_person", func(t *testing.T) {
		root := changeIssue("LEGION-30", phase.Planning)
		r.commit(t, root.Key, r.put(t, root))
		root.Phase, root.Awaiting = phase.AwaitingPerson, &record.Awaiting{GateVersion: 2, Artifact: "artifact-30", Since: record.EventMark{Seq: 9, At: since}}
		_, to := assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)), record.StateFieldPhase, record.StateFieldWaitingOn)
		if want := fmt.Sprintf(`{"artifactId":"artifact-30","gateVersion":2,"kind":"gate","since":"%s"}`, since.Format(time.RFC3339Nano)); to[record.StateFieldWaitingOn] != want {
			t.Fatalf("the wait %s entered is %s, want %s", root.Key, to[record.StateFieldWaitingOn], want)
		}
		root.Phase, root.Awaiting = phase.Planning, nil
		_, to = assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)), record.StateFieldPhase, record.StateFieldWaitingOn)
		if to[record.StateFieldWaitingOn] != "null" {
			t.Fatalf("the wait %s left goes to %s, want null", root.Key, to[record.StateFieldWaitingOn])
		}
	})

	t.Run("an answer that moves answeredAt", func(t *testing.T) {
		root := changeIssue("LEGION-40", phase.AwaitingPerson)
		root.Awaiting = &record.Awaiting{Ask: "ask-40", Since: record.EventMark{Seq: 3, At: since}}
		r.commit(t, root.Key, r.put(t, root))
		// The answer is handed in at a nanosecond, which Postgres stores at the microsecond: the
		// event carries the stored time, the one the route serves, not the value handed in.
		answered := time.Date(2026, 11, 18, 10, 0, 0, 123456789, time.UTC)
		root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &answered, record.AnsweredByAsk, "ask-40"
		_, to := assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)), record.StateFieldWaitingOn)
		var view record.WaitingOnView
		if err := json.Unmarshal([]byte(to[record.StateFieldWaitingOn]), &view); err != nil {
			t.Fatalf("decode the answered wait of %s: %v", root.Key, err)
		}
		if at := view.AnsweredAt; at == nil || at.Nanosecond()%int(time.Microsecond) != 0 || at.Sub(answered).Abs() >= time.Microsecond ||
			view.AnsweredBy != record.AnsweredByAsk {
			t.Fatalf("the answered wait of %s = %+v, want it answered by %s within a microsecond of %s and at a whole microsecond, the time Postgres stores",
				root.Key, view, record.AnsweredByAsk, answered.Format(time.RFC3339Nano))
		}
	})

	t.Run("a write that moves nothing observed writes none", func(t *testing.T) {
		root := changeIssue("LEGION-50", phase.AwaitingPerson)
		answered := time.Date(2026, 11, 18, 11, 0, 0, 500100, time.UTC)
		root.Awaiting = &record.Awaiting{Ask: "ask-50", AnsweredAt: &answered, AnsweredBy: record.AnsweredByReply,
			AnsweredDetail: "ask-51", Since: record.EventMark{Seq: 6, At: since}}
		r.commit(t, root.Key, r.put(t, root))
		// The same record written again.
		assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)))
		// Columns Dispatch observes nothing of.
		root.Status, root.Rank, root.Title, root.LastDispatchSeq = "todo", "B", "retitled", 2
		assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)))
		// awaiting_answered_detail alone, which the waitingOn view does not carry.
		root.Awaiting.AnsweredDetail = "ask-52"
		assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)))
		// awaiting_since_seq alone, which the view does not carry either: it carries the time.
		root.Awaiting.Since.Seq = 7
		assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)))
		// An answer handed in at another nanosecond of the microsecond Postgres stores.
		again := answered.Add(100)
		root.Awaiting.AnsweredAt = &again
		assertMoved(t, root.Key, r.commit(t, root.Key, r.put(t, root)))
	})

	t.Run("a refused write writes none", func(t *testing.T) {
		issue := changeIssue("LEGION-60", phase.Admitted)
		r.commit(t, issue.Key, r.put(t, issue))
		refused := issue
		refused.Phase, refused.GitHubRepo = phase.Planning, "acme/gadgets"
		assertMoved(t, issue.Key, r.commit(t, issue.Key, func(ctx context.Context, tx pgx.Tx) {
			err := r.records.PutIssue(ctx, tx, refused)
			if err == nil || !strings.Contains(err.Error(), "GitHub's spelling acme/gadgets names another repository than acme/widgets") {
				t.Fatalf("the write of %s naming GitHub's spelling of another repository = %v, want it refused by PutIssue's checks: the test's premise", issue.Key, err)
			}
		}))
	})

	t.Run("a rolled-back write writes none", func(t *testing.T) {
		issue := changeIssue("LEGION-70", phase.Admitted)
		r.commit(t, issue.Key, r.put(t, issue))
		issue.Phase = phase.Planning
		assertMoved(t, issue.Key, r.rollBack(t, issue.Key, r.put(t, issue)))
		// A first insert rolled back leaves nothing either.
		fresh := changeIssue("LEGION-71", phase.Admitted)
		assertMoved(t, fresh.Key, r.rollBack(t, fresh.Key, r.put(t, fresh)))
	})

	t.Run("a store with the state events off writes none", func(t *testing.T) {
		off := record.NewStore()
		issue := changeIssue("LEGION-80", phase.Admitted)
		put := func(issue record.Issue) func(context.Context, pgx.Tx) {
			return func(ctx context.Context, tx pgx.Tx) {
				t.Helper()
				if err := off.PutIssue(ctx, tx, issue); err != nil {
					t.Fatalf("put %s: %v", issue.Key, err)
				}
			}
		}
		if got := r.commit(t, issue.Key, put(issue)); len(got.changes) != 0 || got.after[record.StateFieldPhase] != `"admitted"` {
			t.Fatalf("a first write through a store with its state events off queued %+v and the route serves %v, want none queued and the issue recorded", got.changes, got.after)
		}
		issue.Phase = phase.Planning
		if got := r.commit(t, issue.Key, put(issue)); len(got.changes) != 0 || got.after[record.StateFieldPhase] != `"planning"` {
			t.Fatalf("a phase change through a store with its state events off queued %+v and the route serves %v, want none queued and the phase moved", got.changes, got.after)
		}
	})
}

// A write of a slot queues, in its own transaction, one state change naming the slot when it moved
// the index the state route serves at the slot's issue: from null to the index for a grant, from
// one index to the other for a move, and from the index to null for a release, whether ordinary
// (ReleaseSlot) or for what the issue waits on (ReleaseSlotForWait). The same index granted again
// queues nothing, and so do a release of an issue that holds no slot and a write that rolled
// back. The issues are recorded through a store with its state events off, so every state change
// these steps read is a slot's.
func TestASlotWriteQueuesOneStateEventNamingTheSlotItMoved(t *testing.T) {
	r := newChangeRig(t)
	admitted := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recordIssue := func(t *testing.T, key string) {
		t.Helper()
		if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutIssue(ctx, tx, changeIssue(key, phase.Implementing))
		}); err != nil {
			t.Fatalf("record %s: %v", key, err)
		}
	}
	grantAt := func(t *testing.T, key string, index int, at time.Time) func(context.Context, pgx.Tx) {
		return func(ctx context.Context, tx pgx.Tx) {
			t.Helper()
			if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: at}); err != nil {
				t.Fatalf("grant %s slot %d: %v", key, index, err)
			}
		}
	}
	grant := func(t *testing.T, key string, index int) func(context.Context, pgx.Tx) {
		return grantAt(t, key, index, admitted)
	}
	release := func(t *testing.T, key string) func(context.Context, pgx.Tx) {
		return func(ctx context.Context, tx pgx.Tx) {
			t.Helper()
			if err := r.records.ReleaseSlot(ctx, tx, key); err != nil {
				t.Fatalf("release the slot of %s: %v", key, err)
			}
		}
	}
	releaseForWait := func(t *testing.T, key string) func(context.Context, pgx.Tx) {
		return func(ctx context.Context, tx pgx.Tx) {
			t.Helper()
			if err := r.records.ReleaseSlotForWait(ctx, tx, key, admitted.Add(time.Hour)); err != nil {
				t.Fatalf("release the slot of %s for its wait: %v", key, err)
			}
		}
	}

	t.Run("grant", func(t *testing.T) {
		const key = "LEGION-101"
		recordIssue(t, key)
		slotMoved(t, key, r.commit(t, key, grant(t, key, 10)), "null", "10")
	})

	t.Run("move", func(t *testing.T) {
		const key = "LEGION-102"
		recordIssue(t, key)
		r.commit(t, key, grant(t, key, 20))
		slotMoved(t, key, r.commit(t, key, grant(t, key, 21)), "20", "21")
	})

	t.Run("unchanged", func(t *testing.T) {
		const key = "LEGION-103"
		recordIssue(t, key)
		r.commit(t, key, grant(t, key, 30))
		// The same index granted again, at another moment: the index is all Dispatch observes.
		assertMoved(t, key, r.commit(t, key, grantAt(t, key, 30, admitted.Add(time.Minute))))
	})

	t.Run("release", func(t *testing.T) {
		const key = "LEGION-104"
		recordIssue(t, key)
		r.commit(t, key, grant(t, key, 40))
		slotMoved(t, key, r.commit(t, key, release(t, key)), "40", "null")
	})

	t.Run("release for a wait", func(t *testing.T) {
		const key = "LEGION-105"
		recordIssue(t, key)
		r.commit(t, key, grant(t, key, 50))
		slotMoved(t, key, r.commit(t, key, releaseForWait(t, key)), "50", "null")
		// The mark is written with the release, in the same statement.
		if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			issue, err := record.NewStore().Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			if issue.ReleasedForWaitAt == nil || !issue.ReleasedForWaitAt.Equal(admitted.Add(time.Hour)) {
				t.Fatalf("the release of %s for its wait marks it at %v, want %s", key, issue.ReleasedForWaitAt, admitted.Add(time.Hour))
			}
			return nil
		}); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
	})

	t.Run("release of no slot", func(t *testing.T) {
		const key = "LEGION-106"
		recordIssue(t, key)
		assertMoved(t, key, r.commit(t, key, release(t, key)))
		assertMoved(t, key, r.commit(t, key, releaseForWait(t, key)))
	})

	t.Run("rollback", func(t *testing.T) {
		const key = "LEGION-107"
		recordIssue(t, key)
		assertMoved(t, key, r.rollBack(t, key, grant(t, key, 70)))
		r.commit(t, key, grant(t, key, 70))
		assertMoved(t, key, r.rollBack(t, key, grant(t, key, 71)))
		assertMoved(t, key, r.rollBack(t, key, release(t, key)))
		assertMoved(t, key, r.rollBack(t, key, releaseForWait(t, key)))
	})
}

// A release of a tree's slots (ReleaseTreeSlots, the re-admission of the tree's own root) queues,
// in its own transaction, one state change for each slot it frees, as a release of one slot does:
// each naming the slot of its issue, from the index the state route served before the release to
// null, in the order of the slots' indexes, which is the order the release answers its issues in.
// An issue of the tree that holds no slot, and an issue of another tree, get none. A release of a
// tree holding no slot queues nothing, and so do a release that rolled back and a release through
// a store whose state events are off. The issues are recorded and granted their slots through a
// store with its state events off, so every state change these steps read is a release's.
func TestATreeReleaseQueuesOneStateEventForEachSlotItFrees(t *testing.T) {
	r := newChangeRig(t)
	admitted := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	// seed records root and each of members in root's tree, and grants each issue of slots its
	// index.
	seed := func(t *testing.T, root string, members []string, slots map[string]int) {
		t.Helper()
		if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			records := record.NewStore()
			for _, key := range append([]string{root}, members...) {
				issue := changeIssue(key, phase.Implementing)
				issue.Tree = root
				if err := records.PutIssue(ctx, tx, issue); err != nil {
					return err
				}
			}
			for key, index := range slots {
				if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: admitted}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("record the tree of %s: %v", root, err)
		}
	}
	// release frees the slots of tree through records and holds the issues it answers to want.
	release := func(t *testing.T, records *record.Postgres, tree string, want ...string) func(context.Context, pgx.Tx) {
		return func(ctx context.Context, tx pgx.Tx) {
			t.Helper()
			released, err := records.ReleaseTreeSlots(ctx, tx, tree)
			if err != nil {
				t.Fatalf("release the slots of %s's tree: %v", tree, err)
			}
			if !slices.Equal(released, want) {
				t.Fatalf("the release of %s's tree answers %v, want %v, in slot order", tree, released, want)
			}
		}
	}
	// queued is the issue of every state change the outbox holds, in the order it was queued.
	queued := func(t *testing.T) []string {
		t.Helper()
		rows, err := r.st.Pool().Query(context.Background(), `select payload->>'issue' from outbox where kind = $1 order by id`,
			string(record.OutboxKindStateChange))
		if err != nil {
			t.Fatalf("read the state changes: %v", err)
		}
		issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the state changes: %v", err)
		}
		return issues
	}

	t.Run("a tree holding two slots", func(t *testing.T) {
		const root, member, idle, other = "LEGION-201", "LEGION-202", "LEGION-203", "LEGION-204"
		seed(t, root, []string{member, idle}, map[string]int{root: 2, member: 1})
		seed(t, other, nil, map[string]int{other: 0})
		before := len(queued(t))
		got := r.runOver(t, []string{root, member, idle, other}, release(t, r.records, root, member, root), true)
		for key, index := range map[string]string{member: "1", root: "2"} {
			slotMoved(t, key, got[key], index, "null")
		}
		assertMoved(t, idle, got[idle])
		assertMoved(t, other, got[other])
		if order := queued(t)[before:]; !slices.Equal(order, []string{member, root}) {
			t.Fatalf("the release queued the state changes of %v, want %s's and then %s's, in slot order", order, member, root)
		}
	})

	t.Run("a tree holding no slot", func(t *testing.T) {
		const root = "LEGION-211"
		seed(t, root, nil, nil)
		assertMoved(t, root, r.commit(t, root, release(t, r.records, root)))
	})

	t.Run("a rolled-back release", func(t *testing.T) {
		const root, member = "LEGION-221", "LEGION-222"
		seed(t, root, []string{member}, map[string]int{root: 3, member: 4})
		got := r.runOver(t, []string{root, member}, release(t, r.records, root, root, member), false)
		assertMoved(t, root, got[root])
		assertMoved(t, member, got[member])
	})

	t.Run("a store with the state events off", func(t *testing.T) {
		const root = "LEGION-231"
		seed(t, root, nil, map[string]int{root: 5})
		if got := r.commit(t, root, release(t, record.NewStore(), root, root)); len(got.changes) != 0 || got.after[record.StateFieldSlot] != "null" {
			t.Fatalf("a release through a store with its state events off queued %+v and the route serves %v, want none queued and the slot freed", got.changes, got.after)
		}
	})
}
