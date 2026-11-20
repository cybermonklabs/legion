package testdb

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// Querier is what ScanCancel wraps: the daemon's guarded pool or a pgx pool.
type Querier interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CancelAfterScan is pool, whose transactions call cancel right after the first row they read with
// QueryRow for SQL containing match has been scanned: a cancel that lands between two statements of
// the transaction's owner. Its transactions count the Commit calls they receive.
//
// The wrapped row's Scan runs the real scan first, and calls cancel only when that scan returned
// nil. Commits counts every Commit call that reaches a transaction of the wrapper, whatever its
// result. Query and QueryRow outside a transaction pass to pool as they are.
func CancelAfterScan(pool Querier, match string, cancel context.CancelFunc) *ScanCancel {
	return &ScanCancel{pool: pool, match: match, cancel: cancel}
}

// ScanCancel is CancelAfterScan's pool.
type ScanCancel struct {
	pool    Querier
	match   string
	cancel  context.CancelFunc
	commits atomic.Int64
}

func (p *ScanCancel) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &scanCancelTx{Tx: tx, pool: p}, nil
}

func (p *ScanCancel) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p *ScanCancel) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

// Commits is how many Commit calls the wrapper's transactions have received.
func (p *ScanCancel) Commits() int {
	return int(p.commits.Load())
}

// scanCancelTx is a transaction of ScanCancel: once is what calls cancel, at most once.
type scanCancelTx struct {
	pgx.Tx
	pool *ScanCancel
	once sync.Once
}

func (tx *scanCancelTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if !strings.Contains(sql, tx.pool.match) {
		return row
	}
	return scanCancelRow{Row: row, tx: tx}
}

func (tx *scanCancelTx) Commit(ctx context.Context) error {
	tx.pool.commits.Add(1)
	return tx.Tx.Commit(ctx)
}

// scanCancelRow is a row of SQL containing the match: its transaction's cancel follows the first
// of them that scans.
type scanCancelRow struct {
	pgx.Row
	tx *scanCancelTx
}

func (row scanCancelRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	row.tx.once.Do(row.tx.pool.cancel)
	return nil
}
