package testdb

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The fixture's order is held here directly: a recordingQuerier stands under CancelAfterScan, so
// each event below is the fixture's own, and no row pgx has already buffered decides it.
func TestCancelAfterScanCancelsOnlyOnceTheScanHasEnded(t *testing.T) {
	scanFailed := errors.New("the row could not be scanned")
	for _, tc := range []struct {
		name    string
		sql     []string // the QueryRow calls of one transaction, in order
		scanErr error    // what each Scan answers; nil scans 7
		want    []string
	}{
		{
			name: "a matching row that scans",
			sql:  []string{"select 7 /* probe */"},
			want: []string{"scan start", "scan end", "cancel"},
		},
		{
			name: "a row of SQL that does not match",
			sql:  []string{"select 1"},
			want: []string{"scan start", "scan end"},
		},
		{
			name:    "a matching row whose scan fails",
			sql:     []string{"select 7 /* probe */"},
			scanErr: scanFailed,
			want:    []string{"scan start", "scan end"},
		},
		{
			name: "a second matching row of the same transaction",
			sql:  []string{"select 7 /* probe */", "select 7 /* probe */"},
			want: []string{"scan start", "scan end", "cancel", "scan start", "scan end"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &eventLog{}
			querier := recordingQuerier{events: events, scanErr: tc.scanErr}
			wrapped := CancelAfterScan(querier, "probe", func() { events.add("cancel") })
			ctx := context.Background()
			tx, err := wrapped.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, sql := range tc.sql {
				var got int
				err := tx.QueryRow(ctx, sql).Scan(&got)
				if !errors.Is(err, tc.scanErr) {
					t.Fatalf("the scan of %q answered %v, want %v", sql, err, tc.scanErr)
				}
				if err == nil && got != 7 {
					t.Fatalf("the scan of %q read %d, want 7", sql, got)
				}
			}
			if got := events.all(); !slices.Equal(got, tc.want) {
				t.Fatalf("the events are %q, want %q", got, tc.want)
			}
			if err := tx.Commit(ctx); !errors.Is(err, errCommitLost) {
				t.Fatalf("Commit answered %v, want the recording transaction's %v", err, errCommitLost)
			}
			if got := wrapped.Commits(); got != 1 {
				t.Fatalf("Commits() is %d after one failed Commit, want 1", got)
			}
		})
	}
}

// eventLog is the one ordered record the recording querier's rows and the cancel write to.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// errCommitLost is what every recordingTx's Commit answers, so a test reads that Commits() counts
// a Commit whatever its result.
var errCommitLost = errors.New("the commit's reply was lost")

// recordingQuerier is a Querier whose rows record the start and the end of each Scan.
type recordingQuerier struct {
	events  *eventLog
	scanErr error
}

func (q recordingQuerier) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return recordingTx{querier: q}, nil
}

func (q recordingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("the recording querier reads no rows with Query")
}

func (q recordingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return recordingRow(q)
}

// recordingTx is a transaction of the recording querier. Its embedded pgx.Tx is nil, so a method
// the fixture should not call panics.
type recordingTx struct {
	pgx.Tx
	querier recordingQuerier
}

func (tx recordingTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return recordingRow(tx.querier)
}

func (tx recordingTx) Commit(context.Context) error { return errCommitLost }

// recordingRow records "scan start", sets its destination to 7 or answers its querier's scanErr,
// and records "scan end".
type recordingRow recordingQuerier

func (row recordingRow) Scan(dest ...any) error {
	row.events.add("scan start")
	defer row.events.add("scan end")
	if row.scanErr != nil {
		return row.scanErr
	}
	*dest[0].(*int) = 7
	return nil
}

// Over a real pool: the context is live after a row of other SQL, and cancelled after the matching
// row has been read. This does not tell a cancel after the scan from one before it, because pgx can
// hold a row before Scan runs (pgx v5.13.0 rows.go:117-140), so a scan after a cancel can still
// succeed; the recording test above holds that order.
func TestCancelAfterScanCancelsOnceTheMatchingRowIsRead(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), Database(t, "legion_testdb_scan"))
	if err != nil {
		t.Fatalf("open a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapped := CancelAfterScan(pool, "probe", cancel)
	tx, err := wrapped.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	var one int
	if err := tx.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("select 1: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("the context ended after a row of SQL that does not match: %v", err)
	}
	var seven int
	if err := tx.QueryRow(ctx, "select 7 /* probe */").Scan(&seven); err != nil || seven != 7 {
		t.Fatalf("the matching row read %d, %v, want 7", seven, err)
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the context after the matching row is %v, want it cancelled", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit on a live context: %v", err)
	}
	if got := wrapped.Commits(); got != 1 {
		t.Fatalf("Commits() is %d after one Commit, want 1", got)
	}
}
