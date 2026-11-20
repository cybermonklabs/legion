package store

// The store's text rule, measured against the PostgreSQL LEGION_TEST_PG_DSN names: on the UTF8
// server encoding a text value refuses a NUL and a byte that is not UTF-8 (SQLSTATE 22021), and a
// jsonb value refuses a \u0000 escape (22P05) and a surrogate escape that is not half of a pair
// (22P02).
// Every way a statement reaches the daemon's pool stores each of them as U+FFFD instead, whatever
// Go shape the value has, and stores every other value as it was written.

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// unstorableText holds a NUL and a lone 0xff, and storedText is what the store keeps of it.
	unstorableText = "a\x00b\xffc"
	storedText     = "a\uFFFDb\uFFFDc"
	// unstorableJSON holds, in its strings and a key, each escape jsonb refuses and a raw NUL and
	// 0xff, beside a surrogate pair jsonb takes.
	unstorableJSON = `{"escape":"x\u0000y","high":"\ud800z","low":"\udc00","pair":"\ud83d\ude00","key\u0000":1,"raw":"p` +
		"\x00" + `q` + "\xff" + `r"}`
)

// storedJSON is unstorableJSON as the store keeps it, read back into Go.
var storedJSON = map[string]any{
	"escape": "x\uFFFDy", "high": "\uFFFDz", "low": "\uFFFD", "pair": "\U0001F600", "key\uFFFD": float64(1),
	"raw": "p\uFFFDq\uFFFDr",
}

// storableLabel is a named string type, which pgx gives to the codec of its parameter's type.
type storableLabel string

// storableShape is one Go shape of a value: insert writes it as $2 into the row keyed $1, and
// column, read back from that row, holds stored.
type storableShape struct {
	name   string
	insert string
	value  any
	column string
	stored any
}

const (
	insertText  = "insert into storable (key, t) values ($1, $2)"
	insertTexts = "insert into storable (key, ts) values ($1, $2)"
	insertJSON  = "insert into storable (key, j) values ($1, $2)"
)

// unstorableShapes are the shapes of a value whose text the server refuses as it is.
var unstorableShapes = []storableShape{
	{"a string into text", insertText, unstorableText, "t", storedText},
	{"a *string into text", insertText, new(unstorableText), "t", storedText},
	{"a named string type into text", insertText, storableLabel(unstorableText), "t", storedText},
	{"a []string into text[]", insertTexts, []string{unstorableText, "kept"}, "ts", []any{storedText, "kept"}},
	{
		"a []string in = any($2)",
		"insert into storable (key, t) select $1, '" + storedText + "' where '" + storedText + "' = any($2)",
		[]string{"other", unstorableText}, "t", storedText,
	},
	{"a string into jsonb", insertJSON, unstorableJSON, "j", storedJSON},
	{"a []byte into jsonb", insertJSON, []byte(unstorableJSON), "j", storedJSON},
	{"a json.RawMessage into jsonb", insertJSON, json.RawMessage(unstorableJSON), "j", storedJSON},
	{
		"a struct into jsonb", insertJSON,
		struct {
			Raw string `json:"raw"`
		}{Raw: "p\x00q\xffr"},
		"j", map[string]any{"raw": "p\uFFFDq\uFFFDr"},
	},
	{
		"a map into jsonb", insertJSON,
		map[string]any{"raw": "p\x00q\xffr", "key\x00": "kept"},
		"j", map[string]any{"raw": "p\uFFFDq\uFFFDr", "key\uFFFD": "kept"},
	},
}

// storableShapes are values the server takes as they are, which the rule leaves as they are.
var storableShapes = []storableShape{
	{
		"a *string to a uuid parameter", "insert into storable (key, u) values ($1, $2)",
		new("0f8fad5b-d9cb-469f-a165-70867728950e"), "u::text", "0f8fad5b-d9cb-469f-a165-70867728950e",
	},
	{"a clean string", insertText, "plain words, kept as written", "t", "plain words, kept as written"},
	{"the six characters \\u0000 in text", insertText, `x\u0000y`, "t", `x\u0000y`},
	{
		"bytea holding 0x00", "insert into storable (key, b) values ($1, $2)",
		[]byte{0x00, 0xff, 'a', 0x00}, "b", []byte{0x00, 0xff, 'a', 0x00},
	},
}

// statements is what the pool and each of its transactions run a statement through.
type statements interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// statementWay runs one insert through one method of db. insert ends where a returning clause
// may follow.
type statementWay func(ctx context.Context, db statements, insert string, args ...any) error

func execWay(ctx context.Context, db statements, insert string, args ...any) error {
	_, err := db.Exec(ctx, insert, args...)
	return err
}

func queryWay(ctx context.Context, db statements, insert string, args ...any) error {
	rows, err := db.Query(ctx, insert+" returning key", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	inserted := 0
	for rows.Next() {
		inserted++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if inserted != 1 {
		return errors.New("the insert returned no row")
	}
	return nil
}

func queryRowWay(ctx context.Context, db statements, insert string, args ...any) error {
	var key string
	return db.QueryRow(ctx, insert+" returning key", args...).Scan(&key)
}

// storableEntry is one way a statement reaches the daemon's pool.
type storableEntry struct {
	name string
	run  func(ctx context.Context, pool *Pool, insert string, args ...any) error
}

// onThePool runs way on the pool itself, with no transaction.
func onThePool(way statementWay) func(context.Context, *Pool, string, ...any) error {
	return func(ctx context.Context, pool *Pool, insert string, args ...any) error {
		return way(ctx, pool, insert, args...)
	}
}

// inATransaction runs way in a transaction of the pool and commits it, on a context with a hold
// mark when marked is true. Under a mark it also requires that the transaction holds the mark
// while it is open and that its commit clears it.
func inATransaction(marked bool, way statementWay) func(context.Context, *Pool, string, ...any) error {
	return func(ctx context.Context, pool *Pool, insert string, args ...any) error {
		if marked {
			ctx = WithTransactionTracking(ctx)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if marked && !holdsConnection(ctx) {
			return errors.New("the open transaction does not hold the context's mark")
		}
		if err := way(ctx, tx, insert, args...); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if marked && holdsConnection(ctx) {
			return errors.New("the committed transaction left the context's mark held")
		}
		return nil
	}
}

var storableEntries = []storableEntry{
	{"Pool.Exec", onThePool(execWay)},
	{"Pool.Query", onThePool(queryWay)},
	{"Pool.QueryRow", onThePool(queryRowWay)},
	{"Exec in a transaction under a hold mark", inATransaction(true, execWay)},
	{"Query in a transaction under a hold mark", inATransaction(true, queryWay)},
	{"QueryRow in a transaction under a hold mark", inATransaction(true, queryRowWay)},
	{"Exec in a transaction under no mark", inATransaction(false, execWay)},
	{"Query in a transaction under no mark", inATransaction(false, queryWay)},
	{"QueryRow in a transaction under no mark", inATransaction(false, queryRowWay)},
	{"pgx.BeginFunc", func(ctx context.Context, pool *Pool, insert string, args ...any) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return execWay(ctx, tx, insert, args...) })
	}},
	{"a savepoint inside a transaction", inATransaction(false, func(ctx context.Context, db statements, insert string, args ...any) error {
		savepoint, err := db.(pgx.Tx).Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = savepoint.Rollback(ctx) }()
		if err := execWay(ctx, savepoint, insert, args...); err != nil {
			return err
		}
		return savepoint.Commit(ctx)
	})},
	{"SendBatch on a transaction", inATransaction(false, func(ctx context.Context, db statements, insert string, args ...any) error {
		batch := &pgx.Batch{}
		batch.Queue(insert, args...)
		return db.(pgx.Tx).SendBatch(ctx, batch).Close()
	})},
}

// storableTable is a store on a database of the test's own with the table every shape is written
// into: a column of each type the daemon stores text in, and a uuid and a bytea column.
func storableTable(t *testing.T) *Store {
	t.Helper()
	store := emptyStore(t)
	if _, err := store.Pool().Exec(context.Background(),
		"create table storable (key text primary key, t text, ts text[], j jsonb, u uuid, b bytea)"); err != nil {
		t.Fatalf("create the table: %v", err)
	}
	return store
}

// requireStored writes shape through entry and fails unless the row reads back what shape says.
func requireStored(t *testing.T, pool *Pool, entry storableEntry, shape storableShape) {
	t.Helper()
	ctx := context.Background()
	key := entry.name + "/" + shape.name
	if err := entry.run(ctx, pool, shape.insert, key, shape.value); err != nil {
		t.Fatalf("write %s through %s: %v", shape.name, entry.name, err)
	}
	var got any
	if err := pool.QueryRow(ctx, "select "+shape.column+" from storable where key = $1", key).Scan(&got); err != nil {
		t.Fatalf("read back %s written through %s: %v", shape.name, entry.name, err)
	}
	if !reflect.DeepEqual(got, shape.stored) {
		t.Fatalf("%s written through %s reads back %#v, want %#v", shape.name, entry.name, got, shape.stored)
	}
}

// Each NUL, each lone 0xff and, in jsonb, each \u0000 escape and lone surrogate escape is stored
// as U+FFFD, whatever Go shape carries it and whichever way the statement reaches the pool: the
// pool itself, a transaction on a context with a hold mark or with none, pgx.BeginFunc, a
// savepoint, a batch. Without the rule the server refuses each of these writes.
func TestEveryWayIntoThePoolStoresANULAnInvalidByteAndARefusedJSONEscapeAsAReplacementCharacter(t *testing.T) {
	pool := storableTable(t).Pool()
	for _, entry := range storableEntries {
		t.Run(entry.name, func(t *testing.T) {
			for _, shape := range unstorableShapes {
				t.Run(shape.name, func(t *testing.T) { requireStored(t, pool, entry, shape) })
			}
		})
	}
}

// The rule changes only what the server would refuse: a *string to a parameter that is not text,
// a clean string, the six characters \u0000 in a text column (text, not a JSON escape there) and
// a bytea value holding 0x00 are stored as they were written, through every way into the pool.
func TestTheRuleThatReplacesANULKeepsEveryValueTheServerTakesAsItIs(t *testing.T) {
	pool := storableTable(t).Pool()
	for _, entry := range storableEntries {
		t.Run(entry.name, func(t *testing.T) {
			for _, shape := range storableShapes {
				t.Run(shape.name, func(t *testing.T) { requireStored(t, pool, entry, shape) })
			}
		})
	}
}

// The argument rule gives the caller's own argument list back when no string in it needs the
// rule, a leading pgx option included, and otherwise a copy in which each string that needs it
// is a *string to the same text: the caller's list is not written.
func TestTheArgumentRuleGivesAStringWithANULAsAPointerAndLeavesEveryOtherArgument(t *testing.T) {
	clean := []any{pgx.QueryExecModeSimpleProtocol, "kept", 7, []byte{0x00}, nil, (*string)(nil)}
	if got := storableArgs(clean); &got[0] != &clean[0] || len(got) != len(clean) {
		t.Fatalf("storableArgs(%#v) = %#v, want the same list back", clean, got)
	}

	unstorable := []any{pgx.QueryExecModeSimpleProtocol, "kept", "a\x00b", "c\xffd", `{"a":"\u0000"}`, 7}
	before := append([]any(nil), unstorable...)
	got := storableArgs(unstorable)
	if !reflect.DeepEqual(unstorable, before) {
		t.Fatalf("storableArgs wrote the caller's list: %#v, was %#v", unstorable, before)
	}
	for i, arg := range unstorable {
		s, isString := arg.(string)
		if !isString || s == "kept" {
			if got[i] != arg {
				t.Errorf("argument %d = %#v, want %#v as it was", i, got[i], arg)
			}
			continue
		}
		if p, ok := got[i].(*string); !ok || *p != s {
			t.Errorf("argument %d = %#v, want a *string to %q", i, got[i], s)
		}
	}
}

// withExecMode is the test server's DSN with default_query_exec_mode set to mode.
func withExecMode(t *testing.T, mode string) string {
	t.Helper()
	dsn, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	values := dsn.Query()
	values.Set("default_query_exec_mode", mode)
	dsn.RawQuery = values.Encode()
	return dsn.String()
}

// The codec rule needs each parameter's type, which pgx learns from the server only in
// cache_statement, cache_describe and describe_exec. A DSN that names exec or simple_protocol is
// refused by PoolConfig and by Open, with an error that names the key, before any connection is
// opened; one that names cache_describe opens, and a NUL written through it is stored as U+FFFD.
func TestAnExecModeWithoutParameterTypesIsRefusedAndCacheDescribeStoresANUL(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"exec", "simple_protocol"} {
		dsn := withExecMode(t, mode)
		if _, err := PoolConfig(dsn); err == nil || !strings.Contains(err.Error(), "default_query_exec_mode") {
			t.Errorf("PoolConfig with default_query_exec_mode=%s = %v, want a refusal that names the key", mode, err)
		}
		st, err := Open(ctx, dsn)
		if err == nil {
			st.Close()
			t.Errorf("Open with default_query_exec_mode=%s opened a store, want a refusal", mode)
			continue
		}
		if !strings.Contains(err.Error(), "default_query_exec_mode") {
			t.Errorf("Open with default_query_exec_mode=%s = %v, want an error that names the key", mode, err)
		}
	}

	st := storeWith(t, map[string]string{"default_query_exec_mode": "cache_describe"})
	if mode := st.Pool().Config().ConnConfig.DefaultQueryExecMode; mode != pgx.QueryExecModeCacheDescribe {
		t.Fatalf("the store opened with default_query_exec_mode=cache_describe runs %s", mode)
	}
	var text string
	var document map[string]any
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "create temp table storable (t text, j jsonb) on commit drop"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "insert into storable (t, j) values ($1, $2)", unstorableText, unstorableJSON); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select t, j from storable").Scan(&text, &document)
	}); err != nil {
		t.Fatalf("write a NUL in cache_describe: %v", err)
	}
	if text != storedText || !reflect.DeepEqual(document, storedJSON) {
		t.Fatalf("cache_describe stored %q and %#v, want %q and %#v", text, document, storedText, storedJSON)
	}
}
