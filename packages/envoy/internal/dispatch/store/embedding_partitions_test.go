package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The migrations that build embeddings partitioned by project beside the table and mirror its
// writes into it (20261120105330, 20261120105818).
const (
	embeddingsPartitionedVersion = 20261120105330
	embeddingsMirrorVersion      = 20261120105818
)

// The lines the partition half of the fill writes, as partitionEmbeddings writes them.
const (
	partitionCopyBeganLine    = "dispatch: the embeddings partition copy began"
	partitionCopyFinishedLine = "dispatch: the embeddings partition copy finished"
	partitionSwappedLine      = "dispatch: embeddings is partitioned by project: the copy is swapped in"
	partitionNothingLine      = "dispatch: embeddings is partitioned by project already: there is nothing to copy"
)

// openPartitionTestStore is a migrated database holding two projects, PARTA and PARTB, with
// issues issues each, whose rows the issue trigger wrote, and one window whose owner is gone,
// which belongs to no project.
func openPartitionTestStore(t *testing.T, issues int) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	execAll(t, database,
		`insert into projects (key, name, default_assignee) values ('PARTA', 'A', 'alice@d.example'), ('PARTB', 'B', 'alice@d.example')`,
		fmt.Sprintf(`insert into issues (key, project_key, number, title, created_by, rank)
			select p || '-' || n, p, n, 'Issue ' || n || ' of ' || p, '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
			  from unnest(array['PARTA', 'PARTB']) p cross join generate_series(1, %d) n`, issues),
		`update embeddings set embedding = array_fill(0.5::real, array[1536])::vector, model = 'dev:test', embedded_hash = content_hash
		  where kind = 'issue'`,
	)
	return database, seedOwnerlessWindow(t, database)
}

func execAll(t *testing.T, database *Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", strings.Join(strings.Fields(statement), " "), err)
		}
	}
}

// partitionOf is the table the row of kind and id is stored in.
func partitionOf(t *testing.T, database *Store, kind, id string) string {
	t.Helper()
	var table string
	if err := database.Pool.QueryRow(context.Background(),
		`select tableoid::regclass::text from embeddings where kind = $1 and id = $2`, kind, id).Scan(&table); err != nil {
		t.Fatalf("read the table %s %s is stored in: %v", kind, id, err)
	}
	return table
}

// embeddingsContent is every row of table, each as the text of all its columns, in key order:
// what a copy must carry over unchanged.
func embeddingsContent(t *testing.T, database *Store, table string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select (e.*)::text from `+table+` e order by kind, id`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var content []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		content = append(content, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return content
}

func requirePartitioned(t *testing.T, database *Store, want bool) {
	t.Helper()
	partitioned, err := database.embeddingsPartitioned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if partitioned != want {
		t.Fatalf("embeddings partitioned = %t, want %t", partitioned, want)
	}
}

// The fill's second half copies every stored row into the table partitioned by project, each into
// its project's partition and a row of no project into the partition of none, unchanged, and
// swaps that table in under embeddings' own name and index names; a later boot's fill finds
// nothing to copy. The writers then write the partitioned table: a title edit reaches its row
// through embeddings_enqueue's new key, and a project made after the swap gets its partition.
func TestTheFillCopiesEveryRowIntoItsProjectsPartitionAndSwapsItIn(t *testing.T) {
	ctx := context.Background()
	database, ownerless := openPartitionTestStore(t, EmbeddingPartitionCopyBatch+30)
	before := embeddingsContent(t, database, "embeddings")
	requirePartitioned(t, database, false)
	// The mirror carried every row of the fixture over as it was written. PARTB's rows, more than
	// a batch of them, and the row of no project are taken out of the table beside it again, so
	// the copy itself carries them over, and PARTA's are left there for the copy to meet.
	unmirrored, err := database.Pool.Exec(ctx, `delete from embeddings_partitioned where project_key = 'PARTB' or project_key is null`)
	if err != nil || unmirrored.RowsAffected() != EmbeddingPartitionCopyBatch+30+1 {
		t.Fatalf("took %d rows out of embeddings_partitioned (%v), want PARTB's %d and the row of no project",
			unmirrored.RowsAffected(), err, EmbeddingPartitionCopyBatch+30)
	}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)

	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("fill: %v", err)
	}

	requirePartitioned(t, database, true)
	if after := embeddingsContent(t, database, "embeddings"); !slices.Equal(after, before) {
		t.Errorf("the swapped-in table holds %d rows that differ from the %d stored before the copy", len(after), len(before))
	}
	for _, row := range []struct{ kind, id, table string }{
		{"issue", "PARTA-1", "embeddings_project_parta"},
		{"issue", "PARTB-7", "embeddings_project_partb"},
		{"comment_block", ownerless, "embeddings_no_project"},
	} {
		if table := partitionOf(t, database, row.kind, row.id); table != row.table {
			t.Errorf("%s %s is stored in %s, want %s", row.kind, row.id, table, row.table)
		}
	}
	var indexes []string
	if err := database.Pool.QueryRow(ctx, `select array_agg(indexrelid::regclass::text order by 1) from pg_index where indrelid = 'embeddings'::regclass`).
		Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	wantIndexes := []string{"embeddings_cosine", "embeddings_key", "embeddings_kind_project", "embeddings_pending",
		"embeddings_window_embedded", "embeddings_window_owner", "embeddings_window_unembedded"}
	if slices.Sort(indexes); !slices.Equal(indexes, wantIndexes) {
		t.Errorf("embeddings' indexes are %v, want %v", indexes, wantIndexes)
	}
	for _, gone := range []string{"embeddings_partitioned", "embeddings_partitioned_copy"} {
		var found *string
		if err := database.Pool.QueryRow(ctx, `select to_regclass($1)::text`, gone).Scan(&found); err != nil || found != nil {
			t.Errorf("%s is still there after the swap (%v, %v)", gone, found, err)
		}
	}
	oneLine(t, records, partitionCopyBeganLine)
	finishedLine := oneLine(t, records, partitionCopyFinishedLine)
	if rows := loggedAttr(t, finishedLine, "rows").Int64(); rows != int64(len(before)) {
		t.Errorf("the copy finished reporting %d rows read, want the %d stored", rows, len(before))
	}
	if inserted := loggedAttr(t, finishedLine, "inserted").Int64(); inserted != unmirrored.RowsAffected() {
		t.Errorf("the copy finished reporting %d rows inserted, want the %d the mirror had not carried over", inserted, unmirrored.RowsAffected())
	}
	oneLine(t, records, partitionSwappedLine)
	// The swap analyzes the partitioned parent, which autovacuum never does, so the planner has
	// statistics of embeddings itself and not of its partitions alone.
	oneLine(t, records, "dispatch: embeddings is analyzed after its swap")
	var parentStatistics int
	if err := database.Pool.QueryRow(ctx, `select count(*) from pg_statistic where starelid = 'embeddings'::regclass and stainherit`).
		Scan(&parentStatistics); err != nil || parentStatistics == 0 {
		t.Errorf("embeddings holds %d rows of statistics of its own after the swap (%v), want the ANALYZE's", parentStatistics, err)
	}

	// The writers, on the swapped-in table.
	execAll(t, database,
		`update issues set title = 'A new title' where key = 'PARTA-1'`,
		`insert into projects (key, name, default_assignee) values ('LATER', 'Later', 'alice@d.example')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LATER-1', 'LATER', 1, 'Later one', '{"kind":"user","id":"alice"}', 'a')`,
	)
	var text, hash, embedded string
	if err := database.Pool.QueryRow(ctx, `select text_snapshot, content_hash, coalesce(embedded_hash, '') from embeddings where kind = 'issue' and id = 'PARTA-1'`).
		Scan(&text, &hash, &embedded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "A new title") || hash == embedded {
		t.Errorf("after the title edit PARTA-1's row holds %q, hash %s embedded at %s: the enqueue did not meet the row on the partitioned key", text, hash, embedded)
	}
	if table := partitionOf(t, database, "issue", "LATER-1"); table != "embeddings_project_later" {
		t.Errorf("a project made after the swap stores its issue's row in %s, want embeddings_project_later", table)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings where kind = 'issue' and id = 'PARTA-1'`).Scan(&count); err != nil || count != 1 {
		t.Errorf("PARTA-1 has %d rows after its edit, want 1 (%v)", count, err)
	}

	after := &logtest.Recorder{}
	restore := logtest.Swap(t, after)
	defer restore()
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("fill again: %v", err)
	}
	oneLine(t, after, partitionNothingLine)
	if done, err := database.EmbeddingProjectKeyFillDone(ctx); err != nil || !done {
		t.Errorf("EmbeddingProjectKeyFillDone = %t, %v after the swap, want true", done, err)
	}
}

// Every partition of embeddings, and every index of one, reads in a plan as embeddings and as the
// index of embeddings it belongs to (explaintest.PartitionParent, PartitionParentIndex), which is
// what lets every plan guard written for embeddings hold the partitioned table: the names
// migration 20261120105330 gives them (embeddings_name_partition_indexes) are read against the
// catalog's own record of which parent each belongs to, for the partitions the swap brings and
// for one a project made after it gets.
func TestEveryIndexOfAPartitionOfEmbeddingsReadsAsTheIndexItBelongsTo(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 1)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("partition embeddings: %v", err)
	}
	execAll(t, database, `insert into projects (key, name, default_assignee) values ('LATER9', 'Later', 'alice@d.example')`)
	rows, err := database.Pool.Query(ctx, `
		select child.relname::text, parent.relname::text from pg_inherits link
		  join pg_class child on child.oid = link.inhrelid
		  join pg_class parent on parent.oid = link.inhparent
		 where parent.oid = 'embeddings'::regclass
		    or parent.oid in (select indexrelid from pg_index where indrelid = 'embeddings'::regclass)
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	type link struct{ child, parent string }
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (link, error) {
		var l link
		return l, row.Scan(&l.child, &l.parent)
	})
	if err != nil {
		t.Fatal(err)
	}
	var partitions, indexes int
	for _, l := range links {
		if l.parent == "embeddings" {
			partitions++
			if got := explaintest.PartitionParent(l.child); got != "embeddings" {
				t.Errorf("partition %s reads in a plan as %s, want embeddings", l.child, got)
			}
			continue
		}
		indexes++
		if got := explaintest.PartitionParentIndex(l.child); got != l.parent {
			t.Errorf("index %s of a partition reads in a plan as %s, want %s, the index of embeddings it belongs to", l.child, got, l.parent)
		}
	}
	// PARTA, PARTB, the partition of no project and LATER9's, and every index of each.
	if partitions != 4 || indexes != 4*7 {
		t.Errorf("the catalog names %d partitions and %d partition indexes of embeddings, want 4 and %d: the reading above proves nothing of the rest", partitions, indexes, 4*7)
	}
	for _, name := range []string{"embeddings", "embeddings_project_key_backfill", "embeddings_partitioned_copy", "embedding_window_states", "embeddings_pending", "embeddings_kind_project"} {
		if got := explaintest.PartitionParent(name); got != name {
			t.Errorf("%s, no partition, reads as %s", name, got)
		}
		if got := explaintest.PartitionParentIndex(name); got != name {
			t.Errorf("%s, no index of a partition, reads as the index %s", name, got)
		}
	}
}

// The labelling moves a row it labels from the partition of no project into its project's, by
// the UPDATE that labels it, on the partitioned table as on the one before it: a row stored before
// its project was known is in the partition of none, and the fill's pass is what puts it where a
// search of its project reads.
func TestTheFillMovesARowFromTheNoProjectPartitionIntoItsProjects(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 3)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("partition: %v", err)
	}
	// A row stored before its project was known: the label trigger would label it again as it
	// lands in the partition of none, so it is off while the row is made.
	execAll(t, database,
		`alter table embeddings disable trigger embeddings_project_key_label`,
		`update embeddings set project_key = null where kind = 'issue' and id = 'PARTB-2'`,
		`alter table embeddings enable trigger embeddings_project_key_label`,
		`update embeddings_project_key_backfill set kind = '', id = '', done = false`,
	)
	if table := partitionOf(t, database, "issue", "PARTB-2"); table != "embeddings_no_project" {
		t.Fatalf("PARTB-2 unlabelled is stored in %s, want embeddings_no_project", table)
	}
	labelled, err := database.FillEmbeddingProjectKeys(ctx, "")
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	if labelled != 1 {
		t.Errorf("the fill labelled %d rows, want PARTB-2's", labelled)
	}
	if table := partitionOf(t, database, "issue", "PARTB-2"); table != "embeddings_project_partb" {
		t.Errorf("PARTB-2 labelled is stored in %s, want embeddings_project_partb", table)
	}
}

// Every write of embeddings made while the copy runs reaches the table beside it: an insert, an
// update of the text, an update that unlabels a row and one that labels it, each moving it
// between partitions, a delete and a truncate. Compared column for column with the table written.
func TestTheMirrorCarriesEveryWriteMadeBesideTheCopy(t *testing.T) {
	database, ownerless := openPartitionTestStore(t, 2)
	requireMirrored := func(what string) {
		t.Helper()
		if stored, mirrored := embeddingsContent(t, database, "embeddings"), embeddingsContent(t, database, "embeddings_partitioned"); !slices.Equal(stored, mirrored) {
			t.Fatalf("after %s embeddings holds\n\t%v\nand embeddings_partitioned\n\t%v", what, stored, mirrored)
		}
	}
	requireMirrored("the seed")
	execAll(t, database, `update issues set title = 'Retitled' where key = 'PARTA-2'`)
	requireMirrored("a title edit")
	execAll(t, database, `update embeddings set project_key = null where id = 'PARTB-1'`)
	if table := partitionOfIn(t, database, "embeddings_partitioned", "PARTB-1"); table != "embeddings_no_project" {
		t.Errorf("PARTB-1 unlabelled is mirrored in %s, want embeddings_no_project", table)
	}
	execAll(t, database, `update embeddings set project_key = 'PARTB' where id = 'PARTB-1'`)
	if table := partitionOfIn(t, database, "embeddings_partitioned", "PARTB-1"); table != "embeddings_project_partb" {
		t.Errorf("PARTB-1 labelled again is mirrored in %s, want embeddings_project_partb", table)
	}
	requireMirrored("an unlabel and a label")
	execAll(t, database, fmt.Sprintf(`delete from embeddings where id = '%s'`, ownerless))
	requireMirrored("a delete")
	execAll(t, database, `truncate embeddings`)
	requireMirrored("a truncate")
}

func partitionOfIn(t *testing.T, database *Store, table, id string) string {
	t.Helper()
	var partition string
	if err := database.Pool.QueryRow(context.Background(),
		`select tableoid::regclass::text from `+table+` where id = $1`, id).Scan(&partition); err != nil {
		t.Fatalf("read where %s holds %s: %v", table, id, err)
	}
	return partition
}

// A copy batch holds the rows it read until it commits, so a delete of one of them waits for the
// batch and then removes the copied row too, where a delete the batch did not hold off would run
// before the batch's insert committed, find no copy to remove, and leave the batch to bring the
// row back. An update needs no such hold - it meets the batch's uncommitted copy on the unique key
// and waits there - so the row is held to a delete, the write only the batch's row lock orders.
// The control is the same delete against a batch that has committed, which does not wait.
func TestACopyBatchHoldsAWriteOfItsRowsUntilItCommits(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 2)
	execAll(t, database, `delete from embeddings_partitioned`)
	batch, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Rollback(ctx)
	var read, copied int64
	var finished bool
	if err := batch.QueryRow(ctx, copyEmbeddingsBatchQuery, "", "", EmbeddingPartitionCopyBatch).Scan(&read, &copied, &finished); err != nil {
		t.Fatalf("copy a batch: %v", err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := database.Pool.Exec(ctx, `delete from embeddings where id = 'PARTA-1'`)
		deleted <- err
	}()
	if waited := waitForALockWaiter(t, database, 5*time.Second); !waited {
		t.Error("the delete of a row the open batch read did not wait for the batch")
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("the delete: %v", err)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings_partitioned where id = 'PARTA-1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("embeddings_partitioned holds %d rows of PARTA-1 after it was deleted from embeddings, want none: the batch brought the row back", left)
	}
	// The control: nothing holds the rows now, so the same delete does not wait.
	done := make(chan error, 1)
	go func() {
		_, err := database.Pool.Exec(ctx, `delete from embeddings where id = 'PARTA-2'`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a delete with no batch open waited: the wait above proves nothing")
	}
}

// waitForALockWaiter answers whether some backend of the database waits for a lock within bound.
func waitForALockWaiter(t *testing.T, database *Store, bound time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// partitionCopyShortLine is the line PartitionEmbeddings writes when the copy's count and
// embeddings' disagree.
const partitionCopyShortLine = "dispatch: the embeddings partition copy does not hold the rows embeddings holds, so it is emptied and copied again: a write reached one table and not the other, as a write made as a replica, with the mirror's trigger disabled or by hand does"

// A copy that holds fewer rows than embeddings - a row taken out of it by a write the mirror did
// not see - is found by the count the fill makes before it takes any lock, emptied and copied
// again, and swapped in whole, in the same pass: the swapped-in table holds what embeddings held.
// And finding it holds no reader: a reader of embeddings holds its ACCESS SHARE lock across the
// pass, and the fill says the copy is short while that reader still holds it, which a fill that
// took embeddings' ACCESS EXCLUSIVE lock to count could not do before the reader let go. The
// reader lets go once the line is written, so the swap that follows takes its locks.
func TestASwapThatFindsTheCopyShortCopiesAgainWithoutHoldingAReader(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 2)
	before := embeddingsContent(t, database, "embeddings")
	execAll(t, database,
		`delete from embeddings_partitioned where id = 'PARTA-1'`,
		`update embeddings_partitioned_copy set done = true`,
	)
	reader, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	var held int
	if err := reader.QueryRow(ctx, `select count(*) from embeddings`).Scan(&held); err != nil {
		t.Fatalf("read embeddings as a reader that stays open: %v", err)
	}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	filled := make(chan error, 1)
	go func() {
		_, err := database.FillEmbeddingProjectKeys(ctx, "")
		filled <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(loggedLines(records, partitionCopyShortLine)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the fill did not say the copy is short while a reader of embeddings held its lock: it took embeddings' lock before it counted, which queues every reader behind it")
		}
		select {
		case err := <-filled:
			t.Fatalf("the fill ended (%v) before it said the copy is short", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := reader.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-filled; err != nil {
		t.Fatalf("fill over a short copy = %v, want it copied again and swapped in", err)
	}
	requirePartitioned(t, database, true)
	if after := embeddingsContent(t, database, "embeddings"); !slices.Equal(after, before) {
		t.Errorf("the swapped-in table holds %d rows that differ from the %d stored before the copy", len(after), len(before))
	}
	short := oneLine(t, records, partitionCopyShortLine)
	short.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "copy_rows" && attr.Value.Int64() != int64(held-1) || attr.Key == "embeddings_rows" && attr.Value.Int64() != int64(held) {
			t.Errorf("the short copy's line says %s=%v, want embeddings %d rows and the copy %d", attr.Key, attr.Value, held, held-1)
		}
		return true
	})
	oneLine(t, records, partitionSwappedLine)
}

// A copy that still disagrees after it is copied again - here every copy of PARTA-1 is dropped
// as it lands, so the two counts differ however often it is copied - ends the pass with an error
// and leaves the copy emptied, its cursor at the start, and embeddings as it was; once nothing
// drops the row, the next pass copies it whole and swaps it in.
func TestACopyThatDisagreesTwiceIsCopiedAgainByTheNextPass(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 2)
	before := embeddingsContent(t, database, "embeddings")
	execAll(t, database,
		`create function drop_parta_1() returns trigger language plpgsql as $$
		   begin if new.id = 'PARTA-1' then return null; end if; return new; end $$`,
		`create trigger drop_parta_1 before insert on embeddings_project_parta for each row execute function drop_parta_1()`,
		`delete from embeddings_partitioned where id = 'PARTA-1'`,
		`update embeddings_partitioned_copy set done = true`,
	)
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	_, err := database.FillEmbeddingProjectKeys(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "twice in a row") {
		t.Fatalf("fill over a copy that disagrees every time = %v, want the error that it disagreed twice", err)
	}
	if lines := loggedLines(records, partitionCopyShortLine); len(lines) != 2 {
		t.Errorf("the fill said the copy is short %d times, want twice", len(lines))
	}
	requirePartitioned(t, database, false)
	var copied int
	var kind, id string
	var done bool
	if err := database.Pool.QueryRow(ctx, `select (select count(*) from embeddings_partitioned), kind, id, done from embeddings_partitioned_copy`).
		Scan(&copied, &kind, &id, &done); err != nil {
		t.Fatal(err)
	}
	if copied != 0 || kind != "" || id != "" || done {
		t.Errorf("after the second disagreement the copy holds %d rows and its cursor reads (%q, %q, done %t), want it emptied and at the start", copied, kind, id, done)
	}
	execAll(t, database, `drop trigger drop_parta_1 on embeddings_project_parta`)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("the next pass, with nothing dropping the row = %v, want it copied and swapped in", err)
	}
	requirePartitioned(t, database, true)
	if after := embeddingsContent(t, database, "embeddings"); !slices.Equal(after, before) {
		t.Errorf("the swapped-in table holds %d rows that differ from the %d stored before the copy", len(after), len(before))
	}
}

// releasedCoverageInsert is the coverage pass's insert of every release before this one
// (embedqueue/coverage.go's insertMissingIssues), reduced to one row: its conflict target is
// embeddings' key before the swap, (kind, id), which the swap replaces.
const releasedCoverageInsert = `insert into embeddings (kind, id, text_snapshot, content_hash)
	select 'issue', 'PARTA-1', 'x', 'x' on conflict (kind, id) do nothing`

// The swap's two fleet commits: the swapping process's own, and an earlier build's.
const swapOwnCommit, swapEarlierCommit = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"

// registerSwapProcess writes a server_processes row of commit whose heartbeat is age old, and
// answers its instance.
func registerSwapProcess(t *testing.T, database *Store, commit string, age time.Duration) string {
	t.Helper()
	var instance string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, commit, signing_keys, heartbeat_at)
		values (gen_random_uuid(), $1, '{fingerprint}', now() - make_interval(secs => $2))
		returning instance::text`, commit, age.Seconds()).Scan(&instance); err != nil {
		t.Fatalf("register a process of %q: %v", commit, err)
	}
	return instance
}

// publishSwapDashboard publishes a dashboard_builds row named build of commit with
// dashboard.Publish's own statement, as a build's boot publishes its dashboard before it runs its
// coverage pass or registers, and then dates the publish age ago: the row's last start, and the
// commit's publish in dashboard_publishes, which migration 20261121120653's trigger recorded at
// now() under the inserting row's commit.
func publishSwapDashboard(t *testing.T, database *Store, build, commit string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into dashboard_builds (build, entry, commit, owner_lock_protocol) values ($1, 'index.html', $2, $3)
		on conflict (build) do update set last_started_at = now()`, build, commit, OwnerLockProtocol); err != nil {
		t.Fatalf("publish a dashboard of %q: %v", commit, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		with aged as (select now() - make_interval(secs => $3) as at),
		     build as (update dashboard_builds set first_started_at = (select at from aged), last_started_at = (select at from aged) where build = $1)
		update dashboard_publishes set published_at = (select at from aged) where commit = $2`,
		build, commit, age.Seconds()); err != nil {
		t.Fatalf("date the publish of %q: %v", commit, err)
	}
}

// The swap waits for every build of another commit the fleet shows, as the owner lock counts an
// earlier build (OwnerLockLegacyWindow): a registered process of another commit until its row is
// pruned, so one silent for minutes - frozen, or cut from the database - still holds it, and a
// build that has published its dashboard within the hour and not registered yet, which is how an
// earlier build boots before its coverage pass runs. A row past the prune age and a publish past
// the hour hold nothing. The release before's coverage insert plans while the swap waits, and
// fails with 42P10 once the holders are gone and the swap is made, which is why the swap waits.
func TestTheSwapWaitsForEveryBuildOfAnotherCommitTheFleetShows(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 2)
	registerSwapProcess(t, database, swapOwnCommit, 0)
	registerSwapProcess(t, database, "3333333333333333333333333333333333333333", OwnerLockLegacyWindow+time.Minute)
	publishSwapDashboard(t, database, "old-publish", "4444444444444444444444444444444444444444", OwnerLockMarkWindow+time.Minute)
	waitsFor := func(what, commit string) {
		t.Helper()
		_, err := database.FillEmbeddingProjectKeys(ctx, swapOwnCommit)
		if !errors.Is(err, ErrEmbeddingsSwapWaits) || !strings.Contains(err.Error(), commit) {
			t.Fatalf("fill beside %s = %v, want ErrEmbeddingsSwapWaits naming %s", what, err, commit)
		}
		if strings.Contains(err.Error(), "3333") || strings.Contains(err.Error(), "4444") {
			t.Errorf("the wait names a row past the prune age or a publish past the hour: %v", err)
		}
		requirePartitioned(t, database, false)
		execAll(t, database, releasedCoverageInsert)
	}

	silent := registerSwapProcess(t, database, swapEarlierCommit, 5*time.Minute)
	waitsFor("a process of an earlier build silent for five minutes", swapEarlierCommit)
	var done bool
	if err := database.Pool.QueryRow(ctx, `select done from embeddings_partitioned_copy`).Scan(&done); err != nil || !done {
		t.Errorf("the copy reads done %t (%v) while the swap waits, want the copy made", done, err)
	}
	execAll(t, database, fmt.Sprintf(`delete from server_processes where instance = '%s'`, silent))

	publishSwapDashboard(t, database, "booting", swapEarlierCommit, 30*time.Second)
	waitsFor("an earlier build that published its dashboard and has not registered", swapEarlierCommit)
	execAll(t, database, `delete from dashboard_builds where build = 'booting'`,
		fmt.Sprintf(`delete from dashboard_publishes where commit = '%s'`, swapEarlierCommit))

	if err := database.PartitionEmbeddings(ctx, swapOwnCommit); err != nil {
		t.Fatalf("partition with no other build in the fleet = %v, want the swap", err)
	}
	requirePartitioned(t, database, true)
	_, err := database.Pool.Exec(ctx, releasedCoverageInsert)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P10" {
		t.Errorf("the released coverage insert after the swap = %v, want SQLSTATE 42P10, which is why the swap waits", err)
	}
}

// The swap reads the fleet again once it holds its locks: an earlier build whose dashboard publish
// commits after the read before the copy's count, while the swap waits for its locks, holds it.
// A reader of embeddings_partitioned holds the swap's lock wait open, the publish commits in it,
// and the reader lets go: the fill answers ErrEmbeddingsSwapWaits naming the publish's commit,
// and embeddings stays as it was.
func TestTheSwapReadsTheFleetAgainUnderItsLocks(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 2)
	execAll(t, database, `update embeddings_partitioned_copy set done = true`)
	reader, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	if _, err := reader.Exec(ctx, `select count(*) from embeddings_partitioned`); err != nil {
		t.Fatalf("read the copy as a reader that stays open: %v", err)
	}
	filled := make(chan error, 1)
	go func() {
		filled <- database.PartitionEmbeddings(ctx, swapOwnCommit)
	}()
	if !waitForALockWaiter(t, database, 4*time.Second) {
		t.Fatal("the swap did not wait for the reader's lock: the two did not meet, so this row proves nothing")
	}
	publishSwapDashboard(t, database, "late", swapEarlierCommit, 0)
	if err := reader.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-filled; !errors.Is(err, ErrEmbeddingsSwapWaits) || !strings.Contains(err.Error(), swapEarlierCommit) {
		t.Fatalf("the swap beside a publish that committed while it waited for its locks = %v, want ErrEmbeddingsSwapWaits naming %s", err, swapEarlierCommit)
	}
	requirePartitioned(t, database, false)
}

// Every project has its partition from the transaction that inserts it, and a row of a project
// that does not exist is refused rather than kept anywhere, before the swap (by the mirror) and
// after it.
func TestEveryProjectHasItsPartitionAndARowOfNoProjectThatExistsIsRefused(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 1)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('INTX', 'In a transaction', 'alice@d.example')`); err != nil {
		t.Fatal(err)
	}
	var partition *string
	if err := tx.QueryRow(ctx, `select to_regclass('embeddings_project_intx')::text`).Scan(&partition); err != nil || partition == nil {
		t.Errorf("INTX has no partition inside the transaction that inserts it (%v)", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select to_regclass('embeddings_project_intx')::text`).Scan(&partition); err != nil || partition != nil {
		t.Errorf("INTX's partition outlived the rolled-back insert of INTX (%v)", err)
	}
	refused := func(when string) {
		t.Helper()
		_, err := database.Pool.Exec(ctx, `insert into embeddings (kind, id, text_snapshot, content_hash, project_key) values ('issue', 'NOPE-1', 'x', 'x', 'NOPE')`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s, a row of project NOPE = %v, want SQLSTATE 23514 (no partition)", when, err)
		}
	}
	refused("before the swap")
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatal(err)
	}
	refused("after the swap")
}

// A project inserted while the swap holds both tables gets its partition, attached to the table
// the swap renamed, once the swap commits. Its trigger reads the parent by name and names it in
// its statements, and a swap that commits between the two leaves them naming a table that is gone
// (SQLSTATE 42P01): a server whose boot swaps an empty table in as its first project is written
// answered that POST /api/v1/projects with 500. The insert must wait for the swap, which shows the
// two met; a run where it did not wait proves nothing and fails.
func TestAProjectWrittenAsTheSwapCommitsHasItsPartition(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 1)
	swap, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer swap.Rollback(ctx)
	if _, err := swap.Exec(ctx, `select embeddings_swap_in_partitioned()`); err != nil {
		t.Fatalf("swap the partitioned table in: %v", err)
	}
	inserted := make(chan error, 1)
	go func() {
		_, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('RACE', 'Written as the swap commits', 'alice@d.example')`)
		inserted <- err
	}()
	if !waitForALockWaiter(t, database, 5*time.Second) {
		t.Fatal("the insert of RACE did not wait for the open swap: the two did not meet, so this row proves nothing")
	}
	if err := swap.Commit(ctx); err != nil {
		t.Fatalf("commit the swap: %v", err)
	}
	if err := <-inserted; err != nil {
		t.Fatalf("insert RACE as the swap commits = %v, want it inserted with its partition", err)
	}
	requirePartitioned(t, database, true)
	var parent *string
	if err := database.Pool.QueryRow(ctx,
		`select inhparent::regclass::text from pg_inherits where inhrelid = to_regclass('embeddings_project_race')`).Scan(&parent); err != nil || parent == nil || *parent != "embeddings" {
		t.Errorf("RACE's partition is attached to %v (%v), want embeddings, the table the swap renamed", parent, err)
	}
}

// A project gets its partition however it is written: by a session that writes as a replica,
// whose writes fire no ordinary trigger, and by a statement that inserts the project and an issue
// of it at once, whose issue's embeddings row is written before the project's own trigger runs (a
// WITH query the statement does not read runs last). In each, the issue's row lands in the
// project's partition, before the swap and after it. A project written with its table's triggers
// off has no partition at all: before the swap the copy gives it one, so its rows are copied, and
// after the swap a row of it stored with no project is refused by the label trigger, which cannot
// attach a partition inside the statement writing embeddings (SQLSTATE 55006), and names the
// statement that gives it one.
func TestAProjectWrittenAsAReplicaOrBesideItsIssueHasItsPartition(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 1)
	write := func(project, partitioned string) {
		t.Helper()
		conn, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `set session_replication_role = replica`); err != nil {
			t.Fatalf("write as a replica: %v", err)
		}
		_, err = conn.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, 'Written as a replica', 'alice@d.example')`, project+"R")
		if _, resetErr := conn.Exec(ctx, `reset session_replication_role`); resetErr != nil {
			t.Fatal(resetErr)
		}
		if err != nil {
			t.Fatalf("insert %sR as a replica: %v", project, err)
		}
		// Its partition is there before any row of it, so a row stored with no project and
		// labelled by the label trigger, which cannot make one, still lands.
		var partition *string
		if err := database.Pool.QueryRow(ctx, `select to_regclass('embeddings_project_' || lower($1))::text`, project+"R").Scan(&partition); err != nil || partition == nil {
			t.Errorf("%sR, written as a replica, has no partition before any row of it is written (%v)", project, err)
		}
		execAll(t, database,
			fmt.Sprintf(`insert into issues (key, project_key, number, title, created_by, rank) values ('%[1]sR-1', '%[1]sR', 1, 'An issue', '{"kind":"user","id":"alice"}', 'a')`, project),
			fmt.Sprintf(`with p as (insert into projects (key, name, default_assignee) values ('%[1]sS', 'Beside its issue', 'alice@d.example') returning key),
				i as (insert into issues (key, project_key, number, title, created_by, rank) values ('%[1]sS-1', '%[1]sS', 1, 'An issue', '{"kind":"user","id":"alice"}', 'a') returning key)
				select key from i`, project),
		)
		for _, key := range []string{project + "R", project + "S"} {
			if stored := partitionOfIn(t, database, partitioned, key+"-1"); stored != "embeddings_project_"+strings.ToLower(key) {
				t.Errorf("%s-1's embeddings row is stored in %s, want embeddings_project_%s", key, stored, strings.ToLower(key))
			}
		}
	}
	// triggersOff writes project and its first issue with every trigger of their tables off, as a
	// restore that disables triggers does: no partition, and no embeddings row.
	triggersOff := func(project string) {
		t.Helper()
		execAll(t, database,
			`alter table projects disable trigger user`,
			`alter table issues disable trigger user`,
			fmt.Sprintf(`insert into projects (key, name, default_assignee) values ('%s', 'Triggers off', 'alice@d.example')`, project),
			fmt.Sprintf(`insert into issues (key, project_key, number, title, created_by, rank) values ('%[1]s-1', '%[1]s', 1, 'An issue', '{"kind":"user","id":"alice"}', 'a')`, project),
			`alter table issues enable trigger user`,
			`alter table projects enable trigger user`,
			`alter table projects enable always trigger projects_embeddings_partition`,
		)
	}

	write("BEF", "embeddings_partitioned")
	triggersOff("BEFT")
	execAll(t, database,
		`alter table embeddings disable trigger user`,
		`insert into embeddings (kind, id, text_snapshot, content_hash, project_key) values ('issue', 'BEFT-1', 'An issue', 'h', 'BEFT')`,
		`alter table embeddings enable trigger user`,
	)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("partition embeddings over a project with no partition: %v", err)
	}
	if stored := partitionOf(t, database, "issue", "BEFT-1"); stored != "embeddings_project_beft" {
		t.Errorf("BEFT-1, of a project written with its triggers off, is stored in %s after the swap, want embeddings_project_beft", stored)
	}

	write("AFT", "embeddings")
	triggersOff("AFTT")
	insert := `insert into embeddings (kind, id, text_snapshot, content_hash) values ('issue', 'AFTT-1', 'An issue', 'h')`
	_, err := database.Pool.Exec(ctx, insert)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.Contains(pgErr.Hint, "embeddings_add_project_partition('AFTT')") {
		t.Fatalf("a row of AFTT, a project with no partition, stored with no project = %v, want SQLSTATE 23514 naming embeddings_add_project_partition('AFTT')", err)
	}
	execAll(t, database, `select embeddings_add_project_partition('AFTT')`, insert)
	if stored := partitionOf(t, database, "issue", "AFTT-1"); stored != "embeddings_project_aftt" {
		t.Errorf("AFTT-1, labelled once its project has its partition, is stored in %s, want embeddings_project_aftt", stored)
	}
}

// The two migrations take the census's size limit as their shapes say they must: the partitioned
// table's migration locks no table above ACCESS SHARE but projects, and the mirror's is exempt
// from the limit however large embeddings is. The limit is injected just above projects' size,
// over a database whose embeddings is far above it. The control is an in-place rename of
// embeddings over the same database, which the census refuses at that limit.
func TestCensusAdmitsThePartitionMigrationsOverALargeEmbeddings(t *testing.T) {
	ctx := context.Background()
	for _, migration := range []struct {
		file string
		want []string
	}{
		{"20261120105330_embeddings_partitioned.up.sql", []string{"embeddings_partitioned", "embeddings_partitioned_copy", "projects"}},
		{"20261120105818_embeddings_partitioned_mirror.up.sql", []string{"embeddings"}},
	} {
		sql, err := migrationFiles.ReadFile("migrations/" + migration.file)
		if err != nil {
			t.Fatal(err)
		}
		if touched := pgmigrate.TouchedTables(string(sql)); !slices.Equal(touched, migration.want) {
			t.Errorf("%s touches %v, want %v", migration.file, touched, migration.want)
		}
	}

	store := openEmptyTestStore(t)
	migrateThrough(t, store, embeddingsPartitionedVersion-1)
	execAll(t, store,
		`insert into projects (key, name, default_assignee) values ('BIG', 'Big', 'alice@d.example')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			select 'BIG-' || n, 'BIG', n, repeat('Issue ' || n || ' ', 40), '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
			  from generate_series(1, 2000) n`,
	)
	var projects, embeddings int64
	if err := store.Pool.QueryRow(ctx, `select pg_total_relation_size('projects'), pg_total_relation_size('embeddings')`).Scan(&projects, &embeddings); err != nil {
		t.Fatal(err)
	}
	limit := projects + 1
	if embeddings <= 4*limit {
		t.Fatalf("embeddings is %d bytes against a limit of %d: the census would measure nothing above it", embeddings, limit)
	}
	report, err := census(ctx, store.Pool.Config().ConnString(), migrationsThrough(t, embeddingsMirrorVersion), pgmigrate.CensusOptions{TableLimit: limit})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the partition migrations were refused over a %d-byte embeddings at a %d-byte limit: %v", embeddings, limit, refusals(report))
	}
	if len(report.Pending) != 2 || report.Pending[1].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want the two migrations, the mirror's exempt as its function and triggers", report.Pending)
	}

	inPlace := migrationsThrough(t, embeddingsPartitionedVersion-1)
	inPlace["migrations/20261120105330_embeddings_partitioned.up.sql"] = &fstest.MapFile{Data: []byte("alter table embeddings rename to embeddings_unpartitioned;")}
	inPlace["migrations/20261120105330_embeddings_partitioned.census.sql"] = &fstest.MapFile{Data: []byte("select 0;")}
	control, err := census(ctx, store.Pool.Config().ConnString(), inPlace, pgmigrate.CensusOptions{TableLimit: limit})
	if err != nil {
		t.Fatalf("census of the in-place shape: %v", err)
	}
	if !control.Refused() {
		t.Errorf("the census admitted a rename of a %d-byte embeddings at a %d-byte limit: this test's admission proves nothing", embeddings, limit)
	}
}

// Once embeddings is the partitioned table, a migration that rewrites it is held to the census's
// limit by the size its partitions hold, as it was by the size of the table before: a partitioned
// table holds no row of its own, and read as one table it measures 0 bytes, which would admit a
// rewrite of every vector the deployment holds. The limit is injected at one byte under
// embeddings' partitions' size, so the test needs no gigabyte.
func TestTheCensusStillGuardsEmbeddingsOnceItIsPartitioned(t *testing.T) {
	ctx := context.Background()
	database, _ := openPartitionTestStore(t, 50)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("partition embeddings: %v", err)
	}
	requirePartitioned(t, database, true)
	var held int64
	if err := database.Pool.QueryRow(ctx, `select sum(pg_total_relation_size(relid))::bigint from pg_partition_tree('embeddings')`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	rewrite := migrationsThrough(t, 1<<62)
	rewrite["migrations/29991231235959_rewrite_embeddings.up.sql"] = &fstest.MapFile{Data: []byte("update embeddings set dead = dead;")}
	rewrite["migrations/29991231235959_rewrite_embeddings.census.sql"] = &fstest.MapFile{Data: []byte("select 0;")}
	report, err := census(ctx, database.Pool.Config().ConnString(), rewrite, pgmigrate.CensusOptions{TableLimit: held - 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "embeddings is ") {
		t.Errorf("a rewrite of the partitioned embeddings, whose partitions hold %d bytes, at a %d-byte limit: refusals %v, want one naming embeddings' size:\n%s",
			held, held-1, got, reportText(report))
	}
}
