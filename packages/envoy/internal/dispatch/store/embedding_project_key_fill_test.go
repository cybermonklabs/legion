package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The log lines one pass of the fill writes, named here so a test reads the same strings the
// code writes and a reader of the server's log and a reader of these tests see one vocabulary.
// The fill's lease, and what one process's lease does to another process's pass, are api's
// (api.RunEmbeddingProjectKeyFill) and are proved there; what a pass says while it runs is
// proved here, where it is written.
const (
	fillBeganLine     = "dispatch: the embeddings project_key fill began"
	fillLabellingLine = "dispatch: the embeddings project_key fill is labelling"
	fillFinishedLine  = "dispatch: the embeddings project_key fill finished"
	fillNothingLine   = "dispatch: the embeddings project_key fill has nothing to do: every row stored before this release carries its project"
)

// seedUnlabelledEmbeddings stores issues issues and sets every embeddings row back to
// unlabelled, which is the one way a test can make rows that look stored before migration
// 20261109120000: 0073's trigger writes a row for each issue, and 20261109120000 has that
// trigger's function label it. It answers how many rows the fill has to label.
func seedUnlabelledEmbeddings(t *testing.T, database *Store, issues int) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('FILL', 'Fill')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'FILL-' || n, 'FILL', n, 'Issue ' || n, '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		  from generate_series(1, $1::int) n
	`, issues); err != nil {
		t.Fatalf("seed %d issues: %v", issues, err)
	}
	unlabelled, err := database.Pool.Exec(ctx, "update embeddings set project_key = null")
	if err != nil {
		t.Fatalf("unlabel the stored rows: %v", err)
	}
	if rows := unlabelled.RowsAffected(); rows != int64(issues) {
		t.Fatalf("%d embeddings rows were stored for %d issues; the trigger that writes them is what this fixture rests on", rows, issues)
	}
	return int64(issues)
}

// unlabelledEmbeddings is how many rows still carry no project.
func unlabelledEmbeddings(t *testing.T, database *Store) int64 {
	t.Helper()
	var rows int64
	if err := database.Pool.QueryRow(context.Background(),
		"select count(*) from embeddings where project_key is null").Scan(&rows); err != nil {
		t.Fatalf("count the unlabelled rows: %v", err)
	}
	return rows
}

// fillProgress is the cursor row of migration 20261109120100: where the next batch starts and
// whether the pass is finished.
func fillProgress(t *testing.T, database *Store) (kind, id string, done bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		"select kind, id, done from embeddings_project_key_backfill").Scan(&kind, &id, &done); err != nil {
		t.Fatalf("read the fill's progress: %v", err)
	}
	return kind, id, done
}

// loggedLines is every record of the fill's log with this message, in the order they were
// written. The test reads the records themselves rather than a rendering of them, so an
// attribute it asserts on is the value the code logged and not a formatting of it.
func loggedLines(records *logtest.Recorder, message string) []slog.Record {
	var matched []slog.Record
	for _, record := range records.Records() {
		if record.Message == message {
			matched = append(matched, record)
		}
	}
	return matched
}

// oneLine is the single record with this message, and fails the test when there is any other
// number of them: a pass that logged its finish twice is as wrong as one that logged it never.
func oneLine(t *testing.T, records *logtest.Recorder, message string) slog.Record {
	t.Helper()
	matched := loggedLines(records, message)
	if len(matched) != 1 {
		t.Fatalf("the fill logged %d lines reading %q, want exactly one", len(matched), message)
	}
	return matched[0]
}

// loggedAttr is the value a record carries under key, and fails the test when it carries none:
// an attribute a reader needs is part of the line, not a detail of it.
func loggedAttr(t *testing.T, record slog.Record, key string) slog.Value {
	t.Helper()
	var found slog.Value
	var ok bool
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			found, ok = attr.Value, true
			return false
		}
		return true
	})
	if !ok {
		t.Fatalf("the line %q carries no %s attribute", record.Message, key)
	}
	return found
}

// fillProgressEvery sets how often a running fill says where it has got to, for the length of
// the test: zero makes every batch but the last one log, so a test sees the line a deployment
// sees after half a minute of labelling without storing half a minute of rows.
func fillProgressEvery(t *testing.T, every time.Duration) {
	t.Helper()
	previous := embeddingProjectKeyProgressEvery
	t.Cleanup(func() { embeddingProjectKeyProgressEvery = previous })
	embeddingProjectKeyProgressEvery = every
}

// openFillTestStore is a database of its own carrying every migration, with issues issues
// stored and unlabelled.
func openFillTestStore(t *testing.T, issues int) (*Store, int64) {
	t.Helper()
	database := openEmptyTestStore(t)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database, seedUnlabelledEmbeddings(t, database, issues)
}

// An hour-long pass that logs once at its end tells a reader of a rolling deploy nothing while
// it runs: a log with no line of the fill in it reads the same whether the fill is working, has
// finished or never started. So the pass says it began and the cursor it begins from, says where
// it has got to while it runs, and says it finished with the rows it labelled and how long it
// took; and a pass with nothing left to do says that instead, in the one line it costs.
//
// It asserts on the records and their attributes, not on a rendering of them: the count a reader
// needs is the finish line's own rows attribute.
func TestTheEmbeddingProjectKeyFillLogsItsStartItsProgressAndItsFinish(t *testing.T) {
	ctx := context.Background()
	database, stored := openFillTestStore(t, EmbeddingProjectKeyBatch+50)
	// Every batch but the last logs, so the line a deployment sees every half minute is here
	// after three batches of rows.
	fillProgressEvery(t, 0)
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)

	if _, err := database.FillEmbeddingProjectKeys(ctx); err != nil {
		t.Fatalf("label the stored rows: %v", err)
	}

	began := oneLine(t, records, fillBeganLine)
	if from := loggedAttr(t, began, "from_kind").String(); from != "" {
		t.Errorf("the fill began from kind %q; a database no pass has reached begins from the empty cursor", from)
	}
	if from := loggedAttr(t, began, "from_id").String(); from != "" {
		t.Errorf("the fill began from id %q; a database no pass has reached begins from the empty cursor", from)
	}
	if batch := loggedAttr(t, began, "batch").Int64(); batch != EmbeddingProjectKeyBatch {
		t.Errorf("the fill said it labels in batches of %d, want %d", batch, EmbeddingProjectKeyBatch)
	}

	labelling := loggedLines(records, fillLabellingLine)
	if len(labelling) == 0 {
		t.Fatal("the fill logged nothing while it ran, so a reader of an hour of it sees nothing move")
	}
	var climbed int64
	for _, line := range labelling {
		rows := loggedAttr(t, line, "rows").Int64()
		if rows <= climbed {
			t.Errorf("a progress line reports %d rows after one reporting %d; the count is what shows the pass moving", rows, climbed)
		}
		climbed = rows
		if batches := loggedAttr(t, line, "batches").Int64(); batches <= 0 {
			t.Errorf("a progress line reports %d batches, want the batches it has committed", batches)
		}
		if elapsed := loggedAttr(t, line, "elapsed").Duration(); elapsed < 0 {
			t.Errorf("a progress line reports %v elapsed", elapsed)
		}
	}

	finished := oneLine(t, records, fillFinishedLine)
	if rows := loggedAttr(t, finished, "rows").Int64(); rows != stored {
		t.Errorf("the fill finished reporting %d rows, want the %d it was given", rows, stored)
	}
	if batches := loggedAttr(t, finished, "batches").Int64(); batches < 2 {
		t.Errorf("the fill finished reporting %d batches for %d rows in batches of %d, want the batches it committed",
			batches, stored, EmbeddingProjectKeyBatch)
	}
	if duration := loggedAttr(t, finished, "duration").Duration(); duration <= 0 {
		t.Errorf("the fill finished reporting a duration of %v", duration)
	}
	// A reader tells a running fill from a finished one by the order of the lines as much as by
	// their words, so every line of the pass stands before its finish, which is the pass's last
	// line of all.
	var last int
	for at, record := range records.Records() {
		switch record.Message {
		case fillBeganLine, fillLabellingLine:
			last = at
		case fillFinishedLine:
			if at < last {
				t.Errorf("the fill logged its finish at record %d and was still labelling at record %d", at, last)
			}
		}
	}

	// The boot after the one that filled: the progress row says the pass is finished, so it
	// reads that row, says so once, and touches no embeddings row.
	after := &logtest.Recorder{}
	restore := logtest.Swap(t, after)
	defer restore()
	if _, err := database.FillEmbeddingProjectKeys(ctx); err != nil {
		t.Fatalf("run the finished fill again: %v", err)
	}
	oneLine(t, after, fillNothingLine)
	for _, message := range []string{fillBeganLine, fillLabellingLine, fillFinishedLine} {
		if lines := loggedLines(after, message); len(lines) != 0 {
			t.Errorf("a fill with nothing to do logged %d lines reading %q, want none of them", len(lines), message)
		}
	}
}

// embeddingProjectKeyPlanRows is how many embeddings rows the batch's plan below is read over.
// It is some thousands because a plan is read over the statistics of the table it is read on: a
// table of a few hundred rows is a table Postgres will read whole whatever the statement says,
// and the shape this test exists to refuse - a side read again for every candidate row - costs
// nothing a measurement can see there. Twenty batches of rows is enough for the planner to
// prefer a keyset descent of embeddings_pkey to a scan of the table, which is the choice the
// deployment's 1,104,667 rows make it face, and is about three seconds of seeding.
const embeddingProjectKeyPlanRows = 4000

// fillEmbeddingProjectKeysCursorCTEQuery is the batch as it stood before the cursor was read in
// a statement of its own, and is this test's negative control: the shape whose plan the
// deployment measured at 126,769 ms for 200 rows
// (fillEmbeddingProjectKeysCursorQuery's comment). $1 is the batch size, and the cursor is the
// CTE the plan re-reads.
//
// A plan test of a statement is worth what its control is worth. Run as the subject of the
// checks below, this text fails them, and the failures name the defect in the deployment's own
// terms: at the cursor a pass begins from it scans the cursor CTE 200 times, once for each row
// the batch read, and from the middle of the table 2,201 times, taking 2,201 rows of
// embeddings_pkey for a batch of 200 because the planner cannot push a cursor it is not given
// into the index.
const fillEmbeddingProjectKeysCursorCTEQuery = `
	with progress as (
		select kind, id from embeddings_project_key_backfill for update
	), batch as (
		select e.kind, e.id
		  from embeddings e, progress
		 where (e.kind, e.id) > (progress.kind, progress.id)
		 order by e.kind, e.id
		 limit $1
	), last as (
		select b.kind, b.id from batch b order by b.kind desc, b.id desc limit 1
	), labelled as (
		update embeddings e
		   set project_key = embeddings_owner_project_key(e.kind, e.id)
		  from batch b
		 where e.kind = b.kind and e.id = b.id and e.project_key is null
		returning 1
	), moved as (
		update embeddings_project_key_backfill
		   set kind = coalesce((select kind from last), embeddings_project_key_backfill.kind),
		       id = coalesce((select id from last), embeddings_project_key_backfill.id),
		       done = not exists (select 1 from last)
		returning done
	)
	select (select count(*) from labelled), (select done from moved)`

// One batch of the fill costs what a keyset batch costs: one bounded descent of
// embeddings_pkey, and no side of the statement read again for each row that descent answers.
//
// This is a plan test and not a timing one because the defect it refuses hid from both a timing
// test and a reading: on a small warm database the shape this replaced labelled 2,200 rows in
// 30 s, and on the load-test deployment's 1,104,667 rows the same statement took 126,769 ms for
// one batch of 200 - eight days for the table - because the planner made the cursor the inner
// side of a nested loop and read it once per candidate row. What a wall clock sees of that
// depends on the database it runs on; what the plan says is the same everywhere.
//
// The batch is read at two cursors, the one a pass begins from and one in the middle of the
// table, which is where a pass spends nearly the whole of its life, and the control is read at
// both of them.
func TestTheEmbeddingProjectKeyBatchReadsOneBoundedIndexScan(t *testing.T) {
	ctx := context.Background()
	database, _ := openFillTestStore(t, embeddingProjectKeyPlanRows)
	// A plan is chosen on the statistics of the table, and a table just filled by a test has
	// none until it is analysed.
	if _, err := database.Pool.Exec(ctx, "analyze embeddings"); err != nil {
		t.Fatalf("analyse the rows the plans are read over: %v", err)
	}
	keys := explaintest.NewIndexKeys(database.Pool)

	for _, at := range []fillCursor{{"the cursor a pass begins from", "", ""}, middleFillCursor(t, database)} {
		plan := fillBatchPlan(t, database, at, fillEmbeddingProjectKeysBatchQuery, at.kind, at.id, EmbeddingProjectKeyBatch)
		t.Logf("the batch at %s:\n%s", at.what,
			fillBatchPlanText(t, database, at, fillEmbeddingProjectKeysBatchQuery, at.kind, at.id, EmbeddingProjectKeyBatch))
		subject := "the batch at " + at.what
		for _, node := range explaintest.Nodes(t, plan) {
			if node.Type == "CTE Scan" && node.Loops > 1 {
				t.Errorf("%s scanned the CTE %s %.0f times, which is once per row the batch read and not once for the batch:\n%s",
					subject, node.CTE, node.Loops, plan)
			}
		}
		for _, read := range embeddingsReads(t, plan) {
			switch {
			case read.Index == "":
				t.Errorf("%s read embeddings through no index (%s), so a batch costs a pass over the table and not a descent of its key:\n%s",
					subject, read, plan)
			case read.Rows > EmbeddingProjectKeyBatch:
				t.Errorf("%s took %.0f rows from %s for a batch of %d, so the cursor is a filter above the read rather than a bound inside it:\n%s",
					subject, read.Rows, read, EmbeddingProjectKeyBatch, plan)
			case read.Loops > EmbeddingProjectKeyBatch:
				t.Errorf("%s ran %s %.0f times for a batch of %d rows:\n%s",
					subject, read, read.Loops, EmbeddingProjectKeyBatch, plan)
			}
		}
		// The batch reaches embeddings through the table's own primary key, and a condition
		// Postgres wrote on a scan of that index names the key's first column: a scan whose
		// condition names something else reads the whole index and filters it. A scan carrying
		// no condition at all is left to the bounds above (explaintest.CondOptional), because
		// one such scan is right: at the start of the table the planner merges the batch
		// against embeddings_pkey from its beginning, which is the 200 rows the batch wants
		// and no more.
		keys.RequireIndexKeyed(t, plan, "embeddings_pkey", subject, explaintest.CondOptional)

		// The control: the shape this replaced, read at the same cursor over the same rows. A
		// plan of it that holds none of what the checks above refuse would leave them passing
		// over a statement that never had the defect, which is a test of nothing.
		control := fillBatchPlan(t, database, at, fillEmbeddingProjectKeysCursorCTEQuery, EmbeddingProjectKeyBatch)
		t.Logf("the shape this replaced, at %s:\n%s", at.what,
			fillBatchPlanText(t, database, at, fillEmbeddingProjectKeysCursorCTEQuery, EmbeddingProjectKeyBatch))
		rescanned := slices.ContainsFunc(explaintest.Nodes(t, control), func(node explaintest.Node) bool {
			return node.Type == "CTE Scan" && node.Loops > 1
		})
		unbounded := slices.ContainsFunc(embeddingsReads(t, control), func(read explaintest.Node) bool {
			return read.Rows > EmbeddingProjectKeyBatch
		})
		if !rescanned && !unbounded {
			t.Errorf("the shape this replaced neither re-read its cursor CTE nor read past the batch at %s, so this test's control proves nothing:\n%s",
				at.what, control)
		}
	}
}

// The plan above is the one Postgres builds knowing the cursor. The plan the fill runs on is
// not: pgx prepares and caches this statement, so from the sixth batch of a pass of thousands
// Postgres may settle on a generic plan, built with the cursor unknown. The cursor is bound as
// a parameter to make the planner read it as a bound, and a bound it cannot see is a bound it
// cannot push into the index - which is the whole of the defect this change ends - so the
// generic plan is held to the index condition too.
func TestTheEmbeddingProjectKeyBatchKeepsItsIndexConditionWhenPlannedWithoutTheCursor(t *testing.T) {
	ctx := context.Background()
	database, _ := openFillTestStore(t, embeddingProjectKeyPlanRows)
	if _, err := database.Pool.Exec(ctx, "analyze embeddings"); err != nil {
		t.Fatalf("analyse the rows the plan is read over: %v", err)
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()

	plan := explaintest.GenericPlan(t, ctx, connection, "embedding_project_key_batch",
		fillEmbeddingProjectKeysBatchQuery,
		[]string{"text", "text", "int"}, []string{"'issue'", "'FILL-1'", strconv.Itoa(EmbeddingProjectKeyBatch)},
		fillPlanSettings...)
	keys := explaintest.NewIndexKeys(database.Pool)
	keys.RequireIndexKeyed(t, plan, "embeddings_pkey", "the batch planned without its cursor", explaintest.CondOptional)
	keyed := slices.ContainsFunc(explaintest.IndexScans(t, plan), func(scan explaintest.IndexScan) bool {
		return scan.Index == "embeddings_pkey" && strings.Contains(scan.Cond, ">")
	})
	if !keyed {
		t.Errorf("the batch planned without its cursor reaches embeddings under no condition bounding it below, so a late batch walks the rows every batch before it labelled:\n%s", plan)
	}
}

// embeddingsReads is every node of plan that reads embeddings itself: the batch's descent of
// the key, the write's probe of each row that descent answered, and whatever else a planner
// might reach the table by. The nodes are taken by their type and not by the relation alone,
// because the write's own `Update on embeddings` node names the relation and reads nothing: a
// rule written over the relation alone would hold that node to the row count of a read.
func embeddingsReads(t *testing.T, plan json.RawMessage) []explaintest.Node {
	t.Helper()
	var reads []explaintest.Node
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation == "embeddings" && strings.HasSuffix(node.Type, "Scan") {
			reads = append(reads, node)
		}
	}
	return reads
}

// fillCursor is a place in embeddings' primary key order a batch is read from, with the name a
// failure calls it by.
type fillCursor struct{ what, kind, id string }

// middleFillCursor is the key halfway through the rows the test seeded, which is the kind of
// cursor a pass holds for all but its first and last batches.
func middleFillCursor(t *testing.T, database *Store) fillCursor {
	t.Helper()
	at := fillCursor{what: "a cursor in the middle of the table"}
	if err := database.Pool.QueryRow(context.Background(),
		"select kind, id from embeddings order by kind, id offset $1 limit 1", embeddingProjectKeyPlanRows/2).
		Scan(&at.kind, &at.id); err != nil {
		t.Fatalf("read the key halfway through the seeded rows: %v", err)
	}
	return at
}

// fillBatchPlan is the plan Postgres ran for one batch statement at the cursor at: its loops
// are what tells a cursor read once from one read again for every row, and no plan without
// ANALYZE carries them.
func fillBatchPlan(t *testing.T, database *Store, at fillCursor, statement string, args ...any) json.RawMessage {
	t.Helper()
	var raw []byte
	explainOneFillBatch(t, database, at, "analyze, format json", statement, args, func(rows pgx.Rows) {
		if !rows.Next() {
			t.Fatalf("explain the batch at %s: the explain answered no row", at.what)
		}
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("explain the batch at %s: %v", at.what, err)
		}
	})
	return explaintest.OnePlan(t, raw)
}

// fillBatchPlanText is the same plan as Postgres writes one for a person, with the buffers each
// node touched, which the test logs: a reader of a failure reads the plan in the form the
// deployment's own measurement is quoted in.
func fillBatchPlanText(t *testing.T, database *Store, at fillCursor, statement string, args ...any) string {
	t.Helper()
	var lines []string
	explainOneFillBatch(t, database, at, "analyze, buffers", statement, args, func(rows pgx.Rows) {
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("explain the batch at %s as text: %v", at.what, err)
			}
			lines = append(lines, line)
		}
	})
	return strings.Join(lines, "\n")
}

// fillPlanSettings is what the plans below are read under. Sequential scans are off, as in
// every plan test of refs (refs/item_windows_owed_test.go, windowPlanSettings): four thousand
// rows are few enough that Postgres will read the whole table for any of this statement's
// three reads of it, where a deployment's 1,104,667 are not, so each read is named by the
// index it is read through rather than by a scan whose cost the test's own size decides.
//
// It changes nothing about what this test is for. The defect is a side of the statement read
// once per candidate row, and that is a join order, not an access path: the shape this
// replaced re-reads its cursor under this setting exactly as the deployment measured it
// reading it.
var fillPlanSettings = []string{"set local enable_seqscan = off"}

// explainOneFillBatch explains one batch statement under options, with the fill's cursor row set
// to at, and gives the explain's rows to read. It runs in a transaction of its own that it rolls
// back: a batch is a write and ANALYZE runs it, so the rows it labels and the cursor it moves
// have to be undone before the next plan is read over the same table. The cursor is written even
// for the statement that takes it as a parameter, so the subject and its control are read at
// exactly the same place.
func explainOneFillBatch(
	t *testing.T, database *Store, at fillCursor, options, statement string, args []any, read func(pgx.Rows),
) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range fillPlanSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	if _, err := tx.Exec(ctx,
		"update embeddings_project_key_backfill set kind = $1, id = $2", at.kind, at.id); err != nil {
		t.Fatalf("set the cursor the plan is read at: %v", err)
	}
	rows, err := tx.Query(ctx, "explain ("+options+") "+statement, args...)
	if err != nil {
		t.Fatalf("explain the batch at %s: %v", at.what, err)
	}
	defer rows.Close()
	read(rows)
	if err := rows.Err(); err != nil {
		t.Fatalf("explain the batch at %s: %v", at.what, err)
	}
}
