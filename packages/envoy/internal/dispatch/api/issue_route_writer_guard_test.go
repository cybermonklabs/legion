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

// issueRouteWriter is the one source file allowed to write an issue's route: the PATCH of an issue,
// which names the column through its own set("route", ...) and appends issue.updated in the same
// transaction.
const issueRouteWriter = "internal/dispatch/api/issue_patch.go"

// issuesColumnWrites matches a statement, lower-cased with its whitespace collapsed, that writes
// a column of Dispatch's issues table that columns names: an update that sets it, an insert or a
// copy that lists it. columns is one column's name, or a group of names such as (status|labels).
func issuesColumnWrites(columns string) []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`\bupdate (only )?(public\.)?issues\b( (as )?\w+)? set\b[^;]{0,400}?\b` + columns + ` ?=`),
		regexp.MustCompile(`\binsert into (public\.)?issues ?\([^)]*\b` + columns + `\b`),
		regexp.MustCompile(`\bcopy (public\.)?issues ?\([^)]*\b` + columns + `\b`),
	}
}

// routeWrite matches a statement that writes the route column of Dispatch's issues table
// (issuesColumnWrites).
var routeWrite = issuesColumnWrites("route")

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
		if value, ok := goStringLiteral(call.Args[0]); ok && value == "route" {
			calls++
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
// The PATCH of an issue is that one writer (issue_patch.go): it builds its update from set("route",
// ...) and appends issue.updated in the same transaction. The guard holds it to being the only
// one in two scans of the envoy module, which holds Dispatch's server, its migrations and the
// commands, scripts and seeds that write its database. Its Go source, as the module scan assembles
// each statement (TestOnlyTheBrokerInsertsEvents): no statement there may write the column, and
// exactly one set("route", ...) call may exist, in that file. And every other file of it - SQL,
// shell, Python, TypeScript, JavaScript and Go read as written, migrations, seeds and scripts
// included: no file may write it either (routeScanSkipped names what it leaves out). Outside the
// module, the Legion daemon's issues table is its own database's and has no route, and the web
// app's end-to-end fixtures seed a fresh database whose issues have no event yet. A second writer
// belongs behind the same rule: give it its event, then name it here.
func TestOnlyTheIssuePatchWritesTheRoute(t *testing.T) {
	t.Parallel()
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	setCalls := map[string]int{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlStatements(source.file, constants) {
			if writesRoute(statement) {
				offences = append(offences, path+": "+statement)
			}
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
			offences = append(offences, filepath.ToSlash(path))
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
	if scanned < 500 {
		t.Fatalf("the module scan read %d files, want the module: %s is not its root", scanned, moduleRoot)
	}
	t.Logf("%d Go files of the module parsed and %d of its files read as text; the one writer is %s", len(sources), scanned, issueRouteWriter)
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
}

// fakeFile is a file entry named name, for routeScanSkipped.
type fakeFile string

func (f fakeFile) Name() string             { return string(f) }
func (fakeFile) IsDir() bool                { return false }
func (fakeFile) Type() fs.FileMode          { return 0 }
func (fakeFile) Info() (fs.FileInfo, error) { return nil, nil }

// judgedIssueWriter is the source file whose writes of an issue's status, labels and parent R26
// of dispatch://INFRA-3320 judges: the PATCH's write core (writeIssue). It calls
// issueWritePlanCheck before its first write.
const judgedIssueWriter = "internal/dispatch/api/issue_patch.go"

// issueWritePlanCheck is the one check of a write's plan, which a judged writer calls before its
// first write. Its first statement is legionStatusGuard, and R24's checks come after it.
const issueWritePlanCheck = "refuseIssueWritePlan"

// issueWritePlanCheckSource is the source file that declares issueWritePlanCheck.
const issueWritePlanCheckSource = "internal/dispatch/api/parent_status_rule.go"

// legionStatusGuard is the function that judges a write of an issue's status, labels or parent
// for R26 (legion_status_scope.go).
const legionStatusGuard = "refuseLegionStatusWrite"

// statusLabelsOrParentWriters is every place of the module that may write an issue's status,
// labels or parent, each with the reason it may. A path that ends in a slash is a directory, and
// every Go file under it is that writer.
var statusLabelsOrParentWriters = []struct{ path, reason string }{
	{judgedIssueWriter, "the PATCH's write core, whose every write R26 judges: it calls " + issueWritePlanCheck + ", which opens with " + legionStatusGuard + ", before its first write"},
	{"internal/dispatch/api/issue_create.go", "the creation of an issue, which R26 never refuses: it writes the new issue's parent and labels, and no status"},
	{"cmd/dispatch-seed/", "the seeding tool, which fills an empty database before a server runs on it, and which no request reaches"},
	{"internal/dispatch/delivery/deliverytest/", "the delivery history fixture, which tests and the eval fixture loader (evals/evalfixture) write into a database for a check to count over, " +
		"and which no request reaches: no package of the server imports it"},
	{"internal/dispatch/evals/evalfixture/", "the eval checks' fixture loader, which tests, its command (cmd) and the bench loader (evals/evalbench) run against a database for a check to read, " +
		"and which no request reaches: no package of the server imports it"},
	{"internal/dispatch/legion/legiontest/", "the Legion history fixture, which tests and the two eval loaders (evals/evalfixture, evals/evalbench) write into a database for a check to count over, " +
		"and which no request reaches: no package of the server imports it"},
}

// statusLabelsOrParentWriter answers the index in statusLabelsOrParentWriters of the writer that
// path, a path from the module's root, belongs to, and false for a path of no such writer.
func statusLabelsOrParentWriter(path string) (int, bool) {
	for index, writer := range statusLabelsOrParentWriters {
		if path == writer.path || (strings.HasSuffix(writer.path, "/") && strings.HasPrefix(path, writer.path)) {
			return index, true
		}
	}
	return 0, false
}

// judgedIssueColumns is the three columns of Dispatch's issues table whose write R26 judges, as
// the alternation of a regular expression.
const judgedIssueColumns = "status|labels|parent_key"

// statusLabelsOrParentWrite matches a statement, lower-cased with its whitespace collapsed, that
// writes the status, labels or parent_key column of Dispatch's issues table: an update that sets
// one, an insert or a copy that lists one (issuesColumnWrites), and a fragment that assigns one,
// which is the shape of the pieces the PATCH builds its update from ("status = "+placeholder).
// The update's pattern also matches an update that only compares such a column after its set
// list. That can add a finding, and it never hides one.
var statusLabelsOrParentWrite = append(issuesColumnWrites("("+judgedIssueColumns+")"),
	regexp.MustCompile(`^ ?(`+judgedIssueColumns+`) = `))

func writesStatusLabelsOrParent(statement string) bool {
	return slices.ContainsFunc(statusLabelsOrParentWrite, func(pattern *regexp.Regexp) bool {
		return pattern.MatchString(statement)
	})
}

// judgedIssueColumn says whether column is one of the three columns whose write R26 judges
// (judgedIssueColumns).
func judgedIssueColumn(column string) bool {
	return slices.Contains(strings.Split(judgedIssueColumns, "|"), column)
}

// statusLabelsOrParentWrites names, one line each, every write of an issue's status, labels or
// parent that the scan finds in source. It reads three shapes:
//
//   - a statement that sqlStatements assembles (writesStatusLabelsOrParent);
//   - a call set("status" | "labels" | "parent_key", ...), the way the PATCH names a column of
//     the update it builds;
//   - a CopyFrom into pgx.Identifier{"issues"} whose column list names one of the three, the way
//     cmd/dispatch-seed writes its issues. A column list that is no literal the scan can read
//     counts as a write, because the scan cannot tell which columns it names.
func statusLabelsOrParentWrites(source moduleSource, constants map[string]string) []string {
	var found []string
	for _, statement := range sqlStatements(source.file, constants) {
		if writesStatusLabelsOrParent(statement) {
			found = append(found, statement)
		}
	}
	line := func(node ast.Node) string {
		return "line " + strconv.Itoa(source.positions.Position(node.Pos()).Line) + ": "
	}
	ast.Inspect(source.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if callee.Name != "set" || len(call.Args) == 0 {
				return true
			}
			if column, ok := goStringLiteral(call.Args[0]); ok && judgedIssueColumn(column) {
				found = append(found, line(call)+"set("+strconv.Quote(column)+", ...)")
			}
		case *ast.SelectorExpr:
			if callee.Sel.Name != "CopyFrom" || len(call.Args) < 3 || !namesTheIssuesTable(call.Args[1]) {
				return true
			}
			if columns, read := goStringLiterals(call.Args[2]); !read || slices.ContainsFunc(columns, judgedIssueColumn) {
				found = append(found, line(call)+"CopyFrom into issues")
			}
		}
		return true
	})
	return found
}

// goStringLiteral is the value of expression when it is a string literal.
func goStringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

// goStringLiterals is the values of expression when it is a composite literal of string literals
// alone, such as []string{"key", "status"}.
func goStringLiterals(expression ast.Expr) ([]string, bool) {
	composite, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	values := make([]string, 0, len(composite.Elts))
	for _, element := range composite.Elts {
		value, ok := goStringLiteral(element)
		if !ok {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

// namesTheIssuesTable says whether expression is an identifier of the issues table:
// pgx.Identifier{"issues"}, or the same with a schema before the table's name.
func namesTheIssuesTable(expression ast.Expr) bool {
	composite, ok := expression.(*ast.CompositeLit)
	if !ok {
		return false
	}
	if kind, ok := composite.Type.(*ast.SelectorExpr); !ok || kind.Sel.Name != "Identifier" {
		return false
	}
	names, ok := goStringLiterals(composite)
	return ok && len(names) > 0 && names[len(names)-1] == "issues"
}

// calledName is the name of the function or the method that call calls, or the empty string for
// a call of an expression of another shape.
func calledName(call *ast.CallExpr) string {
	switch callee := call.Fun.(type) {
	case *ast.Ident:
		return callee.Name
	case *ast.SelectorExpr:
		return callee.Sel.Name
	}
	return ""
}

// callsFunction says whether file calls the function name, as a function or as a method.
func callsFunction(file *ast.File, name string) bool {
	called := false
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && calledName(call) == name {
			called = true
		}
		return !called
	})
	return called
}

// legionStatusGuardMissing says what is missing when the function check, which file declares,
// does not open with R26's guard in the shape that refuseIssueWritePlan writes:
//
//	if err := refuseLegionStatusWrite(...); err != nil {
//		return ..., err
//	}
//
// It answers the empty string when the function opens with the guard. A guard after another
// statement can come after a check that answers first. A guard that does not return its error
// refuses nothing.
func legionStatusGuardMissing(file *ast.File, check string) string {
	for _, declared := range file.Decls {
		function, ok := declared.(*ast.FuncDecl)
		if !ok || function.Name.Name != check || function.Body == nil {
			continue
		}
		if len(function.Body.List) == 0 || !isLegionStatusGuard(function.Body.List[0]) {
			return "the first statement of " + check + " is not R26's guard, if err := " + legionStatusGuard + "(...); err != nil { return ..., err }"
		}
		return ""
	}
	return "it declares no function " + check
}

// isLegionStatusGuard says whether statement is an if whose init assigns the result of a call of
// legionStatusGuard to one variable, whose condition is that variable != nil, and whose body
// returns that variable as its last result.
func isLegionStatusGuard(statement ast.Stmt) bool {
	named := func(expression ast.Expr, name string) bool {
		identifier, ok := expression.(*ast.Ident)
		return ok && identifier.Name == name
	}
	guard, ok := statement.(*ast.IfStmt)
	if !ok {
		return false
	}
	init, ok := guard.Init.(*ast.AssignStmt)
	if !ok || len(init.Lhs) != 1 || len(init.Rhs) != 1 {
		return false
	}
	refusal, ok := init.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	if call, ok := init.Rhs[0].(*ast.CallExpr); !ok || calledName(call) != legionStatusGuard {
		return false
	}
	condition, ok := guard.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ || !named(condition.X, refusal.Name) || !named(condition.Y, "nil") {
		return false
	}
	if len(guard.Body.List) != 1 {
		return false
	}
	answer, ok := guard.Body.List[0].(*ast.ReturnStmt)
	return ok && len(answer.Results) > 0 && named(answer.Results[len(answer.Results)-1], refusal.Name)
}

// TestOnlyTheJudgedWritersWriteAnIssuesStatusLabelsOrParent holds R26 to every write it judges.
// R26 refuses a write of a Legion issue's status, the removal of the legion label and a move of
// a Legion issue out from under every labelled issue, from a credential that holds neither the
// person scope nor the legion-status scope (legion_status_scope.go). The rule holds only while
// every write of an issue's status, labels or parent goes through its guard: a route or a job
// that writes one of the three columns past the guard writes a Legion issue's status for any
// caller.
//
// The guard reads every Go statement and call of the envoy module that writes one of the three
// columns, in the shapes statusLabelsOrParentWrites reads, and fails:
//
//   - for a write outside the writers statusLabelsOrParentWriters names, each with its reason;
//   - for a named writer in which the scan finds no write, because a scan that finds nothing
//     proves nothing;
//   - when the judged writer, issue_patch.go, does not call refuseIssueWritePlan;
//   - when the first statement of refuseIssueWritePlan (parent_status_rule.go) is not R26's
//     guard, refuseLegionStatusWrite, with its refusal returned.
//
// The scan reads the module's Go source alone, so it leaves out the migrations, which are SQL. A
// migration runs once, at the boot of the release that brings it, and no caller's request runs
// it, so it makes no call for R26 to judge. A new writer belongs behind the guard: make it call
// refuseIssueWritePlan before its first write, then name it here.
func TestOnlyTheJudgedWritersWriteAnIssuesStatusLabelsOrParent(t *testing.T) {
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	found := make([][]string, len(statusLabelsOrParentWriters))
	var offences []string
	judgedRead, checked := false, false
	checkRead, guardMissing := false, ""
	for _, source := range sources {
		path := strings.TrimPrefix(filepath.ToSlash(source.path), moduleRoot+"/")
		switch path {
		case judgedIssueWriter:
			judgedRead, checked = true, callsFunction(source.file, issueWritePlanCheck)
		case issueWritePlanCheckSource:
			checkRead, guardMissing = true, legionStatusGuardMissing(source.file, issueWritePlanCheck)
		}
		writes := statusLabelsOrParentWrites(source, constants)
		if len(writes) == 0 {
			continue
		}
		writer, allowed := statusLabelsOrParentWriter(path)
		for _, write := range writes {
			if allowed {
				found[writer] = append(found[writer], path+": "+write)
			} else {
				offences = append(offences, path+": "+write)
			}
		}
	}
	if len(offences) > 0 {
		t.Errorf("only the writers this guard names may write an issue's status, labels or parent, and these write one too:\n\t%s\n"+
			"Make each writer call %s before its first write, so that R26 judges it, and name it in statusLabelsOrParentWriters.",
			strings.Join(offences, "\n\t"), issueWritePlanCheck)
	}
	for index, writer := range statusLabelsOrParentWriters {
		if len(found[index]) == 0 {
			t.Errorf("the scan finds no write of an issue's status, labels or parent in %s, so it proves nothing there: "+
				"if the write moved or changed its shape, teach the scan to find it again", writer.path)
			continue
		}
		t.Logf("%s, %s:\n\t%s", writer.path, writer.reason, strings.Join(found[index], "\n\t"))
	}
	switch {
	case !judgedRead:
		t.Errorf("the scan did not read %s, the judged writer", judgedIssueWriter)
	case !checked:
		t.Errorf("%s writes an issue's status, labels and parent and does not call %s, so neither R26 nor R24 judges its writes",
			judgedIssueWriter, issueWritePlanCheck)
	}
	switch {
	case !checkRead:
		t.Errorf("the scan did not read %s, which declares %s", issueWritePlanCheckSource, issueWritePlanCheck)
	case guardMissing != "":
		t.Errorf("%s: %s, so R26 does not judge first each write that %s judges",
			issueWritePlanCheckSource, guardMissing, issueWritePlanCheck)
	}
}

// TestTheJudgedWriterGuardSeesAPlantedWriter plants a writer of each shape the guard reads and
// checks that the guard finds each. It checks that the guard finds nothing in code that writes
// none of the three columns: an update of another column, a read of the three, a write of
// another table, and a CopyFrom into issues of other columns. It checks that the guard tells a
// call of refuseIssueWritePlan from a mention of it, and a refuseIssueWritePlan that opens with
// R26's guard from one that does not. It checks that only the named writers' paths are allowed.
func TestTheJudgedWriterGuardSeesAPlantedWriter(t *testing.T) {
	parse := func(t *testing.T, declarations string) moduleSource {
		t.Helper()
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, "planted.go", "package planted\n\n"+declarations+"\n", 0)
		if err != nil {
			t.Fatalf("parse the planted source: %v", err)
		}
		return moduleSource{path: "planted.go", file: file, positions: positions}
	}
	plant := func(t *testing.T, body string) moduleSource {
		t.Helper()
		return parse(t, "func write() {\n"+body+"\n}")
	}
	for _, shape := range []struct{ name, body string }{
		{"an update that sets the status", `tx.Exec(ctx, "UPDATE issues SET status = $2 WHERE key = $1", key, status)`},
		{"an update of an aliased table that sets the labels", `tx.Exec(ctx, "update issues as i set title = $3, labels = $2 where i.key = $1", key, labels, title)`},
		{"an update of the schema's table that sets the parent", `tx.Exec(ctx, "update only public.issues set parent_key = $2 where key = $1", key, parent)`},
		{"an insert that lists the parent", `tx.Exec(ctx, "insert into issues (key, project_key, parent_key) values ($1, $2, $3)", key, project, parent)`},
		{"a copy that lists the labels", `tx.Exec(ctx, "COPY issues (key, labels) FROM STDIN")`},
		{"a CopyFrom whose columns name the status", `tx.CopyFrom(ctx, pgx.Identifier{"issues"}, []string{"key", "status"}, rows)`},
		{"a CopyFrom whose columns the scan cannot read", `tx.CopyFrom(ctx, pgx.Identifier{"public", "issues"}, columns, rows)`},
		{"a set call that names the parent", `set("parent_key", parent)`},
		{"a fragment of an update the code builds", `sets = append(sets, "labels = "+placeholder)`},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if found := statusLabelsOrParentWrites(plant(t, shape.body), map[string]string{}); len(found) != 1 {
				t.Errorf("the guard finds %q in the planted writer, want one write", found)
			}
		})
	}
	quiet := plant(t, strings.Join([]string{
		`tx.Exec(ctx, "update issues set claimed_by = $2 where key = $1", key, session)`,
		`tx.QueryRow(ctx, "select status, labels, parent_key from issues where key = $1", key)`,
		`tx.Exec(ctx, "update request_keys set status = $4 where login = $1", login)`,
		`tx.Exec(ctx, "insert into issue_links (issue_key, kind, target_key) values ($1, $2, $3)", key, kind, target)`,
		`tx.CopyFrom(ctx, pgx.Identifier{"issue_links"}, []string{"issue_key", "status"}, rows)`,
		`tx.CopyFrom(ctx, pgx.Identifier{"issues"}, []string{"key", "title"}, rows)`,
		`set("title", title)`,
		`get("/api/v1/issues?status=todo&labels=legion")`,
	}, "\n"))
	if found := statusLabelsOrParentWrites(quiet, map[string]string{}); len(found) > 0 {
		t.Errorf("the guard counts %q as writes of an issue's status, labels or parent", found)
	}
	for body, want := range map[string]bool{
		`s.refuseIssueWritePlan(ctx, tx, call.writer, plan)`:      true,
		`refuseIssueWritePlan(ctx, tx, writer, plan)`:             true,
		`log("refuseIssueWritePlan")`:                             false,
		`judge := s.refuseIssueWritePlan; _ = judge`:              false,
		`s.refuseIssueWritePlanLater(ctx, tx, call.writer, plan)`: false,
		`if _, err := s.refuseIssueWritePlan(); err != nil {}`:    true,
	} {
		if got := callsFunction(plant(t, body).file, issueWritePlanCheck); got != want {
			t.Errorf("the guard reads %q as a call of %s: %t, want %t", body, issueWritePlanCheck, got, want)
		}
	}
	const guard = "if err := refuseLegionStatusWrite(ctx, q, on, writer, plan); err != nil { return nil, err }"
	for _, check := range []struct {
		name, declarations string
		opens              bool
	}{
		{"the guard first", "func (s *server) refuseIssueWritePlan() {\n" + guard + "\nreturn judge(plan)\n}", true},
		{"the guard after another check", "func (s *server) refuseIssueWritePlan() {\nif refused { return nil, refusal }\n" + guard + "\n}", false},
		{"the guard as a statement with no if", "func (s *server) refuseIssueWritePlan() {\n_ = refuseLegionStatusWrite(ctx, q, on, writer, plan)\n}", false},
		{"the guard's refusal dropped", "func (s *server) refuseIssueWritePlan() {\nif err := refuseLegionStatusWrite(ctx, q, on, writer, plan); err != nil { log(err) }\n}", false},
		{"another error returned", "func (s *server) refuseIssueWritePlan() {\nif err := refuseLegionStatusWrite(ctx, q, on, writer, plan); err != nil { return nil, other }\n}", false},
		{"another function called first", "func (s *server) refuseIssueWritePlan() {\nif err := refuseLegionStatusWriteLater(ctx, q, on, writer, plan); err != nil { return nil, err }\n}", false},
		{"an empty body", "func (s *server) refuseIssueWritePlan() {}", false},
		{"the guard first in another function", "func (s *server) refuseIssueWritePlanLater() {\n" + guard + "\n}", false},
	} {
		t.Run(check.name, func(t *testing.T) {
			missing := legionStatusGuardMissing(parse(t, check.declarations).file, issueWritePlanCheck)
			if opens := missing == ""; opens != check.opens {
				t.Errorf("the guard reads the planted %s as opening with %s: %t (%q), want %t",
					issueWritePlanCheck, legionStatusGuard, opens, missing, check.opens)
			}
		})
	}
	for path, want := range map[string]bool{
		judgedIssueWriter:                           true,
		"internal/dispatch/api/issue_create.go":     true,
		"cmd/dispatch-seed/write_core.go":           true,
		"internal/dispatch/api/issue_claim.go":      false,
		"internal/dispatch/api/issue_patch_bulk.go": false,
		"cmd/dispatch-seeder/main.go":               false,
	} {
		if _, allowed := statusLabelsOrParentWriter(path); allowed != want {
			t.Errorf("the guard allows %s to write an issue's status, labels or parent: %t, want %t", path, allowed, want)
		}
	}
}
