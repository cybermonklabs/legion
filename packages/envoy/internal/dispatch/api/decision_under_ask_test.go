package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-decision-under-ask (R2 of the issue-tree spec, with R7's third exception and R15): a decision
// whose from_ask names an ask of its own issue stands under that ask's node, "a decision under the
// ask it answers", on every path that places blocks, while its document keeps it where its text
// stands. The issue is one Legion does not run and an agent writes each decision, so neither R28
// nor R32 masks the rule.
//
// The agent's spec write of a decision whose from_ask names an issue-level ask, which POST
// /api/v1/issues/{key}/asks opened, stands it under that ask's node, in the store's current
// part_of and in the tree read; the version that write made renders from the tree as its stored
// bytes, byte for byte, with the decision where its text put it, and the Decisions read answers the
// decision's entry, from_ask and all. A decision whose from_ask names the ask of an ask block of
// the spec stands under that ask's node, and one with no from_ask under the issue. A decision a
// PATCH moved holds the placement stored, and the text write that gives it a from_ask moves it
// under that ask with the placement text. A text write that takes a from_ask out puts the decision
// back under the issue, where its text puts it, and one that names another ask moves it there. A
// decision of a second document, which plays no role and so is a room of its own, settles the ask
// of the spec's ask block, which the spec's room keys, and stands under that ask's node too. The
// census kind block then counts no difference over the store, and a PATCH of a decision that holds
// a from_ask, with a current If-Match, is 400 NODE_NOT_MOVABLE, naming the decision and its ask,
// and writes nothing; once the caller may not read that ask, the same refusal names the decision
// alone.
//
// Its four controls, each run alone: dropping the from_ask branch of planBlockParents
// (refs/block_nodes.go) prints "decision <id> stands under issue <key>, want ask <uuid>"; dropping
// the census's reading of a decision's parent through its from_ask (settledAsk in
// graphcensus/census_blocks.go) prints "census kind block counts <n> differences, want 0" with the
// address of each decision under an ask; reading the ask's block id there for a decision of
// another document than the one holding the ask's block, which the node side reads as the
// decision's own parent (graphcensus' askBlockID), prints the same with the address of d4; dropping
// the PATCH's refusal of a decision that holds a from_ask prints "PATCH of decision <id> answered
// 200, want 400 NODE_NOT_MOVABLE".
func TestADecisionThatSettlesAnAskStandsUnderThatAsk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "ASKED")
	issue := createDigestIssue(t, handler, database, "ASKED", "Decisions under the asks they settle", decisionUnderAskSpec, false)
	key := issue.Key
	if err := deps.Docs.SettleIssueDocuments(ctx, key); err != nil {
		t.Fatalf("settle the documents of %s: %v", key, err)
	}
	awaitNoSettlementOwed(t, database)
	blockAsk := blockAskID(t, handler, key, "q1")
	first := decisionUnderAskOpen(t, handler, key, "Does every writer take one store?")
	second := decisionUnderAskOpen(t, handler, key, "Does the ledger order the writes?")
	asked := func(ask string) string { return "dispatch://" + key + "/ask/" + ask }

	// The spec write of d1, whose from_ask names the issue-level ask first.
	number := decisionUnderAskEdit(t, handler, database, key, map[string]any{
		"op": "insert", "after": "end",
		"markdown": ":::decision{#d1 from_ask=\"" + asked(first) + "\"}\n" + decisionUnderAskD1 + "\n:::\n",
	})
	d1 := blockAddressOf(t, database, key, "d1")
	requireDecisionUnder(t, handler, database, key, d1, graphstore.KindAsk, first)
	stored := renderAtVersionStored(t, database, issue.PrimaryArtifactID, number)
	if question, words := strings.Index(stored, decisionUnderAskQuestion), strings.Index(stored, decisionUnderAskD1); question < 0 || words < question {
		t.Errorf("version %d of the spec of %s holds the ask block's question at %d and d1's words at %d, want d1 after the ask block, where the write put it:\n%s",
			number, key, question, words, stored)
	}
	requireRenderAtVersionFromTree(t, database, "the spec of "+key, issue.PrimaryArtifactID, number, stored)
	entry := requireEntry(t, readEntries(t, handler, key), "d1", refs.BlockNodeKind+":"+d1)
	if !slices.Equal(entry.FromAsk, []string{first}) || entry.Document == nil || entry.Document.BlockID != "d1" || !strings.Contains(entry.Text, decisionUnderAskD1) {
		t.Errorf("the Decisions read answers d1 as %s, want its entry in the spec, block d1, with its words and the from_ask %s",
			entryJSON(t, entry), first)
	}

	// d2 settles the ask of the spec's ask block, and d3 settles none.
	decisionUnderAskEdit(t, handler, database, key, map[string]any{
		"op": "insert", "after": "end",
		"markdown": ":::decision{#d2 from_ask=\"" + asked(blockAsk) + "\"}\n" + decisionUnderAskD2 + "\n:::\n\n" +
			":::decision{#d3}\n" + decisionUnderAskD3 + "\n:::\n",
	})
	d2, d3 := blockAddressOf(t, database, key, "d2"), blockAddressOf(t, database, key, "d3")
	requireDecisionUnder(t, handler, database, key, d2, graphstore.KindAsk, blockAsk)
	requireDecisionUnder(t, handler, database, key, d3, graphstore.KindIssue, key)

	// A decision with no from_ask moves, and holds the placement stored; the text write that gives
	// it a from_ask stands it under that ask with the placement text.
	_, tag := placementOf(t, database, refs.BlockNodeKind, d3)
	if moved := patchNode(t, handler, key, d3, tag, map[string]any{"parent": d1}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("PATCH of decision %s, which holds no from_ask, under %s answered %d %s, want 200", d3, d1, moved.Code, moved.Body.String())
	}
	awaitNoSettlementOwed(t, database)
	requireDecisionUnder(t, handler, database, key, d3, refs.BlockNodeKind, d1)
	requireValue(t, database, d3, refs.AttributePlacement, refs.PlacementStored)
	decisionUnderAskEdit(t, handler, database, key, map[string]any{
		"op": "retype", "block": "d3", "type": "decision", "attributes": map[string]any{"from_ask": asked(second)},
	})
	requireDecisionUnder(t, handler, database, key, d3, graphstore.KindAsk, second)
	requireValue(t, database, d3, refs.AttributePlacement, refs.PlacementText)

	// A text write that takes d1's from_ask out puts it back under the issue, where its text puts
	// it, and one that names another ask in d2's moves d2 under that ask.
	decisionUnderAskEdit(t, handler, database, key, map[string]any{
		"op": "retype", "block": "d1", "type": "decision", "attributes": map[string]any{},
	})
	requireDecisionUnder(t, handler, database, key, d1, graphstore.KindIssue, key)
	decisionUnderAskEdit(t, handler, database, key, map[string]any{
		"op": "retype", "block": "d2", "type": "decision", "attributes": map[string]any{"from_ask": asked(first)},
	})
	requireDecisionUnder(t, handler, database, key, d2, graphstore.KindAsk, first)

	// d4, in a second document of the issue, settles the ask of the spec's ask block q1. The issue
	// is no one room: the document plays no role, so it is a room of its own, and d4's row and node
	// are keyed by its own ref_key, while the ask's row names the block of the spec's room.
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "choices.md",
		"content": ":::decision{#d4 from_ask=\"" + asked(blockAsk) + "\"}\n" + decisionUnderAskD4 + "\n:::\n",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload choices.md, holding d4: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	awaitNoSettlementOwed(t, database)
	choices, spec := readDecisionDocument(t, database, key, "choices-md"), readDecisionDocument(t, database, key, "spec")
	if choices.room != choices.id || spec.room == choices.id {
		t.Fatalf("choices.md is keyed under the room %s and the spec under %s, want choices.md a room of its own beside the spec's, or the case proves nothing", choices.room, spec.room)
	}
	d4 := blockAddressOf(t, database, key, "d4")
	if held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, d4, []string{graphstore.TuplePartOf}); err != nil ||
		len(held) != 1 || held[0].ToKind != graphstore.KindAsk || held[0].ToID != blockAsk {
		t.Errorf("decision %s of choices.md stands under %+v (%v), want ask %s alone", d4, held, err, blockAsk)
	}

	requireBlockCensusAgrees(t, database)

	// A PATCH of a decision that holds a from_ask, with a current If-Match, is refused before
	// anything is written, naming the decision and its ask.
	held, tag := placementOf(t, database, refs.BlockNodeKind, d2)
	before := readDeleteCounts(t, database)
	refused := patchNode(t, handler, key, d2, tag, map[string]any{"parent": d1}, "alice")
	refusal := decodeBody[refusalAnswer](t, refused)
	if refused.Code != http.StatusBadRequest || refusal.Code != codeNodeNotMovable {
		t.Errorf("PATCH of decision %s answered %d, want 400 %s: %s", d2, refused.Code, codeNodeNotMovable, refused.Body.String())
	} else if named := strings.Contains(refusal.Error, d2) || strings.Contains(refusal.Error, "#b-d2"); !named || !strings.Contains(refusal.Error, asked(first)) {
		t.Errorf("the refusal of the PATCH of decision %s does not name it and its ask %s: %s", d2, asked(first), refusal.Error)
	}
	requireUnchanged(t, database, "the refused PATCH of decision "+d2, before)
	if now, _ := placementOf(t, database, refs.BlockNodeKind, d2); now.Seq != held.Seq {
		t.Errorf("the refused PATCH of decision %s moved it under %s %s", d2, now.ToKind, now.ToID)
	}

	// The refusal names the ask only where the caller may read it. No writer reserves an ask for
	// its author (graphstore.CheckVisibilityAdmitted), so SQL reserves first, the ask d2 settles,
	// as an ask alice may not read would stand: alice's read of its address answers 404, and her
	// PATCH of d2 is still 400 NODE_NOT_MOVABLE, naming d2 and neither the ask's address nor its
	// id, and writes nothing.
	if read := dispatchReferences(t, handler, asked(first), "alice"); read.Code != http.StatusOK {
		t.Fatalf("alice's read of the ask %s answered %d before it is reserved, want 200: %s", asked(first), read.Code, read.Body.String())
	}
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		graphstore.KindAsk, first, graphstore.VisibilityAuthor); err != nil {
		t.Fatalf("reserve the ask %s for its author: %v", first, err)
	}
	if read := dispatchReferences(t, handler, asked(first), "alice"); read.Code != http.StatusNotFound {
		t.Fatalf("alice's read of the ask %s answered %d once it is reserved for its author, want 404, so this case proves nothing: %s", asked(first), read.Code, read.Body.String())
	}
	before = readDeleteCounts(t, database)
	refused = patchNode(t, handler, key, d2, tag, map[string]any{"parent": d1}, "alice")
	refusal = decodeBody[refusalAnswer](t, refused)
	if body := refused.Body.String(); refused.Code != http.StatusBadRequest || refusal.Code != codeNodeNotMovable {
		t.Errorf("PATCH of decision %s, whose ask alice may not read, answered %d, want 400 %s: %s", d2, refused.Code, codeNodeNotMovable, body)
	} else if named := strings.Contains(body, d2) || strings.Contains(body, "#b-d2"); !named || strings.Contains(body, first) {
		t.Errorf("the refusal of the PATCH of decision %s, whose ask alice may not read, does not name the decision, or names the ask %s: %s", d2, first, body)
	}
	requireUnchanged(t, database, "the refused PATCH of decision "+d2+", whose ask alice may not read", before)
}

// The words of the decision-under-ask test's spec and decisions, each unique in the store.
const (
	decisionUnderAskQuestion = "Which ledger do the writers take?"
	decisionUnderAskD1       = "Every writer takes one store."
	decisionUnderAskD2       = "The writers take the one ledger."
	decisionUnderAskD3       = "The ledger orders every write."
	decisionUnderAskD4       = "The choices take the ledger the spec asks about."
)

// decisionUnderAskSpec is the spec the test's issue is created with: a paragraph and an ask block,
// q1, whose ask d2 settles.
const decisionUnderAskSpec = "The writers' store comes first.\n\n:::ask{#q1}\n" + decisionUnderAskQuestion + "\n:::\n"

// decisionUnderAskOpen opens an ask of the issue key with question through POST
// /api/v1/issues/{key}/asks, as the agent's session, and answers its id.
func decisionUnderAskOpen(t *testing.T, handler http.Handler, key, question string) string {
	t.Helper()
	return decodeBody[model.Ask](t, created(t, "open the ask "+question, sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+key+"/asks", map[string]any{"question": question, "actor": sessionActor()}))).ID
}

// decisionUnderAskEdit applies ops to the spec of key through its edit route, written by the
// agent's session, waits until no document owes a settlement, and answers the number of the
// version the edit wrote.
func decisionUnderAskEdit(t *testing.T, handler http.Handler, database *store.Store, key string, ops ...map[string]any) int {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
		"ops": ops, "summary": "the agent's decisions", "actor": sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("the agent's edit %v of the spec of %s: status=%d body=%s", ops, key, response.Code, response.Body.String())
	}
	version := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, response).Version
	if version == nil {
		t.Fatalf("the agent's edit %v of the spec of %s wrote no version: %s", ops, key, response.Body.String())
	}
	awaitNoSettlementOwed(t, database)
	return version.Number
}

// requireDecisionUnder fails unless the decision block node at address stands under the node
// kind and id name: its one current part_of in the store, and its parent in the tree read of key.
func requireDecisionUnder(t *testing.T, handler http.Handler, database *store.Store, key, address, kind, id string) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the part_of of decision %s: %v", address, err)
	}
	switch {
	case len(held) != 1:
		t.Errorf("decision %s stands under %d nodes %+v, want %s %s alone", address, len(held), held, kind, id)
	case held[0].ToKind != kind || held[0].ToID != id:
		t.Errorf("decision %s stands under %s %s, want %s %s", address, held[0].ToKind, held[0].ToID, kind, id)
	}
	node, read := treeNodes(t, handler, key, "")[address]
	switch {
	case !read:
		t.Errorf("the tree read of %s answers no decision %s", key, address)
	case node.Parent == nil:
		t.Errorf("the tree read of %s answers decision %s under no node, want %s %s", key, address, kind, id)
	case node.Parent.Kind != kind || node.Parent.ID != id:
		t.Errorf("the tree read of %s answers decision %s under %s %s, want %s %s", key, address, node.Parent.Kind, node.Parent.ID, kind, id)
	}
}

// requireBlockCensusAgrees fails unless the store census kind block, taken over database as
// `envoy-dispatch census --store` takes it, read rows and nodes and counts no difference in either
// direction, naming each it counts.
func requireBlockCensusAgrees(t *testing.T, database *store.Store) {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind != "block" {
			continue
		}
		if kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("census kind block read %d rows and %d nodes, want the test's blocks on both sides", kind.Rows, kind.Nodes)
		}
		if counted := kind.RowDifferences + kind.NodeDifferences; counted != 0 {
			var at []string
			for _, difference := range slices.Concat(kind.ReportedRows, kind.ReportedNodes) {
				at = append(at, difference.At+" ("+difference.Cause+")")
			}
			t.Errorf("census kind block counts %d differences, want 0: %s", counted, strings.Join(at, "; "))
		}
		return
	}
	t.Fatalf("the store census read no kind block")
}
