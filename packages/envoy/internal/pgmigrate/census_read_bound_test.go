package pgmigrate

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// spyTx stands in for the census's transaction in censusTable. Each savepoint it begins is a
// spySavepoint, which reads current_setting('statement_timeout') just before its first rollback,
// the statement that ends it, and keeps what it read. With plant set, a spySavepoint runs plant in
// place of the statement that sets CensusStatementTimeout back (restoreCensusBound), which it
// tells by its one argument, the minute's setting.
type spyTx struct {
	pgx.Tx
	plant      string
	savepoints []*spySavepoint
}

func (s *spyTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := s.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	savepoint := &spySavepoint{Tx: nested, plant: s.plant}
	s.savepoints = append(s.savepoints, savepoint)
	return savepoint, nil
}

type spySavepoint struct {
	pgx.Tx
	plant   string
	planted int
	// atEnd is the statement_timeout the savepoint read just before the statement that ends it;
	// empty until that statement.
	atEnd string
}

func (s *spySavepoint) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if s.plant != "" && len(args) == 1 && args[0] == censusStatementTimeoutSetting {
		s.planted++
		return s.Tx.Exec(ctx, s.plant, args...)
	}
	return s.Tx.Exec(ctx, sql, args...)
}

func (s *spySavepoint) Rollback(ctx context.Context) error {
	if s.atEnd == "" {
		if err := s.Tx.QueryRow(ctx, "select current_setting('statement_timeout')").Scan(&s.atEnd); err != nil {
			s.atEnd = "unread: " + err.Error()
		}
	}
	return s.Tx.Rollback(ctx)
}

// readBoundTx is a read-only transaction on a database of the test's own, holding a table things
// with a row, under the bounds Census gives its own transaction: statement_timeout at
// CensusStatementTimeout and lock_timeout at LockTimeout. It skips the test when
// DISPATCH_TEST_DATABASE_URL is unset, as boundedWritesDatabase does.
func readBoundTx(t *testing.T, ctx context.Context) pgx.Tx {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the census's read-bound tests")
	}
	adminURL, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatal("DISPATCH_TEST_DATABASE_URL is not a URL")
	}
	adminURL.Path = "/postgres"
	conn := connect(t, ctx, testdb.DatabaseOn(t, adminURL.String(), "pgmigrate_read_bound"))
	if _, err := conn.Exec(ctx, "create table things (id integer); insert into things values (1)"); err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "select set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true)", censusStatementTimeoutSetting, lockTimeoutSetting); err != nil {
		t.Fatalf("set the census's bounds: %v", err)
	}
	return tx
}

// readBoundOptions are the options censusTable reads, as Census fills them, with tableLimit and a
// TableReadTimeout of readTimeout.
func readBoundOptions(tableLimit int64, readTimeout time.Duration) CensusOptions {
	return CensusOptions{TableLimit: tableLimit, LongTransaction: CensusLongTransaction, TableReadTimeout: readTimeout, reviewedTriggers: reviewedTriggers}
}

// After a read of a touched table that answered, the statement that ends the reads' savepoint
// starts under CensusStatementTimeout, not under the reads' shorter TableReadTimeout, on each of
// the three ways out of a read that answered: a table counted, a table above the limit (judged on
// its size, its rows not counted) and a table a foreign key reaches (its size read alone). The
// savepoint reads its statement_timeout just before that statement (spySavepoint), and 1min is
// the minute as PostgreSQL prints it. The table has no other session's lock on it, so the
// savepoint is the census's one.
func TestCensusTableEndsTheSavepointOfAReadThatAnsweredUnderTheCensusBound(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		touched    TouchedTable
		tableLimit int64
	}{
		"a table counted":               {TouchedTable{Name: "things"}, CensusTableLimit},
		"a table above the limit":       {TouchedTable{Name: "things"}, 1},
		"a table a foreign key reaches": {TouchedTable{Name: "things", Through: "others"}, CensusTableLimit},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			spy := &spyTx{Tx: readBoundTx(t, ctx)}
			entry := &MigrationCensus{}
			if err := censusTable(ctx, spy, row.touched, readBoundOptions(row.tableLimit, time.Second), lockBound{setting: lockTimeoutSetting}, entry, &writeReading{}); err != nil {
				t.Fatalf("censusTable: %v", err)
			}
			if len(spy.savepoints) != 1 {
				t.Fatalf("censusTable began %d savepoints, want 1, the reads'", len(spy.savepoints))
			}
			if got := spy.savepoints[0].atEnd; got != "1min" {
				t.Errorf("the statement that ended the reads' savepoint started under statement_timeout %q, want 1min, CensusStatementTimeout", got)
			}
		})
	}
}

// The statement that sets CensusStatementTimeout back after a read that answered runs under the
// reads' TableReadTimeout, the one statement besides the reads that does (restoreCensusBound),
// and one that outlasts it fails censusTable with its own error, SQLSTATE 57014 and the table's
// name, rather than leaving the savepoint aborted. The plant puts a 2 s sleep in that statement.
// Under a TableReadTimeout of 1 s, PostgreSQL cancels it at 1 s. The control gives the reads
// 30 s, and the same planted statement answers, so the failure comes from the bound it starts
// under. (A control at the minute would set the reads' bound with the restore's own argument,
// which the plant would take for the restore.)
func TestCensusTableFailsByNameWhenTheRestoreOfItsBoundOutlastsTheReadBound(t *testing.T) {
	t.Parallel()
	const plant = "select set_config('statement_timeout', $1, true) from pg_sleep(2)"
	for name, row := range map[string]struct {
		readTimeout time.Duration
		fails       bool
	}{
		"under the reads' 1 s":           {time.Second, true},
		"control: under the reads' 30 s": {30 * time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			spy := &spyTx{Tx: readBoundTx(t, ctx), plant: plant}
			err := censusTable(ctx, spy, TouchedTable{Name: "things"}, readBoundOptions(CensusTableLimit, row.readTimeout), lockBound{setting: lockTimeoutSetting}, &MigrationCensus{}, &writeReading{})
			if len(spy.savepoints) != 1 || spy.savepoints[0].planted != 1 {
				t.Fatalf("the plant did not run once in the reads' savepoint (%d savepoints)", len(spy.savepoints))
			}
			var pgErr *pgconn.PgError
			switch {
			case !row.fails && err != nil:
				t.Fatalf("censusTable: %v, want the planted restore to answer under 30 s", err)
			case row.fails && (err == nil || !errors.As(err, &pgErr) || pgErr.Code != queryCanceled || !strings.Contains(err.Error(), "census: set the statement timeout back to 1m0s after the reads of things")):
				t.Fatalf("censusTable: %v, want the restore's own error, naming things, with SQLSTATE 57014", err)
			}
		})
	}
}
