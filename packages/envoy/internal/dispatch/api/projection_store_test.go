package api

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus/censustest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// projectedTables are the tables migration 20261109022248 made projections of the node and
// tuple store: each row of them names the node it is the projection of, and that node is
// written by the same transaction, before the row. They are the tables
// censustest.ProjectedColumns lists, in order, so a table added there is one this test holds.
var projectedTables = slices.Sorted(maps.Keys(censustest.ProjectedColumns))

// TestNoProjectionRowIsWrittenWithoutTheStore holds every writer of a projected table to the
// one rule the projections rest on: a row of one of them is written only by a transaction that
// writes the row's node in the store first, and the two commit together or not at all.
//
// The source scan is coverage - it names every statement that writes a projected table, so a new
// writer cannot arrive unnoticed, refuses one whose table is built at runtime at a site the test
// does not name (runtimeTableSites), and holds each one that makes or unmakes a row or sets a
// column the store states (projectedColumns, which holds every column of a projected table the
// census compares, as graphcensus's TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds
// requires) to the store writer's call path - and the Postgres halves are the proof: a
// failure injected between the two writes leaves neither side, a row whose node was never
// written is refused inside the statement that writes it, the real write paths leave every row
// they insert with its node and its tuples from the transaction that inserted the row, and an
// update of a column a tuple projects carries the transaction of the tuple it appended.
func TestNoProjectionRowIsWrittenWithoutTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sources := readProjectionWriters(t)
	t.Run("every write of a projected row or a projected column sits on the store writer's call path", func(t *testing.T) {
		requireEveryWriteIsOnTheStorePath(t, sources)
	})
	t.Run("no migration writes a projected table", func(t *testing.T) {
		requireNoMigrationWritesAProjection(t)
	})

	run := newProjectionRun(t)
	t.Run("no stored function or trigger writes a projected table", func(t *testing.T) {
		requireNoStoredFunctionWritesAProjection(t, ctx, run)
	})
	t.Run("no projected table carries a deferred check", func(t *testing.T) {
		requireNoDeferredCheckOnAProjection(t, ctx, run)
	})
	t.Run("a row naming a node the store does not hold is refused inside the statement", func(t *testing.T) {
		requireTheForeignKeyRefusesInsideTheStatement(t, ctx, run)
	})
	t.Run("every statement of a write runs in the node write's own transaction", func(t *testing.T) {
		requireOneTransactionPerWrite(t, ctx, run)
	})
	t.Run("a failure between the two writes leaves neither side", func(t *testing.T) {
		requireNeitherSideSurvivesAFailure(t, ctx, run)
	})
	// The rows are read before any step of the run updates one: an update moves a row's xmin to
	// its own transaction, which the row's node need not share, so the reparent below comes after.
	t.Run("the real write paths leave every row they insert with its node and its tuples", func(t *testing.T) {
		requireEveryRowHasItsNodeAndTuples(t, ctx, run)
	})
	t.Run("a reparent's row carries the transaction of the part_of tuple it appended", func(t *testing.T) {
		requireAReparentCarriesItsTuplesTransaction(t, ctx, run)
	})
	t.Run("no kind outside the list this change moves reached the store", func(t *testing.T) {
		requireOnlyTheMovedKindsReachedTheStore(t, ctx, run)
	})
	t.Run("two writers over one node commit one after the other", func(t *testing.T) {
		requireTwoWritersOverOneNodeCommitInTurn(t, ctx, run)
	})
}

// --- the source scan --------------------------------------------------------------------

// projectionWriteHead matches the head of every statement that writes a projected table
// (projectedTables): an insert or a delete, which makes or unmakes a row, and an update, which
// changes columns of a row that is already there.
var projectionWriteHead = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s+(?:only\s+)?(` +
	literalAlternation(projectedTables) + `)\b`)

// literalAlternation is a regular expression matching any one of words, each read literally.
func literalAlternation(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = regexp.QuoteMeta(word)
	}
	return strings.Join(quoted, "|")
}

// projectedColumns are the columns of each projected table that the store states, each with
// what states it: censustest.ProjectedColumns, which says why each one stands there, and which
// graphcensus's TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds holds to every
// column of a projected table the census compares.
var projectedColumns = censustest.ProjectedColumns

// anchorKeysOutsideTheTuple are the keys of a comment's anchor that its anchored_to tuple does
// not state: where in the artifact the anchor sits and whether it still finds its text. A
// jsonb_set of these alone sets no projected column.
var anchorKeysOutsideTheTuple = []string{"block_id", "orphaned", "quote"}

// updateSet matches what follows an update's table up to its assignments: the alias the
// statement may give the table, and set.
var updateSet = regexp.MustCompile(`(?is)^\s+(?:(?:as\s+)?\w+\s+)?set\b`)

// setListEnd matches the words an update's set list ends at, where they stand outside
// parentheses and quotes.
var setListEnd = regexp.MustCompile(`(?i)\b(?:from|where|returning)\b`)

// assignedColumn matches a column an assignment names. anchorColumn and anchorKeyPath match the
// first two arguments of a jsonb_set anchorKeys reads: the anchor itself, and the path of one of
// its keys.
var (
	assignedColumn = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)
	anchorColumn   = regexp.MustCompile(`(?i)^(?:\w+\.)?anchor$`)
	anchorKeyPath  = regexp.MustCompile(`(?i)^'\{\s*"?([a-z_][a-z0-9_]*)"?\s*\}'(?:\s*::\s*text\s*\[\s*\])?$`)
)

// storeWriters are the store's own writers as a call names them: every function of graphstore a
// write of graph_nodes and graph_tuples from outside it goes through
// (graphstoretest.WriteFunctions), the one list graphstore's register of the store's write sites
// reads too (TestEveryStoreWriteCallSiteIsListed). Among them are the node writes a projection row
// follows (Write, QueueWrite, QueueNodes) and the append of what a write stated under its event
// (Caused, QueueCaused), which gives a link's tuple its legacy_source_seq.
var storeWriters = func() []string {
	var calls []string
	for _, name := range graphstoretest.WriteFunctions() {
		calls = append(calls, "func:graphstore."+name)
	}
	return calls
}()

// projectionWrite is one statement of the sources that writes a projected table. held marks one
// the store writer's call path must hold: an insert, a delete, or an update that sets a column a
// tuple projects.
type projectionWrite struct {
	table  string
	file   string
	line   int
	clause string
	inside string
	held   bool
}

// sourceFunction is one function or method of the repository's non-test Go sources: the calls
// it makes, as the names a call resolves to, and the projection writes its own body issues.
type sourceFunction struct {
	key    string
	file   string
	line   int
	calls  []string
	writes []projectionWrite
}

// projectionSources is the repository's call graph as a name-resolved approximation, with every
// projection write found in it and every write whose table is built at runtime.
type projectionSources struct {
	functions map[string]*sourceFunction
	// byCall answers the functions a call key names: a plain function by its package and
	// name, a method by its package and method name, since a receiver's type needs the type
	// checker and a method call inside a package is resolved well enough by its name.
	byCall map[string][]string
	writes []projectionWrite
	// runtime is every statement whose table the scan cannot read, since its name is built at
	// runtime; runtimeTableSites says which of them reach a projected table.
	runtime []runtimeTableWrite
	files   int
}

// readProjectionWriters parses every non-test Go source of the repository and answers its call
// graph with the projection writes in it.
func readProjectionWriters(t *testing.T) *projectionSources {
	t.Helper()
	root := repositoryRoot(t)
	sources := &projectionSources{functions: map[string]*sourceFunction{}, byCall: map[string][]string{}}
	fset := token.NewFileSet()
	skipped := map[string]struct{}{"node_modules": {}, "vendor": {}, "testdata": {}, ".git": {}, ".jj": {}, "dist": {}, "build": {}}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if _, skip := skipped[entry.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		sources.files++
		sources.read(fset, file, mustRelative(t, root, path))
		return nil
	})
	if err != nil {
		t.Fatalf("read the repository's Go sources: %v", err)
	}
	if sources.files < 100 {
		t.Fatalf("the scan read %d Go source, which is no repository: it is looking in the wrong place", sources.files)
	}
	return sources
}

// read records one parsed file: the functions it declares, the calls each makes, the projection
// writes in its string literals, and the writes whose table it builds at runtime.
func (s *projectionSources) read(fset *token.FileSet, file *ast.File, name string) {
	pkg := file.Name.Name
	imports := map[string]string{}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `"`)
		alias := path[strings.LastIndex(path, "/")+1:]
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		imports[alias] = path[strings.LastIndex(path, "/")+1:]
	}
	for _, declared := range file.Decls {
		function, ok := declared.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		key := pkg + "." + function.Name.Name
		index := "func:" + key
		if function.Recv != nil {
			key = pkg + "." + receiverName(function.Recv) + "." + function.Name.Name
			index = "method:" + pkg + "." + function.Name.Name
		}
		for suffix := 2; s.functions[key] != nil; suffix++ {
			key = fmt.Sprintf("%s#%d", key, suffix)
		}
		held := &sourceFunction{key: key, file: name, line: fset.Position(function.Pos()).Line}
		s.functions[key] = held
		s.byCall[index] = append(s.byCall[index], key)
		// A concatenation is read once, from its outermost +; its inner ones are marked here.
		inner := map[*ast.BinaryExpr]bool{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch found := node.(type) {
			case *ast.CallExpr:
				call := callKey(pkg, imports, found.Fun)
				if call != "" {
					held.calls = append(held.calls, call)
				}
				if call == "func:fmt.Sprintf" {
					s.runtime = append(s.runtime, formattedTableWrites(fset, found, name, key)...)
				}
			case *ast.BinaryExpr:
				if found.Op == token.ADD && !inner[found] {
					s.runtime = append(s.runtime, concatenatedTableWrites(fset, concatenation(found, inner), name, key)...)
				}
			case *ast.BasicLit:
				if found.Kind != token.STRING {
					return true
				}
				// A parsed literal always unquotes; its quoted form is read if one ever did not.
				literal, err := strconv.Unquote(found.Value)
				if err != nil {
					literal = found.Value
				}
				for _, clause := range projectionClauses(literal) {
					// A raw literal spans lines, so a statement in it is named by the line its head
					// is on; an interpreted literal is one line.
					line := fset.Position(found.Pos()).Line
					if strings.HasPrefix(found.Value, "`") {
						line += strings.Count(literal[:clause.offset], "\n")
					}
					write := projectionWrite{
						table: clause.table, file: name, line: line,
						clause: clause.text, inside: key, held: clause.held,
					}
					held.writes = append(held.writes, write)
					s.writes = append(s.writes, write)
				}
			}
			return true
		})
	}
}

// matchedClause is one statement found that writes a projected table: its table, how the scan
// names it, where its head starts in the text it was found in, and whether the store writer's
// call path must hold it.
type matchedClause struct {
	table, text string
	offset      int
	held        bool
}

// projectionClauses finds every statement of text that writes a projected table. An insert and
// a delete are held, and an update is held when it sets a column a tuple projects or when the
// scan cannot read what it sets.
func projectionClauses(text string) []matchedClause {
	var found []matchedClause
	for _, match := range projectionWriteHead.FindAllStringSubmatchIndex(text, -1) {
		verb := strings.ToLower(strings.Join(strings.Fields(text[match[2]:match[3]]), " "))
		clause := matchedClause{table: strings.ToLower(text[match[4]:match[5]]), offset: match[0], held: true}
		clause.text = verb + " " + clause.table
		if verb == "update" {
			clause.text, clause.held = updateClause(clause.table, text[match[1]:])
		}
		found = append(found, clause)
	}
	return found
}

// updateClause reads what an update of table sets, rest being the statement after the table's
// name, and answers how the scan names the update and whether it sets a column a tuple
// projects (updateSets).
func updateClause(table, rest string) (string, bool) {
	named, held := updateSets(table, rest)
	return updateText(table, named), held
}

// updateText is how the scan names an update of the table label names, which sets what named
// says (updateSets): nil for a set list the scan cannot read.
func updateText(label string, named []string) string {
	if named == nil {
		return "update " + label + ", whose set list the scan cannot read"
	}
	return "update " + label + " setting " + strings.Join(named, ", ")
}

// updateSets reads the set list of an update of table, rest being the statement after the
// table's name: each assignment as the scan names it, and whether one sets a column a tuple
// projects (projectedColumns, read for that table). An assignment the scan cannot read is held
// as one that does, and so is a set list it cannot find, for which it answers no names.
func updateSets(table, rest string) ([]string, bool) {
	set := updateSet.FindStringIndex(rest)
	if set == nil {
		return nil, true
	}
	assignments, _ := splitOutside(rest[set[1]:], setListEnd)
	named := []string{}
	held := false
	for _, assignment := range assignments {
		target, value, ok := strings.Cut(assignment, "=")
		columns := assignedColumns(target)
		if !ok || columns == nil {
			named = append(named, strconv.Quote(strings.Join(strings.Fields(assignment), " "))+", which the scan cannot read")
			held = true
			continue
		}
		for _, column := range columns {
			states, projected := projectedColumns[table][column]
			switch {
			case table == "comments" && column == "anchor" && len(columns) == 1:
				write, projects := anchorWrite(value)
				named = append(named, write)
				held = held || projects
			case projected:
				named = append(named, column+" ("+states+")")
				held = true
			default:
				named = append(named, column)
			}
		}
	}
	return named, held
}

// assignedColumns answers the columns the target of one assignment names - one column, or a
// parenthesised list of them - each without the subscript or field it may assign into, and nil
// for a target the scan cannot read.
func assignedColumns(target string) []string {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "(") && strings.HasSuffix(target, ")") {
		target = target[1 : len(target)-1]
	}
	var columns []string
	for _, name := range strings.Split(target, ",") {
		name = strings.TrimSpace(name)
		if cut := strings.IndexAny(name, "[."); cut >= 0 {
			name = name[:cut]
		}
		name = strings.ToLower(strings.Trim(name, `"`))
		if !assignedColumn.MatchString(name) {
			return nil
		}
		columns = append(columns, name)
	}
	return columns
}

// anchorWrite names what value, what an assignment gives a comment's anchor, writes of it, and
// whether that sets the column the anchored_to tuple projects. A jsonb_set of keys of
// anchorKeysOutsideTheTuple alone, one over another, keeps the artifact that tuple names and
// sets nothing it projects. A jsonb_set of the artifact_id key sets it, and so does every other
// write of the anchor - the whole anchor, or a key the scan cannot tell from the artifact's.
func anchorWrite(value string) (string, bool) {
	keys, readable := anchorKeys(value)
	if !readable || len(keys) == 0 {
		return "anchor (" + projectedColumns["comments"]["anchor"] + ")", true
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	named := "the anchor's " + strings.Join(keys, " and ") + " key"
	if len(keys) > 1 {
		named += "s"
	}
	for _, key := range keys {
		if !slices.Contains(anchorKeysOutsideTheTuple, key) {
			return named + " (" + projectedColumns["comments"]["anchor"] + ")", true
		}
	}
	return named, false
}

// anchorKeys reads value as the anchor itself or a jsonb_set of one key of what it is given,
// that being the anchor or another such jsonb_set, and answers the keys it sets, innermost first.
// It answers false for any other value.
func anchorKeys(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if anchorColumn.MatchString(value) {
		return []string{}, true
	}
	open := strings.IndexByte(value, '(')
	if open < 0 || !strings.EqualFold(strings.TrimSpace(value[:open]), "jsonb_set") {
		return nil, false
	}
	inside := value[open+1:]
	arguments, end := splitOutside(inside, nil)
	if end == len(inside) || inside[end] != ')' || strings.TrimSpace(inside[end+1:]) != "" {
		return nil, false
	}
	if len(arguments) != 3 && len(arguments) != 4 {
		return nil, false
	}
	path := anchorKeyPath.FindStringSubmatch(strings.TrimSpace(arguments[1]))
	if path == nil {
		return nil, false
	}
	keys, readable := anchorKeys(arguments[0])
	if !readable {
		return nil, false
	}
	return append(keys, strings.ToLower(path[1])), true
}

// --- the writes whose table is built at runtime -----------------------------------------

// runtimeTableWrite is one statement of the sources whose table name is built at runtime, so the
// scan cannot read which table it writes: its verb, the expression its table is named by, and
// the text after that table, each runtime part of it written as {expression}.
type runtimeTableWrite struct {
	file, inside string
	line         int
	verb, table  string
	rest         string
}

// site is how runtimeTableSites names the statement's site: its file, its function and its
// head, which no edit elsewhere in the file moves.
func (w runtimeTableWrite) site() string {
	return w.file + "#" + w.inside + ": " + w.verb + " " + w.table
}

// runtimeTableSite is what the test says of one site whose table is built at runtime: the
// projected tables its table can be, none for a site that never writes one, and, for an update
// of them, what it sets in each, as the scan names the set list it reads after the table for
// that table (updateSets).
type runtimeTableSite struct {
	tables []string
	sets   []string
}

// runtimeTableSites names every statement of the sources whose table name is built at runtime,
// by its site (runtimeTableWrite.site). The scan cannot read such a table, so a statement at a
// site this list does not name fails the test, and so does a site it names where no such
// statement stands any longer. A site that reaches a projected table is then held as a statement
// naming that table is: its insert or delete to the store writer's call path, and its update
// there too where it sets a column a tuple projects.
var runtimeTableSites = map[string]runtimeTableSite{
	// The one-time pin of a legacy quote anchor's block, over asks and then comments
	// (BackfillAnchorBlocks): the anchor's block_id key alone, which keeps its artifact.
	"packages/envoy/internal/dispatch/docs/anchor_blocks.go#docs.*Service.backfillAnchorBlock: update table": {
		tables: []string{"comments"}, sets: []string{"the anchor's block_id key"},
	},
	// A version's refresh of the anchors on its document, over asks and comments: the anchor's
	// quote and orphaned keys alone, which keep its artifact.
	"packages/envoy/internal/dispatch/docs/anchor_refresh.go#docs.*Service.refreshAnchors: update update.table": {
		tables: []string{"comments"}, sets: []string{"the anchor's orphaned and quote keys"},
	},
	// The block index of a comment, a message and an ask, over the item's own tables (refs'
	// itemTables): its block state, blocks, block texts and block links, none of them projected.
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueRecordIndexed: update i.tables.state":  {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.clearItemWindowsOwed: update tables.state":               {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueStateLock: insert into i.tables.state": {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueBlocks: update tables.blocks":          {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueBlocks: insert into tables.blocks":     {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueBlocks: insert into tables.texts":      {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueBlockRefs: delete from tables.refs":    {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.ItemIndexing.queueBlockRefs: insert into tables.refs":    {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.markItemBlocksOwed: update tables.state":                 {},
	// The tombstone of a deleted comment or message (refs.TombstoneItem): each block's stored
	// excerpt and each text a revision recorded, over the item's own blocks and block texts
	// tables, none of them projected.
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.TombstoneItem: update tables.blocks": {},
	"packages/envoy/internal/dispatch/refs/item_blocks.go#refs.TombstoneItem: update tables.texts":  {},
	// A delete's settle of its post's pending send attempts (queueDeletedPostSettlement):
	// comment_deliveries or message_deliveries, neither of them projected.
	"packages/envoy/internal/dispatch/api/post_history.go#api.queueDeletedPostSettlement: update read.table": {},
	// The stamp of an item's block links (refs.Stamp): comment_block_refs, message_block_refs or
	// ask_block_refs, never block_refs, whose stamp is a statement of its own beside it.
	"packages/envoy/internal/dispatch/refs/refs.go#refs.queueStamp: update tables.refs": {},
	// The settle of one source an owed-index queue held: block_grammar_owed or refs_grammar_owed.
	"packages/envoy/internal/dispatch/refs/rebuild.go#refs.OwedQueue.settleOwedStatement: delete from queue.table": {},
	// The daemon's kept decisions and approvals, parked_decisions and readmitted_approvals, in a
	// database of its own.
	"packages/daemon/internal/record/store.go#record.*Postgres.MoveKeptClaims: update table":      {},
	"packages/daemon/internal/store/kept_decisions.go#store.tellKeptDecisions: delete from table": {},
	"packages/daemon/internal/store/kept_decisions.go#store.releaseKeptDecisions: update table":   {},
	// The repository rename pass (store.RewriteRenamedRepositories): it writes a renamed repository's
	// name or URL into the issues, projects, check, delivery, pull request head, branch commit,
	// architecture source and CI reconcile tables, and deletes the rows the new name would
	// collide with in them, none of which is projected.
	"packages/envoy/internal/dispatch/store/repo_rename_pass.go#store.nameWrite: update table":       {},
	"packages/envoy/internal/dispatch/store/repo_rename_pass.go#store.urlWrite: update table":        {},
	"packages/envoy/internal/dispatch/store/repo_rename_pass.go#store.collisions: delete from table": {},
	// A migration runner's version table, which its caller names: schema_migrations for
	// Dispatch's store (store.versionTable) and the broker's own for the broker. pgmigrate records
	// each migration it applies and fills the identity of a row an earlier runner wrote; its test
	// helper package, which every runner's tests share, writes rows of that table to drive the
	// refusals. Neither reaches a projected table.
	"packages/envoy/internal/pgmigrate/identity.go#pgmigrate.writeFixes: update pgx.Identifier{…}.Sanitize()":                     {},
	"packages/envoy/internal/pgmigrate/identity.go#pgmigrate.Record: insert into pgx.Identifier{…}.Sanitize()":                    {},
	"packages/envoy/internal/pgmigrate/pgmigratetest/identity.go#pgmigratetest.RunIdentityChecks: insert into table":              {},
	"packages/envoy/internal/pgmigrate/pgmigratetest/identity.go#pgmigratetest.RunIdentityChecks: update table":                   {},
	"packages/envoy/internal/pgmigrate/pgmigratetest/identity.go#pgmigratetest.CheckVersionsOfAnEarlierRunner: update table":      {},
	"packages/envoy/internal/pgmigrate/pgmigratetest/identity.go#pgmigratetest.CheckVersionsOfAnEarlierRunner: insert into table": {},
}

// runtimeTableHead matches a literal that ends where a write's table begins, the expression
// after it in a concatenation naming that table; formattedTableHead matches the head of a write
// whose table a format's verb names.
var (
	runtimeTableHead   = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s+(?:only\s+)?$`)
	formattedTableHead = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s+(?:only\s+)?%[sv]`)
)

// statementGoesOn matches, by verb, how a statement's text goes on past its table: an update's
// alias and set, a delete's alias and where, using or returning or nothing more, an insert's
// column list or rows. A head followed by anything else - `update %s: %w`, or a write step's
// description, `"update " + table + " anchor"` - is prose and no statement.
var statementGoesOn = map[string]*regexp.Regexp{
	"update":      updateSet,
	"delete from": regexp.MustCompile(`(?is)^(?:\s+(?:as\s+)?\w+)?\s*(?:$|;|\b(?:where|using|returning)\b)`),
	"insert into": regexp.MustCompile(`(?is)^(?:\s+as\s+\w+)?\s*(?:\(|\b(?:select|values|default\s+values|overriding|with)\b)`),
}

// concatenation flattens the + chain at expression into its parts, marking each inner + of it.
func concatenation(expression ast.Expr, inner map[*ast.BinaryExpr]bool) []ast.Expr {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.ADD {
		return []ast.Expr{expression}
	}
	inner[binary] = true
	return append(concatenation(binary.X, inner), concatenation(binary.Y, inner)...)
}

// stringLiteral answers a string literal and its text, and false for any other expression.
func stringLiteral(expression ast.Expr) (*ast.BasicLit, string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil, "", false
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		text = literal.Value
	}
	return literal, text, true
}

// literalLine is the line of the text at offset in literal: a raw literal spans lines.
func literalLine(fset *token.FileSet, literal *ast.BasicLit, text string, offset int) int {
	line := fset.Position(literal.Pos()).Line
	if strings.HasPrefix(literal.Value, "`") {
		line += strings.Count(text[:offset], "\n")
	}
	return line
}

// concatenatedTableWrites finds each write in a concatenation whose table is the part after a
// literal ending at a write's head: `update ` + tables.state + ` set ...`.
func concatenatedTableWrites(fset *token.FileSet, parts []ast.Expr, file, inside string) []runtimeTableWrite {
	var found []runtimeTableWrite
	for index := range len(parts) - 1 {
		literal, text, ok := stringLiteral(parts[index])
		if !ok {
			continue
		}
		head := runtimeTableHead.FindStringSubmatchIndex(text)
		if head == nil {
			continue
		}
		var rest strings.Builder
		for _, part := range parts[index+2:] {
			if _, text, ok := stringLiteral(part); ok {
				rest.WriteString(text)
			} else {
				rest.WriteString("{" + types.ExprString(part) + "}")
			}
		}
		verb := strings.ToLower(strings.Join(strings.Fields(text[head[2]:head[3]]), " "))
		if !statementGoesOn[verb].MatchString(rest.String()) {
			continue
		}
		found = append(found, runtimeTableWrite{
			file: file, inside: inside, line: literalLine(fset, literal, text, head[0]),
			verb: verb, table: types.ExprString(parts[index+1]), rest: rest.String(),
		})
	}
	return found
}

// formattedTableWrites finds each write in a fmt.Sprintf format whose table one of the format's
// verbs names: `update %s set ...`, the table being the argument that verb takes.
func formattedTableWrites(fset *token.FileSet, call *ast.CallExpr, file, inside string) []runtimeTableWrite {
	if len(call.Args) == 0 {
		return nil
	}
	literal, format, ok := stringLiteral(call.Args[0])
	if !ok {
		return nil
	}
	var found []runtimeTableWrite
	for _, head := range formattedTableHead.FindAllStringSubmatchIndex(format, -1) {
		rest := format[head[1]:]
		verb := strings.ToLower(strings.Join(strings.Fields(format[head[2]:head[3]]), " "))
		if !statementGoesOn[verb].MatchString(rest) {
			continue
		}
		table := "an argument the call does not pass"
		if argument := 1 + formatVerbs(format[:head[1]-2]); argument < len(call.Args) {
			table = types.ExprString(call.Args[argument])
		}
		found = append(found, runtimeTableWrite{
			file: file, inside: inside, line: literalLine(fset, literal, format, head[0]),
			verb: verb, table: table, rest: rest,
		})
	}
	return found
}

// formatVerbs counts the verbs of a format, each of which takes one argument; %% takes none.
func formatVerbs(format string) int {
	verbs := 0
	for index := 0; index < len(format); index++ {
		if format[index] != '%' {
			continue
		}
		if index+1 < len(format) && format[index+1] == '%' {
			index++
			continue
		}
		verbs++
	}
	return verbs
}

// runtimeTableWrites holds every write the sources build their table for at runtime to
// runtimeTableSites, and answers the writes of a projected table the named ones make, with how
// many sites it matched. It fails on a write at a site the list does not name, on a site it
// names where no such write stands any longer, and on an update the list says sets, in one of
// the tables it reaches, other than the scan reads there.
func (s *projectionSources) runtimeTableWrites(t *testing.T) ([]projectionWrite, int) {
	t.Helper()
	matched := map[string]bool{}
	var writes []projectionWrite
	for _, write := range s.runtime {
		key := write.site()
		site, named := runtimeTableSites[key]
		if !named {
			t.Errorf("%s:%d %s %s inside %s: the statement's table is built at runtime, so the scan cannot read which table it writes: runtimeTableSites names each such site, here %q, with the projected tables it reaches and the columns it sets",
				write.file, write.line, write.verb, write.table, write.inside, key)
			continue
		}
		matched[key] = true
		if len(site.tables) == 0 {
			t.Logf("%s:%d %s %s in %s: a table built at runtime, which the test names as no projected table", write.file, write.line, write.verb, write.table, write.inside)
			continue
		}
		for _, table := range site.tables {
			label := table + " (its table built at runtime: " + write.table + ")"
			clause, held := write.verb+" "+label, true
			if write.verb == "update" {
				var named []string
				named, held = updateSets(table, write.rest)
				if !slices.Equal(named, site.sets) {
					t.Errorf("%s:%d update %s inside %s: runtimeTableSites says it sets %q in %s, and the scan reads it setting %q there",
						write.file, write.line, write.table, write.inside, site.sets, table, named)
				}
				clause = updateText(label, named)
			}
			writes = append(writes, projectionWrite{
				table: table, file: write.file, line: write.line, clause: clause, inside: write.inside, held: held,
			})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(runtimeTableSites)) {
		if !matched[key] {
			t.Errorf("runtimeTableSites names %q, and no statement whose table is built at runtime stands there any longer: the list names the sites the scan cannot read, so that one comes out of it", key)
		}
	}
	return writes, len(matched)
}

// splitOutside splits text at its commas that stand outside parentheses, quotes and SQL
// comments, up to the first closing parenthesis text did not open, the first semicolon, or the
// first match of stop, each outside them too. It answers the parts and the index it stopped at.
func splitOutside(text string, stop *regexp.Regexp) ([]string, int) {
	ends := map[int]bool{}
	if stop != nil {
		for _, at := range stop.FindAllStringIndex(text, -1) {
			ends[at[0]] = true
		}
	}
	var parts []string
	depth, start := 0, 0
	for index := 0; index < len(text); index++ {
		switch char := text[index]; {
		case char == '\'' || char == '"':
			index = closingQuote(text, index)
		case strings.HasPrefix(text[index:], "--"):
			if newline := strings.IndexByte(text[index:], '\n'); newline >= 0 {
				index += newline
			} else {
				index = len(text)
			}
		case char == '(':
			depth++
		case char == ')' && depth > 0:
			depth--
		case depth > 0:
			// Inside parentheses only their nesting is read.
		case char == ')' || char == ';' || ends[index]:
			return append(parts, text[start:index]), index
		case char == ',':
			parts = append(parts, text[start:index])
			start = index + 1
		}
	}
	return append(parts, text[start:]), len(text)
}

// closingQuote answers the index of the quote that closes the one at open, a doubled quote
// standing for one inside, or the end of text where none does.
func closingQuote(text string, open int) int {
	for index := open + 1; index < len(text); index++ {
		if text[index] != text[open] {
			continue
		}
		if index+1 < len(text) && text[index+1] == text[open] {
			index++
			continue
		}
		return index
	}
	return len(text)
}

// callKey is the name a call resolves to: a package-qualified function, a function of the
// caller's own package, or a method of it by name.
func callKey(pkg string, imports map[string]string, fun ast.Expr) string {
	switch called := fun.(type) {
	case *ast.Ident:
		return "func:" + pkg + "." + called.Name
	case *ast.SelectorExpr:
		if receiver, ok := called.X.(*ast.Ident); ok {
			if imported, isPackage := imports[receiver.Name]; isPackage {
				return "func:" + imported + "." + called.Sel.Name
			}
		}
		return "method:" + pkg + "." + called.Sel.Name
	}
	return ""
}

func receiverName(fields *ast.FieldList) string {
	if len(fields.List) == 0 {
		return "?"
	}
	switch receiver := fields.List[0].Type.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		if named, ok := receiver.X.(*ast.Ident); ok {
			return "*" + named.Name
		}
	case *ast.IndexExpr:
		if named, ok := receiver.X.(*ast.Ident); ok {
			return named.Name
		}
	}
	return "?"
}

// requireEveryWriteIsOnTheStorePath reports every write of a projected table the sources hold,
// with the call path it sits in, and fails on a held one no store write covers.
//
// A row's existence and the columns of its table the store states (projectedColumns) are what
// the row's node and its tuples state, so an insert, a delete and an update that sets one of
// those columns is held: it is written only beside the store write that states the same. An
// update of any other column restates nothing the store holds; it is listed with its call path,
// or as reached by no store write, and passes.
//
// A write is covered when its function writes the store itself, or when a function that does
// reaches it: the two then run in one call, and so in one transaction. The block index's own
// link writer is the second case - refs.IndexBlocks writes the nodes and calls it.
//
// A statement whose table is built at runtime is held as the projected tables runtimeTableSites
// says it reaches (runtimeTableWrites), and fails where the list does not name its site.
//
// The writers of comments are held to the commentsRowWriters names, each by the function the
// scan reports its statement in: another function that makes, unmakes or sets a projected column
// of a comments row fails, even where a store write reaches it, and so does a name of the list
// the scan no longer finds there. The seed's is one of them, the one writer of comments
// outside internal/dispatch, so a scan that read less of the repository than it should fails
// too rather than passing on the writers it did find.
func requireEveryWriteIsOnTheStorePath(t *testing.T, sources *projectionSources) {
	t.Helper()
	covered, from := sources.reachedByAStoreWrite()
	if len(sources.writes) == 0 {
		t.Fatalf("the scan over %d Go source found no write of a projected table, and the %d writers of comments alone are known (commentsRowWriters)",
			sources.files, len(commentsRowWriters))
	}
	runtimeWrites, sites := sources.runtimeTableWrites(t)
	writes := slices.Concat(sources.writes, runtimeWrites)
	// commentWrites is every held statement of comments by the function it is in, as
	// commentsRowWriters names one.
	commentWrites := map[string][]string{}
	held, listed := 0, 0
	for _, write := range writes {
		if write.table == "comments" && write.held {
			site := write.file + "#" + write.inside
			commentWrites[site] = append(commentWrites[site], fmt.Sprintf("%s at line %d", write.clause, write.line))
		}
		_, reached := covered[write.inside]
		path := "which no store write reaches"
		if reached {
			path = "through " + strings.Join(sources.path(from, write.inside), " -> ")
		}
		switch {
		case !write.held:
			listed++
			t.Logf("%s:%d %s in %s, %s: no column it sets is one a tuple projects", write.file, write.line, write.clause, write.inside, path)
		case !reached:
			held++
			t.Errorf("%s:%d %s inside %s, which no store write reaches: a row of a projected table is made, unmade or given a column a tuple projects only beside the store write that states it",
				write.file, write.line, write.clause, write.inside)
		default:
			held++
			t.Logf("%s:%d %s in %s, %s", write.file, write.line, write.clause, write.inside, path)
		}
	}
	t.Logf("%d writes of a projected table found: %d held to the store writer's call path, %d updates of columns no tuple projects listed; %d statements whose table is built at runtime, at %d sites the test names",
		len(writes), held, listed, len(sources.runtime), sites)
	for _, site := range slices.Sorted(maps.Keys(commentWrites)) {
		if !slices.Contains(commentsRowWriters, site) {
			t.Errorf("%s makes, unmakes or sets a projected column of a comments row (%s), and it is none of the %d writers of comments the check names (commentsRowWriters): a new writer of comments is one the check's own words name before this test admits it",
				site, strings.Join(commentWrites[site], "; "), len(commentsRowWriters))
			continue
		}
		t.Logf("writer of comments %s: %s", site, strings.Join(commentWrites[site], "; "))
	}
	for _, site := range commentsRowWriters {
		if _, found := commentWrites[site]; !found {
			t.Errorf("commentsRowWriters names %s, and the scan found no statement there that makes, unmakes or sets a projected column of a comments row: either that writer moved and the list follows it, or the scan read less of the repository than it should",
				site)
		}
	}
}

// commentsRowWriters are the functions whose statements make or unmake a comments row or set a
// column of it a tuple projects, by the file each is in and the function that issues the
// statement, as the scan names a write: the comment create's row, which the comment route and the
// comment an issue write leaves both queue (queueCommentRow), the comment reply, the seed's
// batches of comments, each beside the nodes the batch's rows project, and the comment write of
// the backfill and of the block-index tick (c-projection-write, Verification v108).
var commentsRowWriters = []string{
	"packages/envoy/internal/dispatch/api/comment_create.go#api.queueCommentRow",
	"packages/envoy/internal/dispatch/api/comment_delivery.go#api.*server.replyComment",
	"packages/envoy/cmd/dispatch-seed/write_items.go#main.*seedRun.writeComments",
	"packages/envoy/internal/dispatch/refs/comment_nodes.go#refs.CommentWrite.Queue",
}

// reachedByAStoreWrite answers every function a store write reaches, with the function each was
// first reached from, so a path can be read back.
func (s *projectionSources) reachedByAStoreWrite() (map[string]struct{}, map[string]string) {
	writers := map[string]struct{}{}
	var queue []string
	for key, function := range s.functions {
		for _, call := range function.calls {
			if slices.Contains(storeWriters, call) {
				writers[key] = struct{}{}
				queue = append(queue, key)
				break
			}
		}
	}
	sort.Strings(queue)
	from := map[string]string{}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		for _, call := range s.functions[key].calls {
			for _, called := range s.byCall[call] {
				if _, seen := writers[called]; seen {
					continue
				}
				writers[called] = struct{}{}
				from[called] = key
				queue = append(queue, called)
			}
		}
	}
	return writers, from
}

// path is how a store write reaches key, read back from the first caller each function was
// reached from.
func (s *projectionSources) path(from map[string]string, key string) []string {
	path := []string{key}
	for range 32 {
		caller, ok := from[key]
		if !ok {
			break
		}
		path = append([]string{caller}, path...)
		key = caller
	}
	return path
}

// requireNoMigrationWritesAProjection reads every migration of the schema: none of them makes
// or unmakes a row of a projected table or sets a column a tuple projects, so no row of one and
// no such column was ever written outside a writer. An update of another column, which restates
// nothing the store holds, is listed.
func requireNoMigrationWritesAProjection(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir("../store/migrations")
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	read := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("../store/migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		read++
		for _, clause := range projectionClauses(pgmigrate.WithoutComments(string(body))) {
			if !clause.held {
				t.Logf("migration %s: %s: no column it sets is one a tuple projects", entry.Name(), clause.text)
				continue
			}
			t.Errorf("migration %s writes %s: %s", entry.Name(), clause.table, clause.text)
		}
	}
	if read == 0 {
		t.Fatal("no migration was read, so nothing was checked")
	}
	t.Logf("%d migrations read, none making or unmaking a row of a projected table or setting a column a tuple projects", read)
}

// --- the fixture the Postgres halves run on ---------------------------------------------

// projectionRun is one database the real write paths wrote: an issue with its spec document, a
// plain comment, a comment anchored to that document, a comment on a project document, a
// session's reply to a mention, which is the delivery path's own write, and a project document
// holding a quote, one of whose paragraphs the run later moves out of that quote.
type projectionRun struct {
	handler  http.Handler
	database *store.Store
	issue    string
	specID   string
	specRef  string
	project  string
	// comments are the ids every comment the run wrote, and documents the artifact ids whose
	// blocks the index wrote.
	comments  []string
	projectID string
	// moverID is the document requireAReparentCarriesItsTuplesTransaction reparents a block of.
	moverID string
}

func newProjectionRun(t *testing.T) *projectionRun {
	t.Helper()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	// The run's pool reports every statement to connectionWatch, so a watched write's statements
	// are read on the connection each of them ran on, the ones a helper ran outside the
	// transaction it was handed among them.
	handler, database, _ := newTestServer(t, testServerOptions{
		envoyURL: listener.URL, store: storetest.TracedStore(t, storetest.Open(t), connectionWatch{}),
	})
	issue := createInteractionIssue(t, handler, "PROJ", "A projection of its node",
		"# Spec\n\nA passage the anchor names.\n")
	run := &projectionRun{handler: handler, database: database, issue: issue.Key, specID: issue.PrimaryArtifactID, project: "PROJ"}
	if err := database.Pool.QueryRow(context.Background(),
		`select ref_key from artifacts where id = $1::uuid`, run.specID).Scan(&run.specRef); err != nil {
		t.Fatalf("read the spec's address: %v", err)
	}

	plain := createThreadComment(t, handler, run.issue, map[string]any{"body": "A plain remark."}, "alice")
	anchored := createThreadComment(t, handler, run.issue, map[string]any{
		"body": "A remark on the passage.", "anchor": map[string]any{"artifact": "spec", "quote": "passage"},
	}, "alice")
	// The document cites the issue, so the block index writes a block_refs row too: a link a
	// block holds is the third projection, and it is written beside the node of the block
	// holding it.
	document := createProjectDocument(t, handler, run.project, "Design notes",
		"# Design notes\n\nA project passage citing dispatch://"+run.issue+".\n")
	run.projectID = document.ID
	owned := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body": "A remark the document owns.",
	}, "alice")
	if owned.Code != http.StatusCreated {
		t.Fatalf("comment on the project document: status=%d body=%s", owned.Code, owned.Body.String())
	}
	ownedComment := decodeBody[model.Comment](t, owned)
	// The document a later step reparents a block of (moverMarkdown). Its rows are inserted here,
	// with every other row the strict read holds to its inserting transaction.
	run.moverID = createProjectDocument(t, handler, run.project, "Moves", moverMarkdown).ID

	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, run.issue, map[string]any{
		"body": "Can this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer the mention: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[model.Comment](t, answered)
	run.comments = []string{plain.ID, anchored.ID, ownedComment.ID, mentioned.ID, reply.ID}
	return run
}

// requireNoStoredFunctionWritesAProjection reads the catalogue: no function of the schema and
// no trigger makes or unmakes a row of a projected table or sets a column a tuple projects, so a
// write hidden in plpgsql is found where the Go sources cannot show it. Each scan gathers every
// such write it finds and fails on a set that is not empty, naming each row of it: a scan that
// only logs its rows cannot fail.
func requireNoStoredFunctionWritesAProjection(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	// prolang is the oid of a pg_language row, so the language is named through pg_language.
	// An internal function's body is the name of a C entry point, not statements, so it is left
	// out. 'internal'::regtype::oid is the oid of the type named internal (2281), which no
	// language has, so comparing prolang with it left nothing out.
	functions, read := catalogueProjectionWrites(t, ctx, run, `
		select p.proname, p.prosrc
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = current_schema()
			and p.prolang <> (select l.oid from pg_language l where l.lanname = 'internal')
	`)
	if read == 0 {
		t.Fatal("no stored function was read, so nothing was checked")
	}
	if len(functions) != 0 {
		t.Errorf("stored functions write a projected table:\n  %s", strings.Join(functions, "\n  "))
	}
	// Every trigger of the schema, on whichever table, with the body it runs: a trigger on any
	// table can write a projected table, and a deferred constraint trigger writes it at COMMIT,
	// where no writer can be named.
	triggers, fired := catalogueProjectionWrites(t, ctx, run, `
		select c.relname || '.' || t.tgname
				|| case when t.tgdeferrable then ' (deferrable constraint trigger)' else '' end
				|| ' -> ' || p.proname,
			p.prosrc
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_namespace n on n.oid = c.relnamespace
		join pg_proc p on p.oid = t.tgfoid
		where not t.tgisinternal and n.nspname = current_schema()
	`)
	if fired == 0 {
		t.Fatal("no trigger was read, so nothing was checked")
	}
	if len(triggers) != 0 {
		t.Errorf("triggers write a projected table:\n  %s", strings.Join(triggers, "\n  "))
	}
	t.Logf("%d stored functions and %d triggers of the schema read", read, fired)
}

// catalogueProjectionWrites runs a catalogue query answering a name and the body it runs, and
// answers every write of a projected table those bodies hold that the store writer's path must
// hold (projectionClauses), each named by its row, with how many rows it read. An update of a
// column no tuple projects is listed.
func catalogueProjectionWrites(t *testing.T, ctx context.Context, run *projectionRun, query string) ([]string, int) {
	t.Helper()
	rows, err := run.database.Pool.Query(ctx, query)
	if err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	defer rows.Close()
	var writes []string
	read := 0
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatalf("read a row of the catalogue: %v", err)
		}
		read++
		for _, clause := range projectionClauses(pgmigrate.WithoutComments(body)) {
			if !clause.held {
				t.Logf("%s: %s: no column it sets is one a tuple projects", name, clause.text)
				continue
			}
			writes = append(writes, fmt.Sprintf("%s writes %s: %s", name, clause.table, clause.text))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	return writes, read
}

// deferredCountersOfAProjection is the deferred work a projected table may carry: work that
// checks nothing about the projection, so running it at COMMIT takes no check away from the
// writer. comments_watermark raises the comment list's ETag counter for every write of a
// comment row (migration 20261106020619). It is deferred because an immediate trigger takes
// the counter row where the write happens and holds it to commit, which serialises every
// comment write of one issue behind the longest transaction touching it. A constraint trigger
// stands in pg_constraint as well as pg_trigger, so both of its spellings are named.
var deferredCountersOfAProjection = []string{
	"comments constraint comments_watermark",
	"comments trigger comments_watermark",
}

// requireNoDeferredCheckOnAProjection proves the foreign key is checked where the row is
// written: no constraint and no trigger of a projected table is deferrable, so nothing queues
// the check for COMMIT, where a writer could no longer be named. The counters named above are
// the exception, each one work the writer is not waiting on an answer from.
func requireNoDeferredCheckOnAProjection(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	deferred, err := readStrings(ctx, run.database.Pool, `
		select c.relname || ' constraint ' || n.conname
		from pg_constraint n join pg_class c on c.oid = n.conrelid
		where c.relname = any($1) and n.condeferrable
		union all
		select c.relname || ' trigger ' || t.tgname
		from pg_trigger t join pg_class c on c.oid = t.tgrelid
		where c.relname = any($1) and t.tgdeferrable
		order by 1
	`, projectedTables)
	if err != nil {
		t.Fatalf("read the deferred checks of the projected tables: %v", err)
	}
	deferred = slices.DeleteFunc(deferred, func(found string) bool {
		return slices.Contains(deferredCountersOfAProjection, found)
	})
	if len(deferred) != 0 {
		t.Errorf("the projected tables carry deferred checks %v, and a deferred check runs at COMMIT, away from the writer", deferred)
	}
	keys, err := readStrings(ctx, run.database.Pool, `
		select c.relname || ' ' || n.conname from pg_constraint n join pg_class c on c.oid = n.conrelid
		where c.relname = any($1) and n.contype = 'f' and n.confrelid = 'graph_nodes'::regclass
		order by 1
	`, projectedTables)
	if err != nil {
		t.Fatalf("read the node foreign keys: %v", err)
	}
	if len(keys) != len(projectedTables) {
		t.Errorf("the tables carry %v, want one foreign key to graph_nodes on each of %v", keys, projectedTables)
	}
}

// requireTheForeignKeyRefusesInsideTheStatement writes a projection row with the node write
// left out. The insert itself is refused, and the transaction is aborted from that statement
// on, which is what a check that is not deferred means.
func requireTheForeignKeyRefusesInsideTheStatement(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	tx, err := run.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write with no node: %v", err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		insert into comments (issue_key, author, body, node_kind, node_address)
		values ($1, '{"kind":"user","id":"alice"}', 'A row whose node was never written.', $2, gen_random_uuid()::text)
	`, run.issue, graphstore.KindComment)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "23503" {
		t.Fatalf("the insert of a row naming no node answered %v, want the foreign key's 23503", err)
	}
	if _, err := tx.Exec(ctx, `select 1`); err == nil {
		t.Error("the transaction read on after the refusal, so the check did not run inside the statement")
	}
}

// --- the watched transaction ------------------------------------------------------------

// issuedStatement is one statement a writer ran, with the backend it ran on and the
// transaction it belonged to.
type issuedStatement struct {
	sql  string
	pid  int32
	txid int64
}

// watchedTx is the transaction a writer under test runs in. It records what every statement
// the writer issues ran on, and can fail the write at a statement of the test's choosing,
// either before the statement runs (the writer is handed an error) or by aborting the
// transaction so the statement itself errors.
type watchedTx struct {
	pgx.Tx
	statements []issuedStatement
	// failBefore is the number of statements to let through; -1 lets every one through. A
	// value equal to the count of a whole write fails nothing, and the caller injects the
	// failure itself once the write has run.
	failBefore int
	abort      bool
	injected   bool
	// traced is every statement the write issued on the run's pool, read by the tracer on the
	// connection each ran on (connectionWatch): a statement a helper sent through a pooled
	// connection never passes through this transaction, and is recorded there all the same.
	traced *watchedWrite
	// backend is the process id of the connection the transaction holds, read when it began.
	backend uint32
}

// connectionWatch is the tracer on the run's pool. It keeps nothing itself: a write under watch
// carries its own record on its context (watchedWriteKey), so a statement is recorded for the
// write that issued it, whichever connection of the pool it ran on, and the run's other work is
// recorded nowhere.
type connectionWatch struct{}

// watchedWriteKey marks the context of one watched write with the record its statements go to.
type watchedWriteKey struct{}

// tracedStatement is one statement a watched write issued, with the backend process id of the
// connection it ran on.
type tracedStatement struct {
	sql string
	pid uint32
}

// watchedWrite is what one watched write issued on the run's pool, in the order it issued it.
type watchedWrite struct {
	mu         sync.Mutex
	statements []tracedStatement
}

func (w *watchedWrite) add(sql string, pid uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statements = append(w.statements, tracedStatement{sql: strings.Join(strings.Fields(sql), " "), pid: pid})
}

func (w *watchedWrite) all() []tracedStatement {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.statements)
}

// index answers the position of the first statement holding clause, and -1 for none.
func (w *watchedWrite) index(clause string) int {
	for position, statement := range w.all() {
		if strings.Contains(statement.sql, clause) {
			return position
		}
	}
	return -1
}

// elsewhere is every statement the write ran on a connection other than the one its
// transaction holds, each named with the backend it ran on: the helper that sent it is the
// writer's code that ran it outside the transaction it was handed.
func (w *watchedTx) elsewhere() []string {
	var off []string
	for _, statement := range w.traced.all() {
		if statement.pid != w.backend {
			off = append(off, fmt.Sprintf("backend %d: %s", statement.pid, statement.sql))
		}
	}
	return off
}

func (connectionWatch) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if write, watched := ctx.Value(watchedWriteKey{}).(*watchedWrite); watched {
		write.add(data.SQL, conn.PgConn().PID())
	}
	return ctx
}

func (connectionWatch) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The batch half: a statement sent in a batch is reported here rather than as a query start, and
// a write that batches its statements is watched as one that sends them one by one.
func (connectionWatch) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (connectionWatch) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if write, watched := ctx.Value(watchedWriteKey{}).(*watchedWrite); watched {
		write.add(data.SQL, conn.PgConn().PID())
	}
}

func (connectionWatch) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// errInjected is the failure the test injects between two writes.
var errInjected = errors.New("injected failure between the node write and the projection write")

func (w *watchedTx) before(ctx context.Context, sql string) error {
	if w.failBefore >= 0 && len(w.statements) == w.failBefore && !w.injected {
		w.injected = true
		if !w.abort {
			return errInjected
		}
		// One statement of the write errors: the transaction is aborted under it, so the
		// statement the writer issues next is the one that fails.
		_, _ = w.Tx.Exec(ctx, `select 1 / 0`)
	}
	statement := issuedStatement{sql: strings.Join(strings.Fields(sql), " ")}
	if err := w.Tx.QueryRow(ctx, `
		select activity.pid, txid_current() from pg_stat_activity activity where activity.pid = pg_backend_pid()
	`).Scan(&statement.pid, &statement.txid); err != nil {
		// An aborted transaction answers nothing; the statement is still recorded, since what
		// the writer issued is what this watch is for.
		statement.pid, statement.txid = 0, 0
	}
	w.statements = append(w.statements, statement)
	return nil
}

func (w *watchedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := w.before(ctx, sql); err != nil {
		return pgconn.CommandTag{}, err
	}
	return w.Tx.Exec(ctx, sql, args...)
}

func (w *watchedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := w.before(ctx, sql); err != nil {
		return nil, err
	}
	return w.Tx.Query(ctx, sql, args...)
}

func (w *watchedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := w.before(ctx, sql); err != nil {
		return errorRow{err}
	}
	return w.Tx.QueryRow(ctx, sql, args...)
}

// SendBatch records each statement of a write phase's batch (store.WriteBatch) as Exec records
// one, in the order the batch holds them, and then sends the batch. A failure injected at one of
// them fails the whole batch, as the batch is sent at once: none of its statements runs.
func (w *watchedTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	for _, queued := range b.QueuedQueries {
		if err := w.before(ctx, queued.SQL); err != nil {
			return errorBatch{err}
		}
	}
	return w.Tx.SendBatch(ctx, b)
}

// errorBatch answers an injected failure to a caller that reads a batch's results.
type errorBatch struct{ err error }

func (r errorBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r errorBatch) Query() (pgx.Rows, error)         { return nil, r.err }
func (r errorBatch) QueryRow() pgx.Row                { return errorRow{r.err} }
func (r errorBatch) Close() error                     { return r.err }
func (r errorBatch) Tx() pgx.Tx                       { return nil }

// errorRow answers an injected failure to a caller that reads one row.
type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

// after is the failure injected once a write has run every statement it has, which is the
// direction that fails after the last of the two writes.
func (w *watchedTx) after(ctx context.Context) error {
	if w.failBefore < 0 || w.injected {
		return nil
	}
	w.injected = true
	if !w.abort {
		return errInjected
	}
	_, err := w.Tx.Exec(ctx, `select 1 / 0`)
	return err
}

// index answers the position of the first statement holding clause, and -1 for none.
func (w *watchedTx) index(clause string) int {
	for position, statement := range w.statements {
		if strings.Contains(statement.sql, clause) {
			return position
		}
	}
	return -1
}

// projectionWriter is one writer of a projected table as this test drives it: the write
// itself, run in the transaction it is handed, and the rows it leaves behind, read back from a
// connection of their own.
type projectionWriter struct {
	name string
	// table is the projected table the write's projection statement writes, and node the
	// clause that writes the row's node.
	table string
	// write runs the whole write - the node, the row, and the facts the row projects - in the
	// caller's transaction.
	write func(ctx context.Context, tx pgx.Tx) error
	// left is what either side of the write left behind, read outside its transaction: every
	// row of the projection and of the store the write would have written, each as the row
	// itself, so a row a rollback left committed is reported as what it holds.
	left func(ctx context.Context) ([]string, error)
}

// projectionWriters are the two writers a transaction of this tree runs: the comment write,
// which writes a comment's node and then its row, and the block index, which writes a version's
// block nodes and then the document_blocks rows that project them.
func projectionWriters(t *testing.T, run *projectionRun) []projectionWriter {
	t.Helper()
	commentID := mustUUID(t, run)
	blockVersion := 900
	blockAddress := run.specRef + "#watched"
	return []projectionWriter{{
		name:  "the comment write",
		table: "comments",
		write: func(ctx context.Context, tx pgx.Tx) error {
			stored, err := commentStored(ctx, tx, commentID, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), nil, &run.projectID, run.project, nil)
			if err != nil {
				return err
			}
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				graphstore.QueueNodes(b, stored.written)
				return nil
			}); err != nil {
				return err
			}
			// The row the node is written for. It carries the columns the write under test
			// owns - the node's address - and no more: what the rest of a comment holds is
			// the create route's, and is not what this writer is.
			if _, err := tx.Exec(ctx, `
				insert into comments (id, issue_key, author, body, node_kind, node_address)
				values ($1::uuid, $2, '{"kind":"user","id":"alice"}', 'A watched remark.', $3, $1)
			`, commentID, run.issue, graphstore.KindComment); err != nil {
				return err
			}
			return graphstore.Caused(ctx, tx, stored.written, graphstore.NoEvent)
		},
		left: func(ctx context.Context) ([]string, error) {
			return readStrings(ctx, run.database.Pool, `
				select 'comments ' || row_to_json(c)::text from comments c where c.id::text = $1
				union all
				select 'graph_nodes ' || row_to_json(n)::text from graph_nodes n where n.kind = 'comment' and n.id = $1
			`, commentID)
		},
	}, {
		name:  "the block index",
		table: "document_blocks",
		write: func(ctx context.Context, tx pgx.Tx) error {
			markdown := "# Watched\n\nA watched passage.\n"
			blocks := []model.ArtifactBlock{{ID: "watched", Type: "paragraph", From: 11, To: len(markdown) - 1}}
			_, won, written, err := refs.IndexBlocks(ctx, tx, run.specID, blockVersion, time.Now().UTC(), markdown, blocks, nil, nil)
			if err != nil {
				return err
			}
			if !won {
				return fmt.Errorf("the pass at version %d lost the fence, so it wrote nothing to watch", blockVersion)
			}
			return graphstore.Caused(ctx, tx, written, graphstore.NoEvent)
		},
		left: func(ctx context.Context) ([]string, error) {
			return readStrings(ctx, run.database.Pool, `
				select 'document_blocks ' || row_to_json(b)::text from document_blocks b
				where b.artifact_id = $2::uuid and b.node_address = $1
				union all
				select 'graph_nodes ' || row_to_json(n)::text from graph_nodes n where n.kind = 'block' and n.id = $1
			`, blockAddress, run.specID)
		},
	}}
}

func mustUUID(t *testing.T, run *projectionRun) string {
	t.Helper()
	var id string
	if err := run.database.Pool.QueryRow(context.Background(), `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("draw an id for the watched comment: %v", err)
	}
	return id
}

// requireOneTransactionPerWrite runs each writer in a watched transaction and reads what every
// statement it issued ran on: one backend and one transaction, the node write's.
//
// Two records are read. The tracer on the run's pool saw every statement the write issued, on
// the connection each ran on, so a helper that sent one through a pooled connection of its own
// is caught by the backend that statement ran on, whether or not it wrote anything the rollback
// could leave. The transaction's own record (watchedTx) names the transaction each statement
// it carried ran in. And a row the rollback left committed is read back out of the database and
// reported as the row it is, beside the statements that ran outside the transaction.
func requireOneTransactionPerWrite(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	for _, writer := range projectionWriters(t, run) {
		watched := run.runWatched(t, ctx, writer, -1, false)
		leftover, err := writer.left(ctx)
		if err != nil {
			t.Fatalf("read what %s left behind: %v", writer.name, err)
		}
		if len(leftover) != 0 {
			t.Errorf("%s left these rows committed after its transaction rolled back:\n  %s\nand ran these statements outside that transaction:\n  %s",
				writer.name, strings.Join(leftover, "\n  "), strings.Join(watched.elsewhere(), "\n  "))
		}
		traced := watched.traced.all()
		tracedNode := watched.traced.index("insert into graph_nodes")
		if tracedNode < 0 {
			t.Fatalf("%s issued no node write on the run's pool; it ran %v", writer.name, traced)
		}
		nodeBackend := traced[tracedNode].pid
		if nodeBackend != watched.backend {
			t.Errorf("%s wrote its node on backend %d, and the transaction it was handed holds backend %d",
				writer.name, nodeBackend, watched.backend)
		}
		for position, statement := range traced {
			if statement.pid != nodeBackend {
				t.Errorf("%s ran statement %d on backend %d, and its node write on backend %d: %s",
					writer.name, position, statement.pid, nodeBackend, statement.sql)
			}
		}

		if len(watched.statements) < 2 {
			t.Fatalf("%s issued %d statement through its transaction, which is no write", writer.name, len(watched.statements))
		}
		nodeWrite := watched.index("insert into graph_nodes")
		if nodeWrite < 0 {
			t.Fatalf("%s issued no node write; it ran %v", writer.name, statementTexts(watched.statements))
		}
		projection := watched.index("into " + writer.table)
		if projection < 0 {
			t.Fatalf("%s wrote no row of %s; it ran %v", writer.name, writer.table, statementTexts(watched.statements))
		}
		if projection < nodeWrite {
			t.Errorf("%s writes the %s row at statement %d and its node at %d: the node comes first, because the foreign key is checked inside the row's own statement",
				writer.name, writer.table, projection, nodeWrite)
		}
		wanted := watched.statements[nodeWrite]
		for position, statement := range watched.statements {
			if statement.pid != wanted.pid || statement.txid != wanted.txid {
				t.Errorf("%s ran statement %d on backend %d in transaction %d, and its node write on backend %d in transaction %d: %s",
					writer.name, position, statement.pid, statement.txid, wanted.pid, wanted.txid, statement.sql)
			}
		}
		if off := watched.elsewhere(); len(off) == 0 {
			t.Logf("%s ran %d statements on the run's pool, all on backend %d, and its transaction %d", writer.name, len(traced), nodeBackend, wanted.txid)
		}
	}
}

func statementTexts(statements []issuedStatement) []string {
	texts := make([]string, len(statements))
	for index, statement := range statements {
		texts[index] = statement.sql
	}
	return texts
}

// runWatched runs one writer in a transaction of its own and rolls it back, answering what it
// issued. failBefore is the statement to fail the write at, -1 for a write that is let through.
func (r *projectionRun) runWatched(t *testing.T, ctx context.Context, writer projectionWriter, failBefore int, abort bool) *watchedTx {
	t.Helper()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", writer.name, err)
	}
	watched := &watchedTx{Tx: tx, failBefore: failBefore, abort: abort,
		traced: &watchedWrite{}, backend: tx.Conn().PgConn().PID()}
	// The write runs on a context carrying its record, so the tracer on the run's pool records
	// every statement it issues, on whichever connection it ran.
	ctx = context.WithValue(ctx, watchedWriteKey{}, watched.traced)
	writeErr := writer.write(ctx, watched)
	if writeErr == nil {
		writeErr = watched.after(ctx)
	}
	if failBefore < 0 && writeErr != nil {
		t.Fatalf("%s failed with nothing injected: %v", writer.name, writeErr)
	}
	if failBefore >= 0 && writeErr == nil {
		t.Errorf("%s ran to the end with a failure injected before statement %d", writer.name, failBefore)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll %s back: %v", writer.name, err)
	}
	return watched
}

// requireNeitherSideSurvivesAFailure fails each writer between its two writes and after both of
// them, as a failure of the writer's own and as a statement of the write erroring, and reads
// neither side afterwards: the node and the row commit together or not at all.
func requireNeitherSideSurvivesAFailure(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	for _, writer := range projectionWriters(t, run) {
		whole := run.runWatched(t, ctx, writer, -1, false)
		projection := whole.index("into " + writer.table)
		if projection < 0 {
			t.Fatalf("%s wrote no row of %s", writer.name, writer.table)
		}
		for _, injection := range []struct {
			what  string
			point int
		}{
			{"before the projection write", projection},
			{"after the projection write", len(whole.statements)},
		} {
			for _, abort := range []bool{false, true} {
				how := "the writer is handed a failure"
				if abort {
					how = "a statement of the write errors"
				}
				failed := run.runWatched(t, ctx, writer, injection.point, abort)
				leftover, err := writer.left(ctx)
				if err != nil {
					t.Fatalf("read what %s left: %v", writer.name, err)
				}
				if len(leftover) != 0 {
					t.Errorf("%s, failed %s while %s, left these rows committed:\n  %s\nand ran these statements outside its transaction:\n  %s\nthe two commit together or not at all",
						writer.name, injection.what, how, strings.Join(leftover, "\n  "), strings.Join(failed.elsewhere(), "\n  "))
				}
			}
		}
		t.Logf("%s: neither side survives a failure before or after the projection write, injected either way", writer.name)
	}
}

// --- what the real write paths left -------------------------------------------------------

// requireEveryRowHasItsNodeAndTuples reads the plain case: every row the run's own requests
// inserted names a node the store holds, and the transaction that inserted the row wrote that
// node and stated at least one fact about it.
//
// A row's xmin is the transaction that wrote it, which is how the three are compared: the row's
// xmin, its node's and each of its tuples'. Both sides are required of every row, so a row whose
// node another transaction wrote, or whose transaction stated no fact about its node, fails
// naming the transactions it read. Every row is read before any step of the run updates one: a
// later update moves the row's xmin to its own transaction, which its node need not share, and
// requireAReparentCarriesItsTuplesTransaction holds such an update to the tuple it appended.
func requireEveryRowHasItsNodeAndTuples(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	for _, table := range projectedTables {
		orphans, err := readStrings(ctx, run.database.Pool, `
			select coalesce(row.node_kind, '(none)') || ' ' || coalesce(row.node_address, '(none)')
				|| ': the row at xmin ' || row.xmin::text
				|| ', its node at xmin ' || coalesce(node.xmin::text, '(no node)')
				|| ', its tuples at xmin ' || coalesce(facts.xmins, '(no tuple)')
			from `+table+` row
			left join graph_nodes node on node.kind = row.node_kind and node.id = row.node_address
			left join lateral (
				select string_agg(distinct fact.xmin::text, ', ') as xmins,
					bool_or(fact.xmin::text = row.xmin::text) as same
				from graph_tuples fact
				where fact.from_kind = row.node_kind and fact.from_id = row.node_address
			) facts on true
			where node.kind is null
			   or node.xmin::text <> row.xmin::text
			   or facts.same is not true
			order by 1
		`)
		if err != nil {
			t.Fatalf("read the rows of %s against the store: %v", table, err)
		}
		if len(orphans) != 0 {
			t.Errorf("%s holds %d row(s) whose node and tuples were not written by the row's own transaction:\n  %s",
				table, len(orphans), strings.Join(orphans, "\n  "))
		}
		var written int
		if err := run.database.Pool.QueryRow(ctx, `select count(*) from `+table).Scan(&written); err != nil {
			t.Fatalf("count the rows of %s: %v", table, err)
		}
		if written == 0 {
			t.Errorf("the run wrote no row of %s, so nothing of it was read", table)
		}
		t.Logf("%s: %d rows, each with its node and the facts its own transaction stated", table, written)
	}
	for _, comment := range run.comments {
		node, found, err := graphstore.ReadNode(ctx, run.database.Pool, graphstore.KindComment, comment)
		if err != nil || !found {
			t.Fatalf("read the node of comment %s: found %v, %v", comment, found, err)
		}
		if node.Supertag != graphstore.KindComment || node.State != graphstore.StateLive || node.SourceEvent == graphstore.NoEvent {
			t.Errorf("the node of comment %s is %+v, want the comment supertag, live, and caused by the event that stored it", comment, node)
		}
		for attribute, want := range map[string]string{
			graphstore.AttributeVisibility: graphstore.VisibilityProject,
			graphstore.AttributeState:      graphstore.StateLive,
		} {
			value, held, err := graphstore.CurrentValue(ctx, run.database.Pool, graphstore.KindComment, comment, attribute)
			if err != nil {
				t.Fatalf("read the %s of comment %s: %v", attribute, comment, err)
			}
			if !held || value != want {
				t.Errorf("comment %s states %s = %q (held %v), want %q", comment, attribute, value, held, want)
			}
		}
	}
	// The two relations, each on the comment that has one: the anchored comment points at the
	// document its anchor names, and the project document's comment at the document it
	// belongs to.
	anchored := run.relations(t, ctx, graphstore.TupleAnchoredTo)
	if len(anchored) != 1 || anchored[0] != run.specRef {
		t.Errorf("the run's anchored_to tuples point at %v, want the spec alone", anchored)
	}
	owned := run.relations(t, ctx, graphstore.TupleOwnedBy)
	if len(owned) != 1 || !strings.HasPrefix(owned[0], run.project+"/") {
		t.Errorf("the run's owned_by tuples point at %v, want the project document alone", owned)
	}
}

// relations are the far ends every comment of the run holds under kind.
func (r *projectionRun) relations(t *testing.T, ctx context.Context, kind string) []string {
	t.Helper()
	ends, err := readStrings(ctx, r.database.Pool, `
		select to_id from graph_tuples
		where from_kind = $1 and kind = $2 and to_kind = 'artifact' and asserted
		order by to_id
	`, graphstore.KindComment, kind)
	if err != nil {
		t.Fatalf("read the %s tuples of the run's comments: %v", kind, err)
	}
	return ends
}

// moverMarkdown is the document requireAReparentCarriesItsTuplesTransaction reparents a block of:
// a quote holding two paragraphs, the second of which (moverLine) the run moves out of the quote
// to the document's own level. An edit's move lands a block at that level alone
// (pmdoc.MoveBlock), so the block moves out of a container rather than into one, and the quote
// keeps its first paragraph, as a move that would empty it is refused. Each paragraph is one
// line, so the moved one's text reads the same outside the quote as inside it, and the version
// that moves it changes no text of it.
const (
	moverLine     = "A line that moves."
	moverMarkdown = "# Moves\n\n> A quoted line.\n>\n> " + moverLine + "\n"
)

// moverBlock is one document_blocks row of the mover document, as the reparent reads it.
type moverBlock struct {
	id, blockType, excerpt, parent, address, xmin string
	changedVersion                                int
}

// moverBlocks reads every document_blocks row of the run's mover document.
func (r *projectionRun) moverBlocks(t *testing.T, ctx context.Context) []moverBlock {
	t.Helper()
	rows, err := r.database.Pool.Query(ctx, `
		select block_id, type, coalesce(excerpt, ''), coalesce(parent_block_id, ''), coalesce(node_address, ''),
		       xmin::text, changed_version
		from document_blocks where artifact_id = $1::uuid
		order by block_id
	`, r.moverID)
	if err != nil {
		t.Fatalf("read the mover document's blocks: %v", err)
	}
	defer rows.Close()
	var blocks []moverBlock
	for rows.Next() {
		var block moverBlock
		if err := rows.Scan(&block.id, &block.blockType, &block.excerpt, &block.parent, &block.address,
			&block.xmin, &block.changedVersion); err != nil {
			t.Fatalf("scan a block of the mover document: %v", err)
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the mover document's blocks: %v", err)
	}
	return blocks
}

// requireAReparentCarriesItsTuplesTransaction drives one update of a column a tuple projects
// through a real write path, and reads both transaction ids. The run moves the mover document's
// second quoted paragraph out of its quote, as a person's edit does, and changes no text of it.
// The version that edit settles into is indexed in its own transaction, which appends the
// part_of tuple from the block to the document and the retraction of its part_of the quote, and
// updates the block's document_blocks row to name no parent, the column that tuple projects
// (planBlockParents and queueBlockParents, refs/block_nodes.go): the row's xmin must be that
// tuple's. The block's node is not written again, because nothing of it moved, so its xmin is
// printed beside the two and compared with neither. An update of a column no tuple projects - a
// comment's body, its resolution, a block's text - is the foreign key's and the census's to hold,
// not this one's.
func requireAReparentCarriesItsTuplesTransaction(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	before := run.moverBlocks(t, ctx)
	var moving, quote moverBlock
	for _, block := range before {
		switch {
		case block.blockType == "blockquote":
			quote = block
		case block.blockType == "paragraph" && block.excerpt == moverLine:
			moving = block
		}
	}
	if quote.id == "" || moving.id == "" || moving.parent != quote.id {
		t.Fatalf("the mover document holds %+v, want the paragraph %q inside a quote", before, moverLine)
	}
	moved := dispatchRequest(t, run.handler, http.MethodPost, "/api/v1/artifacts/"+run.moverID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "move", "block": moving.id, "after": "block:" + quote.id}},
	}, "alice")
	if moved.Code != http.StatusOK || !strings.Contains(moved.Body.String(), `"changed":true`) {
		t.Fatalf("move the paragraph out of the quote: status=%d body=%s", moved.Code, moved.Body.String())
	}
	// The edit's version settles after the edit answers. The tuple its block index appends is
	// read once it has committed, and the row after it, so both are the reparent's.
	var asserted []string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		asserted, err = readStrings(ctx, run.database.Pool, `
			select xmin::text from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'artifact' and asserted
			order by seq
		`, refs.BlockNodeKind, moving.address, graphstore.TuplePartOf)
		if err != nil {
			t.Fatalf("read the moved block's part_of tuple to its document: %v", err)
		}
		if len(asserted) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("15s after the move the store holds no part_of tuple from %s to its document", moving.address)
		}
	}
	if len(asserted) != 1 {
		t.Fatalf("the store holds %d part_of tuples from %s to its document, want the one its reparent appended", len(asserted), moving.address)
	}
	var after moverBlock
	for _, block := range run.moverBlocks(t, ctx) {
		if block.id == moving.id {
			after = block
		}
	}
	if after.parent != "" {
		t.Fatalf("after the move the moved block's row names the parent %q, and its part_of tuple names its document", after.parent)
	}
	// A changed_version that moved would be the row rewritten for its text, an update of a
	// column no tuple projects, and not the reparent this check reads.
	if after.changedVersion != moving.changedVersion {
		t.Fatalf("the move took the moved block's changed_version from %d to %d, so its row was rewritten for its text and not for its parent alone",
			moving.changedVersion, after.changedVersion)
	}
	retracted, err := readStrings(ctx, run.database.Pool, `
		select xmin::text from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $1 and to_id = $4 and not asserted
		order by seq
	`, refs.BlockNodeKind, moving.address, graphstore.TuplePartOf, quote.address)
	if err != nil {
		t.Fatalf("read the retraction of the moved block's part_of the quote: %v", err)
	}
	var node string
	if err := run.database.Pool.QueryRow(ctx, `select xmin::text from graph_nodes where kind = $1 and id = $2`,
		refs.BlockNodeKind, moving.address).Scan(&node); err != nil {
		t.Fatalf("read the moved block's node: %v", err)
	}
	t.Logf("the moved block %s: its row at xmin %s (inserted at %s), the part_of tuple its reparent appended at xmin %s, the retraction of its part_of the quote at xmin %v, its node at xmin %s",
		moving.address, after.xmin, moving.xmin, asserted[0], retracted, node)
	if after.xmin != asserted[0] {
		t.Errorf("the reparented block %s's row carries xmin %s and the part_of tuple its reparent appended carries xmin %s: the update of the column and the fact it projects are not one transaction's",
			moving.address, after.xmin, asserted[0])
	}
}

// requireOnlyTheMovedKindsReachedTheStore fails on a node of any kind but the three this change
// moves - the kinds the three projected tables of migration 20261109022248 name - less the
// kinds the store holds of people and agents and the two its block schema writes, which no
// projection backs, and the four kinds whose nodes name the project a read is limited by
// - an issue, a document, an ask and a message, each written at its own
// create site, beside the rows it writes, which carry no node columns.
func requireOnlyTheMovedKindsReachedTheStore(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	moved := []string{"block", "block_ref", "comment"}
	allowed := append(slices.Clone(moved),
		graphstore.KindPerson, graphstore.KindSession, graphstore.KindRole, graphstore.KindPage,
		"supertag", "attribute",
		graphstore.KindIssue, graphstore.KindArtifact, graphstore.KindAsk, graphstore.KindMessage)
	for _, column := range []string{"select distinct kind from graph_nodes", "select distinct from_kind from graph_tuples"} {
		kinds, err := readStrings(ctx, run.database.Pool, column+" order by 1")
		if err != nil {
			t.Fatalf("read the kinds the store holds: %v", err)
		}
		for _, kind := range kinds {
			if !slices.Contains(allowed, kind) {
				t.Errorf("the store holds a %s, and this change moves %v alone", kind, moved)
			}
		}
	}
}

// requireTwoWritersOverOneNodeCommitInTurn runs two writes over one comment's node at once. The
// second waits at that node's row until the first commits, both commit, and the census over
// the projections and the store reads no difference after them.
func requireTwoWritersOverOneNodeCommitInTurn(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	before, err := projectionCensus(ctx, run.database)
	if err != nil {
		t.Fatalf("read the census before the two writes: %v", err)
	}
	if before != 0 {
		t.Fatalf("the census reads %d difference before the two writes, and the run's own paths wrote every row", before)
	}
	held := run.comments[0]
	node := func(state string) []graphstore.Node {
		return []graphstore.Node{{
			Kind: graphstore.KindComment, ID: held, Supertag: graphstore.KindComment,
			Visibility: graphstore.VisibilityProject, State: state,
			MadeAt: time.Now().UTC(), MadeBy: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent,
		}}
	}
	first, err := run.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first write: %v", err)
	}
	defer first.Rollback(ctx)
	if err := graphstore.Write(ctx, first, node(graphstore.StateLive), nil); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		tx, err := run.database.Pool.Begin(ctx)
		if err != nil {
			second <- err
			return
		}
		if err := graphstore.Write(ctx, tx, node(graphstore.StateLive), nil); err != nil {
			_ = tx.Rollback(ctx)
			second <- err
			return
		}
		second <- tx.Commit(ctx)
	}()
	select {
	case err := <-second:
		t.Fatalf("the second write over the node finished while the first held it: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first write: %v", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the second write: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the second write never finished after the first committed")
	}
	after, err := projectionCensus(ctx, run.database)
	if err != nil {
		t.Fatalf("read the census after the two writes: %v", err)
	}
	if after != 0 {
		t.Errorf("the census reads %d difference after the two writes, want none", after)
	}
}

// projectionCensus counts the differences between the projections and the store: a row naming
// no node, and a comment node no comments row projects. A row naming a node the store does not
// hold is the foreign key's, which refuses it at the writer.
func projectionCensus(ctx context.Context, database *store.Store) (int, error) {
	total := 0
	for _, table := range projectedTables {
		var missing int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from `+table+` where node_kind is null or node_address is null
		`).Scan(&missing); err != nil {
			return 0, fmt.Errorf("count the rows of %s naming no node: %w", table, err)
		}
		total += missing
	}
	var unprojected int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_nodes node
		where node.kind = $1 and not exists (
			select 1 from comments row where row.node_kind = node.kind and row.node_address = node.id)
	`, graphstore.KindComment).Scan(&unprojected); err != nil {
		return 0, fmt.Errorf("count the comment nodes no row projects: %w", err)
	}
	return total + unprojected, nil
}

// --- small readers ------------------------------------------------------------------------

func readStrings(ctx context.Context, q graphstore.Queryer, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var answers []string
	for rows.Next() {
		var answer string
		if err := rows.Scan(&answer); err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, rows.Err()
}

// repositoryRoot is the directory the repository's sources start at, found by climbing out of
// the package this test runs in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("read the test's own directory: %v", err)
	}
	for directory := here; ; {
		for _, marker := range []string{".git", ".jj"} {
			if _, err := os.Stat(filepath.Join(directory, marker)); err == nil {
				return directory
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no repository root above %s", here)
		}
		directory = parent
	}
}

func mustRelative(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("name %s under %s: %v", path, root, err)
	}
	return relative
}
