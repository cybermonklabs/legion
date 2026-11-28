package store

import (
	"context"
	"reflect"
	"strconv"
	"testing"
)

// startFailureAfterReadyVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const startFailureAfterReadyVersion = 20261123212127

// 20261123212127 clears the start failure an earlier release kept on a claim whose agent came up
// after it: every claim stored ready, working or idle loses it (dispatch://INFRA-5463). A claim
// stored in any other state keeps its start failure, since nothing in the row says its agent came
// up after that start, and every claim keeps its death cause. The rows are written with SQL that
// names the columns of the schema the migration finds, as the release before writes them.
func TestTheMigrationClearsTheStartFailureOfAClaimWhoseAgentIsUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, startFailureAfterReadyVersion-1)
	states := []string{
		"queued", "launch_uncertain", "launching", "shim_connected", "registered",
		"ready", "working", "idle", "suspended", "failed", "retired",
	}
	for i, state := range states {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, last_start_failure, last_death_cause)
			values ($1, 'legion', $2, $2, 'implementer', 1, '', '', $3, 0, 0, 0, 0, $4, $5)`,
			"legion-legion-legion-"+strconv.Itoa(100+i)+"-implementer", "LEGION-"+strconv.Itoa(100+i), state,
			"launch: the served check could not be run: waitid: no child processes",
			"the pane's process was gone at the daemon's read"); err != nil {
			t.Fatalf("record the %s claim: %v", state, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the start failure clear: %v", err)
	}

	got := rowsAsText(t, store, `select row(state, last_start_failure <> '', last_death_cause <> '')::text from claims order by issue`)
	want := []string{
		"(queued,t,t)", "(launch_uncertain,t,t)", "(launching,t,t)", "(shim_connected,t,t)", "(registered,t,t)",
		"(ready,f,t)", "(working,f,t)", "(idle,f,t)", "(suspended,t,t)", "(failed,t,t)", "(retired,t,t)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (state, holds a start failure, holds a death cause) = %v, want %v", got, want)
	}
}
