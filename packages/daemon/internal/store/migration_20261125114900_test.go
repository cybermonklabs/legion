package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// launchedDefinitionVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const launchedDefinitionVersion = 20261125114900

// launchedDefinitionRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const launchedDefinitionRelease = "the release that keeps the definition of a plan's role a claim's process was launched with"

// 20261125114900 adds claims.launched_definition and claims.launched_generation
// (supervise.Claim.LaunchedDefinition, LaunchedGeneration), the empty string and 0 on each claim a
// release before wrote, since no such release launched a role of a plan, and the store writes and
// reads the definition and generation each launch records, a later launch's replacing an earlier
// one. The way back drops both columns, so the roll forward reads each claim as launched with no
// plan's definition, and a start of a plan role's step relaunches its process.
func TestAClaimKeepsTheDefinitionItsProcessWasLaunchedWith(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, launchedDefinitionVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	earlier, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	launched, err := claim.NewToken("legion", "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	// A claim as the release before wrote it.
	exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses', '', 'idle', 0, 0, 0, 0)`, string(earlier))
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the definition a claim's process was launched with: %v", err)
	}
	const definitions = `select token || ' [' || launched_definition || '] ' || launched_generation from claims order by token`
	if got, want := rowsAsText(t, store, definitions), []string{string(earlier) + " [] 0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the definitions after the migration = %v, want %v: the release before launched no role of a plan", got, want)
	}

	launchOf := func(token claim.Token) (string, uint64) {
		t.Helper()
		claims, err := store.Claims(ctx)
		if err != nil {
			t.Fatalf("read the claims: %v", err)
		}
		for _, c := range claims {
			if c.Token == token {
				return c.LaunchedDefinition, c.LaunchedGeneration
			}
		}
		t.Fatalf("no claim %s was read back", token)
		return "", 0
	}
	for i, definition := range []string{"d1", "d2"} {
		generation := uint64(i + 1)
		if err := store.PutClaim(ctx, supervise.Claim{Token: launched, Project: "legion", Tree: "LEGION-209", Issue: "LEGION-209",
			Role: claim.RoleImplementer, Generation: generation, State: supervise.StateIdle, LaunchedDefinition: definition, LaunchedGeneration: generation}); err != nil {
			t.Fatalf("write the claim launched with %s at generation %d: %v", definition, generation, err)
		}
		if got, at := launchOf(launched); got != definition || at != generation {
			t.Fatalf("the claim written with the definition %s at generation %d reads back %q at %d", definition, generation, got, at)
		}
	}

	exec(wayBackOf(t, launchedDefinitionVersion, launchedDefinitionRelease))
	if got := rowsAsText(t, store, `select count(*)::text from information_schema.columns where table_name = 'claims' and column_name in ('launched_definition', 'launched_generation')`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the columns launched_definition and launched_generation after the way back = %v, want none", got)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261125114900`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the version rows of the migration after the way back = %v, want none", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if got, want := rowsAsText(t, store, definitions), []string{string(earlier) + " [] 0", string(launched) + " [] 0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the definitions after the roll forward = %v, want %v", got, want)
	}
}
