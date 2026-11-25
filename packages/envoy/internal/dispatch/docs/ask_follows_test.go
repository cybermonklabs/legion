package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TestAnAskOpenedBesideARepairIsCausedByItsOwnEvent: one settlement repairs a block, recording its
// block.repaired, and then opens an ask for a new block, recording that ask's follow beside its
// ask.opened, and the version's own event comes first among what the settlement appends. The
// ask.opened is not where the follow was recorded, so causedByEvent finds it by the ask its payload
// names, and an opened ask whose ask.opened is not among the appended events is an error that
// names the ask, not a panic.
func TestAnAskOpenedBesideARepairIsCausedByItsOwnEvent(t *testing.T) {
	tree, err := pmdoc.Parse(":::ask{#ask-0 urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"mallory\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	block := tree.Children[0]

	var reconciled settlementReconciliation
	repaired := model.Event{Type: "block.repaired", Payload: model.BlockRepairedEventPayload{BlockID: "ask-0"}}
	open := model.Ask{ID: "ask-row-0", State: "open"}
	if !reconciled.repairAsk(block, "ask-0", askReadableWrite(false, open, ""), &repaired) {
		t.Fatal("the repair taking the forged answerer off changed nothing")
	}
	opened := model.Ask{ID: "ask-row-1", State: "open"}
	var node, followed graphstore.Uncaused
	reconciled.follows = append(reconciled.follows, openedFollow{ask: opened.ID, node: &node, followed: &followed})
	reconciled.events = append(reconciled.events, model.Event{Type: "ask.opened", Payload: model.AskEventPayload{Ask: opened}})
	if len(reconciled.events) != 2 || reconciled.events[1].Type != "ask.opened" {
		t.Fatalf("the reconciliation's events are %+v, want the block.repaired and then the ask.opened", reconciled.events)
	}

	appended := []model.Event{{ID: 41, Type: "artifact.version"}}
	appended = append(appended, reconciled.events...)
	caused, err := reconciled.causedByEvent(appended)
	if err != nil {
		t.Fatalf("match the follows to their events: %v", err)
	}
	if stated := caused[2]; len(caused) != 1 || len(stated) != 2 || stated[0] != &node || stated[1] != &followed {
		t.Fatalf("the follows matched to %v, want the node and the follow of ask %s at the ask.opened's place, 2", caused, opened.ID)
	}
	if _, err := reconciled.causedByEvent(appended[:2]); err == nil || !strings.Contains(err.Error(), opened.ID) {
		t.Fatalf("matching a follow whose ask.opened was not appended answered %v, want an error naming ask %s", err, opened.ID)
	}
}

// TestAnAskOpenedBesideAWithheldAnswerIsCausedByItsOwnEvent: one settlement, while another build
// serves, repairs a block whose ask is answered by writing the answer's words back into it,
// recording its block.repaired, and then opens an ask for a new block, recording that ask's follow
// beside its ask.opened. The document has no room for the answer, so withholdAnswersWithoutRoom
// takes the repair back, and since the block without its answer is the block settlement found, it
// drops the block.repaired too, which moves the ask.opened one place up the events. A follow that
// kept the place its ask.opened had when it was recorded would point past the end of the events
// settlement appends, so causedByEvent finds the ask.opened by the ask its payload names and
// states the follow under it at its new place, and an opened ask whose ask.opened is not among the
// appended events is an error that names the ask, not a panic.
func TestAnAskOpenedBesideAWithheldAnswerIsCausedByItsOwnEvent(t *testing.T) {
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	// The answer and the prose are sized so the document holds its block without the answer, and
	// passes what one upload may hold with it.
	text := strings.Repeat("word ", 4000)
	prose := strings.Repeat("word ", (pmdoc.MaxDocumentBytes-len(text)/2)/len("word "))
	tree, err := pmdoc.Parse(prose + "\n\n:::ask{#ask-0 urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	nodes, _ := askNodes(tree)
	block := nodes["ask-0"]
	if block == nil {
		t.Fatal("the document holds no ask block ask-0")
	}
	answered := model.Ask{ID: "ask-row-0", State: "answered",
		Answer: &model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at}}
	// The block as an earlier settlement left it: answered, and its answer withheld.
	if !askReadableWriteWithheld(answered, "")(block) {
		t.Fatal("writing the withheld answer changed nothing in the block")
	}
	before, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}

	var reconciled settlementReconciliation
	repaired := model.Event{Type: "block.repaired", Payload: model.BlockRepairedEventPayload{BlockID: "ask-0"}}
	if !reconciled.repairAskInStep(block, "ask-0", true, answered, "", &repaired) {
		t.Fatal("the repair writing the answer back changed nothing")
	}
	opened := model.Ask{ID: "ask-row-1", State: "open"}
	var node, followed graphstore.Uncaused
	reconciled.follows = append(reconciled.follows, openedFollow{ask: opened.ID, node: &node, followed: &followed})
	reconciled.events = append(reconciled.events, model.Event{Type: "ask.opened", Payload: model.AskEventPayload{Ask: opened}})
	reconciled.withholdAnswersWithoutRoom("document", tree, before)
	if len(reconciled.events) != 1 || reconciled.events[0].Type != "ask.opened" {
		t.Fatalf("the reconciliation's events are %+v, want the ask.opened alone once the block.repaired of the withheld answer is dropped",
			reconciled.events)
	}

	appended := []model.Event{{ID: 41, Type: "artifact.version"}}
	appended = append(appended, reconciled.events...)
	caused, err := reconciled.causedByEvent(appended)
	if err != nil {
		t.Fatalf("match the follows to their events: %v", err)
	}
	if stated := caused[1]; len(caused) != 1 || len(stated) != 2 || stated[0] != &node || stated[1] != &followed {
		t.Fatalf("the follows matched to %v, want the node and the follow of ask %s at the ask.opened's place, 1", caused, opened.ID)
	}
	if _, err := reconciled.causedByEvent(appended[:1]); err == nil || !strings.Contains(err.Error(), opened.ID) {
		t.Fatalf("matching a follow whose ask.opened was not appended answered %v, want an error naming ask %s", err, opened.ID)
	}
}
