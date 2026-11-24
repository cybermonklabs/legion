package pgstore

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/consumernames"
)

// pgRow is one row of memPG: the record's bytes and the revision, or err.
type pgRow struct {
	data     []byte
	revision int64
	err      error
}

func (r pgRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch len(dest) {
	case 1:
		*dest[0].(*int64) = r.revision
	case 2:
		*dest[0].(*[]byte) = r.data
		*dest[1].(*int64) = r.revision
	}
	return nil
}

// memPG is a database of one intake_consumers table in memory, as Store's statements use it: a
// select reads a row by key, an insert at revision 0 refuses a row that exists, an update writes
// only where the row holds the revision given, and List's query reads every row its where clause
// keeps, in key order.
type memPG struct{ rows map[string]pgRow }

func newMemPG() *memPG { return &memPG{rows: map[string]pgRow{}} }

func (m *memPG) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	key := args[0].(string)
	row, held := m.rows[key]
	switch statement := strings.TrimSpace(sql); {
	case strings.HasPrefix(statement, "select"):
		if !held {
			return pgRow{err: pgx.ErrNoRows}
		}
		return row
	case strings.HasPrefix(statement, "insert"):
		if held {
			return pgRow{err: pgx.ErrNoRows}
		}
		m.rows[key] = pgRow{data: args[1].([]byte), revision: 1}
		return pgRow{revision: 1}
	default:
		if !held || row.revision != args[2].(int64) {
			return pgRow{err: pgx.ErrNoRows}
		}
		m.rows[key] = pgRow{data: args[1].([]byte), revision: row.revision + 1}
		return pgRow{revision: row.revision + 1}
	}
}

// Query answers List's query: `logical <> $1` keeps every row but the key given, and
// `left(logical, length($1)) = $1` every row whose key begins with it.
func (m *memPG) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	arg := args[0].(string)
	var keys []string
	for key := range m.rows {
		switch {
		case strings.Contains(sql, "logical <> $1") && key != arg,
			strings.Contains(sql, "left(logical, length($1)) = $1") && strings.HasPrefix(key, arg):
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return &memRows{m: m, keys: keys, at: -1}, nil
}

// memRows is memPG's answer to a query, one row of (key, record) at a time. It implements only
// what List calls; the embedded pgx.Rows is nil, so any other call panics.
type memRows struct {
	pgx.Rows
	m    *memPG
	keys []string
	at   int
}

func (r *memRows) Close()     {}
func (r *memRows) Err() error { return nil }
func (r *memRows) Next() bool { r.at++; return r.at < len(r.keys) }
func (r *memRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.keys[r.at]
	*dest[1].(*[]byte) = r.m.rows[r.keys[r.at]].data
	return nil
}

// newStore is New's store of family on db, failing the test when New refuses it.
func newStore(t testing.TB, family string, db Querier) *Store {
	t.Helper()
	store, err := New(family, db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// daemonFamily is consumernames.DaemonFamily(project), failing the test when it refuses it.
func daemonFamily(t testing.TB, project string) string {
	t.Helper()
	family, err := consumernames.DaemonFamily(project)
	if err != nil {
		t.Fatal(err)
	}
	return family
}
