package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// localeDatabase creates a database of the test's own from template0 with options (an encoding
// and a locale) on the server DISPATCH_TEST_DATABASE_URL names, applies every migration, and
// drops it when the test ends. A server that cannot make the database fails the test: a locale
// test never skips for a locale the server lacks. Without the variable it skips, as every test
// of the package with a database does (storetest.Open).
func localeDatabase(t *testing.T, options string) *store.Store {
	t.Helper()
	raw := os.Getenv(storetest.DatabaseURLEnv)
	if raw == "" {
		t.Skip(storetest.DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, errors.Unwrap(err))
	}
	server.Path = "/postgres"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, server.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer admin.Close(ctx)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "dispatch_locale_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name+" template template0 "+options); err != nil {
		t.Fatalf("create database with %s: %v", options, err)
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
	opened, err := store.Open(ctx, database.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(opened.Pool.Close)
	if err := opened.Migrate(ctx); err != nil {
		t.Fatalf("migrate %s (%s): %v", name, options, err)
	}
	return opened
}

// localeTexts are three texts whose owner and repository names hold İ, ı and I, which lower()
// folds by locale and the reading folds in ASCII alone.
var localeTexts = []readingCase{
	{Name: "an owner with a dotted capital I", readingText: readingText{Repo: "İstanbul/x", Body: "Fixes #12 and https://github.com/Istanbul/X/issues/13."}},
	{Name: "an owner with a dotless i", readingText: readingText{Repo: "ıdea/Lab", HeadRef: "ıtem-7-ıdea", Body: "See #34, https://github.com/Idea/LAB/issues/5."}},
	{Name: "a repository with both", readingText: readingText{Repo: "Acme/İIı", Title: "ACME-9 for Istanbul", Body: "See #56 and https://GITHUB.COM/ACME/Iİ/issues/6."}},
}

// localeAnswer is what one database answers: the reading's identity, its lists for each text, and
// the control's cited list for a repository whose name lower() folds by locale.
type localeAnswer struct {
	identity string
	lists    []readingLists
	control  []string
}

// The reading runs under the collation "C" and folds a cited URL in ASCII, so each database the
// guard admits gives byte-equal keys and one identity for the corpus and for names with İ, ı and
// I. Control in the same test: a copy of the reading that folds the cited list with lower() under
// the database's collation gives the repository İstanbul/x another URL in C than in en_US.utf8.
// The copy names that collation ("default"): a cited URL holds the parts of a match of a pattern
// whose input is under "C", so it takes "C" too, and a bare lower() of it would fold as "C" does.
func TestTheReadingAndTheStatementAnswerAlikeInEveryLocale(t *testing.T) {
	cases := append(readingCorpus(t), localeTexts...)
	databases := []struct{ name, options string }{
		{"libc C", `encoding 'UTF8' locale 'C'`},
		{"libc en_US.utf8", `encoding 'UTF8' locale 'en_US.utf8'`},
		{"libc en_US.UTF8, production's", `encoding 'UTF8' lc_collate 'en_US.UTF8' lc_ctype 'en_US.UTF8'`},
		{"ICU und", `encoding 'UTF8' locale_provider icu icu_locale 'und' locale 'en_US.utf8'`},
	}
	answers := make([]localeAnswer, len(databases))
	t.Run("databases", func(t *testing.T) {
		for i, database := range databases {
			t.Run(database.name, func(t *testing.T) {
				t.Parallel()
				opened := localeDatabase(t, database.options)
				ctx := t.Context()
				schema := readingSchema(t, ctx, opened.Pool)
				answer := localeAnswer{identity: currentReading(t, ctx, opened.Pool, schema)}
				for _, c := range cases {
					answer.lists = append(answer.lists, readingOf(t, ctx, opened.Pool, c.readingText))
				}
				createReadingCopy(t, ctx, opened.Pool, "attribution_keys_lower",
					readingEdit{"pg_catalog.translate((case when m[4] is null", "pg_catalog.lower((case when m[4] is null", 1},
					readingEdit{`end) collate "C", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') u`, `end) collate "default") u`, 1})
				if err := opened.Pool.QueryRow(ctx, `select k.cited_issues from attribution_keys_lower('İstanbul/x', '', '', 'Fixes #12.', null) k`).Scan(&answer.control); err != nil {
					t.Fatalf("the control's reading: %v", err)
				}
				answers[i] = answer
			})
		}
	})
	if t.Failed() {
		return
	}
	first := answers[0]
	for i, answer := range answers[1:] {
		database := databases[i+1].name
		if answer.identity != first.identity {
			t.Errorf("%s: the identity is %s, and %s in %s", database, answer.identity, first.identity, databases[0].name)
		}
		for j, c := range cases {
			if differ := first.lists[j].differences(answer.lists[j]); len(differ) > 0 {
				t.Errorf("%s: %s (%.80q) differs from %s: %s", database, c.Name, c.Body, databases[0].name, strings.Join(differ, "; "))
			}
		}
	}
	for j, c := range cases[:len(cases)-len(localeTexts)] {
		if differ := c.readingLists.differences(first.lists[j]); len(differ) > 0 {
			t.Errorf("%s: %s (%.80q) differs from the corpus: %s", databases[0].name, c.Name, c.Body, strings.Join(differ, "; "))
		}
	}
	for j, c := range localeTexts {
		t.Logf("%s: %+v", c.Name, first.lists[len(cases)-len(localeTexts)+j])
	}
	inC, inUS := strings.Join(answers[0].control, " "), strings.Join(answers[1].control, " ")
	t.Logf("the control's cited list for İstanbul/x: %q in %s, %q in %s", inC, databases[0].name, inUS, databases[1].name)
	if inC == inUS || !strings.Contains(inC, "İstanbul") {
		t.Errorf("the control: lower() under the database's collation gave %q in %s and %q in %s, want two answers, İstanbul kept in C", inC, databases[0].name, inUS, databases[1].name)
	}
}
