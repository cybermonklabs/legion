package docs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The text routes' asks member reads each ask's answer off the ask's node, not its row (R3: every
// read of an answer reads the node): while the row holds an answer a process of the release before
// wrote into it alone, before the decisions step's catch-up
// states it on the node, the text reads the ask open, as its node holds it, with no answerer; once
// the node holds an answer, the text reads that one.
func TestTheTextsAsksReadTheAnswerOffTheAsksNodeNotItsRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ship")
	rowAnswer, err := json.Marshal(model.AskAnswer{User: "bob", Selected: []string{"Yes"}, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("encode the row's answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, rowAnswer); err != nil {
		t.Fatalf("answer the ask's row alone, as the release before writes it: %v", err)
	}
	read, err := service.TextWithAsks(ctx, artifactID, false)
	if err != nil {
		t.Fatalf("read the text: %v", err)
	}
	if len(read.Asks) != 1 || read.Asks[0].State != asks.StateOpen || read.Asks[0].AnsweredBy != "" {
		t.Fatalf("the text's asks with the row answered by bob and the node open = %s, want ship open with no answerer, as its node holds it", askLines(read.Asks))
	}

	answerAskOnItsNode(t, service, ask.ID, model.AskAnswer{User: "alice", Selected: []string{"No"}, At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)})
	if read, err = service.TextWithAsks(ctx, artifactID, false); err != nil {
		t.Fatalf("read the text: %v", err)
	}
	const answered = `#ship state="answered" answered_by="alice" answered_at="2026-11-20T10:00:00Z" selected=["No"]` + "\n"
	if got := askLines(read.Asks); got != answered {
		t.Fatalf("the text's asks once the node holds alice's answer = %s, want %s", got, answered)
	}
}
