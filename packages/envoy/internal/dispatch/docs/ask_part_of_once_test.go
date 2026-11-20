package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A settlement that restores a retracted ask and rewords it, in one pass over its block, indexes
// the ask twice: the restoration, then the rewording (reconcileAskBlocks). The restoration here
// places parts the store has not placed - the ask's parts as no block-aware build indexed them,
// whose ids are drawn past the block nodes the store already holds - and the rewording keeps its
// options, so it reads where the store places them while the restoration's part_of is still held
// for its event. Each part's part_of is asserted once (causedByEvent).
func TestASettlementThatRestoresAndRewordsAnAskStatesEachPartOfOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	const asked = ":::ask{#ask-block multiple=\"false\" state=\"open\" urgency=\"high\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
	const reworded = ":::ask{#ask-block multiple=\"false\" state=\"open\" urgency=\"high\"}\nWhich transport should we expose first?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
	seedServiceText(t, service, artifactID, asked)
	service.settleRoom(artifactID, 0)
	var askID string
	if err := service.store.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read the settled ask: %v", err)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
			Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "replacement"},
			Children: []*pmdoc.Node{{Type: "text", Text: "No decision remains."}},
		}}}
	})
	settleCurrentGeneration(t, service, artifactID)
	// The ask's parts as no block-aware build indexed them: no part row, a state row at no
	// revision, and the next id past every part node the store holds of it.
	for _, statement := range []string{
		`delete from ask_block_refs where ask_id = $1::uuid`,
		`delete from ask_block_texts where ask_id = $1::uuid`,
		`delete from ask_blocks where ask_id = $1::uuid`,
		`update ask_block_state set revision = 0, body_sha256 = null, next_block = 10 where ask_id = $1::uuid`,
	} {
		if _, err := service.store.Pool.Exec(ctx, statement, askID); err != nil {
			t.Fatalf("leave ask %s unindexed (%s): %v", askID, statement, err)
		}
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		restored, err := pmdoc.Parse(reworded)
		if err != nil {
			t.Fatalf("parse the reworded ask: %v", err)
		}
		return restored
	})
	settleCurrentGeneration(t, service, artifactID)
	var state, question string
	if err := service.store.Pool.QueryRow(ctx, `select state, question from asks where id = $1::uuid`, askID).Scan(&state, &question); err != nil {
		t.Fatalf("read the restored ask: %v", err)
	}
	if state != "open" || question != "Which transport should we expose first?" {
		t.Fatalf("the ask reads %s %q, want it restored open and reworded", state, question)
	}
	rows, err := service.store.Pool.Query(ctx, `
		select from_id, count(*) from graph_tuples
		where to_kind = 'ask' and to_id = $1 and kind = 'part_of' and from_kind = 'block' and asserted
		group by from_id order by from_id
	`, askID)
	if err != nil {
		t.Fatalf("count the part_of of ask %s's parts: %v", askID, err)
	}
	defer rows.Close()
	placedAnew := 0
	for rows.Next() {
		var part string
		var asserted int
		if err := rows.Scan(&part, &asserted); err != nil {
			t.Fatalf("count the part_of of ask %s's parts: %v", askID, err)
		}
		if part > "ask/"+askID+"#b1" && len(part) == len("ask/"+askID+"#b10") {
			placedAnew++
		}
		if asserted != 1 {
			t.Errorf("part %s states its part_of %d times, want once", part, asserted)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the part_of of ask %s's parts: %v", askID, err)
	}
	if placedAnew == 0 {
		t.Fatalf("no part past b9 of ask %s is placed, so the restoration placed nothing and this test would prove nothing", askID)
	}
}
