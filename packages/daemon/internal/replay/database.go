package replay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// replayDatabasePrefix begins the name of every database a replay makes (legion_replay_<run id>),
// so a database a run kept (RunConfig.KeepDB) is told from every other on its server. The
// templates the runs are copied from (replayTemplatePrefix) and a template still being made
// (replayBuildPrefix) begin with it too.
const replayDatabasePrefix = "legion_replay_"

// replayTemplatePrefix begins the name of each template a replay's database is copied from:
// legion_replay_template_<key>, the key templateKey's.
const replayTemplatePrefix = replayDatabasePrefix + "template_"

// replayBuildPrefix begins the name a template is made under (legion_replay_build_<id>) before it
// takes its keyed name, so no run ever copies a template that is not whole. A database of this
// name stays on the server only where the process making it stopped before it was done.
const replayBuildPrefix = replayDatabasePrefix + "build_"

// templateLockClass is the first key of the session advisory lock under which a template is made
// (replayTemplate), in the two-key space of pg_advisory_lock(int, int), apart from the one-key
// space every lock of the daemon's own store lives in. The second key is the template's name.
const templateLockClass = "legion.replay.template"

// The replay opens dedicated connections, not transaction-pooled sessions. On the databases it
// owns, unqualified store and migration tables resolve in public, with catalogs searched first
// implicitly and temporary relations last. The admin connection needs no application schema.
const replaySearchPath = "public,pg_temp"

// replayTables is the tables a rule replays read that no daemon migration makes, created by the
// replay in its own database after the daemon's migrations, and in no daemon's database, since the
// daemon runs none of these rules: the merger's ready mark inside merging of the release at
// 9542c6fb (ReadyMark, assumption 20), which the rule legion-v19.57.3 reads, kept for the issue's
// generation and the merger's phase entry it was made in, so a later visit to merging reads none
// of it; the sign-offs the workflow held behind an issue beneath, which the rule cover reads as a
// wait (HeldSignOff), kept the same way for the architect's entry into accepting; the covers
// that stand under the rule cover, the holder of each covered waiter; the moment of each issue's
// first slot in a generation, which the rules returned-by-first-slot, rework-first and
// rework-first-keep place the line's returned part by, and arrival-order an issue with no arrival
// kept, kept from the slot rows the passes read and from the start state; and the moment each issue
// arrived in the line in a generation, which the rules rework-first, rework-first-keep and
// arrival-order place by, kept from the records the passes read and from the start state. The
// admission pass reads them, and keeps the covers, the first slots and the arrivals, through
// ruleTables; a read that may write nothing, such as the start state's view, keeps none of them. A
// rule added with a table of its own adds it here.
var replayTables = []string{
	`create table replay_ready_marks (
		issue text not null,
		generation bigint not null,
		entry bigint not null,
		marked_at timestamptz not null,
		primary key (issue, generation, entry)
	)`,
	`create table replay_held_signoffs (
		issue text not null,
		generation bigint not null,
		entry bigint not null,
		held_at timestamptz not null,
		primary key (issue, generation, entry)
	)`,
	`create table replay_covers (
		waiter text primary key,
		holder text not null unique
	)`,
	`create table replay_first_slots (
		issue text not null,
		generation bigint not null,
		slot_at timestamptz not null,
		primary key (issue, generation)
	)`,
	`create table replay_arrivals (
		issue text not null,
		generation bigint not null,
		arrived_at timestamptz not null,
		primary key (issue, generation)
	)`,
}

// replayDatabase is the database one replay made on its DSN's server: its name, the URL it is
// reached at, and the URL of the DSN's own database, which the replay connects to only to make and
// drop its own.
type replayDatabase struct {
	name, url, admin string
}

// openReplayDatabase makes a database of the replay's own on the server dsn names, named
// legion_replay_<run id>, as a file copy of the template that carries the daemon's migrations and
// the replay's own tables (replayTemplate), set to commit without waiting for the write-ahead log,
// and answers it with the store opened on it. The first run on a server makes the template; every
// later run, of this process or another, copies it, so no run migrates a database of its own. It
// never writes the database dsn names: that connection only makes the replay's databases, and
// takes the lock their template is made under. A DSN on which no database can be made is refused
// with the server's reason.
func openReplayDatabase(ctx context.Context, dsn string) (replayDatabase, *store.Store, error) {
	if dsn == "" {
		return replayDatabase{}, nil, errors.New("replay: no Postgres DSN to make the replay's database on (--pg-dsn)")
	}
	all, err := migrations.All()
	if err != nil {
		return replayDatabase{}, nil, fmt.Errorf("replay: read the daemon's migrations: %w", err)
	}
	name := randomName(replayDatabasePrefix)
	target, err := databaseURL(dsn, name)
	if err != nil {
		return replayDatabase{}, nil, err
	}
	db := replayDatabase{name: name, url: target, admin: dsn}
	admin, err := connectReplayAdmin(ctx, dsn)
	if err != nil {
		return replayDatabase{}, nil, fmt.Errorf("replay: connect to %s to make the replay's database: %w", redacted(dsn), err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	template, err := replayTemplate(ctx, admin, dsn, templateKey(all, replayTables))
	if err != nil {
		return replayDatabase{}, nil, err
	}
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()+" template "+pgx.Identifier{template}.Sanitize()+" strategy file_copy"); err != nil {
		return replayDatabase{}, nil, fmt.Errorf("replay: the server of %s refuses to make the replay's database %s from %s: %w", redacted(dsn), name, template, err)
	}
	// Every fact commits, and a replay's facts are many: a commit that does not wait for the
	// write-ahead log to reach the disk changes no decision. A copy takes none of its template's
	// settings, and the setting reaches every connection opened after it, so it is set here,
	// before the store opens.
	if _, err := admin.Exec(ctx, "alter database "+pgx.Identifier{name}.Sanitize()+" set synchronous_commit = off"); err != nil {
		return db, nil, errors.Join(fmt.Errorf("replay: set the replay's database %s to commit without waiting for the disk: %w", name, err), db.drop(ctx))
	}
	st, err := store.Open(ctx, target)
	if err != nil {
		return db, nil, errors.Join(fmt.Errorf("replay: open the replay's database %s: %w", name, err), db.drop(ctx))
	}
	return db, st, nil
}

// templateKey is the key of the template for the migration set all and the replay's own tables:
// the first 16 bytes, in hex, of the SHA-256 of the count of all, each migration's name and SQL in
// the order given, the count of tables, each table's statement and the schema search path.
// Every field is preceded by its length so moving bytes between fields gives a different key.
// A template made for other migrations, tables or a different search path is never copied.
func templateKey(all []migrations.Migration, tables []string) string {
	sum := sha256.New()
	count := func(n int) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(n))
		_, _ = sum.Write(size[:])
	}
	// A hash's Write never returns an error.
	field := func(value string) {
		count(len(value))
		_, _ = io.WriteString(sum, value)
	}
	count(len(all))
	for _, migration := range all {
		field(migration.Name)
		field(migration.SQL)
	}
	count(len(tables))
	for _, table := range tables {
		field(table)
	}
	field(replaySearchPath)
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// replayTemplate is the name of the template on admin's server that a replay's database is
// copied from, legion_replay_template_<key>, made where the server holds none. It is made at most
// once: under a session advisory lock taken on admin, which the lock's holder releases once the
// template stands, every other run on the DSN's database waits for it and then finds the template
// made. The template is made under a name of its own (replayBuildPrefix) as a file copy of
// template1, so it carries the server's extensions as template1 held them then; it is migrated
// with the daemon's migrations, given the replay's own tables (replayTables), closed to
// connections, so no session can keep a copy from being made, and only then renamed to its keyed
// name, so a template that is not whole is never copied. A build that fails is dropped. Another
// process making the same template outside the lock (one whose DSN names another database of the
// server, where the lock is another lock) can only lose the rename, and then drops its own copy
// and answers the one that won.
//
// A template stays on the server for the runs after it. An operator removes the templates with
// `drop database` of each database whose name begins with legion_replay_template_, and of a
// legion_replay_build_ database a stopped process left, while no replay runs on the server; the
// next run makes the template again.
func replayTemplate(ctx context.Context, admin *pgx.Conn, dsn, key string) (string, error) {
	template := replayTemplatePrefix + key
	if _, err := admin.Exec(ctx, "select pg_advisory_lock(hashtext($1), hashtext($2))", templateLockClass, template); err != nil {
		return "", fmt.Errorf("replay: take the lock the template %s is made under: %w", template, err)
	}
	// Closing admin releases the lock too, so an unlock that fails leaves it held only until
	// openReplayDatabase returns.
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), "select pg_advisory_unlock(hashtext($1), hashtext($2))", templateLockClass, template)
	}()
	var made bool
	if err := admin.QueryRow(ctx, "select exists (select from pg_database where datname = $1)", template).Scan(&made); err != nil {
		return "", fmt.Errorf("replay: read whether the server of %s holds the template %s: %w", redacted(dsn), template, err)
	}
	if made {
		return template, nil
	}
	name := randomName(replayBuildPrefix)
	buildURL, err := databaseURL(dsn, name)
	if err != nil {
		return "", err
	}
	build := replayDatabase{name: name, url: buildURL, admin: dsn}
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()+" template template1 strategy file_copy"); err != nil {
		return "", fmt.Errorf("replay: the server of %s refuses to make the template %s: %w", redacted(dsn), template, err)
	}
	if err := migrateTemplate(ctx, buildURL); err != nil {
		return "", errors.Join(fmt.Errorf("replay: make the template %s in %s: %w", template, name, err), build.drop(ctx))
	}
	// One statement string runs as one transaction: the template is closed to connections and
	// renamed together, or neither. A rename that finds a session of the build's own store still
	// closing is tried again for a while, as a drop is.
	quoted := pgx.Identifier{name}.Sanitize()
	rename := "alter database " + quoted + " with allow_connections false; alter database " + quoted + " rename to " + pgx.Identifier{template}.Sanitize()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := admin.Exec(ctx, rename)
		var refused *pgconn.PgError
		switch {
		case err == nil:
			return template, nil
		case errors.As(err, &refused) && refused.Code == duplicateDatabase:
			return template, build.drop(ctx)
		case errors.As(err, &refused) && refused.Code == objectInUse && time.Now().Before(deadline):
			time.Sleep(250 * time.Millisecond)
		default:
			return "", errors.Join(fmt.Errorf("replay: give the template %s its name: %w", template, err), build.drop(ctx))
		}
	}
}

// duplicateDatabase is PostgreSQL's 42P04, which a rename answers when a database of that name
// stands already.
const duplicateDatabase = "42P04"

// migrateTemplate migrates the database target names with the daemon's migrations and creates the
// replay's own tables in it, then closes every connection it opened.
func migrateTemplate(ctx context.Context, target string) error {
	st, err := store.Open(ctx, target)
	if err != nil {
		return fmt.Errorf("open it: %w", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate it: %w", err)
	}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, table := range replayTables {
			if _, err := tx.Exec(ctx, table); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("create the replay's own tables in it: %w", err)
	}
	return nil
}

// randomName is prefix followed by 16 random hex digits.
func randomName(prefix string) string {
	var suffix [8]byte
	// crypto/rand.Read fills the buffer whole and never returns an error.
	_, _ = rand.Read(suffix[:])
	return prefix + hex.EncodeToString(suffix[:])
}

// drop drops the replay's database, from a connection to the DSN's own, which it writes nothing
// to. A drop that finds a session of the database still closing is tried again for a while.
func (db replayDatabase) drop(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	admin, err := connectReplayAdmin(ctx, db.admin)
	if err != nil {
		return fmt.Errorf("replay: connect to drop the replay's database %s: %w", db.name, err)
	}
	defer func() { _ = admin.Close(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := admin.Exec(ctx, "drop database if exists "+pgx.Identifier{db.name}.Sanitize()+" with (force)")
		var refused *pgconn.PgError
		if err == nil || !errors.As(err, &refused) || refused.Code != objectInUse || time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("replay: drop the replay's database %s: %w", db.name, err)
			}
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// objectInUse is PostgreSQL's 55006, which a forced drop answers while a session it terminated has
// not exited yet.
const objectInUse = "55006"

func connectReplayAdmin(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["search_path"] = "pg_catalog, pg_temp"
	return pgx.ConnectConfig(ctx, cfg)
}

// databaseURL is dsn naming the database name in place of its own: the URL form's path, or a
// keyword form's dbname, whichever dsn is written in. It refuses a dsn pgx cannot read, and a
// rewrite pgx reads another server or user from than dsn, so the replay's database is made on the
// server the caller named and on no other. It pins the search path for both template creation
// and replay execution rather than inheriting the caller's schema order.
func databaseURL(dsn, name string) (string, error) {
	given, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("replay: read the Postgres DSN: %w", err)
	}
	var rewritten string
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("replay: read the Postgres DSN: %w", err)
		}
		parsed.Path, parsed.RawPath = "/"+name, ""
		query := parsed.Query()
		query.Set("search_path", replaySearchPath)
		parsed.RawQuery = query.Encode()
		rewritten = parsed.String()
	} else {
		rewritten = dsn + " dbname=" + name + " search_path='" + replaySearchPath + "'"
	}
	read, err := pgconn.ParseConfig(rewritten)
	if err != nil {
		return "", fmt.Errorf("replay: name the replay's database on the Postgres DSN: %w", err)
	}
	if read.Host != given.Host || read.Port != given.Port || read.User != given.User || read.Database != name {
		return "", errors.New("replay: naming the replay's database on the Postgres DSN would reach another server or user than the DSN names")
	}
	return rewritten, nil
}

// redacted is dsn as an error names it: its server and database, never its password.
func redacted(dsn string) string {
	config, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "the Postgres DSN"
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s", config.User, config.Host, config.Port, config.Database)
}
