package store

import (
	"context"
	"testing"
)

// holdsDecisionVersion is 20261126183426_answer_questions_holds_decision, the migration that adds
// answer_questions.holds_decision and answer_questions.decision_relayed.
const holdsDecisionVersion = 20261126183426

// A question records, where it is received, whether it holds a person's decision for its agent's
// wake (AnswerQuestion.HoldsDecision) and whether the parked-decision relay carries that decision
// (AnswerQuestion.DecisionRelayed), and no later write changes either: a move of the question,
// which writes its state, its reply and its wake, leaves both as they were received. A question
// recorded before the columns existed reads neither, and so does one received without a decision.
func TestAQuestionHoldsTheDecisionItWasReceivedWith(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, holdsDecisionVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into answer_questions (id, project, identity, session, token, issue, role, kind,
		may_ask_nothing, handoff, wake, state, received_at, last_seen_at, updated_at)
		values ('q-before', 'legion', 'dispatch:m-before', 'ses_tester', $1, 'LEGION-209', 'tester', 'ask_reply', true, '{}', true,
			'received', $2, $2, $2)`, string(testerToken), at(1)); err != nil {
		t.Fatalf("record a question before the migration: %v", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through holds_decision: %v", err)
	}

	held := testerQuestion("q-held", "dispatch:34", "ses_tester", at(2))
	held.Kind, held.MayAskNothing, held.Wake, held.HoldsDecision = "ask_reply", true, true, true
	relayed := testerQuestion("q-relayed", "dispatch:35", "ses_tester", at(3))
	relayed.Kind, relayed.MayAskNothing, relayed.Wake, relayed.HoldsDecision, relayed.DecisionRelayed = "ask_reply", true, true, true, true
	for _, q := range []AnswerQuestion{held, relayed} {
		if got, inserted, err := store.ReceiveQuestion(ctx, q); err != nil || !inserted || !got.HoldsDecision || got.DecisionRelayed != q.DecisionRelayed {
			t.Fatalf("the question received with a decision = %+v, %t, %v; want it recorded holding the decision, relayed %t", got, inserted, err,
				q.DecisionRelayed)
		}
	}
	if _, _, err := store.ReceiveQuestion(ctx, testerQuestion("q-plain", "dispatch:m-1", "ses_tester", at(4))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []AnswerQuestion{held, relayed} {
		moved := q
		moved.State, moved.ReplyText, moved.Wake, moved.HoldsDecision, moved.DecisionRelayed, moved.UpdatedAt = QuestionAppended, "Plum.", false, false,
			false, at(5)
		if err := store.UpdateQuestion(ctx, moved, QuestionReceived); err != nil {
			t.Fatal(err)
		}
	}

	type holds struct{ decision, relayed bool }
	for id, want := range map[string]holds{"q-before": {}, "q-held": {decision: true}, "q-relayed": {decision: true, relayed: true}, "q-plain": {}} {
		q, ok, err := store.Question(ctx, id)
		if err != nil || !ok {
			t.Fatalf("read %s: %t, %v", id, ok, err)
		}
		if got := (holds{q.HoldsDecision, q.DecisionRelayed}); got != want {
			t.Errorf("%s holds a decision %t, relayed %t; want %t, %t", id, got.decision, got.relayed, want.decision, want.relayed)
		}
	}
}
