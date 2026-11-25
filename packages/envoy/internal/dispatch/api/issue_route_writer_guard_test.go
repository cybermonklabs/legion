//go:build !race

// The guard is static, as lock_level_test.go's is and for the reason given there: it reads the
// repository's source, so the race detector adds nothing to it.

package api

//ci:reads packages/envoy/**/*.go !**/*_test.go `packages/envoy/internal/dispatch/api/module_source_test.go#scanModuleSources` walks packages/envoy (infra, testdata and all) and parses every .go file it finds but the test files, for the tests that ask it for the module's sources: `packages/envoy/internal/dispatch/api/lock_level_test.go#TestNoForUpdateOnOwnerTables`, `packages/envoy/internal/dispatch/api/event_insert_guard_test.go#TestOnlyTheBrokerInsertsEvents`, `packages/envoy/internal/dispatch/api/review_order_guard_test.go#TestEveryReviewReadOrdersByReviewOrder`, `packages/envoy/internal/dispatch/api/review_write_guard_test.go#TestOnlyQueueReviewWritesReviews`, `packages/envoy/internal/dispatch/api/one_expression_guard_test.go#TestOneExpressionDecidesWhoWaitsInOneSpelling`, `packages/envoy/internal/dispatch/api/issue_route_writer_guard_test.go#TestOnlyTheIssuePatchWritesTheRoute`; that last also walks packages/envoy and reads every non-test file of it as text

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// issueRouteWriter is the file whose update of an issue writes its route: the PATCH of an issue,
// which names the column through its own set("route", ...) and appends issue.updated in the same
// transaction.
const issueRouteWriter = "internal/dispatch/api/issue_patch.go"

// routeWriter is a file that writes an issue's route in a statement of its own, how many such
// statements it holds, and why its write moves no earlier event's copies.
type routeWriter struct {
	statements int
	why        string
}

// otherRouteWriters are the files besides the PATCH that write an issue's route, each as an
// insert of a new issue that appends issue.created in the same transaction. A new issue has no
// earlier event whose record a later mention's copy could pass, so each keeps the guard's rule.
// Each is named with how many of its statements write the column, so a write moving out of the
// scan's reach, or a second one added, reds here.
var otherRouteWriters = map[string]routeWriter{
	"internal/dispatch/api/issue_create.go": {statements: 1, why: "the create of an issue takes a route and appends " +
		"issue.created in the same transaction; the new issue has no earlier event"},
	"going/eval/write.go": {statements: 1, why: "the eval's seed (WriteSeed) inserts each issue with its route and " +
		"its issue.created in one transaction, into a database no server has opened, so no earlier event of " +
		"the issue, and no record of one, exists"},
}

// otherRouteWriter is the entry of otherRouteWriters path names, if any: the file itself, by its
// path from the module's root, and no other file of the same name.
func otherRouteWriter(path string) (string, bool) {
	for name := range otherRouteWriters {
		if path == name || strings.HasSuffix(path, "/"+name) {
			return name, true
		}
	}
	return "", false
}

// routeWrite matches a statement, lower-cased with its whitespace collapsed, that writes the route
// column of Dispatch's issues table: an update that sets it, an insert or a copy that lists it.
var routeWrite = []*regexp.Regexp{
	regexp.MustCompile(`\bupdate (only )?(public\.)?issues\b( (as )?\w+)? set\b[^;]{0,400}?\broute ?=`),
	regexp.MustCompile(`\binsert into (public\.)?issues ?\([^)]*\broute\b`),
	regexp.MustCompile(`\bcopy (public\.)?issues ?\([^)]*\broute\b`),
}

func writesRoute(statement string) bool {
	for _, pattern := range routeWrite {
		if pattern.MatchString(statement) {
			return true
		}
	}
	return false
}

// routeSetCalls counts the calls in file of a function named set whose first argument is the
// string "route": the way the PATCH names the column in the update it builds.
func routeSetCalls(file *ast.File) int {
	calls := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "set" {
			return true
		}
		if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			if value, err := strconv.Unquote(literal.Value); err == nil && value == "route" {
				calls++
			}
		}
		return true
	})
	return calls
}

// routeScanSkipped says which files of the module the text scan leaves out, and why: tests write
// rows directly on purpose, a fixture's or a red proof's; and documents and other data hold no
// writer.
func routeScanSkipped(path string, entry fs.DirEntry) bool {
	if entry.IsDir() {
		return entry.Name() == "node_modules"
	}
	if strings.HasSuffix(path, "_test.go") || strings.Contains(entry.Name(), ".test.") {
		return true
	}
	switch filepath.Ext(path) {
	case ".sql", ".sh", ".py", ".ts", ".js", ".go":
		return false
	}
	return true
}

// TestOnlyTheIssuePatchWritesTheRoute enforces what the order of a mention's copy behind an
// earlier event of its issue rests on when the issue's route moves: every write of an issue's
// route appends an event of that issue in its transaction. Appending it moves the issue's last_seq
// past the owed_seq of every earlier record, so each earlier unpublished event holds every copy of
// a later comment until a batch that read the move records it again, the new route among what it
// owes (owedAgain in internal/dispatch/outbox) - whatever record publishes the move's own event,
// one made after its process lost the publisher hold or one of the previous release among them.
// A route written with no event moves no last_seq, so an earlier event's record recorded after
// the comment keeps naming the old route's session, and a later comment's copy for the new one
// passes it (TestARouteChangeKeepsAMentionCopyBehindAnEarlierEvent's control row).
//
// The PATCH of an issue is the one writer that moves a route (issue_patch.go): it builds its update
// from set("route", ...) and appends issue.updated in the same transaction. Besides it, the files
// of otherRouteWriters write a route as they insert a new issue, and append its issue.created in
// the same transaction, each named with its reason and with how many of its statements write the
// column. The guard holds them to being the only ones in two scans of the envoy module, which holds
// Dispatch's server, its migrations and the commands, scripts and seeds that write its database.
// Its Go source, as the module scan assembles each statement (TestOnlyTheBrokerInsertsEvents): no
// statement of any other file may write the column, each named file holds its count of them, and
// exactly one set("route", ...) call may exist, in issue_patch.go. And every other file of it -
// SQL, shell, Python, TypeScript, JavaScript and Go read as written, migrations, seeds and scripts
// included: no file but those may write it either (routeScanSkipped names what it leaves out).
// Outside the module, the Legion daemon's issues table is its own database's and has no route, and
// the web app's end-to-end fixtures seed a fresh database whose issues have no event yet. Another
// writer belongs behind the same rule: give it its event, then name it here.
func TestOnlyTheIssuePatchWritesTheRoute(t *testing.T) {
	t.Parallel()
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	setCalls := map[string]int{}
	named := map[string]int{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlStatements(source.file, constants) {
			if !writesRoute(statement) {
				continue
			}
			if name, ok := otherRouteWriter(path); ok {
				named[name]++
				continue
			}
			offences = append(offences, path+": "+statement)
		}
		if calls := routeSetCalls(source.file); calls > 0 {
			setCalls[path] += calls
		}
	}
	scanned := 0
	if err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if routeScanSkipped(path, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if writesRoute(sqlWhitespace.ReplaceAllString(strings.ToLower(string(text)), " ")) {
			if _, ok := otherRouteWriter(filepath.ToSlash(path)); !ok {
				offences = append(offences, filepath.ToSlash(path))
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan the module: %v", err)
	}
	if len(offences) > 0 {
		t.Fatalf("only %s may write an issue's route, and these write it too:\n\t%s\n"+
			"Append an event of the issue in the same transaction, as the PATCH appends issue.updated, "+
			"and name the writer in this guard: a route written with no event lets a later mention's "+
			"copy pass an earlier event whose record names the old route.", issueRouteWriter, strings.Join(offences, "\n\t"))
	}
	var patch int
	for path, calls := range setCalls {
		if strings.HasSuffix(path, issueRouteWriter) {
			patch += calls
			continue
		}
		t.Errorf("%s names the route column in %d set(\"route\", ...) call(s); only %s may write it", path, calls, issueRouteWriter)
	}
	if patch != 1 {
		t.Fatalf("%s holds %d set(\"route\", ...) calls the scan can see, want 1: if the write moved or "+
			"was renamed, this guard now passes by seeing nothing", issueRouteWriter, patch)
	}
	for name, writer := range otherRouteWriters {
		if named[name] != writer.statements {
			t.Errorf("%s holds %d statements the scan sees write the route, want %d (%s): if one moved or was "+
				"written around a value this scan cannot resolve, this guard now passes by seeing nothing",
				name, named[name], writer.statements, writer.why)
		}
	}
	if scanned < 500 {
		t.Fatalf("the module scan read %d files, want the module: %s is not its root", scanned, moduleRoot)
	}
	t.Logf("%d Go files of the module parsed and %d of its files read as text; the writers are %s and %d named others",
		len(sources), scanned, issueRouteWriter, len(otherRouteWriters))
}

// TestTheRouteWriterGuardSeesAPlantedWriter plants a second writer of the route in each shape the
// guard reads and checks it is seen, and that what it must not count is not: an update of another
// column, a read of the route, and a test file, which writes rows on purpose.
func TestTheRouteWriterGuardSeesAPlantedWriter(t *testing.T) {
	t.Parallel()
	planted := `package planted

func move(tx tx, key string) error {
	_, err := tx.Exec(ctx, ` + "`" + `
		UPDATE issues
		   SET route = $2, updated_at = now()
		 WHERE key = $1` + "`" + `, key, "session:s1")
	set("route", "session:s1")
	return err
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	statements := sqlStatements(file, map[string]string{})
	if !slices.ContainsFunc(statements, writesRoute) {
		t.Fatalf("the planted Go update reads as %q, none of which the guard counts as a route write", statements)
	}
	if calls := routeSetCalls(file); calls != 1 {
		t.Fatalf("the planted file holds %d set(\"route\", ...) calls the guard sees, want 1", calls)
	}
	for _, text := range []string{
		"insert into issues (key, project_key, route) values ('A-1', 'A', 'session:s1');",
		"COPY issues (key, route) FROM STDIN;",
		"update issues i set title = 'x', route = 'role:r' where i.key = 'A-1';",
		"update issues as i set route = $2 where i.key = $1;",
		"update only issues set route = $2 where key = $1;",
	} {
		if !writesRoute(sqlWhitespace.ReplaceAllString(strings.ToLower(text), " ")) {
			t.Errorf("the guard does not count %q as a route write", text)
		}
	}
	for _, text := range []string{
		"update issues set title = $2 where key = $1;",
		"select route from issues where key = $1;",
		"update issues set claimed_by = $2 where key = $1; select route from issues;",
	} {
		if writesRoute(sqlWhitespace.ReplaceAllString(strings.ToLower(text), " ")) {
			t.Errorf("the guard counts %q as a route write", text)
		}
	}
	if !routeScanSkipped("route_change_test.go", fakeFile("route_change_test.go")) ||
		routeScanSkipped("issue_patch.go", fakeFile("issue_patch.go")) ||
		routeScanSkipped("20261117190118_events_owed_keys.up.sql", fakeFile("20261117190118_events_owed_keys.up.sql")) {
		t.Error("the text scan reads a test file or leaves out a source or a migration")
	}
	// A named writer is let through by its own path alone: a planted writer elsewhere, a file of
	// the same name in another directory included, is an offence.
	if _, ok := otherRouteWriter("../../../going/eval/write.go"); !ok {
		t.Error("the guard does not know the eval seed's writer by its path")
	}
	for _, path := range []string{"../../../internal/dispatch/api/planted.go", "../../../cmd/dispatch-seed/write.go", "../../../notgoing/eval/write.go"} {
		if name, ok := otherRouteWriter(path); ok {
			t.Errorf("the guard lets %s through as the named writer %s", path, name)
		}
	}
}

// fakeFile is a file entry named name, for routeScanSkipped.
type fakeFile string

func (f fakeFile) Name() string             { return string(f) }
func (fakeFile) IsDir() bool                { return false }
func (fakeFile) Type() fs.FileMode          { return 0 }
func (fakeFile) Info() (fs.FileInfo, error) { return nil, nil }
