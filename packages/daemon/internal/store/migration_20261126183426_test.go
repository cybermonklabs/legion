package store

import (
	"context"
	"testing"
)

// holdsDecisionVersion is 20261126183426_answer_questions_holds_decision, the migration that adds
// answer_questions.holds_decision.
const holdsDecisionVersion = 20261126183426

// A question records, where it is received, whether it holds a person's decision for its agent's
// wake (AnswerQuestion.HoldsDecision), and no later write changes that: a move of the question,
// which writes its state, its reply and its wake, leaves it as it was received. A question
// recorded before the column existed reads none, and so does one received without a decision.
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
	if got, inserted, err := store.ReceiveQuestion(ctx, held); err != nil || !inserted || !got.HoldsDecision {
		t.Fatalf("the question received with a decision = %+v, %t, %v; want it recorded holding the decision", got, inserted, err)
	}
	if _, _, err := store.ReceiveQuestion(ctx, testerQuestion("q-plain", "dispatch:m-1", "ses_tester", at(3))); err != nil {
		t.Fatal(err)
	}
	moved := held
	moved.State, moved.ReplyText, moved.Wake, moved.HoldsDecision, moved.UpdatedAt = QuestionAppended, "Plum.", false, false, at(4)
	if err := store.UpdateQuestion(ctx, moved, QuestionReceived); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]bool{"q-before": false, "q-held": true, "q-plain": false} {
		q, ok, err := store.Question(ctx, id)
		if err != nil || !ok {
			t.Fatalf("read %s: %t, %v", id, ok, err)
		}
		if q.HoldsDecision != want {
			t.Errorf("%s holds a decision %t, want %t", id, q.HoldsDecision, want)
		}
	}
}
