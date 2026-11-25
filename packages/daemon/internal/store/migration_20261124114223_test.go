package store

import (
	"context"
	"strings"
	"testing"
)

// releasedAgentMessagesVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const releasedAgentMessagesVersion = 20261124114223

// 20261124114223 gives the answer loop what it holds for an agent whose claim the daemon
// released: answer_questions.wake, false on a question recorded before it and kept as the loop
// writes it after, the state woken beside every state before it, and the outbox kind
// agent_message beside every kind the check named before. A state and a kind the daemon does not
// write are still refused. The negative controls are the woken question and the agent_message row
// written before the migration, which the two checks refuse. The rows before it are written with
// SQL that names the columns of the schema the migration finds, as the release before writes them.
func TestMigrationReleasedAgentMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, releasedAgentMessagesVersion-1)
	question := func(id, state string) error {
		_, err := store.Pool().Exec(ctx, `insert into answer_questions (id, project, identity, session, token, issue, role, kind,
				may_ask_nothing, handoff, state, received_at, last_seen_at, updated_at)
			values ($1, 'legion', $1, 'ses_tester', $2, 'LEGION-209', 'tester', 'message', false, '{}', $3, $4, $4, $4)`,
			id, string(testerToken), state, at(0))
		return err
	}
	row := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-209', '{}', 0, now(), '')`, kind)
		return err
	}
	if err := question("q-woken-early", "woken"); err == nil || !strings.Contains(err.Error(), "answer_questions_state_check") {
		t.Fatalf("a woken question recorded before the migration: %v; want the state check to refuse it", err)
	}
	if err := row("agent_message"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("an agent_message row queued before the migration: %v; want the kind check to refuse it", err)
	}
	if err := question("q-before", "received"); err != nil {
		t.Fatalf("record the question of the release before: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the released agent's messages: %v", err)
	}

	if got, ok, err := store.Question(ctx, "q-before"); err != nil || !ok || got.Wake {
		t.Fatalf("the question recorded before the migration = %+v, %t, %v; want it read with no wake", got, ok, err)
	}
	held := testerQuestion("q-held", "dispatch:c-held", "ses_tester", at(1))
	held.Wake = true
	if got, inserted, err := store.ReceiveQuestion(ctx, held); err != nil || !inserted || !got.Wake {
		t.Fatalf("a question received for a released claim = %+v, %t, %v; want it recorded with its wake", got, inserted, err)
	}
	held.State, held.UpdatedAt = QuestionWoken, at(2)
	if err := store.UpdateQuestion(ctx, held, QuestionReceived); err != nil {
		t.Fatalf("move the held question to woken: %v", err)
	}
	if got, _, err := store.Question(ctx, "q-held"); err != nil || got.State != QuestionWoken || !got.Wake {
		t.Fatalf("the held question after its move = %+v, %v; want it woken with its wake", got, err)
	}
	for i, state := range []string{"received", "generating", "appended", "posted", "forwarded", "passed_on", "dropped", "woken"} {
		if err := question("q-"+state, state); err != nil {
			t.Errorf("a question in the state %s (%d): %v; want it recorded", state, i, err)
		}
	}
	if err := question("q-lost", "lost"); err == nil || !strings.Contains(err.Error(), "answer_questions_state_check") {
		t.Errorf("a question in the state lost: %v; want the state check to refuse a state the loop does not write", err)
	}
	for _, kind := range []string{"notice", "controller_notice", "hook_prompt", "merge_forward", "design_changes", "agent_message"} {
		if err := row(kind); err != nil {
			t.Errorf("a %s row after the migration: %v; want it queued", kind, err)
		}
	}
	if err := row("merge_queue_publish"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Errorf("a merge_queue_publish row after the migration: %v; want the kind check to refuse a kind this release does not write", err)
	}
}
