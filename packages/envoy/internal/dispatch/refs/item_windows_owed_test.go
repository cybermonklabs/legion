package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The two shapes ItemsOwedWindows had before it read a marker of its own, kept here as the
// oracles the marker read is held equal to
// (TestItemsOwedWindowsAnswersWhatTheShapesBeforeTheMarkerAnswered) and as the negative control
// of the plan test (TestItemsOwedWindowsReadsThePartialIndex). Both decide owedness from state,
// so both read every row of the kind's state table, which is what the marker is here to stop.
//
// itemsOwedWindowsTwoNotExists is the first of them: the question as two correlated `not
// exists` probes under an `or`, which the planner can only run as two subplans, once per row of
// the state table. It is the shape measured at 13.7 s and 1,023,190 index searches over 511,595
// comment state rows (INFRA-988).
func itemsOwedWindowsTwoNotExists(kind ItemKind, tables itemTables) string {
	return `
		select s.` + tables.item + `::text from ` + tables.state + ` s
		where s.` + tables.item + ` > $1::uuid
		  and exists (
			select 1 from ` + tables.blocks + ` b where b.` + tables.item + ` = s.` + tables.item + ` and b.position is not null
		  )
		  and (not exists (
			select 1 from embedding_window_states w
			where w.kind = '` + kind.WindowKind() + `' and w.owner_key = '` + string(kind) + `/' || s.` + tables.item + `::text
			  and w.block_state = ` + itemBlockState + `
		  ) or not exists (
			select 1 from embeddings e
			where e.kind = '` + kind.WindowKind() + `' and split_part(e.id, '#', 1) = '` + string(kind) + `/' || s.` + tables.item + `::text
		  ))
		order by s.` + tables.item + ` limit $2
	`
}

// itemsOwedWindowsAntiJoins is the second: the same predicate written so that each side is
// read once, which is where the read stood in the working copy before the marker. It is the
// shape measured at 2.92 s a pass with 472 MB of temporary files written, and it is the plan
// test's control, because a read that decides owedness from state has to walk the state table
// however it is written.
func itemsOwedWindowsAntiJoins(kind ItemKind, tables itemTables) string {
	ownerKey := `'` + string(kind) + `/' || s.` + tables.item + `::text`
	return `
		with owed as materialized (
			select s.` + tables.item + ` as id
			from ` + tables.state + ` s
			left join embedding_window_states w
			       on w.kind = '` + kind.WindowKind() + `'
			      and w.owner_key = ` + ownerKey + `
			      and w.block_state = ` + itemBlockState + `
			left join (
				select split_part(e.id, '#', 1) as owner_key from embeddings e
				where e.kind = '` + kind.WindowKind() + `' and e.kind in ` + windowKinds + `
				group by 1
			) e on e.owner_key = ` + ownerKey + `
			where s.` + tables.item + ` > $1::uuid
			  and exists (
				select 1 from ` + tables.blocks + ` b
				where b.` + tables.item + ` = s.` + tables.item + ` and b.position is not null
			  )
			  and (w.owner_key is null or e.owner_key is null)
		)
		select id::text from owed order by id limit $2
	`
}

// windowState is one state an item's windows can be in, as the fixture writes it: whether the
// item holds a current block, what its state row's body_sha256 says, what the stamp of the state
// its windows were written from says (none: no row at all), whether it holds a window row, and
// whether its state row carries the window arm's marker (windows_owed_at).
// owed is whether ItemsOwedWindows must list it.
//
// The marker is what the read asks now, and the stamp and the window row are what the two
// shapes before it asked, so a state whose marker disagrees with them is a state the fixture
// must not be able to write: the marker is set wherever the blocks move (the trigger) and
// cleared only where the windows are written. Each state below therefore carries the marker the
// item would really hold in it, and the equivalence test reads all three questions over the
// same rows.
type windowState struct {
	name    string
	block   blockRow
	sum     *string
	stamp   *string
	window  bool
	marker  bool
	owed    bool
	comment string
}

// blockRow is which block row the fixture writes for an item: a current one, one a revision
// removed (position null), or none at all.
type blockRow int

const (
	currentBlock blockRow = iota
	removedBlock
	noBlock
)

// indexedSum is the body_sha256 a state row carries once a block-aware build has indexed the
// item, and otherSum another, the sum a build that re-indexed the item after its windows were
// written left behind.
const (
	indexedSum = "0f0f0f0f"
	otherSum   = "a1a1a1a1"
)

// windowStates is every state the window arm has to tell apart, each one written by the fixture
// for every kind ItemsOwedWindows serves: the states the marker itself can be in, then the
// states the readings before it told apart, which the marker must answer for the same way.
var windowStates = []windowState{{
	name:    "windowed, and the marker cleared",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(indexedSum),
	window:  true,
	marker:  false,
	owed:    false,
	comment: "the write that made these windows cleared the marker in the same statement",
}, {
	name:    "marked, and not yet windowed",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
	marker:  true,
	owed:    true,
	comment: "the trigger marked the write of the text, and no window has been written since",
}, {
	name:    "marked by a change of the item's blocks",
	block:   currentBlock,
	sum:     new(otherSum),
	stamp:   new(indexedSum),
	window:  true,
	marker:  true,
	owed:    true,
	comment: "the blocks moved on from the state the windows standing now were written from",
}, {
	name:    "marked, and its window row removed",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(indexedSum),
	window:  false,
	marker:  true,
	owed:    true,
	comment: "whoever removes a window row marks its owner, since the stamp still stands",
}, {
	name:    "marked, and the index holds no block yet",
	block:   noBlock,
	sum:     nil,
	stamp:   nil,
	window:  false,
	marker:  true,
	owed:    false,
	comment: "an older build's write marks both arms at once; the block arm owns this item",
}, {
	name:    "marked, and its only block was removed",
	block:   removedBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
	marker:  true,
	owed:    false,
	comment: "no current block, so the index holds no block these windows could stand for",
}, {
	name:    "no stamp",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  true,
	marker:  true,
	owed:    true,
	comment: "windows written before the stamps existed say nothing about the blocks they cover",
}, {
	name:    "the stamp stands at another state",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(otherSum),
	window:  true,
	marker:  true,
	owed:    true,
	comment: "a build without windows re-indexed the item, so its windows are of the old text",
}, {
	name:    "no sum, and the stamp reads the empty sum",
	block:   currentBlock,
	sum:     nil,
	stamp:   new(""),
	window:  true,
	marker:  false,
	owed:    false,
	comment: "a state row with no sum stamps and compares as '' (itemBlockState's coalesce)",
}, {
	name:    "no sum, and the stamp reads a sum",
	block:   currentBlock,
	sum:     nil,
	stamp:   new(indexedSum),
	window:  true,
	marker:  true,
	owed:    true,
	comment: "the same coalesce the other way: '' is not the sum the windows were written from",
}, {
	name:    "the only block was removed",
	block:   removedBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
	marker:  false,
	owed:    false,
	comment: "no current block, and the write that removed it wrote the windows it left",
}, {
	name:    "no block at all",
	block:   noBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
	marker:  false,
	owed:    false,
	comment: "an item the block index has not reached is owed an index, not windows",
}}

// windowFixtureFillers is how many items in the first state each kind carries beside the states
// above: a population, so the planner's choice over the fixture is one it could make over a
// deployment rather than the choice every plan is on a handful of rows.
const windowFixtureFillers = 200

// itemWriter is the SQL that writes one item of a kind and one block row of it. Each is written
// as SQL rather than through this package's own indexer because the fixture has to write states
// no indexer leaves behind - a stamp of another sum, a window row deleted under a standing stamp
// - which is exactly what the window arm exists to find.
type itemWriter struct {
	insertItem  string
	insertBlock string
}

func writerFor(t *testing.T, kind ItemKind) itemWriter {
	t.Helper()
	switch kind {
	case ItemComment:
		return itemWriter{
			insertItem: `insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1) returning id::text`,
			insertBlock: `insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, changed_revision, changed_at, removed_at)
			              values ($1::uuid, 'b1', 'comment/' || $1 || '#b1', $2::integer, 'paragraph', 'sum', 'excerpt', 1, now(), case when $2::integer is null then now() end)`,
		}
	case ItemMessage:
		return itemWriter{
			insertItem: `insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1) returning id::text`,
			insertBlock: `insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at, removed_at)
			              values ($1::uuid, 'b1', 'message/' || $1 || '#b1', $2::integer, 'paragraph', 'sum', 'excerpt', now(), now(), case when $2::integer is null then now() end)`,
		}
	case ItemAsk:
		return itemWriter{
			insertItem: `insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', $1) returning id::text`,
			insertBlock: `insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, changed_revision, changed_at, removed_at)
			              values ($1::uuid, 'b1', 'ask/' || $1 || '#b1', $2::integer, 'question', 'sum', 'excerpt', 1, now(), case when $2::integer is null then now() end)`,
		}
	}
	t.Fatalf("no fixture writer for %q", kind)
	return itemWriter{}
}

// windowFixtureItem is one item the fixture wrote, with the state its windows are in.
type windowFixtureItem struct {
	id    string
	state windowState
}

// owedWindowFixture is one database holding, for each kind ItemsOwedWindows serves, one item in
// every state of windowStates and windowFixtureFillers more whose windows stand.
type owedWindowFixture struct {
	database *store.Store
	items    map[ItemKind][]windowFixtureItem
}

func newOwedWindowFixture(t *testing.T) owedWindowFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the window fixture's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")

	fixture := owedWindowFixture{database: database, items: map[ItemKind][]windowFixtureItem{}}
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		writer := writerFor(t, kind)
		for _, state := range windowStates {
			fixture.items[kind] = append(fixture.items[kind], windowFixtureItem{
				id:    writeWindowItem(t, tx, kind, writer, state),
				state: state,
			})
		}
		for range windowFixtureFillers {
			writeWindowItem(t, tx, kind, writer, windowStates[0])
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the window fixture: %v", err)
	}
	// The plan this fixture is read for is the one the planner makes over row counts it knows,
	// and a table just written has none.
	if _, err := database.Pool.Exec(ctx, `analyze`); err != nil {
		t.Fatalf("analyze the window fixture: %v", err)
	}
	return fixture
}

// writeWindowItem writes one item of kind in state: the item, its block row, the state row its
// kind's trigger made (settled to state.sum and to the marker the state carries, as an index
// that cleared owed_at and a write of the windows that cleared windows_owed_at leave it), the
// stamp of the state its windows were written from, and one window row.
func writeWindowItem(t *testing.T, tx pgx.Tx, kind ItemKind, writer itemWriter, state windowState) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := tx.QueryRow(ctx, writer.insertItem, "fixture body for "+state.name).Scan(&id); err != nil {
		t.Fatalf("write the %s whose %s: %v", kind, state.name, err)
	}
	tables, err := kind.tables()
	if err != nil {
		t.Fatalf("tables of %s: %v", kind, err)
	}
	switch state.block {
	case currentBlock:
		execFixture(t, tx, "write the block of the "+string(kind), writer.insertBlock, id, 0)
	case removedBlock:
		execFixture(t, tx, "write the removed block of the "+string(kind), writer.insertBlock, id, nil)
	case noBlock:
	}
	marker := "null"
	if state.marker {
		marker = "now()"
	}
	execFixture(t, tx, "settle the block state of the "+string(kind),
		`update `+tables.state+` set body_sha256 = $2, owed_at = null, windows_owed_at = `+marker+
			` where `+tables.item+` = $1::uuid`, id, state.sum)
	if state.stamp != nil {
		execFixture(t, tx, "stamp the windows of the "+string(kind),
			`insert into embedding_window_states (kind, owner_key, block_state) values ($1, $2, $3)`,
			kind.WindowKind(), kind.WindowOwnerKey(id), *state.stamp)
	}
	if state.window {
		execFixture(t, tx, "write the window of the "+string(kind),
			`insert into embeddings (kind, id, text_snapshot, content_hash) values ($1, $2 || '#b1', 'window text', 'hash')`,
			kind.WindowKind(), kind.WindowOwnerKey(id))
	}
	return id
}

// owedIDs is every item of kind the fixture wrote whose windows do not stand for its blocks, in
// the order ItemsOwedWindows answers them.
func (f owedWindowFixture) owedIDs(kind ItemKind) []string {
	var owed []string
	for _, item := range f.items[kind] {
		if item.state.owed {
			owed = append(owed, item.id)
		}
	}
	slices.Sort(owed)
	return owed
}

// Every item whose windows do not stand for its blocks is listed, and every item whose windows
// do stand for them is not: the stamp missing, the stamp standing at another state and the
// window row gone are each owed, and an item whose only block a revision removed, one the block
// index has not reached, and one whose null sum the stamp reads as the empty string are each
// not.
func TestItemsOwedWindowsListsTheItemsWhoseWindowsDoNotStandForTheirBlocks(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		listed, err := ItemsOwedWindows(ctx, fixture.database.Pool, kind, "", 1000)
		if err != nil {
			t.Fatalf("list the %ss owed windows: %v", kind, err)
		}
		if want := fixture.owedIDs(kind); !slices.Equal(listed, want) {
			t.Errorf("the %ss owed windows are %v, want %v:\n%s", kind, listed, want, fixture.describe(kind, listed))
		}
	}
}

// describe names each item of kind the fixture wrote, the state its windows are in and whether
// the read listed it, so a failure says which state the answer got wrong.
func (f owedWindowFixture) describe(kind ItemKind, listed []string) string {
	var lines strings.Builder
	for _, item := range f.items[kind] {
		fmt.Fprintf(&lines, "  %s %s: %s, owed=%v, listed=%v (%s)\n",
			kind, item.id, item.state.name, item.state.owed, slices.Contains(listed, item.id), item.state.comment)
	}
	return lines.String()
}

// The answer is paged by id past after, limit ids at a time, and walking it a page at a time
// reaches every owed item once and in id order, which is the walk the window arm makes
// (docs.windowDrift).
func TestItemsOwedWindowsPagesTheOwedItemsByID(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		for _, limit := range []int{1, 2, 3} {
			var walked []string
			after := ""
			for range len(fixture.owedIDs(kind)) + 2 {
				page, err := ItemsOwedWindows(ctx, fixture.database.Pool, kind, after, limit)
				if err != nil {
					t.Fatalf("page the %ss owed windows after %q: %v", kind, after, err)
				}
				if len(page) == 0 {
					break
				}
				if len(page) > limit {
					t.Fatalf("a page of %ss owed windows holds %d ids, more than the limit of %d", kind, len(page), limit)
				}
				walked = append(walked, page...)
				after = page[len(page)-1]
			}
			if want := fixture.owedIDs(kind); !slices.Equal(walked, want) {
				t.Errorf("walking the %ss owed windows %d at a time reaches %v, want %v", kind, limit, walked, want)
			}
		}
	}
}

// The marker read answers what both shapes before it answered, over every state of the fixture
// and from every cursor the window arm can hold: each id of the fixture is taken as `after` in
// turn, at three limits, and the three statements are read side by side. Reading a marker
// instead of state is where such a change goes wrong, so the answers are compared against the
// texts of the readings this replaced rather than against what this test thinks the answer is.
func TestItemsOwedWindowsAnswersWhatTheShapesBeforeTheMarkerAnswered(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		tables, err := kind.tables()
		if err != nil {
			t.Fatalf("tables of %s: %v", kind, err)
		}
		oracles := map[string]string{
			"two-not-exists": itemsOwedWindowsTwoNotExists(kind, tables),
			"anti-join":      itemsOwedWindowsAntiJoins(kind, tables),
		}
		cursors := []string{""}
		for _, item := range fixture.items[kind] {
			cursors = append(cursors, item.id)
		}
		for _, after := range cursors {
			for _, limit := range []int{1, 2, 1000} {
				listed, err := ItemsOwedWindows(ctx, fixture.database.Pool, kind, after, limit)
				if err != nil {
					t.Fatalf("list the %ss owed windows after %q: %v", kind, after, err)
				}
				for shape, oracle := range oracles {
					rows, err := fixture.database.Pool.Query(ctx, oracle, uuidAfter(after), limit)
					if err != nil {
						t.Fatalf("read the %s shape over %ss after %q: %v", shape, kind, after, err)
					}
					want, err := pgx.CollectRows(rows, pgx.RowTo[string])
					if err != nil {
						t.Fatalf("read the %s shape over %ss after %q: %v", shape, kind, after, err)
					}
					if !slices.Equal(listed, want) {
						t.Errorf("the %ss owed windows after %q, %d at a time, are %v, where the %s shape this replaced answers %v:\n%s",
							kind, after, limit, listed, shape, want, fixture.describe(kind, listed))
					}
				}
			}
		}
	}
}

// windowPlanSettings is what the plans below are read under. Sequential scans are off, as every
// plan test of this package reads its plan, so each side the statement reads is named by the
// index it is read through rather than by a scan of the whole table.
var windowPlanSettings = []string{"set local enable_seqscan = off"}

// Each kind's listing of the items owed their windows, held to that kind's own partial index.
func TestItemsOwedWindowsReadsThePartialIndex(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	keys := explaintest.NewIndexKeys(fixture.database.Pool)

	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		tables, err := kind.tables()
		if err != nil {
			t.Fatalf("tables of %s: %v", kind, err)
		}
		requireOwedWindowsReadsThePartialIndex(t, connection, keys, owedWindowsRead{
			subject:      fmt.Sprintf("the %ss owed windows", kind),
			statement:    itemsOwedWindowsQuery(tables),
			control:      itemsOwedWindowsAntiJoins(kind, tables),
			driving:      tables.state,
			blocks:       tables.blocks,
			owedIndex:    tables.state + "_windows_owed",
			otherIndexes: []string{tables.state + "_pkey", tables.state + "_owed"},
		})
	}
}

// owedWindowsRead is one listing of the owners owed their windows, as the plan test reads it:
// subject names the read in a failure, statement is the listing itself and control the shape
// before the marker over the same rows, driving is the relation the listing walks and blocks
// the one it probes for the owners that relation answered, owedIndex is the partial index on
// windows_owed_at the listing must be read through, and otherIndexes are the indexes of
// driving it must not search.
type owedWindowsRead struct {
	subject, statement, control string
	driving, blocks, owedIndex  string
	otherIndexes                []string
}

// windowStateRelations is what the shape before the marker reads for every row of the driving
// relation and the marker read answers without: the stamp of the state each owner's windows
// were written from, and the window rows themselves.
var windowStateRelations = []string{"embedding_window_states", "embeddings"}

// requireOwedWindowsReadsThePartialIndex holds one owed-windows listing to the marker's partial
// index. The listing reads the owed rows through that index and touches nothing else of the
// deployment's size: one scan of the driving relation, through that index, running once, and no
// scan at all of the stamps or of the window rows, which is what the marker bought. The blocks
// are probed for the rows that index answered, not for every row of the driving relation.
//
// The control is the shape before the marker, read the same way over the same rows: it decides
// owedness from state, so its plan reads the driving relation through some other index and
// reads both the stamps and the window rows. A test whose control did not do that would be
// proving nothing about the index this one is for.
func requireOwedWindowsReadsThePartialIndex(
	t *testing.T, connection *pgxpool.Conn, keys *explaintest.IndexKeys, read owedWindowsRead,
) {
	t.Helper()
	plan := analyzedPlan(t, connection, read.statement, uuidAfter(""), blockIndexWindowBatch)
	scans := explaintest.Scans(t, plan)
	if under := namesUnderASubplan(scans); len(under) > 0 {
		t.Errorf("%s read %s under a subplan, so a side is probed per row:\n%s",
			read.subject, strings.Join(under, ", "), plan)
	}
	driving := readOnce(t, scans, plan, read.subject, read.driving)
	// The one index the read searches on the driving relation is the partial one: a bitmap scan
	// names it on its index node and the relation on its heap node, so the index is read from
	// the plan's index scans rather than from the scan of the relation.
	keys.RequireIndexKeyed(t, plan, read.owedIndex, read.subject, explaintest.CondRequired)
	for _, other := range read.otherIndexes {
		if searchesIndex(t, plan, other) {
			t.Errorf("%s searched %s, want %s, the partial index over the marked rows:\n%s",
				read.subject, other, read.owedIndex, plan)
		}
	}
	for _, relation := range windowStateRelations {
		for _, scan := range scans {
			if scan.Relation == relation {
				t.Errorf("%s read %s, which the marker is here to stop it reading:\n%s",
					read.subject, relation, plan)
			}
		}
	}
	for _, scan := range scans {
		if scan.Relation == read.blocks && scan.Loops > driving.Rows {
			t.Errorf("%s probe %s %.0f times for %.0f rows of %s, want at most one probe per marked row:\n%s",
				read.subject, scan.Relation, scan.Loops, driving.Rows, read.driving, plan)
		}
	}

	// The control: the shape before the marker, over the same rows.
	control := analyzedPlan(t, connection, read.control, uuidAfter(""), blockIndexWindowBatch)
	controlScans := explaintest.Scans(t, control)
	if searchesIndex(t, control, read.owedIndex) {
		t.Errorf("the shape before the marker searched %s for %s, so this test's control proves nothing:\n%s",
			read.owedIndex, read.subject, control)
	}
	for _, relation := range windowStateRelations {
		if !slices.ContainsFunc(controlScans, func(scan explaintest.Scan) bool { return scan.Relation == relation }) {
			t.Errorf("the shape before the marker read no %s for %s, so this test's control proves nothing:\n%s",
				relation, read.subject, control)
		}
	}
}

// searchesIndex reports whether plan reads through the index named index anywhere.
func searchesIndex(t *testing.T, plan json.RawMessage, index string) bool {
	t.Helper()
	return slices.ContainsFunc(explaintest.IndexScans(t, plan), func(scan explaintest.IndexScan) bool {
		return scan.Index == index
	})
}

// blockIndexWindowBatch is the batch the window arm asks each kind for
// (docs.blockIndexWindowBatch), the limit the plans are read at.
const blockIndexWindowBatch = 200

// readOnce is the one scan of plan that reads relation, which must run once: a relation read
// twice, or read once per row of something else, is a side this statement would be paying for
// per row. It fails the test and answers a zero scan when the plan reads relation any other
// number of times.
func readOnce(t *testing.T, scans []explaintest.Scan, plan json.RawMessage, subject, relation string) explaintest.Scan {
	t.Helper()
	var found []explaintest.Scan
	for _, scan := range scans {
		if scan.Relation == relation {
			found = append(found, scan)
		}
	}
	if len(found) != 1 {
		t.Errorf("%s read %s with %d scans, want one:\n%s", subject, relation, len(found), plan)
		return explaintest.Scan{}
	}
	if found[0].Loops != 1 {
		t.Errorf("%s read %s %.0f times, want once:\n%s", subject, relation, found[0].Loops, plan)
	}
	return found[0]
}

// namesUnderASubplan names every scan of a plan that runs under a subplan: the shape of a
// question asked per row of the scan driving it.
func namesUnderASubplan(scans []explaintest.Scan) []string {
	var names []string
	for _, scan := range scans {
		if scan.Subplan != "" {
			names = append(names, fmt.Sprintf("%s under %s", scan.Relation, scan.Subplan))
		}
	}
	return names
}

// analyzedPlan is the plan Postgres ran for statement, with what each node actually did: the
// loops of a node are what tells a side read once from one probed per row, and no plan without
// ANALYZE carries them.
func analyzedPlan(t *testing.T, connection *pgxpool.Conn, statement string, args ...any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range windowPlanSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the owed-windows read: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}
