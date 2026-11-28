package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testwait"
)

// spyTx stands in for the census's transaction in censusTable, and in the classifier boundedWrites
// reads the catalog through. Each savepoint it begins is a spySavepoint. The first statement sent
// on the transaction itself once a savepoint has begun, a savepoint of its own included, first
// reads current_setting('statement_timeout') and keeps it in after: in censusTable the first such
// statement is boundedWrites' first read of the catalog, sent after the reads' savepoint.
type spyTx struct {
	pgx.Tx
	plant      string
	savepoints []*spySavepoint
	after      string
}

func (s *spyTx) note(ctx context.Context) {
	if len(s.savepoints) == 0 || s.after != "" {
		return
	}
	if err := s.Tx.QueryRow(ctx, "select current_setting('statement_timeout')").Scan(&s.after); err != nil {
		s.after = "unread: " + err.Error()
	}
}

func (s *spyTx) Begin(ctx context.Context) (pgx.Tx, error) {
	s.note(ctx)
	nested, err := s.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	savepoint := &spySavepoint{Tx: nested, plant: s.plant}
	s.savepoints = append(s.savepoints, savepoint)
	return savepoint, nil
}

func (s *spyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.note(ctx)
	return s.Tx.Exec(ctx, sql, args...)
}

func (s *spyTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.note(ctx)
	return s.Tx.Query(ctx, sql, args...)
}

func (s *spyTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	s.note(ctx)
	return s.Tx.QueryRow(ctx, sql, args...)
}

// spySavepoint is a savepoint of a spyTx. It counts the set_config statements of
// statement_timeout sent in it (bounds), and just before its first rollback, the statement that
// ends it, it reads current_setting('statement_timeout') and keeps it in atEnd. With plant set,
// that first rollback sends plant first, and returns plant's error without ending the savepoint
// when plant fails; the rollback after it, censusTable's deferred one, ends the savepoint.
type spySavepoint struct {
	pgx.Tx
	plant   string
	planted bool
	bounds  int
	atEnd   string
}

func (s *spySavepoint) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "set_config('statement_timeout'") {
		s.bounds++
	}
	return s.Tx.Exec(ctx, sql, args...)
}

func (s *spySavepoint) Rollback(ctx context.Context) error {
	if s.atEnd == "" {
		if err := s.Tx.QueryRow(ctx, "select current_setting('statement_timeout')").Scan(&s.atEnd); err != nil {
			s.atEnd = "unread: " + err.Error()
		}
	}
	if s.plant != "" && !s.planted {
		s.planted = true
		if _, err := s.Tx.Exec(ctx, s.plant); err != nil {
			return err
		}
	}
	return s.Tx.Rollback(ctx)
}

// readBoundTx is a read-only transaction on a database of the test's own (fixtureDatabase),
// holding a table things with a row, under the settings locking's Census gives its own
// transaction (censusSettings); the zero Locking's are production's.
func readBoundTx(t *testing.T, ctx context.Context, locking Locking) pgx.Tx {
	t.Helper()
	conn := fixtureDatabase(t, ctx, "pgmigrate_read_bound", "read-bound tests", "create table things (id integer); insert into things values (1)")
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	set, args := setConfig(true, locking.censusSettings())
	if _, err := tx.Exec(ctx, set, args...); err != nil {
		t.Fatalf("set the census's settings: %v", err)
	}
	return tx
}

// readBoundOptions are the three options censusTable reads (TableLimit, LongTransaction and
// TableReadTimeout), filled as Census fills them, with tableLimit and a TableReadTimeout of
// readTimeout.
func readBoundOptions(tableLimit int64, readTimeout time.Duration) CensusOptions {
	return CensusOptions{TableLimit: tableLimit, LongTransaction: CensusLongTransaction, TableReadTimeout: readTimeout}
}

// The reads of a touched table run under TableReadTimeout, and only they and the rollback that
// ends their savepoint do: every statement censusTable sends after that rollback, and the
// census's statement after censusTable returns, starts under CensusStatementTimeout. This holds
// on each way out of reads that answered: a table counted; a table above the limit, whose writes
// boundedWrites then reads in the catalog; and a table a foreign key reaches, whose size alone is
// read. The reads' bound is 30 s, which differs from the minute and leaves the reads, which take
// milliseconds, half a minute: the rollback starting under 30s shows the reads ran under it, and
// a statement after it starting under 1min, as PostgreSQL prints the minute, shows it came back.
// At TableReadTimeout equal to CensusStatementTimeout, the bound the transaction already holds,
// censusTable sends no set_config, as the merge base sent none. The table has no other session's
// lock on it, so the reads' savepoint is the first censusTable begins.
func TestCensusTableRunsOnlyTheTablesReadsUnderTheReadBound(t *testing.T) {
	t.Parallel()
	const insert = "insert into things (id) values (2)"
	for name, row := range map[string]struct {
		touched     TouchedTable
		tableLimit  int64
		readTimeout time.Duration
		writes      string
		bounds      int
		atEnd       string
	}{
		"a table counted":                          {TouchedTable{Name: "things"}, CensusTableLimit, 30 * time.Second, "", 1, "30s"},
		"a table above the limit, its writes read": {TouchedTable{Name: "things"}, 1, 30 * time.Second, insert, 1, "30s"},
		"a table a foreign key reaches":            {TouchedTable{Name: "things", Through: "others"}, CensusTableLimit, 30 * time.Second, "", 1, "30s"},
		"a table counted, the bound the minute":    {TouchedTable{Name: "things"}, CensusTableLimit, CensusStatementTimeout, "", 0, "1min"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tx := readBoundTx(t, ctx, Locking{})
			spy := &spyTx{Tx: tx}
			writes := &writeReading{}
			if row.writes != "" {
				classified, err := tableSizeExemption(ctx, tx, tx, row.writes)
				if err != nil || classified.statements == nil {
					t.Fatalf("parse %q: %v", row.writes, err)
				}
				writes = &writeReading{c: &classifier{ctx: ctx, census: spy, catalog: spy}, statements: classified.statements, changedBy: map[string]string{}, reviewed: reviewedTriggers}
			}
			entry := &MigrationCensus{}
			if err := (Locking{}).censusTable(ctx, spy, row.touched, readBoundOptions(row.tableLimit, row.readTimeout), lockBound{setting: lockTimeoutSetting}, entry, writes); err != nil {
				t.Fatalf("censusTable: %v", err)
			}
			if len(spy.savepoints) == 0 {
				t.Fatal("censusTable began no savepoint for the table's reads")
			}
			reads := spy.savepoints[0]
			if reads.bounds != row.bounds || reads.atEnd != row.atEnd {
				t.Errorf("the reads' savepoint sent %d set_config of statement_timeout and ended under %q; want %d and %q", reads.bounds, reads.atEnd, row.bounds, row.atEnd)
			}
			if row.writes != "" {
				if len(entry.Tables) != 1 || entry.Tables[0].BoundedWrites != 1 || len(entry.Refusals) != 0 {
					t.Fatalf("tables %+v, refusals %q; want things admitted for one bounded write", entry.Tables, entry.Refusals)
				}
				if spy.after != "1min" {
					t.Errorf("boundedWrites' first read of the catalog started under statement_timeout %q, want 1min, CensusStatementTimeout", spy.after)
				}
			}
			var next string
			if err := tx.QueryRow(ctx, "select current_setting('statement_timeout')").Scan(&next); err != nil || next != "1min" {
				t.Errorf("the census's statement after censusTable started under statement_timeout %q (%v), want 1min, CensusStatementTimeout", next, err)
			}
		})
	}
}

// readBoundPlantKey is the advisory lock the plant of
// TestCensusTableFailsByNameWhenTheEndOfItsReadsOutlastsTheReadBound waits for. Postgres keys an
// advisory lock by its database as well, and each row's database is its own, so no other test's
// session takes this lock.
const readBoundPlantKey = 6354

// The rollback that ends the savepoint of reads that answered starts under the reads'
// TableReadTimeout, and one that outlasts it fails censusTable with its own error, SQLSTATE 57014
// and the table's name, where a rollback whose error nothing read would leave the savepoint
// aborted and fail the census's next statement with 25P02 and no name. The plant, sent where that
// rollback starts (spySavepoint) and under the setting the rollback starts under, waits for an
// advisory lock (readBoundPlantKey) that another session of the test holds on the test's own
// database. Under a TableReadTimeout of 5 s, PostgreSQL cancels that wait at 5 s; the reads before
// it take milliseconds, and a read past 5 s would fail the row by another message. The bound stays
// 5 s, because the reads before the plant run under it too, and a shorter one could fail the row
// on a loaded machine. The census's transaction takes its settings from a Locking whose Timeout is
// a minute, above both read bounds, so that the read bound ends the plant's lock wait and not a
// lock_timeout of the same 5 s. The control gives the reads 30 s, and once pg_locks shows the plant
// waiting, the test releases the lock and the plant answers, so the failure comes from the bound in
// force where the savepoint ends, and the control's verdict is that release, never a span. A read
// past 30 s in the control would make censusTable refuse the table and return no error, so the
// control also finds no refusal.
func TestCensusTableFailsByNameWhenTheEndOfItsReadsOutlastsTheReadBound(t *testing.T) {
	t.Parallel()
	locking := Locking{Timeout: time.Minute}
	for name, row := range map[string]struct {
		readTimeout time.Duration
		fails       bool
	}{
		"under the reads' 5 s":           {5 * time.Second, true},
		"control: under the reads' 30 s": {30 * time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tx := readBoundTx(t, ctx, locking)
			// A second session on the test's own database, which holds the lock the plant waits for.
			holder := connect(t, ctx, tx.Conn().Config().ConnString())
			if _, err := holder.Exec(ctx, "select pg_advisory_lock($1)", readBoundPlantKey); err != nil {
				t.Fatalf("take the plant's advisory lock: %v", err)
			}
			census := tx.Conn().PgConn().PID()
			spy := &spyTx{Tx: tx, plant: fmt.Sprintf("select pg_advisory_xact_lock(%d)", readBoundPlantKey)}
			entry := &MigrationCensus{}
			done := make(chan error, 1)
			go func() {
				done <- locking.censusTable(ctx, spy, TouchedTable{Name: "things"}, readBoundOptions(CensusTableLimit, row.readTimeout), lockBound{setting: lockTimeoutSetting}, entry, &writeReading{})
			}()
			if !row.fails {
				testwait.Until(t, "the plant to wait for the advisory lock the test holds", func() (bool, bool) {
					select {
					case err := <-done:
						t.Fatalf("censusTable returned before the plant waited for the test's advisory lock: %v", err)
					default:
					}
					var waits bool
					if err := holder.QueryRow(ctx, "select exists(select from pg_locks where pid = $1 and locktype = 'advisory' and not granted)", census).Scan(&waits); err != nil {
						t.Fatalf("read whether the plant waits for the advisory lock: %v", err)
					}
					return waits, waits
				})
				var released bool
				if err := holder.QueryRow(ctx, "select pg_advisory_unlock($1)", readBoundPlantKey).Scan(&released); err != nil || !released {
					t.Fatalf("release the plant's advisory lock: released %v, %v", released, err)
				}
			}
			err := testwait.Receive(t, "censusTable to return", done)
			if len(spy.savepoints) == 0 || !spy.savepoints[0].planted {
				t.Fatalf("the plant did not run where the reads' savepoint ends (%d savepoints)", len(spy.savepoints))
			}
			var pgErr *pgconn.PgError
			switch {
			case !row.fails && err != nil:
				t.Fatalf("censusTable: %v, want the plant to answer once the test released its lock, under 30 s", err)
			case !row.fails && len(entry.Refusals) != 0:
				t.Fatalf("the census refused %q, want no refusal of things, whose reads answer within the reads' 30 s", entry.Refusals)
			case row.fails && (err == nil || !errors.As(err, &pgErr) || pgErr.Code != queryCanceled || !strings.Contains(err.Error(), "census: end the savepoint of the reads of things: ")):
				t.Fatalf("censusTable: %v, want the savepoint end's own error, naming things, with SQLSTATE 57014", err)
			}
		})
	}
}
