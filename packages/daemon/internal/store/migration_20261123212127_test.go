package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// startFailureAfterReadyVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const startFailureAfterReadyVersion = 20261123212127

// 20261123212127 clears the start failure an earlier release kept on a claim whose agent came up
// after it: every claim stored ready, working or idle loses it (dispatch://INFRA-5463). A claim
// stored queued, launching, suspended or failed keeps its start failure, since nothing in the row
// says its agent came up after that start, and so does every other column of every row.
func TestTheMigrationClearsTheStartFailureOfAClaimWhoseAgentIsUp(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, startFailureAfterReadyVersion-1)
	const refusal = "launch: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"
	const death = "the pane's process was gone at the daemon's read"
	states := []supervise.ClaimState{
		supervise.StateReady, supervise.StateWorking, supervise.StateIdle,
		supervise.StateQueued, supervise.StateLaunching, supervise.StateSuspended, supervise.StateFailed,
	}
	for i, state := range states {
		issue := "LEGION-" + string(rune('1'+i))
		token, err := claim.NewToken("legion", issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue,
			Role: claim.RoleImplementer, Generation: 1, State: state, LastStartFailure: refusal, LastDeathCause: death}); err != nil {
			t.Fatalf("record the %s claim: %v", state, err)
		}
	}
	// One claim that never failed a start, so the clear's own filter is held too.
	if err := store.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-9-implementer", Project: "legion", Tree: "LEGION-9",
		Issue: "LEGION-9", Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatalf("record the claim with no start failure: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the start failure clear: %v", err)
	}

	got := rowsAsText(t, store, `select row(issue, state, last_start_failure <> '', last_death_cause <> '')::text from claims order by issue`)
	want := []string{
		"(LEGION-1,ready,f,t)", "(LEGION-2,working,f,t)", "(LEGION-3,idle,f,t)",
		"(LEGION-4,queued,t,t)", "(LEGION-5,launching,t,t)", "(LEGION-6,suspended,t,t)", "(LEGION-7,failed,t,t)",
		"(LEGION-9,idle,f,f)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (issue, state, holds a start failure, holds a death cause) = %v, want %v", got, want)
	}
}
