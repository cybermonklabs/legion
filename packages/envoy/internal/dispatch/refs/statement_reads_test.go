package refs

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The helpers below read one of this package's statements the way a plan test reads it: as the
// function sent it, text and arguments (statementRecorder), over a fixture written in bulk so the
// planner plans it from real sizes (loadFixture), and by the plan it ran under (explainAnalyzed).

// recordedStatement is one statement a statementRecorder passed on, its text and the arguments
// it bound, as the caller sent them.
type recordedStatement struct {
	sql  string
	args []any
}

// statementRecorder is a Queryer that passes every statement on to the Queryer it wraps and
// records it, in the order it was sent, so a test can run a function's own statement again for
// its rows or its plan.
type statementRecorder struct {
	Queryer
	statements []recordedStatement
}

func (r *statementRecorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.statements = append(r.statements, recordedStatement{sql: sql, args: args})
	return r.Queryer.Query(ctx, sql, args...)
}

func (r *statementRecorder) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.statements = append(r.statements, recordedStatement{sql: sql, args: args})
	return r.Queryer.QueryRow(ctx, sql, args...)
}

// copyText writes rows into columns of table in one COPY of the text format, on tx's connection,
// and fails the test unless every row landed. The text format takes each value as its column's
// type reads its text, so a uuid, a jsonb document, an array or a time is written the way SQL
// writes it, and a fixture needs no Go type for any of them.
func copyText(t testing.TB, tx pgx.Tx, table string, columns []string, rows [][]any) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	var data strings.Builder
	for _, row := range rows {
		if len(row) != len(columns) {
			t.Fatalf("a row of %s holds %d values for its %d columns: %v", table, len(row), len(columns), row)
		}
		for index, value := range row {
			if index > 0 {
				data.WriteByte('\t')
			}
			data.WriteString(copyTextValue(t, value))
		}
		data.WriteByte('\n')
	}
	tag, err := tx.Conn().PgConn().CopyFrom(context.Background(), strings.NewReader(data.String()),
		"copy "+table+" ("+strings.Join(columns, ", ")+") from stdin")
	if err != nil {
		t.Fatalf("copy %d rows into %s: %v", len(rows), table, err)
	}
	if tag.RowsAffected() != int64(len(rows)) {
		t.Fatalf("%d of %d rows landed in %s", tag.RowsAffected(), len(rows), table)
	}
}

// copyTextEscapes are the characters the text format of COPY reads as the end of a value or a
// row, each written as the escape it reads back.
var copyTextEscapes = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// copyTextValue is value as the text format of COPY writes it: nil, and a nil pointer, are SQL
// null, and a time is written with its microseconds, the precision Postgres keeps.
func copyTextValue(t testing.TB, value any) string {
	t.Helper()
	switch value := value.(type) {
	case nil:
		return `\N`
	case string:
		return copyTextEscapes.Replace(value)
	case *string:
		if value == nil {
			return `\N`
		}
		return copyTextEscapes.Replace(*value)
	case int:
		return strconv.Itoa(value)
	case *int:
		if value == nil {
			return `\N`
		}
		return strconv.Itoa(*value)
	case time.Time:
		return value.UTC().Format("2006-01-02 15:04:05.000000Z07:00")
	}
	t.Fatalf("copyText writes no value of type %T", value)
	return ""
}

// copyTable is one table a bulk fixture writes and the columns its rows carry.
type copyTable struct {
	name    string
	columns []string
}

// loadFixture writes a fixture in bulk: in one transaction, it turns off every user trigger of
// tables, copies each table's rows (copyText) in the order of tables, each after every table its
// foreign keys name, turns the triggers on again and commits, then analyzes every table of
// tables, so the planner plans from the tables' real sizes. The triggers are on again before the
// commit, so no other session sees them off, as testdata/coverage_corpus.sql of embedqueue loads
// its corpus; one test login owns every table, so it may turn them off. They write the search
// vectors, the owed queues, the user threads, the watermarks and the embeddings, which no read
// of these tests reads, and they were most of a load's time. artifacts_ref_key is one of them, so
// a fixture writes each document's ref_key itself.
func loadFixture(t testing.TB, pool *store.Pool, tables []copyTable, rows map[string][][]any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	names := make([]string, len(tables))
	for index, table := range tables {
		names[index] = table.name
		if _, err := tx.Exec(ctx, "alter table "+table.name+" disable trigger user"); err != nil {
			t.Fatalf("turn the triggers of %s off: %v", table.name, err)
		}
	}
	for _, table := range tables {
		copyText(t, tx, table.name, table.columns, rows[table.name])
	}
	for _, name := range names {
		if _, err := tx.Exec(ctx, "alter table "+name+" enable trigger user"); err != nil {
			t.Fatalf("turn the triggers of %s on: %v", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "analyze "+strings.Join(names, ", ")); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
}

// explainAnalyzed is the plan statement ran under, from explain (analyze, format json), in a
// transaction of its own that runs each of settings first (each a `set local`) and rolls back.
func explainAnalyzed(t testing.TB, pool *store.Pool, statement recordedStatement, settings ...string) json.RawMessage {
	t.Helper()
	plan, err := explainAnalyzedWith(t, pool, "analyze, format json", statement, settings...)
	if err != nil {
		t.Fatalf("explain the statement under %v: %v\n%s", settings, err, statement.sql)
	}
	return plan
}

// explainAnalyzedWith is explainAnalyzed's plan read with the explain options given, analyze and
// format json among them, or the error the explain ended with, which it answers where
// explainAnalyzed fails the test: a statement_timeout among settings ends a statement that runs
// past it with SQLSTATE 57014.
func explainAnalyzedWith(t testing.TB, pool *store.Pool, options string, statement recordedStatement, settings ...string) (json.RawMessage, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain ("+options+") "+statement.sql, statement.args...).Scan(&raw); err != nil {
		return nil, err
	}
	return explaintest.OnePlan(t, raw), nil
}
