package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheStateRouteRefusesAnAskBlockAndWritesTheAsk holds one question to one state
// (askHoldsState). A spec holds one `:::ask` block, q1, and once settlement indexes its ask the
// question is two nodes the store holds: the block, `<ref_key>#q1` at dispatch://<KEY>/spec#b-q1,
// which is only the question's place in the spec, and its ask, ask/<uuid> at
// dispatch://<KEY>/ask/<uuid>, whose node holds the question's state. alice's cookie writes
// proposed, overtaken and live on the block by its dispatch address and proposed by its store
// address, and settles it by its dispatch address: each is 409 ASK_HOLDS_STATE naming the ask, and
// neither node moves its state or gains a state tuple. Her proposed by the ask's own address is
// then written as on any other node: the ask's row holds proposed, its newest state tuple is
// proposed written by her, and the block's row holds the state it held before.
func TestTheStateRouteRefusesAnAskBlockAndWritesTheAsk(t *testing.T) {
	t.Parallel()
	handler, database := blockAskHandler(t)
	ctx := context.Background()
	const question = "Ship the cache this week?"
	issue := createInteractionIssue(t, handler, "TEST", "One question holds one state",
		"Context\n\n:::ask{#q1 urgency=\"med\" multiple=\"false\"}\n"+question+"\n\n- Ship: Now.\n- Hold: Later.\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "q1", question)
	askID := blockAskID(t, handler, issue.Key, "q1")
	var blockNode string
	if err := database.Pool.QueryRow(ctx, `select ref_key || '#q1' from artifacts where id = $1::uuid`,
		issue.PrimaryArtifactID).Scan(&blockNode); err != nil {
		t.Fatalf("read the store id of the block q1: %v", err)
	}
	block := "dispatch://" + issue.Key + "/spec#b-q1"
	ask := "dispatch://" + issue.Key + "/ask/" + askID

	blockBefore := storedStateOf(t, database, block)
	askBefore := storedStateOf(t, database, ask)
	if askBefore.state == graphstore.StateProposed {
		t.Fatalf("the ask %s is proposed before any write, so a write of proposed on it would move nothing", ask)
	}

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive} {
		requireRefused(t, state+" on the ask block by its dispatch address", writeStateAs(t, handler, issue.Key, block, state, "alice"),
			http.StatusConflict, codeAskHoldsState, block, ask)
	}
	requireRefused(t, "proposed on the ask block by its store address", writeStateAs(t, handler, issue.Key, blockNode, graphstore.StateProposed, "alice"),
		http.StatusConflict, codeAskHoldsState, ask)
	requireRefused(t, "the settle of the ask block",
		dispatchRequest(t, handler, http.MethodPost, statePath(issue.Key, block, "settle"), nil, "alice"),
		http.StatusConflict, codeAskHoldsState, block, ask)
	if after := storedStateOf(t, database, block); after != blockBefore {
		t.Fatalf("the refused writes moved the block %s from %+v to %+v", block, blockBefore, after)
	}
	if after := storedStateOf(t, database, ask); after != askBefore {
		t.Fatalf("the refused writes on the block moved the ask %s from %+v to %+v", ask, askBefore, after)
	}

	response := writeStateAs(t, handler, issue.Key, ask, graphstore.StateProposed, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("write proposed on the ask %s: status=%d body=%s", ask, response.Code, response.Body.String())
	}
	if written := decodeBody[nodeStateWritten](t, response); written.Node.State != graphstore.StateProposed ||
		written.Node.Kind != graphstore.KindAsk || written.Previous != askBefore.state {
		t.Fatalf("the write of proposed on the ask answers %+v, want the ask at proposed from %s", written, askBefore.state)
	}
	if after := storedNodeOf(t, database, ask); after.State != graphstore.StateProposed {
		t.Fatalf("the ask's row holds %s after the write of proposed, want proposed", after.State)
	}
	if newest := newestStateTuple(t, database, graphstore.KindAsk, askID); newest.ToID != graphstore.StateProposed ||
		newest.Who.Kind != "user" || newest.Who.ID != "alice" {
		t.Fatalf("the ask's newest state tuple is %s written by %+v, want proposed written by alice", newest.ToID, newest.Who)
	}
	if after := storedNodeOf(t, database, block); after.State != blockBefore.state {
		t.Fatalf("the write on the ask moved the block %s from %s to %s", block, blockBefore.state, after.State)
	}
}

// TestTheStateRouteRefusesACopiedAskBlockNamingItsSourceAsk uploads the spec's text as a second
// document of the same issue. Once the copy's block has a current copied_from to the source ask
// and no ask of its own, its proposed write is 409 ASK_HOLDS_STATE naming that source. The
// refusal leaves both nodes' states and state tuple counts as they were.
func TestTheStateRouteRefusesACopiedAskBlockNamingItsSourceAsk(t *testing.T) {
	t.Parallel()
	copied := uploadAskBlockCopy(t, "Copy of a question")
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool {
		ties := currentTies(t, copied.database, copied.blockNode, graphstore.TupleCopiedFrom)
		return len(ties) == 1 && ties[0].ToKind == graphstore.KindAsk && ties[0].ToID == copied.askID
	}) {
		t.Fatalf("the copy's block %s never held a current copied_from to ask %s", copied.blockNode, copied.askID)
	}
	if owned := countRows(t, copied.database, `select count(*) from asks where block_artifact_id = $1::uuid`, copied.document.ID); owned != 0 {
		t.Fatalf("the copy opened %d asks of its own, want none", owned)
	}
	copied.requireProposedRefused(t, "proposed on the copied ask block")
}

// TestTheStateRouteRefusesAnUnclaimedCopyNamingTheAskItsOpeningLineNames holds the state route to
// the question map's third case (refs.QuestionMap): a copy no settlement of this build has claimed,
// whose block has no ask of its own and whose node holds no copied_from tuple, and whose latest
// version's opening line names the ask it copies as copied_from. The spec's text is uploaded as a
// second document of the same issue; once settlement has written the copy's provenance into its
// latest version and owes the copy nothing more, the test takes the copied_from tuples off the
// copy's block node, so the store holds the copy as the release before left one, its source named
// in its opening line alone. The question map then gives the block the source ask, and the copy's
// proposed write is 409 ASK_HOLDS_STATE naming that same ask. The refusal leaves both nodes'
// states and state tuple counts as they were.
func TestTheStateRouteRefusesAnUnclaimedCopyNamingTheAskItsOpeningLineNames(t *testing.T) {
	t.Parallel()
	copied := uploadAskBlockCopy(t, "An unclaimed copy of a question")
	// A settlement is what states the claim: once the copy owes none, no later pass runs over it
	// that could state the claim again after the tuples below are taken off.
	awaitSettledVersionHolding(t, copied.handler, copied.database, copied.document.ID, `copied_from="`+copied.askID+`"`)
	if removed, err := copied.database.Pool.Exec(context.Background(), `delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3`,
		refs.BlockNodeKind, copied.blockNode, graphstore.TupleCopiedFrom); err != nil || removed.RowsAffected() == 0 {
		t.Fatalf("take the copied_from tuples off the copy's block %s: %d rows, %v", copied.blockNode, removed.RowsAffected(), err)
	}
	if ties := currentTies(t, copied.database, copied.blockNode, graphstore.TupleCopiedFrom); len(ties) != 0 {
		t.Fatalf("the copy's block %s still holds the copied_from %+v, want none", copied.blockNode, ties)
	}
	if owned := countRows(t, copied.database, `select count(*) from asks where block_artifact_id = $1::uuid`, copied.document.ID); owned != 0 {
		t.Fatalf("the copy opened %d asks of its own, want none", owned)
	}
	read := dispatchRequest(t, copied.handler, http.MethodGet, "/api/v1/references/questions?kind=artifact&id="+copied.document.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the copy's question map: status=%d body=%s", read.Code, read.Body.String())
	}
	if questions := decodeBody[referenceQuestions](t, read).Questions; len(questions) != 1 || questions[0].BlockID != "q1" ||
		questions[0].Ask != copied.askID || questions[0].Reference != copied.ask {
		t.Fatalf("the copy's question map is %+v, want q1 naming the ask its opening line names, %s at %s", questions, copied.askID, copied.ask)
	}
	copied.requireProposedRefused(t, "proposed on the unclaimed copy's ask block")
}

// askBlockCopy is an issue whose spec holds the `:::ask` block q1, which settlement has indexed
// as an ask, and a copy of the spec's text uploaded as a second document of the same issue
// (uploadAskBlockCopy): the handler and the store that hold them, the issue's key, the ask's id
// and its dispatch address, the copy, and the store address of the copy's block node q1.
type askBlockCopy struct {
	handler           http.Handler
	database          *store.Store
	issue, askID, ask string
	document          model.Artifact
	blockNode         string
}

// uploadAskBlockCopy seeds an issue titled title whose spec holds the `:::ask` block q1, waits
// for settlement to index its ask (seedBlockAsk), and uploads the spec's text as spec-record.md,
// a second document of the same issue, as sessionActor.
func uploadAskBlockCopy(t *testing.T, title string) askBlockCopy {
	t.Helper()
	handler, database := blockAskHandler(t)
	const question = "Ship the cache this week?"
	issue, askID := seedBlockAsk(t, handler, title, "q1",
		":::ask{#q1 urgency=\"med\" multiple=\"false\"}\n"+question+"\n\n- Ship: Now.\n- Hold: Later.\n:::\n", question)
	record, _ := documentTexts(t, handler, issue.PrimaryArtifactID)
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": record,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[artifactUploadResponse](t, uploaded).Artifact
	return askBlockCopy{
		handler: handler, database: database,
		issue: issue.Key, askID: askID, ask: "dispatch://" + issue.Key + "/ask/" + askID,
		document: copied, blockNode: refs.BlockAddress(copied.RefKey, "q1"),
	}
}

// requireProposedRefused writes proposed on the copy's block q1, by the block's dispatch address
// and as alice's cookie, and requires the write's 409 ASK_HOLDS_STATE naming the block and the
// spec's ask (requireRefused, logged under name), and that the refusal left both nodes' states and
// state tuple counts as they were.
func (c askBlockCopy) requireProposedRefused(t *testing.T, name string) {
	t.Helper()
	block := refs.BlockRef("dispatch://"+c.issue+"/artifact/"+c.document.Slug, "q1")
	blockBefore := storedStateOf(t, c.database, block)
	askBefore := storedStateOf(t, c.database, c.ask)

	requireRefused(t, name, writeStateAs(t, c.handler, c.issue, block, graphstore.StateProposed, "alice"),
		http.StatusConflict, codeAskHoldsState, block, c.ask)
	if after := storedStateOf(t, c.database, block); after != blockBefore {
		t.Fatalf("the refused write moved the copied block %s from %+v to %+v", block, blockBefore, after)
	}
	if after := storedStateOf(t, c.database, c.ask); after != askBefore {
		t.Fatalf("the refused write moved the source ask %s from %+v to %+v", c.ask, askBefore, after)
	}
}
