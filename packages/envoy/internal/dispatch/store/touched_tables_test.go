package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// thisChildsMigrations names every migration this change adds, by its .up.sql file in
// migrations/. A migration of this child joins the list in the commit that adds it: the
// measurement driver (packages/dispatch/e2e/acceptance/answer-attribute-removal.sh) reads a
// pg_total_relation_size of every table this test prints from the pull request body, so a
// migration left off the list leaves its tables unmeasured.
var thisChildsMigrations = []string{
	"20261116082319_graph_supertags_decision_reading.up.sql",
	"20261116094058_asks_node.up.sql",
	"20261116094059_asks_node_index.up.sql",
	"20261116094100_ask_followers_node_index.up.sql",
	"20261116094101_graph_edges_asks.up.sql",
	"20261117002006_block_moved_to.up.sql",
	"20261117002045_artifacts_retired_at.up.sql",
	"20261117093337_decisions_documents.up.sql",
	"20261121034242_graph_edges_asks_and_tuple_arm_filter.up.sql",
}

// TestTheTablesThisChildsMigrationsTouch prints, once each, the tables this child's migrations
// lock above ACCESS SHARE as pgmigrate.TouchedTables reads them from each migration's text, one
// `touched: <table>` line each, or `touched: none` when they touch no table, and the
// `census_table_limit: <bytes>` every reading of one is held to (pgmigrate.CensusTableLimit, as
// this branch holds it). It reads no git and no database, so it runs in CI with no tag. A name
// that is no .up.sql file, or that migrations/ does not hold, fails it: a misspelled entry would
// otherwise drop that migration's tables from the list without a word.
func TestTheTablesThisChildsMigrationsTouch(t *testing.T) {
	var tables []string
	for _, name := range thisChildsMigrations {
		if !strings.HasSuffix(name, ".up.sql") {
			t.Fatalf("%s is not a migration's .up.sql file: the list names each migration by the file that applies it", name)
		}
		sql, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("migrations/ holds no %s: %v", name, err)
		}
		tables = append(tables, pgmigrate.TouchedTables(string(sql))...)
	}
	slices.Sort(tables)
	tables = slices.Compact(tables)
	if len(tables) == 0 {
		t.Log("touched: none")
	}
	for _, table := range tables {
		t.Logf("touched: %s", table)
	}
	t.Logf("census_table_limit: %d", pgmigrate.CensusTableLimit)
}
