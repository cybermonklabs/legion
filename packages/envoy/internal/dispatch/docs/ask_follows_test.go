package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TestAnAskOpenedBesideAWithheldAnswerIsCausedByItsOwnEvent is the settlement that panicked in
// api's TestSettlementCannotGrowADocumentPastWhatOneUploadMayHoldByRestoringAnswers. One
// settlement repairs a block whose ask is answered, recording its block.repaired, and then opens
// an ask for a new block, recording that ask's follow beside its ask.opened. The document has no
// room for the answer, so applyAnswers takes the repair back, and since the block without its
// answer is the block settlement found, it drops the block.repaired too, which moves the
// ask.opened one place up the events. A follow that remembered the ask.opened's position pointed
// past the end of the events settlement appended. followsByEvent finds the ask.opened by the ask its
// payload names instead, and an opened ask whose ask.opened is not among the appended events is
// an error that names the ask, not a panic.
func TestAnAskOpenedBesideAWithheldAnswerIsCausedByItsOwnEvent(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	// The answer and the prose are sized so the document holds its block without the answer, and
	// passes what one upload may hold with it.
	text := strings.Repeat("word ", 4000)
	prose := strings.Repeat("word ", (pmdoc.MaxDocumentBytes-len(text)/2)/len("word "))
	tree, err := pmdoc.Parse(prose + "\n\n:::ask{#ask-0 urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	var block *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			block = node
		}
		return block == nil
	})
	if block == nil {
		t.Fatal("the document holds no ask block")
	}
	answered := model.Ask{ID: "ask-row-0", State: "answered",
		Answer: &model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at}}
	// The block as an earlier settlement left it: answered, and its answer withheld.
	setAskServerAttributes(block, answered, "", true)
	before, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}

	var reconciled settlementReconciliation
	repaired := model.Event{Type: "block.repaired", Payload: model.BlockRepairedEventPayload{BlockID: "ask-0"}}
	if !reconciled.repairAsk(block, "ask-0", answered, "", &repaired) {
		t.Fatal("the repair writing the answer back changed nothing")
	}
	opened := model.Ask{ID: "ask-row-1", State: "open"}
	var node, followed graphstore.Uncaused
	reconciled.follows = append(reconciled.follows, openedFollow{ask: opened.ID, node: &node, followed: &followed})
	reconciled.events = append(reconciled.events, model.Event{Type: "ask.opened", Payload: model.AskEventPayload{Ask: opened}})
	decision, _, _ := reconciled.weighAnswers(tree, before)
	reconciled.applyAnswers("document", decision)
	if len(reconciled.events) != 1 || reconciled.events[0].Type != "ask.opened" {
		t.Fatalf("the reconciliation's events are %+v, want the ask.opened alone once the block.repaired of the withheld answer is dropped",
			reconciled.events)
	}

	appended := []model.Event{{ID: 41, Type: "artifact.version"}}
	appended = append(appended, reconciled.events...)
	caused, err := reconciled.followsByEvent(appended)
	if err != nil {
		t.Fatalf("match the follows to their events: %v", err)
	}
	if stated := caused[1]; len(caused) != 1 || len(stated) != 2 || stated[0] != &node || stated[1] != &followed {
		t.Fatalf("the follows matched to %v, want the node and the follow of ask %s at the ask.opened's place, 1", caused, opened.ID)
	}
	if _, err := reconciled.followsByEvent(appended[:1]); err == nil || !strings.Contains(err.Error(), opened.ID) {
		t.Fatalf("matching a follow whose ask.opened was not appended answered %v, want an error naming ask %s", err, opened.ID)
	}
}
