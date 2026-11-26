package docs

import (
	"strings"
	"testing"

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
	if !reconciled.repairAsk(block, "ask-0", askReadableWrite(open, ""), &repaired) {
		t.Fatal("the repair taking the forged answerer off changed nothing")
	}
	opened := model.Ask{ID: "ask-row-1", State: "open"}
	var node, followed graphstore.Uncaused
	reconciled.follows = append(reconciled.follows, openedFollow{ask: opened.ID, node: &node, followed: &followed})
	reconciled.events = append(reconciled.events, model.Event{Type: "ask.opened", Payload: model.AskEventPayload{Ask: opened}})
	if len(reconciled.events) != 2 || reconciled.events[1].Type != "ask.opened" {
		t.Fatalf("the reconciliation's events are %+v, want the block.repaired and then the ask.opened", reconciled.events)
	}

	requireFollowCausedAt(t, &reconciled, opened.ID, &node, &followed, 2)
}

// requireFollowCausedAt holds what causedByEvent makes of the follow of ask, whose node and follow
// are node and followed, over the version's own event and then reconciled's events: it states the
// two under the ask.opened at place, and over the events before place, which leave that
// ask.opened out, it answers an error that names the ask.
func requireFollowCausedAt(t *testing.T, reconciled *settlementReconciliation, ask string, node, followed *graphstore.Uncaused, place int) {
	t.Helper()
	appended := []model.Event{{ID: 41, Type: "artifact.version"}}
	appended = append(appended, reconciled.events...)
	caused, err := reconciled.causedByEvent(appended)
	if err != nil {
		t.Fatalf("match the follows to their events: %v", err)
	}
	if stated := caused[place]; len(caused) != 1 || len(stated) != 2 || stated[0] != node || stated[1] != followed {
		t.Fatalf("the follows matched to %v, want the node and the follow of ask %s at the ask.opened's place, %d", caused, ask, place)
	}
	if _, err := reconciled.causedByEvent(appended[:place]); err == nil || !strings.Contains(err.Error(), ask) {
		t.Fatalf("matching a follow whose ask.opened was not appended answered %v, want an error naming ask %s", err, ask)
	}
}
