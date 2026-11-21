package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// R12 of the spec: no read takes live to mean present, and R5: a proposed node is
// drawn wherever the node is drawn, and each read of a node answers its node state. The Decisions
// read holds a decision, an ask and a reading at every state but gone, each answering its state.
//
// The agent's choices.md holds a decision, d1, with a reading, g1, under it; alice's asks.md holds
// an ask bob answers. With her cookie alice writes each of the three proposed, settles each, and
// writes each overtaken, and after each step GET /api/v1/issues/{key}/decisions still lists the
// decision with its reading and the ask, and each of the three answers the state that step wrote.
//
// A decision a version above the deferral bound drops is gone whatever state it held: the agent's
// drops.md holds d1 and d2, indexed by its own write; alice writes d2 proposed; the agent's next
// version, past the bound, holds d1 alone, and the typed-node pass in that write states d2 gone,
// so the read no longer lists it, while d1 stays.
func TestTheDecisionsReadListsADecisionAnAskAndAReadingAtEveryStateButGone(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::"+readingA1+":::\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "REST, until a client streams.", nil)
	agentUploadAs(t, handler, issue.Key, "choices.md",
		"::::decision{#d1}\nWe ship REST first.\n\n:::reading{#g1}\nThe agent reads it as REST for every service.\n:::\n::::\n", nil)
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")
	d1, g1 := choices.address("d1"), choices.address("g1")
	decisionNode, askNode := refs.BlockNodeKind+":"+d1, graphstore.KindAsk+":"+askID

	requireListed := func(when, state string) {
		t.Helper()
		entries := readEntries(t, handler, issue.Key)
		decision := requireEntry(t, entries, when+": the decision", decisionNode)
		ask := requireEntry(t, entries, when+": the ask", askNode)
		readings := readingsOf(decision)
		if len(readings) != 1 || !strings.HasPrefix(readings[0], g1+"<-") {
			t.Errorf("%s: the decision answers the readings %v, want g1, %s", when, readings, g1)
		}
		if decision.State != state || ask.State != state {
			t.Errorf("%s: the decision answers the state %q and the ask %q, want %q", when, decision.State, ask.State, state)
		}
		if len(decision.Readings) == 1 && decision.Readings[0].State != state {
			t.Errorf("%s: the reading g1 answers the state %q, want %q", when, decision.Readings[0].State, state)
		}
	}
	requireListed("all three live", graphstore.StateLive)

	// Each node by the store address the state route takes: a block's own node id, and the ask's
	// kind and id.
	addresses := []string{d1, g1, graphstore.KindAsk + "/" + askID}
	for _, address := range addresses {
		if response := writeStateAs(t, handler, issue.Key, address, graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
			t.Fatalf("write proposed on %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	requireListed("all three proposed", graphstore.StateProposed)
	for _, address := range addresses {
		response := dispatchRequest(t, handler, http.MethodPost, statePath(issue.Key, address, "settle"), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("settle %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	requireListed("all three settled", graphstore.StateSettled)
	for _, address := range addresses {
		if response := writeStateAs(t, handler, issue.Key, address, graphstore.StateOvertaken, "alice"); response.Code != http.StatusOK {
			t.Fatalf("write overtaken on %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	requireListed("all three overtaken", graphstore.StateOvertaken)

	// A proposed decision a deferred version drops.
	agentUploadAs(t, handler, issue.Key, "drops.md", ":::decision{#d1}\nKeep one.\n:::\n\n:::decision{#d2}\nDrop one.\n:::\n", nil)
	drops := readDecisionDocument(t, database, issue.Key, "drops-md")
	if indexed := blockIndexVersion(t, database, drops.id); indexed == nil || *indexed != 1 {
		t.Fatalf("the first version of drops.md is indexed at %v, want 1, by its own write", indexed)
	}
	kept, dropped := refs.BlockNodeKind+":"+drops.address("d1"), refs.BlockNodeKind+":"+drops.address("d2")
	if response := writeStateAs(t, handler, issue.Key, drops.address("d2"), graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
		t.Fatalf("write proposed on d2: status=%d body=%s", response.Code, response.Body.String())
	}
	requireEntry(t, readEntries(t, handler, issue.Key), "the proposed d2", dropped)
	agentUploadAs(t, handler, issue.Key, "drops.md",
		strings.Repeat("- x\n", itemsOverTheBlockBound)+"\n:::decision{#d1}\nKeep one.\n:::\n", nil)
	if indexed := blockIndexVersion(t, database, drops.id); indexed != nil {
		t.Fatalf("the second version of drops.md is indexed at %d, want it owed: no block index runs past the bound", *indexed)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, drops.address("d2"))
	if err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("the proposed d2 the deferred version dropped is held %t at the state %q (%v), want gone", held, node.State, err)
	}
	entries := readEntries(t, handler, issue.Key)
	requireEntry(t, entries, "the kept d1 after the deferred version", kept)
	if listed := slices.ContainsFunc(entries, func(entry decisionEntry) bool { return entryKey(entry) == dropped }); listed {
		t.Errorf("the Decisions read still lists %s, which the deferred version dropped", dropped)
	}
}
