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
// question is two nodes the store holds: the block, `<tree ref_key>#q1` under the tree artifact of
// the issue's room the spec is a fragment of (blockKeyOf), at dispatch://<KEY>/spec#b-q1, which is
// only the question's place in the spec, and its ask, ask/<uuid> at dispatch://<KEY>/ask/<uuid>,
// whose node holds the question's state. alice's cookie writes proposed, overtaken and live on the
// block by its dispatch address and proposed by its store address, and settles it by its dispatch
// address: each is 409 ASK_HOLDS_STATE naming the ask, and neither node moves its state or gains a
// state tuple. Her proposed by the ask's own address is then written as on any other node: the
// ask's row holds proposed, its newest state tuple is proposed written by her, and the block's row
// holds the state it held before.
func TestTheStateRouteRefusesAnAskBlockAndWritesTheAsk(t *testing.T) {
	t.Parallel()
	handler, database := blockAskHandler(t)
	const question = "Ship the cache this week?"
	issue := createInteractionIssue(t, handler, "TEST", "One question holds one state",
		"Context\n\n:::ask{#q1 urgency=\"med\" multiple=\"false\"}\n"+question+"\n\n- Ship: Now.\n- Hold: Later.\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "q1", question)
	askID := blockAskID(t, handler, issue.Key, "q1")
	blockNode := blockKeyOf(t, database, issue.PrimaryArtifactID) + "#q1"
	block := "dispatch://" + issue.Key + "/spec#b-q1"
	ask := "dispatch://" + issue.Key + "/ask/" + askID

	blockBefore := readAskBlockNodeState(t, database, blockNode)
	askBefore := readAskBlockNodeState(t, database, ask)
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
	if after := readAskBlockNodeState(t, database, blockNode); after != blockBefore {
		t.Fatalf("the refused writes moved the block %s from %+v to %+v", block, blockBefore, after)
	}
	if after := readAskBlockNodeState(t, database, ask); after != askBefore {
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
	if after := storedNodeOf(t, database, blockNode); after.State != blockBefore.state {
		t.Fatalf("the write on the ask moved the block %s from %s to %s", block, blockBefore.state, after.State)
	}
}

// TestTheStateRouteRefusesACopiedAskBlockNamingItsSourceAsk uploads the spec's text as a second
// document of the same issue. Once the copy's block has a current copied_from to the source ask
// and no ask of its own, its proposed write is 409 ASK_HOLDS_STATE naming that source. The
// refusal leaves both nodes' states and state tuple counts as they were.
func TestTheStateRouteRefusesACopiedAskBlockNamingItsSourceAsk(t *testing.T) {
	t.Parallel()
	handler, database := blockAskHandler(t)
	const question = "Ship the cache this week?"
	issue, askID := seedBlockAsk(t, handler, "Copy of a question", "q1",
		":::ask{#q1 urgency=\"med\" multiple=\"false\"}\n"+question+"\n\n- Ship: Now.\n- Hold: Later.\n:::\n", question)
	record, _ := documentTexts(t, handler, issue.PrimaryArtifactID)
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": record,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	copyID := refs.BlockAddress(copied.RefKey, "q1")
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool {
		ties := currentTies(t, database, copyID, graphstore.TupleCopiedFrom)
		return len(ties) == 1 && ties[0].ToKind == graphstore.KindAsk && ties[0].ToID == askID
	}) {
		t.Fatalf("the copy's block %s never held a current copied_from to ask %s", copyID, askID)
	}
	if owned := countRows(t, database, `select count(*) from asks where block_artifact_id = $1::uuid`, copied.ID); owned != 0 {
		t.Fatalf("the copy opened %d asks of its own, want none", owned)
	}
	block := refs.BlockRef("dispatch://"+issue.Key+"/artifact/"+copied.Slug, "q1")
	ask := "dispatch://" + issue.Key + "/ask/" + askID
	blockBefore := readAskBlockNodeState(t, database, block)
	askBefore := readAskBlockNodeState(t, database, ask)

	requireRefused(t, "proposed on the copied ask block", writeStateAs(t, handler, issue.Key, block, graphstore.StateProposed, "alice"),
		http.StatusConflict, codeAskHoldsState, block, ask)
	if after := readAskBlockNodeState(t, database, block); after != blockBefore {
		t.Fatalf("the refused write moved the copied block %s from %+v to %+v", block, blockBefore, after)
	}
	if after := readAskBlockNodeState(t, database, ask); after != askBefore {
		t.Fatalf("the refused write moved the source ask %s from %+v to %+v", ask, askBefore, after)
	}
}

// askBlockNodeState holds a node's stored state and its count of state tuples.
type askBlockNodeState struct {
	state  string
	tuples int
}

// readAskBlockNodeState requires a row so the states compared before and after are the store's
// own, not a default for a node the store has not recorded.
func readAskBlockNodeState(t *testing.T, database *store.Store, address string) askBlockNodeState {
	t.Helper()
	node := storedNodeOf(t, database, address)
	history, err := graphstore.History(context.Background(), database.Pool, node.Kind, node.ID, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the state tuples of %s: %v", address, err)
	}
	return askBlockNodeState{state: node.State, tuples: len(history)}
}
