package refs

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// twoReadsLoadBlockNodesStatement is loadBlockNodes' statement as it read before its one read by
// joins (blockNodesStatement), kept here alone as the reference that read is held to: each block's
// document read a row at a time (BlockDocumentSQL), its first version through a lateral over its
// text rows that tests a move of each block and reads each text row's document again
// (blockFirstVersion), its recorded authors through a sub-plan a row (twoReadsRecordedAuthorsSQL),
// and its stored node and the reader's predicate through the one probe of its row
// (visibleStoredKind), word for word as the read by joins keeps it.
func twoReadsLoadBlockNodesStatement(named blockNodesNamed) string {
	storedJoin, storedColumns, seen := visibleStoredKind("block", "d.node_id", "$2")
	return `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at, changed.authors,
		       ` + documentColumns("a") + `, ` + blockFirstColumns("first") + `, ` + twoReadsRecordedAuthorsSQL("d.node_id") + `,
		       ` + storedColumns + `
		from document_blocks d
		cross join lateral (select ` + BlockDocumentSQL("d") + ` as id) owner
		cross join lateral (
			select a.id, a.issue_key, a.project_key, a.session_id, a.slug, a.kind, a.is_primary
			from artifacts a where a.id = owner.id limit 1
		) a
		left join lateral (
			select changed.authors from artifact_versions changed
			where changed.artifact_id = owner.id and changed.number = d.changed_version limit 1
		) changed on true
		` + blockFirstVersion("first", "d.artifact_id", "d.block_id") + `
		` + storedJoin + `
		where ` + named.held() + ` and ` + seen
}

// twoReadsRecordedAuthorsSQL is the sub-plan the block loader read a block's recorded authors by
// before its one grouped read: the who of each authored_by the block node whose address the SQL
// text address names states now, in their sequence order, as one jsonb array, and null where it
// states none.
func twoReadsRecordedAuthorsSQL(address string) string {
	return `(select jsonb_agg(held.who order by held.seq) from (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.who, t.seq, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = `+address+` and t.kind = '`+graphstore.TupleAuthoredBy+`'
			  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held where held.asserted)`
}

// twoReadsBlockRowsStatement is the issue tree read's own read of the document_blocks rows of its
// block nodes as it read them before the block loader's one read answered them too: each row's
// block id, type, text sum, changed version, and its document (BlockDocumentSQL) with that
// document's slug and band, for every reader alike.
var twoReadsBlockRowsStatement = `
		select d.node_id, d.block_id, d.type, d.text_sha256, d.changed_version, doc.id::text, doc.slug, ` + DocumentBandSQL("doc") + `
		from document_blocks d
		cross join lateral (select ` + BlockDocumentSQL("d") + ` as id) owner
		join artifacts doc on doc.id = owner.id
		where d.node_id = any($1)`

// loaderFixtureTables are the tables the block loader's fixture copies rows into, each after the
// tables its foreign keys name.
var loaderFixtureTables = []copyTable{
	{name: "projects", columns: []string{"key", "name", "default_assignee", "created_at"}},
	{name: "issues", columns: []string{"key", "project_key", "number", "title", "created_by", "rank", "created_at", "updated_at"}},
	{name: "artifacts", columns: []string{"id", "issue_key", "project_key", "slug", "name", "kind", "ref_key", "is_primary", "created_by", "created_at"}},
	{name: "artifact_versions", columns: []string{"artifact_id", "number", "markdown", "authors", "created_at"}},
	{name: "issue_trees", columns: []string{"issue_key", "artifact_id"}},
	{name: "document_rooms", columns: []string{"artifact_id", "room"}},
	{name: "issue_room_blocks", columns: []string{"tree_artifact_id", "block_id", "document_id"}},
	{name: "document_blocks", columns: []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at", "created_at"}},
	{name: "document_block_texts", columns: []string{"artifact_id", "block_id", "version", "text"}},
	{name: "document_indexed_versions", columns: []string{"artifact_id", "version"}},
	{name: "graph_nodes", columns: []string{"kind", "id", "supertag", "visibility", "state", "made_at", "made_by", "source_event"}},
	{name: "graph_tuples", columns: []string{"from_kind", "from_id", "kind", "to_kind", "to_id", "at", "who", "source_event", "legacy_source_seq", "asserted"}},
}

// The people of the block loader's fixture, as the store holds an actor.
const (
	loaderAlice = `{"kind":"user","id":"alice"}`
	loaderBob   = `{"kind":"user","id":"bob"}`
	loaderCarol = `{"kind":"user","id":"carol"}`
)

// loaderFixture is the block loader's fixture: every block node it writes, and the ones the test
// names on their own.
type loaderFixture struct {
	blocks              []string
	reserved, moved     string
	untexted, authored  string
	roomed, unknownMade string
}

// writeLoaderFixture writes, through COPY, two issues of one project. LOAD-2 holds a spec of
// three versions, of which a block-aware build indexed the first two: a block first held by
// version 1, one first held by version 3 (made in it, since version 2 was indexed without it), one
// with no text row at all, one reserved for its author alice, and one whose node records two
// authors standing and a third retracted. LOAD-1 is an issue room: its tree artifact, its spec and
// its Verification document, each a fragment of the room, and four blocks keyed by the tree - one
// in the spec's fragment, one in the Verification document's whose first version no indexed
// version before it makes known, one whose node has stood in both documents (two asserted
// in_document), and one no issue_room_blocks row names, which is the tree's own.
func writeLoaderFixture(t *testing.T, database *store.Store) loaderFixture {
	t.Helper()
	epoch := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	tick := 0
	at := func() time.Time {
		tick++
		return epoch.Add(time.Duration(tick) * time.Second)
	}
	rows := map[string][][]any{}
	add := func(table string, values ...any) { rows[table] = append(rows[table], values) }
	add("projects", "LOAD", "The loader's project", "alice", at())
	for number := 1; number <= 2; number++ {
		key := "LOAD-" + strconv.Itoa(number)
		add("issues", key, "LOAD", number, key, loaderAlice, fmt.Sprintf("U%07d", number), at(), at())
	}
	const (
		spec   = "00000000-0000-4000-8000-00000000d001"
		tree   = "00000000-0000-4000-8000-00000000d101"
		room   = "00000000-0000-4000-8000-00000000d102"
		checks = "00000000-0000-4000-8000-00000000d103"
	)
	add("artifacts", spec, "LOAD-2", "LOAD", "spec", "spec", "doc", "LOAD-2/spec", "true", loaderAlice, at())
	add("artifacts", tree, "LOAD-1", "LOAD", "tree", "tree", "tree", "LOAD-1/tree", "false", loaderAlice, at())
	add("artifacts", room, "LOAD-1", "LOAD", "spec", "spec", "doc", "LOAD-1/spec", "true", loaderAlice, at())
	add("artifacts", checks, "LOAD-1", "LOAD", "verification-md", "verification.md", "doc", "LOAD-1/verification-md", "false", loaderAlice, at())
	for number, authors := range []string{`[` + loaderAlice + `]`, `[` + loaderBob + `]`, `[` + loaderAlice + `,` + loaderBob + `]`} {
		for _, document := range []string{spec, room, checks} {
			add("artifact_versions", document, number+1, "version "+strconv.Itoa(number+1), authors, at())
		}
	}
	add("document_indexed_versions", spec, 1)
	add("document_indexed_versions", spec, 2)
	add("document_indexed_versions", room, 1)
	add("issue_trees", "LOAD-1", tree)
	add("document_rooms", room, tree)
	add("document_rooms", checks, tree)

	var fixture loaderFixture
	block := func(artifact, refKey, blockID, typ string, changed int, texts ...int) string {
		node, when := refKey+"#"+blockID, at()
		add("document_blocks", artifact, blockID, node, typ, "sum-"+blockID, "The block "+blockID+".", changed, when, when)
		for _, version := range texts {
			add("document_block_texts", artifact, blockID, version, "The block "+blockID+" at version "+strconv.Itoa(version)+".")
		}
		fixture.blocks = append(fixture.blocks, node)
		return node
	}
	node := func(id, visibility, madeBy string) {
		add("graph_nodes", BlockNodeKind, id, "block", visibility, graphstore.StateLive, at(), madeBy, int(graphstore.NoEvent))
	}
	tuple := func(from, kind, toKind, toID, who string, asserted bool) {
		add("graph_tuples", BlockNodeKind, from, kind, toKind, toID, at(), who, int(graphstore.NoEvent), nil, strconv.FormatBool(asserted))
	}

	first := block(spec, "LOAD-2/spec", "p1", "paragraph", 2, 1, 2)
	node(first, graphstore.VisibilityProject, loaderAlice)
	fixture.unknownMade = block(spec, "LOAD-2/spec", "p3", "requirement", 3, 3)
	node(fixture.unknownMade, graphstore.VisibilityProject, loaderBob)
	fixture.untexted = block(spec, "LOAD-2/spec", "p4", "paragraph", 3)
	fixture.reserved = block(spec, "LOAD-2/spec", "mine", "paragraph", 2, 2)
	node(fixture.reserved, graphstore.VisibilityAuthor, loaderAlice)
	fixture.authored = block(spec, "LOAD-2/spec", "both", "check", 3, 1, 3)
	node(fixture.authored, graphstore.VisibilityProject, loaderAlice)
	tuple(fixture.authored, graphstore.TupleAuthoredBy, "person", "bob", loaderBob, true)
	tuple(fixture.authored, graphstore.TupleAuthoredBy, "person", "carol", loaderCarol, true)
	tuple(fixture.authored, graphstore.TupleAuthoredBy, "person", "alice", loaderAlice, true)
	tuple(fixture.authored, graphstore.TupleAuthoredBy, "person", "carol", loaderCarol, false)

	fixture.roomed = block(tree, "LOAD-1/tree", "r1", "requirement", 2, 1, 2)
	add("issue_room_blocks", tree, "r1", room)
	node(fixture.roomed, graphstore.VisibilityProject, loaderAlice)
	checked := block(tree, "LOAD-1/tree", "c1", "check", 3, 2, 3)
	add("issue_room_blocks", tree, "c1", checks)
	fixture.moved = block(tree, "LOAD-1/tree", "mv", "paragraph", 3, 1, 3)
	add("issue_room_blocks", tree, "mv", checks)
	tuple(fixture.moved, TupleInDocument, graphstore.KindArtifact, "LOAD-1/spec", loaderAlice, true)
	tuple(fixture.moved, TupleInDocument, graphstore.KindArtifact, "LOAD-1/verification-md", loaderAlice, true)
	tuple(checked, TupleInDocument, graphstore.KindArtifact, "LOAD-1/verification-md", loaderAlice, true)
	block(tree, "LOAD-1/tree", "loose", "paragraph", 1, 1)

	loadFixture(t, database.Pool, loaderFixtureTables, rows)
	return fixture
}

// The set-wise block loader: over a fixture of room-fragment blocks (issue_room_blocks rows), a
// block moved between two fragments (two asserted in_document), a block with no text row, a block
// first held by a later version, one whose node records authors, and one reserved for its author,
// read by its author, by another person and by nobody proved, through each of the three forms
// blockNodesAddressed names blocks by - one block, a list the kept plan reads, and one past
// blockNodesKeptPlan - the read by joins answers what the statement it replaced answered, row for
// row and column for column; and the issue tree's one read of the blocks answers every row the
// tree read's own row read answered, each column equal, and as nodes the rows its reader may see,
// as the registry's loader answers them. A read that dropped the moved-block join answers a first
// version of the moved block, and fails here naming that block.
func TestTheSetWiseBlockLoaderAnswersAsTheOldOne(t *testing.T) {
	database := storetest.Open(t)
	fixture := writeLoaderFixture(t, database)
	alice, bob := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}
	padded := slices.Clone(fixture.blocks)
	for index := 0; len(padded) <= blockNodesKeptPlan; index++ {
		padded = append(padded, "LOAD-9/nowhere#n"+strconv.Itoa(index))
	}
	for _, reader := range []struct {
		name string
		who  model.Actor
	}{{"alice, the reserved block's author", alice}, {"bob", bob}, {"nobody proved", model.Actor{}}} {
		lists := [][]string{fixture.blocks, padded}
		for _, block := range fixture.blocks {
			lists = append(lists, []string{block})
		}
		for _, ids := range lists {
			read := fmt.Sprintf("%s reading %d ids", reader.name, len(ids))
			requireSameBlockRows(t, read, database.Pool, reader.who, ids)
		}
		requireBlockNodesAndRows(t, reader.name, database.Pool, reader.who, fixture.blocks)
	}

	// The fixture holds what the test means it to.
	held := func(who model.Actor) map[string]loaderRow {
		return loaderRows(t, "the fixture's check", database.Pool, twoReadsLoadBlockNodesStatement, who, fixture.blocks)
	}
	byAlice, byBob := held(alice), held(bob)
	if _, seen := byAlice[fixture.reserved]; !seen || len(byAlice) != len(fixture.blocks) {
		t.Errorf("alice reads %d of the %d blocks, %s among them %t: want every block, her reserved one too", len(byAlice), len(fixture.blocks), fixture.reserved, seen)
	}
	if _, seen := byBob[fixture.reserved]; seen || len(byBob) != len(fixture.blocks)-1 {
		t.Errorf("bob reads %d of the %d blocks, %s among them %t: want every block but alice's reserved one", len(byBob), len(fixture.blocks), fixture.reserved, seen)
	}
	for _, check := range []struct {
		block, column, want string
	}{
		{fixture.moved, "version", "<nil>"},
		{fixture.untexted, "version", "<nil>"},
		{fixture.unknownMade, "version", "3"},
		{fixture.unknownMade, "known", "true"},
		{fixture.roomed, "id", "00000000-0000-4000-8000-00000000d102"},
		{fixture.authored, "jsonb_agg", fmt.Sprint([]any{map[string]any{"kind": "user", "id": "bob"}, map[string]any{"kind": "user", "id": "alice"}})},
	} {
		if got := byAlice[check.block].column(check.column); got != check.want {
			t.Errorf("the fixture's %s reads %s %s, want %s: the fixture does not hold what the test means it to", check.block, check.column, got, check.want)
		}
	}
}

// loaderRow is one row a block statement answered: its columns' names and values, in order.
type loaderRow struct {
	names  []string
	values []any
}

// column is the first value of the column named name, printed, and "absent" where the row has no
// such column.
func (row loaderRow) column(name string) string {
	if index := slices.Index(row.names, name); index >= 0 {
		return fmt.Sprint(row.values[index])
	}
	return "absent"
}

// loaderRows reads the rows statement answers for who over ids, under the arguments
// blockNodesAddressed binds, keyed by the block's node id, the first column.
func loaderRows(t *testing.T, read string, q Queryer, statement func(blockNodesNamed) string, who model.Actor, ids []string) map[string]loaderRow {
	t.Helper()
	named, args := blockNodesAddressed(ids, who)
	rows, err := q.Query(context.Background(), statement(named), args...)
	if err != nil {
		t.Fatalf("%s: read the blocks: %v", read, err)
	}
	return collectLoaderRows(t, read, rows)
}

// collectLoaderRows is rows keyed by their first column, a block's node id.
func collectLoaderRows(t *testing.T, read string, rows pgx.Rows) map[string]loaderRow {
	t.Helper()
	defer rows.Close()
	held := map[string]loaderRow{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("%s: read a block's row: %v", read, err)
		}
		row := loaderRow{values: values}
		for _, field := range rows.FieldDescriptions() {
			row.names = append(row.names, field.Name)
		}
		id := fmt.Sprint(values[0])
		if _, twice := held[id]; twice {
			t.Fatalf("%s: the read answers %s twice", read, id)
		}
		held[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: read the blocks: %v", read, err)
	}
	return held
}

// requireSameBlockRows fails unless the read by joins answers who's read of ids with the rows the
// statement it replaced answers, block for block, each column the old statement answers equal.
func requireSameBlockRows(t *testing.T, read string, q Queryer, who model.Actor, ids []string) {
	t.Helper()
	want := loaderRows(t, read+", the statement replaced", q, twoReadsLoadBlockNodesStatement, who, ids)
	got := loaderRows(t, read+", the read by joins", q, func(named blockNodesNamed) string { return blockNodesStatement(named, true) }, who, ids)
	for _, id := range slices.Sorted(keysOf(want, got)) {
		old, oldHeld := want[id]
		now, nowHeld := got[id]
		if oldHeld != nowHeld {
			t.Errorf("%s: the block %s is answered by the statement replaced %t and by the read by joins %t", read, id, oldHeld, nowHeld)
			continue
		}
		for index, value := range old.values {
			if index >= len(now.values) || !reflect.DeepEqual(value, now.values[index]) {
				var answered any = "nothing"
				if index < len(now.values) {
					answered = now.values[index]
				}
				t.Errorf("%s: the block %s answers %s %v in the read by joins and %v in the statement replaced", read, id, old.names[index], answered, value)
			}
		}
	}
}

// requireBlockNodesAndRows fails unless BlockNodesAndRows answers who's read of ids with every row
// the tree read's own row read answers, each column equal, and with each node the registry's
// loader answers who, as ExcerptedNodes answers it.
func requireBlockNodesAndRows(t *testing.T, read string, q Queryer, who model.Actor, ids []string) {
	t.Helper()
	ctx := context.Background()
	nodes, blocks, err := BlockNodesAndRows(ctx, q, who, ids)
	if err != nil {
		t.Fatalf("%s: read the blocks' nodes and rows: %v", read, err)
	}
	rows, err := q.Query(ctx, twoReadsBlockRowsStatement, ids)
	if err != nil {
		t.Fatalf("%s: read the blocks' rows as the tree read read them: %v", read, err)
	}
	want := collectLoaderRows(t, read, rows)
	if len(blocks) != len(want) {
		t.Errorf("%s: the one read answers %d rows and the tree read's row read %d", read, len(blocks), len(want))
	}
	for id, row := range want {
		got, held := blocks[id]
		band := fmt.Sprint(row.values[7])
		wanted := BlockRow{
			BlockID: fmt.Sprint(row.values[1]), Type: fmt.Sprint(row.values[2]), TextSHA256: fmt.Sprint(row.values[3]),
			ChangedVersion: int(row.values[4].(int32)), Document: fmt.Sprint(row.values[5]), Slug: fmt.Sprint(row.values[6]), Band: band[0],
		}
		if !held || got != wanted {
			t.Errorf("%s: the block %s's row is %+v (held %t) in the one read and %+v in the tree read's row read", read, id, got, held, wanted)
		}
	}
	excerpted, err := ExcerptedNodes(ctx, q, who, targetsOfBlocks(ids), "")
	if err != nil {
		t.Fatalf("%s: read the blocks through the registry: %v", read, err)
	}
	if len(nodes) != len(excerpted) {
		t.Errorf("%s: the one read answers %d nodes and the registry's loader %d", read, len(nodes), len(excerpted))
	}
	for target, node := range excerpted {
		if got, held := nodes[target.ID]; !held || !reflect.DeepEqual(got, node) {
			t.Errorf("%s: the block %s is %+v (held %t) in the one read and %+v through the registry", read, target.ID, got, held, node)
		}
	}
}

// targetsOfBlocks is each block node id of ids as a target.
func targetsOfBlocks(ids []string) []Target {
	targets := make([]Target, len(ids))
	for index, id := range ids {
		targets[index] = Target{Kind: BlockNodeKind, ID: id}
	}
	return targets
}

// keysOf is every key of the maps, each once.
func keysOf(maps ...map[string]loaderRow) func(func(string) bool) {
	return func(yield func(string) bool) {
		seen := map[string]bool{}
		for _, held := range maps {
			for key := range held {
				if seen[key] {
					continue
				}
				seen[key] = true
				if !yield(key) {
					return
				}
			}
		}
	}
}
