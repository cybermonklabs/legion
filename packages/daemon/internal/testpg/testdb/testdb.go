// Package testdb gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows; ways to
// reach it across a network's delay (Delayed) or through a proxy that holds or cuts a commit's
// reply (CommitFaults); and a pool whose transactions are cancelled between two statements
// (CancelAfterScan). A test is skipped without that Postgres: there is nothing to run against.
// TableRows and ChangedTables (rows.go) read every table of a database's current schema whole, so
// a test can say which tables a write changed, a row changed in place among them, or that it
// changed none.
//
// Database is an empty database, as a URL a pgx pool opens (PoolBounded), and PlainDatabase the
// same as a URL any other client reads. A test that needs the daemon's schema takes
// internal/testpg's Clone or CloneStore instead, which copy a template that package migrated once.
//
// This package imports no package of this module except internal/testwait, which imports none,
// and nothing added here may import another: migrating the template needs internal/store, so
// internal/testpg imports it, and a test binary of internal/store that imported internal/testpg
// back would be a cycle. Keeping the schema-free half apart is what lets internal/store's own
// tests take the same fixture every other package's tests take.
//
// DatabaseURL, Delayed, CommitFaults and PoolBounded are the rewrites of LEGION_TEST_PG_DSN a test
// binary of this module makes, each through rewriteDSN; DatabaseURL's comment says which callers
// make them and what the check refuses. RandomName is exported for internal/testpg alone, which
// names its template and each clone of it with it.
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Image is the Postgres server image a developer starts for the daemon's own tests: PostgreSQL 18
// (tag 18, server 18.6-1.pgdg13+2), pinned by digest through the mirror. 18 is also the major
// version of the Postgres service CI's daemon Go job runs (ci.yaml's `daemon-go` job), and
// the version envoy-infra gives a new Cloud SQL instance.
const Image = "mirror.gcr.io/library/postgres@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4"

// SkipMessage is what a test is skipped with when there is no Postgres to run it against. It is
// exported for the packages whose own skip says the same thing (internal/daemon's testDSN). It
// names the pgvector image CI's Postgres services run (.github/ci-images.yaml, its pgvector row)
// rather than Image: the daemon tests that boot a scratch Dispatch run Dispatch's migrations,
// which need the vector extension that Image lacks.
const SkipMessage = "LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. In a Legion pod, read skill://legion-worker/references/tools.md, \"The database your tests use\"; never the sessions database. Elsewhere, start one and set it:\n" +
	"  docker run -d --name legion-pg --cpus 2 --memory 2g -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 docker.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11\n" +
	"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
	"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./..."

// Database is the URL of a new, empty database named <prefix>_<random>, dropped at the test's end,
// for a pgx pool to open: store.Open, pgxpool, or a daemon's postgres_dsn. A pool opened on it
// holds PoolConns connections unless LEGION_TEST_PG_DSN sets a size of its own, which it keeps
// (PoolBounded), so a test's pools do not grow with the machine's CPU count. A URL that libpq,
// pgx.Connect, psql, a scratch Dispatch or another child process reads is PlainDatabase's, which
// holds no pool key whatever LEGION_TEST_PG_DSN holds (poolKeys says why).
func Database(t testing.TB, prefix string) string {
	t.Helper()
	plain := PlainDatabase(t, prefix)
	bounded, err := PoolBounded(os.Getenv(dsnName), plain)
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

// PlainDatabase is Database's database as a URL that holds none of pgxpool's keys, even where
// LEGION_TEST_PG_DSN holds one (DatabaseURL takes them out): the URL a client other than a pgx
// pool reads (Database says which).
func PlainDatabase(t testing.TB, prefix string) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(SkipMessage)
	}
	database, drop, err := create(dsn, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := testwait.TeardownContext(t)
		defer cancel()
		if err := drop(ctx); err != nil {
			t.Error(err)
		}
	})
	return database
}

// create makes an empty database named <prefix>_<random> on the server dsn names, and answers its
// URL and a function that drops it with the context its caller supplies, which also closes the
// admin connection create opened. Both URLs it connects or points a caller at — the admin
// database's and the new database's — come from DatabaseURL, and it makes them before it connects,
// so a DSN whose rewrite names another server ends the call with no connection and no database.
// Database is its caller: one database per test.
func create(dsn, prefix string) (string, func(context.Context) error, error) {
	admin, err := DatabaseURL(dsn, "postgres")
	if err != nil {
		return "", nil, err
	}
	name, err := RandomName(prefix)
	if err != nil {
		return "", nil, err
	}
	database, err := DatabaseURL(dsn, name)
	if err != nil {
		return "", nil, err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	// A copy of template1's files. Created the default way, from PostgreSQL 15, every page of
	// template1 goes into the write-ahead log: 4.4 MB of log for each empty database.
	if _, err := conn.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		_ = conn.Close(ctx)
		return "", nil, fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := conn.Exec(ctx, WithoutJIT(name)); err != nil {
		_ = Drop(ctx, conn, name)
		_ = conn.Close(ctx)
		return "", nil, fmt.Errorf("turn JIT off on %s: %w", name, err)
	}
	drop := func(ctx context.Context) error {
		defer func() { _ = conn.Close(context.Background()) }()
		if err := Drop(ctx, conn, name); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	return database, drop, nil
}

// WithoutJIT is the statement that turns JIT off for every session that connects to the database
// name from then on. The daemon's database is the one its operator names: a Kubernetes
// deployment's session store is Cloud SQL (docs/kubernetes.md, session_store: cloudsql-iam), which
// lists LLVM JIT compilation among the features it does not support
// (https://cloud.google.com/sql/docs/postgres/features, "Unsupported features"), and a box's is
// whatever postgres_dsn names, whose jit a test cannot know. CI's Postgres image runs with jit =
// on, under which a statement whose estimate passes jit_above_cost compiles before it runs, so a
// test's time would hold compilation that a Cloud SQL store never pays. The setting is the
// database's, so it reaches every pool and process a test opens there, and it goes with the
// database: no setting of the server changes. A clone does not copy its template's settings, so
// every database here takes it when it is made (create, testpg's template and Clone; docs/testing.md,
// "A database a Go test creates").
func WithoutJIT(name string) string {
	return "alter database " + name + " set jit = off"
}

// insufficientPrivilege is PostgreSQL's 42501, which a forced drop answers when a session of the
// database belongs to another role.
const insufficientPrivilege = "42501"

// objectInUse is PostgreSQL's 55006, which a forced drop answers while a backend it terminated
// has not exited yet: PostgreSQL waits only a few seconds for one before it gives up.
const objectInUse = "55006"

// Execer is the admin handle Drop needs: a forced drop and the read that identifies the database's
// backends when the test's teardown deadline ends.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Drop drops name, waiting out an autovacuum worker and a backend that has not exited yet.
// `drop database ... with (force)` terminates every other session of the database, which
// PostgreSQL refuses to a login that is neither superuser nor a member of pg_signal_backend as
// soon as one session belongs to another role — an autovacuum worker, which attaches to a
// database a test has just filled. Where the login may terminate every session, the drop then
// waits only a few seconds for those backends to exit, and answers 55006 while one has not.
//
// Both refusals are retried until ctx ends. A caller supplies the test binary's bound: a test's
// cleanup takes testwait.TeardownContext(t), which stays before go test's alarm and is finite
// under -timeout 0; testpg.Main uses testwait.TeardownDeadline after m.Run has returned. That
// lets a correct teardown survive machine load but still ends a holder that never leaves, naming
// its backends. Any error that is not either refusal is the caller's at once.
//
// It lives here rather than in internal/testpg because this package imports no package of the
// module except internal/testwait, which imports none, so every package's tests can take it —
// internal/store's among them, whose own tests cannot import internal/testpg at all.
func Drop(ctx context.Context, admin Execer, name string) error {
	var (
		attempt  int
		last     error
		reason   string
		backends = "not read"
	)
	for {
		if cause := ctx.Err(); cause != nil {
			if last == nil {
				last = errors.New("no forced drop ran before its context ended")
				reason = "the context ended before a forced drop ran"
			}
			return dropDeadlineError(cause, last, attempt, reason, name, backends)
		}
		attempt++
		_, err := admin.Exec(ctx, "drop database "+name+" with (force)")
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			if cause := ctx.Err(); cause != nil {
				if last == nil {
					last = err
					reason = "the forced drop did not finish before its context ended"
				}
				return dropDeadlineError(cause, last, attempt, reason, name, backends)
			}
			return err
		}
		switch pgErr.Code {
		case insufficientPrivilege:
			reason = "a session of another role, an autovacuum worker, holds the database and this login may not terminate it"
		case objectInUse:
			reason = "a backend the forced drop terminated has not exited"
		default:
			return err
		}
		last = err
		if read := holdingBackends(ctx, admin, name); read != "" {
			backends = read
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return dropDeadlineError(ctx.Err(), err, attempt, reason, name, backends)
		case <-timer.C:
		}
	}
}

// holdingBackends names the backends still attached to the database. It returns "" when the read
// fails, so the caller keeps the last list a live context read rather than a cut read's error. It
// prints usename, state, the wait event and the backend type, never the statement text, which can
// hold a role's password.
func holdingBackends(ctx context.Context, admin Execer, name string) string {
	var backends string
	if err := admin.QueryRow(ctx, `select coalesce(string_agg(
		'pid=' || pid::text || ' type=' || coalesce(backend_type, 'not visible') ||
		' user=' || coalesce(usename, '') || ' state=' || coalesce(state, '') || ' wait=' ||
		coalesce(nullif(coalesce(wait_event_type, '') || '/' || coalesce(wait_event, ''), '/'), 'none'),
		'; ' order by pid), 'none')
		from pg_stat_activity where datname = $1 and pid <> pg_backend_pid()`, name).Scan(&backends); err != nil {
		return ""
	}
	return backends
}

// dropDeadlineError keeps the PostgreSQL error that explains why the drop was retried, and names
// the backends the last live pg_stat_activity read saw. reason says what the context's end
// interrupted, so it reads right whether or not any attempt was refused.
func dropDeadlineError(cause, err error, attempts int, reason, name, backends string) error {
	return fmt.Errorf("%w (gave up dropping %s after %d attempt(s) before its context bound: %s; backends still holding %s: %s): %w",
		err, name, attempts, reason, name, backends, cause)
}

// RandomName is prefix with a random suffix, so two databases of one prefix never collide. It is
// exported for internal/testpg, which names its template and each clone with it.
func RandomName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
}

// DatabaseURL is the URL of database on the server dsn names: dsn with its path replaced and every
// pool key taken out of its query (poolKeys), every other byte of the query kept. Its URL goes to
// readers that are no pool, and PoolBounded puts the pool keys back for a pool. Every database a
// test binary of this module names from LEGION_TEST_PG_DSN is named here — create above, and
// internal/testpg's prepare and Clone — so one place reads each rewrite back, and no caller checks
// itself. The proxy URLs of Delayed and CommitFaults and PoolBounded's pool URL are the other
// rewrites of a DSN this package makes, and they take the same check (rewriteDSN below). dsn is
// what LEGION_TEST_PG_DSN holds, which every caller read from that variable, and a refusal names
// it.
//
// A rewrite from which pgx reads anything other than dsn's reading with the change asked for is
// refused: another host, port, user or database, and another password, run-time parameter or
// connection setting (rewriteDSN's reading). Go's net/url ends the user name at the last `@` and
// it starts the query at the first `?`. libpq and pgx do neither. So a DSN whose host libpq reads
// as loopback, which is the only DSN the live proofs' shell guard admits
// (scripts/e2e/lib/loopback-dsn.sh), can rewrite into a URL pgx connects to another machine with.
//
// A refusal names which of host, port, user, database, credential (the password), parameters and
// connection settings pgx reads from the rewrite other than the connection the rewrite was asked
// for. It prints no value and no text of either parser, because the value it would print is the
// misreading itself: a password a parser reads as a host, a port or a database is printed as that
// host, port or database, and the text a parser quotes is the part of the string it stopped in. A
// string a parser refuses earns a fixed line naming which string it was, the caller's DSN or the
// rewrite built from it, and nothing more.
//
// What that costs a reader of a refusal: they learn which fields moved, and not what they moved
// to. The DSN is the caller's own, and the caller can read it.
//
// This check is the second of the two frames that hold a live proof to the server its caller
// named, and it is the one place the Go half of those frames is written. The first frame is that
// shell guard: it reads the caller's DSN with libpq before anything starts, and it reaches no
// further than the shell. The comment above require_loopback_dsn is the one place the shell half
// is written. testdb_test.go drives the cases of this check.
func DatabaseURL(dsn, database string) (string, error) {
	return rewriteDSN(dsnName, dsn, rewrite{database: database, pools: true})
}

// dsnName is the variable every DSN this package rewrites was read from, and it starts a refusal.
const dsnName = "LEGION_TEST_PG_DSN"

// PoolConns is the size PoolBounded gives a pool when LEGION_TEST_PG_DSN sets no size of its own
// (pool_max_conns); a size the DSN sets is kept. A pool that is not told its size is sized by pgx
// at the greater of 4 and the machine's CPU count (pgxpool.ParseConfig), so on a box with 288 CPUs
// one test that runs 100 statements at once asks for 100 connections, which is all PostgreSQL's
// default max_connections allows, and fails with "too many clients" there while it passes on a
// small machine. 4 is pgx's own floor: the size every pool has on a machine with 4 CPUs or fewer.
const PoolConns = 4

// poolMaxConns is the key of a DSN's query that pgxpool reads as its pool's size.
const poolMaxConns = "pool_max_conns"

// poolKeys are the keys of a DSN's query that pgxpool reads and takes out before it connects
// (pgxpool.ParseConfig, pgx v5.13.0). Every other reader of a URL fails on one: pgx.Connect sends
// it to the server as a setting the server does not know, libpq and psql refuse a URL that holds
// one, and Dispatch's store.Open refuses one that sets pool_max_conns.
var poolKeys = map[string]bool{
	poolMaxConns: true, "pool_min_conns": true, "pool_min_idle_conns": true, "pool_max_conn_lifetime": true,
	"pool_max_conn_lifetime_jitter": true, "pool_max_conn_idle_time": true, "pool_health_check_period": true,
	"pool_ping_timeout": true,
}

// splitPoolPairs is a URL's raw query split as pgx splits it, on `&`, into the pairs whose key is
// one of poolKeys and the rest, each pair kept byte for byte. A key is read as pgx reads it, with
// its %XX decoded and a `+` kept.
func splitPoolPairs(rawQuery string) (pool, rest []string) {
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		if decoded, err := url.PathUnescape(key); err == nil && poolKeys[decoded] {
			pool = append(pool, pair)
		} else {
			rest = append(rest, pair)
		}
	}
	return pool, rest
}

// PoolBounded is dsn as a URL for a pgx pool, with the pool keys LEGION_TEST_PG_DSN sets (server,
// as the caller read it from that variable) in place of any dsn holds, and pool_max_conns set to
// PoolConns where server sets no size. dsn is a URL DatabaseURL made, which holds no pool key, or
// server itself. Every other byte of dsn's query is kept, since pgx reads a query with its own
// libpq-style parser (pgconn's parse_url.go), which decodes only %XX and keeps a `+`, where Go's
// url.Values would write an encoded space back as `+` and drop a pair holding `;`. Only pgxpool
// reads a pool key (poolKeys), so its URL goes only to pools: Database's, internal/testpg's Clone
// and template's, and internal/daemon's and internal/store's testDSN. DatabaseURL's and
// PlainDatabase's, which an admin connection, psql and a scratch Dispatch are given, hold none.
func PoolBounded(server, dsn string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("%s: net/url refuses it", dsnName)
	}
	config, err := pgconn.ParseConfig(server)
	if err != nil {
		return "", fmt.Errorf("%s: pgx refuses it", dsnName)
	}
	pool, _ := splitPoolPairs(parsed.RawQuery)
	read := map[string]string{}
	for key, value := range config.RuntimeParams {
		if poolKeys[key] {
			read[key] = value
		}
	}
	if _, sized := read[poolMaxConns]; !sized {
		pool = append(pool, poolMaxConns+"="+strconv.Itoa(PoolConns))
		read[poolMaxConns] = strconv.Itoa(PoolConns)
	}
	return rewriteDSN(dsnName, dsn, rewrite{pools: true, pool: pool, poolRead: read})
}

// rewrite is what a caller of rewriteDSN changes about the DSN it was given: the database the
// rewrite must name (DatabaseURL), the address it must send the connection to (Delayed,
// CommitFaults, Outage), the pool keys of its query, and whether TLS is off. With pools set, the
// rewrite takes every pool key out of the query and appends pool, the raw pairs pgx reads as
// poolRead: none for DatabaseURL, the server's own or pool_max_conns=PoolConns for PoolBounded.
// With plain set, it appends sslmode=disable, which pgx reads over an sslmode the query already
// holds since it keeps the last of a repeated key (pgconn's parse_url.go), so an attempt makes no
// TLS connection and falls back to none (Outage). A field left empty is one the rewrite keeps.
type rewrite struct {
	database string
	host     string
	port     uint16
	pools    bool
	pool     []string
	poolRead map[string]string
	plain    bool
}

// applied is read with this rewrite in it, which is what pgx must read from the rewritten DSN.
func (r rewrite) applied(read reading) reading {
	if r.database != "" {
		read.database = r.database
	}
	if r.host != "" {
		read.host, read.port = r.host, r.port
	}
	if r.pools {
		params := make(map[string]string, len(read.params)+len(r.poolRead))
		for key, value := range read.params {
			if !poolKeys[key] {
				params[key] = value
			}
		}
		maps.Copy(params, r.poolRead)
		read.params = params
	}
	if r.plain {
		read.settings.tls, read.settings.fallbacks = tlsReading{}, ""
	}
	return read
}

// reading is what pgconn.ParseConfig read from a DSN: the host, port, user and database a rewrite
// keeps or sets, the password, every run-time parameter (each key pgx does not take itself,
// pool_max_conns among them, which pgxpool takes out after), and the rest of what pgx reads from a
// DSN's settings (settings). A refusal names the fields that differ and prints no value of one.
type reading struct {
	host     string
	port     uint16
	user     string
	database string
	password string
	params   map[string]string
	settings settings
}

// settings is what pgx reads from a DSN's settings beside the fields reading names on its own:
// the connect timeout, the TLS each attempt makes (sslmode and the files and checks it names), the
// attempts it falls back to, and the authentication and protocol options. The server name a TLS
// attempt verifies, and the host and port of each fallback, follow the host, which reading compares
// on its own and Delayed moves.
type settings struct {
	connectTimeout           time.Duration
	tls                      tlsReading
	fallbacks                string
	kerberosSrvName          string
	kerberosSpn              string
	sslNegotiation           string
	sslCertMode              string
	minProtocol, maxProtocol string
	channelBinding           string
	requireAuth              string
}

// tlsReading is what a TLS configuration pgx built from a DSN says of the check it makes: none
// (off), or whether it skips the server's name, verifies the chain itself, trusts named roots, and
// sends a certificate of its own.
type tlsReading struct {
	on, skipsName, verifiesChain, namesRoots, sendsCertificate bool
}

func tlsReadingOf(config *tls.Config) tlsReading {
	if config == nil {
		return tlsReading{}
	}
	return tlsReading{on: true, skipsName: config.InsecureSkipVerify, verifiesChain: config.VerifyPeerCertificate != nil,
		namesRoots: config.RootCAs != nil, sendsCertificate: len(config.Certificates) != 0 || config.GetClientCertificate != nil}
}

// moved names the fields of this reading that are not asked's, in the order the fields are
// written above, and nothing where the two readings are the same. It is what a refusal says of a
// rewrite: which fields moved, and no value of one.
func (r reading) moved(asked reading) string {
	var fields []string
	for _, field := range []struct {
		name  string
		moved bool
	}{
		{"host", r.host != asked.host},
		{"port", r.port != asked.port},
		{"user", r.user != asked.user},
		{"database", r.database != asked.database},
		// The password is named "credential": a refusal prints no part of a password, and the
		// word "password" holds the letters of short ones ("ss", "word").
		{"credential", r.password != asked.password},
		{"parameters", !maps.Equal(r.params, asked.params)},
		{"connection settings", r.settings != asked.settings},
	} {
		if field.moved {
			fields = append(fields, field.name)
		}
	}
	switch len(fields) {
	case 0:
		return ""
	case 1:
		return fields[0]
	default:
		return strings.Join(fields[:len(fields)-1], ", ") + " and " + fields[len(fields)-1]
	}
}

// readingOf is what pgx read from a DSN it took.
func readingOf(cfg *pgconn.Config) reading {
	fallbacks := make([]string, 0, len(cfg.Fallbacks))
	for _, fallback := range cfg.Fallbacks {
		fallbacks = append(fallbacks, fmt.Sprintf("%+v", tlsReadingOf(fallback.TLSConfig)))
	}
	return reading{host: cfg.Host, port: cfg.Port, user: cfg.User, database: cfg.Database, password: cfg.Password,
		params: maps.Clone(cfg.RuntimeParams), settings: settings{
			connectTimeout: cfg.ConnectTimeout, tls: tlsReadingOf(cfg.TLSConfig), fallbacks: strings.Join(fallbacks, ";"),
			kerberosSrvName: cfg.KerberosSrvName, kerberosSpn: cfg.KerberosSpn, sslNegotiation: cfg.SSLNegotiation,
			sslCertMode: cfg.SSLCertMode, minProtocol: cfg.MinProtocolVersion, maxProtocol: cfg.MaxProtocolVersion,
			channelBinding: cfg.ChannelBinding, requireAuth: cfg.RequireAuth,
		}}
}

// rewriteDSN is dsn rewritten as want says: one rewrite of dsn's URL, read back with
// pgconn.ParseConfig, which is the parser pgx connects through. It answers that rewrite where pgx
// reads it as the reading of dsn with want in it, every value pgx reads compared (reading), and a
// refusal and no DSN at all otherwise. name is what the caller read dsn from, and it starts the
// refusal. The query is never rebuilt: a key the rewrite adds is appended to it as it stands.
//
// Three strings go through a parser here: dsn through net/url, dsn through pgx, and the rewrite
// through pgx. A parser that refuses one of them earns the fixed line that names which string it
// was. Where pgx reads both strings, this function compares the rewrite's reading with dsn's
// reading with want in it, and the refusal names the fields that differ. DatabaseURL's comment
// says why no line holds a value.
//
// The answer stays a URL, which is the form the DSN came in. A reader downstream takes that form
// and no keyword/value connection string (host=… dbname=…): packages/pi-legion's rig scenarios
// read a run's database out of a DSN with new URL(), and Bun.SQL takes a URL.
func rewriteDSN(name, dsn string, want rewrite) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("%s: net/url refuses it", name)
	}
	before, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("%s: pgx refuses it", name)
	}
	read := readingOf(before)
	if want.database != "" {
		parsed.Path = "/" + want.database
	}
	if want.host != "" {
		parsed.Host = net.JoinHostPort(want.host, strconv.Itoa(int(want.port)))
	}
	if want.pools {
		_, rest := splitPoolPairs(parsed.RawQuery)
		parsed.RawQuery = strings.Join(append(rest, want.pool...), "&")
	}
	if want.plain {
		if parsed.RawQuery != "" {
			parsed.RawQuery += "&"
		}
		parsed.RawQuery += "sslmode=disable"
	}
	rewritten := parsed.String()
	after, err := pgconn.ParseConfig(rewritten)
	if err != nil {
		return "", fmt.Errorf("the rewrite of %s: pgx refuses it", name)
	}
	if moved := readingOf(after).moved(want.applied(read)); moved != "" {
		return "", fmt.Errorf(
			"the rewrite of %s is not the connection it was asked for: pgx reads another %s from it",
			name, moved)
	}
	return rewritten, nil
}

// DSNFile is a 0600 file under the test's temporary directory holding PlainDatabase's URL, as the
// providers Secret's key holds the session database's: the session database is read with
// pgx.Connect (internal/ompsessions), never through a pool.
func DSNFile(t testing.TB, prefix string) (dsn, file string) {
	t.Helper()
	dsn = PlainDatabase(t, prefix)
	file = filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(file, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dsn, file
}
