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

// itemsOwedWindowsTwoNotExists is the statement ItemsOwedWindows sent before INFRA-988: the same
// question asked as two correlated `not exists` probes under an `or`, which the planner can only
// run as two subplans, once per row of the kind's state table. It is kept here as the oracle the
// rewrite is held equal to (TestItemsOwedWindowsAnswersWhatTheTwoNotExistsShapeAnswered) and as
// the negative control of the plan test (TestItemsOwedWindowsReadsEachSideOnce), which is the
// shape measured at 13.7 s and 1,023,190 index searches over 511,595 comment state rows.
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

// windowState is one state an item's windows can be in, as the fixture writes it: whether the
// item holds a current block, what its state row's body_sha256 says, what the stamp of the state
// its windows were written from says (none: no row at all) and whether it holds a window row.
// owed is whether ItemsOwedWindows must list it.
type windowState struct {
	name    string
	block   blockRow
	sum     *string
	stamp   *string
	window  bool
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
// for every kind ItemsOwedWindows serves. The four the rewrite has to answer for are the first
// four; the rest are the rows an `or` rewritten wrong starts listing.
var windowStates = []windowState{{
	name:    "windows stand for the blocks",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(indexedSum),
	window:  true,
	owed:    false,
	comment: "the stamp is the state the blocks are in and a window stands under the owner key",
}, {
	name:    "no stamp",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  true,
	owed:    true,
	comment: "windows written before the stamps existed say nothing about the blocks they cover",
}, {
	name:    "the stamp stands at another state",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(otherSum),
	window:  true,
	owed:    true,
	comment: "a build without windows re-indexed the item, so its windows are of the old text",
}, {
	name:    "no window row",
	block:   currentBlock,
	sum:     new(indexedSum),
	stamp:   new(indexedSum),
	window:  false,
	owed:    true,
	comment: "the stamp stands but the window row is gone, so the windows are written again",
}, {
	name:    "no sum, and the stamp reads the empty sum",
	block:   currentBlock,
	sum:     nil,
	stamp:   new(""),
	window:  true,
	owed:    false,
	comment: "a state row with no sum stamps and compares as '' (itemBlockState's coalesce)",
}, {
	name:    "no sum, and the stamp reads a sum",
	block:   currentBlock,
	sum:     nil,
	stamp:   new(indexedSum),
	window:  true,
	owed:    true,
	comment: "the same coalesce the other way: '' is not the sum the windows were written from",
}, {
	name:    "the only block was removed",
	block:   removedBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
	owed:    false,
	comment: "no current block, so the index holds no block these windows could stand for",
}, {
	name:    "no block at all",
	block:   noBlock,
	sum:     new(indexedSum),
	stamp:   nil,
	window:  false,
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
// kind's trigger made (settled to state.sum, as an index that cleared owed_at leaves it), the
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
	execFixture(t, tx, "settle the block state of the "+string(kind),
		`update `+tables.state+` set body_sha256 = $2, owed_at = null where `+tables.item+` = $1::uuid`, id, state.sum)
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
// index has not reached, and one whose null sum the stamp reads as ” are each not.
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

// The rewrite answers what the two `not exists` clauses under their `or` answered, over every
// state of the fixture and from every cursor the window arm can hold: each id of the fixture is
// taken as `after` in turn, at three limits, and the two statements are read side by side. A
// rewritten predicate is where an `or` goes wrong, so the two texts are compared rather than the
// new one being compared to what this test thinks the answer is.
func TestItemsOwedWindowsAnswersWhatTheTwoNotExistsShapeAnswered(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		tables, err := kind.tables()
		if err != nil {
			t.Fatalf("tables of %s: %v", kind, err)
		}
		oracle := itemsOwedWindowsTwoNotExists(kind, tables)
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
				rows, err := fixture.database.Pool.Query(ctx, oracle, uuidAfter(after), limit)
				if err != nil {
					t.Fatalf("read the two-not-exists shape over %ss after %q: %v", kind, after, err)
				}
				want, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					t.Fatalf("read the two-not-exists shape over %ss after %q: %v", kind, after, err)
				}
				if !slices.Equal(listed, want) {
					t.Errorf("the %ss owed windows after %q, %d at a time, are %v, where the shape this replaced answers %v:\n%s",
						kind, after, limit, listed, want, fixture.describe(kind, listed))
				}
			}
		}
	}
}

// windowPlanSettings is what the plans below are read under. Sequential scans are off, as every
// plan test of this package reads its plan, so each side the statement reads is named by the
// index it is read through rather than by a scan of the whole table; hash and merge joins are
// left on, because joining each side once is what this statement is for.
var windowPlanSettings = []string{"set local enable_seqscan = off"}

// The statement reads each side once, through the indexes that already serve it, where the shape
// it replaced probed two of them once per row of the state table. The negative control is that
// shape itself: the same fixture, the same check, and it fails on the loops its subplans run.
func TestItemsOwedWindowsReadsEachSideOnce(t *testing.T) {
	fixture := newOwedWindowFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	firstKeys := explaintest.NewIndexKeys(fixture.database.Pool)

	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		tables, err := kind.tables()
		if err != nil {
			t.Fatalf("tables of %s: %v", kind, err)
		}
		plan := analyzedPlan(t, connection, itemsOwedWindowsQuery(kind, tables), uuidAfter(""), blockIndexWindowBatchForPlans)
		if repeated := nodesRunMoreThanOnce(t, plan); len(repeated) > 0 {
			t.Errorf("the %ss owed windows read %s more than once, so a side is probed per row rather than read once:\n%s",
				kind, strings.Join(repeated, ", "), plan)
		}
		for _, index := range []string{"embedding_window_states_pkey", "embeddings_window_owner"} {
			firstKeys.RequireIndexKeyed(t, plan, index,
				fmt.Sprintf("the %ss owed windows", kind), explaintest.CondOptional)
		}

		// The negative control: the shape this replaced, read the same way. Its subplans run
		// once per row of the state table, which is the cost INFRA-988 measured, so the check
		// above must name them. A rewrite that kept the old shape would pass the check above
		// only if this one stops holding.
		oracle := analyzedPlan(t, connection, itemsOwedWindowsTwoNotExists(kind, tables), uuidAfter(""), blockIndexWindowBatchForPlans)
		if repeated := nodesRunMoreThanOnce(t, oracle); len(repeated) == 0 {
			t.Errorf("the two-not-exists shape over %ss runs every node once, so this test's control proves nothing:\n%s", kind, oracle)
		}
	}
}

// blockIndexWindowBatchForPlans is the batch the window arm asks for (docs.blockIndexWindowBatch),
// the limit the plans below are read at.
const blockIndexWindowBatchForPlans = 200

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

// loopNode is what nodesRunMoreThanOnce reads of one node of an `EXPLAIN (ANALYZE, FORMAT JSON)`
// plan: what it is, the relation and index it names, how many times it ran, and its children.
type loopNode struct {
	NodeType    string          `json:"Node Type"`
	Relation    string          `json:"Relation Name"`
	Index       string          `json:"Index Name"`
	ActualLoops float64         `json:"Actual Loops"`
	Plans       json.RawMessage `json:"Plans"`
}

// nodesRunMoreThanOnce names every node of plan that ran more than once: a correlated subplan, or
// the inner side of a nested loop, which is a side read once per row of the side driving it. A
// statement written to read each side once holds none.
func nodesRunMoreThanOnce(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	var node loopNode
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("read a plan node: %v\n%s", err, plan)
	}
	var repeated []string
	if node.ActualLoops > 1 {
		name := node.NodeType
		if node.Relation != "" {
			name += " on " + node.Relation
		}
		if node.Index != "" {
			name += " using " + node.Index
		}
		repeated = append(repeated, fmt.Sprintf("%s (%.0f loops)", name, node.ActualLoops))
	}
	if len(node.Plans) == 0 {
		return repeated
	}
	var children []json.RawMessage
	if err := json.Unmarshal(node.Plans, &children); err != nil {
		t.Fatalf("read a plan node's children: %v\n%s", err, node.Plans)
	}
	for _, child := range children {
		repeated = append(repeated, nodesRunMoreThanOnce(t, child)...)
	}
	return repeated
}
