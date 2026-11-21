package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// emptyLocaleDatabase creates a database of the test's own from template0 with options (an
// encoding and a locale) on the server DISPATCH_TEST_DATABASE_URL names, as a logical restore
// makes one, and drops it when the test ends. A server that cannot make the database fails the
// test: a locale test never skips for a locale the server lacks. Without the variable it skips,
// as every test of the package with a database does (storetest.Open).
func emptyLocaleDatabase(t *testing.T, options string) *store.Store {
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
	opened, err := store.Open(ctx, database.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(opened.Pool.Close)
	return opened
}

// localeDatabase is emptyLocaleDatabase with every migration applied.
func localeDatabase(t *testing.T, options string) *store.Store {
	t.Helper()
	opened := emptyLocaleDatabase(t, options)
	if err := opened.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate a database of %s: %v", options, err)
	}
	return opened
}

// admittedLocales are the databases the guard of the texts migration admits: the three the plan
// names, and production's spelling of its collation.
var admittedLocales = []struct{ name, options string }{
	{"libc C", `encoding 'UTF8' locale 'C'`},
	{"libc en_US.utf8", `encoding 'UTF8' locale 'en_US.utf8'`},
	{"libc en_US.UTF8, production's", `encoding 'UTF8' lc_collate 'en_US.UTF8' lc_ctype 'en_US.UTF8'`},
	{"ICU und", `encoding 'UTF8' locale_provider icu icu_locale 'und' locale 'en_US.utf8'`},
}

// The statement compares repository names and links with lower() under the database's own
// collation, as the other readers do, and each admitted database folds an ASCII letter as "C"
// does: a link stored as Istanbul/WIDGETS names its issue for istanbul/widgets#9 in each. Control
// in the same test: the same link of #10 names nobody for #9.
func TestTheStatementMatchesRepositoriesInEveryLocale(t *testing.T) {
	for _, database := range admittedLocales {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			pool := localeDatabase(t, database.options).Pool
			ctx := t.Context()
			if err := PrepareAttribution(ctx, pool); err != nil {
				t.Fatal(err)
			}
			seedProjectRepository(t, ctx, pool, "IST", "Istanbul", "istanbul/widgets")
			for _, issue := range []string{"IST-9", "IST-10"} {
				if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, created_by, rank)
					values ($1, 'IST', $2::int, $1, 'todo', '{"kind":"system","id":"test"}', $1)`, issue, strings.TrimPrefix(issue, "IST-")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values
				('IST-9', 'https://github.com/Istanbul/WIDGETS/pull/9', 'github'), ('IST-10', 'https://github.com/Istanbul/WIDGETS/pull/10', 'github')`); err != nil {
				t.Fatal(err)
			}
			storeStatementRow(t, ctx, pool, statementRow{repo: "istanbul/widgets", number: 9, branch: "main", title: "no key", texts: readTexts("")})
			wantAnswer(t, ctx, pool, "istanbul/widgets", 9, "IST-9 from external_link")
			if _, err := pool.Exec(ctx, `delete from issue_external_links where issue_key = 'IST-9'`); err != nil {
				t.Fatal(err)
			}
			wantAnswer(t, ctx, pool, "istanbul/widgets", 9, "nobody from none")
		})
	}
}

// refusalFunction is the statement of the texts migration that creates
// attribution_database_refusal(), as the migration writes it.
func refusalFunction(t *testing.T) string {
	t.Helper()
	sql, err := os.ReadFile("../store/migrations/20261117111040_pull_request_texts.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	const opening, closing = "create function attribution_database_refusal()", "$guard$;"
	start := strings.Index(string(sql), opening)
	end := strings.Index(string(sql)[max(start, 0):], closing)
	if start < 0 || end < 0 {
		t.Fatalf("the texts migration holds no statement %q … %q", opening, closing)
	}
	return string(sql)[start : start+end+len(closing)]
}

// A logical restore into a database the migration's guard would refuse brings schema_migrations
// with it, so neither the migration nor the census runs there again. Each start asks the guard:
// PrepareAttribution answers its line. Control in the same test: in an en_US.utf8 database it
// answers nil.
func TestPrepareAttributionRefusesADatabaseItCannotRead(t *testing.T) {
	for _, database := range []struct{ name, options, refusal string }{
		{"ICU tr-TR", `encoding 'UTF8' locale_provider icu icu_locale 'tr-TR' locale 'en_US.utf8'`, "tr-TR"},
		{"SQL_ASCII", `encoding 'SQL_ASCII' locale 'C'`, "SQL_ASCII"},
		{"libc en_US.utf8", `encoding 'UTF8' locale 'en_US.utf8'`, ""},
	} {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			pool := emptyLocaleDatabase(t, database.options).Pool
			ctx := t.Context()
			if _, err := pool.Exec(ctx, `create table pull_request_texts (repo text)`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, refusalFunction(t)); err != nil {
				t.Fatal(err)
			}
			err := PrepareAttribution(ctx, pool)
			if database.refusal == "" {
				if err != nil {
					t.Fatalf("PrepareAttribution refused an admitted database: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), database.refusal) {
				t.Fatalf("PrepareAttribution = %v, want a refusal that names %s", err, database.refusal)
			}
			t.Logf("refused: %v", err)
		})
	}
}

// localeTexts are three texts whose owner and repository names hold İ, ı and I, which lower()
// folds by locale and the reading folds in ASCII alone.
var localeTexts = []readingCase{
	{Name: "an owner with a dotted capital I", readingText: readingText{Repo: "İstanbul/x", Body: "Fixes #12 and https://github.com/Istanbul/X/issues/13."}},
	{Name: "an owner with a dotless i", readingText: readingText{Repo: "ıdea/Lab", HeadRef: "ıtem-7-ıdea", Body: "See #34, https://github.com/Idea/LAB/issues/5."}},
	{Name: "a repository with both", readingText: readingText{Repo: "Acme/İIı", Title: "ACME-9 for Istanbul", Body: "See #56 and https://GITHUB.COM/ACME/Iİ/issues/6."}},
}

// localeAnswer is what one database answers: the reading's identity, its lists for each text,
// the statement's answer for each text stored as a pull request, and the control's cited list
// for a repository whose name lower() folds by locale.
type localeAnswer struct {
	identity   string
	lists      []readingLists
	statements []string
	control    []string
}

// storeLocaleCorpus stores each case as acme/widgets#(i+1) under its own repository name, with
// an issue for each key its lists name, of a project whose default repository is acme/widgets,
// and answers the statement for each.
func storeLocaleCorpus(t *testing.T, ctx context.Context, pool *store.Pool, cases []readingCase) []string {
	t.Helper()
	if err := PrepareAttribution(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := []PullRequestKey{}
	for i, c := range cases {
		for _, issue := range slices.Concat(c.TitleKeys, c.BranchKeys, c.CommitKeys) {
			project, number, _ := strings.Cut(issue, "-")
			if n, err := strconv.ParseInt(number, 10, 32); err == nil {
				seedProjectRepository(t, ctx, pool, project, project, "acme/widgets")
				if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, created_by, rank)
					values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}', $1) on conflict do nothing`, issue, project, n); err != nil {
					t.Fatal(err)
				}
			}
		}
		storeStatementRow(t, ctx, pool, statementRow{repo: c.Repo, number: i + 1, branch: c.HeadRef, title: c.Title,
			texts: &statementTexts{body: c.Body, messages: c.Messages}})
		keys = append(keys, PullRequestKey{Repo: c.Repo, Number: i + 1})
	}
	answers, err := PullRequestIssues(ctx, pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, k := range keys {
		lines = append(lines, describeAttribution(answers[k]))
	}
	return lines
}

// The reading runs under the collation "C" and folds a cited URL in ASCII, so each database the
// guard admits gives byte-equal keys, one identity and one answer of the statement for the
// corpus and for names with İ, ı and I. Control in the same test: a copy of the reading that folds
// the cited list with lower() under the database's collation gives the repository İstanbul/x
// another URL in C than in en_US.utf8.
// The copy names that collation ("default"): a cited URL holds the parts of a match of a pattern
// whose input is under "C", so it takes "C" too, and a bare lower() of it would fold as "C" does.
func TestTheReadingAndTheStatementAnswerAlikeInEveryLocale(t *testing.T) {
	cases := append(readingCorpus(t), localeTexts...)
	databases := admittedLocales
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
				answer.statements = storeLocaleCorpus(t, ctx, opened.Pool, cases)
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
			if first.statements[j] != answer.statements[j] {
				t.Errorf("%s: the statement answers %s (%.80q) %q, and %q in %s", database, c.Name, c.Body, answer.statements[j], first.statements[j], databases[0].name)
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

// citedPairs are pull requests that each cite one GitHub issue by a bare #n, which the reading
// spells under the pull request's repository in ASCII lower case, beside the link of an issue to
// that GitHub issue and the answer every database gives. The link of #21 is the cited URL's own
// spelling, İ kept; the link of #22 folds İ to i, which the reading's fold does not; #70 is a
// pair in ASCII alone.
var citedPairs = []struct {
	repo         string
	number       int
	body         string
	issue, link  string
	answer       string
	projectRepos [2]string
}{
	{"İstanbul/x", 21, "Fixes #12.", "IST-12", "https://github.com/İstanbul/x/issues/12", "IST-12 from github_issue_link", [2]string{"IST", "İstanbul/x"}},
	{"İstanbul/x", 22, "Fixes #13.", "IST-13", "https://github.com/istanbul/x/issues/13", "nobody from none", [2]string{"IST", "İstanbul/x"}},
	{"acme/widgets", 70, "Fixes #77.", "ASC-77", "https://github.com/Acme/Widgets/issues/77", "ASC-77 from github_issue_link", [2]string{"ASC", "acme/widgets"}},
}

// linkReaders are what one statement text of each reader answers for citedPairs: each pull
// request's answer by key, and the numbers a read by each issue lists.
type linkReaders struct {
	byKey   map[int]string
	byIssue map[string][]int
}

func readLinkPairs(t *testing.T, ctx context.Context, q Querier, statements *attributionStatements) linkReaders {
	t.Helper()
	readers := linkReaders{byKey: map[int]string{}, byIssue: map[string][]int{}}
	for _, pair := range citedPairs {
		rows, err := q.Query(ctx, statements.pullRequestIssues, []string{pair.repo}, []int{pair.number})
		if err != nil {
			t.Fatalf("the statement by key of %s#%d: %v", pair.repo, pair.number, err)
		}
		for rows.Next() {
			var key PullRequestKey
			var answer PullRequestAttribution
			if err := rows.Scan(&key.Repo, &key.Number, &answer.IssueKey, &answer.Source, &answer.HasFacts, &answer.Known); err != nil {
				t.Fatal(err)
			}
			readers.byKey[pair.number] = describeAttribution(answer)
		}
		rows.Close()
		listed := []int{}
		rows, err = q.Query(ctx, statements.headsAttributedTo, pair.issue)
		if err != nil {
			t.Fatalf("the statement by issue of %s: %v", pair.issue, err)
		}
		for rows.Next() {
			var number int
			// The select's columns are HeadsAttributedTo's scan: with the merge of main's #4426, the
			// linked repository and the pull request's repository follow the times.
			if err := rows.Scan(&number, new(string), new(bool), new(*time.Time), new(*time.Time), new(*string), new(*time.Time), new(string), new(string)); err != nil {
				t.Fatal(err)
			}
			listed = append(listed, number)
		}
		rows.Close()
		readers.byIssue[pair.issue] = listed
	}
	return readers
}

// disagreements is each pair of a pull request and an issue that one reader gives and the other
// does not.
func (readers linkReaders) disagreements() []string {
	var differ []string
	for issue, listed := range readers.byIssue {
		for _, pair := range citedPairs {
			byKey := strings.HasPrefix(readers.byKey[pair.number], issue+" from ")
			if byIssue := slices.Contains(listed, pair.number); byKey != byIssue {
				differ = append(differ, fmt.Sprintf("#%d and %s: by key %v, by issue %v", pair.number, issue, byKey, byIssue))
			}
		}
	}
	slices.Sort(differ)
	return differ
}

// A link and a citation are compared in the fold the reading gives a cited URL, ASCII lower case
// under "C", by key and by issue alike: in each database the guard admits, the reader by key and
// the reader by issue give the same pairs, and each pair the same answer, for a repository with
// İ and for one in ASCII. Two controls in the same test. The arms of the plan's text, lower()
// under the database's collation on the link alone by issue and on both sides by key, give
// pairs that differ between the two readers in each database whose lower() folds İ. A cited list
// folded with lower() under the database's collation gives #23 two answers, one in C and another
// in en_US.utf8.
func TestALinkAndACitationMatchByKeyAndByIssueAlikeInEveryLocale(t *testing.T) {
	planText := func(t *testing.T, statements *attributionStatements) *attributionStatements {
		t.Helper()
		byKey, byIssue := " and "+linkURLAsCited+" = u.url", "array(select "+linkURLAsCited+" from issue_external_links"
		for _, text := range []struct {
			statement, term string
		}{{statements.pullRequestIssues, byKey}, {statements.headsAttributedTo, byIssue}} {
			if strings.Count(text.statement, text.term) != 1 {
				t.Fatalf("the statement holds %q %d times, want 1", text.term, strings.Count(text.statement, text.term))
			}
		}
		return &attributionStatements{
			pullRequestIssues: strings.Replace(statements.pullRequestIssues, byKey, "", 1),
			headsAttributedTo: strings.Replace(statements.headsAttributedTo, byIssue, "array(select lower(l.url) from issue_external_links", 1),
		}
	}
	answers := make([]linkReaders, len(admittedLocales))
	copied := make([]string, len(admittedLocales))
	t.Run("databases", func(t *testing.T) {
		for i, database := range admittedLocales {
			t.Run(database.name, func(t *testing.T) {
				t.Parallel()
				pool := localeDatabase(t, database.options).Pool
				ctx := t.Context()
				if err := PrepareAttribution(ctx, pool); err != nil {
					t.Fatal(err)
				}
				schema := readingSchema(t, ctx, pool)
				for _, pair := range citedPairs {
					seedProjectRepository(t, ctx, pool, pair.projectRepos[0], pair.projectRepos[0], pair.projectRepos[1])
					project, number, _ := strings.Cut(pair.issue, "-")
					if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, created_by, rank)
						values ($1, $2, $3::int, $1, 'todo', '{"kind":"system","id":"test"}', $1)`, pair.issue, project, number); err != nil {
						t.Fatal(err)
					}
					if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')`, pair.issue, pair.link); err != nil {
						t.Fatal(err)
					}
					storeStatementRow(t, ctx, pool, statementRow{repo: pair.repo, number: pair.number, branch: "main", title: "no key", texts: readTexts(pair.body)})
				}
				statements := attributionStatementsFor(schema)
				answers[i] = readLinkPairs(t, ctx, pool, statements)
				for _, pair := range citedPairs {
					if got := answers[i].byKey[pair.number]; got != pair.answer {
						t.Errorf("%s#%d answers %q, want %q", pair.repo, pair.number, got, pair.answer)
					}
				}
				if differ := answers[i].disagreements(); len(differ) > 0 {
					t.Errorf("the readers by key and by issue give other pairs: %v", differ)
				}

				plan := readLinkPairs(t, ctx, pool, planText(t, statements))
				differ := plan.disagreements()
				t.Logf("the control, the arms of the plan's text: by key %v, by issue %v, pairs that differ %v", plan.byKey, plan.byIssue, differ)
				if database.name != "libc C" && len(differ) == 0 {
					t.Error("the control: the arms of the plan's text gave the readers by key and by issue the same pairs in a database whose lower() folds İ")
				}

				createReadingCopy(t, ctx, pool, "attribution_keys_lower",
					readingEdit{"pg_catalog.translate((case when m[4] is null", "pg_catalog.lower((case when m[4] is null", 1},
					readingEdit{`end) collate "C", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') u`, `end) collate "default") u`, 1})
				storeStatementRow(t, ctx, pool, statementRow{repo: "İstanbul/x", number: 23, branch: "main", title: "no key", texts: readTexts("Fixes #12.")})
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `update pull_request_texts t set cited_issues = (select k.cited_issues
					from attribution_keys_lower(t.repo, t.head_ref, t.title, t.body, t.commit_messages) k) where t.number = 23`); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				copied[i] = answerOf(t, ctx, pool, "İstanbul/x", 23)
			})
		}
	})
	if t.Failed() {
		return
	}
	for i, answer := range answers[1:] {
		for _, pair := range citedPairs {
			if answer.byKey[pair.number] != answers[0].byKey[pair.number] {
				t.Errorf("%s: #%d answers %q, and %q in %s", admittedLocales[i+1].name, pair.number, answer.byKey[pair.number], answers[0].byKey[pair.number], admittedLocales[0].name)
			}
		}
	}
	t.Logf("the control, #23 with a cited list folded by lower() under the database's collation: %q", copied)
	if copied[0] == copied[1] {
		t.Errorf("the control: #23 answers %q in %s and in %s, want two answers", copied[0], admittedLocales[0].name, admittedLocales[1].name)
	}
}
