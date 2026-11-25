package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's start task named a kept decision by its reference alone and sent the architect to read it
// (workflow/parked_decision.go:KeptDecisionsTask at d46ec4e0: "While nobody in %s's tree could
// hear it, a person decided on the tree: %s. Read each before you continue."), and main's
// putBackARetakenConfirmation (store/kept_decisions.go at d46ec4e0) put a decision back so that
// "the resend names them again; a second telling is the worst case, never a loss".
// dispatch://INFRA-3686's R1 has the texts carry the person's words instead: "It carries up to
// 4,000 characters of one act ... Above that, it carries the first part, the count of the rest
// (or that more follows, where the event did not carry the whole text) and the item's
// reference." A changes request's reason reaches the kept row already cut by its notice, with the
// count of what the cut left off (the daemon's noticeWords: "the kept words carry that count
// (Rest), and every text that names the decision later gives the count of the rest"). The
// put-back's own comment says "The words go back with the row, read from the mark's own fields
// (record.ParkedDecision.Mark), so the resend names them as the first send did", and the mark
// holds that count (record.DecisionMark.Rest). Here a kept changes request whose reason its notice
// cut is named by a start task whose confirmation is taken back; the row put back must keep the
// count of the rest, or every later start or catch-up that names it from the row carries the
// reason's first part as if it were whole.
func TestADecisionPutBackKeepsTheCountOfTheRestOfItsWords(t *testing.T) {
	r := newKeptRig(t)
	const tree = "LEGION-302"
	ref := "dispatch://LEGION-302/artifact/spec"
	r.issue(tree, tree)
	start := r.startRow(tree)
	r.exec(`insert into parked_decisions (issue, ref, decision, actor, at, claimed_by) values ($1, $2, 'design-changes-requested', 'user:alice', $3, $4)`,
		tree, ref, r.at, start)
	const cutOff = 1500
	words := &record.ActWords{Text: strings.Repeat("Rework the plan. ", texts.OneAct/len("Rework the plan. ")), By: "user:alice",
		At: r.at.Add(-time.Hour), Rest: cutOff}
	mark := record.ParkedDecision{Issue: tree, Ref: ref, Decision: record.DecisionDesignChangesRequested, Actor: "user:alice", At: r.at,
		Words: words}.Mark()
	if told := mark.Carry(texts.OneAct); told.Rest != cutOff {
		t.Fatalf("the start task's mark carries the reason with rest %d, want %d; this test proves nothing", told.Rest, cutOff)
	}
	kept, err := json.Marshal(record.KeptDecisions{Decisions: []record.DecisionMark{mark}})
	if err != nil {
		t.Fatal(err)
	}
	architect := r.architect(tree, start)
	d := supervise.Delivery{ID: "outbox:1", Task: "a person decided on the tree", Generation: 1, QueuedAt: r.at, Kept: kept}
	if err := r.store.PutClaimAndDelivery(r.ctx, architect, d); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	confirmed := d
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, architect, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	if err := r.store.PutDelivery(r.ctx, architect.Token, d); err != nil {
		t.Fatalf("write the delivery back unconfirmed: %v", err)
	}
	var back []record.ParkedDecision
	if err := Transact(r.ctx, r.store.Pool(), func(_ context.Context, tx pgx.Tx) error {
		var err error
		back, err = record.NewStore().ParkedDecisions(r.ctx, tx, tree)
		return err
	}); err != nil {
		t.Fatalf("read the decision put back: %v", err)
	}
	if len(back) != 1 || back[0].Words == nil {
		t.Fatalf("the rows put back are %+v; want the changes request with its words", back)
	}
	if told := back[0].Mark().Carry(texts.OneAct); told.Rest != cutOff {
		t.Errorf("the changes request put back carries its reason's first %d units with rest %d (words by %q at %s, more %t, rest %d): a start or "+
			"catch-up that names it from this row gives the reason as whole, where the notice cut %d units off it and the start task's mark counted them",
			len(told.Text), told.Rest, back[0].Words.By, back[0].Words.At.Format(time.RFC3339), back[0].Words.More, back[0].Words.Rest, cutOff)
	}
}
