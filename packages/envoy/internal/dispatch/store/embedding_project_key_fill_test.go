package store

import (
	"context"
	"log/slog"
	"testing"
	"time"

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
