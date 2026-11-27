package graphcensus

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheBackfillRefusesToRunWithoutItsIndexes holds Backfill to refusing, before it writes
// anything, while any of BackfillIndexes is absent, to naming it, and to printing the statements
// that build it again by hand: each index is dropped in turn, as its down file drops it after its
// version is recorded, which no boot then builds again. The run must fail naming that index and no
// other, leave the block row it would have written naming no node, and record no horizon, and its
// message must end in the schema's search_path and the index's migration's own create statement,
// one a line. Those statements are then run as the message says, each on its own on a connection
// of their own, and must build the index the migration built (pg_get_indexdef, read before the
// drop). The control is the same run once every index stands again: it succeeds and names the
// block's node, so the refusal is the index's absence and nothing else of the database, and the
// printed remedy is one that clears it.
func TestTheBackfillRefusesToRunWithoutItsIndexes(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	document := seedPreStoreDocument(t, database, "only")
	definitions := indexDefinitions(t, pool)
	written := backfillWrites(t, pool, document.artifactID)
	for _, index := range BackfillIndexes {
		if _, err := pool.Exec(ctx, "drop index "+index.Name); err != nil {
			t.Fatalf("drop %s: %v", index.Name, err)
		}
		_, err := Backfill(ctx, database, io.Discard)
		statements := requireRefusal(t, pool, err, index, false)
		if blocks, horizons := written(); blocks != 0 || horizons != 0 {
			t.Errorf("the refused run with %s dropped named %d block node(s) and recorded %d horizon(s), want none", index.Name, blocks, horizons)
		}
		runByHand(t, pool, statements)
		requireDefinition(t, pool, index.Name, definitions[index.Name])
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("the backfill with every index built again by hand, the control: %v", err)
	}
	if blocks, horizons := written(); blocks != 1 || horizons != 1 {
		t.Errorf("the backfill with every index built again by hand named %d block node(s) and recorded %d horizon(s), want 1 and 1", blocks, horizons)
	}
}

// TestTheBackfillRefusesToRunOverAnInvalidIndex holds Backfill to refusing while one of
// BackfillIndexes stands but is invalid, as a concurrent build that failed leaves it, and to
// printing a drop of it before its build: its migration's create ... if not exists would leave an
// invalid index of its name as it is. document_blocks_naming_no_node is dropped and a concurrent
// build of that name fails on the table's rows, which leaves an INVALID index of the name in the
// catalog, one the planner never reads. The run must fail naming it, print the search_path, the
// drop and the migration's create statement, one a line, and leave the block rows naming no node
// and record no horizon. The printed statements are then run as the message says, and must build
// the index the migration built; the same run, the control, then succeeds and names both blocks'
// nodes, so the refusal is the index's invalidity and nothing else of the database, and the
// printed remedy is one that clears it.
func TestTheBackfillRefusesToRunOverAnInvalidIndex(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	document := seedPreStoreDocument(t, database, "first", "second")
	index := BackfillIndexes[slices.IndexFunc(BackfillIndexes, func(i BackfillIndex) bool { return i.Name == "document_blocks_naming_no_node" })]
	definition := indexDefinitions(t, pool)[index.Name]
	if _, err := pool.Exec(ctx, "drop index "+index.Name); err != nil {
		t.Fatalf("drop %s: %v", index.Name, err)
	}
	// The two block rows of one document share artifact_id, so a unique build over it fails once
	// its catalog entry is made, and the entry stays, marked invalid.
	if _, err := pool.Exec(ctx, "create unique index concurrently "+index.Name+" on document_blocks (artifact_id)"); err == nil {
		t.Fatalf("the unique build of %s over two blocks of one document succeeded; this test needs it to fail", index.Name)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `select indisvalid from pg_index where indexrelid = to_regclass($1)`, index.Name).Scan(&valid); err != nil || valid {
		t.Fatalf("%s after the failed build: valid %t, %v; want an INVALID index of that name", index.Name, valid, err)
	}
	written := backfillWrites(t, pool, document.artifactID)
	_, err := Backfill(ctx, database, io.Discard)
	statements := requireRefusal(t, pool, err, index, true)
	if blocks, horizons := written(); blocks != 0 || horizons != 0 {
		t.Errorf("the refused run named %d block node(s) and recorded %d horizon(s), want none", blocks, horizons)
	}
	runByHand(t, pool, statements)
	requireDefinition(t, pool, index.Name, definition)
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("the backfill with the index built again by hand, the control: %v", err)
	}
	if blocks, horizons := written(); blocks != 2 || horizons != 1 {
		t.Errorf("the backfill with the index built again by hand named %d block node(s) and recorded %d horizon(s), want 2 and 1", blocks, horizons)
	}
}

// indexDefinitions reads pg_get_indexdef of each of BackfillIndexes, as the migrations built them.
func indexDefinitions(t *testing.T, pool *store.Pool) map[string]string {
	t.Helper()
	definitions := map[string]string{}
	for _, index := range BackfillIndexes {
		var definition string
		if err := pool.QueryRow(context.Background(), `select pg_get_indexdef(to_regclass($1))`, index.Name).Scan(&definition); err != nil || definition == "" {
			t.Fatalf("read the definition of %s (%q): %v", index.Name, definition, err)
		}
		definitions[index.Name] = definition
	}
	return definitions
}

// backfillWrites answers how many of the document's blocks name their node and how many horizons
// graph_backfill_horizon holds: what a refused run must leave at none.
func backfillWrites(t *testing.T, pool *store.Pool, artifactID string) func() (int, int) {
	return func() (blocks, horizons int) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `
			select (select count(*) from document_blocks where artifact_id = $1::uuid and node_kind is not null),
			       (select count(*) from graph_backfill_horizon)
		`, artifactID).Scan(&blocks, &horizons); err != nil {
			t.Fatalf("read what the run wrote: %v", err)
		}
		return blocks, horizons
	}
}

// requireRefusal fails unless err is the refusal of a run with index alone unusable: it names that
// index and no other of BackfillIndexes, and its message ends, one statement a line, in the
// search_path of the schema the store's tables are in, a drop of the index where invalid says one
// of its name stands, and a create ... if not exists of the index, the statement of the index's own
// migration (runByHand and requireDefinition hold it to building what the migration built). It
// answers those statements.
func requireRefusal(t *testing.T, pool *store.Pool, err error, index BackfillIndex, invalid bool) []string {
	t.Helper()
	if err == nil {
		t.Fatalf("the backfill ran with %s unusable", index.Name)
	}
	message := err.Error()
	head, tail, _ := strings.Cut(message, "\n")
	if !strings.HasPrefix(head, "refused: the index(es) "+index.Name+" are absent or invalid") {
		t.Fatalf("the backfill with %s unusable failed with %q, want a refusal naming it", index.Name, message)
	}
	for _, other := range BackfillIndexes {
		if other != index && strings.Contains(message, other.Name) {
			t.Errorf("the refusal with only %s unusable names %s too: %q", index.Name, other.Name, message)
		}
	}
	var statements []string
	for _, line := range strings.Split(tail, "\n") {
		statements = append(statements, strings.TrimSpace(line))
	}
	var schema string
	if err := pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read the store's schema: %v", err)
	}
	want := []string{"set search_path to " + pgx.Identifier{schema}.Sanitize() + ";"}
	if invalid {
		want = append(want, "drop index concurrently if exists "+pgx.Identifier{index.Name}.Sanitize()+";")
	}
	build := "create index concurrently if not exists " + index.Name + " on "
	if len(statements) != len(want)+1 || !slices.Equal(statements[:len(want)], want) ||
		!strings.HasPrefix(statements[len(want)], build) || !strings.HasSuffix(statements[len(want)], ";") {
		t.Fatalf("the refusal with %s unusable ends in the statements %q; want %q and then its migration's %s…;", index.Name, statements, want, build)
	}
	t.Logf("with %s unusable:\n%s", index.Name, message)
	return statements
}

// runByHand runs statements as the refusal tells an operator to: each on its own, outside a
// transaction, on one connection opened for them and closed after, so its search_path reaches no
// other test's connection.
func runByHand(t *testing.T, pool *store.Pool, statements []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect to run the printed statements: %v", err)
	}
	defer conn.Close(ctx)
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("run the printed statement %q: %v", statement, err)
		}
	}
}

// requireDefinition fails unless the index name stands valid with definition, the one its
// migration built.
func requireDefinition(t *testing.T, pool *store.Pool, name, definition string) {
	t.Helper()
	var got string
	var valid bool
	if err := pool.QueryRow(context.Background(), `
		select pg_get_indexdef(i.indexrelid), i.indisvalid from pg_index i where i.indexrelid = to_regclass($1)
	`, name).Scan(&got, &valid); err != nil {
		t.Fatalf("read %s after the printed statements ran: %v", name, err)
	}
	if got != definition || !valid {
		t.Errorf("the printed statements built %s as %q (valid %t), want %q, valid, as its migration built it", name, got, valid, definition)
	}
}
