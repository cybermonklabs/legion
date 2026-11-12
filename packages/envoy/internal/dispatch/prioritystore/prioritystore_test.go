package prioritystore

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The two writers of these tests: a person and a session, the two shapes an actor is recorded in.
var (
	alice = model.Actor{Kind: "user", ID: "alice@theorem.dev"}
	bob   = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
)

// A tuple as the log holds it, read straight from the table rather than through this package, for
// the check that no write ever rewrote or removed one.
type loggedTuple struct {
	seq                                  int64
	fromKind, fromID, kind, toKind, toID string
	at                                   time.Time
	who                                  string
	sourceEvent                          int64
	asserted                             bool
}

// A priority is written with its four attributes and read back from the node row; a rename moves
// that row and appends one tuple; a retraction appends one more; and through all of it the log
// only grows — every row written earlier is still there, with every column as it was. The
// attribute's history is the two values under one name, each with its own writer and moment.
func TestAPriorityIsWrittenRenamedAndRetractedWithNoRowEverRewritten(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t).Pool

	const id = "a-priority"
	// Postgres keeps a timestamptz to the microsecond, so the times written are the times read
	// back and an equality is an equality.
	start := time.Now().UTC().Truncate(time.Microsecond)
	written := map[string]string{
		AttributeTitle:       "Agents stop losing work",
		AttributeDescription: "Every project's work, read in one place.",
		AttributeOwner:       "sami@theorem.dev",
		AttributeRank:        "U",
	}
	writeOnce(t, pool, func(tx pgx.Tx) error { return Create(ctx, tx, id, written, alice, start) })

	node, held, err := Read(ctx, pool, id)
	if err != nil {
		t.Fatalf("read the priority: %v", err)
	}
	if !held {
		t.Fatal("the store holds no priority of that id after writing one")
	}
	if node.Kind != KindPriority || node.Supertag != KindPriority {
		t.Errorf("the node is kind %q supertag %q, want %q for both", node.Kind, node.Supertag, KindPriority)
	}
	if node.Visibility != VisibilityProject || node.State != StateLive {
		t.Errorf("the node is %q/%q, want %q/%q", node.Visibility, node.State, VisibilityProject, StateLive)
	}
	if !node.MadeAt.Equal(start) || node.MadeBy != alice || node.SourceEvent != NoEvent {
		t.Errorf("the node was made at %s by %+v for event %d, want %s by %+v for %d",
			node.MadeAt, node.MadeBy, node.SourceEvent, start, alice, NoEvent)
	}
	for name, want := range written {
		if held, carried := node.Attribute(name); !carried || held != want {
			t.Errorf("the node's %s is %q (carried %t), want %q", name, held, carried, want)
		}
	}
	if value, carried, err := CurrentValue(ctx, pool, KindPriority, id, AttributeTitle); err != nil {
		t.Fatalf("read the title off the log: %v", err)
	} else if !carried || value != written[AttributeTitle] {
		t.Errorf("the log's newest title is %q (carried %t), want %q", value, carried, written[AttributeTitle])
	}

	// A second create of one id is refused, and writes neither a row nor a tuple.
	beforeRefusal := wholeLog(t, pool)
	if err := tryWrite(pool, func(tx pgx.Tx) error {
		return Create(ctx, tx, id, map[string]string{AttributeTitle: "a second one"}, bob, start)
	}); !errors.Is(err, ErrPriorityExists) {
		t.Errorf("a second create of one id answered %v, want %v", err, ErrPriorityExists)
	}
	if again := wholeLog(t, pool); !reflect.DeepEqual(again, beforeRefusal) {
		t.Errorf("the refused create moved the log: %d rows, want the %d it found", len(again), len(beforeRefusal))
	}

	// The rename: one attribute, written by somebody else a minute later.
	const renamed = "Agents never lose work"
	renamedAt := start.Add(time.Minute)
	beforeRename := wholeLog(t, pool)
	writeOnce(t, pool, func(tx pgx.Tx) error {
		return SetAttributes(ctx, tx, id, map[string]string{AttributeTitle: renamed}, bob, renamedAt)
	})
	afterRename := wholeLog(t, pool)
	assertOnlyAppended(t, beforeRename, afterRename, 1)

	node, _, err = Read(ctx, pool, id)
	if err != nil {
		t.Fatalf("read the renamed priority: %v", err)
	}
	if title, _ := node.Attribute(AttributeTitle); title != renamed {
		t.Errorf("the node row's title is %q, want the renamed %q", title, renamed)
	}
	if description, _ := node.Attribute(AttributeDescription); description != written[AttributeDescription] {
		t.Errorf("the rename moved the description to %q, want the %q it was written with",
			description, written[AttributeDescription])
	}
	if !node.MadeAt.Equal(start) || node.MadeBy != alice {
		t.Errorf("the rename moved who made the node to %+v at %s, want %+v at %s",
			node.MadeBy, node.MadeAt, alice, start)
	}

	// The attribute's history: the two values under one name, oldest first, each with the writer
	// and the moment that stated it.
	history, err := History(ctx, pool, KindPriority, id, AttributeTitle)
	if err != nil {
		t.Fatalf("read the title's history: %v", err)
	}
	wantHistory := []struct {
		value string
		who   model.Actor
		at    time.Time
	}{
		{written[AttributeTitle], alice, start},
		{renamed, bob, renamedAt},
	}
	if len(history) != len(wantHistory) {
		t.Fatalf("the title's history holds %d tuples, want %d: %+v", len(history), len(wantHistory), history)
	}
	for index, want := range wantHistory {
		stated := history[index]
		if stated.ToKind != KindValue || stated.ToID != want.value {
			t.Errorf("tuple %d states %s/%q, want %s/%q", index, stated.ToKind, stated.ToID, KindValue, want.value)
		}
		if stated.Who != want.who || !stated.At.Equal(want.at) || !stated.Asserted {
			t.Errorf("tuple %d was stated by %+v at %s (asserted %t), want %+v at %s asserted",
				index, stated.Who, stated.At, stated.Asserted, want.who, want.at)
		}
	}

	// The relation, and the retraction that ends it.
	const issueKey = "ACME-7"
	advancedAt := start.Add(2 * time.Minute)
	writeOnce(t, pool, func(tx pgx.Tx) error { return Advance(ctx, tx, issueKey, id, alice, advancedAt) })
	if issues, err := IssuesAdvancing(ctx, pool, id); err != nil {
		t.Fatalf("read the issues advancing the priority: %v", err)
	} else if len(issues) != 1 || issues[0].FromID != issueKey || !issues[0].Asserted {
		t.Errorf("the issues advancing the priority are %+v, want the one %s", issues, issueKey)
	}
	if priorities, err := PrioritiesAdvanced(ctx, pool, issueKey); err != nil {
		t.Fatalf("read the priorities the issue advances: %v", err)
	} else if len(priorities) != 1 || priorities[0].ToID != id {
		t.Errorf("the priorities %s advances are %+v, want the one %s", issueKey, priorities, id)
	}

	retractedAt := start.Add(3 * time.Minute)
	beforeRetraction := wholeLog(t, pool)
	writeOnce(t, pool, func(tx pgx.Tx) error { return Retract(ctx, tx, issueKey, id, bob, retractedAt) })
	afterRetraction := wholeLog(t, pool)
	assertOnlyAppended(t, beforeRetraction, afterRetraction, 1)

	if issues, err := IssuesAdvancing(ctx, pool, id); err != nil {
		t.Fatalf("read the issues advancing the retracted priority: %v", err)
	} else if len(issues) != 0 {
		t.Errorf("the retracted relation still reads as current: %+v", issues)
	}
	if priorities, err := PrioritiesAdvanced(ctx, pool, issueKey); err != nil {
		t.Fatalf("read the priorities the issue advances after the retraction: %v", err)
	} else if len(priorities) != 0 {
		t.Errorf("the issue still advances %+v after retracting it", priorities)
	}

	// The whole history of the priority, newest first: the retraction, then the assertion, then
	// the rename, and every attribute it was created with below them.
	timeline, err := Timeline(ctx, pool, id)
	if err != nil {
		t.Fatalf("read the priority's history: %v", err)
	}
	if len(timeline) != len(afterRetraction) {
		t.Errorf("the priority's history holds %d tuples and the log holds %d", len(timeline), len(afterRetraction))
	}
	if len(timeline) < 3 {
		t.Fatalf("the priority's history holds %d tuples, want every write of this test", len(timeline))
	}
	if newest := timeline[0]; newest.Kind != RelationAdvances || newest.Asserted || newest.FromID != issueKey {
		t.Errorf("the newest entry is %+v, want %s's retraction of its relation", newest, issueKey)
	}
	if second := timeline[1]; second.Kind != RelationAdvances || !second.Asserted {
		t.Errorf("the second entry is %+v, want the assertion of the relation", second)
	}
	if third := timeline[2]; third.Kind != AttributeTitle || third.ToID != renamed {
		t.Errorf("the third entry is %+v, want the rename to %q", third, renamed)
	}

	// Deleting a priority is a state and not a removal: the row is still there, and the listing
	// leaves it out.
	goneAt := start.Add(4 * time.Minute)
	beforeDelete := wholeLog(t, pool)
	writeOnce(t, pool, func(tx pgx.Tx) error { return SetState(ctx, tx, id, StateGone, bob, goneAt) })
	assertOnlyAppended(t, beforeDelete, wholeLog(t, pool), 1)
	if node, held, err = Read(ctx, pool, id); err != nil || !held {
		t.Fatalf("read the deleted priority: held %t, %v", held, err)
	} else if node.State != StateGone {
		t.Errorf("the deleted priority's state is %q, want %q", node.State, StateGone)
	}
	if live, err := List(ctx, pool); err != nil {
		t.Fatalf("list the live priorities: %v", err)
	} else if len(live) != 0 {
		t.Errorf("the listing still holds the deleted priority: %+v", live)
	}
}

// The listing is in rank order, and a move writes the key between two neighbours, onto the node
// row and into the log. A neighbour the store does not hold, and one that is the priority itself,
// are refusals rather than a key.
func TestThePrioritiesListInRankOrderAndAMoveWritesTheKeyBetweenTwoOfThem(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t).Pool
	at := time.Now().UTC().Truncate(time.Microsecond)

	ids := []string{"first", "second", "third"}
	for _, id := range ids {
		writeOnce(t, pool, func(tx pgx.Tx) error {
			key, err := RankAt(ctx, tx, id, "", "")
			if err != nil {
				return err
			}
			return Create(ctx, tx, id, map[string]string{AttributeTitle: id, AttributeRank: key}, alice, at)
		})
	}
	assertListed(t, pool, ids)

	// The third priority moves between the first two: after the first, before the second.
	beforeMove := wholeLog(t, pool)
	var moved string
	writeOnce(t, pool, func(tx pgx.Tx) error {
		var err error
		moved, err = Move(ctx, tx, "third", "second", "first", bob, at.Add(time.Minute))
		return err
	})
	assertOnlyAppended(t, beforeMove, wholeLog(t, pool), 1)
	assertListed(t, pool, []string{"first", "third", "second"})

	node, _, err := Read(ctx, pool, "third")
	if err != nil {
		t.Fatalf("read the moved priority: %v", err)
	}
	if key, _ := node.Attribute(AttributeRank); key != moved {
		t.Errorf("the moved priority's node row holds the order %q, want the %q the move answered", key, moved)
	}
	if value, carried, err := CurrentValue(ctx, pool, KindPriority, "third", AttributeRank); err != nil {
		t.Fatalf("read the moved priority's order off the log: %v", err)
	} else if !carried || value != moved {
		t.Errorf("the log's newest order is %q (carried %t), want %q", value, carried, moved)
	}

	for name, move := range map[string]func(pgx.Tx) error{
		"a neighbour that is the priority itself": func(tx pgx.Tx) error {
			_, err := Move(ctx, tx, "third", "third", "", bob, at)
			return err
		},
		"a neighbour no row names": func(tx pgx.Tx) error {
			_, err := Move(ctx, tx, "third", "", "fourth", bob, at)
			return err
		},
		"neighbours in the wrong order": func(tx pgx.Tx) error {
			_, err := Move(ctx, tx, "third", "first", "second", bob, at)
			return err
		},
	} {
		if err := tryWrite(pool, move); !errors.Is(err, ErrRankNeighbor) {
			t.Errorf("%s answered %v, want %v", name, err, ErrRankNeighbor)
		}
	}
	if err := tryWrite(pool, func(tx pgx.Tx) error {
		_, err := Move(ctx, tx, "fourth", "", "", bob, at)
		return err
	}); !errors.Is(err, ErrNoPriority) {
		t.Errorf("moving a priority no row names answered %v, want %v", err, ErrNoPriority)
	}
}

// assertListed holds the live listing to one order of ids.
func assertListed(t *testing.T, pool *store.Pool, want []string) {
	t.Helper()
	listed, err := List(context.Background(), pool)
	if err != nil {
		t.Fatalf("list the live priorities: %v", err)
	}
	held := make([]string, len(listed))
	for index, node := range listed {
		held[index] = node.ID
	}
	if !reflect.DeepEqual(held, want) {
		t.Errorf("the listing is %v, want %v", held, want)
	}
}

// assertOnlyAppended holds a write to appending: every row the log held before it is still there
// with every column as it was, and the log grew by exactly appended rows.
func assertOnlyAppended(t *testing.T, before, after []loggedTuple, appended int) {
	t.Helper()
	if len(after) != len(before)+appended {
		t.Errorf("the log holds %d rows, want the %d it held plus %d", len(after), len(before), appended)
	}
	for index, was := range before {
		if index >= len(after) {
			t.Fatalf("the log lost the row at %d: %+v", index, was)
		}
		if is := after[index]; is != was {
			t.Errorf("the row at %d was rewritten: %+v, was %+v", index, is, was)
		}
	}
}

// wholeLog is every tuple row of the database, read without this package's own statements.
func wholeLog(t *testing.T, pool *store.Pool) []loggedTuple {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select seq, from_kind, from_id, kind, to_kind, to_id, at, who::text, source_event, asserted
		from priority_tuples
		order by seq
	`)
	if err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	defer rows.Close()
	var logged []loggedTuple
	for rows.Next() {
		var row loggedTuple
		if err := rows.Scan(&row.seq, &row.fromKind, &row.fromID, &row.kind, &row.toKind, &row.toID,
			&row.at, &row.who, &row.sourceEvent, &row.asserted); err != nil {
			t.Fatalf("read a tuple row: %v", err)
		}
		logged = append(logged, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	return logged
}

// writeOnce runs one write in a transaction of its own and commits it, as a route's handler does.
func writeOnce(t *testing.T, pool *store.Pool, write func(pgx.Tx) error) {
	t.Helper()
	if err := tryWrite(pool, write); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// tryWrite is writeOnce for a write the caller expects to be refused: the transaction rolls back
// and the refusal is answered.
func tryWrite(pool *store.Pool, write func(pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
