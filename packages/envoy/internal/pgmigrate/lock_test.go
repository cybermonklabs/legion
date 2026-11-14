package pgmigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testpg"
)

// What the watch reports after a sequence of readings. One empty reading of a wait that named its
// holders is the cancellation race between pg_locks and pg_blocking_pids and is held back; two in a
// row mean nobody holding the lock can be named - the holder left, or only a prepared transaction,
// which pg_blocking_pids reports as pid 0, still blocks - and a departed holder must not be named.
// Each row is the sequence that tells one term of the rule from its absence.
func TestObserveNamesOnlyAHolderTheLatestReadingsSupport(t *testing.T) {
	named := func(pid uint32) *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages", Holders: []Session{{PID: pid}}}
	}
	empty := func() *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages"}
	}
	otherLock := &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "asks"}
	otherMode := &LockWait{LockType: "relation", Mode: "ShareLock", Object: "messages"}
	for _, tc := range []struct {
		name     string
		readings []*LockWait
		want     *LockWait
	}{
		{"one empty reading as the cancellation lands keeps the holder", []*LockWait{named(97), empty()}, named(97)},
		{"two empty readings in a row name nobody", []*LockWait{named(97), empty(), empty()}, empty()},
		{"a holder named after a held-back reading replaces it, and the next empty reading is held back again", []*LockWait{named(97), empty(), named(98), empty()}, named(98)},
		{"a holder that takes over replaces the one before it", []*LockWait{named(97), named(98)}, named(98)},
		{"an empty reading of another lock replaces at once", []*LockWait{named(97), otherLock}, otherLock},
		{"an empty reading of another mode on the same table replaces at once", []*LockWait{named(97), otherMode}, otherMode},
		{"a reading of not waiting ends the run, so the next empty one is held back", []*LockWait{named(97), empty(), nil, empty()}, named(97)},
		{"a first reading that names nobody is recorded, so the refusal names the lock", []*LockWait{empty()}, empty()},
		{"a reading of not waiting after a named one keeps it, and the next empty one is held back", []*LockWait{named(97), nil, empty()}, named(97)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w watch
			for _, reading := range tc.readings {
				w.observe(reading)
			}
			if !reflect.DeepEqual(w.wait, tc.want) {
				t.Errorf("after %d readings the watch reports %+v, want %+v", len(tc.readings), w.wait, tc.want)
			}
		})
	}
}

// stallingForwarder passes bytes between one client and the Postgres server dsn names, on a
// loopback port of its own, until stall stops it: from then on it passes nothing in either
// direction and holds both connections open, so a statement written to it neither completes nor
// fails. That is the transport failure a diagnosis's cleanup must not wait on without a bound.
// Only the cleanup this test registers releases it, by closing every connection, and that runs
// after the test's own checks: a call that came back at all came back while the stall still held.
// Every goroutine the forwarder starts has ended when the test returns.
type stallingForwarder struct {
	// addr is the address to dial in the server's place.
	addr *net.TCPAddr

	mu        sync.Mutex
	stalled   bool
	swallowed bool
	conns     []net.Conn
	closing   bool
	wg        sync.WaitGroup
}

// stall stops the forwarder passing bytes, in both directions, for the rest of the test.
func (f *stallingForwarder) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalled = true
}

// holding reports whether the stall has taken bytes the client wrote and passed them to nobody:
// that something the client sent is waiting on this transport and waits on it still.
func (f *stallingForwarder) holding() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.swallowed
}

// track holds every connection the forwarder opens: a pump blocked reading one ends only when
// that connection closes, which the test's cleanup does for all of them.
func (f *stallingForwarder) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		conn.Close()
		return false
	}
	f.conns = append(f.conns, conn)
	return true
}

// pump passes what it reads from src on to dst until either end stops or the stall begins. The
// chunk the stall meets reaches nobody, and the forwarder records that it swallowed it.
func (f *stallingForwarder) pump(dst, src net.Conn) {
	defer f.wg.Done()
	buf := make([]byte, 16*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			f.mu.Lock()
			halted := f.stalled
			if halted {
				f.swallowed = true
			}
			f.mu.Unlock()
			if halted {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// startForwarder puts a stallingForwarder in front of the server dsn names and returns it,
// listening and passing everything on until something calls stall.
func startForwarder(t *testing.T, dsn string) *stallingForwarder {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	network, address := pgconn.NetworkAddress(config.Host, config.Port)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &stallingForwarder{addr: listener.Addr().(*net.TCPAddr)}
	t.Cleanup(func() {
		listener.Close()
		f.mu.Lock()
		f.closing = true
		for _, conn := range f.conns {
			conn.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			if !f.track(client) {
				return
			}
			server, err := net.Dial(network, address)
			if err != nil {
				t.Errorf("dial the test database: %v", err)
				client.Close()
				return
			}
			if !f.track(server) {
				return
			}
			f.wg.Add(2)
			go f.pump(server, client)
			go f.pump(client, server)
		}
	}()
	return f
}

// stallAtTheCleanup stalls the forwarder at the first statement run on a context other than the
// one the diagnosis's reads ran on. Every statement of the diagnosis - its transaction, each
// holder's savepoint, the diagnosis query and that savepoint's rollback - runs on the one context
// diagnoseHolders reads under, and the cleanup's rollback is the first that does not, because
// diagnoseHolders gives the rollback and the close a context of their own
// (diagnosisCleanupTimeout). So the stall begins once the diagnosis has been read and before any
// byte of the cleanup leaves pgx, which traces a statement before it writes it. Nothing here
// reads the wire: what the cleanup sends, and whether pgx sends anything at all, does not decide
// when the forwarder stops.
type stallAtTheCleanup struct {
	forwarder *stallingForwarder

	mu    sync.Mutex
	armed bool
	seen  bool
	reads <-chan struct{}
}

// arm starts the watch, once the connection is up: every statement from here is the diagnosis's
// or its cleanup's, and none is signing in.
func (s *stallAtTheCleanup) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *stallAtTheCleanup) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.armed:
	case !s.seen:
		s.seen, s.reads = true, ctx.Done()
	case ctx.Done() != s.reads:
		s.forwarder.stall()
	}
	return ctx
}

func (s *stallAtTheCleanup) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A diagnosis whose cleanup cannot finish returns anyway, with what it read. diagnoseHolders rolls
// its read-only transaction back and closes its connection on a context of their own, bounded by
// diagnosisCleanupTimeout, so a transport that takes the rollback and answers it never - neither
// completing nor failing - holds the runner up a second at most, not for as long as that
// transport lives, and the holder the diagnosis read before the stall is still named. The stall
// outlives the answer: the forwarder is released only by its own cleanup, after the checks below,
// and those checks fail a call that came back with nothing of its cleanup still held.
func TestDiagnoseHoldersReturnsWhenTheCleanupCannotFinish(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.URL(t)
	// The session the diagnosis reads. It stays connected, so pg_stat_activity has a row for it.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	var pid uint32
	if err := holder.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read the holder's pid: %v", err)
	}

	forwarder := startForwarder(t, dsn)
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	config.Host, config.Port = forwarder.addr.IP.String(), uint16(forwarder.addr.Port)
	// Everything the diagnosis sends has to cross the forwarder: a fallback dials the address the
	// URL named and would reach the server behind its back, and TLS over a loopback hop buys
	// nothing.
	config.TLSConfig, config.Fallbacks = nil, nil
	tracer := &stallAtTheCleanup{forwarder: forwarder}
	config.Tracer = tracer
	dial := func(ctx context.Context) (*pgx.Conn, error) {
		conn, err := pgx.ConnectConfig(ctx, config)
		if err == nil {
			tracer.arm()
		}
		return conn, err
	}

	// What the whole call may take: the diagnosis's own reads, a handful of round trips on a
	// local server, and then the cleanup, which diagnosisCleanupTimeout bounds at one second. The
	// three seconds are that second and two more for a loaded machine, written out rather than
	// read from diagnosisCleanupTimeout so that raising that bound reddens this row instead of
	// moving it: a cleanup left to LockTimeout's five seconds has to fail here.
	const bound = 3 * time.Second
	start := time.Now()
	done := make(chan []string, 1)
	go func() { done <- diagnoseHolders(ctx, dial, []Session{{PID: pid}}) }()
	select {
	case diagnoses := <-done:
		returned := time.Since(start)
		// The forwarder is released by its own cleanup, which runs after this check, so a
		// forwarder still holding the bytes of the rollback is one this call never waited for.
		if !forwarder.holding() {
			t.Fatalf("diagnoseHolders returned after %v and the forwarder had swallowed nothing: the cleanup was never held open, so this row says nothing about its bound", returned)
		}
		want := fmt.Sprintf("diagnosis of pid %d, read when refused: ", pid)
		if len(diagnoses) != 1 || !strings.HasPrefix(diagnoses[0], want) {
			t.Fatalf("diagnoseHolders returned %q after %v, want one diagnosis starting %q", diagnoses, returned, want)
		}
	case <-time.After(bound):
		t.Fatalf("diagnoseHolders has not returned in %v: its cleanup is waiting on a transport that passes nothing", bound)
	}
}

// Exec reads a migration as the census read it, whatever the runner's connection carries, because
// it sets the text settings (textSettings) transaction-local before the migration runs. Each row
// gives the runner's connection one setting at a value the census does not read under: a session
// SET, which an earlier migration can leave on a pooled connection, a connection option, or a
// database default. The row's migration is one insert of one string constant to the census's
// parser, and under that setting Postgres reads it as two inserts, or refuses it. Each row applies
// the migration through Exec in a transaction and commits, then reads the table on a connection of
// its own in UTF-8: it must hold one row, the constant the census's parser read. A connection left
// in SJIS cannot read Á back, so the runner's connection does not read the table. The runner's
// connection must still carry the row's setting after the commit, since Exec's settings end with
// its transaction.
func TestExecReadsAMigrationAsTheCensusReadsIt(t *testing.T) {
	ctx := context.Background()
	// The census's parser reads each text below as one insert of the one constant after it. Under
	// standard_conforming_strings off, \' escapes the quote after it: 'a\'' is a', and the rest of
	// the text is a second insert and a comment.
	const backslashBeforeQuote = `insert into r values ('a\'');insert into r values ($$x$$);--')`
	const backslashBeforeQuoteValue = `a\');insert into r values ($$x$$);--`
	// Under client_encoding SJIS, the second byte of Á (C3 81 in UTF-8) and the backslash after it
	// are one character, so the backslash escapes nothing and the quote after it ends the literal.
	const backslashAfterNonASCII = `insert into r values (E'Á\');insert into r values ($$x$$);--')`
	const backslashAfterNonASCIIValue = `Á');insert into r values ($$x$$);--`
	// Under backslash_quote off, Postgres refuses the \' (SQLSTATE 22P06).
	const escapedQuote = `insert into r values (E'a\'')`
	const escapedQuoteValue = `a'`
	// sessionSet connects to url and runs set on the connection, for the session.
	sessionSet := func(set string) func(*testing.T, string) *pgx.Conn {
		return func(t *testing.T, url string) *pgx.Conn {
			conn := connect(t, ctx, url)
			if _, err := conn.Exec(ctx, set); err != nil {
				t.Fatalf("%s: %v", set, err)
			}
			return conn
		}
	}
	for _, row := range []struct {
		name string
		// runner connects to the row's database, which url names, and gives the connection the
		// row's setting.
		runner func(t *testing.T, url string) *pgx.Conn
		// setting is the setting the row changes, and value its value on the runner's connection.
		setting, value string
		// want is the one constant the census's parser reads in sql.
		sql, want string
	}{
		{"a session set of standard_conforming_strings off", sessionSet("set standard_conforming_strings = off"), "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a connection option of standard_conforming_strings off", func(t *testing.T, url string) *pgx.Conn {
			config, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			config.RuntimeParams["options"] = "-c standard_conforming_strings=off"
			conn, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatalf("connect with standard_conforming_strings off: %v", err)
			}
			t.Cleanup(func() { conn.Close(context.Background()) })
			return conn
		}, "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a database default of standard_conforming_strings off", func(t *testing.T, url string) *pgx.Conn {
			config, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			if _, err := connect(t, ctx, url).Exec(ctx, "alter database "+pgx.Identifier{config.Database}.Sanitize()+" set standard_conforming_strings = off"); err != nil {
				t.Fatalf("set the database's default: %v", err)
			}
			return connect(t, ctx, url)
		}, "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a session set of client_encoding SJIS", sessionSet("set client_encoding = 'SJIS'"), "client_encoding", "SJIS", backslashAfterNonASCII, backslashAfterNonASCIIValue},
		{"a session set of backslash_quote off", sessionSet("set backslash_quote = off"), "backslash_quote", "off", escapedQuote, escapedQuoteValue},
	} {
		t.Run(row.name, func(t *testing.T) {
			statements, ok := parseStatements(row.sql)
			if !ok || len(statements) != 1 {
				t.Fatalf("the census's parser read %d statements (ok %v), want one", len(statements), ok)
			}
			var tree any
			if err := json.Unmarshal(statements[0].body, &tree); err != nil {
				t.Fatalf("read the parse tree: %v", err)
			}
			var census []string
			stringConstants(tree, &census)
			if !reflect.DeepEqual(census, []string{row.want}) {
				t.Fatalf("the census's parser read the string constants %q, want %q", census, row.want)
			}

			url := testpg.URL(t)
			readerConfig, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			readerConfig.RuntimeParams["client_encoding"] = "UTF8"
			reader, err := pgx.ConnectConfig(ctx, readerConfig)
			if err != nil {
				t.Fatalf("connect the reader: %v", err)
			}
			t.Cleanup(func() { reader.Close(context.Background()) })
			if _, err := reader.Exec(ctx, "create table r (v text)"); err != nil {
				t.Fatal(err)
			}

			runner := row.runner(t, url)
			carries := func(when string) {
				t.Helper()
				var value string
				if err := runner.QueryRow(ctx, "select current_setting($1)", row.setting).Scan(&value); err != nil || value != row.value {
					t.Fatalf("%s, the runner's connection has %s = %q, %v; want %q", when, row.setting, value, err, row.value)
				}
			}
			carries("before Exec")
			tx, err := runner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := Exec(ctx, tx, Migration{Version: 1, Name: "0001_r.up.sql", SQL: row.sql}, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			carries("after the commit")

			rows, err := reader.Query(ctx, "select v from r")
			if err != nil {
				t.Fatal(err)
			}
			stored, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, census) {
				t.Fatalf("r holds %q, want %q, the one constant the census's parser read", stored, census)
			}
		})
	}
}

// stringConstants appends to found every string constant of a parse tree, read from libpg_query's
// JSON: an A_Const writes one as {"sval": {"sval": "…"}}.
func stringConstants(node any, found *[]string) {
	switch node := node.(type) {
	case map[string]any:
		for key, value := range node {
			if s, ok := value.(string); ok && key == "sval" {
				*found = append(*found, s)
				continue
			}
			stringConstants(value, found)
		}
	case []any:
		for _, value := range node {
			stringConstants(value, found)
		}
	}
}
