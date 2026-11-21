package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pullRequestTextsVersion is the migration that stores the texts a pull request's issue is read
// from (pull_request_texts), with the guard on the database that reading needs.
const pullRequestTextsVersion = 20261117111040

func migratedThrough(t *testing.T, version int) *Store {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.migrate(context.Background(), migrationsThrough(t, version)); err != nil {
		t.Fatalf("migrate through %d: %v", version, err)
	}
	return store
}

func storeHeadAndTexts(t *testing.T, store *Store, number int) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, state) values ('acme/widgets', $1, 'aaaa', 'legion/ACME-1', 'merged')
	`, number); err != nil {
		t.Fatalf("store #%d: %v", number, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body)
		values ('acme/widgets', $1, 'aaaa', 'legion/ACME-1', 'acme/widgets', 'main', 'A change', 'No key here.')
	`, number); err != nil {
		t.Fatalf("store the texts of #%d: %v", number, err)
	}
}

func textsRows(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select repo || '#' || number from pull_request_texts order by 1`)
	if err != nil {
		t.Fatalf("read pull_request_texts: %v", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read pull_request_texts: %v", err)
	}
	return keys
}

// A row of texts describes one results row: when the results row goes, its texts go with it.
func TestTheTextsGoWithTheirResultsRow(t *testing.T) {
	store := migratedThrough(t, pullRequestTextsVersion)
	storeHeadAndTexts(t, store, 1)
	storeHeadAndTexts(t, store, 2)
	if _, err := store.Pool.Exec(context.Background(), `delete from pull_request_heads where repo = 'acme/widgets' and number = 1`); err != nil {
		t.Fatalf("delete #1: %v", err)
	}
	if got := textsRows(t, store); len(got) != 1 || got[0] != "acme/widgets#2" {
		t.Errorf("texts after the results row of #1 is deleted = %v, want [acme/widgets#2]", got)
	}
}

// A results row that moves to another repository name takes its texts with it.
func TestARenameMovesTheTexts(t *testing.T) {
	store := migratedThrough(t, pullRequestTextsVersion)
	storeHeadAndTexts(t, store, 1)
	if _, err := store.Pool.Exec(context.Background(), `update pull_request_heads set repo = 'acme/gizmos' where repo = 'acme/widgets' and number = 1`); err != nil {
		t.Fatalf("rename #1's repository: %v", err)
	}
	if got := textsRows(t, store); len(got) != 1 || got[0] != "acme/gizmos#1" {
		t.Errorf("texts after the rename = %v, want [acme/gizmos#1]", got)
	}
}

// A stamp of a refused facts read on a timeline row is no change a page shows, so it logs no
// row of delivery_changes. Control in the same test: a new title logs one.
func TestARefusalStampLogsNoDeliveryChange(t *testing.T) {
	ctx := context.Background()
	store := migratedThrough(t, pullRequestTextsVersion)
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author)
		values ('acme/widgets', 1, 'A change', 'https://github.com/acme/widgets/pull/1', 'someone')
	`); err != nil {
		t.Fatalf("store #1's timeline row: %v", err)
	}
	changes := func() int {
		t.Helper()
		var n int
		if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes`).Scan(&n); err != nil {
			t.Fatalf("count delivery_changes: %v", err)
		}
		return n
	}
	before := changes()
	if _, err := store.Pool.Exec(ctx, `update delivery_pull_requests set facts_refused_at = clock_timestamp() where repo = 'acme/widgets' and number = 1`); err != nil {
		t.Fatalf("stamp #1's refusal: %v", err)
	}
	if got := changes(); got != before {
		t.Errorf("a refusal stamp logged %d rows of delivery_changes, want none", got-before)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_pull_requests set title = 'A new title' where repo = 'acme/widgets' and number = 1`); err != nil {
		t.Fatalf("retitle #1: %v", err)
	}
	if got := changes(); got != before+1 {
		t.Errorf("a new title logged %d rows of delivery_changes, want 1 (the control)", got-before)
	}
}

// randomLetters answers n letters from crypto/rand: a text that does not compress, so an index
// entry of it is as long as the text (a URL of one repeated letter compresses and fits).
func randomLetters(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random letters: %v", err)
	}
	for i, b := range raw {
		raw[i] = 'a' + b%26
	}
	return string(raw)
}

// A release before this one stored the URL of an envelope with no check, so a results row can
// hold a URL no btree entry holds (2,704 bytes). The index on lower(url) is partial, over URLs
// of at most 1,024 bytes, so its build cannot fail over such a row: the census answers 0, the
// migration applies, and the index holds every row but that one. lower() makes a text at most
// 1.5 times as long in bytes, and Ⱥ is one of the letters that grows (2 bytes to 3), so a URL of
// 1,024 bytes of it is the longest entry the index takes. Control in the same test: a full index
// on lower(url) over the same rows fails with SQLSTATE 54000.
func TestTheMigrationAppliesOverAURLTheIndexCannotHold(t *testing.T) {
	ctx := context.Background()
	store := migratedThrough(t, pullRequestTextsVersion-1)
	long := "https://github.com/acme/widgets/pull/" + randomLetters(t, 4000)
	folds := strings.Repeat("Ⱥ", 512)
	if len(folds) != 1024 {
		t.Fatalf("the URL of #6 holds %d bytes, want 1024", len(folds))
	}
	for number, url := range map[int]string{5: long, 6: folds, 7: "https://github.com/acme/widgets/pull/7"} {
		if _, err := store.Pool.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, state, url) values ('acme/widgets', $1, 'aaaa', 'merged', $2)`, number, url); err != nil {
			t.Fatalf("store #%d: %v", number, err)
		}
	}

	control, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Exec(ctx, `create index control_full on pull_request_heads (lower(url))`)
	if rollback := control.Rollback(ctx); rollback != nil {
		t.Fatalf("roll the control back: %v", rollback)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "54000" {
		t.Fatalf("a full index on lower(url) over a URL of 4,000 random letters answered %v, want SQLSTATE 54000 (the control)", err)
	}

	if got := censusCount(t, store, pullRequestTextsVersion); got != 0 {
		t.Fatalf("census over a stored URL of 4,000 bytes = %d, want 0: the guard counts the database, no row", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, pullRequestTextsVersion)); err != nil {
		t.Fatalf("the migration over a stored URL of 4,000 bytes: %v", err)
	}
	var entries float64
	var short int
	if err := store.Pool.QueryRow(ctx, `
		select (select reltuples from pg_class where oid = 'pull_request_heads_url_lower'::regclass),
			(select count(*) from pull_request_heads where octet_length(url) <= 1024)
	`).Scan(&entries, &short); err != nil {
		t.Fatalf("read the index's entries: %v", err)
	}
	if entries != float64(short) || short != 2 {
		t.Errorf("pull_request_heads_url_lower holds %v entries, want %d: the rows with a URL of at most 1,024 bytes", entries, short)
	}

	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off; set local enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	const everyEntry = `select number from pull_request_heads where octet_length(url) <= 1024 and lower(url) >= '' order by number`
	rows, err := tx.Query(ctx, "explain (costs off) "+everyEntry)
	if err != nil {
		t.Fatalf("explain the read of every entry: %v", err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "pull_request_heads_url_lower") {
		t.Fatalf("the read of every entry does not read pull_request_heads_url_lower:\n%s", strings.Join(plan, "\n"))
	}
	rows, err = tx.Query(ctx, everyEntry)
	if err != nil {
		t.Fatalf("read every entry: %v", err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(numbers) != "[6 7]" {
		t.Errorf("pull_request_heads_url_lower holds %v, want [6 7]: #6's URL of 1,024 bytes and not #5's of 4,000", numbers)
	}
}

// localeDatabase creates a database of the test's own on the test server from template0 with
// options (an encoding and a locale), drops it when the test ends, and opens a store on it. A
// server that cannot make the database fails the test: a locale test never skips.
func localeDatabase(t *testing.T, options string) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	server, err := url.Parse(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database URL: %v", errors.Unwrap(err))
	}
	server.Path = "/postgres"
	admin, err := pgx.Connect(ctx, server.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer admin.Close(ctx)
	name := "dispatch_locale_" + randomDatabaseSuffix(t)
	if _, err := admin.Exec(ctx, "create database "+name+" template template0 strategy file_copy "+options); err != nil {
		t.Fatalf("make a database of %s: %v", options, err)
	}
	t.Cleanup(func() {
		drop, err := pgx.Connect(context.Background(), server.String())
		if err != nil {
			t.Errorf("connect to drop %s: %v", name, err)
			return
		}
		defer drop.Close(context.Background())
		if _, err := drop.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	database := *server
	database.Path = "/" + name
	store, err := Open(ctx, database.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(store.Pool.Close)
	return store, name
}

// The reading of keys runs under the collation "C", and the readers by issue compare names with
// lower() under the database's own collation. The migration refuses a database where the two
// would differ, and one whose encoding is not UTF8, with a line that names the cause, and its
// census answers 1 there, so the pre-deploy census stops the deploy before a pod is replaced.
// Production's database is UTF8 with the libc collation en_US.UTF8. Control in the same test:
// lower('I') is 'i' in each admitted database and is not in the tr-TR one, so the condition
// the guard reads differs where it must.
func TestTheMigrationRefusesADatabaseItCannotRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, options string
		foldsI        bool   // whether lower('I') is 'i' in the database
		refusal       string // a part of the guard's line, "" where the database is admitted
	}{
		{"libc C", `encoding 'UTF8' locale 'C'`, true, ""},
		{"libc en_US.utf8", `encoding 'UTF8' locale 'en_US.utf8'`, true, ""},
		{"libc en_US.UTF8, production's", `encoding 'UTF8' lc_collate 'en_US.UTF8' lc_ctype 'en_US.UTF8'`, true, ""},
		{"ICU und", `encoding 'UTF8' locale_provider icu icu_locale 'und' locale 'en_US.utf8'`, true, ""},
		{"ICU tr-TR", `encoding 'UTF8' locale_provider icu icu_locale 'tr-TR' locale 'en_US.utf8'`, false, "tr-TR"},
		{"SQL_ASCII", `encoding 'SQL_ASCII' locale 'C'`, true, "has encoding SQL_ASCII"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, name := localeDatabase(t, tc.options)
			if err := store.migrate(ctx, migrationsThrough(t, pullRequestTextsVersion-1)); err != nil {
				t.Fatalf("migrate through %d: %v", pullRequestTextsVersion-1, err)
			}
			var folds bool
			if err := store.Pool.QueryRow(ctx, `select lower('I') = 'i'`).Scan(&folds); err != nil {
				t.Fatal(err)
			}
			if folds != tc.foldsI {
				t.Errorf("lower('I') = 'i' is %v in this database, want %v (the control)", folds, tc.foldsI)
			}

			census := censusCount(t, store, pullRequestTextsVersion)
			err := store.migrate(ctx, migrationsThrough(t, pullRequestTextsVersion))
			if tc.refusal == "" {
				if census != 0 {
					t.Errorf("census = %d, want 0", census)
				}
				if err != nil {
					t.Fatalf("the migration refused an admitted database: %v", err)
				}
				var refusal *string
				if err := store.Pool.QueryRow(ctx, `select attribution_database_refusal()`).Scan(&refusal); err != nil {
					t.Fatal(err)
				}
				if refusal != nil {
					t.Errorf("attribution_database_refusal() = %q, want null", *refusal)
				}
				return
			}
			if census != 1 {
				t.Errorf("census = %d, want 1", census)
			}
			if err == nil {
				t.Fatal("the migration applied")
			}
			line := err.Error()
			t.Logf("census %d; the migration refused: %s", census, line)
			if !strings.Contains(line, "dispatch: database "+name) || !strings.Contains(line, tc.refusal) {
				t.Errorf("the refusal %q does not name the database %s and %q", line, name, tc.refusal)
			}
			var version int
			var texts, guard bool
			if err := store.Pool.QueryRow(ctx, `
				select coalesce(max(version), 0), to_regclass('pull_request_texts') is not null,
					to_regproc('attribution_database_refusal') is not null
				from schema_migrations
			`).Scan(&version, &texts, &guard); err != nil {
				t.Fatal(err)
			}
			if version >= pullRequestTextsVersion || texts || guard {
				t.Errorf("after the refusal: version %d, texts table %v, guard function %v; want an earlier version and neither", version, texts, guard)
			}
		})
	}
}
