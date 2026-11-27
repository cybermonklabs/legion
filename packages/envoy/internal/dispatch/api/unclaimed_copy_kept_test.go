package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A copy of an answered ask's `:::ask` block that no settlement has claimed - its source named on
// its opening line alone, as the release before left such a copy - is the `:::ask` block node of
// an answered ask from its first read (R32, as the architect ruled at
// dispatch://INFRA-1253/comment/2aa81020-a1e3-4037-b43b-d4b3104ab195): an agent's PATCH of the
// copy and its DELETE are each 409 ANSWERED_ASK_KEPT quoting the source's question, and store
// nothing. docs.PersonDecisionOfBlock finds the copy's ask as refs.QuestionMap does: the block's
// own ask row, else its copied_from tuple, else the copied_from of its opening line. Read through
// the row and the tuple alone, the copy names no ask, and both writes were taken.
func TestAnAgentMovesAndDeletesNoUnclaimedCopyOfAnAnsweredAsk(t *testing.T) {
	t.Parallel()
	copied := uploadAskBlockCopy(t, "An unclaimed copy of an answered question")
	answerDecisionBlock(t, copied.handler, copied.askID, "alice", []string{"Ship"}, "", nil)
	// A settlement is what states the claim, and every settlement of the room states it again, one
	// that writes no version too. The answer arms one for the copy once its own drain has run
	// (docs' Ledger.Commit, SettleCopiesOf), held in memory where no row shows it, and it may fire
	// after the copy's version below names its source. So every armed settlement is stopped and
	// those running are waited for (docs.Service.Quiesce) before the tuples below are taken off,
	// and no later pass states the claim again.
	awaitSettledVersionHolding(t, copied.handler, copied.database, copied.document.ID, `copied_from="`+copied.askID+`"`)
	if err := copied.documents.Quiesce(context.Background()); err != nil {
		t.Fatalf("stop the settlements armed for the issue's room: %v", err)
	}
	// The answer's live write of the spec schedules the room's settlement, whose version of the
	// spec owes its record digest at its COMMIT and whose batch marks the issue's queries owed: its
	// own fill and drain then write after that COMMIT, the drain a version of the decisions query
	// (R7). The copy's version above may stand before they have run, so the counts below are read
	// once no settlement, no fill and no drain is owed, and the refused writes are all that moves.
	awaitNoSettlementOwed(t, copied.database)
	if removed, err := copied.database.Pool.Exec(context.Background(), `delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3`,
		refs.BlockNodeKind, copied.blockNode, graphstore.TupleCopiedFrom); err != nil || removed.RowsAffected() == 0 {
		t.Fatalf("take the copied_from tuples off the copy's block %s: %d rows, %v", copied.blockNode, removed.RowsAffected(), err)
	}
	if owned := countRows(t, copied.database, `select count(*) from asks where block_artifact_id = $1::uuid`, copied.document.ID); owned != 0 {
		t.Fatalf("the copy holds %d asks of its own, want none", owned)
	}
	const question = "Ship the cache this week?"

	held, tag := placementOf(t, copied.database, refs.BlockNodeKind, copied.blockNode)
	before := readDeleteCounts(t, copied.database)
	// The shared bearer names the session it writes for, as every agent's write does.
	moved := refusedWith(t, "the agent's move of the unclaimed copy "+copied.blockNode,
		patchNode(t, copied.handler, copied.issue, copied.blockNode, tag, map[string]any{"parent": held.ToKind + "/" + held.ToID, "actor": sessionActor()}, ""),
		http.StatusConflict, codeAnsweredAskKept)
	deleted := refusedWith(t, "the agent's delete of the unclaimed copy "+copied.blockNode,
		serveNodeRequest(t, copied.handler, http.MethodDelete, deleteNodePath(copied.issue, copied.blockNode, false, 1),
			map[string]any{"actor": sessionActor()}, http.Header{"Authorization": {"Bearer agent-token"}, "If-Match": {tag}}),
		http.StatusConflict, codeAnsweredAskKept)
	for what, message := range map[string]string{"move": moved, "delete": deleted} {
		if !strings.Contains(message, question) {
			t.Errorf("the refusal of the agent's %s of the copy quotes no %q: %s", what, question, message)
		}
	}
	requireUnchanged(t, copied.database, "the agent's refused move and delete of the unclaimed copy", before)
}
