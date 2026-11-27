package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testDSN is the devbox and CI Postgres the store tests run against. Without it
// there is nothing to test: the store has no in-memory mode by design. A pool a test opens on it
// holds testdb.PoolConns connections unless LEGION_TEST_PG_DSN sets a size of its own, which it
// keeps (testdb.PoolBounded): each URL this package's tests build from it goes only to a pool,
// Open's or pgxpool's, and a pool sized by the CPU count would ask a large machine's Postgres for
// more connections than it allows.
//
// These tests name no internal/testpg helper, and the file imports that package nowhere: testpg
// imports this one, to migrate the template its Clone copies, so an import of it here would be a
// cycle in this package's test binary. internal/testpg's own skip message holds the pinned
// Postgres image to start, and testdb.Drop, which waits out an autovacuum worker and a backend
// that has not exited yet, is the one copy of that wait: internal/testpg/testdb imports no package
// of this module except internal/testwait, which imports none, so this package's own tests can
// take it where they cannot take internal/testpg.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against: start one and set it as internal/testpg's own skip message says, which holds the whole recipe and the image to run.")
	}
	bounded, err := testdb.PoolBounded(dsn, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

// emptyStore opens a store on a database of its own, so "an empty database" in a
// test means one, and tests never see each other's rows.
func emptyStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "legion_store_test_" + randomSuffix(t)
	// A file copy of template1 (docs/testing.md, "A database a Go test creates").
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropContext, cancel := testwait.TeardownContext(t)
		defer cancel()
		if err := testdb.Drop(dropContext, admin, name); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	testURL := *base
	testURL.Path = "/" + name
	store, err := Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(store.Close)
	return store
}

// migratedStore is an empty database with the schema applied.
func migratedStore(t *testing.T) *Store {
	t.Helper()
	store := emptyStore(t)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func TestOpenNamesTheUnreachableAddressAndNotThePassword(t *testing.T) {
	t.Parallel()
	_, err := Open(context.Background(), "postgres://legion:hunter2@127.0.0.1:1/legion")
	if err == nil {
		t.Fatal("expected an error connecting to 127.0.0.1:1, got none")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error does not name the host:port it failed to reach: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the DSN password: %v", err)
	}
}

// The driver's own text is not ours to trust: whatever it says on a future
// version, the password the daemon was configured with does not reach a log.
func TestConnectErrorRedactsThePasswordOutOfWhatItWraps(t *testing.T) {
	t.Parallel()
	for _, detail := range []string{
		"failed to connect to `host=db.internal password=hunter2`",
		"cannot parse postgres://legion:hunter2@db.internal:5432/legion",
	} {
		err := &connectError{address: "db.internal:5432", password: "hunter2", err: errors.New(detail)}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password its driver printed: %v", err)
		}
		if !strings.Contains(err.Error(), "db.internal:5432") {
			t.Errorf("error does not name the host:port it failed to reach: %v", err)
		}
	}
}

// postgres://legion:legion@… is the ordinary devbox DSN: the password equals
// the user and the database name. Redaction that blanked every occurrence
// would hide the two fields an operator reads to diagnose the refusal.
func TestConnectErrorKeepsTheFieldsThatMerelyMatchThePassword(t *testing.T) {
	t.Parallel()
	err := &connectError{
		address:  "127.0.0.1:5432",
		password: "legion",
		err:      errors.New("failed to connect to `user=legion database=legion`: connection refused"),
	}
	if !strings.Contains(err.Error(), "user=legion database=legion") {
		t.Errorf("error hides the user and database the daemon was connecting as: %v", err)
	}
}

// Each connection error of dispatch://INFRA-2597/spec R3, as Open itself returns it, is read as R3
// reads it (c-boot-db-retry case 4): no answer — a refused connection, a reset, a connection
// closed before an answer, an attempt's own deadline, a host name that does not resolve — and the
// codes that tell a client to try again later can clear at the next attempt; every other answer,
// a DSN pgx cannot parse, a TLS failure (a certificate the client cannot verify, an alert of the
// handshake, the server's or the client's own) and a stop of the caller cannot.
//
// It needs no database: each row is a listener of its own, or an address nothing listens on. Each
// row also holds the type its error carries, measured against PostgreSQL 18.6 through pgx on
// Open's own path, so a row that passes passes on the error pgx really builds, not on whatever
// its listener happened to make it return. The server's codes come from a listener that answers
// the startup message with one ErrorResponse (refusingBackend): 57P01, 57P02 and class 08 cannot
// be made on a scratch server in a test, and the rest answer the same way from a real one.
func TestOpenRetryableReadsEachConnectionErrorOfR3(t *testing.T) {
	t.Parallel()
	refused := "postgres://legion@127.0.0.1:1/legion?sslmode=disable"
	at := func(listen func(*testing.T) string) func(*testing.T) string {
		return func(t *testing.T) string {
			return "postgres://legion@" + listen(t) + "/legion?sslmode=disable"
		}
	}
	answering := func(code, message string) func(*testing.T) string {
		return at(func(t *testing.T) string { return refusingBackend(t, code, message) })
	}
	// clientCertificateDemanded is a server that demands a certificate of the client, which pgx has
	// none of: under TLS 1.3 the server sends its alert once it reads the client's answer.
	clientCertificateDemanded := func(t *testing.T) string {
		return tlsBackend(t, func(config *tls.Config) { config.ClientAuth = tls.RequireAnyClientCert })
	}
	for _, row := range []struct {
		name string
		dsn  func(*testing.T) string
		// attempt is Open's deadline; zero is ten seconds, an attempt's limit at the daemon's start.
		attempt time.Duration
		// cancelled hands Open a context its caller has already cancelled.
		cancelled bool
		// carries is the identity the row's error must carry, for the row to be the row it names.
		carries func(error) bool
		want    bool
	}{
		{name: "a refused connection", dsn: func(*testing.T) string { return refused }, carries: carriesType[*net.OpError], want: true},
		{name: "a connection reset as it is accepted", dsn: at(resettingListener), carries: carriesType[*net.OpError], want: true},
		{name: "a connection closed after its startup message", dsn: at(closingListener), carries: carriesError(io.ErrUnexpectedEOF), want: true},
		{name: "a listener that never answers, past the attempt's deadline", dsn: at(silentListener), attempt: 200 * time.Millisecond,
			carries: carriesError(context.DeadlineExceeded), want: true},
		{name: "a host name that does not resolve", dsn: func(*testing.T) string {
			return "postgres://legion@no-such-host.invalid:5432/legion?sslmode=disable"
		}, carries: carriesType[*net.DNSError], want: true},
		{name: "08006 connection failure", dsn: answering("08006", "could not receive data from client: Connection reset by peer"), carries: carriesCode("08006"), want: true},
		{name: "53300 slots full", dsn: answering("53300", "remaining connection slots are reserved for roles with the SUPERUSER attribute"), carries: carriesCode("53300"), want: true},
		{name: "57P01 admin shutdown", dsn: answering("57P01", "terminating connection due to administrator command"), carries: carriesCode("57P01"), want: true},
		{name: "57P02 crash shutdown", dsn: answering("57P02", "terminating connection because of crash of another server process"), carries: carriesCode("57P02"), want: true},
		{name: "57P03 shutting down", dsn: answering("57P03", "the database system is shutting down"), carries: carriesCode("57P03"), want: true},
		{name: "28P01 a refused password", dsn: answering("28P01", `password authentication failed for user "legion"`), carries: carriesCode("28P01"), want: false},
		{name: "28000 no pg_hba entry", dsn: answering("28000", `no pg_hba.conf entry for host "127.0.0.1", user "legion", database "legion", no encryption`), carries: carriesCode("28000"), want: false},
		{name: "3D000 a database that does not exist", dsn: answering("3D000", `database "legion" does not exist`), carries: carriesCode("3D000"), want: false},
		{name: "a DSN pgx cannot parse", dsn: func(*testing.T) string { return "postgres://x@127.0.0.1/x?sslmode=bogus" },
			carries: carriesType[*pgconn.ParseConfigError], want: false},
		{name: "a certificate verify-full cannot verify, beside a refused dial", dsn: func(t *testing.T) string {
			// Two hosts, as localhost is two addresses, 127.0.0.1 and ::1: pgx tries each, and its
			// error carries the refused certificate at the first beside the refused dial at the
			// second, so the row holds that a TLS failure is read before the network's.
			return "postgres://legion@" + tlsBackend(t, nil) + ",127.0.0.1:1/legion?sslmode=verify-full"
		}, carries: func(err error) bool {
			return carriesType[*tls.CertificateVerificationError](err) && carriesType[*net.OpError](err)
		}, want: false},
		// Over TCP, Go's TLS client returns an alert the server sent as a *net.OpError whose Op is
		// "remote error", and one it sent itself as a *net.OpError whose Op is "local error"
		// (crypto/tls/conn.go): each row holds the Op pgx v5.13.0 returned on Open's path.
		{name: "a server that demands a client certificate, which alerts the client", dsn: func(t *testing.T) string {
			return "postgres://legion@" + clientCertificateDemanded(t) + "/legion?sslmode=require"
		}, carries: carriesOp("remote error"), want: false},
		{name: "a server that speaks only TLS 1.1, which alerts the client", dsn: func(t *testing.T) string {
			return "postgres://legion@" + tlsBackend(t, func(config *tls.Config) {
				config.MinVersion, config.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
			}) + "/legion?sslmode=require"
		}, carries: carriesOp("remote error"), want: false},
		{name: "a handshake message of a type TLS does not define, which the client alerts", dsn: func(t *testing.T) string {
			return "postgres://legion@" + sslBackend(t, func(conn net.Conn) {
				// One handshake record (22, TLS 1.2, 4 bytes) holding a handshake message of type
				// 255, which no version of TLS defines, and of length 0.
				if _, err := conn.Write([]byte{22, 3, 3, 0, 4, 255, 0, 0, 0}); err == nil {
					_, _ = io.Copy(io.Discard, conn)
				}
			}) + "/legion?sslmode=require"
		}, carries: carriesOp("local error"), want: false},
		{name: "a server that demands a client certificate, after a refused dial", dsn: func(t *testing.T) string {
			// The refused dial comes first in the error, so the row holds that the alert is read
			// wherever it stands, and not only at the chain's first *net.OpError.
			return "postgres://legion@127.0.0.1:1," + clientCertificateDemanded(t) + "/legion?sslmode=require"
		}, carries: func(err error) bool {
			return carriesOp("dial")(err) && carriesOp("remote error")(err)
		}, want: false},
		{name: "a caller that stopped", dsn: func(*testing.T) string { return refused }, cancelled: true,
			carries: carriesError(context.Canceled), want: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			attempt := row.attempt
			if attempt == 0 {
				attempt = 10 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), attempt)
			defer cancel()
			if row.cancelled {
				cancel()
			}
			st, err := Open(ctx, row.dsn(t))
			if err == nil {
				st.Close()
				t.Fatal("Open succeeded; the row needs the error it names")
			}
			if !row.carries(err) {
				t.Fatalf("Open failed with %q, which does not carry the error this row names", err)
			}
			if got := OpenRetryable(err); got != row.want {
				t.Errorf("OpenRetryable(%q) = %t, want %t", err, got, row.want)
			}
		})
	}
}

// carriesType is whether err carries an error of type T anywhere in its chain.
func carriesType[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// carriesError is whether an error carries target anywhere in its chain.
func carriesError(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// carriesCode is whether an error carries a server's answer of code.
func carriesCode(code string) func(error) bool {
	return func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code
	}
}

// carriesOp is whether an error carries, anywhere in its chain, a *net.OpError whose Op is op.
func carriesOp(op string) func(error) bool {
	var carries func(error) bool
	carries = func(err error) bool {
		if opErr, ok := err.(*net.OpError); ok && opErr.Op == op {
			return true
		}
		switch err := err.(type) {
		case interface{ Unwrap() error }:
			return carries(err.Unwrap())
		case interface{ Unwrap() []error }:
			return slices.ContainsFunc(err.Unwrap(), carries)
		}
		return false
	}
	return carries
}

// fakePostgres listens on 127.0.0.1 for the test, hands each connection it accepts to serve, and
// returns its address. The listener closes when the test ends.
func fakePostgres(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return listener.Addr().String()
}

// resettingListener ends each connection with a TCP reset (SO_LINGER 0) as soon as it is accepted.
func resettingListener(t *testing.T) string {
	return fakePostgres(t, func(conn net.Conn) {
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	})
}

// closingListener reads each connection's startup message and closes the connection before it
// answers.
func closingListener(t *testing.T) string {
	return fakePostgres(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = pgproto3.NewBackend(conn, conn).ReceiveStartupMessage()
	})
}

// silentListener accepts each connection and never answers it; the connection ends when its
// client closes it.
func silentListener(t *testing.T) string {
	return fakePostgres(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	})
}

// refusingBackend answers each connection's startup message with one FATAL ErrorResponse of code
// and message, as a server that refuses the connection answers it.
func refusingBackend(t *testing.T, code, message string) string {
	return fakePostgres(t, func(conn net.Conn) {
		defer conn.Close()
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: message})
		_ = backend.Flush()
	})
}

// sslBackend answers each connection's SSLRequest with S, as a server that speaks TLS does, and
// then hands the connection to serve.
func sslBackend(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	return fakePostgres(t, func(conn net.Conn) {
		defer conn.Close()
		startup, err := pgproto3.NewBackend(conn, conn).ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := startup.(*pgproto3.SSLRequest); !ok {
			return
		}
		if _, err := conn.Write([]byte("S")); err != nil {
			return
		}
		serve(conn)
	})
}

// tlsBackend answers each connection's SSLRequest with S and then serves a TLS handshake with a
// self-signed certificate no client can verify, under the settings tune makes (nil makes none).
// Whatever the handshake ends in, the server reads the connection to its end, so its close sends
// no reset that could overtake the alert it sent.
func tlsBackend(t *testing.T, tune func(*tls.Config)) string {
	t.Helper()
	config := &tls.Config{Certificates: []tls.Certificate{selfSignedCertificate(t)}}
	if tune != nil {
		tune(config)
	}
	return sslBackend(t, func(conn net.Conn) {
		_ = tls.Server(conn, config).Handshake()
		_, _ = io.Copy(io.Discard, conn)
	})
}

// selfSignedCertificate is a certificate for localhost that signs itself, which no root a client
// trusts has signed.
func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the certificate's key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign the certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestSchemaVersionIsZeroBeforeAnyMigration(t *testing.T) {
	t.Parallel()
	version, err := emptyStore(t).SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version on an empty database: %v", err)
	}
	if version != 0 {
		t.Errorf("schema version on an empty database = %d, want 0", version)
	}
}

// latestMigration is the highest embedded migration and how many there are: what a fresh
// database is brought to, whatever stage added the newest file.
func latestMigration(t *testing.T) (count, version int) {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	return len(all), all[len(all)-1].Version
}

func TestMigrateAppliesEveryEmbeddedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	count, latest := latestMigration(t)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied != count {
		t.Errorf("migrate applied %d migrations, want all %d", applied, count)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != latest {
		t.Errorf("schema version = %d, want %d", version, latest)
	}
}

func TestMigrateAppliesNothingASecondTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	_, latest := latestMigration(t)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("second migrate applied %d migrations, want 0", applied)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != latest {
		t.Errorf("schema version after a second migrate = %d, want %d", version, latest)
	}
}

// Two branches can each take the next migration number, and they land in either order: a database
// migrated by a daemon with the higher one first has to take the lower one when it arrives. Migrate
// applies every embedded migration the database has not recorded, not only those above its highest,
// so a database at 0001-0040 and 0042 and later takes 0041 — here child_release_retries, a table no
// later migration alters — and ends with every version recorded.
func TestMigrateAppliesALowerMigrationADatabaseAlreadyPastItLacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	const skipped = 41
	for _, migration := range all {
		if migration.Version == skipped {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	childReleaseRetries := func() bool {
		t.Helper()
		var exists bool
		if err := store.pool.QueryRow(ctx, "select to_regclass('public.child_release_retries') is not null").Scan(&exists); err != nil {
			t.Fatalf("look for the child_release_retries table: %v", err)
		}
		return exists
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != all[len(all)-1].Version || childReleaseRetries() {
		t.Fatalf("before Migrate: schema version %d (%v), child_release_retries table %t; want the latest, and no table", version, err, childReleaseRetries())
	}

	applied, err := store.Migrate(ctx)

	if err != nil || applied != 1 {
		t.Fatalf("migrate = %d, %v; want the one missing migration applied", applied, err)
	}
	if !childReleaseRetries() {
		t.Error("migration 0041 was recorded as applied, but the child_release_retries table does not exist")
	}
	var recorded []int
	rows, err := store.pool.Query(ctx, "select version from schema_version order by version")
	if err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan schema_version: %v", err)
		}
		recorded = append(recorded, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	want := make([]int, len(all))
	for i, migration := range all {
		want[i] = migration.Version
	}
	if !slices.Equal(recorded, want) {
		t.Errorf("schema_version holds %v, want every embedded version %v", recorded, want)
	}
}

// A migration whose own text runs a session SET, one without LOCAL, leaves that setting on its
// connection after the commit. apply resets that connection's session before the connection
// returns to the pool (releaseMigrationConn). So after migrate returns, no connection that the pool
// can give carries the migration's statement_timeout or search_path. The test holds as many
// connections as the pool can give at once (its MaxConns) and reads each one, because the
// migration ran on one of them. Each one must read what a new connection reads, and the idle bound
// too, which the reset must keep (idleBound). The control runs the same SETs in a committed
// transaction on a connection of its own and reads them back. The SETs survive a commit, so the
// row shows that the reset clears them.
//
// Control: remove the discard all and the DeallocateAll from releaseMigrationConn, so the
// connection returns to the pool as the migration left it, and this fails: a pooled connection
// reads 77s and the row's schema.
func TestMigrateResetsTheSessionAMigrationLeftOnItsConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	if all[0].Version != 1 {
		t.Fatalf("the first embedded migration is %s, want 0001_init, which creates schema_version", all[0].Name)
	}
	// The schema is the row's own, in the database of the test's own that emptyStore drops.
	schema := "migrate_leak_" + randomSuffix(t)
	leaked := schema + ", public"
	set := []migrations.Migration{
		all[0],
		{Version: 2, Name: "0002_leak_schema.up.sql", SQL: "create schema " + schema},
		{Version: 3, Name: "0003_leak.up.sql", SQL: "set statement_timeout = '77s'; set search_path = " + leaked},
	}
	if _, err := store.migrate(ctx, set); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The server compares the idle bound as an interval, as requireSetting does.
	const read = "select current_setting('statement_timeout'), current_setting('search_path'), " +
		"current_setting('idle_in_transaction_session_timeout')::interval = $1::text::interval"

	// A new connection, opened as the pool opens its own, reads the values that the reset must
	// give back on every pooled connection.
	fresh, err := pgx.ConnectConfig(ctx, store.pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect a new connection: %v", err)
	}
	t.Cleanup(func() { fresh.Close(context.Background()) })
	var wantTimeout, wantSearchPath string
	var freshIdleBound bool
	if err := fresh.QueryRow(ctx, read, idleBound.String()).Scan(&wantTimeout, &wantSearchPath, &freshIdleBound); err != nil {
		t.Fatalf("read a new connection's settings: %v", err)
	}
	if wantTimeout == "77s" || wantSearchPath == leaked || !freshIdleBound {
		t.Fatalf("a new connection reads statement_timeout %q, search_path %q and the idle bound %t; want neither of the migration's settings and the idle bound %s, or the row proves nothing", wantTimeout, wantSearchPath, freshIdleBound, idleBound)
	}

	acquiring, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	maxConns := store.pool.Config().MaxConns
	held := make([]*pgxpool.Conn, 0, maxConns)
	t.Cleanup(func() {
		for _, conn := range held {
			conn.Release()
		}
	})
	for i := range maxConns {
		// acquiring carries no mark, so the pool refuses none of these and the release that
		// acquire returns has nothing to clear.
		conn, _, err := store.pool.acquire(acquiring)
		if err != nil {
			t.Fatalf("acquire pooled connection %d of %d: %v", i+1, maxConns, err)
		}
		held = append(held, conn)
	}
	for i, conn := range held {
		var timeout, searchPath string
		var idle bool
		if err := conn.QueryRow(ctx, read, idleBound.String()).Scan(&timeout, &searchPath, &idle); err != nil {
			t.Fatalf("read pooled connection %d's settings: %v", i+1, err)
		}
		if timeout != wantTimeout || searchPath != wantSearchPath {
			t.Errorf("pooled connection %d reads statement_timeout %q and search_path %q; want %q and %q, the values a new connection reads: the migration's session SET reached the pool", i+1, timeout, searchPath, wantTimeout, wantSearchPath)
		}
		if !idle {
			t.Errorf("pooled connection %d does not read the idle bound %s: the reset lost the value that Open gives each connection", i+1, idleBound)
		}
	}

	// The control: the same SETs on a connection of the test's own, in a transaction that commits.
	control, err := pgx.ConnectConfig(ctx, store.pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("control: connect: %v", err)
	}
	t.Cleanup(func() { control.Close(context.Background()) })
	if _, err := control.Exec(ctx, "begin; set statement_timeout = '77s'; set search_path = "+leaked+"; commit"); err != nil {
		t.Fatalf("control: run the SETs in a committed transaction: %v", err)
	}
	var controlTimeout, controlSearchPath string
	// The control's SETs do not change the idle bound, so its column is not read (nil).
	if err := control.QueryRow(ctx, read, idleBound.String()).Scan(&controlTimeout, &controlSearchPath, nil); err != nil {
		t.Fatalf("control: read the settings: %v", err)
	}
	if controlTimeout != "77s" || controlSearchPath != leaked {
		t.Fatalf("control: after a committed SET the connection reads %q and %q; want 77s and %q: the SETs do not survive a commit, so the row proves nothing", controlTimeout, controlSearchPath, leaked)
	}
}

func TestBootsCountsTheProjectsBootsFromItsFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	first := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := store.RecordBoot(ctx, "WIDGETS", first.Add(-time.Hour)); err != nil {
		t.Fatalf("record another project's boot: %v", err)
	}
	firstID, err := store.RecordBoot(ctx, "LEGION", first)
	if err != nil {
		t.Fatalf("record first boot: %v", err)
	}
	secondID, err := store.RecordBoot(ctx, "LEGION", first.Add(time.Minute))
	if err != nil {
		t.Fatalf("record second boot: %v", err)
	}
	if firstID == secondID {
		t.Errorf("both boots got id %d, want one row each", firstID)
	}

	count, firstBootAt, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 2 {
		t.Errorf("boots count = %d, want 2", count)
	}
	if !firstBootAt.Equal(first) {
		t.Errorf("first boot at = %s, want %s", firstBootAt, first)
	}
}

func TestBootsIsZeroForAProjectThatNeverBooted(t *testing.T) {
	t.Parallel()
	count, firstBootAt, err := migratedStore(t).Boots(context.Background(), "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 0 {
		t.Errorf("boots count = %d, want 0", count)
	}
	if !firstBootAt.IsZero() {
		t.Errorf("first boot at = %s, want the zero time", firstBootAt)
	}
}

func TestStopBootStampsTheBootItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	started := time.Now().UTC().Truncate(time.Microsecond)

	id, err := store.RecordBoot(ctx, "LEGION", started)
	if err != nil {
		t.Fatalf("record boot: %v", err)
	}
	running, err := store.RecordBoot(ctx, "LEGION", started)
	if err != nil {
		t.Fatalf("record second boot: %v", err)
	}
	stopped := started.Add(90 * time.Second)
	if err := store.StopBoot(ctx, id, stopped); err != nil {
		t.Fatalf("stop boot: %v", err)
	}

	var stoppedAt *time.Time
	if err := store.pool.QueryRow(ctx, "select stopped_at from daemon_boot where id = $1", id).Scan(&stoppedAt); err != nil {
		t.Fatalf("read stopped boot: %v", err)
	}
	if stoppedAt == nil || !stoppedAt.Equal(stopped) {
		t.Errorf("stopped_at = %v, want %s", stoppedAt, stopped)
	}

	var otherStoppedAt *time.Time
	if err := store.pool.QueryRow(ctx, "select stopped_at from daemon_boot where id = $1", running).Scan(&otherStoppedAt); err != nil {
		t.Fatalf("read running boot: %v", err)
	}
	if otherStoppedAt != nil {
		t.Errorf("stopped_at of the still-running boot = %s, want null", otherStoppedAt)
	}
}

func TestStopBootRefusesAnUnknownBoot(t *testing.T) {
	t.Parallel()
	err := migratedStore(t).StopBoot(context.Background(), 4242, time.Now().UTC())
	if err == nil {
		t.Fatal("expected an error stopping boot 4242, got none")
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("error does not name the boot it could not stop: %v", err)
	}
}

func TestTxCommitsWhatTheFunctionWrote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)

	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into daemon_boot (project, started_at) values ($1, now())", "LEGION")
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	count, _, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 1 {
		t.Errorf("boots count = %d, want the committed row", count)
	}
}

func TestTxRollsBackWhatTheFunctionWroteBeforeFailing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	failure := errors.New("the phase gave up")

	err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into daemon_boot (project, started_at) values ($1, now())", "LEGION"); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("tx error = %v, want the function's own error", err)
	}

	count, _, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 0 {
		t.Errorf("boots count = %d, want 0 after a rolled-back transaction", count)
	}
}

// migrateThrough applies every embedded migration up to and including version, as an older
// daemon's database stands.
func migrateThrough(t *testing.T, store *Store, version int) {
	t.Helper()
	ctx := context.Background()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version > version {
			return
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
}

// phaseRowOf is the phases row of role on issue, read through st as the record reads it, and the
// zero row where it holds none (record.PhaseRowOf). The migration tests read a row through the
// store they migrated and through one reopened as a daemon restart reopens it.
func phaseRowOf(t *testing.T, st *Store, issue string, role claim.Role) record.PhaseRow {
	t.Helper()
	ctx := context.Background()
	var rows []record.PhaseRow
	if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().Phases(ctx, tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the phases rows of %s: %v", issue, err)
	}
	return record.PhaseRowOf(rows, role)
}

// A daemon upgraded in place has workers holding tasks delivered before the delivery carried its
// run. A completion attributed to no run is refused, so those phases would stall until every
// worker happened to be handed a new task: 0011 gives each such delivery the run its issue is on,
// and leaves a claim on an issue no workflow records — an operator's own (LEGION-272) — at zero.
func TestTheDeliveryGenerationBackfillsFromTheIssuesRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 10)

	for _, row := range []struct{ token, role, issue string }{
		{token: "tok-tester", role: "tester", issue: "LEGION-208"},
		{token: "tok-operator", role: "implementer", issue: "LEGION-UNRECORDED"},
	} {
		if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', 'LEGION-208', $2, $3, 1, '', '', 'working', 0, 0, 0, 0)`,
			row.token, row.issue, row.role); err != nil {
			t.Fatalf("seed the %s claim: %v", row.role, err)
		}
		if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, confirmed_at)
			values ($1, 'delivery-'||$1, 'the task', now(), now())`, row.token); err != nil {
			t.Fatalf("seed the %s delivery: %v", row.role, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 3, 'in_progress', 'V', 0)`); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	for _, want := range []struct {
		token      string
		generation int64
	}{
		{token: "tok-tester", generation: 3},
		{token: "tok-operator", generation: 0},
	} {
		var generation int64
		if err := store.pool.QueryRow(ctx, "select generation from pending_task_deliveries where claim_token = $1",
			want.token).Scan(&generation); err != nil {
			t.Fatalf("read %s's delivery: %v", want.token, err)
		}
		if generation != want.generation {
			t.Fatalf("%s's delivery carries run %d, want %d", want.token, generation, want.generation)
		}
	}
}

// A worker between turns at an in-place upgrade holds no delivery at all, so nothing tells the
// daemon which run it is working and its next completion is refused: 0012 gives each live claim
// the run its issue is on. A claim whose process is gone is left at zero — it is relaunched, and
// the task its new process is given carries its own run — as is one on an issue no workflow
// records.
func TestTheServingRunBackfillsForClaimsBetweenTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 10)

	for _, row := range []struct{ token, issue, state string }{
		{token: "tok-idle", issue: "LEGION-208", state: "idle"},
		{token: "tok-retired", issue: "LEGION-208", state: "retired"},
		{token: "tok-unrecorded", issue: "LEGION-UNRECORDED", state: "idle"},
	} {
		if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', 'LEGION-208', $2, 'tester', 1, '', '', $3, 0, 0, 0, 0)`,
			row.token, row.issue, row.state); err != nil {
			t.Fatalf("seed the %s claim: %v", row.token, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 5, 'in_progress', 'V', 0)`); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	for _, want := range []struct {
		token   string
		serving int64
	}{
		{token: "tok-idle", serving: 5},
		{token: "tok-retired", serving: 0},
		{token: "tok-unrecorded", serving: 0},
	} {
		var serving int64
		if err := store.pool.QueryRow(ctx, "select serving_generation from claims where token = $1",
			want.token).Scan(&serving); err != nil {
			t.Fatalf("read %s: %v", want.token, err)
		}
		if serving != want.serving {
			t.Fatalf("%s serves run %d, want %d", want.token, serving, want.serving)
		}
	}
}

// A tree close and a workspace removal name the root generation of the linger they expire, and
// the outbox decodes rows strictly. 0016 gives each one queued in the older shape (a close naming
// no linger, a removal naming its issue's generation) the generation of its tree's root when that
// root still lingers, so the row goes on to expire that linger, and deletes the rest: their linger
// has ended, and a row that never decodes would fail every tick and every promotion of its tree.
func TestQueuedLingerRowsOfTheOlderShapeNameTheirLingerOrGo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 13)

	for _, issue := range []struct {
		key, tree  string
		generation int
		lingers    bool
	}{
		{key: "LEGION-208", tree: "LEGION-208", generation: 4, lingers: true},
		{key: "LEGION-209", tree: "LEGION-208", generation: 1},
		{key: "LEGION-300", tree: "LEGION-300", generation: 2},
		{key: "LEGION-301", tree: "LEGION-300", generation: 1},
	} {
		var lingerUntil *time.Time
		if issue.lingers {
			until := time.Now().Add(time.Hour)
			lingerUntil = &until
		}
		if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
			values ($1, $2, 'LEGION', $1, 'testing', $3, 'in_progress', 'V', $4, 0)`, issue.key, issue.tree, issue.generation, lingerUntil); err != nil {
			t.Fatalf("seed %s: %v", issue.key, err)
		}
	}
	type row struct{ kind, issue, payload string }
	for _, seeded := range []row{
		{kind: "supervise", issue: "LEGION-209", payload: `{"op": "tree_close", "tree": "LEGION-208", "role": "tester", "generation": 1}`},
		{kind: "workspace_remove", issue: "LEGION-209", payload: `{"generation": 1}`},
		{kind: "supervise", issue: "LEGION-301", payload: `{"op": "tree_close", "tree": "LEGION-300", "role": "tester", "generation": 1}`},
		{kind: "workspace_remove", issue: "LEGION-301", payload: `{"generation": 1}`},
		{kind: "supervise", issue: "LEGION-301", payload: `{"op": "start", "tree": "LEGION-300", "role": "tester", "generation": 1, "phase": "testing", "task": "Test it."}`},
	} {
		if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, $2, $3, 0, now(), '')`,
			seeded.kind, seeded.issue, seeded.payload); err != nil {
			t.Fatalf("seed the %s row of %s: %v", seeded.kind, seeded.issue, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	rows, err := store.pool.Query(ctx, "select id, kind, issue, payload from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var queued record.OutboxRow
		var kind string
		if err := rows.Scan(&queued.ID, &kind, &queued.Issue, &queued.Payload); err != nil {
			t.Fatalf("scan an outbox row: %v", err)
		}
		queued.Kind = record.OutboxKind(kind)
		payload, err := record.DecodeOutboxPayload(queued)
		if err != nil {
			t.Fatalf("the migrated %s row of %s does not decode: %v", kind, queued.Issue, err)
		}
		switch value := payload.(type) {
		case record.SuperviseRequest:
			got = append(got, fmt.Sprintf("%s %s linger=%d", queued.Issue, value.Op, value.Linger))
		case record.WorkspaceRemove:
			got = append(got, fmt.Sprintf("%s workspace_remove linger=%d", queued.Issue, value.Linger))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	want := []string{"LEGION-209 tree_close linger=4", "LEGION-209 workspace_remove linger=4", "LEGION-301 start linger=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("outbox after 0016 = %q, want %q", got, want)
	}
}

// A worker whose process died mid-task leaves a claim launching, shim_connected or
// launch_uncertain: the relaunch resumes that session and is given no task, because the claim
// already holds one. 0014 gives such a claim the issue's run. A first launch has no session and
// keeps zero — the task it is given attributes it. Started from 0010 the two backfills run in one
// boot and together cover every live claim; started from 0013, a claim already past relaunching
// with a zero run is out of 0014's reach, which is the limit its header states.
func TestTheServingRunBackfillsForARelaunchingClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		from      int
		wantReady int64
	}{
		{from: 10, wantReady: 5},
		{from: 13, wantReady: 0},
	} {
		t.Run(fmt.Sprintf("from %04d", tc.from), func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			migrateThrough(t, store, tc.from)
			for _, row := range []struct{ token, state, session string }{
				{token: "tok-launching", state: "launching", session: "ses-resumed"},
				{token: "tok-shim", state: "shim_connected", session: "ses-resumed"},
				{token: "tok-uncertain", state: "launch_uncertain", session: "ses-resumed"},
				{token: "tok-first", state: "launching", session: ""},
				{token: "tok-retired", state: "retired", session: "ses-resumed"},
				{token: "tok-ready", state: "ready", session: "ses-resumed"},
			} {
				if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
					session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
					values ($1, 'LEGION', 'LEGION-208', 'LEGION-208', 'tester', 1, $3, '', $2, 0, 0, 0, 0)`,
					row.token, row.state, row.session); err != nil {
					t.Fatalf("seed the %s claim: %v", row.token, err)
				}
			}
			if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 5, 'in_progress', 'V', 0)`); err != nil {
				t.Fatalf("seed the issue: %v", err)
			}

			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate the rest: %v", err)
			}

			for _, want := range []struct {
				token   string
				serving int64
			}{
				{token: "tok-launching", serving: 5},
				{token: "tok-shim", serving: 5},
				{token: "tok-uncertain", serving: 5},
				{token: "tok-first", serving: 0},
				{token: "tok-retired", serving: 0},
				{token: "tok-ready", serving: tc.wantReady},
			} {
				var serving int64
				if err := store.pool.QueryRow(ctx, "select serving_generation from claims where token = $1",
					want.token).Scan(&serving); err != nil {
					t.Fatalf("read %s: %v", want.token, err)
				}
				if serving != want.serving {
					t.Fatalf("%s serves run %d, want %d", want.token, serving, want.serving)
				}
			}
		})
	}
}

// thisRelease is the version of every migration this branch adds, and idleRelease the newest
// version of the release it merged (main's `20261101222956_issue_idle_release`). Together they
// build the database shapes a deployment really has: one that recorded the other release's
// versions and none of this one's. The list is what
// `jj diff --from main@origin --to @ --name-only packages/daemon/internal/store/migrations`
// named on dispatch://INFRA-714's branch. A migration a later branch adds that rewrites
// outbox_kind_check with a kind of its own belongs in it too, as dispatch://INFRA-3274's
// 20261114031356, the role-holders publication's 20261116165926, the step record's
// 20261117000528, the plan record's 20261117023932, dispatch://INFRA-4374's 20261119003157
// (conflict_confirm and merge_forward), dispatch://INFRA-175's 20261119203047 (design_changes),
// dispatch://INFRA-3320's 20261122154643 (the two basis seeds), the merge holds' 20261126110259
// (merge_holds_changed), the copies of a plan's tasks' 20261121165047 (task_branch), the pull
// requests by task's 20261122122826 (task_pull_requests_close), the removal of a removed task's
// working copy's 20261123072431 (task_copy_remove) and the merge of main's 20261126144204 (the
// lists of both) do: the case of a
// database main migrated applies a version left out of the list before the list's own rewrites of
// that check, which do not name the later kind, so the row seeded of that kind fails the upgrade.
// So does one that reads a column a version of the list adds, as t-e-54's 20261116144907 reads
// claims.abandoned_session (20261029121200) and dispatch://INFRA-5463's 20261123212127 reads
// claims.last_start_failure (20261029121000): applied before that version, it fails on the missing
// column. So does a migration that changes a table a version of the list creates, as
// 20261117213545 adds a column to 20261117034837's plan_steps; 20261123200630 and the givers of a
// plan version's 20261124033010 each add one to its issue_plans; and 20261123221142 and the
// sub-owners' 20261124023617 each change its plan_tasks: left out, it is applied as main's, before
// that table exists; and one that replaces an index a version of the list creates, as
// 20261122074350 replaces 20261105183200's pod_resumes_one_open_per_role. dispatch://INFRA-3320's
// gate basis (20261114064234) and awaiting_children (20261114232555) are in it as versions that
// branch added. The project pause's 20261115194357 is in it as well, though it rewrites no kind
// list: a database migrated before the pause takes it in the upgrade, over a row of every kind the
// schema admits, and its held_by_pause column on the outbox must take them all.
// 20261126144204 is the last migration that writes outbox_kind_check: it stands
// above main's 20261119003157, 20261119203047, 20261122154643 and 20261126110259 and this branch's
// 20261121165047, 20261122122826 and 20261123072431, which write it too, and main's 20261111090000,
// 20261111091500 and 20261116144907 write none. The cache trimmer's two, 20261111090000 and
// 20261111091500, read no column a version of the list adds either, so neither is in the list. Any
// other version left out makes the cases below weaker,
// never red, which is why the comment says where the list comes from.
var thisRelease = map[int]bool{
	20261029120000: true, 20261029120100: true, 20261029120200: true, 20261029120300: true,
	20261029120400: true, 20261029120600: true, 20261029120700: true, 20261029120800: true,
	20261029120900: true, 20261029121000: true, 20261029121100: true, 20261029121200: true,
	20261102003600: true, 20261105081500: true, 20261105183200: true, 20261107095842: true,
	20261108152000: true, 20261108190000: true, 20261109130413: true, 20261111181500: true,
	20261113082214: true, 20261113110610: true, 20261114031356: true, 20261114064234: true,
	20261114232555: true, 20261115060739: true, 20261115121838: true, 20261115194357: true,
	20261116144907: true, 20261116165926: true, 20261117000528: true, 20261117004117: true,
	20261117023932: true, 20261117034837: true, 20261117064152: true, 20261117213545: true,
	20261117224125: true, 20261119003157: true, 20261119203047: true, 20261121165047: true,
	20261122074350: true, 20261122122826: true, 20261122154643: true, 20261123072431: true,
	20261123200630: true, 20261123212127: true, 20261123221142: true, 20261124023617: true,
	20261124033010: true, 20261126110259: true, 20261126144204: true,
}

const idleRelease = 20261101222956

// retiredOutboxKinds are the kinds no lane holds any more: the three of the merge the daemon no
// longer submits, which 20261029120800 deletes. A database of a release that still queued them is
// upgraded with such a row pending, so the seeds below offer them too and keep the ones the
// schema being upgraded takes.
var retiredOutboxKinds = []record.OutboxKind{"ready_merge", "auto_merge_disable", "merge_queue_publish"}

// Every outbox kind the daemon writes is one the schema's check admits, on a fresh database and on
// databases that applied a lower migration late. 0016 (the READY packet's publish) and 0020
// (controller_notice) would each redefine the check with the other's kind missing, and a database
// past 0020 applies the lower 0016 late, where a check without controller_notice would refuse the
// queued row and the upgrade: neither sets the list, 0021 does, after both. In the same way
// `issue_branch` (0030) and `issue_suspend` (0034) each come with a migration that rewrites the
// check, and a database that ran 0031 through 0038 before 0029 and 0030 applies those two last, so
// a queued close suspension must survive them and both kinds must stand at the end in either
// order. Migrations from 0039 on ship only in binaries that carry 0030, and Migrate applies in
// ascending order, so none can run before it: 0043 sets the list after every kind it names. On a
// fresh database the rewrite with the highest version is the last word, so a kind that only a
// lower one names is refused there: design_changes would be, had its 20261119203047 stood below
// the role-holders kind's 20261116165926 or the merge forward's 20261119003157, whose lists leave
// it out, and is, unless each of the task copies' three lists above it (20261121165047,
// 20261122122826 and 20261123072431) names it too; and step_record, plan_record, conflict_confirm
// and merge_forward are, unless the design news's list, which runs after the step record's
// 20261117000528, the plan record's 20261117023932 and the merge forward's 20261119003157, names
// them too.
func TestTheOutboxCheckAdmitsEveryKindWhicheverOrderTheMigrationsRan(t *testing.T) {
	t.Parallel()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	// Every kind the daemon runs, read from the lanes rather than listed here: a kind added to a
	// lane and forgotten in a hand-kept list is exactly the omission this case exists to catch,
	// and it is how a kind of the role-message lane came to be missing from two rewrites of an
	// earlier release. Every lane is read, the claim lane's claim_release, the documents lane's
	// design_changes and the record lane's step_record and plan_record among them.
	var kinds []record.OutboxKind
	for _, lane := range record.OutboxLanes() {
		kinds = append(kinds, record.LaneKinds(lane)...)
	}
	insert := "insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', '{}', 0, now(), '')"
	for _, tc := range []struct {
		name  string
		first func(migrations.Migration) bool
	}{
		{"a fresh database", func(migrations.Migration) bool { return false }},
		{"a database that ran 0020 before 0016", func(m migrations.Migration) bool { return m.Version <= 20 && m.Version != 16 }},
		{"a database that ran 0031 through 0038 before 0029 and 0030", func(m migrations.Migration) bool { return m.Version <= 28 || (m.Version >= 31 && m.Version <= 38) }},
		// A database the release this branch merged had already migrated, which is every
		// deployment running main: it recorded main's versions, so this branch's binary applies
		// only the versions it adds, and each of main's is skipped for ever. That is the shape
		// `Store.apply` makes possible at all — it asks whether each migration's own version is
		// recorded rather than comparing against the highest — and it is the one a filter on a
		// version number cannot build, because the versions this release adds are spread on
		// either side of main's newest. A rewrite of a CHECK also validates the rows the table
		// holds, so a release whose list forgets a kind another release queues cannot be applied
		// at all: the daemon fails its boot on every restart until someone deletes the row. That
		// is what this case holds, and the seeds below are what make it bite.
		{"a database main migrated", func(m migrations.Migration) bool { return !thisRelease[m.Version] }},
		// The same database one release earlier, at the idle release's own newest migration: this
		// release's binary then applies its own versions and main's later ones together.
		{"a database at the idle release", func(m migrations.Migration) bool {
			return !thisRelease[m.Version] && m.Version <= idleRelease
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			for _, migration := range all {
				if !tc.first(migration) {
					continue
				}
				if _, err := store.apply(ctx, migration); err != nil {
					t.Fatalf("apply %s: %v", migration.Name, err)
				}
			}
			version, err := store.SchemaVersion(ctx)
			if err != nil {
				t.Fatalf("read the schema version: %v", err)
			}
			// Every kind the schema admits where this database stands, seeded before the upgrade
			// runs. A row waits in the outbox until it finishes, so a deployment is upgraded with
			// whatever its own release had queued, and `ADD CONSTRAINT … CHECK` validates the
			// rows the table holds: a migration of this release whose list forgets a kind the
			// other release queues fails the upgrade itself, and the daemon then fails to boot on
			// every restart until someone deletes the row by hand. Which kinds those are is asked
			// of the schema rather than listed here, by trying each and keeping the ones it
			// takes — a kind a release adds to its own list is seeded by that alone, with no
			// version to keep in step — and each failed insert is its own statement, so nothing
			// it leaves behind reaches the next.
			seeded := 0
			for _, kind := range append(kinds, retiredOutboxKinds...) {
				if _, err := store.pool.Exec(ctx, insert, string(kind)); err == nil {
					seeded++
				}
			}
			if version > 0 && seeded == 0 {
				t.Fatalf("the schema at %d admitted no kind at all, so this case seeds nothing", version)
			}
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			// The seeded rows have done their work — the upgrade validated every rewritten CHECK
			// over them — and they go before the final insert, because one kind holds a unique
			// index of its own: gated_steps_publish is one row per project
			// (20261106041346_gated_steps_publish), so a second row of it would be refused for
			// that index rather than for the kind this case is asking about.
			if _, err := store.pool.Exec(ctx, "delete from outbox"); err != nil {
				t.Fatalf("clear the seeded rows: %v", err)
			}
			for _, kind := range kinds {
				if _, err := store.pool.Exec(ctx, insert, string(kind)); err != nil {
					t.Errorf("an outbox row of kind %s: %v", kind, err)
				}
			}
		})
	}
}

// mergeChecksVersion is 20261126144204_checks_of_the_merge_with_main, which writes the phase check
// and the outbox kind check again with every value of both branches of the merge of main 169cfac.
const mergeChecksVersion = 20261126144204

// mainsLateMigrations are main's migrations that a database this branch's head migrated before
// its merge of main lacks and that write either check, all below that head's newest:
// dispatch://INFRA-3320's gate basis, its phase awaiting_children (which writes the phase check
// without step) and its two basis seeds' kind list, and the merge holds' 20261126110259 (which
// writes the kind check with merge_holds_changed and without the three task kinds).
var mainsLateMigrations = []int{20261114064234, 20261114232555, 20261122154643, 20261126110259}

// A database this branch's head migrated before its merge of main takes main's four migrations
// late, after this branch's own lists of the two checks, and each of main's three lists takes this
// branch's values away: the phase step, and the task_branch, task_pull_requests_close and
// task_copy_remove kinds. 20261126144204 stands above every migration and writes both lists again
// with the values of both branches, so the upgraded database takes an issue at step or at
// awaiting_children, and a row of each task kind, each basis seed and main's merge_holds_changed.
// The control is the same
// database upgraded without 20261126144204: the step and a task_branch row are refused there,
// which is what the migration exists for.
func TestTheChecksOfTheMergeWithMainPutBackWhatMainsLateListsTakeAway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	if !slices.ContainsFunc(all, func(m migrations.Migration) bool { return m.Version == mergeChecksVersion }) {
		t.Fatalf("no embedded migration has version %d", mergeChecksVersion)
	}
	for _, tc := range []struct {
		name      string
		withMerge bool
	}{
		{name: "the control, without the merge's checks", withMerge: false},
		{name: "with the merge's checks", withMerge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			var head, late []migrations.Migration
			for _, migration := range all {
				switch {
				case migration.Version == mergeChecksVersion:
					if tc.withMerge {
						late = append(late, migration)
					}
				case slices.Contains(mainsLateMigrations, migration.Version):
					late = append(late, migration)
				default:
					head = append(head, migration)
				}
			}
			if _, err := store.migrate(ctx, head); err != nil {
				t.Fatalf("migrate the database as this branch's head did: %v", err)
			}
			if applied, err := store.migrate(ctx, late); err != nil || applied != len(late) {
				t.Fatalf("the upgrade applied %d of main's late migrations and the merge's (%v), want all %d", applied, err, len(late))
			}
			if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'implementing', 1, 'in_progress', 'A', 0)`); err != nil {
				t.Fatalf("seed the issue: %v", err)
			}
			step := `update issues set phase = 'step' where key = 'LEGION-208'`
			queue := func(kind string) error {
				_, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
					values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
				return err
			}
			if !tc.withMerge {
				if _, err := store.pool.Exec(ctx, step); err == nil || !strings.Contains(err.Error(), "issues_phase_check") {
					t.Fatalf("an issue at step after main's late phase list = %v, want the phase check to refuse it", err)
				}
				if err := queue("task_branch"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
					t.Fatalf("a task_branch row after main's late kind list = %v, want the kind check to refuse it", err)
				}
				return
			}
			if _, err := store.pool.Exec(ctx, step); err != nil {
				t.Fatalf("an issue at step after the merge's checks: %v", err)
			}
			if _, err := store.pool.Exec(ctx, `update issues set phase = 'awaiting_children', children_wait_start = 'implementing',
				children_wait_task = 'finish', children_wait_reason = 'a child of it is open', children_wait_since = now()
				where key = 'LEGION-208'`); err != nil {
				t.Fatalf("an issue at awaiting_children after the merge's checks: %v", err)
			}
			for _, kind := range []string{"task_branch", "task_pull_requests_close", "task_copy_remove",
				"pending_basis_seed", "reparent_seed", "phase_ended", "merge_holds_changed"} {
				if err := queue(kind); err != nil {
					t.Errorf("a %s row after the merge's checks: %v", kind, err)
				}
			}
		})
	}
}

// A database can hold issue_suspend rows the previous release queued with a `release` key when
// 0051 arrives. An issue's close now deletes its Sandbox in every case, the field is gone, and the
// payload decodes strictly (record.DecodeOutboxPayload), so a row still carrying the key would
// fail to decode and stall its issue's lifecycle lane. The migration strips the key and leaves
// everything else of the row as it is; a row that never had it is untouched.
func TestTheIssueSuspendMigrationStripsTheReleaseKeyFromQueuedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 50)
	insert := `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('issue_suspend', $1, $2, 0, now(), '') returning id`
	var releasing, keeping int64
	if err := store.pool.QueryRow(ctx, insert, "LEGION-209", `{"tree":"LEGION-208","generation":3,"treeGeneration":7,"release":true}`).Scan(&releasing); err != nil {
		t.Fatalf("queue the releasing suspension the previous release wrote: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, "LEGION-210", `{"tree":"LEGION-208","generation":4,"treeGeneration":7}`).Scan(&keeping); err != nil {
		t.Fatalf("queue the suspension written without the field: %v", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the release-key strip: %v", err)
	}
	for _, want := range []struct {
		id      int64
		payload record.IssueSuspend
	}{
		{releasing, record.IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7}},
		{keeping, record.IssueSuspend{Tree: "LEGION-208", Generation: 4, TreeGeneration: 7}},
	} {
		var payload []byte
		if err := store.pool.QueryRow(ctx, "select payload from outbox where id = $1", want.id).Scan(&payload); err != nil {
			t.Fatalf("read row %d: %v", want.id, err)
		}
		got, err := record.DecodeOutboxPayload(record.OutboxRow{ID: want.id, Kind: record.OutboxKindIssueSuspend, Payload: payload})
		if err != nil {
			t.Fatalf("decode row %d (%s) after the migration: %v", want.id, payload, err)
		}
		if got != want.payload {
			t.Fatalf("row %d decodes as %#v, want %#v", want.id, got, want.payload)
		}
	}
}
