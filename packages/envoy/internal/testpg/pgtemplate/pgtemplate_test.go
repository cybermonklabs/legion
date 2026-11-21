package pgtemplate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// dispatch is the harness these tests run, made as internal/dispatch/store/storetest makes its
// own: its template is the one every Dispatch test process on the server shares, built through
// the same store.MigrateTemplate, so these tests build nothing a Dispatch run would not build,
// and leave nothing it would not leave.
var dispatch = New(Dispatch, store.MigrationsFingerprint, store.MigrateTemplate)

func TestMain(m *testing.M) { os.Exit(dispatch.Main(m)) }

// serverURL is the Postgres server the Dispatch tests run against, skipping t when none is named.
func serverURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the template harness's tests")
	}
	return raw
}

// connect opens a connection to databaseURL, closed when t ends: before the database's drop,
// which t.Cleanup's reverse order gives a database cloned before the connection opened.
func connect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to a test database: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A clone copies the template's files (the package comment says why). The test reads the
// write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18), so
// what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	t.Parallel()
	template := dispatch.templateName(t, serverURL(t)) // finds or builds the template
	ctx := context.Background()
	conn, err := dispatch.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", template).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	name := randomName("dispatch_test_")
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, cloneStatement(name, template)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	dispatch.drop(name)
	written := backendWALBytes(t, conn) - before

	if written > templateBytes/100 {
		t.Fatalf("one clone wrote %d bytes of write-ahead log, more than a hundredth of the %d-byte template: it went through the log", written, templateBytes)
	}
	t.Logf("one clone wrote %d bytes of write-ahead log; the template holds %d bytes", written, templateBytes)
}

// Every test is handed a database of its own, carrying the migrations and nothing another test
// wrote, and that database leaves the server when the test ends. Broken, a test reads another
// test's rows, or a run leaves its databases behind on a server the next run has to share.
func TestEachTestIsHandedADatabaseOfItsOwnWhichIsDroppedAfterwards(t *testing.T) {
	// Serial: it waits on the harness's shared drops WaitGroup, which a parallel test's drop
	// would add to while this one waited.
	ctx := context.Background()
	raw := serverURL(t)
	first := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
	second := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
	var firstName, secondName string
	if err := first.QueryRow(ctx, "select current_database()").Scan(&firstName); err != nil {
		t.Fatalf("read the first database's name: %v", err)
	}
	if err := second.QueryRow(ctx, "select current_database()").Scan(&secondName); err != nil {
		t.Fatalf("read the second database's name: %v", err)
	}
	if firstName == secondName {
		t.Fatalf("two tests were handed the same database %s", firstName)
	}

	// projects.default_assignee is not null, so the row this writes names one.
	if _, err := first.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TEST', 'Test', 'alice@d.example')`); err != nil {
		t.Fatalf("write a row in the first database: %v", err)
	}
	var elsewhere int
	if err := second.QueryRow(ctx, `select count(*) from projects where key = 'TEST'`).Scan(&elsewhere); err != nil {
		t.Fatalf("read the second database: %v", err)
	}
	if elsewhere != 0 {
		t.Fatalf("what one test wrote is in another test's database: it holds %d such row(s)", elsewhere)
	}

	// The drop runs in the background when the inner test ends, and Main waits for it; what this
	// reads is that the cleanup is registered at all, on a database of a subtest of its own.
	var dropped string
	t.Run("a database of its own", func(t *testing.T) {
		inner := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
		if err := inner.QueryRow(ctx, "select current_database()").Scan(&dropped); err != nil {
			t.Fatalf("read the inner database's name: %v", err)
		}
	})
	conn, err := dispatch.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	dispatch.drops.Wait()
	var held int
	if err := conn.QueryRow(ctx, "select count(*) from pg_database where datname = $1", dropped).Scan(&held); err != nil {
		t.Fatalf("read whether the server still holds %s: %v", dropped, err)
	}
	if held != 0 {
		t.Fatalf("%s is still on the server after the test that took it ended", dropped)
	}
}

// A test may drop its own database before the harness does: internal/dispatch/store's
// dropRoleAfterTestDatabase drops the test's database and then a role the test made, in a cleanup
// registered after the clone, which t.Cleanup runs before the harness's drop. That drop then finds
// no database, which is the end the harness's drop is for, so Main reports nothing. Broken, Main
// fails a package whose every test passed, naming a database nobody left behind.
func TestADatabaseItsTestDroppedFirstIsADropThatSucceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	// A harness of its own, so its Main waits for this test's drop alone and the package's
	// harness, whose Main decides the run, never sees it.
	harness := New(Dispatch, store.MigrationsFingerprint, store.MigrateTemplate)
	var name string
	code := harness.MainWith(nil, func(*testing.M) int {
		t.Run("a test that drops its own database", func(t *testing.T) {
			databaseURL := harness.Unmigrated(t, raw, "dispatch_unmigrated_test_")
			// Registered after the clone, as dropRoleAfterTestDatabase is, so it runs before the
			// harness's drop; forced, as that one is, because the connection below closes first
			// and Postgres may not have torn its backend down yet.
			t.Cleanup(func() {
				if _, err := harness.admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
					t.Errorf("drop the test's own database %s: %v", name, err)
				}
			})
			if err := connect(t, databaseURL).QueryRow(ctx, "select current_database()").Scan(&name); err != nil {
				t.Fatalf("read the test database's name: %v", err)
			}
		})
		return 0
	})
	if code != 0 {
		t.Errorf("Main failed the run after a test dropped its own database %s: %q", name, harness.dropFails)
	}
}

// The template is the migration set's, whole, and left on the server: that is what lets the next
// test process clone it instead of applying every migration again, which is most of the Postgres
// work a test run does. Broken, either every process migrates a template of its own again, or -
// worse - a process clones a database that is not this build's schema.
func TestTheTemplateIsTheMigrationSetsAndStaysForTheNextProcess(t *testing.T) {
	// Serial: it takes the server-wide template lock, which every Dispatch test process waiting
	// on its first clone queues behind.
	ctx := context.Background()
	clone := connect(t, dispatch.Clone(t, serverURL(t), "dispatch_test_")) // finds or builds the template

	fingerprint, err := store.MigrationsFingerprint()
	if err != nil {
		t.Fatalf("fingerprint the migration set: %v", err)
	}
	if want := Dispatch.Prefix + fingerprint; dispatch.name != want {
		t.Fatalf("the template is %s, want %s: a process whose set differs would clone this one", dispatch.name, want)
	}

	// The clone carries the whole set, which is what a second process is promised by finding the
	// template already named.
	migrations, err := pgmigrate.Load(os.DirFS("../../dispatch/store"), "migrations")
	if err != nil {
		t.Fatalf("load the migration set: %v", err)
	}
	var recorded int
	if err := clone.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("count the clone's recorded migrations: %v", err)
	}
	if recorded != len(migrations) {
		t.Errorf("the clone records %d migrations, want the set's %d", recorded, len(migrations))
	}

	// What a process arriving second does: it takes the lock, finds the template already there,
	// and builds nothing. It comes last, and nothing of this test runs under the lock but the
	// two reads below, because the lock is the server's.
	unlock, err := dispatch.lock(ctx)
	if err != nil {
		t.Fatalf("take the template lock a second time: %v", err)
	}
	defer unlock()
	built, err := dispatch.exists(ctx, dispatch.name)
	if err != nil {
		t.Fatal(err)
	}
	if !built {
		t.Fatalf("%s is not on the server, so the next process would migrate one of its own", dispatch.name)
	}
	var halfBuilt int
	if err := dispatch.admin.QueryRow(ctx, "select count(*) from pg_database where starts_with(datname, $1)", dispatch.buildingPrefix()).Scan(&halfBuilt); err != nil {
		t.Fatalf("count the half-built templates: %v", err)
	}
	if halfBuilt != 0 {
		t.Errorf("%d database(s) named %s* are still on the server: a build left its scratch behind", halfBuilt, dispatch.buildingPrefix())
	}
}

// backendWALBytes is the write-ahead log conn's backend has written. pg_stat_get_backend_wal
// reads the counters the backend last flushed, and the forced flush runs as its own statement ends.
func backendWALBytes(t *testing.T, conn *pgxpool.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "select pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("force the backend's statistics flush: %v", err)
	}
	var written int64
	if err := conn.QueryRow(ctx, "select wal_bytes::bigint from pg_stat_get_backend_wal(pg_backend_pid())").Scan(&written); err != nil {
		t.Fatalf("read the backend's write-ahead log: %v", err)
	}
	return written
}

// A server that outlives a run (dev-postgres.sh's, a pod's test Postgres) keeps the template an
// earlier build left, under the name that build's fingerprint gave it. A build whose runner
// recorded each version alone, as every runner before pgmigrate's identity check did, left a
// schema_migrations with no name, checksum or applied_checksum, and its fingerprint digested the
// set's versions, names and texts alone (earlierFingerprint), the same for a set this build
// embeds unchanged. A clone of that template hands this build's tests a version table this
// runner never leaves. This test leaves such a template under the name the earlier fingerprint
// gives this build's set, with a prefix and lock key of its own so that no other process clones
// it, and reads what the harness then clones: a version table whose every row records what this
// runner records of its file (pgmigrate.Migration.RecordedIdentity, and the file's own checksum
// as the text applied). Broken, the fingerprint does not cover what the runner records, the
// harness finds the earlier template under its own name, and the clone records no identity.
func TestATemplateAnEarlierRunnerLeftForTheSameSetIsNotCloned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	migrations, err := pgmigrate.Load(os.DirFS("../../dispatch/store"), "migrations")
	if err != nil {
		t.Fatalf("load the migration set: %v", err)
	}
	// A prefix of its own, short enough that it and a fingerprint's 32 hex digits fit in the 63
	// bytes a Postgres name holds, and a lock key no other harness takes.
	template := Template{Prefix: "pgt_stale_" + randomName("")[:8] + "_", LockKey: 8150102}
	harness := New(template, store.MigrationsFingerprint, store.MigrateTemplate)
	code := harness.MainWith(nil, func(*testing.M) int {
		t.Run("a clone on a server that holds an earlier runner's template", func(t *testing.T) {
			harness.open(t, raw)
			earlier := template.Prefix + earlierFingerprint(migrations)
			// Both templates are this test's own, under a prefix no other process names, and
			// neither is a test's clone, so this test drops them itself.
			t.Cleanup(func() {
				fingerprint, _ := store.MigrationsFingerprint()
				for _, name := range []string{earlier, template.Prefix + fingerprint} {
					if _, err := harness.admin.Exec(context.Background(), "drop database if exists "+name); err != nil {
						t.Errorf("drop the template %s: %v", name, err)
					}
				}
			})
			leaveAnEarlierRunnersTemplate(t, harness, earlier)

			clone := connect(t, harness.Clone(t, raw, "dispatch_test_"))
			if harness.name == earlier {
				t.Errorf("the harness clones %s, the template an earlier runner left for this set", earlier)
			}
			check, err := pgmigrate.CheckVersions(ctx, clone, migrations, "schema_migrations")
			if err != nil {
				t.Fatalf("read the clone's schema_migrations: %v", err)
			}
			var report strings.Builder
			check.Write(&report)
			if check.Matching != len(migrations) || len(check.Backfills) != 0 || !check.OK() {
				t.Errorf("the clone's schema_migrations records what this runner records of %d of the set's %d files, and no name for %d; want every file's:\n%s",
					check.Matching, len(migrations), len(check.Backfills), report.String())
			}
			var columns []string
			if err := clone.QueryRow(ctx, `select coalesce(array_agg(column_name::text order by column_name), '{}') from information_schema.columns
				where table_schema = current_schema() and table_name = 'schema_migrations'`).Scan(&columns); err != nil {
				t.Fatalf("read the clone's version table columns: %v", err)
			}
			for _, column := range pgmigrate.IdentityColumnNames {
				if !slices.Contains(columns, column) {
					t.Errorf("the clone's schema_migrations has the columns %v, without %s", columns, column)
				}
			}
		})
		return 0
	})
	if code != 0 {
		t.Errorf("Main failed the run: %q", harness.dropFails)
	}
}

// CloneOf makes a template of a key's own once, for the first test that asks for the key, and
// hands every test that asks a copy of its own; the template is the process's alone, so Main
// drops it once the tests and their drops have ended. This is what the two-build suite's databases
// come from (storetest.OpenFrom). Broken, either each test migrates the key's template again, or a
// run leaves its templates behind on a server the next run shares.
func TestATemplateOfAKeysOwnIsMadeOnceAndDroppedByMain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	// A harness of its own, so that its Main runs here, as the package's would at its end.
	harness := New(Dispatch, store.MigrationsFingerprint, store.MigrateTemplate)
	migrated := 0
	migrate := func(url string) error {
		migrated++
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, "create table made_by_the_key (id integer)")
		return err
	}
	var made []string
	code := harness.MainWith(nil, func(*testing.M) int {
		var names []string
		for test := range 2 {
			t.Run(fmt.Sprintf("test %d of the key", test+1), func(t *testing.T) {
				conn := connect(t, harness.CloneOf(t, raw, "a key", "dispatch_test_", migrate))
				var name string
				var holds bool
				if err := conn.QueryRow(ctx, "select current_database(), to_regclass('made_by_the_key') is not null").Scan(&name, &holds); err != nil {
					t.Fatalf("read the test's database: %v", err)
				}
				if !holds {
					t.Errorf("the test's database %s does not hold what the key's migrate made", name)
				}
				names = append(names, name)
			})
		}
		if len(names) == 2 && names[0] == names[1] {
			t.Errorf("two tests of the key were handed the same database %s", names[0])
		}
		made = harness.keyedNames()
		return 0
	})
	if code != 0 {
		t.Errorf("Main failed the run: %q", harness.dropFails)
	}
	if migrated != 1 {
		t.Errorf("the key's template was migrated %d times for two tests, want once", migrated)
	}
	if len(made) != 1 || !strings.HasPrefix(made[0], Dispatch.Prefix+"keyed_") {
		t.Fatalf("the harness made the templates %v for one key, want one named %skeyed_…", made, Dispatch.Prefix)
	}
	dispatch.open(t, raw)
	held, err := dispatch.exists(ctx, made[0])
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Errorf("%s, the key's template, is still on the server after Main returned", made[0])
	}
}

// earlierFingerprint is pgmigrate.Fingerprint as every build before the runners recorded
// identities computed it: a digest of each migration's version, name and text alone.
func earlierFingerprint(migrations []pgmigrate.Migration) string {
	digest := sha256.New()
	for _, migration := range migrations {
		fmt.Fprintf(digest, "%d\x00%d\x00%s\x00%d\x00%s\x00", migration.Version, len(migration.Name), migration.Name, len(migration.SQL), migration.SQL)
	}
	return hex.EncodeToString(digest.Sum(nil)[:16])
}

// leaveAnEarlierRunnersTemplate makes name the template a runner from before the identity check
// left: every migration applied, each recorded by its version alone, closed to connections.
func leaveAnEarlierRunnersTemplate(t *testing.T, h *Harness, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create the earlier runner's template: %v", err)
	}
	if err := store.MigrateTemplate(ctx, h.url(name)); err != nil {
		t.Fatalf("migrate the earlier runner's template: %v", err)
	}
	conn, err := pgx.Connect(ctx, h.url(name))
	if err != nil {
		t.Fatalf("connect to the earlier runner's template: %v", err)
	}
	drops := make([]string, len(pgmigrate.IdentityColumnNames))
	for i, column := range pgmigrate.IdentityColumnNames {
		drops[i] = "drop column " + column
	}
	_, err = conn.Exec(ctx, "alter table schema_migrations "+strings.Join(drops, ", "))
	conn.Close(ctx)
	if err != nil {
		t.Fatalf("take the identity columns off the earlier runner's schema_migrations: %v", err)
	}
	if _, err := h.admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		t.Fatalf("close the earlier runner's template to connections: %v", err)
	}
}
