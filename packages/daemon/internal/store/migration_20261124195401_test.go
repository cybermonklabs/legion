package store

import (
	"context"
	"reflect"
	"testing"
)

// messageWakeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const messageWakeVersion = 20261124195401

// 20261124195401 adds claims.message_wake_at and back-fills nothing: a row written before it
// reads null, which is a claim no message waits for (dispatch://INFRA-5682 R1). The row is
// written with SQL that names the columns of the schema the migration finds, as the release
// before it writes them.
func TestTheMigrationAddsTheMessageWakeMarkAndMarksNoClaim(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, messageWakeVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak, last_turn_start)
		values ('legion-legion-legion-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'suspended',
		0, 0, 0, 0, now())`); err != nil {
		t.Fatalf("record the claim of the release before: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the message wake mark: %v", err)
	}

	got := rowsAsText(t, store, `select row(token, message_wake_at is null)::text from claims order by token`)
	want := []string{"(legion-legion-legion-208-implementer,t)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (token, holds no message mark) = %v, want %v", got, want)
	}
}
