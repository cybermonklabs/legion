package admit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// blockRun is the run id the state events of these tests carry.
const blockRun = "7d3e9a41-0c6b-4f28-b5e1-93a2c4d8f067"

// publishingAdmission is an admission of this cap (newAdmissionForProject) over a record store with
// the state events on (record.Postgres.PublishState), which is what makes the pass write the
// admission block, and which logs nowhere.
func publishingAdmission(t *testing.T, cap int) *Admission {
	t.Helper()
	admission := newAdmissionForProject(t, testProject, cap, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	records := record.NewStore()
	records.PublishState(blockRun, nil)
	admission.store = records
	return admission
}

// admissionEvents is every state_admission row the outbox holds, in the order they were queued,
// each decoded as the outbox decodes it, so a row this package could not send fails here.
func admissionEvents(t *testing.T, pool *pgxpool.Pool) []record.StateAdmission {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = $1 order by id`, string(record.OutboxKindStateAdmission))
	if err != nil {
		t.Fatalf("list the admission events: %v", err)
	}
	defer rows.Close()
	var events []record.StateAdmission
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("read an admission event: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode admission event %d: %v", row.ID, err)
		}
		event := payload.(record.StateAdmission)
		if row.Issue != testProject || event.Project != testProject || event.Run != blockRun {
			t.Fatalf("admission event %d is queued under %q for project %q and run %q, want %q, %q and %q",
				row.ID, row.Issue, event.Project, event.Run, testProject, testProject, blockRun)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the admission events: %v", err)
	}
	return events
}

// servedBlock is the admission block GET /legion/v1/state serves of the store now, under the
// admission's cap and gate steps, decoded: the state route's own projection (projection.Project,
// in the repeatable-read snapshot the route reads in), its block encoded as the route encodes it.
func servedBlock(t *testing.T, pool *pgxpool.Pool, admission *Admission) any {
	t.Helper()
	ctx := context.Background()
	var encoded []byte
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		state, err := projection.Project(ctx, projection.Inputs{
			Tx: tx, Store: record.NewStore(), Project: testProject, Cap: admission.cap,
			Steps: admission.engine.GatedSteps(), Now: fixedNow,
		})
		if err != nil {
			return err
		}
		encoded, err = json.Marshal(state.Admission)
		return err
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	return decodedBlock(t, encoded)
}

// decodedBlock is an encoded block as plain JSON values, which compare equal whatever order the
// encoding wrote the keys in: a block stored as jsonb comes back in jsonb's own key order.
func decodedBlock(t *testing.T, encoded []byte) any {
	t.Helper()
	var block any
	if err := json.Unmarshal(encoded, &block); err != nil {
		t.Fatalf("decode the block %s: %v", encoded, err)
	}
	return block
}

// An admission pass ends with the project's admission block, judged afresh after every write of
// its fact, and writes the block's state event where the block moved and nowhere else. The block
// is the one the state route serves of the same store, field for field, so a reader of the events
// holds what a reader of the route holds. A first pass writes the first block; then a slot granted,
// a Dispatch rank moved and a blocker link added each move the block and write one event, and a
// pass that moves nothing — a tick that promotes nothing, the entry sweep, a boot reconcile — writes
// none.
func TestAnAdmissionPassQueuesTheBlockOnlyWhenItChanges(t *testing.T) {
	// changed applies change and asserts it wrote one admission event, the block the state route
	// serves of the store after it, different from the block before it.
	changed := func(t *testing.T, pool *pgxpool.Pool, admission *Admission, change func()) {
		t.Helper()
		before := admissionEvents(t, pool)
		if len(before) == 0 {
			t.Fatal("no admission event stands before the change; the case writes no first block")
		}
		change()
		after := admissionEvents(t, pool)
		if len(after) != len(before)+1 {
			t.Fatalf("the change wrote %d admission events, want one", len(after)-len(before))
		}
		got, previous := decodedBlock(t, after[len(after)-1].Admission), decodedBlock(t, before[len(before)-1].Admission)
		if reflect.DeepEqual(got, previous) {
			t.Fatalf("the event carries the block %s it carried before the change", after[len(after)-1].Admission)
		}
		if want := servedBlock(t, pool, admission); !reflect.DeepEqual(got, want) {
			t.Fatalf("the event carries the block %s, want the block the state serves, %v", after[len(after)-1].Admission, want)
		}
	}
	// The issues of the cases. A state event names an issue by its key (record.StateChange), so
	// every key is one Dispatch could have minted.
	const run, first, second, blocker = "LEGION-100", "LEGION-101", "LEGION-102", "LEGION-199"
	// running seeds a root at work holding the cap's one slot, so no pass promotes anything.
	running := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		seedSlotted(t, pool, run, "0")
		seedMirror(t, pool, run, "in_progress")
	}

	t.Run("a slot granted", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		seedWaiting(t, pool, first, "A")
		seedMirror(t, pool, first, "todo")
		applySweep(t, pool, admission, "sweep")

		changed(t, pool, admission, func() { tick(t, pool, admission, "tick") })
		assertSlots(t, pool, []record.Slot{{Issue: first, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a Dispatch rank moved", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		running(t, pool)
		seedWaiting(t, pool, first, "A")
		seedWaiting(t, pool, second, "B")
		seedMirror(t, pool, first, "todo")
		seedMirror(t, pool, second, "todo")
		applySweep(t, pool, admission, "sweep")
		assertWaiting(t, pool, []string{first, second})

		changed(t, pool, admission, func() {
			applyMirrored(t, pool, admission, "rank", intake.DispatchIssue{Key: second, ResolvedRepo: "acme/widgets",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: second, Rank: "0", HandedOver: handed}, engineStub{})
		})
		assertWaiting(t, pool, []string{second, first})
	})

	t.Run("a blocker link added", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		running(t, pool)
		seedWaiting(t, pool, first, "A")
		seedMirror(t, pool, first, "todo")
		seedMirror(t, pool, blocker, "in_progress")
		applySweep(t, pool, admission, "sweep")

		changed(t, pool, admission, func() {
			applyMirrored(t, pool, admission, "link", intake.DispatchIssue{Key: first, ResolvedRepo: "acme/widgets",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: first, Rank: "A", HandedOver: handed,
				Blockers: []string{blocker}}, engineStub{})
		})
		events := admissionEvents(t, pool)
		var block record.AdmissionBlock
		if err := json.Unmarshal(events[len(events)-1].Admission, &block); err != nil {
			t.Fatalf("decode the block: %v", err)
		}
		if want := []record.BlockedIssue{{Issue: first, BlockedBy: []string{blocker}}}; !reflect.DeepEqual(block.Blocked, want) {
			t.Fatalf("the block's blocked = %#v, want %#v", block.Blocked, want)
		}
	})

	t.Run("a pass that moves nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		running(t, pool)
		seedWaiting(t, pool, first, "A")
		seedMirror(t, pool, first, "todo")
		// The boot's reconcile is a pass too, and here the first: it writes the first block.
		reconcile(t, pool, admission, nil)
		events := admissionEvents(t, pool)
		if len(events) != 1 {
			t.Fatalf("the first pass wrote %d admission events, want one: the first block", len(events))
		}
		if got, want := decodedBlock(t, events[0].Admission), servedBlock(t, pool, admission); !reflect.DeepEqual(got, want) {
			t.Fatalf("the first event carries the block %s, want the block the state serves, %v", events[0].Admission, want)
		}

		tick(t, pool, admission, "tick")
		applySweep(t, pool, admission, "sweep")
		reconcile(t, pool, admission, nil)
		if events := admissionEvents(t, pool); len(events) != 1 {
			t.Fatalf("three passes that moved nothing left %d admission events, want the first one alone", len(events))
		}
	})
}

// A pass's block is judged once, after the pass's last write, and that one judgement is both the
// block the pass publishes and the block the state route serves of the same store after the pass.
// A pass that promotes writes a slot after the judgement it promoted from, and a pass that releases
// a slot writes before its line's last judgement: in both the published block is the served one,
// field for field, so it never lags the pass's own writes. Each pass here is a tick, which reads
// the same final judgement for the controller's lists.
func TestTheBlockAPassPublishesIsTheBlockTheStateServesAfterIt(t *testing.T) {
	const run, first, blocker = "LEGION-100", "LEGION-101", "LEGION-199"
	// published applies the tick, and holds its one new event's block to the served block and
	// its active list to active.
	published := func(t *testing.T, pool *pgxpool.Pool, admission *Admission, active []string) {
		t.Helper()
		before := len(admissionEvents(t, pool))
		tick(t, pool, admission, "tick")
		events := admissionEvents(t, pool)
		if len(events) != before+1 {
			t.Fatalf("the pass wrote %d admission events, want one", len(events)-before)
		}
		got := events[len(events)-1].Admission
		if want := servedBlock(t, pool, admission); !reflect.DeepEqual(decodedBlock(t, got), want) {
			t.Fatalf("the pass published the block %s, want the block the state serves after it, %v", got, want)
		}
		var block record.AdmissionBlock
		if err := json.Unmarshal(got, &block); err != nil {
			t.Fatalf("decode the block: %v", err)
		}
		if !reflect.DeepEqual(block.Active, active) {
			t.Fatalf("the published block's active = %v, want %v", block.Active, active)
		}
	}

	t.Run("a pass that promotes", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		seedWaiting(t, pool, first, "A")
		seedMirror(t, pool, first, "todo")
		applySweep(t, pool, admission, "sweep")
		published(t, pool, admission, []string{first})
		assertSlots(t, pool, []record.Slot{{Issue: first, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a pass that releases a slot", func(t *testing.T) {
		pool := migratedPool(t)
		admission := publishingAdmission(t, 1)
		// A root at work in implementing, whose Dispatch blocker is not done: the pass gives its
		// slot back for the wait (releaseSlotsForWaits).
		seedSlottedInPhase(t, pool, testProject, run, "0", phase.Implementing, nil)
		seedMirror(t, pool, run, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")
		applySweep(t, pool, admission, "sweep")
		if events := admissionEvents(t, pool); len(events) != 1 {
			t.Fatalf("the sweep wrote %d admission events, want the first block", len(events))
		}
		published(t, pool, admission, []string{})
		assertSlots(t, pool, nil)
	})
}
