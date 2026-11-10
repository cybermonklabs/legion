package libx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ListTables is every table the library under test created in the run's schema, in
// name order. It is read rather than written down, so a fact about "the library's own
// tables" names the tables that really exist.
func ListTables(ctx context.Context, db *sql.DB, schema string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = $1 ORDER BY tablename`, schema)
	if err != nil {
		return nil, fmt.Errorf("list tables in %s: %w", schema, err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan table name in %s: %w", schema, err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables in %s: %w", schema, err)
	}
	return tables, nil
}

// CountRows is how many rows a table holds that match where, which may be empty for
// the whole table. The table name comes from the experiment's own code, never from
// input.
func CountRows(ctx context.Context, db *sql.DB, table, where string, args ...any) (int64, error) {
	query := fmt.Sprintf(`SELECT count(*) FROM %s`, Quote(table))
	if where != "" {
		query += " WHERE " + where
	}
	var n int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows of %s: %w", table, err)
	}
	return n, nil
}

// RowCounts is one line naming how many rows of each table match the same condition:
// what one workflow instance occupies in the library's own tables.
func RowCounts(ctx context.Context, db *sql.DB, tables []string, where string, args ...any) (string, error) {
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		n, err := CountRows(ctx, db, table, where, args...)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " "), nil
}

// SumRowBytes is how many bytes of PostgreSQL row the matching rows occupy across the
// given tables. It is what one open wait costs in storage, measured rather than
// estimated.
func SumRowBytes(ctx context.Context, db *sql.DB, tables []string, where string, args ...any) (int64, error) {
	var total int64
	for _, table := range tables {
		query := fmt.Sprintf(`SELECT COALESCE(sum(pg_column_size(t.*)), 0) FROM %s t`, Quote(table))
		if where != "" {
			query += " WHERE " + where
		}
		var n int64
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("measure row bytes of %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}

// Backends is what pg_stat_activity says about the connections to the evaluation
// database: how many there are and what each one is doing. A wait that held a
// transaction open would show here as a backend idle in transaction, and would stay
// there; a poller that begins and rolls back a transaction every few hundred
// milliseconds shows there too, which is why the long-held ones are counted apart.
type Backends struct {
	Total             int
	Active            int
	Idle              int
	IdleInTransaction int
	// IdleInTransactionLonger counts the backends that have been idle in
	// transaction for longer than LongTransaction, which no poll cycle can be.
	IdleInTransactionLonger int
}

// LongTransaction is how long a transaction has to have been sitting idle before it
// is something other than a poll that happened to be sampled mid-flight.
const LongTransaction = 2 * time.Second

// String is the one-line form a fact prints.
func (b Backends) String() string {
	return fmt.Sprintf("total=%d active=%d idle=%d idle-in-transaction=%d idle-in-transaction-for-longer-than-%s=%d",
		b.Total, b.Active, b.Idle, b.IdleInTransaction, LongTransaction, b.IdleInTransactionLonger)
}

// ReadBackends counts the connections to the evaluation database by state.
func ReadBackends(ctx context.Context, db *sql.DB) (Backends, error) {
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(state, 'unknown'), count(*),
		count(*) FILTER (WHERE now() - state_change > $1::interval)
		FROM pg_stat_activity WHERE datname = current_database() GROUP BY 1`,
		fmt.Sprintf("%d milliseconds", LongTransaction.Milliseconds()))
	if err != nil {
		return Backends{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	defer rows.Close()
	var b Backends
	for rows.Next() {
		var state string
		var n, long int
		if err := rows.Scan(&state, &n, &long); err != nil {
			return Backends{}, fmt.Errorf("scan pg_stat_activity: %w", err)
		}
		b.Total += n
		switch state {
		case "active":
			b.Active += n
		case "idle":
			b.Idle += n
		case "idle in transaction", "idle in transaction (aborted)":
			b.IdleInTransaction += n
			b.IdleInTransactionLonger += long
		}
	}
	if err := rows.Err(); err != nil {
		return Backends{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	return b, nil
}

// CountHeldLocks counts the locks the connections to this database hold that a
// waiting workflow would hold if it waited inside a transaction or on a row: a
// transaction id, a tuple, or a row-exclusive lock on one of the schema's tables.
// Locks every live backend holds on itself are not counted.
func CountHeldLocks(ctx context.Context, db *sql.DB, schema string) (int64, error) {
	var n int64
	query := `SELECT count(*)
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE a.datname = current_database()
		  AND (l.locktype IN ('transactionid', 'tuple')
		       OR (l.locktype = 'relation' AND l.mode <> 'AccessShareLock'
		           AND l.relation IN (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		                              WHERE n.nspname = $1)))`
	if err := db.QueryRowContext(ctx, query, schema).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pg_locks: %w", err)
	}
	return n, nil
}
