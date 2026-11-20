package storetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pg_locks holds every lock of every database on the server, while a lock is taken within one
// database. A test's database is one of many on its server: Open clones one per test from one
// template, and the packages of a run, and runs beside it, test against the same server at once.
// So the same table has the same oid in every clone, every test that files an issue in a project
// of the same key files the same issue key, and the same key names the same advisory lock in
// every database. A read of pg_locks that does not keep to its own database counts another
// test's waiter, or names another test's holder, as its own: the API's upload lock probe once
// named another test's spec settlement as the holder of its issue's owner lock, and failed.
//
// Waiting and Holders are the reads that keep to the database they run in, of a Lock that names
// what they look for. Hold, HoldOn and Queue plant that same Lock in a database a test names,
// another clone of its own (Open) among them, and ConnectInListingOrder opens the connections a
// test needs when the order the server lists holders in matters, so a test shows its read counts
// its own database's lock and not another's. internal/textcheck's lock_reads_test.go refuses a
// read of pg_locks or pg_stat_activity in test code whose query block keeps to no database or
// backend of its own, as that test's comment spells out, with what it cannot see.
//
// Every call here to the server but one has a deadline, each derived from what the call waits for:
// answerWait for a call that waits for no lock, holdWait for HoldOn's lock and queueWait for
// Queue's read of its waiter. A call past its deadline fails the test naming what it waited for,
// and HoldOn's failure names the lock's holders, so a server that stopped answering or a lock that
// never frees is a named failure rather than a test binary that hangs until go test's own timeout.
// One wait has no deadline, by design: Queue's waiter asks for its lock until it is cancelled, by
// the end of its test's context or by Queue's cleanup, which cancels it before its connection
// closes; that wait is the plant, and nothing waits on it.

// thisDatabase keeps a read of pg_locks, aliased l, to the rows of the database it runs in.
const thisDatabase = `l.database = (select oid from pg_database where datname = current_database())`

// answerWait bounds a call here that waits for no lock, only for the server to answer it:
// opening a connection, and reading pg_locks or pg_stat_activity, which takes no lock a test can
// hold. The server answers such a call in milliseconds, and in a few seconds on a loaded CI runner
// or box, where opening a connection is the slowest of them, since the server starts a process
// for it. Thirty seconds is far past that and far inside a package's go test timeout.
const answerWait = 30 * time.Second

// holdWait bounds HoldOn's wait for its lock and for the statement it then runs. A test plants a
// lock that is free in its database, or that a writer of the test's own setup holds for as long
// as that writer's transaction runs: the settlement of the spec document an issue's creation
// wrote holds the issue's owner lock for some milliseconds, and the API's upload lock probe test
// waits that settlement out. The statement is a read that waits for no lock. Twenty seconds is the
// window the API's upload lock probe gives the same writers (uploadLockProbeWindow): a lock still
// held past it is held by something that does not let go.
const holdWait = 20 * time.Second

// queueWait bounds Queue's wait for its waiter to show in pg_locks. The waiter asks for a lock
// that Queue's own holder took just before, so its request joins the lock's queue as soon as the
// server runs it, which takes no longer than any other statement that waits for no lock.
const queueWait = 10 * time.Second

// Lock is one lock as pg_locks shows it: the predicate over pg_locks, aliased l, that names its
// rows, and how a transaction takes it, so the lock a read looks for and the lock a test plants
// cannot drift apart.
type Lock struct {
	name  string
	match string
	args  []any
	take  func(ctx context.Context, tx pgx.Tx) error
}

func (l Lock) String() string { return l.name }

// RelationLock is a table's lock, by its name: every row pg_locks holds on the table, a lock of
// the relation or of one of its rows, as a test that waits on the table counts them. A
// transaction takes it in access exclusive mode, which every other mode waits behind.
func RelationLock(table string) Lock {
	return Lock{
		name:  "table " + table,
		match: `l.relation = $1::regclass`,
		args:  []any{table},
		take: func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "lock table "+pgx.Identifier{table}.Sanitize()+" in access exclusive mode")
			return err
		},
	}
}

// AdvisoryLock is the advisory lock of one bigint key, as pg_advisory_xact_lock(bigint) takes it:
// pg_locks shows the key's high half as classid and its low half as objid.
func AdvisoryLock(key int64) Lock {
	return Lock{
		name:  fmt.Sprintf("advisory lock %d", key),
		match: `l.locktype = 'advisory' and ((l.classid::bigint << 32) | l.objid::bigint) = $1`,
		args:  []any{key},
		take: func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1::bigint)`, key)
			return err
		},
	}
}

// AdvisoryLockPair is the advisory lock of two integer keys, as pg_advisory_xact_lock(integer,
// integer) takes it: pg_locks shows the first as classid and the second as objid, each as the
// unsigned 32-bit value an oid carries, with objsubid 2.
func AdvisoryLockPair(key1, key2 int32) Lock {
	return Lock{
		name: fmt.Sprintf("advisory lock (%d, %d)", key1, key2),
		match: `l.locktype = 'advisory' and l.objsubid = 2
			and l.classid = ($1::bigint & 4294967295)::oid and l.objid = ($2::bigint & 4294967295)::oid`,
		args: []any{int64(key1), int64(key2)},
		take: func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1::integer, $2::integer)`, key1, key2)
			return err
		},
	}
}

// OwnerLock is owner's lock (store.LockOwner): the two-key advisory lock whose first key is
// store.OwnerLockNamespace and whose second is the hashtext of the owner's lock key
// (store.Owner.LockKey). Both come from store rather than being spelled again here, so a change
// to either reaches this read instead of leaving it looking for a lock nothing takes any more. A
// transaction takes it through store.LockOwner, as every writer of the owner does.
func OwnerLock(owner store.Owner) Lock {
	return Lock{
		name: fmt.Sprintf("the owner lock of %s %s", owner.Kind, owner.Key),
		match: `l.locktype = 'advisory' and l.objsubid = 2
			and l.classid = ($1::bigint & 4294967295)::oid and l.objid = (hashtext($2)::bigint & 4294967295)::oid`,
		args: []any{int64(store.OwnerLockNamespace), owner.LockKey()},
		take: func(ctx context.Context, tx pgx.Tx) error { return store.LockOwner(ctx, tx, owner) },
	}
}

// Waiting counts the backends of the database pool reads that wait for lock: its ungranted rows
// in pg_locks, of that database alone. It waits for the answer at most answerWait, within ctx.
func Waiting(ctx context.Context, pool *store.Pool, lock Lock) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, answerWait)
	defer cancel()
	var waiting int
	if err := pool.QueryRow(ctx, `select count(*) from pg_locks l where not l.granted and `+thisDatabase+` and (`+lock.match+`)`, lock.args...).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("count the waiters on %s, waiting at most %s for the answer: %w", lock, answerWait, err)
	}
	return waiting, nil
}

// WaitersOn is Waiting of lock on database, within t's context, failing t on a failed read.
func WaitersOn(t testing.TB, database *store.Store, lock Lock) int {
	t.Helper()
	waiting, err := Waiting(t.Context(), database.Pool, lock)
	if err != nil {
		t.Fatal(err)
	}
	return waiting
}

// LockHolder is a backend holding a lock, with what pg_stat_activity shows of it: its state, the
// start of its transaction (nil for none) and its last statement.
type LockHolder struct {
	PID       int32
	State     string
	XactStart *time.Time
	Query     string
}

func (h LockHolder) String() string {
	start := "null"
	if h.XactStart != nil {
		start = h.XactStart.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("backend %d, state %q, xact_start %s, query %q", h.PID, h.State, start, h.Query)
}

// Holders names every backend of the database pool reads that holds lock, granted, in pid order,
// the reading backend left out. Every one, not the first the server lists: that order is the
// order of the server's backend slots, which no test decides, and a lock held shared has several
// holders. A holder that ended between a caller's refusal and this read is simply not named. It
// waits for the answer at most answerWait, within ctx.
func Holders(ctx context.Context, pool *store.Pool, lock Lock) ([]LockHolder, error) {
	ctx, cancel := context.WithTimeout(ctx, answerWait)
	defer cancel()
	return readHolders(ctx, pool, lock)
}

// querier is what readHolders reads through: a store's pool (Holders), or a connection of its own
// (HoldOn's failure).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readHolders is Holders' read, through db, within ctx.
func readHolders(ctx context.Context, db querier, lock Lock) ([]LockHolder, error) {
	rows, err := db.Query(ctx, `
		select a.pid, a.state, a.xact_start, a.query
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.granted and a.pid <> pg_backend_pid() and `+thisDatabase+` and (`+lock.match+`)
		order by a.pid
	`, lock.args...)
	if err != nil {
		return nil, fmt.Errorf("read the holders of %s, waiting at most %s for the answer: %w", lock, answerWait, err)
	}
	holders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LockHolder, error) {
		var holder LockHolder
		var state, query *string
		if err := row.Scan(&holder.PID, &state, &holder.XactStart, &query); err != nil {
			return holder, err
		}
		if state != nil {
			holder.State = *state
		}
		if query != nil {
			holder.Query = *query
		}
		return holder, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the holders of %s, waiting at most %s for the answer: %w", lock, answerWait, err)
	}
	return holders, nil
}

// listingTries bounds how many rounds ConnectInListingOrder opens connections in before it gives
// up.
const listingTries = 64

// Hold takes lock in a transaction on a connection of its own to database and then runs then
// with args in it (HoldOn). It answers the holder's backend.
func Hold(t testing.TB, database *store.Store, lock Lock, then string, args ...any) uint32 {
	t.Helper()
	return HoldOn(t, connect(t, database), lock, then, args...)
}

// HoldOn takes lock in a transaction on conn and then runs then with args in it, so
// pg_stat_activity shows then as the holder's last statement; an empty then runs nothing more.
// then's rows are read and dropped, and it may answer none. The transaction stays open, and the
// lock held, until conn closes, which a connection ConnectInListingOrder opened does when t ends.
// It answers the holder's backend. It waits for the lock and the statement at most holdWait,
// within t's context, and fails t past that naming the lock's holders in conn's database.
func HoldOn(t testing.TB, conn *pgx.Conn, lock Lock, then string, args ...any) uint32 {
	t.Helper()
	return holdOn(t, conn, lock, holdWait, then, args...)
}

// holdOn is HoldOn with its bound given, so a test of the bound need not wait holdWait out.
func holdOn(t testing.TB, conn *pgx.Conn, lock Lock, within time.Duration, then string, args ...any) uint32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds %s, waiting at most %s: %v", lock, within, err)
	}
	if err := lock.take(ctx, tx); err != nil {
		t.Fatalf("take %s, waiting at most %s: %v; %s", lock, within, err, holdersBeside(t, conn, lock))
	}
	if then != "" {
		rows, err := tx.Query(ctx, then, args...)
		if err != nil {
			t.Fatalf("run %q under %s, waiting at most %s: %v", then, lock, within, err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("run %q under %s, waiting at most %s: %v", then, lock, within, err)
		}
	}
	return conn.PgConn().PID()
}

// holdersBeside names lock's holders in conn's database, read on a connection of its own, for
// HoldOn's failure: conn itself is closed once a wait on it passes its deadline (pgx's default
// context watcher ends the connection).
func holdersBeside(t testing.TB, conn *pgx.Conn, lock Lock) string {
	ctx, cancel := context.WithTimeout(t.Context(), answerWait)
	defer cancel()
	reader, err := pgx.ConnectConfig(ctx, conn.Config())
	if err != nil {
		return fmt.Sprintf("its holders could not be read: connect, waiting at most %s: %v", answerWait, err)
	}
	defer closeWithin(reader)
	holders, err := readHolders(ctx, reader, lock)
	switch {
	case err != nil:
		return fmt.Sprintf("its holders could not be read: %v", err)
	case len(holders) == 0:
		return "no backend of its database holds it now"
	}
	named := make([]string, len(holders))
	for i, holder := range holders {
		named[i] = holder.String()
	}
	return "it is held in its database by " + strings.Join(named, "; ")
}

// ConnectInListingOrder opens a connection to first and one to second, each closed when t ends,
// such that pg_stat_activity lists first's backend before second's: so a read that names the
// first holder the server lists, as a hash join with pg_stat_activity on its outer side does,
// meets a lock held on the first before one held on the second. That order is the order of the
// server's backend slots, and a new connection takes whichever slot the server hands out next,
// which no test decides and which can run high or low for a while. So it opens one connection to
// each in turn, the two in alternating order, keeps them all open while it looks (a slot closed
// would be handed out again), and answers the earliest-listed connection to first and the
// latest-listed one to second once the first is listed before the second, closing the others. A
// connection whose backend has gone from the listing is dead, and it fails t naming that backend.
// It fails t after listingTries rounds.
//
// The listing order is not the order of the backends' process ids, which rise as connections
// open: in a run of slots handed out high to low, every connection opened later is listed earlier
// and has the higher process id, so no one pair is first in both orders. A read that names the
// holder with the lowest process id meets first a holder whose connection opened before every
// other (Hold, called before this), unless the ids wrapped between the two: they start again low
// once they pass the kernel's pid_max, which on a server that shares its host's process ids comes
// every few minutes on a busy host. A test that needs that order reads the two ids, as the API's
// upload lock probe tests do.
func ConnectInListingOrder(t testing.TB, first, second *store.Store) (*pgx.Conn, *pgx.Conn) {
	t.Helper()
	var firsts, seconds []*pgx.Conn
	for round := range listingTries {
		if round%2 == 0 {
			firsts = append(firsts, connect(t, first))
			seconds = append(seconds, connect(t, second))
		} else {
			seconds = append(seconds, connect(t, second))
			firsts = append(firsts, connect(t, first))
		}
		all := append(slices.Clone(firsts), seconds...)
		ctx, cancel := context.WithTimeout(t.Context(), answerWait)
		positions, err := listingPositions(ctx, first.Pool, all)
		cancel()
		if err != nil {
			t.Fatalf("read where the server lists the connections' backends, waiting at most %s: %v", answerWait, err)
		}
		for _, conns := range []struct {
			database string
			conns    []*pgx.Conn
		}{{"first", firsts}, {"second", seconds}} {
			for _, conn := range conns.conns {
				if positions[conn] == 0 {
					t.Fatalf("backend %d, of a connection opened to the %s database, is gone from the server's listing (pg_stat_get_activity) in round %d: its connection is dead, so no order holds on it", conn.PgConn().PID(), conns.database, round)
				}
			}
		}
		early := slices.MinFunc(firsts, func(a, b *pgx.Conn) int { return cmp.Compare(positions[a], positions[b]) })
		late := slices.MaxFunc(seconds, func(a, b *pgx.Conn) int { return cmp.Compare(positions[a], positions[b]) })
		if positions[early] >= positions[late] {
			continue
		}
		for _, conn := range all {
			if conn != early && conn != late {
				closeWithin(conn)
			}
		}
		return early, late
	}
	t.Fatalf("no connection to the first database of %d opened is listed before one of %d to the second", len(firsts), len(seconds))
	return nil, nil
}

// Queue has a backend of database wait for lock: one connection takes the lock (Hold) and a
// second asks for it, both until t ends, the second having taken each of holding first, in the
// same transaction, for a read that asks what a waiter holds as well. It answers once the second
// waits, read off that backend's own row in pg_locks, so a read that counts waiters wrongly cannot
// end the wait early, and it answers the waiting backend. It waits for that at most queueWait.
func Queue(t testing.TB, database *store.Store, lock Lock, holding ...Lock) uint32 {
	t.Helper()
	Hold(t, database, lock, "")
	waiter := connect(t, database)
	pid := waiter.PgConn().PID()
	// The waiter asks for the lock until t's context ends, just before t's cleanups run.
	ctx, cancel := context.WithCancel(t.Context())
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		tx, err := waiter.Begin(ctx)
		if err != nil {
			return
		}
		for _, held := range holding {
			if err := held.take(ctx, tx); err != nil {
				return
			}
		}
		// It returns once the holder's connection closes, or cancel stops the wait.
		_ = lock.take(ctx, tx)
	}()
	// Registered after the waiter's connection, so it runs first: the wait ends before the
	// connection closes under it.
	t.Cleanup(func() {
		cancel()
		<-asked
	})
	read, stop := context.WithTimeout(t.Context(), queueWait)
	defer stop()
	for {
		var waiting bool
		if err := database.Pool.QueryRow(read, `select exists (select 1 from pg_locks where pid = $1 and not granted)`, int32(pid)).Scan(&waiting); err != nil {
			t.Fatalf("read whether backend %d waits for %s, waiting at most %s for it to: %v", pid, lock, queueWait, err)
		}
		if waiting {
			return pid
		}
		if read.Err() != nil {
			t.Fatalf("backend %d never waited for %s within %s", pid, lock, queueWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// connect opens a connection of its own to database, closed when t ends, which rolls back any
// transaction left open on it. It waits for the server at most answerWait, within t's context.
func connect(t testing.TB, database *store.Store) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), answerWait)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect to the database, waiting at most %s: %v", answerWait, err)
	}
	t.Cleanup(func() { closeWithin(conn) })
	return conn
}

// closeWithin closes conn, waiting at most answerWait for the server to take its goodbye. It runs
// in t's cleanups too, after t's context has ended, so its deadline is its own.
func closeWithin(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), answerWait)
	defer cancel()
	_ = conn.Close(ctx)
}

// listingPositions is where pg_stat_activity lists each of conns' backends: the order of the
// server's backend slots, which pg_stat_get_activity reads them in, counted from 1 by its
// ordinality, which numbers every backend the function lists before the filter keeps conns'. A
// backend that is gone is not in the answer, so its position reads 0.
func listingPositions(ctx context.Context, pool *store.Pool, conns []*pgx.Conn) (map[*pgx.Conn]int64, error) {
	pids := make([]int32, len(conns))
	for i, conn := range conns {
		pids[i] = int32(conn.PgConn().PID())
	}
	rows, err := pool.Query(ctx, `
		select pid, ordinality from pg_stat_get_activity(null) with ordinality where pid = any($1)
	`, pids)
	if err != nil {
		return nil, err
	}
	byPID := map[int32]int64{}
	var pid int32
	var position int64
	if _, err := pgx.ForEachRow(rows, []any{&pid, &position}, func() error {
		byPID[pid] = position
		return nil
	}); err != nil {
		return nil, err
	}
	positions := make(map[*pgx.Conn]int64, len(conns))
	for i, conn := range conns {
		positions[conn] = byPID[pids[i]]
	}
	return positions, nil
}
