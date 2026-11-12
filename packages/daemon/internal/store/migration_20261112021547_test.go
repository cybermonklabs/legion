package store

import (
	"context"
	"testing"
)

// pluginContractGenerationVersion is the version of the migration this file tests, so the test
// can migrate through everything before it whatever lands beside it.
const pluginContractGenerationVersion = 20261112021547

// claims.plugin_contract_generation arrives 0 for every row the table already holds, and 0 means
// "no contract is recorded for the generation this claim stands in": every view of such a claim
// gives its contract as 0 (api.PluginContractOf, dispatch://INFRA-2883 R7). Left at that, the
// upgrade would show every claim of the fleet as having registered nothing, so the migration
// backfills the claims whose recorded registration is certainly their current generation's: a
// contract is recorded and the state holds a capability — registered, ready, working, idle — a
// registered agent with a running process, which no launch can have moved past since.
//
// Every other claim keeps the default and reads as what the record says it is. A suspended,
// failed or retired claim is exactly the case the column exists for: nothing in the row says
// whether the generation it stands in registered and then lost its process or never registered,
// which is how two role pods that came back with no pi-legion were both shown as plugin contract
// 59, a number of an earlier generation's registration.
//
// The stored plugin_contract is untouched by all of it: the eight readers that decide on it read
// what they always read.
func TestThePluginContractGenerationMigrationBackfills(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, pluginContractGenerationVersion-1)

	rows := []struct {
		token      string
		state      string
		generation int
		contract   int
		want       int
		why        string
	}{
		{token: "legion-LEGION-208-architect", state: "ready", generation: 3, contract: 59, want: 3,
			why: "a ready claim holds the registration of the process it stands in"},
		{token: "legion-LEGION-209-implementer", state: "working", generation: 2, contract: 46, want: 2,
			why: "a working claim holds the registration of the process it stands in"},
		{token: "legion-LEGION-209-tester", state: "registered", generation: 1, contract: 59, want: 1,
			why: "a registered claim is the registration itself"},
		{token: "legion-LEGION-210-reviewer", state: "idle", generation: 7, contract: 59, want: 7,
			why: "an idle claim holds the registration of the process it stands in"},
		{token: "legion-LEGION-211-implementer", state: "failed", generation: 2, contract: 59, want: 0,
			why: "nothing in a failed claim says whether the generation it stands in registered at all"},
		{token: "legion-LEGION-212-implementer", state: "suspended", generation: 4, contract: 59, want: 0,
			why: "nothing in a suspended claim says whether the generation it stands in registered at all"},
		{token: "legion-LEGION-213-planner", state: "retired", generation: 1, contract: 59, want: 0,
			why: "nothing in a retired claim says whether the generation it stands in registered at all"},
		{token: "legion-LEGION-214-planner", state: "launching", generation: 2, contract: 59, want: 0,
			why: "a launching claim's process has registered nothing yet, whatever an earlier one declared"},
		{token: "legion-LEGION-215-planner", state: "ready", generation: 1, contract: 0, want: 0,
			why: "a claim with no contract recorded has no generation to record for it"},
	}
	for _, row := range rows {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
			plugin_contract)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', $2, 'ses', '', $3, 0, 0, 0, 0, $4)`,
			row.token, row.generation, row.state, row.contract); err != nil {
			t.Fatalf("seed %s as the release this upgrades from left it: %v", row.token, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the contract's generation: %v", err)
	}

	for _, row := range rows {
		var generation, contract int
		if err := store.Pool().QueryRow(ctx,
			`select plugin_contract_generation, plugin_contract from claims where token = $1`, row.token,
		).Scan(&generation, &contract); err != nil {
			t.Fatalf("read the contract generation of %s: %v", row.token, err)
		}
		if generation != row.want {
			t.Errorf("plugin_contract_generation of the %s claim = %d, want %d: %s", row.state, generation, row.want, row.why)
		}
		if contract != row.contract {
			t.Errorf("plugin_contract of the %s claim = %d, want the %d it already held: the stored contract keeps its lifetime",
				row.state, contract, row.contract)
		}
	}

	// What a claim written through the store reads back as, so the column the migration adds is
	// the one putClaim writes and scanClaim reads.
	c := tmuxClaim("legion-LEGION-216-implementer")
	c.Generation, c.PluginContract, c.PluginContractGeneration = 5, 59, 5
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put a claim on the migrated schema: %v", err)
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims back: %v", err)
	}
	var read bool
	for _, got := range claims {
		if got.Token != c.Token {
			continue
		}
		read = true
		if got.PluginContract != 59 || got.PluginContractGeneration != 5 {
			t.Errorf("the claim round-tripped as contract %d of generation %d, want 59 of generation 5",
				got.PluginContract, got.PluginContractGeneration)
		}
	}
	if !read {
		t.Fatalf("the store does not hold the claim %s that was just written", c.Token)
	}
}
