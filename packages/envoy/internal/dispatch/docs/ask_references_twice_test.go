package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One transaction that reconciles an ask's references twice - two rewordings of one approval
// request in one version write, the second planned before the first is written - ends where two
// reconciliations run one after the other end. The ask, written as this build writes one, cites
// CORE-2; the first rewording cites CORE-3 and the second CORE-4. Its one refs row is CORE-4's,
// holding tuples_stored, and its mentions tuples stand for that row alone: CORE-4's asserted,
// CORE-2's retracted by the first rewording, and CORE-3's, which the first asserted, retracted by
// the second. A second plan that read only which targets the first leaves would insert CORE-4's
// row with no tuples_stored and no citation, and leave CORE-3's citation standing with no row.
func TestAnAskReconciledTwiceInOneTransactionHoldsTheSecondsCitations(t *testing.T) {
	database, _ := blockIndexService(t)
	ctx := context.Background()
	const asked, first, second = "Which day, after dispatch://CORE-2?", "Which day, after dispatch://CORE-3?", "Which day, after dispatch://CORE-4?"
	ask := writeAskAsThisBuild(t, database, asked)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rewordings: %v", err)
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	for _, rewording := range []struct{ previous, question string }{{asked, first}, {first, second}} {
		reader := refs.NewReader(queued, tx, "ask", ask, graphEdgeServer)
		_, step, cited, err := planEditedAskReferences(queued, tx, reader, ask, rewording.previous, rewording.question)
		if err != nil {
			t.Fatalf("plan the references of %q: %v", rewording.question, err)
		}
		if err := write(queued, tx, step); err != nil {
			t.Fatalf("queue the references of %q: %v", rewording.question, err)
		}
		if err := write(queued, tx, func(b *store.WriteBatch) error {
			return graphstore.QueueCaused(b, cited, graphstore.NoEvent)
		}); err != nil {
			t.Fatalf("queue the citations of %q: %v", rewording.question, err)
		}
	}
	if err := ledger.runQueue(queued, nil); err != nil {
		t.Fatalf("write the rewordings: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the rewordings: %v", err)
	}

	rows, err := database.Pool.Query(ctx, `
		select to_id, tuples_stored from refs where from_kind = 'ask' and from_id = $1 order by to_id
	`, ask)
	if err != nil {
		t.Fatalf("read the ask's refs rows: %v", err)
	}
	held := map[string]bool{}
	for rows.Next() {
		var target string
		var stored bool
		if err := rows.Scan(&target, &stored); err != nil {
			t.Fatalf("read the ask's refs rows: %v", err)
		}
		held[target] = stored
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask's refs rows: %v", err)
	}
	if stored, ok := held["CORE-4"]; len(held) != 1 || !ok || !stored {
		t.Errorf("the ask holds the refs rows %v (target: tuples_stored), want CORE-4's alone, holding tuples_stored", held)
	}

	rows, err = database.Pool.Query(ctx, `
		select distinct on (to_id) to_id, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'issue'
		order by to_id, seq desc
	`, graphstore.KindAsk, ask, graphstore.TupleMentions)
	if err != nil {
		t.Fatalf("read the ask's citations: %v", err)
	}
	stands := map[string]bool{}
	for rows.Next() {
		var target string
		var asserted bool
		if err := rows.Scan(&target, &asserted); err != nil {
			t.Fatalf("read the ask's citations: %v", err)
		}
		stands[target] = asserted
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask's citations: %v", err)
	}
	want := map[string]bool{"CORE-2": false, "CORE-3": false, "CORE-4": true}
	for target, asserted := range want {
		if got, stated := stands[target]; !stated || got != asserted {
			t.Errorf("the ask's newest citation of %s is stated %t, asserted %t; want asserted %t", target, stated, got, asserted)
		}
	}
	if len(stands) != len(want) {
		t.Errorf("the ask's citations are %v, want %v", stands, want)
	}
}
