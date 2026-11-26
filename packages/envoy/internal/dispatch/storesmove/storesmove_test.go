package storesmove

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// urlOf is the connection string of database, which the move opens a connection of its own to.
func urlOf(database *store.Store) string { return database.Pool.Config().ConnString() }

// rowsByNode is a store's rows by the node each is of ("<kind>/<id>"), each node's in the order
// of its seq, each row as the text of the columns the census compares.
type rowsByNode map[string][]string

// readRows reads query, which selects a node's address and a row's text, into rowsByNode.
func readRows(t *testing.T, pool *store.Pool, what, query string, args ...any) rowsByNode {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	defer rows.Close()
	read := rowsByNode{}
	for rows.Next() {
		var node, row string
		if err := rows.Scan(&node, &row); err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		read[node] = append(read[node], row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	return read
}

// oldTuples reads the four tables' tuples and oldNodes their node rows, before the move.
func oldTuples(t *testing.T, pool *store.Pool) rowsByNode {
	return readRows(t, pool, "the old tuples", `
		select from_kind || '/' || from_id, row(kind, to_kind, to_id, at, who, source_event, asserted)::text
		from (select * from priority_tuples union all select * from workflow_tuples) old
		order by from_kind, from_id, seq`)
}

func oldNodes(t *testing.T, pool *store.Pool) rowsByNode {
	return readRows(t, pool, "the old node rows", `
		select kind || '/' || id, row(supertag, visibility, state, attributes, made_at, made_by, source_event)::text
		from (select * from priority_nodes union all select * from workflow_nodes) old
		order by kind, id`)
}

// movedTuples reads the store's tuples of the three kinds, and the advances tuples, less the
// values the move states of each run and plan: its visibility, its state, and the project of each
// of noProject, whose old row held none. movedNodes reads the store's node rows of the three
// kinds.
func movedTuples(t *testing.T, pool *store.Pool, noProject []string) rowsByNode {
	return readRows(t, pool, "the moved tuples", `
		select from_kind || '/' || from_id, row(kind, to_kind, to_id, at, who, source_event, asserted)::text
		from graph_tuples
		where (from_kind in ('priority', 'step', 'plan') or to_kind = 'priority' and kind = 'advances')
		  and not (from_kind in ('step', 'plan') and to_kind = 'value' and kind in ('visibility', 'state'))
		  and not (to_kind = 'value' and kind = 'project' and from_id = any($1))
		order by from_kind, from_id, seq`, noProject)
}

func movedNodes(t *testing.T, pool *store.Pool) rowsByNode {
	return readRows(t, pool, "the moved node rows", `
		select kind || '/' || id, row(supertag, visibility, state, attributes, made_at, made_by, source_event)::text
		from graph_nodes where kind in ('priority', 'step', 'plan')
		order by kind, id`)
}

// requireSameRows fails naming each node whose rows differ between before and after.
func requireSameRows(t *testing.T, what string, before, after rowsByNode) {
	t.Helper()
	for node, rows := range before {
		if !slices.Equal(rows, after[node]) {
			t.Errorf("%s of %s: before the move %q, after it %q", what, node, rows, after[node])
		}
	}
	for node, rows := range after {
		if _, ok := before[node]; !ok {
			t.Errorf("%s of %s: none before the move, %q after it", what, node, rows)
		}
	}
}

// count is the integer query answers.
func count(t *testing.T, pool *store.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(query), err)
	}
	return n
}

// requireMovedShape fails unless the four names are views, the record is gone and the two
// triggers stand on graph_nodes; requireOldShape unless the four are tables and the record stands.
func requireMovedShape(t *testing.T, pool *store.Pool) {
	t.Helper()
	if n := count(t, pool, `select count(*) from pg_class where oid in (to_regclass('priority_nodes'), to_regclass('priority_tuples'), to_regclass('workflow_nodes'), to_regclass('workflow_tuples')) and relkind = 'v'`); n != 4 {
		t.Errorf("%d of the four names are views after the move, want 4", n)
	}
	if n := count(t, pool, `select count(*) from pg_class where oid = to_regclass('stores_moved_seqs')`); n != 0 {
		t.Errorf("stores_moved_seqs stands after the move, want it dropped")
	}
	if n := count(t, pool, `select count(*) from pg_trigger where tgrelid = 'graph_nodes'::regclass and tgname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project')`); n != 2 {
		t.Errorf("%d of the two triggers stand on graph_nodes after the move, want 2", n)
	}
}

func requireOldShape(t *testing.T, pool *store.Pool) {
	t.Helper()
	if n := count(t, pool, `select count(*) from pg_class where oid in (to_regclass('priority_nodes'), to_regclass('priority_tuples'), to_regclass('workflow_nodes'), to_regclass('workflow_tuples')) and relkind = 'r'`); n != 4 {
		t.Errorf("%d of the four names are tables, want 4: the move dropped what it must keep", n)
	}
	if n := count(t, pool, `select count(*) from pg_class where oid = to_regclass('stores_moved_seqs')`); n != 1 {
		t.Errorf("stores_moved_seqs is gone, want it standing")
	}
}

// requireCensusOf fails unless the census reads no difference and counts what the old tables
// held: priority and workflow tuples and node rows.
func requireCensusOf(t *testing.T, census *Census, priorityTuples, priorityNodes, workflowTuples, workflowNodes int64) {
	t.Helper()
	if census == nil {
		t.Fatal("the move took no census before the drop")
	}
	for _, line := range census.Lines() {
		t.Logf("census before the drop: %s", line)
	}
	if census.Differs() {
		t.Fatalf("the census before the drop read a difference: %s", census.First())
	}
	want := map[string][2]int64{"priority": {priorityTuples, priorityNodes}, "workflow": {workflowTuples, workflowNodes}}
	for _, taken := range census.Stores {
		if got := [2]int64{taken.Tuples.Old.Rows, taken.Nodes.Old.Rows}; got != want[taken.Store] {
			t.Errorf("the census counts %d tuples and %d node rows of the %s store, want %d and %d",
				got[0], got[1], taken.Store, want[taken.Store][0], want[taken.Store][1])
		}
		if taken.Tuples.Old != taken.Tuples.Moved || taken.Nodes.Old != taken.Nodes.Moved {
			t.Errorf("the census of the %s store reads the two sides with other counts or digests: %+v", taken.Store, taken)
		}
	}
}

// TestTheMoveCarriesEveryRowOfTheOldTablesIntoTheStore moves a database whose four tables hold
// rows the release before wrote, in its shapes (storesmovetest.Write), in batches of 3. After it,
// the census before the drop read no difference and counted every old row, each old tuple is a
// tuple of the store, among its node's in the order of its old seq, each old node row a node row
// with the same columns, each run and plan holds its visibility and state as one value tuple each,
// dated at its made_at, and its project_key, every tuple the move appended stands above the
// greatest old seq of priority_tuples, the four names are views, the record is gone, the two
// triggers stand, and graph_tuples_kinds holds (issue, advances, priority) in pg_stats_ext's
// most_common_vals from the move's analyze.
func TestTheMoveCarriesEveryRowOfTheOldTablesIntoTheStore(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	fixtures := storesmovetest.Write(t, pool)
	tuplesBefore, nodesBefore := oldTuples(t, pool), oldNodes(t, pool)
	priorityTuples := count(t, pool, `select count(*) from priority_tuples`)
	workflowTuples := count(t, pool, `select count(*) from workflow_tuples`)
	greatest := count(t, pool, `select max(seq) from priority_tuples`)
	graphBefore := count(t, pool, `select coalesce(max(seq), 0) from graph_tuples`)

	result, err := Move(ctx, urlOf(database), Options{Batch: 3})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !result.Moved || result.Copied != int(priorityTuples+workflowTuples) || result.SwapCopied != 0 {
		t.Errorf("the move answered %+v, want it to have moved the %d old tuples in its copy and none in its swap", *result, priorityTuples+workflowTuples)
	}
	requireCensusOf(t, result.Census, priorityTuples, int64(len(fixtures.Priorities)), workflowTuples, int64(len(fixtures.Runs)+len(fixtures.Plans)))
	requireSameRows(t, "the tuples", tuplesBefore, movedTuples(t, pool, []string{fixtures.RunWithNoProject, fixtures.PlanWithNoProject}))
	requireSameRows(t, "the node rows", nodesBefore, movedNodes(t, pool))
	requireMovedShape(t, pool)

	for _, id := range append(slices.Clone(fixtures.Runs), fixtures.Plans...) {
		for _, attribute := range []string{"visibility", "state"} {
			if n := count(t, pool, `
				select count(*) from graph_tuples t join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id
				where t.from_id = $1 and t.kind = $2 and t.to_kind = 'value' and t.at = n.made_at and t.who = n.made_by
				  and t.to_id = case when $2 = 'visibility' then n.visibility else n.state end
			`, id, attribute); n != 1 {
				t.Errorf("%s holds %d %s tuples equal to its row's and dated at its made_at, want 1", id, n, attribute)
			}
		}
		if n := count(t, pool, `select count(*) from graph_nodes where id = $1 and project_key = $2`, id, fixtures.Project); n != 1 {
			t.Errorf("%s holds no project_key %s", id, fixtures.Project)
		}
	}
	if n := count(t, pool, `select count(*) from graph_tuples where seq > $1 and seq <= $2`, graphBefore, greatest); n != 0 {
		t.Errorf("%d tuples the move appended took a seq at or below %d, the greatest seq of priority_tuples", n, greatest)
	}
	var state string
	if err := pool.QueryRow(ctx, `select state from graph_nodes where kind = 'priority' and id = $1`, fixtures.Deleted).Scan(&state); err != nil || state != "gone" {
		t.Errorf("the deleted priority %s reads state %q (%v), want gone", fixtures.Deleted, state, err)
	}
	stats := storesmovetest.ReadKindsStatistics(t, pool)
	t.Logf("after the move: %s", stats)
	if !stats.Analyzed || !stats.HasAdvances || stats.AdvancesFreq <= 0 {
		t.Errorf("after the move: %s; want non-null most_common_vals holding (issue, advances, priority)", stats)
	}
}

// TestAStoppedMoveResumesFromItsRecordAndCopiesNoTupleTwice stops a move after its third batch of
// two (DISPATCH_TEST_STORES_MOVE_BATCH, DISPATCH_TEST_STORES_MOVE_STOP_AFTER): the four tables
// stand, the record holds the six tuples the three batches moved, and the store holds those six.
// A second move then resumes from the record, and every old tuple is one tuple of the store, none
// twice.
func TestAStoppedMoveResumesFromItsRecordAndCopiesNoTupleTwice(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	fixtures := storesmovetest.Write(t, pool)
	tuplesBefore, nodesBefore := oldTuples(t, pool), oldNodes(t, pool)

	result, err := Move(ctx, urlOf(database), Options{Batch: 2, StopAfter: 3})
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("a move stopped after three batches answered %v (%+v), want ErrStopped", err, result)
	}
	requireOldShape(t, pool)
	if n := count(t, pool, `select count(*) from stores_moved_seqs`); n != 6 {
		t.Errorf("the record holds %d tuples after three batches of two, want 6", n)
	}
	if n := count(t, pool, `
		select count(*) from graph_tuples
		where from_kind in ('priority', 'step', 'plan') and not (from_kind in ('step', 'plan') and to_kind = 'value' and kind in ('visibility', 'state'))
	`); n != 6 {
		t.Errorf("the store holds %d moved tuples after three batches of two, want 6", n)
	}

	result, err = Move(ctx, urlOf(database), Options{Batch: 2})
	if err != nil {
		t.Fatalf("resume the move: %v", err)
	}
	if want := sumRows(tuplesBefore, "priority/", "issue/", "step/", "plan/") - 6; result.Copied != want {
		t.Errorf("the resumed move copied %d tuples, want the %d the record did not hold", result.Copied, want)
	}
	requireCensusOf(t, result.Census, int64(sumRows(tuplesBefore, "priority/", "issue/")), int64(len(fixtures.Priorities)),
		int64(sumRows(tuplesBefore, "step/", "plan/")), int64(len(fixtures.Runs)+len(fixtures.Plans)))
	requireSameRows(t, "the tuples", tuplesBefore, movedTuples(t, pool, []string{fixtures.RunWithNoProject, fixtures.PlanWithNoProject}))
	requireSameRows(t, "the node rows", nodesBefore, movedNodes(t, pool))
	requireMovedShape(t, pool)
}

// sumRows is how many rows rows holds of the nodes whose address starts with one of prefixes.
func sumRows(rows rowsByNode, prefixes ...string) int {
	n := 0
	for node, held := range rows {
		if slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(node, prefix) }) {
			n += len(held)
		}
	}
	return n
}

// TestTheCensusRefusesADifferenceAndDropsNothing plants three differences, each in a database of
// its own, and holds the move to refusing each with a *DifferenceError that names its store and
// the row by its address, with the four tables and the record standing: an old tuple the record
// holds that no batch moved, a tuple of the store out of a priority that no old tuple is, and a
// plan whose issue attribute names another issue than its part_of.
func TestTheCensusRefusesADifferenceAndDropsNothing(t *testing.T) {
	for _, planted := range []struct {
		name  string
		plant func(t *testing.T, pool *store.Pool, fixtures storesmovetest.Fixtures) (store, kind, id string, oldSeq int64)
	}{
		{"an old tuple recorded and never moved", func(t *testing.T, pool *store.Pool, fixtures storesmovetest.Fixtures) (string, string, string, int64) {
			seq := count(t, pool, `select min(seq) from priority_tuples where from_id = 'keep-it-up' and kind = 'rank'`)
			if _, err := pool.Exec(context.Background(), `insert into stores_moved_seqs (store, seq) values ('priority', $1)`, seq); err != nil {
				t.Fatalf("record a tuple as moved: %v", err)
			}
			return "priority", "priority", "keep-it-up", seq
		}},
		{"a tuple of the store no old tuple is", func(t *testing.T, pool *store.Pool, fixtures storesmovetest.Fixtures) (string, string, string, int64) {
			if _, err := pool.Exec(context.Background(), `
				insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
				values ('priority', 'ship-the-thing', 'title', 'value', 'Planted', now(), '{"kind":"user","id":"mallory"}', 0, true)
			`); err != nil {
				t.Fatalf("plant a tuple: %v", err)
			}
			return "priority", "priority", "ship-the-thing", 0
		}},
		{"a plan whose issue attribute names another issue than its part_of", func(t *testing.T, pool *store.Pool, fixtures storesmovetest.Fixtures) (string, string, string, int64) {
			id := "0f1e2d3c-0000-4000-8000-0000000000d1"
			storesmovetest.PutNode(t, pool, "plan", id, "MOVE-1", []storesmovetest.Attribute{
				storesmovetest.Text("issue", "MOVE-2"), storesmovetest.Text("project", fixtures.Project), storesmovetest.Text("source", "default"),
			}, nil, fixtures.Who, fixtures.At)
			return "workflow", "plan", id, 0
		}},
	} {
		t.Run(planted.name, func(t *testing.T) {
			database := storetest.Open(t)
			pool := database.Pool
			fixtures := storesmovetest.Write(t, pool)
			wantStore, wantKind, wantID, wantSeq := planted.plant(t, pool, fixtures)
			_, err := Move(context.Background(), urlOf(database), Options{})
			var differs *DifferenceError
			if !errors.As(err, &differs) {
				t.Fatalf("the move over %s answered %v, want a *DifferenceError", planted.name, err)
			}
			t.Logf("the move refused: %v", err)
			first := differs.Census.First()
			if first.Store != wantStore || first.Kind != wantKind || first.ID != wantID {
				t.Errorf("the first difference is %s, want one of the %s store at %s/%s", first, wantStore, wantKind, wantID)
			}
			if wantSeq != 0 && (first.OldSeq == nil || *first.OldSeq != wantSeq) {
				t.Errorf("the first difference is %s, want it to name old seq %d", first, wantSeq)
			}
			requireOldShape(t, pool)
			if n := count(t, pool, `select count(*) from pg_trigger where tgname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project')`); n != 0 {
				t.Errorf("%d of the two triggers stand after a refused swap, want none", n)
			}
		})
	}
}

// TestTheSwapMovesATupleWrittenAfterTheCopy runs the copy, then writes in the old tables what the
// release before writes before its locks give way to the swap's: a new priority and an advances
// relation on it, and run A of an issue, whose seqs lie below run B's, which committed before the
// copy, so the copy passed above them. The swap moves each, as it moves every old tuple the record
// lacks, not only those above the greatest seq it holds, and its census reads no difference.
func TestTheSwapMovesATupleWrittenAfterTheCopy(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	fixtures := storesmovetest.Write(t, pool)
	who, at := fixtures.Who, fixtures.At.Add(time.Hour)
	// Run A takes its seq before run B does, and commits after the copy.
	reserved := count(t, pool, `select nextval(pg_get_serial_sequence('workflow_tuples', 'seq'))`)
	runB := "0f1e2d3c-0000-4000-8000-0000000000e2"
	storesmovetest.PutNode(t, pool, "step", runB, "MOVE-2", []storesmovetest.Attribute{
		storesmovetest.Text("issue", "MOVE-2"), storesmovetest.Text("project", fixtures.Project), storesmovetest.Text("kind", "step"),
	}, nil, who, at)

	conn, err := store.Connect(ctx, urlOf(database), "stores-moved-test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if err := takeLock(ctx, conn); err != nil {
		t.Fatal(err)
	}
	m := newMover(Options{})
	copied, _, err := m.copy(ctx, conn)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}

	storesmovetest.CreatePriority(t, pool, "after-the-copy", map[string]string{"title": "After the copy", "rank": "b0"}, who, at)
	storesmovetest.Advance(t, pool, "MOVE-3", "after-the-copy", true, who, at)
	runA := "0f1e2d3c-0000-4000-8000-0000000000e1"
	if _, err := pool.Exec(ctx, `
		insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('step', $1, 'step', 'project', 'live', '{"issue":"MOVE-2","project":"MOVE","kind":"step"}', $2, $3::jsonb, 0)
	`, runA, at, `{"kind":"user","id":"alice@example.com","proven":true}`); err != nil {
		t.Fatalf("write run A: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into workflow_tuples (seq, from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, 'step', $2, 'part_of', 'issue', 'MOVE-2', $3, $4::jsonb, 0, true)
	`, reserved, runA, at, `{"kind":"user","id":"alice@example.com","proven":true}`); err != nil {
		t.Fatalf("write run A's tuple at its seq: %v", err)
	}
	tuplesBefore := oldTuples(t, pool)

	result := &Result{}
	if err := m.swapWithRetries(ctx, conn, result); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if result.SwapCopied != 6 {
		t.Errorf("the swap moved %d tuples after a copy of %d, want the 6 written after it: the priority's four, its advances and run A's part_of", result.SwapCopied, copied)
	}
	requireCensusOf(t, result.Census, int64(sumRows(tuplesBefore, "priority/", "issue/")), int64(len(fixtures.Priorities)+1),
		int64(sumRows(tuplesBefore, "step/", "plan/")), int64(len(fixtures.Runs)+len(fixtures.Plans)+2))
	if n := count(t, pool, `select count(*) from graph_tuples where from_kind = 'step' and from_id = $1 and kind = 'part_of'`, runA); n != 1 {
		t.Errorf("run A holds %d part_of tuples in the store, want 1", n)
	}
	requireMovedShape(t, pool)
}

// TestTheMoveStatesTheIssueProjectOfARunWithNone moves a run and a plan whose rows hold no
// project attribute, as a daemon that named none wrote them, and reads for each a project value
// tuple equal to its issue's project, dated at the node's made_at, and a project_key equal to it;
// a run whose row holds one holds its own project tuple alone.
func TestTheMoveStatesTheIssueProjectOfARunWithNone(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	fixtures := storesmovetest.Write(t, pool)
	if _, err := Move(ctx, urlOf(database), Options{}); err != nil {
		t.Fatalf("move: %v", err)
	}
	for _, id := range []string{fixtures.RunWithNoProject, fixtures.PlanWithNoProject} {
		var project, key string
		var at, madeAt time.Time
		if err := pool.QueryRow(ctx, `
			select t.to_id, t.at, n.made_at, coalesce(n.project_key, '')
			from graph_nodes n join graph_tuples t on t.from_kind = n.kind and t.from_id = n.id
			where n.id = $1 and t.kind = 'project' and t.to_kind = 'value'
		`, id).Scan(&project, &at, &madeAt, &key); err != nil {
			t.Fatalf("read the project %s holds: %v", id, err)
		}
		if project != fixtures.Project || !at.Equal(madeAt) || key != fixtures.Project {
			t.Errorf("%s holds project %q at %v, made at %v, with project_key %q; want its issue's project %s at its made_at, as its project_key too",
				id, project, at, madeAt, key, fixtures.Project)
		}
	}
	if n := count(t, pool, `select count(*) from graph_tuples where from_id = $1 and kind = 'project'`, fixtures.Runs[0]); n != 1 {
		t.Errorf("%s, whose row holds a project, holds %d project tuples, want its own one", fixtures.Runs[0], n)
	}
}

// TestTheTriggersCompleteAnUnmarkedWriteThroughTheViewsAndPassOverAMarkedOne writes, after the
// move, through the four views as the release before writes: on a connection without the writer
// mark (store.StoresMovedWriterSetting), a run written twice, at its start and its end, takes one
// visibility and one state tuple, dated at its made_at, and the project_key its project attribute
// names; a priority and its advances reach the store, each tuple taking a seq of the graph's own.
// The same run written on a connection with the mark takes neither tuple and no project_key.
func TestTheTriggersCompleteAnUnmarkedWriteThroughTheViewsAndPassOverAMarkedOne(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	fixtures := storesmovetest.Write(t, pool)
	if _, err := Move(ctx, urlOf(database), Options{}); err != nil {
		t.Fatalf("move: %v", err)
	}
	unmarked, err := pgx.Connect(ctx, urlOf(database))
	if err != nil {
		t.Fatalf("connect with no mark: %v", err)
	}
	defer unmarked.Close(context.Background())
	marked, err := store.Connect(ctx, urlOf(database), "stores-moved-test")
	if err != nil {
		t.Fatalf("connect with the mark: %v", err)
	}
	defer marked.Close(context.Background())

	daemon := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	at := fixtures.At.Add(2 * time.Hour)
	write := func(db storesmovetest.Execer, id string) {
		storesmovetest.PutNode(t, db, "step", id, "MOVE-2", []storesmovetest.Attribute{
			storesmovetest.Text("issue", "MOVE-2"), storesmovetest.Text("project", fixtures.Project),
			storesmovetest.Text("kind", "step"), storesmovetest.Text("role", "planner"),
		}, nil, daemon, at)
		storesmovetest.PutNode(t, db, "step", id, "MOVE-2", []storesmovetest.Attribute{
			storesmovetest.Text("issue", "MOVE-2"), storesmovetest.Text("project", fixtures.Project),
			storesmovetest.Text("kind", "step"), storesmovetest.Text("role", "planner"), storesmovetest.Text("result", "passed"),
		}, nil, daemon, at.Add(time.Minute))
	}
	stated := func(id string) (visibility, state int64, project string) {
		visibility = count(t, pool, `select count(*) from graph_tuples t join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id where t.from_id = $1 and t.kind = 'visibility' and t.at = n.made_at`, id)
		state = count(t, pool, `select count(*) from graph_tuples t join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id where t.from_id = $1 and t.kind = 'state' and t.at = n.made_at`, id)
		if err := pool.QueryRow(ctx, `select coalesce(project_key, '') from graph_nodes where kind = 'step' and id = $1`, id).Scan(&project); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return visibility, state, project
	}

	unmarkedRun := "0f1e2d3c-0000-4000-8000-0000000000f1"
	write(unmarked, unmarkedRun)
	if visibility, state, project := stated(unmarkedRun); visibility != 1 || state != 1 || project != fixtures.Project {
		t.Errorf("a run the release before writes twice through the views holds %d visibility and %d state tuples at its made_at and project_key %q, want 1, 1 and %s",
			visibility, state, project, fixtures.Project)
	}
	markedRun := "0f1e2d3c-0000-4000-8000-0000000000f2"
	write(marked, markedRun)
	if visibility, state, project := stated(markedRun); visibility != 0 || state != 0 || project != "" {
		t.Errorf("a run a marked connection writes through the views holds %d visibility and %d state tuples and project_key %q, want none of them: the triggers act on unmarked writes alone",
			visibility, state, project)
	}

	graphBefore := count(t, pool, `select max(seq) from graph_tuples`)
	storesmovetest.CreatePriority(t, unmarked, "through-the-view", map[string]string{"title": "Through the view", "rank": "c0"}, fixtures.Who, at)
	storesmovetest.Advance(t, unmarked, "MOVE-1", "through-the-view", true, fixtures.Who, at)
	if n := count(t, pool, `select count(*) from graph_nodes where kind = 'priority' and id = 'through-the-view'`); n != 1 {
		t.Errorf("a priority written through priority_nodes stands as %d node rows, want 1", n)
	}
	if n := count(t, pool, `select count(*) from graph_tuples where seq > $1 and (from_id = 'through-the-view' or to_id = 'through-the-view')`, graphBefore); n != 5 {
		t.Errorf("a priority written through priority_tuples and its advances hold %d tuples above the graph's seq %d, want 5", n, graphBefore)
	}
}

// TestABootOverMovedStoresMovesNothing moves a database, then boots the move again: it finds the
// four names views and changes no row of graph_nodes or graph_tuples.
func TestABootOverMovedStoresMovesNothing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	storesmovetest.Write(t, pool)
	if _, err := Move(ctx, urlOf(database), Options{}); err != nil {
		t.Fatalf("move: %v", err)
	}
	digest := func() string {
		var nodes, tuples string
		if err := pool.QueryRow(ctx, `
			select (select md5(string_agg(row(g.*)::text, E'\n' order by kind, id)) from graph_nodes g),
			       (select md5(string_agg(row(g.*)::text, E'\n' order by seq)) from graph_tuples g)
		`).Scan(&nodes, &tuples); err != nil {
			t.Fatalf("digest the store: %v", err)
		}
		return nodes + " " + tuples
	}
	before := digest()
	result, err := Move(ctx, urlOf(database), Options{})
	if err != nil {
		t.Fatalf("move again: %v", err)
	}
	if result.Moved || result.Copied != 0 {
		t.Errorf("a boot over moved stores answered %+v, want it to move nothing", *result)
	}
	if after := digest(); after != before {
		t.Errorf("a boot over moved stores changed the store: digests %s before, %s after", before, after)
	}
	requireMovedShape(t, pool)
}

// TestABootOverAnEmptyDatabaseSwaps moves a database the migrations built and nothing wrote: the
// four tables hold no row, the census counts none, and the swap still replaces them with the views
// and the triggers.
func TestABootOverAnEmptyDatabaseSwaps(t *testing.T) {
	database := storetest.Open(t)
	result, err := Move(context.Background(), urlOf(database), Options{})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !result.Moved || result.Copied != 0 || result.SwapCopied != 0 {
		t.Errorf("the move over an empty database answered %+v, want a swap that moved no tuple", *result)
	}
	requireCensusOf(t, result.Census, 0, 0, 0, 0)
	requireMovedShape(t, database.Pool)
}

// TestOwnMigrationsAreMigrationsOfTheStore holds the rehearsal's list of this issue's own
// migrations to the migrations this binary embeds, so a rename of one leaves no name the list
// holds and no file holds.
func TestOwnMigrationsAreMigrationsOfTheStore(t *testing.T) {
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range ownMigrations {
		if !slices.ContainsFunc(migrations, func(m pgmigrate.Migration) bool { return m.Name == name }) {
			t.Errorf("ownMigrations names %s, which the store embeds no migration of", name)
		}
	}
}
