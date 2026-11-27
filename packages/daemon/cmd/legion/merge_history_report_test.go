package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// mainsNewestAtTheChainsMerge is the newest migration main carried at the last forward merge of
// the branch this census ships on: the merge holds' kind list. Main's schema, which a deployment
// runs until this release, is every embedded migration up to it. The migrations above it are this
// branch's, the merge snapshot's columns among them.
const mainsNewestAtTheChainsMerge = 20261126110259

// mergeHistoryDatabase is a database of its own, as a URL a daemon's postgres_dsn takes, and a
// store open on it. With main it holds main's schema, every embedded migration up to
// mainsNewestAtTheChainsMerge, each applied and its version recorded as Migrate records it, so a
// Migrate of it would apply this branch's own migrations alone, as it would on a deployment's
// database; without it, every migration, as Migrate leaves it.
func mergeHistoryDatabase(t *testing.T, main bool) (string, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.Database(t, "legion_merge_history")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if !main {
		if _, err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return dsn, st
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var versions []int64
	for _, migration := range all {
		if migration.Version > mainsNewestAtTheChainsMerge {
			continue
		}
		if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, migration.SQL)
			return err
		}); err != nil {
			t.Fatalf("apply %s as main's run did: %v", migration.Name, err)
		}
		versions = append(versions, int64(migration.Version))
	}
	if len(versions) == 0 {
		t.Fatalf("no embedded migration stands at or below %d, so this database is not main's", mainsNewestAtTheChainsMerge)
	}
	// A timestamp version is past int4, so Migrate widens the column at the first one it records.
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "alter table schema_version alter column version type bigint"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "insert into schema_version (version) select unnest($1::bigint[])", versions)
		return err
	}); err != nil {
		t.Fatalf("record main's versions: %v", err)
	}
	return dsn, st
}

// mergeHistoryConfig is a legion.yaml of project ACME whose postgres_dsn is dsn.
func mergeHistoryConfig(t *testing.T, dsn string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf("project: ACME\nport: 13370\npostgres_dsn: %q\nstate_dir: %s\n", dsn, filepath.Join(dir, "state"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	return path
}

// execAll runs each statement on st, in a transaction of its own.
func execAll(t *testing.T, st *store.Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, statement)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// The inserts of the census's fixtures, in the columns main's schema and this branch's both hold.

// historyIssue records key of project on repo, in phase with Dispatch showing dispatchStatus.
func historyIssue(key, project, repo, phase, dispatchStatus string) string {
	status := map[string]string{"done": "done", "implementing": "in_progress"}[phase]
	return fmt.Sprintf(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, dispatch_status)
		values ('%s', '%s', '%s', '%s', '%s', 1, '%s', 'A', 1, '%s', '%s')`, key, key, project, key, phase, status, repo, dispatchStatus)
}

// historyPullRequest records key's pull request number on repo in state, with a base, a final
// head and, for a merged one, its merge commit.
func historyPullRequest(key, repo string, number int, state string) string {
	mergeSHA := ""
	if state == "merged" {
		mergeSHA = strings.Repeat("b", 40)
	}
	return fmt.Sprintf(`insert into pull_requests (issue, repo, number, branch, base, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha)
		values ('%s', '%s', %d, 'legion/%s', 'main', '%s', now(), '[]', 0, 0, '[]', 1, '', '', '%s', 'MERGEABLE', '%s')`,
		key, repo, number, key, strings.Repeat("a", 40), state, mergeSHA)
}

// historyCheck records key's production check of pull request number on repo, closed.
func historyCheck(key, project, repo string, number int) string {
	return fmt.Sprintf(`insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, closed_at, outcome)
		values ('%s', '%s', '%s', '%s', %d, '%s', now(), now() + interval '1 hour', now(), 'recorded')`,
		key, project, key, repo, number, strings.Repeat("c", 40))
}

// historyClock records the clock a clear kept of pull request number on repo, naming key.
func historyClock(key, repo string, number int) string {
	return fmt.Sprintf(`insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
		values ('%s', %d, '%s', now(), now())`, repo, number, key)
}

// census is one run of `legion merge-history-report` with args: its exit and each line it
// printed, by the line's first word.
type census struct {
	code   int
	lines  map[string][]string
	stdout string
}

// runCensus runs `legion merge-history-report args…` through the command dispatcher.
func runCensus(t *testing.T, args ...string) census {
	t.Helper()
	code, stdout, _ := legionRun(t, append([]string{"merge-history-report"}, args...)...)
	lines := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			lines[fields[0]] = fields[1:]
		}
	}
	return census{code: code, lines: lines, stdout: stdout}
}

// counts is the issues, done, requested and missing_metadata values of the line label: a class,
// or total.
func (c census) counts(label string) [4]string {
	fields := c.lines[label]
	var got [4]string
	for i, name := range []string{"issues", "done", "requested", "missing_metadata"} {
		for j := 0; j+1 < len(fields); j += 2 {
			if fields[j] == name {
				got[i] = fields[j+1]
			}
		}
	}
	return got
}

// expect fails the test unless the census exited code and counted each label as want says.
func (c census) expect(t *testing.T, what string, code int, want map[string][4]string) {
	t.Helper()
	if c.code != code {
		t.Fatalf("the census of %s exited %d, want %d:\n%s", what, c.code, code, c.stdout)
	}
	for label, counts := range want {
		if got := c.counts(label); got != counts {
			t.Fatalf("the census of %s counts %s as %v (issues, done, requested, missing_metadata), want %v:\n%s", what, label, got, counts, c.stdout)
		}
	}
}

// The census counts each recorded issue once, by the class its rows derive. The pair of
// repositories holds the seven closed issues of the evidence: a snapshot, a retained merged row
// and a closed production check of a pull request (ANSWERED, the check's merge knowing no base and
// no final head); an open pull request, two clocks a clear kept of one issue, and a production
// check of pull request 0 (PARTIAL); and silence (NONE). The snapshot's, the open row's and the
// silent issue each stand on a request. An eighth issue of the project, on another repository, is
// silent and at work, and another project's issue is never counted. A selection of one class holds
// two classes of no member, which count 0. A selection of no issue and a database it cannot reach
// count nothing, and print UNKNOWN.
func TestMergeHistoryReportCountsEachIssueOnce(t *testing.T) {
	t.Parallel()
	dsn, st := mergeHistoryDatabase(t, false)
	const old, renamed, other = "acme/old-widgets", "acme/widgets", "acme/tools"
	execAll(t, st,
		historyIssue("ACME-1", "ACME", renamed, "done", "todo"),
		`update issues set merge_number = 11, merge_head = '`+strings.Repeat("a", 40)+`', merge_sha = '`+strings.Repeat("b", 40)+
			`', merge_base = 'main', merge_repo = 'acme/widgets', merged_at = now() where key = 'ACME-1'`,
		historyIssue("ACME-2", "ACME", old, "done", "done"),
		historyPullRequest("ACME-2", old, 12, "merged"),
		historyIssue("ACME-3", "ACME", renamed, "done", "done"),
		historyCheck("ACME-3", "ACME", renamed, 13),
		historyIssue("ACME-4", "ACME", renamed, "done", "todo"),
		historyPullRequest("ACME-4", renamed, 14, "open"),
		historyIssue("ACME-5", "ACME", old, "done", "done"),
		historyClock("ACME-5", old, 51),
		historyClock("ACME-5", renamed, 52),
		historyIssue("ACME-6", "ACME", renamed, "done", "done"),
		historyCheck("ACME-6", "ACME", renamed, 0),
		historyIssue("ACME-7", "ACME", renamed, "done", "todo"),
		historyIssue("ACME-8", "ACME", other, "implementing", "in_progress"),
		historyIssue("OTHER-1", "OTHER", renamed, "done", "todo"),
		`update issues set merge_number = 91 where key = 'OTHER-1'`,
	)
	config := mergeHistoryConfig(t, dsn)

	pair := runCensus(t, "--config", config, "--project", "ACME", "--repo", old, "--repo", "ACME/Widgets")
	pair.expect(t, "the pair", 0, map[string][4]string{
		"ANSWERED": {"3", "3", "1", "1"},
		"PARTIAL":  {"3", "3", "1", "0"},
		"NONE":     {"1", "1", "1", "0"},
		"total":    {"7", "7", "3", "1"},
	})
	if got, want := pair.lines["selected"], []string{"7", "ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5", "ACME-6", "ACME-7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the census of the pair selected %v, want %v", got, want)
	}
	if _, unknown := pair.lines["unknown"]; unknown || len(pair.lines["snapshot_at"]) != 1 || pair.lines["snapshot_at"][0] == unknownCount {
		t.Fatalf("the census of the pair printed %q, want its snapshot's time and no unknown line", pair.stdout)
	}

	for _, args := range [][]string{{"--config", config, "--project", "ACME"}, {"--config", config}} {
		project := runCensus(t, args...)
		project.expect(t, "the project "+strings.Join(args[2:], " "), 0, map[string][4]string{
			"ANSWERED": {"3", "3", "1", "1"},
			"PARTIAL":  {"3", "3", "1", "0"},
			"NONE":     {"2", "1", "1", "0"},
			"total":    {"8", "7", "3", "1"},
		})
		if got := project.lines["selected"]; len(got) != 9 || got[0] != "8" || got[8] != "ACME-8" {
			t.Fatalf("the census of the project selected %v, want the eight issues of ACME and no other project's", got)
		}
	}

	runCensus(t, "--config", config, "--repo", other).expect(t, "the other repository", 0, map[string][4]string{
		"ANSWERED": {"0", "0", "0", "0"},
		"PARTIAL":  {"0", "0", "0", "0"},
		"NONE":     {"1", "0", "0", "0"},
		"total":    {"1", "0", "0", "0"},
	})

	unknown := [4]string{unknownCount, unknownCount, unknownCount, unknownCount}
	everyCount := map[string][4]string{"ANSWERED": unknown, "PARTIAL": unknown, "NONE": unknown, "total": unknown}
	empty := runCensus(t, "--config", config, "--project", "NOPE")
	empty.expect(t, "a project of no recorded issue", 1, everyCount)
	if got := empty.lines["unknown"]; !reflect.DeepEqual(got, []string{"empty"}) {
		t.Fatalf("the census of a project of no recorded issue printed unknown %v, want empty:\n%s", got, empty.stdout)
	}
	unreached := runCensus(t, "--config", legionConfig(t, "ACME", 13370))
	unreached.expect(t, "a database it cannot reach", 1, everyCount)
	if got := unreached.lines["unknown"]; !reflect.DeepEqual(got, []string{"connect"}) {
		t.Fatalf("the census of a database it cannot reach printed unknown %v, want connect:\n%s", got, unreached.stdout)
	}
}

// catalogText is each column of the database's current schema with its type, and each table, one
// a line, sorted: what a migration of the census's input would change.
func catalogText(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select 'column ' || table_name || '.' || column_name || ' ' || data_type
		from information_schema.columns where table_schema = current_schema()
		union all select 'table ' || tablename from pg_tables where schemaname = current_schema() order by 1`)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	text, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	return text
}

// The census reads the schema of main's release, which a deployment runs until this one: it has
// no merge snapshot's columns, and the missing columns grant no evidence. Two positive merges (a
// retained merged row and a closed production check of a pull request), three rows that prove
// none (an open pull request, a clock a clear kept and a check of pull request 0) and a silent
// issue count two ANSWERED, three PARTIAL and one NONE. The command reads in a read-only
// transaction: no catalog entry and no row changes, so it migrated nothing. A database whose
// catalog holds no record is no old schema of zero issues: it counts nothing and prints UNKNOWN.
func TestMergeHistoryReportReadsTheMainSchema(t *testing.T) {
	t.Parallel()
	dsn, st := mergeHistoryDatabase(t, true)
	const repo = "acme/widgets"
	execAll(t, st,
		historyIssue("ACME-1", "ACME", repo, "done", "done"),
		historyPullRequest("ACME-1", repo, 11, "merged"),
		historyIssue("ACME-2", "ACME", repo, "done", "done"),
		historyCheck("ACME-2", "ACME", repo, 12),
		historyIssue("ACME-3", "ACME", repo, "done", "todo"),
		historyPullRequest("ACME-3", repo, 13, "open"),
		historyIssue("ACME-4", "ACME", repo, "done", "done"),
		historyClock("ACME-4", repo, 14),
		historyIssue("ACME-5", "ACME", repo, "done", "done"),
		historyCheck("ACME-5", "ACME", repo, 0),
		historyIssue("ACME-6", "ACME", repo, "done", "done"),
	)
	config := mergeHistoryConfig(t, dsn)
	catalog, rows := catalogText(t, st), testdb.TableRows(t, st.Pool())

	read := runCensus(t, "--config", config, "--project", "ACME")
	read.expect(t, "main's schema", 0, map[string][4]string{
		"ANSWERED": {"2", "2", "0", "1"},
		"PARTIAL":  {"3", "3", "1", "0"},
		"NONE":     {"1", "1", "0", "0"},
		"total":    {"6", "6", "1", "1"},
	})
	if got := catalogText(t, st); !reflect.DeepEqual(got, catalog) {
		t.Fatalf("the catalog after the census differs from the one before it: the census changed the schema it read")
	}
	if changed := testdb.ChangedTables(rows, testdb.TableRows(t, st.Pool())); len(changed) != 0 {
		t.Fatalf("the census changed the rows of %v, want none written", changed)
	}

	bare := mergeHistoryConfig(t, testdb.Database(t, "legion_merge_history_bare"))
	unread := runCensus(t, "--config", bare, "--project", "ACME")
	unknown := [4]string{unknownCount, unknownCount, unknownCount, unknownCount}
	unread.expect(t, "a database whose catalog holds no record", 1, map[string][4]string{"ANSWERED": unknown, "PARTIAL": unknown, "NONE": unknown, "total": unknown})
	if got := unread.lines["unknown"]; !reflect.DeepEqual(got, []string{"read"}) {
		t.Fatalf("the census of a database whose catalog holds no record printed unknown %v, want read:\n%s", got, unread.stdout)
	}
}
