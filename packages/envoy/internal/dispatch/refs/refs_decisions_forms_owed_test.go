package refs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// decisionsFormsFixture is a transaction holding project CORE and its issues CORE-1 and CORE-2,
// whose comments cite an issue's Decisions page.
func decisionsFormsFixture(t *testing.T) pgx.Tx {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	insertSecondIssue(t, tx)
	return tx
}

// execAll runs each statement with args, as a build's index of a text runs them at the database.
func execAll(t *testing.T, tx pgx.Tx, args []any, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := tx.Exec(context.Background(), statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// drainDecisionsForms lists what the live decisions forms queue owes, requires that to be the one
// source, and re-indexes it as the drain does (ReindexOwed), with this build's grammar.
func drainDecisionsForms(t *testing.T, tx pgx.Tx, kind, id string) {
	t.Helper()
	drainOwedFrom(t, tx, RefsDecisionsFormsOwed, kind, id)
}

// drainOwedFrom is drainDecisionsForms for any queue.
func drainOwedFrom(t *testing.T, tx pgx.Tx, queue OwedQueue, kind, id string) {
	t.Helper()
	ctx := context.Background()
	listed, err := ListOwed(ctx, tx, queue, 10)
	if err != nil || len(listed) != 1 || listed[0].Kind != kind || listed[0].ID != id || listed[0].Queue != queue {
		t.Fatalf("list what %s owes = %#v (%v), want the %s alone, of that queue", queue.Table(), listed, err, kind)
	}
	if indexed, err := ReindexOwed(ctx, tx, listed[0], refsGrammarServer); err != nil || !indexed {
		t.Fatalf("re-index the %s = %v (%v), want it indexed", kind, indexed, err)
	}
}

// The release before this one reads `dispatch://CORE-2/decisions` as no reference. Its re-index of
// a comment that holds one deletes the issue's edge from the comment and from its block, which owes
// the forms queue the comment, and then settles that row with the statement of its own index. Only
// the relay of that delete says the comment was indexed without the edge: it owes it to the
// decisions forms queue, which this build's drain re-indexes the comment from, writing the edge
// back and settling both queues. A write of this build, which indexes the whole text and clears the
// relayed row in a statement after the delete that relays it, leaves the decisions queue empty
// (TestAWriteLeavesNothingOwedInTheFormsQueue).
func TestAnOlderBuildsSettleOfTheFormsQueueIsRelayedAndTheDecisionsDrainRestoresTheEdge(t *testing.T) {
	tx := decisionsFormsFixture(t)
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", "The decisions: dispatch://CORE-2/decisions.")
	if edges := edgesFrom(t, tx, "comment", comment); !slices.Equal(edges, []string{"issue CORE-2"}) {
		t.Fatalf("this build's index of the comment wrote the edges %v, want the issue the Decisions page names", edges)
	}
	blocks := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment)
	if len(blocks) != 1 {
		t.Fatalf("this build's index of the comment wrote the block edges %v, want one", blocks)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after this build's write, want it empty", owed)
	}
	requireNoDecisionsFormsOwed(t, tx, "after this build's write")

	execAll(t, tx, []any{comment},
		`delete from refs where from_kind = 'comment' and from_id = $1`,
		`delete from comment_block_refs where comment_id = $1::uuid`)
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"comment " + comment}) {
		t.Fatalf("the release before's deletes owe the forms queue %v, want the comment", owed)
	}
	execAll(t, tx, []any{comment}, `
		with grammar as (delete from refs_grammar_owed where kind = 'comment' and id = $1::uuid)
		delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`)

	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the release before settled its row, want it empty", owed)
	}
	if owed := decisionsFormsOwed(t, tx); !slices.Equal(owed, []string{"comment " + comment}) {
		t.Fatalf("the decisions forms queue holds %v after that settle, want the comment owed", owed)
	}
	if edges := edgesFrom(t, tx, "comment", comment); len(edges) != 0 {
		t.Fatalf("the comment holds the edges %v after the release before indexed it, want none", edges)
	}

	drainDecisionsForms(t, tx, "comment", comment)

	if edges := edgesFrom(t, tx, "comment", comment); !slices.Equal(edges, []string{"issue CORE-2"}) {
		t.Fatalf("the drain wrote the edges %v, want the issue the Decisions page names", edges)
	}
	if after := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment); !slices.Equal(after, blocks) {
		t.Fatalf("the drain wrote the block edges %v, want %v", after, blocks)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the drain, want it empty", owed)
	}
	requireNoDecisionsFormsOwed(t, tx, "after the drain")
}

// A comment can name the issue in the same block as the Decisions page's link, as a bare key, and
// the release before then deletes nothing: the issue's edge is the bare key's too, and it only adds
// the retired decisions document's edge, to the comment and to its block. No delete owes the forms
// queue, and it settles a row it does not hold, which relays nothing, so the decisions queue is owed
// by the block table's own insert alone. Its drain deletes the edge this build does not write, and
// settles.
func TestAnOlderBuildsInsertOfTheRetiredDocumentsEdgeIsOwedWithNothingDeleted(t *testing.T) {
	tx := decisionsFormsFixture(t)
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", "See CORE-2 and https://dispatch.example/issues/CORE-2/decisions.")
	edges := edgesFrom(t, tx, "comment", comment)
	if !slices.Equal(edges, []string{"issue CORE-2"}) {
		t.Fatalf("this build's index of the comment wrote the edges %v, want the issue alone", edges)
	}
	blocks := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment)
	if len(blocks) != 1 {
		t.Fatalf("this build's index of the comment wrote the block edges %v, want one", blocks)
	}
	requireNoDecisionsFormsOwed(t, tx, "after this build's write")
	blockID, _, _ := strings.Cut(blocks[0], " ")

	execAll(t, tx, []any{comment, blockID, "comment/" + comment + "#" + blockID},
		`insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id)
		 values ($1::uuid, $2, $3, 'artifact', 'CORE-2/decisions-md')`)
	execAll(t, tx, []any{comment},
		`insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'artifact', 'CORE-2/decisions-md')`,
		`delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`)

	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after an index that deleted nothing, want it empty", owed)
	}
	if owed := decisionsFormsOwed(t, tx); !slices.Equal(owed, []string{"comment " + comment}) {
		t.Fatalf("the decisions forms queue holds %v after the insert of the retired document's edge, want the comment owed", owed)
	}
	if held := edgesFrom(t, tx, "comment", comment); !slices.Equal(held, []string{"artifact CORE-2/decisions-md", "issue CORE-2"}) {
		t.Fatalf("the comment holds the edges %v after the release before indexed it, want the issue's and the retired document's", held)
	}

	drainDecisionsForms(t, tx, "comment", comment)

	if after := edgesFrom(t, tx, "comment", comment); !slices.Equal(after, edges) {
		t.Fatalf("the drain left the edges %v, want %v: the retired document's edge is not one this build writes for the link", after, edges)
	}
	if after := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment); !slices.Equal(after, blocks) {
		t.Fatalf("the drain left the block edges %v, want %v", after, blocks)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the drain, want it empty", owed)
	}
	requireNoDecisionsFormsOwed(t, tx, "after the drain, whose own delete of the edge owed the forms queue and whose settle relayed it")
}

// A text stored before this release, indexed by a build that does not read the Decisions page, is
// in the bootstrap queue, and one an older build settled since is in the live queue too. A complete
// index of the source by this build clears the row of each, whichever queue it was listed from:
// its own write after the settle of the forms queue that relays the live row, and the drain of
// either queue, whose first statement settles the row it listed and whose last clears the other.
func TestACompleteIndexOfASourceOwedInBothDecisionsQueuesClearsBothDebts(t *testing.T) {
	tx := decisionsFormsFixture(t)
	const body = "The decisions: dispatch://CORE-2/decisions."
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", body)
	blocks := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment)
	// The index an older build left: no edge for the link, and the source owed in both queues.
	oweAsOlderBuildsLeaveIt := func() {
		t.Helper()
		execAll(t, tx, []any{comment},
			`delete from refs where from_kind = 'comment' and from_id = $1`,
			`delete from comment_block_refs where comment_id = $1::uuid`,
			`insert into refs_decisions_forms_owed (kind, id) values ('comment', $1::uuid) on conflict do nothing`,
			`insert into refs_decisions_forms_seed_owed (kind, id) values ('comment', $1::uuid) on conflict do nothing`)
		if edges := edgesFrom(t, tx, "comment", comment); len(edges) != 0 {
			t.Fatalf("the comment holds %v after an index that does not read the page, want none", edges)
		}
	}
	restored := func(by string) {
		t.Helper()
		if edges := edgesFrom(t, tx, "comment", comment); !slices.Equal(edges, []string{"issue CORE-2"}) {
			t.Fatalf("%s wrote the edges %v, want the issue the Decisions page names", by, edges)
		}
		if after := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment); !slices.Equal(after, blocks) {
			t.Fatalf("%s wrote the block edges %v, want %v", by, after, blocks)
		}
		if owed := formsOwed(t, tx); len(owed) != 0 {
			t.Fatalf("the forms queue holds %v after %s, want it empty", owed, by)
		}
		requireNoDecisionsFormsOwed(t, tx, "after "+by)
	}

	oweAsOlderBuildsLeaveIt()
	indexItemAsAWriteDoes(t, tx, ItemComment, comment, body, refsGrammarServer, time.Now())
	restored("a write of this build")

	oweAsOlderBuildsLeaveIt()
	drainOwedFrom(t, tx, RefsDecisionsFormsSeedOwed, "comment", comment)
	restored("the drain of the bootstrap queue")

	oweAsOlderBuildsLeaveIt()
	drainOwedFrom(t, tx, RefsDecisionsFormsOwed, "comment", comment)
	restored("the drain of the live queue")
}
