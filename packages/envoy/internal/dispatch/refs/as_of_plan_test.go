package refs

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// asOfReadIndexes are the read indexes of the tuple table as the spec spells them, each with its
// key columns in order and its predicate as the catalog renders it. graph_tuples_as_of_in serves
// two of the five reads the spec names - the current set into a node and the as-of read into one -
// so four indexes carry the five.
var asOfReadIndexes = []struct {
	name, columns, predicate, serves string
}{
	{"graph_tuples_current", "from_kind, from_id, kind, to_kind, to_id, seq desc",
		"((to_kind <> 'value'::text) AND (kind <> 'delivered_to'::text))", "the current set out of a node"},
	{"graph_tuples_values", "from_kind, from_id, kind, seq desc",
		"(to_kind = 'value'::text)", "the current value of one attribute"},
	{"graph_tuples_as_of_out", "from_kind, from_id, kind, at desc, seq desc",
		"", "the as-of reads out of a node: its text, its parent, its rank and its moves"},
	{"graph_tuples_as_of_in", "to_kind, to_id, kind, at desc, seq desc",
		"(to_kind <> 'value'::text)", "the current set into a node and the children of a node at a time"},
}

// asOfSeed is the store the plans are read over: SEED-1, whose tree holds 480 blocks under its
// issue node, each with a long history of text values; SEED-2, a child whose blocks stand under a
// block of SEED-1; and a handful of moved addresses. It holds about 50,000 tuples over about 500
// nodes, analyzed, so the planner prefers an index wherever one serves the read.
type asOfSeed struct {
	readSeq int64
}

// asOfSeedBlocks is how many blocks SEED-1's tree holds, and asOfSeedTexts how many text values
// each one's history holds.
const (
	asOfSeedBlocks = 480
	asOfSeedTexts  = 95
)

// seedAsOfStore writes the seed and analyzes both store tables. Two of the issue node's children
// hold ranks that differ only in case at one position, b1 the rank 1a and b2 the rank 1B.
func seedAsOfStore(t *testing.T, database *store.Store) asOfSeed {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 values ('issue', 'SEED-1', 'issue', 'project', 'live', now() - interval '2 days', '{}', 0),
		        ('issue', 'SEED-2', 'issue', 'project', 'live', now() - interval '2 days', '{}', 0),
		        ('artifact', 'SEED-1/spec', 'document', 'project', 'live', now() - interval '2 days', '{}', 0),
		        ('artifact', 'SEED-2/spec', 'document', 'project', 'live', now() - interval '2 days', '{}', 0)`,
		`insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 select 'block', 'SEED-1/spec#b' || i, 'block', 'project', 'live', now() - interval '1 day' + i * interval '1 second', '{}', 0
		 from generate_series(1, 480) i`,
		`insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 select 'block', 'SEED-2/spec#c' || i, 'block', 'project', 'live', now() - interval '1 day' + i * interval '1 second', '{}', 0
		 from generate_series(1, 10) i`,
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 values ('artifact', 'SEED-1/spec', 'part_of', 'issue', 'SEED-1', now() - interval '2 days', '{}', 0),
		        ('artifact', 'SEED-2/spec', 'part_of', 'issue', 'SEED-2', now() - interval '2 days', '{}', 0)`,
		// Each block: its part_of - the first hundred under the issue, each other one under one of
		// those - its in_document, its rank, its state, its placement, its place in the text - its
		// text_parent, the spec for the first hundred and one of those for each other one, and its
		// text_rank - and its history of text.
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select 'block', 'SEED-1/spec#b' || i, 'part_of',
		        case when i <= 100 then 'issue' else 'block' end,
		        case when i <= 100 then 'SEED-1' else 'SEED-1/spec#b' || ((i - 1) % 100 + 1) end,
		        now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, 'in_document', 'artifact', 'SEED-1/spec', now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, '` + AttributeRank + `', 'value',
		        case i when 1 then '1a' when 2 then '1B' else '2' || lpad(i::text, 4, '0') end,
		        now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, 'state', 'value', 'live', now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, '` + AttributePlacement + `', 'value', 'text', now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, '` + TupleTextParent + `',
		        case when i <= 100 then 'artifact' else 'block' end,
		        case when i <= 100 then 'SEED-1/spec' else 'SEED-1/spec#b' || ((i - 1) % 100 + 1) end,
		        now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, '` + AttributeTextRank + `', 'value', '1' || lpad(i::text, 4, '0'), now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 480) i
		 union all
		 select 'block', 'SEED-1/spec#b' || i, 'text', 'value',
		        ':::requirement{#b' || i || '}' || chr(10) || 'The words of b' || i || ', written ' || version || ' times.' || chr(10) || ':::' || chr(10),
		        now() - interval '1 day' + version * interval '1 minute', '{}'::jsonb, 0
		 from generate_series(1, 480) i, generate_series(1, 95) version`,
		// SEED-2's blocks, each in its own document and placed under SEED-1's first block.
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select 'block', 'SEED-2/spec#c' || i, 'part_of', 'block', 'SEED-1/spec#b1', now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 10) i
		 union all
		 select 'block', 'SEED-2/spec#c' || i, 'in_document', 'artifact', 'SEED-2/spec', now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 10) i
		 union all
		 select 'block', 'SEED-2/spec#c' || i, '` + AttributeRank + `', 'value', '1' || lpad(i::text, 3, '0'), now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, 10) i`,
		// Ten old addresses, each moved to a block of SEED-1.
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select 'block', 'SEED-1/old#b' || i, '` + graphstore.TupleMovedTo + `', 'block', 'SEED-1/spec#b' || i, now() - interval '1 hour', '{}'::jsonb, 0
		 from generate_series(1, 10) i`,
		`analyze graph_nodes`,
		`analyze graph_tuples`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the store: %v\n%s", err, statement)
		}
	}
	var seed asOfSeed
	var tuples, nodes int
	if err := database.Pool.QueryRow(ctx, `select max(seq), count(*), (select count(*) from graph_nodes) from graph_tuples`).Scan(&seed.readSeq, &tuples, &nodes); err != nil {
		t.Fatalf("read the seed's size: %v", err)
	}
	t.Logf("the seed holds %d tuples over %d nodes, read at seq %d", tuples, nodes, seed.readSeq)
	if tuples < asOfSeedBlocks*asOfSeedTexts {
		t.Fatalf("the seed holds %d tuples, fewer than its %d blocks' histories of text alone", tuples, asOfSeedBlocks*asOfSeedTexts)
	}
	return seed
}

// asOfRead is one statement the walk of an issue's tree makes, with the arguments it is read
// under and the indexes that serve it: its plan reads graph_tuples or graph_nodes through at
// least one of them. A later read is one more entry of asOfReads.
type asOfRead struct {
	name      string
	statement string
	args      []any
	through   []string
}

// asOfReads are the walk's reads over the seed: the head's cut, the children of two nodes in rank
// order, the values and the relations out of every block of SEED-1's tree in one read, the rows of
// nodes, the moves of old addresses, and the nodes of a child placed under its parent's tree, each
// at the seed's seq; and the reads a render makes as of a time inside the seed's history of text
// (TreeCut.Dated): the children, the values and the relations - a block's text_rank and its
// text_parent among them - and the blocks of a document. A read of the part_of or the moved_to out
// of a node is served by graph_tuples_current as by graph_tuples_as_of_out, each keyed on the node
// and the kind: the planner takes whichever it costs lower. The values and relations read takes a
// lateral read of each node's tuples, keyed on the node, over a tree's every block, where a join
// of the nodes to the tuples is planned as one scan of the whole table.
func asOfReads(seed asOfSeed) []asOfRead {
	parents := []Target{{Kind: graphstore.KindIssue, ID: "SEED-1"}, {Kind: BlockNodeKind, ID: "SEED-1/spec#b1"}}
	parentKinds, parentIDs := targetColumns(parents)
	blocks := []Target{{Kind: BlockNodeKind, ID: "SEED-1/spec#b7"}, {Kind: BlockNodeKind, ID: "SEED-1/spec#b300"}}
	blockKinds, blockIDs := targetColumns(blocks)
	tree := make([]Target, asOfSeedBlocks)
	for index := range tree {
		tree[index] = Target{Kind: BlockNodeKind, ID: "SEED-1/spec#b" + strconv.Itoa(index+1)}
	}
	treeKinds, treeIDs := targetColumns(tree)
	seq, open := TreeCut{ReadSeq: seed.readSeq}.bounds()
	unbounded, at := TreeCut{At: time.Now().Add(-24*time.Hour + 30*time.Minute), Dated: true}.bounds()
	return []asOfRead{
		{"the head's cut", headCutStatement, nil, []string{"graph_tuples_pkey"}},
		{"the children of a node in rank order at a time", treeChildrenStatement,
			[]any{parentKinds, parentIDs, seq, open}, []string{"graph_tuples_as_of_in"}},
		{"the children of a node in rank order as of a render's time", treeChildrenStatement,
			[]any{parentKinds, parentIDs, unbounded, at}, []string{"graph_tuples_as_of_in"}},
		{"the subtrees of nodes the walk of SEED-1 enters, every level in one read, at a time", treeSubtreesStatement,
			[]any{parentKinds, parentIDs, seq, open, "SEED-1"}, []string{"graph_tuples_as_of_in"}},
		{"the values and the relations out of every block of a tree at a time", treeFactsStatement,
			[]any{treeKinds, treeIDs, seq, open}, []string{"graph_tuples_as_of_out"}},
		{"the values and the relations out of every block of a tree as of a render's time, a text_rank and a text_parent among them", treeFactsStatement,
			[]any{treeKinds, treeIDs, unbounded, at}, []string{"graph_tuples_as_of_out"}},
		{"the rows of nodes", treeNodesStatement,
			[]any{blockKinds, blockIDs}, []string{"graph_nodes_pkey"}},
		{"the moved_to resolve at a time", treeMovedToStatement,
			[]any{[]string{"SEED-1/old#b3", "SEED-1/spec#b9"}, seq, open, graphstore.MovedToHops},
			[]string{"graph_tuples_as_of_out", "graph_tuples_current"}},
		{"the nodes of an issue placed under an issue above, at a time", treePlacedAboveStatement,
			[]any{"SEED-2", []string{"SEED-1"}, seq, open}, []string{"graph_tuples_as_of_in"}},
		{"the blocks of a document as of a render's time", treeMembersStatement,
			[]any{"SEED-1/spec", unbounded, at}, []string{"graph_tuples_as_of_in"}},
	}
}

// asOfUnkeyedRead is the control: the children read with its parent's kind left out, which no
// index of the tuple table leads with, so it reads graph_tuples whole.
const asOfUnkeyedRead = `
	select t.from_kind, t.from_id from graph_tuples t
	where t.to_id = $1 and t.kind = 'part_of' and t.seq <= $2`

// The as-of reads of the walk of an issue's tree, which its record and its renders read: over a
// seeded store of about 50,000 tuples on about 500 nodes, analyzed, each read's plan - explained
// with analyze, as the planner chooses it - reaches graph_tuples and graph_nodes through an index
// of the store, the stated one as an Index Scan, an Index Only Scan or a Bitmap Index Scan, and
// holds no Seq Scan of either table; a read that leaves its kind out is written in as a control and
// reads graph_tuples whole. The read indexes stand as the spec spells them. The
// children read answers a node's children by rank byte for byte (B before a), on a database whose
// own collation orders a before B. Last, of this child's migrations no statement writes the tuple
// table but a type migration's insert (store.TypeMigrations).
func TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'a' < 'B'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of a and B: %v", err)
	}
	if !collated {
		t.Fatal("this database orders 'B' before 'a' by its own collation, so it cannot tell a read that orders ranks byte for byte from one that forgot to: run this test where select 'a' < 'B' is true")
	}

	for _, index := range asOfReadIndexes {
		var columns, predicate string
		err := database.Pool.QueryRow(ctx, `
			select (select string_agg(a.attname || case when (i.indoption[k.n - 1] & 1) = 1 then ' desc' else '' end, ', ' order by k.n)
			        from generate_series(1, i.indnkeyatts) k (n)
			        join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[k.n - 1]),
			       coalesce(pg_get_expr(i.indpred, i.indrelid), '')
			from pg_index i join pg_class c on c.oid = i.indexrelid
			where c.relname = $1`, index.name).Scan(&columns, &predicate)
		if err != nil {
			t.Errorf("the catalog holds no index %s, which serves %s: %v", index.name, index.serves, err)
			continue
		}
		if columns != index.columns || predicate != index.predicate {
			t.Errorf("%s, which serves %s, is (%s) where %q in the catalog, want (%s) where %q",
				index.name, index.serves, columns, predicate, index.columns, index.predicate)
		}
	}

	seed := seedAsOfStore(t, database)
	children, err := TreeChildren(ctx, database.Pool, []Target{{Kind: graphstore.KindIssue, ID: "SEED-1"}}, TreeCut{ReadSeq: seed.readSeq})
	if err != nil {
		t.Fatalf("read the children of SEED-1: %v", err)
	}
	if len(children) < 2 || children[0].Node.ID != "SEED-1/spec#b2" || children[1].Node.ID != "SEED-1/spec#b1" {
		t.Errorf("the children of SEED-1 begin %+v, want b2 (rank 1B) and then b1 (rank 1a), ranks compared byte for byte", children[:min(len(children), 2)])
	}
	if len(children) != 101 {
		t.Errorf("SEED-1 holds %d children at seq %d, want the 100 blocks placed under it and its spec", len(children), seed.readSeq)
	}
	requireSubtreesReadEveryLevelsChildren(t, database, seed)

	for _, read := range asOfReads(seed) {
		plan := asOfPlan(t, database, read.statement, read.args)
		for _, table := range []string{"graph_tuples", "graph_nodes"} {
			if explaintest.SeqScansRelation(t, plan, table) {
				t.Errorf("%s reads %s by a Seq Scan:\n%s", read.name, table, plan)
			}
		}
		// An index scan of either table with no index condition reads the whole index, unless it
		// stops at its first row, as the head's cut reads the newest seq off the primary key.
		for _, node := range explaintest.Nodes(t, plan) {
			if (node.Relation == "graph_tuples" || node.Relation == "graph_nodes") && node.Index != "" && node.IndexCond == "" && node.Rows > 1 {
				t.Errorf("%s reads %s through %s with no index condition, %v rows a loop: the whole index", read.name, node.Relation, node.Index, node.Rows)
			}
		}
		scans := explaintest.IndexScans(t, plan)
		if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return slices.Contains(read.through, scan.Index) }) {
			t.Errorf("%s reads no part of its plan through %v, which serve it; it reads %v", read.name, read.through, scans)
		}
		t.Logf("%s reads %v", read.name, scans)
	}
	if plan := asOfPlan(t, database, asOfUnkeyedRead, []any{"SEED-1", seed.readSeq}); !explaintest.SeqScansRelation(t, plan, "graph_tuples") {
		t.Errorf("the control, a children read with its parent's kind left out, plans with no Seq Scan of graph_tuples, so the test cannot see a read no index serves:\n%s", plan)
	}

	requireTypeMigrationsAloneWriteTheTupleTable(t)
}

// requireSubtreesReadEveryLevelsChildren holds the one statement of TreeSubtrees to the children
// TreeChildren reads level by level over the seed, from SEED-1's node down: under each node the walk
// of SEED-1 enters, the same children in the same order, and nothing under a node it does not
// enter - SEED-2's blocks are answered under b1 and read under nowhere.
func requireSubtreesReadEveryLevelsChildren(t *testing.T, database *store.Store, seed asOfSeed) {
	t.Helper()
	ctx := context.Background()
	cut := TreeCut{ReadSeq: seed.readSeq}
	under, err := TreeSubtrees(ctx, database.Pool, []Target{{Kind: graphstore.KindIssue, ID: "SEED-1"}}, "SEED-1", cut)
	if err != nil {
		t.Fatalf("read the subtrees of SEED-1: %v", err)
	}
	levels := map[Target][]TreeChild{}
	read := 0
	for frontier := []Target{{Kind: graphstore.KindIssue, ID: "SEED-1"}}; len(frontier) > 0; {
		children, err := TreeChildren(ctx, database.Pool, frontier, cut)
		if err != nil {
			t.Fatalf("read a level of SEED-1's tree: %v", err)
		}
		frontier = nil
		for _, child := range children {
			levels[child.Parent] = append(levels[child.Parent], child)
			read++
			if TreeOwns("SEED-1", child.Node) {
				frontier = append(frontier, child.Node)
			}
		}
	}
	if !reflect.DeepEqual(under, levels) {
		t.Errorf("the subtrees read answers %d parents, the level reads %d: the one statement does not read the children the levels read", len(under), len(levels))
	}
	if got := len(under[Target{Kind: BlockNodeKind, ID: "SEED-1/spec#b1"}]); got != 14 {
		t.Errorf("b1 holds %d children in the subtrees read, want its 4 blocks of SEED-1 and the 10 of SEED-2 placed under it", got)
	}
	if _, entered := under[Target{Kind: BlockNodeKind, ID: "SEED-2/spec#c1"}]; entered || read != 491 {
		t.Errorf("the level reads read %d children, want 491 - SEED-1's 480 blocks, its spec and SEED-2's 10 blocks - and the subtrees read entered SEED-2's c1 = %t, want false", read, entered)
	}
}

// asOfPlan is the plan of statement under args, explained with analyze and buffers.
func asOfPlan(t *testing.T, database *store.Store, statement string, args []any) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement, args...).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	return explaintest.OnePlan(t, raw)
}

// asOfChildMigration names a migration of this child, <timestamp>_issue_tree_<what>.up.sql, as
// store's TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit finds them.
var asOfChildMigration = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// asOfTupleInsert is the one form a type migration's statement on the tuple table takes.
var asOfTupleInsert = regexp.MustCompile(`(?is)\binsert\s+into\s+graph_tuples\b`)

// asOfUpsert is an insert into the tuple table whose conflict clause updates the row it meets.
var asOfUpsert = regexp.MustCompile(`(?is)\binsert\s+into\s+graph_tuples\b[^;]*?\bon\s+conflict\b[^;]*?\bdo\s+update\b`)

// asOfCreateTrigger is a create trigger, which writes no row of its table and runs only on later
// writes, never on the rows already there (pgmigrate's catalogOnlyStatements): the head
// projection's trigger on graph_tuples (20261123180100_issue_tree_head) writes none of its rows.
var asOfCreateTrigger = regexp.MustCompile(`(?is)^\s*(--[^\n]*\n\s*)*create\s+(or\s+replace\s+)?(constraint\s+)?trigger\b`)

// requireTypeMigrationsAloneWriteTheTupleTable reads every migration of this child and every type
// migration (store.TypeMigrations) as pgmigrate.TouchedTables reads them: a statement that writes
// graph_tuples is an insert of a type migration, with no conflict clause that updates, and no
// other migration of this child writes graph_tuples at all. A create trigger on the table writes
// none of its rows (asOfCreateTrigger).
func requireTypeMigrationsAloneWriteTheTupleTable(t *testing.T) {
	t.Helper()
	migrations, err := pgmigrate.Load(os.DirFS("../store"), "migrations")
	if err != nil {
		t.Fatalf("load the store's migrations: %v", err)
	}
	for _, name := range store.TypeMigrations {
		if !slices.ContainsFunc(migrations, func(migration pgmigrate.Migration) bool { return migration.Name == name }) {
			t.Errorf("store.TypeMigrations names %s, which is no migration of the store", name)
		}
	}
	read := 0
	for _, migration := range migrations {
		typeMigration := slices.Contains(store.TypeMigrations, migration.Name)
		if !typeMigration && !asOfChildMigration.MatchString(migration.Name) {
			continue
		}
		read++
		for _, statement := range pgmigrate.CensusStatements(migration.SQL) {
			if asOfCreateTrigger.MatchString(statement) || !slices.Contains(pgmigrate.TouchedTables(statement), "graph_tuples") {
				continue
			}
			beside := asOfTupleInsert.ReplaceAllString(statement, "insert into the_type_insert")
			switch {
			case !typeMigration:
				t.Errorf("%s, a migration of this child that is no type migration, writes graph_tuples: %q", migration.Name, statement)
			case slices.Contains(pgmigrate.TouchedTables(beside), "graph_tuples") || asOfUpsert.MatchString(statement):
				t.Errorf("%s, a type migration, writes graph_tuples other than by an insert: %q", migration.Name, statement)
			}
		}
	}
	if read <= len(store.TypeMigrations) {
		t.Errorf("read %d migrations, which holds no migration of this child beside the %d type migrations", read, len(store.TypeMigrations))
	}
	t.Logf("read %d migrations: this child's and the %d type migrations", read, len(store.TypeMigrations))
}
