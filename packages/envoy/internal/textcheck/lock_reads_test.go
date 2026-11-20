package textcheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lockReadRoots are the Go modules whose tests run against a Postgres server other tests share,
// from this directory: the envoy module, with packages/envoy/infra beneath it, and the Legion
// daemon's.
var lockReadRoots = []string{"../..", "../../../daemon"}

// lockReadRepository is the repository's root, from this directory: an offence's path and an
// exemption's key are relative to it.
const lockReadRepository = "../../../.."

// lockViewFile is a file that names a view or function lockViewRead reads, as a quick first look
// before the file is parsed.
var lockViewFile = regexp.MustCompile(`pg_(locks|stat_activity|stat_get_activity|lock_status|stat_get_backend_idset)\b`)

// lockViewRead is a read, in a statement's shape (lockReadShape), of a view or function that lists
// every database's locks or backends on the server: pg_locks or pg_stat_activity after from, join
// or a comma; the functions behind them, pg_stat_get_activity and pg_lock_status; or the ids of
// every backend that the per-backend statistics functions take, pg_stat_get_backend_idset. Its
// submatch is the view's name.
var lockViewRead = regexp.MustCompile(`(?:\bfrom|\bjoin|,)\s*(pg_locks|pg_stat_activity)\b|\b(pg_stat_get_activity|pg_lock_status|pg_stat_get_backend_idset)\s*\(`)

// lockReadOwnDatabase is the test's own database by its oid.
const lockReadOwnDatabase = `\(\s*select\s+oid\s+from\s+pg_database\s+where\s+datname\s*=\s*current_database\(\s*\)\s*\)`

// lockReadScope is a condition that keeps a read's rows to its test's own database or a backend of
// its own: the database, by name against current_database() or by oid (pg_locks' database,
// pg_stat_activity's datid, a backend id's pg_stat_get_backend_dbid); one backend by its process
// id, a parameter's, a list's or the reading backend's own; the backends one of those blocks; or
// an application name a test gives its own connections, as a parameter. Each is a comparison,
// with = and the test's own value on one side, so a function named in a select list
// (current_database(), count(*)), a column compared with itself or with another value
// (database = database, datname <> current_database()), or a backend compared with every backend
// (pid = any(select pid ...), cardinality(pg_blocking_pids(pid)) > 0) keeps to nothing.
var lockReadScope = regexp.MustCompile(`(?:\b\w+\.)?\b(?:database|datid)\s*=\s*` + lockReadOwnDatabase +
	`|\bpg_stat_get_backend_dbid\s*\(\s*\w+\s*\)\s*=\s*` + lockReadOwnDatabase +
	`|(?:\b\w+\.)?\bdatname\s*=\s*current_database\(\s*\)` +
	`|\bcurrent_database\(\s*\)\s*=\s*(?:\w+\.)?datname\b` +
	`|(?:\b\w+\.)?\bpid\s*=\s*(?:\$[0-9]+\b|any\s*\(\s*\$[0-9]+\s*(?:::[\w\[\]]+)?\s*\)|pg_backend_pid\(\s*\))` +
	`|(?:\$[0-9]+(?:::\w+)?|pg_backend_pid\(\s*\))\s*=\s*any\s*\(\s*pg_blocking_pids\s*\(` +
	`|(?:\b\w+\.)?\bapplication_name\s*=\s*\$[0-9]+\b`)

// lockReadCorrelation is a nested read's condition that ties its rows to one row of the read
// around it by process id (i.pid = l.pid), lockReadBlockingStep a recursive query's step that
// takes the backends a backend of the rows so far blocks (waiting.pid =
// any(pg_blocking_pids(blocked.pid))), and lockReadExclusion the start of a read whose rows only
// take their own process ids out of the read around it (pid not in (select pid ...)).
var (
	lockReadCorrelation  = regexp.MustCompile(`\b\w+\.pid\s*=\s*\w+\.pid\b`)
	lockReadBlockingStep = regexp.MustCompile(`\b\w+\.pid\s*=\s*any\s*\(\s*pg_blocking_pids\s*\(`)
	lockReadExclusion    = regexp.MustCompile(`(?:\b\w+\.)?\bpid\s+not\s+in\s*$`)
)

// lockReadFilter is the word a condition of a query block's filter follows; lockReadUndone is what
// at the same level of a block lets a row through that a condition beside it keeps out: an or, or
// a gap ‹?›, which can hold one; lockReadSetOperation is what parts one query block of a group
// from the next; lockReadSubquery is how a group that is a query of its own starts.
var (
	lockReadFilter       = regexp.MustCompile(`\b(where|and|on)$`)
	lockReadUndone       = regexp.MustCompile(`\bor\b|‹\?›`)
	lockReadSetOperation = regexp.MustCompile(`\b(union|intersect|except)\b(\s+all\b)?`)
	lockReadSubquery     = regexp.MustCompile(`^\s*(select|with|values)\b`)
)

// lockReadExemptions are the reads of test code that keep to no database or backend of their own
// and whose tests depend on no other test's database, keyed "<path> <function>", each with why:
// two that no server runs; one that counts locks on a table only its own database holds; and two
// that read every backend of the server, one counting them on purpose and one whose test asserts
// nothing of what it reads. A key that names no such read is itself an offence, so the list
// cannot outlive its reads.
var lockReadExemptions = map[string]string{
	"packages/envoy/cmd/dispatch/connection_ceiling_test.go connectionRefusalCause": "a refused connection's diagnosis, not a " +
		"lock read: it counts every client backend of the server beside the server's max_connections, the limit Postgres " +
		"refused the connection on, so it counts the whole server's on purpose",
	"packages/envoy/internal/broker/store/migrate_guard_test.go TestMigrateReleasesTheVersionTablesAlterLockBeforeItsMigrations": "the " +
		"test's own migration counts the locks on broker_schema_migrations, a table the test creates in an empty database of " +
		"its own (emptySchemaStore): a table created on the server takes an oid from the server's one counter, which no " +
		"other database's table holds",
	"packages/envoy/internal/dispatch/store/pool_test.go TestHorizonIsServedWhileTheSharedPoolIsFullyAcquired": "a read the " +
		"horizon pool must answer while the shared pool is fully acquired; it counts every backend of the server, and the " +
		"test asserts that it answers, and nothing of the count",
	"packages/envoy/internal/pgmigrate/set_test.go TestLoadRefusesACensusWithNoMigrationOneThatIsNotASelectAndOneThatCallsAForbiddenFunction": "census " +
		"files the loader must refuse for the function each calls (pg_terminate_backend, pg_cancel_backend); no server runs " +
		"them",
}

// lockReadTestPrograms are the programs only a test suite runs, by directory from the
// repository's root: each counts as test code, so what it imports does too, as a test file's
// imports do. The request seeder is one: Dispatch's browser suite runs it against its broker
// (packages/dispatch/e2e/real-broker.ts), and it imports brokertest and, through it, the broker's
// storetest, which a program that ships would make code that ships. A directory listed here that
// holds no Go file is an offence, so the list cannot outlive its programs.
var lockReadTestPrograms = []string{"packages/envoy/internal/broker/brokertest/requestseed"}

// The tests of these modules share one Postgres server: storetest.Open clones a database per test
// from one template, the packages of a run test beside each other, and runs beside each other on
// one server. pg_locks and pg_stat_activity list every database's locks and backends, and every
// clone holds the same tables under the same oids and takes the same advisory keys, so a read of
// either that keeps to no database or backend counts another test's waiter, or names another
// test's holder, as its own: the API's upload lock probe named another test's spec settlement as
// its issue's holder and failed. So every such read in test code keeps its rows to its database or
// a backend of its own, or reads through storetest (Waiting, Holders and WaitersOn keep to the
// database they run in), or is named in lockReadExemptions with why its test depends on no other
// test's database.
//
// Test code is every _test.go file, every program lockReadTestPrograms names, and every file of a
// package only test code imports: the fixtures, storetest among them. The check reads each
// statement as a string literal, a `+` chain of literals, its package's string constants and its
// function's string variables that are set once, strings.Join of a list of them, or fmt.Sprintf of
// one; a part it cannot read, a verb's value among them, is a gap ‹?›. It reads the statement's
// shape (lockReadShape): comments, quoted literals, the quotes of identifiers and pg_catalog's
// qualifier taken out, so `from /* ... */ "pg_locks"` is a read and `like '%pg_locks%'` is not.
//
// Each read of a view (lockViewRead) lies in a query block: the statement, or the parentheses of a
// subquery, cut at union, intersect and except. The block keeps to its database or a backend when
// a condition of its own filter, after where, and or on at its own level, is a lockReadScope
// comparison, with no or and no gap at that level, which could let through a row the condition
// keeps out (a gap is any text, a caller's `x or true` among them). A condition in parentheses of
// its own counts when those parentheses stand as a condition themselves, by the same rule. A
// subquery's block keeps to its database too when its rows are tied by process id to a row of the
// block around it, which keeps to its own (lockReadCorrelation), when it is a recursive query's
// step from a block beside it that keeps to its own (lockReadBlockingStep), or when it only takes
// process ids out of the block around it (lockReadExclusion): a backend's locks are in its own
// database. A statement keeps to its database when every block that reads a view does. So a
// condition in the select list, one undone by an or, one of another subquery, and one that comes
// in as a parameter are refused, even when every caller passes a scoped one: the check reads a
// statement, not the calls that reach it, and a caller added later can pass any condition. Such a
// read names its scope in the statement, as a constant of its package (`... where ` +
// ownBackends), keeps a caller's condition in parentheses of its own, or reads through storetest.
//
// What the check does not see: the reads of one block share its conditions, so a join of two views
// where only one keeps to its database passes; and a view named through what it cannot resolve (a
// parameter, a field, a variable set more than once, a function it does not know) is a gap to it,
// so that statement is no read. Each such read is found by reading it, not by this check.
func TestEveryLockReadOfATestKeepsToItsOwnDatabase(t *testing.T) {
	offences, counts := unscopedLockReads(t, lockReadRepository, lockReadRoots, lockReadTestPrograms, lockReadExemptions)
	if counts.testFiles == 0 || counts.reads == 0 {
		t.Fatalf("UNKNOWN: the check read %d Go files, %d of them test code, and found %d lock reads under %v: it cannot see what it guards", counts.files, counts.testFiles, counts.reads, lockReadRoots)
	}
	t.Logf("read %d Go files, %d of them test code; %d statements of test code read pg_locks or pg_stat_activity: %d keep to their database or a backend, %d are exempt", counts.files, counts.testFiles, counts.reads, counts.scoped, counts.exempt)
	if len(offences) > 0 {
		t.Fatalf("%d reads of pg_locks or pg_stat_activity in test code keep to no database or backend of their own:\n\t%s\n"+
			"read the lock through storetest (Waiting, Holders, WaitersOn), or keep the read to its database "+
			"(datname = current_database(), or pg_locks' database against its oid) or to one backend by its pid, "+
			"as a condition of its own block's filter (where, and or on) with no or beside it; "+
			"a read whose test depends on no other test's database goes in lockReadExemptions with why",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// lockReadPlant is one read TestTheLockReadCheckSeesWhatItGuards plants: the function that holds
// it, its body, and what the check must make of it: refused, with the statement it names; passed,
// as kept to its database or a backend; exempt; or no read at all.
type lockReadPlant struct {
	function, body string
	verdict        string // "refused", "kept", "exempt" or "none"
	statement      string // a refused read's statement as the check names it
}

// viewName is a view's or function's name. The planted reads are built from it, so this file holds
// no read of its own.
func viewName(rest string) string { return "pg_" + rest }

// The check refuses an unscoped read in a test file, in a fixture only tests import, and in a
// package only a listed test program imports; one assembled from a part it cannot read; one whose
// scope comes in as a parameter, though its one caller passes a scoped condition; and each
// spelling of an unscoped read below, where SQL or Go spells the same read another way, or a
// condition stands where it keeps no row out. It passes a read that keeps its own block to its
// database, a backend or what one blocks, in each place that keeps its rows; a read whose scope
// comes from a constant; a literal that names a view in a sentence or in a quoted literal; a read
// in a package that production code imports; and an exempt read. It refuses an exemption that
// names no read and a listed program that is not there.
func TestTheLockReadCheckSeesWhatItGuards(t *testing.T) {
	repository := t.TempDir()
	views := strings.NewReplacer("LOCKS", viewName("locks"), "ACTIVITY", viewName("stat_activity"),
		"GETACT", viewName("stat_get_activity"), "IDSET", viewName("stat_get_backend_idset"))
	write := func(path, source string) {
		t.Helper()
		full := filepath.Join(repository, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(views.Replace(source)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ownDatabase := "(select oid from pg_database where datname = current_database())"
	plants := []lockReadPlant{
		{"unscoped", "_ = `select count(*) from LOCKS where relation = 'x'::regclass and not granted`", "refused",
			"select count(*) from LOCKS where relation = 'x'::regclass and not granted"},
		{"activity", "_ = `select count(*) from ACTIVITY where wait_event_type = 'Lock'`", "refused",
			"select count(*) from ACTIVITY where wait_event_type = 'lock'"},
		{"listing", "_ = `select pid from GETACT(null)`", "refused", "select pid from GETACT(null)"},
		{"assembled", "_ = `select count(*) from LOCKS l where ` + more", "refused", "select count(*) from LOCKS l where ‹?›"},
		{"scopeFromParameter", "_ = `select count(*) from ACTIVITY where ` + more", "refused", "select count(*) from ACTIVITY where ‹?›"},
		{"callsIt", "scopeFromParameter(here)", "none", ""},
		{"commaJoin", "_ = `select count(*) from pg_class c, LOCKS l where l.relation = c.oid and c.relname = 'x' and not l.granted`", "refused",
			"select count(*) from pg_class c, LOCKS l where l.relation = c.oid and c.relname = 'x' and not l.granted"},
		{"quotedName", "_ = `select count(*) from \"LOCKS\" where not granted`", "refused", `select count(*) from "LOCKS" where not granted`},
		{"blockComment", "_ = `select count(*) from /* the view */ LOCKS where not granted`", "refused",
			"select count(*) from /* the view */ LOCKS where not granted"},
		{"lineComment", `_ = "select count(*) from -- the view\nLOCKS where not granted"`, "refused",
			"select count(*) from -- the view LOCKS where not granted"},
		{"catalogName", "_ = `select count(*) from pg_catalog.LOCKS where not granted`", "refused",
			"select count(*) from pg_catalog.LOCKS where not granted"},
		{"joined", `_ = strings.Join([]string{"select count(*) from", "LOCKS", "where not granted"}, " ")`, "refused",
			"select count(*) from LOCKS where not granted"},
		{"viewInAVariable", `view := "LOCKS"; _ = "select count(*) from " + view + " where not granted"`, "refused",
			"select count(*) from LOCKS where not granted"},
		{"backendIDs", "_ = `select count(*) from IDSET() s where pg_stat_get_backend_wait_event_type(s) = 'Lock'`", "refused",
			"select count(*) from IDSET() s where pg_stat_get_backend_wait_event_type(s) = 'lock'"},
		{"scopeInTheSelectList", "_ = `select pid, datname = current_database() as mine from ACTIVITY where wait_event_type = 'Lock'`", "refused",
			"select pid, datname = current_database() as mine from ACTIVITY where wait_event_type = 'lock'"},
		{"databaseSelected", "_ = `select current_database(), count(*) from LOCKS where not granted`", "refused",
			"select current_database(), count(*) from LOCKS where not granted"},
		{"scopeNegated", "_ = `select count(*) from ACTIVITY where datname <> current_database()`", "refused",
			"select count(*) from ACTIVITY where datname <> current_database()"},
		{"databaseAgainstItself", "_ = `select count(*) from LOCKS l where not l.granted and l.database = l.database`", "refused",
			"select count(*) from LOCKS l where not l.granted and l.database = l.database"},
		{"databaseFromAVerb", "_ = fmt.Sprintf(`select count(*) from LOCKS l where l.database = %d or not l.granted`, len(more))", "refused",
			"select count(*) from LOCKS l where l.database = ‹?› or not l.granted"},
		{"blockedByAnyone", "_ = `select count(*) from ACTIVITY where cardinality(pg_blocking_pids(pid)) > 0`", "refused",
			"select count(*) from ACTIVITY where cardinality(pg_blocking_pids(pid)) > 0"},
		{"anyBackend", "_ = `select count(*) from LOCKS where not granted and pid = any(select pid from ACTIVITY)`", "refused",
			"select count(*) from LOCKS where not granted and pid = any(select pid from ACTIVITY)"},
		{"scopeUndoneByAnOr", "_ = `select count(*) from ACTIVITY where datname = current_database() or wait_event_type = 'Lock'`", "refused",
			"select count(*) from ACTIVITY where datname = current_database() or wait_event_type = 'lock'"},
		{"scopeBesideAGap", "_ = `select count(*) from ACTIVITY where datname = current_database() and ` + more", "refused",
			"select count(*) from ACTIVITY where datname = current_database() and ‹?›"},
		{"scopeOfAnotherRead", "_ = `select count(*) from LOCKS where not granted and exists (select 1 from ACTIVITY where datname = current_database())`", "refused",
			"select count(*) from LOCKS where not granted and exists (select 1 from ACTIVITY where datname = current_database())"},
		{"scopeOfAnotherTable", "_ = `select count(*) from LOCKS where not granted and exists (select 1 from pg_database where datname = current_database())`", "refused",
			"select count(*) from LOCKS where not granted and exists (select 1 from pg_database where datname = current_database())"},
		{"scopeOfAnotherBranch", "_ = `select pid from ACTIVITY where datname = current_database() union select pid from LOCKS where not granted`", "refused",
			"select pid from ACTIVITY where datname = current_database() union select pid from LOCKS where not granted"},
		{"scopeNotFiltering", "_ = `select count(*) from ACTIVITY where not (datname = current_database())`", "refused",
			"select count(*) from ACTIVITY where not (datname = current_database())"},
		{"scoped", "_ = `select count(*) from LOCKS l where l.database = " + ownDatabase + "`", "kept", ""},
		{"byPID", "_ = `select mode from LOCKS where pid = $1`", "kept", ""},
		{"byPIDs", "_ = `select pid from GETACT(null) with ordinality where pid = any($1)`", "kept", ""},
		{"blockedByOwnBackend", "_ = `select count(*) from ACTIVITY where $1 = any(pg_blocking_pids(pid))`", "kept", ""},
		{"byApplication", "_ = `select count(*) from ACTIVITY where application_name = $1`", "kept", ""},
		{"byOwnBackendIDs", "_ = `select count(*) from IDSET() s where pg_stat_get_backend_dbid(s) = " + ownDatabase + "`", "kept", ""},
		{"fromConstant", "_ = `select count(*) from ACTIVITY where ` + here", "kept", ""},
		{"scopeInParentheses", "_ = `select count(*) from ACTIVITY where (datname = current_database() and wait_event_type = 'Lock')`", "kept", ""},
		{"gapInParentheses", "_ = `select count(*) from ACTIVITY where datname = current_database() and (` + more + `)`", "kept", ""},
		{"orInParentheses", "_ = `select count(*) from ACTIVITY where datname = current_database() and (state = 'idle' or state = 'active')`", "kept", ""},
		{"joinedByPID", "_ = `select l.mode from LOCKS l join ACTIVITY a on a.pid = l.pid where a.datname = current_database()`", "kept", ""},
		{"correlated", "_ = `select count(*) from LOCKS l where l.database = " + ownDatabase + " and exists (select 1 from LOCKS i where i.pid = l.pid and i.granted)`", "kept", ""},
		{"excluded", "_ = `select pid from ACTIVITY where datname = current_database() and pid not in (select pid from LOCKS where granted)`", "kept", ""},
		{"recursiveStep", "_ = `with recursive waiting(pid) as (select pid from LOCKS where not granted and $1 = any(pg_blocking_pids(pid)) " +
			"union select blocked.pid from LOCKS blocked, waiting where not blocked.granted and waiting.pid = any(pg_blocking_pids(blocked.pid))) " +
			"select count(*) from waiting`", "kept", ""},
		{"sprintfScoped", "_ = fmt.Sprintf(`select count(*) from ACTIVITY where datname = current_database() and query like '%%%s%%'`, more)", "kept", ""},
		{"sentence", `_ = "read LOCKS: %v"`, "none", ""},
		{"quotedLiteral", "_ = `select count(*) from issues where title like '%from LOCKS%'`", "none", ""},
		{"exempt", "_ = `select count(*) from ACTIVITY`", "exempt", ""},
	}
	source := []string{"package a", "", `import (`, `	"fmt"`, `	"strings"`, "", `	_ "example.com/m/fixturetest"`, `)`, "",
		"const here = `datname = current_database()`", "",
		"var _ = fmt.Sprint", "var _ = strings.Join", ""}
	var want []string
	reads, kept := 0, 0
	for _, plant := range plants {
		source = append(source, "func "+plant.function+"(more string) { "+plant.body+" }")
		switch plant.verdict {
		case "refused":
			want = append(want, fmt.Sprintf("m/a/a_test.go:%d: %s: %s", len(source), plant.function, views.Replace(plant.statement)))
			reads++
		case "kept":
			reads++
			kept++
		case "exempt":
			reads++
		}
	}
	write("m/go.mod", "module example.com/m\n")
	write("m/a/a_test.go", strings.Join(append(source, ""), "\n"))
	write("m/fixturetest/fixture.go", "package fixturetest\n\nfunc Waiters() { _ = `select count(*) from LOCKS where not granted` }\n")
	write("m/prod/prod.go", "package prod\n\nfunc Count() { _ = `select count(*) from ACTIVITY` }\n")
	write("m/main.go", "package main\n\nimport _ \"example.com/m/prod\"\n\nfunc main() {}\n")
	write("m/seed/main.go", "package main\n\nimport _ \"example.com/m/seedlib\"\n\nfunc main() {}\n")
	write("m/seedlib/seedlib.go", "package seedlib\n\nfunc Reads() { _ = `select pid from ACTIVITY where state = 'active'` }\n")

	offences, counts := unscopedLockReads(t, repository, []string{filepath.Join(repository, "m")}, []string{"m/seed", "m/gone"}, map[string]string{
		"m/a/a_test.go exempt":   "planted",
		"m/a/a_test.go vanished": "planted, and names no read",
	})
	want = append(want,
		"m/a/a_test.go vanished: exempt, but no read of test code there keeps to no database or backend",
		"m/fixturetest/fixture.go:3: Waiters: select count(*) from "+views.Replace("LOCKS")+" where not granted",
		"m/gone: listed as a program only tests run, but it holds no Go file",
		"m/seedlib/seedlib.go:3: Reads: select pid from "+views.Replace("ACTIVITY")+" where state = 'active'",
	)
	slices.Sort(want)
	if !slices.Equal(offences, want) {
		t.Errorf("the check found\n\t%s\nwant\n\t%s", strings.Join(offences, "\n\t"), strings.Join(want, "\n\t"))
	}
	if want := (lockReadCounts{files: counts.files, testFiles: 4, reads: reads + 2, scoped: kept, exempt: 1}); counts != want {
		t.Errorf("the check counted %+v, want %+v: 4 test files (the test, the fixture only it imports, the listed program and the package only it imports), and of the reads the fixture's, the seeder's and each plant that is a read", counts, want)
	}
}

// lockReadCounts is what unscopedLockReads read: Go files, those of them that are test code, the
// statements of test code that read pg_locks or pg_stat_activity, and of those the ones that keep
// to a database or backend and the ones exempt.
type lockReadCounts struct {
	files, testFiles, reads, scoped, exempt int
}

// goSource is one Go file the walk found: its package's directory and name, whether it is a test
// file, and the import paths it names.
type goSource struct {
	path, dir, pkg string
	test           bool
	imports        []string
}

// unscopedLockReads walks the Go modules under roots and names every statement of test code that
// reads a view of every database's locks or backends (lockViewRead) in a query block that keeps to
// no database or backend (lockReadKeepsToItsOwn), as "<path>:<line>: <function>: <statement>", the
// statement lowercased, its whitespace collapsed and a part it cannot read written ‹?›, but those
// exempt names by "<path> <function>"; each key of exempt that names no such read; and each of
// programs, the directories of the programs only tests run, that holds no Go file. Paths are
// relative to repository. node_modules, vendor, testdata and hidden directories are passed over,
// as go passes over the last two.
func unscopedLockReads(t *testing.T, repository string, roots, programs []string, exempt map[string]string) ([]string, lockReadCounts) {
	t.Helper()
	repository, err := filepath.Abs(repository)
	if err != nil {
		t.Fatal(err)
	}
	var counts lockReadCounts
	modules := map[string]string{} // a module's directory -> its path
	var sources []goSource
	files := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); path != root && (name == "node_modules" || name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case entry.Name() == "go.mod":
				text, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for line := range strings.Lines(string(text)) {
					if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
						modules[filepath.Dir(path)] = strings.Trim(strings.TrimSpace(module), `"`)
					}
				}
			case strings.HasSuffix(path, ".go"):
				file, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				source := goSource{path: path, dir: filepath.Dir(path), pkg: file.Name.Name, test: strings.HasSuffix(path, "_test.go")}
				for _, spec := range file.Imports {
					imported, err := strconv.Unquote(spec.Path.Value)
					if err != nil {
						return err
					}
					source.imports = append(source.imports, imported)
				}
				sources = append(sources, source)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the Go files under %s: %v", root, err)
		}
	}
	counts.files = len(sources)

	// A file of a program only tests run counts as a test file.
	listed := map[string]bool{}
	for _, program := range programs {
		listed[program] = false
	}
	for i, source := range sources {
		absolute, err := filepath.Abs(source.dir)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(repository, absolute)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := listed[filepath.ToSlash(rel)]; ok {
			listed[filepath.ToSlash(rel)] = true
			sources[i].test = true
		}
	}

	// Each directory's import path, from the module whose directory is the nearest above it.
	importPath := map[string]string{}
	for _, source := range sources {
		if _, done := importPath[source.dir]; done {
			continue
		}
		for dir := source.dir; ; dir = filepath.Dir(dir) {
			if module, ok := modules[dir]; ok {
				rel, err := filepath.Rel(dir, source.dir)
				if err != nil {
					t.Fatal(err)
				}
				importPath[source.dir] = strings.TrimSuffix(module+"/"+filepath.ToSlash(rel), "/.")
				break
			}
			if parent := filepath.Dir(dir); parent == dir {
				importPath[source.dir] = ""
				break
			}
		}
	}
	// A package only test code imports: it has importers, and each is a test file or a file of
	// another such package. Every imported package starts as one, and each loses it once a file
	// of a package that is not one imports it, until no package loses it.
	directory := map[string]string{}
	for dir, path := range importPath {
		directory[path] = dir
	}
	importers := map[string][]goSource{}
	for _, source := range sources {
		for _, imported := range source.imports {
			if dir, ok := directory[imported]; ok && dir != source.dir {
				importers[dir] = append(importers[dir], source)
			}
		}
	}
	testOnly := map[string]bool{}
	for dir := range importers {
		testOnly[dir] = true
	}
	for changed := true; changed; {
		changed = false
		for dir := range testOnly {
			for _, importer := range importers[dir] {
				if !importer.test && !testOnly[importer.dir] {
					delete(testOnly, dir)
					changed = true
					break
				}
			}
		}
	}

	used := map[string]bool{}
	var offences []string
	constants := map[string]map[string]string{}
	for _, source := range sources {
		if !source.test && !testOnly[source.dir] {
			continue
		}
		counts.testFiles++
		text, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatal(err)
		}
		if !lockViewFile.Match(bytes.ToLower(text)) {
			continue
		}
		key := source.dir + " " + source.pkg
		if constants[key] == nil {
			constants[key] = packageStringConstants(t, files, sources, source)
		}
		file, err := parser.ParseFile(files, source.path, text, 0)
		if err != nil {
			t.Fatal(err)
		}
		absolute, err := filepath.Abs(source.path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(repository, absolute)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		for _, read := range lockReadsOf(files, file, constants[key]) {
			counts.reads++
			switch {
			case lockReadKeepsToItsOwn(read.shape):
				counts.scoped++
			case exempt[rel+" "+read.function] != "":
				counts.exempt++
				used[rel+" "+read.function] = true
			default:
				offences = append(offences, fmt.Sprintf("%s:%d: %s: %s", rel, read.line, read.function, read.text))
			}
		}
	}
	for program, found := range listed {
		if !found {
			offences = append(offences, program+": listed as a program only tests run, but it holds no Go file")
		}
	}
	for key := range exempt {
		if !used[key] {
			offences = append(offences, key+": exempt, but no read of test code there keeps to no database or backend")
		}
	}
	slices.Sort(offences)
	return offences, counts
}

// packageStringConstants is every package-level string constant of source's package, by name, as
// a statement is assembled from them: a literal, or a `+` chain of literals and the package's
// other constants, resolved until no pass resolves one more. A name two of the package's files
// declare with different values, or one the chain cannot read whole, is left out.
func packageStringConstants(t *testing.T, files *token.FileSet, sources []goSource, of goSource) map[string]string {
	t.Helper()
	values := map[string]ast.Expr{}
	ambiguous := map[string]bool{}
	for _, source := range sources {
		if source.dir != of.dir || source.pkg != of.pkg {
			continue
		}
		file, err := parser.ParseFile(files, source.path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				valueSpec := spec.(*ast.ValueSpec)
				if len(valueSpec.Values) != len(valueSpec.Names) {
					continue
				}
				for i, name := range valueSpec.Names {
					if _, seen := values[name.Name]; seen {
						ambiguous[name.Name] = true
					}
					values[name.Name] = valueSpec.Values[i]
				}
			}
		}
	}
	resolved := map[string]string{}
	for changed := true; changed; {
		changed = false
		for name, value := range values {
			if _, done := resolved[name]; done || ambiguous[name] {
				continue
			}
			if text, whole, ok := statementText(value, resolved); ok && whole {
				resolved[name] = text
				changed = true
			}
		}
	}
	return resolved
}

// lockRead is one statement lockReadsOf found: its line, the function or package-level value that
// holds it, its text as an offence names it, and its shape (lockReadShape).
type lockRead struct {
	line     int
	function string
	text     string
	shape    string
}

// lockReadsOf is every statement of file that reads a view of every database's locks or backends
// (lockViewRead, in its shape): each string literal, `+` chain of them, constants and its
// function's string variables set once (localStrings) that is not part of a longer chain,
// strings.Join or fmt.Sprintf of them, its text lowercased, its whitespace collapsed, a part it
// cannot read written ‹?›.
func lockReadsOf(files *token.FileSet, file *ast.File, constants map[string]string) []lockRead {
	var reads []lockRead
	for _, decl := range file.Decls {
		holder := ""
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			holder = decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) == 1 {
				receiver := decl.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if index, ok := receiver.(*ast.IndexExpr); ok {
					receiver = index.X
				}
				if named, ok := receiver.(*ast.Ident); ok {
					holder = named.Name + "." + holder
				}
			}
		case *ast.GenDecl:
			var names []string
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range value.Names {
						names = append(names, name.Name)
					}
				}
			}
			holder = strings.Join(names, ",")
		}
		names := localStrings(decl, constants)
		ast.Inspect(decl, func(node ast.Node) bool {
			expression, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			// A constant or variable on its own is read where it is set, and inside each chain it
			// is part of; a use of it alone is neither.
			if _, alone := expression.(*ast.Ident); alone {
				return true
			}
			text, _, ok := statementText(expression, names)
			if !ok {
				return true
			}
			shape := lockReadShape(text)
			if lockViewRead.MatchString(shape) {
				reads = append(reads, lockRead{
					line:     files.Position(expression.Pos()).Line,
					function: holder,
					text:     strings.ToLower(strings.Join(strings.Fields(text), " ")),
					shape:    shape,
				})
				return false
			}
			// A call's arguments are statements of their own; a chain's parts are not.
			_, call := expression.(*ast.CallExpr)
			return call
		})
	}
	return reads
}

// localStrings is constants with the string variables of decl's function, its closures' among
// them, that are set once, to a statement the check can read whole: a statement assembled from one
// reads its text. A name the function sets more than once, or also takes as a parameter, is
// neither a constant nor a variable to it, since the check cannot tell which value a use reads.
func localStrings(decl ast.Decl, constants map[string]string) map[string]string {
	function, ok := decl.(*ast.FuncDecl)
	if !ok || function.Body == nil {
		return constants
	}
	values := map[string]ast.Expr{}
	sets := map[string]int{}
	ast.Inspect(function, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			for i, target := range node.Lhs {
				if name, ok := target.(*ast.Ident); ok {
					sets[name.Name]++
					if len(node.Rhs) == len(node.Lhs) && (node.Tok == token.DEFINE || node.Tok == token.ASSIGN) {
						values[name.Name] = node.Rhs[i]
					}
				}
			}
		case *ast.ValueSpec:
			for i, name := range node.Names {
				sets[name.Name]++
				if len(node.Values) == len(node.Names) {
					values[name.Name] = node.Values[i]
				}
			}
		case *ast.RangeStmt:
			for _, target := range []ast.Expr{node.Key, node.Value} {
				if name, ok := target.(*ast.Ident); ok {
					sets[name.Name]++
				}
			}
		case *ast.UnaryExpr:
			if name, ok := node.X.(*ast.Ident); ok && node.Op == token.AND {
				sets[name.Name]++
			}
		case *ast.FuncType:
			for _, fields := range []*ast.FieldList{node.Params, node.Results} {
				if fields == nil {
					continue
				}
				for _, field := range fields.List {
					for _, name := range field.Names {
						sets[name.Name]++
					}
				}
			}
		}
		return true
	})
	if len(sets) == 0 {
		return constants
	}
	names := maps.Clone(constants)
	for name := range sets {
		delete(names, name)
	}
	for changed := true; changed; {
		changed = false
		for name, value := range values {
			if _, done := names[name]; done || sets[name] != 1 {
				continue
			}
			if text, whole, ok := statementText(value, names); ok && whole {
				names[name] = text
				changed = true
			}
		}
	}
	return names
}

// statementText renders expression as the text of a statement: a string literal; a `+` chain of
// literals and constants; strings.Join of a list of them; or fmt.Sprintf of one, each verb's
// value a part it cannot read. A part it cannot read is written ‹?›. ok is whether it holds a
// string at all, and whole whether it holds no gap.
func statementText(expression ast.Expr, constants map[string]string) (text string, whole, ok bool) {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind != token.STRING {
			return "", false, false
		}
		value, err := strconv.Unquote(expression.Value)
		if err != nil {
			return "", false, false
		}
		return value, true, true
	case *ast.ParenExpr:
		return statementText(expression.X, constants)
	case *ast.Ident:
		value, found := constants[expression.Name]
		return value, found, found
	case *ast.BinaryExpr:
		if expression.Op != token.ADD {
			return "", false, false
		}
		left, leftWhole, leftOK := statementText(expression.X, constants)
		right, rightWhole, rightOK := statementText(expression.Y, constants)
		if !leftOK && !rightOK {
			return "", false, false
		}
		if !leftOK {
			left = "‹?›"
		}
		if !rightOK {
			right = "‹?›"
		}
		return left + right, leftWhole && rightWhole, true
	case *ast.CallExpr:
		function, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", false, false
		}
		pkg, ok := function.X.(*ast.Ident)
		if !ok {
			return "", false, false
		}
		switch {
		case pkg.Name == "fmt" && function.Sel.Name == "Sprintf" && len(expression.Args) > 0:
			format, _, ok := statementText(expression.Args[0], constants)
			if !ok {
				return "", false, false
			}
			verbs := 0
			text := sprintfVerb.ReplaceAllStringFunc(format, func(verb string) string {
				if verb == "%%" {
					return "%"
				}
				verbs++
				return "‹?›"
			})
			return text, verbs == 0, true
		case pkg.Name == "strings" && function.Sel.Name == "Join" && len(expression.Args) == 2:
			list, ok := expression.Args[0].(*ast.CompositeLit)
			if !ok {
				return "", false, false
			}
			separator, whole, ok := statementText(expression.Args[1], constants)
			if !ok {
				separator = "‹?›"
			}
			parts := make([]string, len(list.Elts))
			for i, element := range list.Elts {
				part, partWhole, partOK := statementText(element, constants)
				if !partOK {
					part = "‹?›"
				}
				parts[i] = part
				whole = whole && partWhole
			}
			return strings.Join(parts, separator), whole, true
		}
	}
	return "", false, false
}

// sprintfVerb is a verb of a fmt format, with its flags, width, precision and argument index.
var sprintfVerb = regexp.MustCompile(`%%|%(\[[0-9]+\])?[-+# 0]*(\*|[0-9]+)?(\.(\*|[0-9]+)?)?(\[[0-9]+\])?[a-zA-Z]`)

// lockReadShape is statement as the check reads it: lowercased; its comments, the text inside its
// quoted literals (the quotes stay, so '%pg_locks%' reads as two quotes) and the double quotes of
// its identifiers taken out; pg_catalog's qualifier dropped; and its whitespace collapsed. So a
// view named "pg_locks", after a comment, or as pg_catalog.pg_locks reads as pg_locks, and a word
// inside a quoted literal or a gap inside one reads as nothing.
func lockReadShape(statement string) string {
	text := strings.ToLower(statement)
	var shape strings.Builder
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\'':
			shape.WriteString("''")
			for i++; i < len(text); i++ {
				if text[i] == '\'' {
					if i+1 < len(text) && text[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case text[i] == '"':
		case strings.HasPrefix(text[i:], "--"):
			for i < len(text) && text[i] != '\n' {
				i++
			}
			shape.WriteByte(' ')
		case strings.HasPrefix(text[i:], "/*"):
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				i = len(text)
			} else {
				i += 2 + end + 1
			}
			shape.WriteByte(' ')
		default:
			shape.WriteByte(text[i])
		}
	}
	return catalogQualifier.ReplaceAllString(strings.Join(strings.Fields(shape.String()), " "), "")
}

// catalogQualifier is pg_catalog's qualifier of a name.
var catalogQualifier = regexp.MustCompile(`\bpg_catalog\s*\.\s*`)

// lockReadKeepsToItsOwn is whether every query block of shape that reads a view (lockViewRead)
// keeps to its database or a backend of its own, as TestEveryLockReadOfATestKeepsToItsOwnDatabase
// says.
func lockReadKeepsToItsOwn(shape string) bool {
	statement := parseLockRead(shape)
	for _, match := range lockViewRead.FindAllStringSubmatchIndex(shape, -1) {
		view := -1
		for group := 1; group < len(match)/2; group++ {
			if match[2*group] >= 0 {
				view = match[2*group]
				break
			}
		}
		if !statement.keepsToItsOwn(statement.blockOf(view), 0) {
			return false
		}
	}
	return true
}

// lockReadStatement is a statement's shape with its parentheses matched: the innermost group each
// byte lies in, group 0 being the statement itself, and each group's parentheses and the group
// around it. A parenthesis belongs to the group around it.
type lockReadStatement struct {
	shape     string
	innermost []int
	groups    []lockReadGroup
}

// lockReadGroup is one pair of parentheses: shape[open] is "(" (-1 for the statement) and
// shape[close] its ")" (len(shape) for the statement, or for one never closed).
type lockReadGroup struct {
	open, close, parent int
}

// lockReadBlock is one query block: shape[from:to], the part of group's inside between its set
// operations (union, intersect, except).
type lockReadBlock struct {
	group, from, to int
}

func parseLockRead(shape string) lockReadStatement {
	statement := lockReadStatement{shape: shape, innermost: make([]int, len(shape)), groups: []lockReadGroup{{open: -1, close: len(shape), parent: -1}}}
	current := 0
	for i := range len(shape) {
		statement.innermost[i] = current
		switch shape[i] {
		case '(':
			statement.groups = append(statement.groups, lockReadGroup{open: i, close: len(shape), parent: current})
			current = len(statement.groups) - 1
		case ')':
			if current != 0 {
				statement.groups[current].close = i
				current = statement.groups[current].parent
				statement.innermost[i] = current
			}
		}
	}
	return statement
}

// level is group's own text in shape[from:to], each nested group's inside left out so its
// parentheses read (), and where each of its bytes lies in shape.
func (s lockReadStatement) level(group, from, to int) (string, []int) {
	var text strings.Builder
	var at []int
	for i := from; i < to; i++ {
		if s.innermost[i] == group {
			text.WriteByte(s.shape[i])
			at = append(at, i)
		}
	}
	return text.String(), at
}

// blockOf is the query block the byte at shape[at] lies in.
func (s lockReadStatement) blockOf(at int) lockReadBlock {
	group := s.innermost[at]
	block := lockReadBlock{group: group, from: s.groups[group].open + 1, to: s.groups[group].close}
	text, positions := s.level(group, block.from, block.to)
	for _, operation := range lockReadSetOperation.FindAllStringIndex(text, -1) {
		if positions[operation[0]] > at {
			block.to = positions[operation[0]]
			break
		}
		block.from = positions[operation[1]-1] + 1
	}
	return block
}

// blocks is every query block of group.
func (s lockReadStatement) blocks(group int) []lockReadBlock {
	from, to := s.groups[group].open+1, s.groups[group].close
	text, positions := s.level(group, from, to)
	var blocks []lockReadBlock
	for _, operation := range lockReadSetOperation.FindAllStringIndex(text, -1) {
		blocks = append(blocks, lockReadBlock{group: group, from: from, to: positions[operation[0]]})
		from = positions[operation[1]-1] + 1
	}
	return append(blocks, lockReadBlock{group: group, from: from, to: to})
}

// condition is the query block whose filter the condition starting at shape[at] is part of, and
// whether it is: it follows where, and or on at that block's own level, with no or and no gap at
// that level, or it opens parentheses that do so, with no or and no gap inside them at their own
// level.
func (s lockReadStatement) condition(at int) (lockReadBlock, bool) {
	for {
		block := s.blockOf(at)
		text, positions := s.level(block.group, block.from, block.to)
		if lockReadUndone.MatchString(text) {
			return block, false
		}
		before := strings.TrimSpace(text[:countBelow(positions, at)])
		switch {
		case lockReadFilter.MatchString(before):
			return block, true
		case before == "" && block.group != 0 && !lockReadSubquery.MatchString(text):
			at = s.groups[block.group].open
		default:
			return block, false
		}
	}
}

// countBelow is how many of positions, which rise, are below at.
func countBelow(positions []int, at int) int {
	n, _ := slices.BinarySearch(positions, at)
	return n
}

// keepsToItsOwn is whether block keeps to its database or a backend of its own: by a condition of
// its own (lockReadScope), or, for a subquery, by its rows' process ids tied to a block around it
// that keeps to its own, as a recursive step from a block beside it that does, or as process ids
// taken out of the block around it. depth bounds the walk to the blocks around it.
func (s lockReadStatement) keepsToItsOwn(block lockReadBlock, depth int) bool {
	if s.conditionOf(block, lockReadScope) {
		return true
	}
	if block.group == 0 || depth > len(s.groups) {
		return false
	}
	open := s.groups[block.group].open
	if lockReadExclusion.MatchString(s.shape[:open]) {
		return true
	}
	if s.conditionOf(block, lockReadCorrelation) && s.keepsToItsOwn(s.blockOf(open), depth+1) {
		return true
	}
	if s.conditionOf(block, lockReadBlockingStep) {
		for _, beside := range s.blocks(block.group) {
			if beside != block && s.conditionOf(beside, lockReadScope) {
				return true
			}
		}
	}
	return false
}

// conditionOf is whether a condition of block's own filter matches pattern.
func (s lockReadStatement) conditionOf(block lockReadBlock, pattern *regexp.Regexp) bool {
	for _, match := range pattern.FindAllStringIndex(s.shape, -1) {
		if of, ok := s.condition(match[0]); ok && of == block {
			return true
		}
	}
	return false
}
