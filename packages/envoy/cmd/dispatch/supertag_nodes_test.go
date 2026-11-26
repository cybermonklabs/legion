package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// typeMigrations are the type migrations written before the store's backfill, in the order the
// runner applies them: the seed, then each migration that changed a type before it. The test runs
// them a second time, in that order and through the same path the migration runner runs each, over
// the store as they left it (openAtTheBackfill), to prove a second run leaves the same nodes. The
// seed alone would not do: run again, it puts back the version and the attributes it was written
// at, which every later type migration has moved on from. A type migration written after the
// backfill (above backfillVersion) is not listed here and runs once, as the runner applies every
// file: it writes graph_tuples and graph_nodes above the census limit, so it may write only bounded
// writes under the migration census's bounded-write rule (typeMigrationShape), and the seed tuples
// it inserts at source_event 0 fall under no unique key, so a second run would append them again.
// TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs those over a store the
// backfill filled.
var typeMigrations = []string{
	"../../internal/dispatch/store/migrations/20261109051232_graph_supertags.up.sql",
	"../../internal/dispatch/store/migrations/20261113230000_graph_supertags_check_ci_check.up.sql",
	"../../internal/dispatch/store/migrations/20261115130000_graph_supertags_element_versions.up.sql",
}

// backfillVersion is the newest migration of the releases that carry the graph store and its
// backfill (`backfill-graph-store`) and not 20261123080000: each of legion-envoy-v34.11.0 to
// legion-envoy-v34.11.8, every release before legion-envoy-v34.12.0, which carries that migration,
// holds 20261121060636_search_document_text as its newest migration and none after it, so a
// deployment may have run the backfill over a store at this version. Those releases do not hold
// every migration this tree numbers below it (unreleasedVersionsBelowTheBackfill), the backfill's
// four indexes (20261119020700 to 20261119021000) among them, which came with a later release: the
// runner applies every version it has not recorded, so this tree's MigrateThrough builds them here
// too (openAtTheBackfill). Every type migration of typeMigrations is numbered below it; one
// numbered above it runs over graph_nodes and graph_tuples the backfill may have filled.
const backfillVersion = 20261121060636

// unreleasedVersionsBelowTheBackfill is every version this tree numbers below backfillVersion that
// no v34.11.x release holds, read from each release's migrations
// (`git ls-tree --name-only legion-envoy-v34.11.<n> -- packages/envoy/internal/dispatch/store/migrations/`):
// legion-envoy-v34.11.0 to legion-envoy-v34.11.8 hold the same releasedThroughTheBackfill
// migrations, every one numbered at or below backfillVersion and every one a file of this tree. The
// runner applies every version its version table does not record, whatever its number, so a store
// of those releases that ran the backfill meets each version listed here as pending over the
// graph_nodes and graph_tuples the backfill filled, before the type migrations numbered above
// backfillVersion (requireTheUnreleasedBelowTheBackfillAreTheFiles). Among them,
// 20261116015042_graph_nodes_project adds two nullable columns to graph_nodes,
// 20261116015056_graph_nodes_project_index builds an index of it concurrently, and
// 20261116094058_asks_node adds two not valid foreign keys that reference its primary key. The
// census counts none of the three as a change of graph_nodes for the bounded writes of a type
// migration after them. Each version listed here that writes rows of graph_nodes (writesGraphNodes)
// is a type migration such a store meets after its backfill, and
// requireTheCensusAdmitsEachTypeMigrationAboveTheLimit judges it as it judges the type migrations
// numbered above backfillVersion. A version a later release holds is listed here too, as
// 20261110224508_post_corrections is, which legion-envoy-v34.22.0 first holds: a v34.11.x store
// meets it after its backfill all the same.
var unreleasedVersionsBelowTheBackfill = []int{
	20261110224508, 20261116015042, 20261116015056, 20261116015736, 20261116035709, 20261116081157,
	20261116085957, 20261116094058, 20261116094059, 20261116094100, 20261116094101, 20261116232217,
	20261117002006, 20261117002045, 20261117025900, 20261117025901, 20261117025902, 20261117025903,
	20261117025904, 20261117025905, 20261117025906, 20261117025907, 20261117025908, 20261117025909,
	20261117025910, 20261117093337, 20261117100008, 20261117100010, 20261117100015, 20261117100016,
	20261118161237, 20261118181230, 20261119020700, 20261119020800, 20261119020900, 20261119021000,
	20261120020130, 20261120023712, 20261120131444, 20261120150130, 20261120204136, 20261121034242,
	20261121043859,
}

// releasedThroughTheBackfill is how many migrations each v34.11.x release holds, all of them
// numbered at or below backfillVersion.
const releasedThroughTheBackfill = 290

// openAtTheBackfill is a store this tree's runner migrated through backfillVersion and no further:
// the store a deployment of those releases holds before it runs the backfill, and the store the type
// migrations before the backfill left, with the backfill's four indexes beside it. Those are
// indexes alone, so they change no row this test writes or reads, and the backfill this test runs
// is this tree's (graphcensus.Backfill), which refuses to run without them.
func openAtTheBackfill(t *testing.T) *store.Store {
	t.Helper()
	return storetest.OpenFrom(t, fmt.Sprintf("this tree's runner through %d (openAtTheBackfill)", backfillVersion), func(databaseURL string) error {
		ctx := context.Background()
		database, err := store.Open(ctx, databaseURL)
		if err != nil {
			return fmt.Errorf("open the database: %w", err)
		}
		defer database.Pool.Close()
		return database.MigrateThrough(ctx, backfillVersion)
	})
}

// storeMigrationsDir holds the migrations this binary embeds (store's migrationFiles), read from
// disk as typeMigrations are; pgmigratetest.CheckEmbedsEveryFile holds the two to one set.
const storeMigrationsDir = "../../internal/dispatch/store"

// blockSchemaFile is the seed of the typed block types. It is read from disk rather than from
// pmdoc, so what the store holds is compared with the file itself.
const blockSchemaFile = "../../internal/dispatch/pmdoc/schema/blocks.json"

// closedSupertags is the root's closed list of types (R21 of the parent change's specification
// and R6 of this change's): the twenty-seven this change creates. `proposed` is a value of a
// node's state and no type, and the role is the agent, so neither is a supertag. The store holds
// one supertag node for each and no other. Each later change that creates a kind adds it to this
// list in its own pull request: the change that moves lenses and lens kinds adds `lens_kind`, the
// change that moves issues and their documents adds `query` and `query version`, the change that
// moves handoffs adds `handoff`, and the root adds `priority`, `step` and `plan`. The list is
// written here as the test's own copy, so a seed that adds or drops a name has to be read by a
// person before this test agrees with it.
var closedSupertags = []string{
	"issue", "document", "document version", "block", "callout", "ask", "requirement",
	"plan_task", "check", "result", "decision", "reading", "criterion",
	"constraint", "lens", "risk", "comment", "message", "person", "session", "role",
	"page", "interaction", "delivery_attempt", "component", "supertag", "attribute",
}

// TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt proves that a type is data: the seed
// writes the closed list as nodes, a typed block type's attributes come from blocks.json field
// for field, the schema the server serves and validates against is built from those nodes, and
// nothing but a migration writes one. A type that exists only in the file, or only in the
// store, fails here. A pointer is listed by its target's type as that type stands at each read,
// and a pointer whose rows name its node by the targets its attribute node holds at that read.
func TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt(t *testing.T) {
	// Not parallel: it installs pmdoc's process-wide block schema.
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	// The installed schema is process-wide, and this test installs the store's. The hook puts
	// back the schema this binary booted with once the test is over.
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))

	file := readBlockSchemaFile(t)
	drift := &schemaDrift{}

	requireClosedSupertagList(t, ctx, pool)
	requireAttributeNodesMatchTheFile(t, ctx, pool, file, drift)
	requireTheStoreBuildsTheFilesSchema(t, ctx, pool, file, drift)
	drift.report(t)
	requireTypeNodeValuesAreTheirTuples(t, ctx, pool)
	requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes(t, ctx)
	requireNoRouteOrCommandWritesAType(t)
	requireTheServedSchemaFollowsTheNodes(t, ctx, pool)
	requireAPointerReadsItsTargetsTypeAtEachRead(t, ctx, pool)
	requireAnUnknownKindIsSkipped(t, ctx, pool, file)
}

// TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs every type migration
// a deployment of the releases that carry the backfill meets after it, as that deployment meets
// it. The store is at backfillVersion (openAtTheBackfill); it holds an issue and comments written
// as the releases before the graph store wrote them, which name no node, and backfill-graph-store's
// own run (graphcensus.Backfill) states their nodes and tuples, as a deployment runs it once that
// release serves. The store then forgets the versions below backfillVersion that no v34.11.x
// release holds (unreleasedVersionsBelowTheBackfill), which such a deployment meets as pending
// over the store its backfill filled, and the migration census of this binary's pending
// migrations runs with its table limit at one byte (censusLimit), the store census tests' size
// fixture, under which every table that holds a row is above the limit, so an admission here holds
// at any size: it must admit each pending migration that writes graph_nodes, for bounded writes of
// graph_nodes and graph_tuples, with no refusal. The forgotten rows are put back, the runner
// applies the rest, and the store is held to the file as
// TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds a fresh one, and to leaving every
// node and tuple the backfill wrote as it was.
func TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema(t *testing.T) {
	t.Parallel()
	requireTheUnreleasedBelowTheBackfillAreTheFiles(t)
	database := openAtTheBackfill(t)
	ctx := context.Background()
	pool := database.Pool

	writeRowsBeforeTheStore(t, ctx, pool)
	runTheBackfill(t, ctx, database)
	backfilled := backfilledRows(t, ctx, pool)
	held := forgetVersions(t, ctx, pool, unreleasedVersionsBelowTheBackfill)
	requireTheCensusAdmitsEachTypeMigrationAboveTheLimit(t, ctx, pool)
	putVersionsBack(t, ctx, pool, held)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("apply the migrations after %d over the backfilled store: %v", backfillVersion, err)
	}

	file := readBlockSchemaFile(t)
	drift := &schemaDrift{}
	requireClosedSupertagList(t, ctx, pool)
	requireAttributeNodesMatchTheFile(t, ctx, pool, file, drift)
	requireTheStoreBuildsTheFilesSchema(t, ctx, pool, file, drift)
	drift.report(t)
	requireTypeNodeValuesAreTheirTuples(t, ctx, pool)
	if after := backfilledRows(t, ctx, pool); !slices.Equal(backfilled, after) {
		t.Errorf("the migrations after %d moved what the backfill wrote\nbefore: %v\nafter:  %v", backfillVersion, backfilled, after)
	}
}

// backfillComments is how many comments writeRowsBeforeTheStore writes, each a node and its tuples
// once the backfill states them.
const backfillComments = 20

// writeRowsBeforeTheStore writes a project, an issue and backfillComments comments on it as a
// release before the graph store wrote them: rows that name no node, which backfill-graph-store
// states the nodes and tuples of.
func writeRowsBeforeTheStore(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("write the project and the issue: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select 'CORE-1', '{"kind":"user","id":"alice"}', 'A remark written before the store, number ' || n
		from generate_series(1, $1::int) n
	`, backfillComments); err != nil {
		t.Fatalf("write the comments: %v", err)
	}
}

// runTheBackfill runs backfill-graph-store's own run over the store and fails unless it gave a
// node to every comment writeRowsBeforeTheStore wrote (its Rows; its Nodes count the author's node
// too): a backfill that wrote nothing would leave graph_nodes and graph_tuples as the migrations
// alone left them, and this test would prove nothing about a store it filled.
func runTheBackfill(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	var out strings.Builder
	report, err := graphcensus.Backfill(ctx, database, &out)
	t.Logf("backfill-graph-store:\n%s", out.String())
	if err != nil {
		t.Fatalf("backfill-graph-store: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind != "comment" {
			continue
		}
		if kind.Rows != backfillComments || kind.Nodes == 0 || kind.Tuples == 0 || !kind.Complete {
			t.Fatalf("the backfill gave %d comment rows a node (%d nodes, %d tuples), complete %t; want the %d comments written before the store, complete", kind.Rows, kind.Nodes, kind.Tuples, kind.Complete, backfillComments)
		}
		return
	}
	t.Fatalf("the backfill reported no comment kind: %+v", report.Kinds)
}

// backfilledRows is every node that is no type node, and every tuple out of one, as its row
// stands: what the backfill wrote, which a type migration must leave as it is.
func backfilledRows(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select 'node ' || kind || ' ' || id || ' ' || supertag || ' ' || visibility || ' ' || state || ' ' ||
			attributes::text || ' ' || made_at::text || ' ' || made_by::text || ' ' || source_event::text
		from graph_nodes where kind not in ('supertag', 'attribute')
		union all
		select 'tuple ' || seq::text || ' ' || from_kind || ' ' || from_id || ' ' || kind || ' ' || to_kind || ' ' ||
			to_id || ' ' || at::text || ' ' || who::text || ' ' || source_event::text || ' ' || asserted::text
		from graph_tuples where from_kind not in ('supertag', 'attribute')
		order by 1
	`)
	if err != nil {
		t.Fatalf("read what the backfill wrote: %v", err)
	}
	defer rows.Close()
	var written []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read a row the backfill wrote: %v", err)
		}
		written = append(written, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the backfill wrote: %v", err)
	}
	if len(written) == 0 {
		t.Fatal("graph_nodes and graph_tuples hold no row but the type nodes', so the backfill wrote nothing to compare")
	}
	return written
}

// requireTheUnreleasedBelowTheBackfillAreTheFiles holds unreleasedVersionsBelowTheBackfill to the
// migrations this binary embeds, read from disk (storeMigrationsDir): each version listed is a
// migration of this tree numbered below backfillVersion, listed once and in order, and the
// migrations at or below backfillVersion that the versions leave out number
// releasedThroughTheBackfill, the ones every v34.11.x release holds. A migration numbered below
// backfillVersion that no such release holds and the versions leave out fails that count, and the
// census never reads it as pending over the backfilled store.
func requireTheUnreleasedBelowTheBackfillAreTheFiles(t *testing.T) {
	t.Helper()
	migrations, err := pgmigrate.Load(os.DirFS(storeMigrationsDir), "migrations")
	if err != nil {
		t.Fatalf("load the store's migrations: %v", err)
	}
	names := map[int]string{}
	for _, migration := range migrations {
		names[migration.Version] = migration.Name
	}
	unreleased := map[int]bool{}
	for i, version := range unreleasedVersionsBelowTheBackfill {
		if i > 0 && version <= unreleasedVersionsBelowTheBackfill[i-1] {
			t.Errorf("unreleasedVersionsBelowTheBackfill lists %d after %d: list each version once, in ascending order", version, unreleasedVersionsBelowTheBackfill[i-1])
		}
		if version >= backfillVersion {
			t.Errorf("unreleasedVersionsBelowTheBackfill lists %d, which is not below %d", version, backfillVersion)
		}
		if names[version] == "" {
			t.Errorf("unreleasedVersionsBelowTheBackfill lists %d, which is no migration of this tree", version)
		}
		unreleased[version] = true
	}
	released := 0
	for _, migration := range migrations {
		if migration.Version <= backfillVersion && !unreleased[migration.Version] {
			released++
		}
	}
	if released != releasedThroughTheBackfill {
		t.Errorf("this tree holds %d migrations at or below %d beside unreleasedVersionsBelowTheBackfill, and each v34.11.x release holds %d: "+
			"list in unreleasedVersionsBelowTheBackfill each version below %d that `git ls-tree --name-only legion-envoy-v34.11.8 -- packages/envoy/internal/dispatch/store/migrations/` does not name",
			released, backfillVersion, releasedThroughTheBackfill, backfillVersion)
	}
}

// forgetVersions deletes the rows of versions from the store's version table, so the census and the
// runner read those migrations as pending, and answers the rows as they stood, as JSON, for
// putVersionsBack. It fails unless the table recorded every one of them.
func forgetVersions(t *testing.T, ctx context.Context, pool *store.Pool, versions []int) string {
	t.Helper()
	var held string
	var forgotten int
	if err := pool.QueryRow(ctx, `
		with forgotten as (delete from schema_migrations where version = any($1) returning *)
		select coalesce(jsonb_agg(to_jsonb(forgotten)), '[]')::text, count(*) from forgotten
	`, versions).Scan(&held, &forgotten); err != nil {
		t.Fatalf("forget the version rows of %v: %v", versions, err)
	}
	if forgotten != len(versions) {
		t.Fatalf("the version table recorded %d of the %d versions %v", forgotten, len(versions), versions)
	}
	return held
}

// putVersionsBack writes the rows forgetVersions answered back as they stood, so the runner applies
// none of those migrations a second time.
func putVersionsBack(t *testing.T, ctx context.Context, pool *store.Pool, held string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into schema_migrations select * from jsonb_populate_recordset(null::schema_migrations, $1::jsonb)`, held); err != nil {
		t.Fatalf("put the forgotten version rows back: %v", err)
	}
}

// writesGraphNodes says whether a pending migration writes rows of graph_nodes, as a type migration
// does: it names graph_nodes itself, not as a table a foreign key reaches, and the census exempts
// it from the limit neither as catalog-only (TableSizeExemption) nor as a concurrent index build.
// Among the migrations below backfillVersion, one that only adds a column or an index to
// graph_nodes, or a foreign key into it, touches graph_nodes and writes none of its rows.
func writesGraphNodes(entry pgmigrate.MigrationCensus) bool {
	return entry.TableSizeExemption == "" && !entry.ConcurrentIndex &&
		slices.ContainsFunc(entry.Tables, func(table pgmigrate.TableCensus) bool { return table.Name == "graph_nodes" && table.Through == "" })
}

// censusLimit is the table limit of the census this file takes, one byte: the store census tests'
// size fixture, under which every table that holds a row is above the limit.
const censusLimit = 1

// pendingCensus takes the migration census of this binary's migrations over the store, with the
// table limit at censusLimit, and logs its report, naming the store it read as over.
func pendingCensus(t *testing.T, ctx context.Context, pool *store.Pool, over string) *pgmigrate.Report {
	t.Helper()
	migrations, err := pgmigrate.Load(os.DirFS(storeMigrationsDir), "migrations")
	if err != nil {
		t.Fatalf("load the store's migrations: %v", err)
	}
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect for the census: %v", err)
	}
	defer conn.Close(context.Background())
	report, err := pgmigrate.Census(ctx, conn, migrations, "schema_migrations", pgmigrate.CensusOptions{TableLimit: censusLimit})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	var out strings.Builder
	report.Write(&out)
	t.Logf("census at a %d-byte table limit over %s:\n%s", censusLimit, over, out.String())
	return report
}

// requireTheCensusAdmitsEachTypeMigrationAboveTheLimit takes the migration census of this binary's
// migrations over the backfilled store (pendingCensus), and holds each type migration pending there
// to being admitted for bounded writes of graph_nodes and of graph_tuples, which the census read
// above the limit, with no refusal at all. The type migrations are each pending migration that
// writes rows of graph_nodes (writesGraphNodes), whatever its number: those numbered above
// backfillVersion, written after the backfill, and those of unreleasedVersionsBelowTheBackfill,
// which a store of the v34.11.x releases meets after its backfill too. Any other pending migration
// is not judged here: its refusal at one byte says nothing about a type migration. A census that
// judged no type migration fails, since it proved nothing.
func requireTheCensusAdmitsEachTypeMigrationAboveTheLimit(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	judged := 0
	for _, entry := range pendingCensus(t, ctx, pool, "the backfilled store").Pending {
		admitted := map[string]bool{}
		for _, table := range entry.Tables {
			if table.Name != "graph_nodes" && table.Name != "graph_tuples" {
				continue
			}
			if !table.Exists || table.Bytes <= censusLimit {
				t.Fatalf("%s: the census read %s as %+v, not above the %d-byte limit, so nothing was proved about a table above it", entry.Migration.Name, table.Name, table, censusLimit)
			}
			admitted[table.Name] = table.BoundedWrites > 0
		}
		name := entry.Migration.Name
		if !writesGraphNodes(entry) {
			continue
		}
		judged++
		for _, table := range []string{"graph_nodes", "graph_tuples"} {
			if !admitted[table] {
				t.Errorf("%s is not admitted for bounded writes of %s above the limit; a type migration pending over a backfilled store writes %s", name, table, typeMigrationShape)
			}
		}
		for _, refusal := range entry.Refusals {
			t.Errorf("the census refuses %s over a backfilled store: %s", name, refusal)
		}
	}
	if judged == 0 {
		t.Fatalf("no pending migration writes graph_nodes, so the census judged no type migration over the backfilled store")
	}
}

// requireClosedSupertagList holds the store to one supertag node per name of the closed list and
// no other, each with the base attributes every node of this store carries, as node columns and
// as tuples.
func requireClosedSupertagList(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	nodes, err := graphstore.NodesOfKind(ctx, pool, schemanodes.KindSupertag)
	if err != nil {
		t.Fatalf("read the supertag nodes: %v", err)
	}
	var names []string
	for _, node := range nodes {
		names = append(names, node.ID)
		if node.Supertag != schemanodes.KindSupertag {
			t.Errorf("supertag node %q carries supertag %q, want %q", node.ID, node.Supertag, schemanodes.KindSupertag)
		}
		requireBaseAttributes(t, ctx, pool, node)
	}
	want := slices.Clone(closedSupertags)
	slices.Sort(want)
	got := slices.Clone(names)
	slices.Sort(got)
	for _, name := range missing(got, want) {
		t.Errorf("the store holds supertag node %q, which the closed list does not name", name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the store holds supertag nodes %v, want the closed list %v\nonly in the store: %v\nonly in the list: %v",
			got, want, missing(got, want), missing(want, got))
	}
}

// requireBaseAttributes holds one node to carrying visibility and state as columns and as the
// tuples the node row projects.
func requireBaseAttributes(t *testing.T, ctx context.Context, pool *store.Pool, node graphstore.Node) {
	t.Helper()
	if node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive {
		t.Errorf("node %s/%s is %s/%s, want %s/%s",
			node.Kind, node.ID, node.Visibility, node.State, graphstore.VisibilityProject, graphstore.StateLive)
	}
	if node.SourceEvent != graphstore.NoEvent {
		t.Errorf("node %s/%s records event %d, want the sentinel %d: the seed records no event",
			node.Kind, node.ID, node.SourceEvent, graphstore.NoEvent)
	}
	for attribute, want := range map[string]string{"visibility": node.Visibility, "state": node.State} {
		value, held, err := graphstore.CurrentValue(ctx, pool, node.Kind, node.ID, attribute)
		if err != nil {
			t.Fatalf("read %s of node %s/%s: %v", attribute, node.Kind, node.ID, err)
		}
		if !held || value != want {
			t.Errorf("node %s/%s carries %s as the tuple %q (held %t), want %q",
				node.Kind, node.ID, attribute, value, held, want)
		}
	}
}

// requireTypeNodeValuesAreTheirTuples holds every type node's row to the log it projects (R1):
// each attribute the row's attributes column carries is the newest value tuple of that name out
// of the node, and every value tuple out of the node other than its two base attributes names an
// attribute the row carries. A value a migration set on the row alone, which a reader of the log
// could not read back, fails here, and so does a tuple the row has no attribute for.
//
// A value is compared as the migrations write it: a JSON string as the string itself, and any
// other JSON value as its text, so a version reads `4` and a list of choices `["note", "warning"]`.
func requireTypeNodeValuesAreTheirTuples(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select n.kind, n.id, n.attributes::text,
		       coalesce((
		           select jsonb_object_agg(newest.kind, newest.to_id)
		           from (
		               select distinct on (t.kind) t.kind, t.to_id
		               from graph_tuples t
		               where t.from_kind = n.kind and t.from_id = n.id and t.to_kind = $3 and t.asserted
		               order by t.kind, t.seq desc
		           ) newest
		       ), '{}')::text
		from graph_nodes n where n.kind in ($1, $2)
		order by n.kind, n.id
	`, schemanodes.KindSupertag, schemanodes.KindAttribute, graphstore.KindValue)
	if err != nil {
		t.Fatalf("read the type nodes and their value tuples: %v", err)
	}
	defer rows.Close()
	compared := 0
	for rows.Next() {
		var kind, id, attributesText, tuplesText string
		if err := rows.Scan(&kind, &id, &attributesText, &tuplesText); err != nil {
			t.Fatalf("read a type node and its value tuples: %v", err)
		}
		var attributes map[string]json.RawMessage
		if err := json.Unmarshal([]byte(attributesText), &attributes); err != nil {
			t.Fatalf("read the attributes of %s/%s: %v", kind, id, err)
		}
		var tuples map[string]string
		if err := json.Unmarshal([]byte(tuplesText), &tuples); err != nil {
			t.Fatalf("read the value tuples of %s/%s: %v", kind, id, err)
		}
		for name := range tuples {
			if graphstore.IsBaseAttribute(name) {
				delete(tuples, name)
			}
		}
		for name, raw := range attributes {
			want := string(raw)
			var text string
			if json.Unmarshal(raw, &text) == nil {
				want = text
			}
			held, stated := tuples[name]
			switch {
			case !stated:
				t.Errorf("type node %s/%s carries %s = %s on its row and no value tuple states it: a type migration appends a value tuple for each value it sets, and writes %s",
					kind, id, name, want, typeMigrationShape)
			case held != want:
				t.Errorf("type node %s/%s carries %s = %s on its row, and its newest value tuple states %s: a type migration appends a value tuple for each value it moves, and writes %s",
					kind, id, name, want, held, typeMigrationShape)
			}
			compared++
		}
		for name, held := range tuples {
			if _, carried := attributes[name]; !carried {
				t.Errorf("type node %s/%s states %s = %s as its newest value tuple, which its row carries no attribute for", kind, id, name, held)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the type nodes and their value tuples: %v", err)
	}
	if compared == 0 {
		t.Fatalf("no type node carries an attribute value, so the comparison of rows with their tuples proved nothing")
	}
	t.Logf("every type node's %d attribute value(s) are the newest value tuples out of it", compared)
}

// requireAttributeNodesMatchTheFile holds every typed block type's attribute nodes to the file,
// field by field and in both directions: an attribute only in the file, or only in the store,
// fails, and so does one whose kind, choices, default, server flag, markdown flag, targets, scope
// or element versions differ.
// The file names an attribute as a document writes it, and its node is the one
// schemanodes.StoreName names, `<type>/<store name>`, so ask's `state` is the node
// ask/answer_state. Each type and attribute it fails on is recorded in drift.
func requireAttributeNodesMatchTheFile(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema, drift *schemaDrift) {
	t.Helper()
	nodes, err := graphstore.NodesOfKind(ctx, pool, schemanodes.KindAttribute)
	if err != nil {
		t.Fatalf("read the attribute nodes: %v", err)
	}
	held := map[string]graphstore.Node{}
	for _, node := range nodes {
		held[node.ID] = node
		requireBaseAttributes(t, ctx, pool, node)
		if node.Supertag != schemanodes.KindAttribute {
			t.Errorf("attribute node %q carries supertag %q, want %q", node.ID, node.Supertag, schemanodes.KindAttribute)
		}
	}
	wanted := map[string]struct{}{}
	for _, typ := range file.Types {
		tied := map[string]struct{}{}
		tuples, err := graphstore.CurrentIn(ctx, pool, schemanodes.KindSupertag, typ.Name, []string{schemanodes.TupleAttributeOf})
		if err != nil {
			t.Fatalf("read the attributes tied to %q: %v", typ.Name, err)
		}
		for _, tuple := range tuples {
			tied[tuple.FromID] = struct{}{}
		}
		for position, declared := range typ.Attributes {
			id := typ.Name + "/" + schemanodes.StoreName(typ.Name, declared.Name)
			wanted[id] = struct{}{}
			if _, tiedToType := tied[id]; !tiedToType {
				t.Errorf("no %s tuple ties attribute node %q to supertag %q", schemanodes.TupleAttributeOf, id, typ.Name)
				drift.add(id)
			}
			node, inStore := held[id]
			if !inStore {
				t.Errorf("%s declares attribute %q of %q, which the store holds no node for", blockSchemaFile, declared.Name, typ.Name)
				drift.add(id)
				continue
			}
			requireAttributeNodeCarries(t, node, typ.Name, position, declared, drift)
		}
		if len(tied) != len(typ.Attributes) {
			t.Errorf("supertag %q is tied to %d attribute nodes, want the %d its type declares", typ.Name, len(tied), len(typ.Attributes))
			drift.add(typ.Name)
		}
	}
	for id := range held {
		if _, declared := wanted[id]; !declared {
			t.Errorf("the store holds attribute node %q, which %s declares nowhere", id, blockSchemaFile)
			drift.add(id)
		}
	}
}

// requireAttributeNodeCarries holds one attribute node of type typeName to what the file says
// about that attribute: its name and place, the store name schemanodes.StoreName gives it, and
// each field a BlockAttributeSchema carries. A node that differs is recorded in drift.
func requireAttributeNodeCarries(t *testing.T, node graphstore.Node, typeName string, position int, declared pmdoc.BlockAttribute, drift *schemaDrift) {
	t.Helper()
	carried := node.Attributes
	if name, _ := carried["name"].(string); name != declared.Name {
		t.Errorf("attribute node %q carries name %q, want %q", node.ID, name, declared.Name)
		drift.add(node.ID)
	}
	if held, want := carried["store_name"], schemanodes.StoreName(typeName, declared.Name); held != want {
		t.Errorf("attribute node %q carries store name %v, want %q", node.ID, held, want)
		drift.add(node.ID)
	}
	if at, ok := carried["position"].(float64); !ok || int(at) != position {
		t.Errorf("attribute node %q carries position %v, want %d", node.ID, carried["position"], position)
		drift.add(node.ID)
	}
	// The fields of BlockAttributeSchema, compared as the file writes them: the node's
	// attributes are the attribute's own JSON with the name, the store name and the position
	// beside it, so one encoding of each side names every difference at once.
	stored := map[string]any{}
	for key, value := range carried {
		if key == "name" || key == "store_name" || key == "position" {
			continue
		}
		stored[key] = value
	}
	wanted := map[string]any{}
	if err := json.Unmarshal(mustMarshal(t, declared.BlockAttributeSchema), &wanted); err != nil {
		t.Fatalf("read the file's attribute %q: %v", node.ID, err)
	}
	if string(mustMarshal(t, stored)) != string(mustMarshal(t, wanted)) {
		t.Errorf("attribute node %q carries %s, want the file's %s", node.ID, mustMarshal(t, stored), mustMarshal(t, wanted))
		drift.add(node.ID)
	}
}

// requireTheStoreBuildsTheFilesSchema reads the schema back out of the nodes and holds it to the
// file, type for type and attribute for attribute, recording in drift what the two differ on. The
// file's nodes and marks are left out of the comparison: they are the Proof schema's, which the
// store holds no node for, and the server installs the build's own beside the store's types
// (pmdoc.Install).
func requireTheStoreBuildsTheFilesSchema(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema, drift *schemaDrift) {
	t.Helper()
	built, err := schemanodes.Read(ctx, pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes: %v", err)
	}
	file.Nodes, file.Marks = nil, nil
	if string(mustMarshal(t, built)) != string(mustMarshal(t, file)) {
		t.Errorf("the schema the store's nodes build is\n%s\nwant the file's\n%s", mustMarshal(t, built), mustMarshal(t, file))
		drift.compare(built, file)
	}
}

// typeMigrationShape is the shape a failing run names for the type migration a change to
// blocks.json needs. The type migrations written before the store's backfill (typeMigrations)
// write `with … insert … select … where not exists`, and two of them `on conflict … do update`,
// which the migration census refuses once graph_tuples and graph_nodes are above its limit, so
// none of them is the shape. A later one writes under the census's bounded-write rule
// (pgmigrate.BoundedWriteRows): the node of each type or attribute it adds and its tuples as
// literal rows, and each node it moves, every block type's supertag node to the new version among
// them, by an update of the node's primary key for each value it sets, as
// 20261123080000_graph_supertags_plan_task_steps does, the first of them;
// TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema holds each to it.
var typeMigrationShape = fmt.Sprintf("literal rows alone - `insert into graph_nodes (…) values (…)` and `insert into graph_tuples (…) values (…)`, "+
	"at most %s rows a statement, each with no `on conflict` or with `on conflict … do nothing` - "+
	"and, for a node it moves, `update graph_nodes set <column> = <literal> where (kind, id) in ((…))`, one statement for each value it sets",
	pgmigrate.Thousands(pgmigrate.BoundedWriteRows))

// schemaDrift is every type and attribute on which blocks.json and the store's type nodes
// disagree, and the two schema versions where those differ, gathered while the checks above
// report each field. A failing run ends on one instruction naming them all (report), so whoever
// changed the file reads once what the change still needs rather than once for every field.
type schemaDrift struct {
	// names are the types, each by its name, and the attributes, each by its node's id,
	// `<type>/<store name>`.
	names                     []string
	fileVersion, storeVersion int
}

// add records one type or attribute, once.
func (d *schemaDrift) add(name string) {
	if !slices.Contains(d.names, name) {
		d.names = append(d.names, name)
	}
}

// compare records what the schema the store's nodes build and the file's differ on beyond their
// attributes: the version, and each type only one of them declares, or whose content rule,
// renderer, parents or place among the types differs. The attributes are the attribute nodes'
// own, which requireAttributeNodesMatchTheFile has recorded field by field.
func (d *schemaDrift) compare(built, file pmdoc.BlockSchema) {
	if built.Version != file.Version {
		d.fileVersion, d.storeVersion = file.Version, built.Version
	}
	builtOrder, fileOrder := commonTypes(built, file), commonTypes(file, built)
	for index, name := range fileOrder {
		if builtOrder[index] != name {
			d.add(name)
		}
	}
	for _, want := range file.Types {
		index := slices.IndexFunc(built.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == want.Name })
		if index < 0 || built.Types[index].Content != want.Content || built.Types[index].Render != want.Render ||
			!slices.Equal(built.Types[index].Parents, want.Parents) {
			d.add(want.Name)
		}
	}
	for _, typ := range built.Types {
		if !slices.ContainsFunc(file.Types, func(want pmdoc.BlockTypeSchema) bool { return want.Name == typ.Name }) {
			d.add(typ.Name)
		}
	}
}

// commonTypes is the names of the types of of that in declares too, in of's order, so the order
// of two schemas is compared over the types both hold and a type only one holds moves no other.
func commonTypes(of, in pmdoc.BlockSchema) []string {
	var names []string
	for _, typ := range of.Types {
		if slices.ContainsFunc(in.Types, func(other pmdoc.BlockTypeSchema) bool { return other.Name == typ.Name }) {
			names = append(names, typ.Name)
		}
	}
	return names
}

// report fails the run once with what a change to blocks.json needs: the types and attributes
// the file and the store's nodes disagree on, the two versions where they differ, and the
// migration that brings the store to the file. It says nothing where the two agree.
func (d *schemaDrift) report(t *testing.T) {
	t.Helper()
	if len(d.names) == 0 && d.fileVersion == d.storeVersion {
		return
	}
	what := "the block schema"
	if len(d.names) > 0 {
		what = strings.Join(slices.Sorted(slices.Values(d.names)), ", ")
	}
	versions := ""
	if d.fileVersion != d.storeVersion {
		versions = fmt.Sprintf(", and the file is at version %d where the store's nodes build version %d", d.fileVersion, d.storeVersion)
	}
	t.Errorf("pmdoc/schema/blocks.json and the store's type nodes disagree on %s%s. "+
		"A change to pmdoc/schema/blocks.json needs a store migration of its own: "+
		"it writes the node of each type or attribute the change adds or alters, with its tuples, and moves every block type's supertag node to the new version. "+
		"Written after the store's backfill, it writes under the migration census's bounded-write rule: %s. "+
		"Write one for this change. It runs once, as the runner applies every file, and joins no second run in this file (typeMigrations).",
		what, versions, typeMigrationShape)
}

// requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes runs every type migration written before
// the store's backfill again, in order, over the store as they left it (openAtTheBackfill), and
// holds every node row and every tuple to being the one the first run left, and each type node's
// values to its tuples. A type migration written after the backfill runs once: it writes only
// bounded writes under the migration census's bounded-write rule (typeMigrationShape), and the
// seed tuples it inserts at source_event 0 fall under no unique key, so it cannot run twice
// unchanged, and the runner applies each file once anyway. Only typeMigrations runs here, and a
// later change adds no migration to it. The store is not the fully migrated one: over a store a
// later type migration moved, the earlier ones run again would put back the version they set.
func requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes(t *testing.T, ctx context.Context) {
	t.Helper()
	pool := openAtTheBackfill(t).Pool
	before, beforeTuples := schemaRows(t, ctx, pool)
	for _, migration := range typeMigrations {
		text, err := os.ReadFile(filepath.Clean(migration))
		if err != nil {
			t.Fatalf("read the type migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(text)); err != nil {
			t.Fatalf("run the type migration %s a second time: %v", filepath.Base(migration), err)
		}
	}
	after, afterTuples := schemaRows(t, ctx, pool)
	if !slices.Equal(before, after) {
		t.Errorf("a second run of the type migrations left different nodes\nbefore: %v\nafter:  %v", before, after)
	}
	if beforeTuples != afterTuples {
		t.Errorf("a second run of the type migrations left %d tuples, want the %d the first run wrote", afterTuples, beforeTuples)
	}
	requireTypeNodeValuesAreTheirTuples(t, ctx, pool)
}

// requireNoRouteOrCommandWritesAType reads the route table and the subcommands and fails on one
// that could create, retitle or delete a type node. Authoring a supertag is offered to nobody:
// a type changes by a migration alone.
func requireNoRouteOrCommandWritesAType(t *testing.T) {
	t.Helper()
	var index bytes.Buffer
	if err := api.WriteRouteIndex(&index); err != nil {
		t.Fatalf("read the route table: %v", err)
	}
	var table struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Auth   string `json:"auth"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(index.Bytes(), &table); err != nil {
		t.Fatalf("read the route index: %v", err)
	}
	schemaRoutes := 0
	for _, route := range table.Routes {
		names := namesAType(route.Path)
		if names && route.Method != "GET" {
			t.Errorf("%s %s addresses a type node and writes; no route creates, retitles or deletes one", route.Method, route.Path)
		}
		if route.Path == "/api/v1/schema/blocks" {
			schemaRoutes++
			if route.Method != "GET" || route.Auth != "public" {
				t.Errorf("the block schema route is %s %s (%s), want a public GET", route.Method, route.Path, route.Auth)
			}
		}
	}
	if schemaRoutes != 1 {
		t.Errorf("the route table holds %d rows for /api/v1/schema/blocks, want exactly the one that serves it", schemaRoutes)
	}
	for _, command := range subcommands {
		if namesAType(command.name) {
			t.Errorf("subcommand %q addresses a type node; a type changes by a migration alone", command.name)
		}
	}
}

// namesAType reports whether a route's path or a subcommand's name addresses a type node.
func namesAType(name string) bool {
	for _, word := range []string{"supertag", "schema/blocks", "block-schema", "blockschema", "/types", "attribute-of"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// requireTheServedSchemaFollowsTheNodes changes a type in the database alone - a field of the
// attribute node callout/kind, then a field of the supertag node callout itself - starts the
// server's install again after each (InstallFromStore, the call the server's boot makes), and
// holds both the schema the server serves (pmdoc.SchemaJSON, the bytes GET
// /api/v1/schema/blocks writes) and the refusal a write earns to following the node.
func requireTheServedSchemaFollowsTheNodes(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	const danger = ":::callout{#c1 kind=\"danger\" title=\"T\"}\nOnly.\n:::\n"
	if _, err := pmdoc.ParseForWrite(danger, nil); err == nil {
		t.Fatalf("a callout of kind danger is accepted before the store declares that choice")
	}
	if _, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{choices}', '["note", "warning", "danger"]'::jsonb)
		where kind = $1 and id = 'callout/kind'
	`, schemanodes.KindAttribute); err != nil {
		t.Fatalf("add a choice to the callout kind attribute node: %v", err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the changed node builds: %v", err)
	}
	served := decodeSchema(t, pmdoc.SchemaJSON())
	choices := attributeChoices(t, served, "callout", "kind")
	if !slices.Contains(choices, "danger") {
		t.Errorf("the schema the server serves offers callout kinds %v, want the node's own, holding danger", choices)
	}
	if _, err := pmdoc.ParseForWrite(danger, nil); err != nil {
		t.Errorf("a callout of kind danger is refused after the store declares that choice: %v", err)
	}

	// The supertag node's own content rule. The file gives callout block+, so a callout that
	// holds a list is written; the store alone moves it to paragraph+, after which the served
	// schema says paragraph+ and the same callout is refused.
	const listed = ":::callout{#c2 kind=\"note\" title=\"T\"}\n- one\n- two\n:::\n"
	if _, err := pmdoc.ParseForWrite(listed, nil); err != nil {
		t.Fatalf("a callout holding a list is refused before the store changes the callout's content rule: %v", err)
	}
	if content := typeContent(t, decodeSchema(t, pmdoc.SchemaJSON()), "callout"); content != pmdoc.BlockContentBlocks {
		t.Fatalf("the served schema gives callout the content %q before the store changes it, want the file's %q", content, pmdoc.BlockContentBlocks)
	}
	changed, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{content}', to_jsonb($2::text))
		where kind = $1 and id = 'callout'
	`, schemanodes.KindSupertag, string(pmdoc.BlockContentParagraphs))
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("change the content rule of the supertag node callout: %d row(s), %v", changed.RowsAffected(), err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the changed supertag node builds: %v", err)
	}
	if content := typeContent(t, decodeSchema(t, pmdoc.SchemaJSON()), "callout"); content != pmdoc.BlockContentParagraphs {
		t.Errorf("the schema the server serves gives callout the content %q, want the supertag node's own %q", content, pmdoc.BlockContentParagraphs)
	}
	if _, err := pmdoc.ParseForWrite(listed, nil); err == nil {
		t.Errorf("a callout holding a list is accepted after the supertag node's content rule became %q", pmdoc.BlockContentParagraphs)
	}
}

// requireAPointerReadsItsTargetsTypeAtEachRead seeds two block pointers, each a plan_task's serves
// at one requirement and each carrying in to_types the targets the attribute node plan_task/serves
// holds, as the index copies them. The first is written as this build's block index writes it:
// its blocks are nodes of the store, the serves relation is a tuple, and its rows name their
// nodes. The second is written as a build before the store wrote it, its rows naming no node.
// Each is read through the three readers that list or count it - the edges in at its target
// (graph_edges), the edges out of its document (refs.Edges' own block_refs arm) and the target's
// backlink count - with no document indexed again and the pointers' rows as they were.
//
// It retypes the target in document_blocks alone: block_ref_listed reads the target's type at
// each read, so both links are hidden while the target is a callout and listed once it is a
// requirement again. It then changes the targets of the attribute node, as the test's other
// changes write a type node: every reader of a row that names its node reads those targets at
// read time (block_pointer_targets), so the first link is hidden while the targets leave out
// requirement and listed once they hold it again. The second reads the to_types its row carries,
// which only an index of its document changes, so it stays listed throughout.
func requireAPointerReadsItsTargetsTypeAtEachRead(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	var targets []string
	if err := pool.QueryRow(ctx, `
		select coalesce(array(select jsonb_array_elements_text(attributes->'targets')), '{}')
		from graph_nodes where kind = $1 and id = 'plan_task/serves'
	`, schemanodes.KindAttribute).Scan(&targets); err != nil {
		t.Fatalf("read the targets of the attribute node plan_task/serves: %v", err)
	}
	if !slices.Contains(targets, "requirement") || slices.Contains(targets, "callout") {
		t.Fatalf("plan_task/serves points at %v, want requirement and not callout, so that the retype below moves the link", targets)
	}
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('STP', 'Supertag pointers', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('STP-1', 'STP', 1, 'Pointers', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed the pointers' project and issue: %v", err)
	}
	indexed := seedPointerDocument(t, ctx, pool, "pointers")
	older := seedPointerDocument(t, ctx, pool, "older-pointers")
	requirement := indexed.refKey + "#r1"
	stamped := blockPointer{document: indexed.id, task: indexed.refKey + "#t1"}
	unstamped := blockPointer{document: older.id, task: older.refKey + "#t2"}
	writeStampedPointer(t, ctx, pool, indexed, targets)
	if _, err := pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 't2', $2, 'plan_task', md5('t2'), 'The task an older build indexed.', 1, now());
	`, older.id, unstamped.task); err != nil {
		t.Fatalf("seed the older pointer's block: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, to_types)
		values ($1::uuid, 't2', $2, 'block', $3, 'serves', 1, $4::text[])
	`, older.id, unstamped.task, requirement, targets); err != nil {
		t.Fatalf("seed the older pointer's row: %v", err)
	}
	rows := append(pointerRows(t, ctx, pool, stamped), pointerRows(t, ctx, pool, unstamped)...)

	both := func(listed bool) map[blockPointer]bool {
		return map[blockPointer]bool{stamped: listed, unstamped: listed}
	}
	requirePointersListed(t, ctx, pool, "the target a requirement", requirement, both(true))
	retypeBlock(t, ctx, pool, requirement, "callout")
	requirePointersListed(t, ctx, pool, "the target retyped to a callout", requirement, both(false))
	retypeBlock(t, ctx, pool, requirement, "requirement")
	requirePointersListed(t, ctx, pool, "the target a requirement again", requirement, both(true))

	setPointerTargets(t, ctx, pool, []string{"callout"})
	requirePointersListed(t, ctx, pool, "plan_task/serves pointing at callouts alone", requirement,
		map[blockPointer]bool{stamped: false, unstamped: true})
	setPointerTargets(t, ctx, pool, targets)
	requirePointersListed(t, ctx, pool, "plan_task/serves pointing at its own targets again", requirement, both(true))

	if after := append(pointerRows(t, ctx, pool, stamped), pointerRows(t, ctx, pool, unstamped)...); !slices.Equal(after, rows) {
		t.Errorf("the retype or the change of targets rewrote the pointers' rows, which neither may touch:\nbefore %v\nafter  %v", rows, after)
	}
}

// pointerDocument is one document of the pointer fixture: its uuid and its ref_key.
type pointerDocument struct{ id, refKey string }

// blockPointer is one pointer of the fixture: the document it is written in and its citing block.
type blockPointer struct{ document, task string }

func seedPointerDocument(t *testing.T, ctx context.Context, pool *store.Pool, slug string) pointerDocument {
	t.Helper()
	var document pointerDocument
	if err := pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('STP-1', 'STP', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug).Scan(&document.id, &document.refKey); err != nil {
		t.Fatalf("seed the pointer document %s: %v", slug, err)
	}
	return document
}

// writeStampedPointer writes the requirement r1 and the task t1 of document, and t1's serves at
// r1, as this build's block index writes a version: the two blocks' nodes, their in_document
// tuples and the serves tuple through the store's writer, and then the rows, each naming its node.
func writeStampedPointer(t *testing.T, ctx context.Context, pool *store.Pool, document pointerDocument, targets []string) {
	t.Helper()
	writer := model.Actor{Kind: "system", ID: "supertag-test"}
	at := time.Now().UTC()
	requirement, task := document.refKey+"#r1", document.refKey+"#t1"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the indexed pointer's write: %v", err)
	}
	defer tx.Rollback(ctx)
	var nodes []graphstore.Node
	tuples := []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: task, Kind: "serves", ToKind: refs.BlockNodeKind, ToID: requirement,
		At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}
	for _, block := range []string{requirement, task} {
		nodes = append(nodes, graphstore.Node{
			Kind: refs.BlockNodeKind, ID: block, Supertag: refs.BlockNodeKind,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: at, MadeBy: writer, SourceEvent: graphstore.NoEvent,
		})
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: block, Kind: refs.TupleInDocument, ToKind: "artifact", ToID: document.refKey,
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write the indexed pointer's nodes and tuples: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'r1', $2, $4, $2, 'requirement', md5('r1'), 'The requirement the tasks serve.', 1, now()),
		       ($1::uuid, 't1', $3, $4, $3, 'plan_task', md5('t1'), 'The task that serves it.', 1, now())
	`, document.id, requirement, task, refs.BlockNodeKind); err != nil {
		t.Fatalf("seed the indexed pointer's two blocks: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, node_kind, node_address, to_kind, to_id, kind, created_version, to_types)
		values ($1::uuid, 't1', $2, $4, $2, 'block', $3, 'serves', 1, $5::text[])
	`, document.id, task, requirement, refs.BlockNodeKind, targets); err != nil {
		t.Fatalf("seed the indexed pointer's row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the indexed pointer: %v", err)
	}
}

// setPointerTargets writes the targets of the attribute node plan_task/serves in the database
// alone, as the type changes above write a type node: no document is indexed again.
func setPointerTargets(t *testing.T, ctx context.Context, pool *store.Pool, targets []string) {
	t.Helper()
	changed, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{targets}', to_jsonb($2::text[]))
		where kind = $1 and id = 'plan_task/serves'
	`, schemanodes.KindAttribute, targets)
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("point plan_task/serves at %v: %d row(s), %v", targets, changed.RowsAffected(), err)
	}
}

// requirePointersListed holds the three readers of each pointer to listing it, or to hiding it,
// as listed says: the serves edge in at the requirement from its task, the serves edge out of its
// document at the requirement, and the requirement's backlink count, which counts the listed ones.
func requirePointersListed(t *testing.T, ctx context.Context, pool *store.Pool, stage, requirement string, listed map[blockPointer]bool) {
	t.Helper()
	in, err := refs.Edges(ctx, pool, model.Actor{}, refs.Query{Direction: "in", Kind: "block", ID: requirement})
	if err != nil {
		t.Fatalf("%s: read the edges in at %s: %v", stage, requirement, err)
	}
	counts, err := refs.BacklinkCounts(ctx, pool, model.Actor{}, "block", []string{requirement}, nil)
	if err != nil {
		t.Fatalf("%s: count the backlinks of %s: %v", stage, requirement, err)
	}
	shown := 0
	for pointer, isListed := range listed {
		want := 0
		if isListed {
			want = 1
			shown++
		}
		out, err := refs.Edges(ctx, pool, model.Actor{}, refs.Query{Direction: "out", Kind: "artifact", ID: pointer.document})
		if err != nil {
			t.Fatalf("%s: read the edges out of %s: %v", stage, pointer.document, err)
		}
		if got := servesEdges(in, pointer.task); got != want {
			t.Errorf("%s: the edges in at %s list %d serves edge(s) from %s, want %d (%+v)", stage, requirement, got, pointer.task, want, in)
		}
		if got := servesEdges(out, requirement); got != want {
			t.Errorf("%s: the edges out of the document of %s list %d serves edge(s) at %s, want %d (%+v)", stage, pointer.task, got, requirement, want, out)
		}
	}
	if got := counts[requirement]; got != shown {
		t.Errorf("%s: %s counts %d backlink(s), want %d", stage, requirement, got, shown)
	}
}

// servesEdges counts the serves edges of a read whose far end is node.
func servesEdges(edges []model.GraphEdge, node string) int {
	count := 0
	for _, edge := range edges {
		if edge.Kind == "serves" && edge.Node.ID == node {
			count++
		}
	}
	return count
}

// retypeBlock changes the type of one current block row and nothing else, which is how the
// pointer's target changes type here: no version is written and no document is indexed.
func retypeBlock(t *testing.T, ctx context.Context, pool *store.Pool, node, typ string) {
	t.Helper()
	changed, err := pool.Exec(ctx, `update document_blocks set type = $2 where node_id = $1`, node, typ)
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("retype %s to %s: %d row(s), %v", node, typ, changed.RowsAffected(), err)
	}
}

// pointerRows is the block_refs row of one pointer and its citing block's row, each as text.
func pointerRows(t *testing.T, ctx context.Context, pool *store.Pool, pointer blockPointer) []string {
	t.Helper()
	read, err := pool.Query(ctx, `
		select r::text from block_refs r where r.artifact_id = $1::uuid and r.from_node = $2
		union all
		select d::text from document_blocks d where d.artifact_id = $1::uuid and d.node_id = $2
		order by 1
	`, pointer.document, pointer.task)
	if err != nil {
		t.Fatalf("read the pointer's rows: %v", err)
	}
	defer read.Close()
	var rows []string
	for read.Next() {
		var row string
		if err := read.Scan(&row); err != nil {
			t.Fatalf("read a pointer row: %v", err)
		}
		rows = append(rows, row)
	}
	if err := read.Err(); err != nil {
		t.Fatalf("read the pointer's rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%s holds %d pointer and citing row(s), want its one pointer and its one plan_task", pointer.task, len(rows))
	}
	return rows
}

// requireAnUnknownKindIsSkipped adds a supertag the block schema never declared and a node of a
// kind this build cannot address, and holds the reader to skipping both and to taking a new
// block type with no Go file changed.
func requireAnUnknownKindIsSkipped(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema) {
	t.Helper()
	writer := `{"kind": "system", "id": "supertag-test"}`
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'lantern', 'supertag', 'project', 'live', '{}'::jsonb, now(), $1::jsonb, 0),
			('lantern', 'one', 'lantern', 'project', 'live', '{}'::jsonb, now(), $1::jsonb, 0)
	`, writer); err != nil {
		t.Fatalf("add a supertag and a node of a kind this build cannot address: %v", err)
	}
	built, err := schemanodes.Read(ctx, pool)
	if err != nil {
		t.Fatalf("read the schema beside a kind this build cannot address: %v", err)
	}
	for _, typ := range built.Types {
		if typ.Name == "lantern" {
			t.Errorf("the reader took the supertag %q, which declares no block type", typ.Name)
		}
	}
	if len(built.Types) != len(file.Types) {
		t.Errorf("the schema beside the unknown kinds holds %d types, want the %d the file declares", len(built.Types), len(file.Types))
	}

	// A block type added as a row alone, with no Go file changed, is served.
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'claim', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host', 'version', $2::int, 'position', 99),
				now(), $1::jsonb, 0),
			('attribute', 'claim/supports', 'attribute', 'project', 'live',
				jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0, 'kind', 'block[]', 'targets', jsonb_build_array('requirement')),
				now(), $1::jsonb, 0)
	`, writer, file.Version); err != nil {
		t.Fatalf("add a block type as a row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		values ('attribute', 'claim/supports', 'attribute_of', 'supertag', 'claim', now(), $1::jsonb, 0, null, true)
	`, writer); err != nil {
		t.Fatalf("tie the new type's attribute to it: %v", err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the new block type builds: %v", err)
	}
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("the type the store gained is no typed block of the installed schema")
	}
	if !slices.Contains(pmdoc.LinkKinds(), "supports") {
		t.Errorf("the link kinds are %v, want the new type's supports among them", pmdoc.LinkKinds())
	}
}

// schemaRows is every supertag and attribute node as its row stands, and how many tuples those
// nodes hold: what a second seed must leave untouched.
func schemaRows(t *testing.T, ctx context.Context, pool *store.Pool) ([]string, int) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select kind || ' ' || id || ' ' || supertag || ' ' || visibility || ' ' || state || ' ' ||
			attributes::text || ' ' || made_at::text || ' ' || made_by::text || ' ' || source_event::text
		from graph_nodes
		where kind in ('supertag', 'attribute')
		order by kind, id
	`)
	if err != nil {
		t.Fatalf("read the type nodes: %v", err)
	}
	defer rows.Close()
	var written []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read a type node: %v", err)
		}
		written = append(written, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the type nodes: %v", err)
	}
	var tuples int
	if err := pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind in ('supertag', 'attribute')
	`).Scan(&tuples); err != nil {
		t.Fatalf("count the type nodes' tuples: %v", err)
	}
	return written, tuples
}

func readBlockSchemaFile(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	body, err := os.ReadFile(filepath.Clean(blockSchemaFile))
	if err != nil {
		t.Fatalf("read %s: %v", blockSchemaFile, err)
	}
	return decodeSchema(t, body)
}

func decodeSchema(t *testing.T, body []byte) pmdoc.BlockSchema {
	t.Helper()
	var value pmdoc.BlockSchema
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode a block schema: %v", err)
	}
	return value
}

func attributeChoices(t *testing.T, value pmdoc.BlockSchema, typeName, attribute string) []string {
	t.Helper()
	for _, typ := range value.Types {
		if typ.Name != typeName {
			continue
		}
		declared, held := typ.Attributes.Lookup(attribute)
		if !held {
			t.Fatalf("type %q declares no attribute %q", typeName, attribute)
		}
		return declared.Choices
	}
	t.Fatalf("the schema declares no type %q", typeName)
	return nil
}

func typeContent(t *testing.T, value pmdoc.BlockSchema, typeName string) pmdoc.BlockContentRule {
	t.Helper()
	for _, typ := range value.Types {
		if typ.Name == typeName {
			return typ.Content
		}
	}
	t.Fatalf("the schema declares no type %q", typeName)
	return ""
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// missing is what the left list holds and the right does not, as a difference a reader can act
// on.
func missing(left, right []string) []string {
	var only []string
	for _, name := range left {
		if !slices.Contains(right, name) {
			only = append(only, name)
		}
	}
	if only == nil {
		return []string{}
	}
	return only
}
