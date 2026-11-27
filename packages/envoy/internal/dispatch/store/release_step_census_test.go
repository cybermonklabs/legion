package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// baseMigrationsNotOnMain are the migrations of this branch's base, legion/INFRA-1150
// (cybermonklabs/legion#4173 at 6610bca2de), that main does not hold, by file stem. Production
// runs main, so it has applied none of them, and the release that carries this branch meets each
// as pending beside this branch's own, unless the base ships first. Two of them are type
// migrations: 20261125163555 and 20261126040700.
var baseMigrationsNotOnMain = []string{
	"20261117002006_block_moved_to",
	"20261117002045_artifacts_retired_at",
	"20261117093337_decisions_documents",
	"20261125163550_asks_node",
	"20261125163551_asks_node_index",
	"20261125163552_ask_followers_node_index",
	"20261125163553_graph_edges_asks",
	"20261125163554_graph_edges_asks_and_tuple_arm_filter",
	"20261125163555_graph_supertags_decision_reading",
	"20261126040000_refs_decisions_forms_owed",
	"20261126040100_refs_forms_decisions_relay_trigger",
	"20261126040200_block_refs_decisions_forms_insert_trigger",
	"20261126040300_comment_block_refs_decisions_forms_insert_trigger",
	"20261126040400_message_block_refs_decisions_forms_insert_trigger",
	"20261126040500_ask_block_refs_decisions_forms_insert_trigger",
	"20261126040600_refs_decisions_forms_seed",
	"20261126040700_graph_supertags_version_8",
}

// siblingMigrationsNotOnMain are the migrations of cybermonklabs/legion#4193 that this branch
// holds through its merge of that branch, and that neither main nor the base holds, by file stem.
// The release that carries this branch meets each as pending too, unless #4193 ships first. Three
// of them are type migrations: 20261122230406, 20261123174710 and 20261123174720.
var siblingMigrationsNotOnMain = []string{
	"20261116081010_block_pointer_targets_present",
	"20261116172615_graph_nodes_proposed_index",
	"20261117042519_block_moved_to_lens",
	"20261122230406_interaction_source",
	"20261122230407_graph_nodes_interaction_live_index",
	"20261123174710_lens_catalogue",
	"20261123174720_lens_catalogue_further_kinds",
	"20261126050002_graph_edges_asks_and_store_class",
	"20261126050007_graph_edges_store_class_last",
}

// releaseStepTableLimit is the table limit of the census of a release's step: one byte, as
// census_test.go's size tests take it, so graph_nodes and graph_tuples are above it and an
// admission for bounded writes holds at any size they reach.
const releaseStepTableLimit = 1

// A deployment takes one census of every migration its binary carries that the database does not
// record, before any of them applies, and the runner then applies them all in version order
// (cmd/dispatch/README.md, "Database migrations" and "Pre-deploy census"), so each release is one
// step. This test takes the census of the step in which the release that carries this branch
// deploys, over two databases. The first holds every migration of main this tree holds, as
// production does, so the base's migrations main lacks (baseMigrationsNotOnMain), #4193's
// (siblingMigrationsNotOnMain) and this branch's own (issueTreeMigrations) are pending together.
// The second also holds the base's and #4193's, as the upgrade checks' release before does (a
// build of the base and #4193), so this branch's own are pending alone. In each, the census must
// admit every pending migration that writes graph_nodes, each type migration of TypeMigrations
// among them, for bounded writes of graph_nodes and graph_tuples, with no refusal
// (requireTheStepAdmitsEachTypeMigration). A migration pending before a type migration that
// touches either table with more than bounded writes, as a create trigger does, takes that
// admission away (pgmigrate's census, changedBy), so such a migration is dated after every type
// migration its release can carry.
func TestTheReleaseStepOverMainAdmitsEachTypeMigrationForBoundedWrites(t *testing.T) {
	t.Parallel()
	set, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load the migrations: %v", err)
	}
	branch := issueTreeMigrations(set)
	if len(branch) == 0 {
		t.Fatal("no migration is named <timestamp>_issue_tree_<what>.up.sql, so the set read here is the wrong one")
	}
	notOnMain := map[string]bool{}
	for _, stem := range slices.Concat(baseMigrationsNotOnMain, siblingMigrationsNotOnMain) {
		notOnMain[stem] = true
	}
	var overMain []pgmigrate.Migration
	for _, migration := range set {
		stem := strings.TrimSuffix(migration.Name, ".up.sql")
		if notOnMain[stem] || issueTreeMigrationName.MatchString(migration.Name) {
			overMain = append(overMain, migration)
			delete(notOnMain, stem)
		}
	}
	for stem := range notOnMain {
		t.Fatalf("the migrations hold no %s.up.sql, which baseMigrationsNotOnMain or siblingMigrationsNotOnMain names", stem)
	}
	requireTheStepAdmitsEachTypeMigration(t, "over main, with the base's, #4193's and this branch's migrations pending", overMain)
	requireTheStepAdmitsEachTypeMigration(t, "over the base and #4193, with this branch's migrations pending", branch)
}

// requireTheStepAdmitsEachTypeMigration migrates a fresh database through every embedded
// migration but pending, takes the census of the embedded set over it at releaseStepTableLimit,
// and holds it to reading exactly pending as pending, and to admitting each pending migration that
// writes graph_nodes for bounded writes of graph_nodes and graph_tuples, which it reads above the
// limit, with no refusal. A migration of TypeMigrations in which the census reads no write of
// graph_nodes fails too, since nothing judged it. step names the case in each failure.
func requireTheStepAdmitsEachTypeMigration(t *testing.T, step string, pending []pgmigrate.Migration) {
	t.Helper()
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.migrate(ctx, withoutMigrations(t, migrationFiles, pending)); err != nil {
		t.Fatalf("%s: migrate every migration but the pending ones: %v", step, err)
	}
	report, err := census(ctx, database.Pool.Config().ConnString(), migrationFiles, pgmigrate.CensusOptions{TableLimit: releaseStepTableLimit})
	if err != nil {
		t.Fatalf("%s: census: %v", step, err)
	}
	var held, want []string
	for _, entry := range report.Pending {
		held = append(held, entry.Migration.Name)
	}
	for _, migration := range pending {
		want = append(want, migration.Name)
	}
	if !slices.Equal(held, want) {
		t.Fatalf("%s: the census holds %v pending, want %v:\n%s", step, held, want, reportText(report))
	}
	typeMigration := map[string]bool{}
	for _, name := range TypeMigrations {
		typeMigration[name] = true
	}
	var judged []string
	failed := false
	for _, entry := range report.Pending {
		name := entry.Migration.Name
		if !writesGraphNodes(entry) {
			if typeMigration[name] {
				failed = true
				t.Errorf("%s: %s is on TypeMigrations, and the census reads no write of graph_nodes in it, so nothing judged it", step, name)
			}
			continue
		}
		judged = append(judged, name)
		for _, table := range typeTables {
			i := slices.IndexFunc(entry.Tables, func(read pgmigrate.TableCensus) bool { return read.Name == table && read.Through == "" })
			switch {
			case i < 0:
				failed = true
				t.Errorf("%s: %s writes graph_nodes, and the census read no %s of it, which a type migration writes too", step, name, table)
			case !entry.Tables[i].Exists || entry.Tables[i].Bytes <= releaseStepTableLimit:
				t.Fatalf("%s: %s: the census read %s as %+v, not above the %d-byte limit, so nothing was proved of a table above it", step, name, table, entry.Tables[i], releaseStepTableLimit)
			case entry.Tables[i].BoundedWrites == 0:
				failed = true
				t.Errorf("%s: the census does not admit %s for bounded writes of %s above the limit", step, name, table)
			}
		}
		for _, refusal := range entry.Refusals {
			failed = true
			t.Errorf("%s: the census refuses %s: %s", step, name, refusal)
		}
	}
	if len(judged) == 0 {
		t.Fatalf("%s: no pending migration writes graph_nodes, so the census judged no type migration", step)
	}
	t.Logf("%s: the census of %d pending migrations at a %d-byte table limit judged the %d that write graph_nodes: %v", step, len(report.Pending), releaseStepTableLimit, len(judged), judged)
	if failed {
		t.Logf("%s: the census:\n%s", step, reportText(report))
	}
}

// writesGraphNodes says whether a pending migration writes rows of graph_nodes, as a type
// migration does: it names graph_nodes itself, not as a table a foreign key reaches, and the
// census exempts it from the limit neither as catalog-only nor as a concurrent index build.
func writesGraphNodes(entry pgmigrate.MigrationCensus) bool {
	return entry.TableSizeExemption == "" && !entry.ConcurrentIndex &&
		slices.ContainsFunc(entry.Tables, func(table pgmigrate.TableCensus) bool { return table.Name == "graph_nodes" && table.Through == "" })
}
