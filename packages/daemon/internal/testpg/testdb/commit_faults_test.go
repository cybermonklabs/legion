package testdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// faultRig is one proxy test's database: proxied reaches it through CommitFaults' proxy, and
// observer reaches it directly, so the observer counts what committed whatever the proxy did to
// proxied. Both are single connections, so the database is PlainDatabase's, whose URL holds no
// pool key for pgx.Connect to send to the server.
type faultRig struct {
	through  string
	fault    *CommitFault
	proxied  *pgx.Conn
	observer *pgx.Conn
}

func newFaultRig(t *testing.T) faultRig {
	t.Helper()
	dsn := PlainDatabase(t, "legion_testdb_faults")
	through, fault := CommitFaults(t, dsn)
	rig := faultRig{through: through, fault: fault, proxied: connect(t, through), observer: connect(t, dsn)}
	if _, err := rig.observer.Exec(context.Background(), "create table fault_probe (id int primary key)"); err != nil {
		t.Fatalf("create fault_probe: %v", err)
	}
	return rig
}

// connect is a connection to dsn, closed when the test ends.
func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// begun is a transaction on the proxied connection that has run sql: the statements before a
// COMMIT, which the proxy passes as they are.
func (r faultRig) begun(t *testing.T, sql string, args ...any) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := r.proxied.Begin(ctx)
	if err != nil {
		t.Fatalf("begin through the proxy: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s through the proxy: %v", sql, err)
	}
	return tx
}

// probes is how many rows of fault_probe the observer reads.
func (r faultRig) probes(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.observer.QueryRow(context.Background(), "select count(*) from fault_probe").Scan(&n); err != nil {
		t.Fatalf("count fault_probe: %v", err)
	}
	return n
}

// committed is the answer of one Commit, and how long that Commit took.
type committed struct {
	err  error
	took time.Duration
}

// commit runs tx's Commit on a goroutine of its own, so the test can wait on the fault's channel
// while the COMMIT is held or cut. A test that ends before that Commit returns waits for it in a
// cleanup, which the proxy's close at the end of the test's context lets return, so the
// connection's own cleanup never runs beside the Commit.
func commit(t *testing.T, tx pgx.Tx) <-chan committed {
	answer := make(chan committed, 1)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		err := tx.Commit(context.Background())
		answer <- committed{err: err, took: time.Since(start)}
	}()
	t.Cleanup(func() { <-done })
	return answer
}

// settled is what Commit answered, once it does; a Commit that never answers fails the test.
func settled(t *testing.T, answer <-chan committed) committed {
	t.Helper()
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case got := <-answer:
		return got
	case <-time.After(bound):
		t.Fatalf("Commit through the proxy did not return within %s", bound)
		return committed{}
	}
}

func TestAnUnarmedProxyPassesACommit(t *testing.T) {
	r := newFaultRig(t)
	if err := r.begun(t, "insert into fault_probe values (1)").Commit(context.Background()); err != nil {
		t.Fatalf("Commit through an unarmed proxy: %v", err)
	}
	if got := r.probes(t); got != 1 {
		t.Fatalf("the observer counts %d rows after the commit, want 1", got)
	}
}

func TestHoldReplyHoldsTheAnswerOfAVisibleCommit(t *testing.T) {
	r := newFaultRig(t)
	const hold = 300 * time.Millisecond
	fired := r.fault.HoldReply("insert into fault_probe", hold)
	answer := commit(t, r.begun(t, "insert into fault_probe values (1)"))
	Met(t, fired)
	testwait.Eventually(t, "the observer to count the row whose COMMIT reached the server", func() bool {
		return r.probes(t) == 1
	})
	select {
	case got := <-answer:
		t.Fatalf("Commit returned (%v) before the observer counted its row: the proxy held no reply", got.err)
	default:
	}
	got := settled(t, answer)
	if got.err != nil {
		t.Fatalf("Commit whose reply was held: %v", got.err)
	}
	if got.took < hold {
		t.Fatalf("Commit whose reply was held for %s returned after %s", hold, got.took)
	}
}

func TestCutAfterCommitBreaksTheClientOfACommittedTransaction(t *testing.T) {
	r := newFaultRig(t)
	fired := r.fault.Cut("insert into fault_probe", CutAfterCommit)
	answer := commit(t, r.begun(t, "insert into fault_probe values (1)"))
	Met(t, fired)
	got := settled(t, answer)
	if got.err == nil {
		t.Fatal("Commit whose reply was dropped returned nil, want the broken connection's error")
	}
	if pgErr := (*pgconn.PgError)(nil); errors.As(got.err, &pgErr) {
		t.Fatalf("Commit whose reply was dropped returned the server's answer %v, want the broken connection's error", got.err)
	}
	testwait.Eventually(t, "the observer to count the row whose reply was dropped", func() bool {
		return r.probes(t) == 1
	})
}

func TestCutBeforeCommitRollsTheTransactionBack(t *testing.T) {
	r := newFaultRig(t)
	backend := r.proxied.PgConn().PID()
	fired := r.fault.Cut("insert into fault_probe", CutBeforeCommit)
	answer := commit(t, r.begun(t, "insert into fault_probe values (1)"))
	Met(t, fired)
	if got := settled(t, answer); got.err == nil {
		t.Fatal("Commit whose COMMIT never reached the server returned nil, want an error")
	}
	// The server ends the transaction's backend once it reads the closed connection, and a COMMIT
	// it had been sent would have run before that: the count after its end is final.
	testwait.Eventually(t, "the server to end the backend of the cut connection", func() bool {
		var left int
		if err := r.observer.QueryRow(context.Background(),
			"select count(*) from pg_stat_activity where pid = $1", backend).Scan(&left); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return left == 0
	})
	if got := r.probes(t); got != 0 {
		t.Fatalf("the observer counts %d rows of a transaction whose COMMIT never reached the server, want 0", got)
	}
}

func TestCutAfterCommitRefusingRefusesTheNextConnection(t *testing.T) {
	r := newFaultRig(t)
	fired := r.fault.Cut("insert into fault_probe", CutAfterCommitRefusing)
	answer := commit(t, r.begun(t, "insert into fault_probe values (1)"))
	Met(t, fired)
	settled(t, answer)
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, faultMetBound, time.Second))
	defer cancel()
	if conn, err := pgx.Connect(ctx, r.through); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("a connection through the refusing proxy succeeded, want it closed at once")
	}
	r.fault.Refuse(false)
	conn, err := pgx.Connect(ctx, r.through)
	if err != nil {
		t.Fatalf("connect through the proxy after Refuse(false): %v", err)
	}
	_ = conn.Close(ctx)
}

func TestAFaultMeetsOnlyAMatchingTransaction(t *testing.T) {
	r := newFaultRig(t)
	fired := r.fault.HoldReply("insert into fault_probe", time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, faultMetBound, time.Second))
	defer cancel()
	if err := r.begun(t, "select 1").Commit(ctx); err != nil {
		t.Fatalf("Commit of a transaction that never named fault_probe: %v", err)
	}
	select {
	case <-fired:
		t.Fatal("the fault met the COMMIT of a transaction that never named fault_probe")
	default:
	}
}

// pgx prepares a statement it runs with arguments once, under a name of its own, and runs it again
// with a Bind message alone: the second transaction below sends no SQL text but its BEGIN and its
// COMMIT, so the fault meets it only through the statement that Bind names.
func TestAFaultMatchesACachedStatementByItsBind(t *testing.T) {
	r := newFaultRig(t)
	const insert = "insert into fault_probe values ($1)"
	if err := r.begun(t, insert, 1).Commit(context.Background()); err != nil {
		t.Fatalf("the first commit: %v", err)
	}
	fired := r.fault.HoldReply("insert into fault_probe", 100*time.Millisecond)
	answer := commit(t, r.begun(t, insert, 2))
	Met(t, fired)
	if got := settled(t, answer); got.err != nil {
		t.Fatalf("the second commit: %v", got.err)
	}
	if got := r.probes(t); got != 2 {
		t.Fatalf("the observer counts %d rows after both commits, want 2", got)
	}
}

// A test that ends while a reply is held ends with it. The pool here is opened after the proxy, so
// its Close runs before the proxy's own cleanup and waits for the connection the held Commit has:
// the proxy closes as the test's context ends, before any cleanup runs, which ends that Commit.
func TestTheProxyDropsAHeldReplyAsItsTestEnds(t *testing.T) {
	const hold = 30 * time.Second
	start := time.Now()
	t.Run("held", func(t *testing.T) {
		through, fault := CommitFaults(t, Database(t, "legion_testdb_faults"))
		pool, err := pgxpool.New(context.Background(), through)
		if err != nil {
			t.Fatalf("open a pool through the proxy: %v", err)
		}
		t.Cleanup(pool.Close)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, "create table fault_probe (id int primary key)"); err != nil {
			t.Fatalf("create fault_probe: %v", err)
		}
		fired := fault.HoldReply("insert into fault_probe", hold)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin through the proxy: %v", err)
		}
		if _, err := tx.Exec(ctx, "insert into fault_probe values (1)"); err != nil {
			t.Fatalf("insert through the proxy: %v", err)
		}
		commit(t, tx)
		Met(t, fired)
	})
	if took := time.Since(start); took >= hold {
		t.Fatalf("a test that ended while a reply was held for %s took %s to end", hold, took)
	}
}

// rawClient is a TCP connection to the proxy at through, for a test that writes the protocol's
// bytes itself, and the proxy URL's config. Its deadline is bounded, so a read the proxy never
// answers fails the test and does not hang it.
func rawClient(t *testing.T, through string) (net.Conn, *pgconn.Config) {
	t.Helper()
	config, err := pgconn.ParseConfig(through)
	if err != nil {
		t.Fatal("pgx refuses the proxy's URL")
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(testwait.Bounded(t, faultMetBound, time.Second))); err != nil {
		t.Fatalf("set the connection's deadline: %v", err)
	}
	return conn, config
}

// exchange sends message on conn and reads the first byte of the server's answer.
func exchange(t *testing.T, conn net.Conn, message interface{ Encode([]byte) ([]byte, error) }) byte {
	t.Helper()
	encoded, err := message.Encode(nil)
	if err != nil {
		t.Fatalf("encode %T: %v", message, err)
	}
	if _, err := conn.Write(encoded); err != nil {
		t.Fatalf("send %T through the proxy: %v", message, err)
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read the server's answer to %T through the proxy: %v", message, err)
	}
	return answer[0]
}

// startup is the startup message of config's user and database.
func startup(config *pgconn.Config) *pgproto3.StartupMessage {
	return &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": config.User, "database": config.Database},
	}
}

// pgx drops its connection when the server refuses TLS and dials again. libpq keeps it and sends
// its startup message on it, with no type byte, so the proxy reads the message after an
// SSLRequest as untyped too, and the server reads that startup message and answers it with an
// authentication message.
func TestTheProxyPassesAStartupMessageAfterARefusedSSLRequest(t *testing.T) {
	through, _ := CommitFaults(t, Database(t, "legion_testdb_faults"))
	conn, config := rawClient(t, through)
	if answer := exchange(t, conn, &pgproto3.SSLRequest{}); answer != 'N' {
		t.Skipf("the server answers an SSLRequest with %q: this test needs one that refuses TLS", answer)
	}
	if answer := exchange(t, conn, startup(config)); answer != 'R' {
		t.Fatalf("the server answered the startup message with a message of type %q, want an authentication message 'R'", answer)
	}
}

// A message's length counts the length field itself, so no message is shorter than 4 bytes, and
// no untyped one is shorter than 8, its length and the code that says what it is. No client sends
// a shorter length. The proxy ends the connection that sends one, as the server does, instead of
// reading a message shorter than the bytes it then slices, which would panic the test binary; and
// the next client gets through.
func TestTheProxyEndsAConnectionWhoseMessageIsShorterThanItsHeader(t *testing.T) {
	through, _ := CommitFaults(t, PlainDatabase(t, "legion_testdb_faults"))
	for _, tc := range []struct {
		name    string
		typed   bool // the client sends its startup message first, so the proxy reads a type byte
		message []byte
	}{
		{name: "an untyped message of length 4", message: []byte{0, 0, 0, 4}},
		{name: "a typed message of length 3", typed: true, message: []byte{'Q', 0, 0, 0, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, config := rawClient(t, through)
			if tc.typed {
				if answer := exchange(t, conn, startup(config)); answer != 'R' {
					t.Fatalf("the server answered the startup message with a message of type %q, want an authentication message 'R'", answer)
				}
			}
			if _, err := conn.Write(tc.message); err != nil {
				t.Fatalf("send %s through the proxy: %v", tc.name, err)
			}
			// ReadAll ends at the close: nil at an orderly one, or the reset of a close that left
			// bytes unread. A timeout is the proxy waiting for bytes the length names.
			var timeout net.Error
			if _, err := io.ReadAll(conn); errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatalf("the proxy kept the connection that sent %s open: %v", tc.name, err)
			}
			var one int
			if err := connect(t, through).QueryRow(context.Background(), "select 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("a client after %s read %d, %v through the proxy, want 1", tc.name, one, err)
			}
		})
	}
}

// A client of sslnegotiation=direct (PostgreSQL 17 and later) starts with a TLS handshake record,
// whose header reads as an untyped message far longer than any the server reads before startup.
// The proxy cannot read TLS, so it ends that connection at once, as a server without TLS does,
// rather than wait for the length that header names.
func TestTheProxyEndsADirectTLSConnectionAtOnce(t *testing.T) {
	through, _ := CommitFaults(t, PlainDatabase(t, "legion_testdb_faults"))
	direct, err := url.Parse(through)
	if err != nil {
		t.Fatal("net/url refuses the proxy's URL")
	}
	query := direct.Query()
	query.Set("sslmode", "require")
	query.Set("sslnegotiation", "direct")
	direct.RawQuery = query.Encode()
	bound := testwait.Bounded(t, faultMetBound, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	start := time.Now()
	conn, err := pgx.Connect(ctx, direct.String())
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatal("a direct TLS connection through the proxy succeeded, want the proxy to end it")
	}
	if took := time.Since(start); took >= bound {
		t.Fatalf("a direct TLS connection through the proxy failed only at its %s deadline (%v), want the proxy to end it at once", bound, err)
	}
}

func TestAFaultThatCannotBeArmedFailsTheTest(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(fault *CommitFault)
		want string
	}{
		{
			name: "a second fault while the first waits for its COMMIT",
			arm: func(fault *CommitFault) {
				fault.HoldReply("", time.Hour)
				fault.Cut("", CutBeforeCommit)
			},
			want: "a fault is armed while another still waits for its COMMIT",
		},
		{
			name: "a Cut that is none of the cuts",
			arm:  func(fault *CommitFault) { fault.Cut("", 0) },
			want: "Cut 0 is none of CutBeforeCommit, CutAfterCommit and CutAfterCommitRefusing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No server is dialled: the proxy dials only for a connection it accepts, and none is
			// made. The arming runs on a goroutine of its own, which the recorder's Fatalf ends.
			recorder := &fatalRecorder{TB: t}
			_, fault := CommitFaults(recorder, "postgres://legion@127.0.0.1:1/legion")
			armed := make(chan struct{})
			go func() {
				defer close(armed)
				tc.arm(fault)
			}()
			<-armed
			if !strings.Contains(recorder.message, tc.want) {
				t.Fatalf("arming %s failed the test with %q, want it to say %q", tc.name, recorder.message, tc.want)
			}
		})
	}
}

// fatalRecorder is t with Fatalf recorded and the goroutine ended, as t.Fatalf does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
