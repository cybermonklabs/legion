package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// sessionRunVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const sessionRunVersion = 20261125021636

// 20261125021636 adds claims.session_run, the run of a claim's session since it last finished a
// turn (supervise.Claim.SessionRun, dispatch://INFRA-2045 R1). A claim standing across the
// migration that records a session reads as one whose session has finished a turn and ended no
// launch, so the controller's replacement rule can reach a session already alive at the upgrade;
// one that records no session reads the empty run. A claim the release before it inserts afterwards,
// its columns named, takes the empty run. This release writes the run and reads it back whole.
//
// The negative control is the read of the column before the migration, which the database refuses
// as a column it does not know. The control of the back-fill is the migration without its update:
// the claim with a session then reads a session that never finished a turn, and this test fails.
func TestAClaimsSessionRunIsStoredWith20261125021636(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, sessionRunVersion-1)
	// insert is a claim as the release before the migration writes it, the columns of the schema
	// it finds named.
	insert := func(token, session string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', '', '', 'controller', 1, $2, '', 'idle', 0, 0, 0, 0)`, token, session); err != nil {
			t.Fatalf("record the claim %s: %v", token, err)
		}
	}
	insert("legion-a-controller", "01a1f630-e11e-714b-bf51-e3c604e6ee47")
	insert("legion-b-controller", "")
	var run string
	if err := store.Pool().QueryRow(ctx, `select session_run::text from claims limit 1`).Scan(&run); err == nil || !strings.Contains(err.Error(), "session_run") {
		t.Fatalf("reading session_run before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the session's run: %v", err)
	}
	insert("legion-c-controller", "01a20ff7-0000-7000-8000-000000000000")

	got := rowsAsText(t, store, `select row(token, session_run)::text from claims order by token`)
	want := []string{
		`(legion-a-controller,"{""turnFinished"": true}")`,
		`(legion-b-controller,{})`,
		`(legion-c-controller,{})`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (token, session_run) = %v, want %v", got, want)
	}

	// This release writes the run and reads it back whole.
	c := supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController,
		State: supervise.StateIdle, Generation: 2, Session: "ses_controller", SessionFile: "/sessions/controller.jsonl",
		SessionRun: supervise.SessionRun{Ends: 2, Endings: []string{"its process died before the controller was ready: OOMKilled",
			"its turn stayed open longer than 1h30m0s, from 2026-11-24T22:53:47Z"}, TurnFinished: true}}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim: %v", err)
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, read := range claims {
		if read.Token == c.Token {
			if !reflect.DeepEqual(read.SessionRun, c.SessionRun) {
				t.Fatalf("the claim reads back the run %+v, want %+v", read.SessionRun, c.SessionRun)
			}
			return
		}
	}
	t.Fatalf("the claims read back hold no %s", c.Token)
}
