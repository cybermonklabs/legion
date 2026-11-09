package api

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectedTables are the tables migration 20261109022248 made projections of the node and
// tuple store: each row of them names the node it is the projection of, and that node is
// written by the same transaction, before the row.
var projectedTables = []string{"block_refs", "comments", "document_blocks"}

// TestNoProjectionRowIsWrittenWithoutTheStore holds every writer of a projected table to the
// one rule the projections rest on: a row of one of them is written only by a transaction that
// writes the row's node in the store first, and the two commit together or not at all.
//
// The source scan is coverage - it names every writer there is, so a new one cannot arrive
// unnoticed - and the Postgres halves are the proof: a failure injected between the two writes
// leaves neither side, a row whose node was never written is refused inside the statement that
// writes it, and the real write paths leave every row with its node and its tuples from the
// transaction that wrote the row.
func TestNoProjectionRowIsWrittenWithoutTheStore(t *testing.T) {
	ctx := context.Background()
	sources := readProjectionWriters(t)
	t.Run("every write of a projected table sits on the store writer's call path", func(t *testing.T) {
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
	t.Run("the real write paths leave every row with its node and its tuples", func(t *testing.T) {
		requireEveryRowHasItsNodeAndTuples(t, ctx, run)
	})
	t.Run("no kind outside the list this change moves reached the store", func(t *testing.T) {
		requireOnlyTheMovedKindsReachedTheStore(t, ctx, run)
	})
	t.Run("two writers over one node commit one after the other", func(t *testing.T) {
		requireTwoWritersOverOneNodeCommitInTurn(t, ctx, run)
	})
}

// --- the source scan --------------------------------------------------------------------

// rowWrite matches a statement that makes or unmakes a row of a projected table. A row's
// existence is what its node states, so an insert and a delete are the writes this rule
// covers; a statement that only changes columns of a row that is already there restates
// nothing about which node the row projects.
var rowWrite = regexp.MustCompile(`(?is)\b(?:insert\s+into|delete\s+from)\s+(block_refs|comments|document_blocks)\b`)

// nodeColumnWrite matches an update that assigns a row's node address, the one column update
// that does say which node a row projects.
var nodeColumnWrite = regexp.MustCompile(`(?is)\bupdate\s+(block_refs|comments|document_blocks)\b[^;]*?\bset\b[^;]*?\bnode_(?:kind|address)\b`)

// The store's own writers, as a call names them. Everything that writes graph_nodes and
// graph_tuples goes through one of the three (internal/dispatch/graphstore).
var storeWriters = []string{"func:graphstore.Write", "func:graphstore.WriteArms", "func:graphstore.Authored"}

// projectionWrite is one statement of the sources that writes a projected table.
type projectionWrite struct {
	table   string
	file    string
	line    int
	clause  string
	inside  string
	through []string
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
// projection write found in it.
type projectionSources struct {
	functions map[string]*sourceFunction
	// byCall answers the functions a call key names: a plain function by its package and
	// name, a method by its package and method name, since a receiver's type needs the type
	// checker and a method call inside a package is resolved well enough by its name.
	byCall map[string][]string
	writes []projectionWrite
	files  int
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

// read records one parsed file: the functions it declares, the calls each makes and the
// projection writes in its string literals.
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
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch found := node.(type) {
			case *ast.CallExpr:
				if call := callKey(pkg, imports, found.Fun); call != "" {
					held.calls = append(held.calls, call)
				}
			case *ast.BasicLit:
				if found.Kind != token.STRING {
					return true
				}
				for _, clause := range projectionClauses(found.Value) {
					write := projectionWrite{
						table: clause.table, file: name, line: fset.Position(found.Pos()).Line,
						clause: clause.text, inside: key,
					}
					held.writes = append(held.writes, write)
					s.writes = append(s.writes, write)
				}
			}
			return true
		})
	}
}

// matchedClause is one write clause found in a string literal.
type matchedClause struct{ table, text string }

func projectionClauses(literal string) []matchedClause {
	var found []matchedClause
	for _, match := range rowWrite.FindAllStringSubmatch(literal, -1) {
		found = append(found, matchedClause{table: match[1], text: strings.Join(strings.Fields(match[0]), " ")})
	}
	for _, match := range nodeColumnWrite.FindAllStringSubmatch(literal, -1) {
		found = append(found, matchedClause{table: match[1], text: strings.Join(strings.Fields(match[0]), " ")})
	}
	return found
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

// requireEveryWriteIsOnTheStorePath reports every projection write the sources hold with the
// call path it sits in, and fails on one no store write covers.
//
// A write is covered when its function writes the store itself, or when a function that does
// reaches it: the two then run in one call, and so in one transaction. The block index's own
// link writer is the second case - refs.IndexBlocks writes the nodes and calls it.
func requireEveryWriteIsOnTheStorePath(t *testing.T, sources *projectionSources) {
	t.Helper()
	covered, from := sources.reachedByAStoreWrite()
	if len(sources.writes) == 0 {
		t.Fatalf("the scan over %d Go source found no write of a projected table, and the three writers of comments alone are known", sources.files)
	}
	for _, write := range sources.writes {
		path := sources.path(from, write.inside)
		if _, ok := covered[write.inside]; !ok {
			t.Errorf("%s:%d writes %s inside %s, which no store write reaches: a row of a projected table is written only beside the node it projects",
				write.file, write.line, write.table, write.inside)
			continue
		}
		t.Logf("%s:%d %s in %s, through %s", write.file, write.line, write.table, write.inside, strings.Join(path, " -> "))
	}
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
// or unmakes a row of a projected table, so no row of one was ever written outside a writer.
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
		for _, clause := range projectionClauses(withoutSQLComments(string(body))) {
			t.Errorf("migration %s writes %s: %s", entry.Name(), clause.table, clause.text)
		}
	}
	if read == 0 {
		t.Fatal("no migration was read, so nothing was checked")
	}
	t.Logf("%d migrations read, none writing a projected table", read)
}

// withoutSQLComments drops the line comments of a SQL file, so a clause written in prose is no
// statement.
func withoutSQLComments(body string) string {
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		if cut := strings.Index(line, "--"); cut >= 0 {
			lines[index] = line[:cut]
		}
	}
	return strings.Join(lines, "\n")
}

// --- the fixture the Postgres halves run on ---------------------------------------------

// projectionRun is one database the real write paths wrote: an issue with its spec document, a
// plain comment, a comment anchored to that document, a comment on a project document, and a
// session's reply to a mention, which is the delivery path's own write.
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
	handler, database := newTargetedMessageHandler(t, listener.URL)
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
// no trigger on a projected table writes one, so a write hidden in plpgsql is found where the
// Go sources cannot show it.
func requireNoStoredFunctionWritesAProjection(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	rows, err := run.database.Pool.Query(ctx, `
		select p.proname, p.prosrc
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = current_schema() and p.prolang <> 'internal'::regtype::oid
	`)
	if err != nil {
		t.Fatalf("read the schema's functions: %v", err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatalf("read a function: %v", err)
		}
		read++
		for _, clause := range projectionClauses(withoutSQLComments(body)) {
			t.Errorf("the stored function %s writes %s: %s", name, clause.table, clause.text)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the schema's functions: %v", err)
	}
	triggers, err := readStrings(ctx, run.database.Pool, `
		select c.relname || '.' || t.tgname || ' -> ' || p.proname
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_proc p on p.oid = t.tgfoid
		where not t.tgisinternal and c.relname = any($1)
		order by 1
	`, projectedTables)
	if err != nil {
		t.Fatalf("read the triggers of the projected tables: %v", err)
	}
	t.Logf("%d stored functions read, none writing a projected table; the triggers on them are %v", read, triggers)
}

// requireNoDeferredCheckOnAProjection proves the foreign key is checked where the row is
// written: no constraint and no trigger of a projected table is deferrable, so nothing queues
// the check for COMMIT, where a writer could no longer be named.
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
	`, run.issue, commentNodeKind)
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
}

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
	// left is what either side of the write left behind: the number of rows of the projection
	// and of the store the write would have written, read outside its transaction.
	left func(ctx context.Context) (int, int, error)
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
			stored, err := commentStored(ctx, tx, commentID, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), nil, &run.projectID)
			if err != nil {
				return err
			}
			if err := stored.writeNode(ctx, tx); err != nil {
				return err
			}
			// The row the node is written for. It carries the columns the write under test
			// owns - the node's address - and no more: what the rest of a comment holds is
			// the create route's, and is not what this writer is.
			if _, err := tx.Exec(ctx, `
				insert into comments (id, issue_key, author, body, node_kind, node_address)
				values ($1::uuid, $2, '{"kind":"user","id":"alice"}', 'A watched remark.', $3, $1)
			`, commentID, run.issue, commentNodeKind); err != nil {
				return err
			}
			return stored.caused(ctx, tx, graphstore.NoEvent)
		},
		left: func(ctx context.Context) (int, int, error) {
			return countPair(ctx, run.database,
				`select count(*) from comments where id = $1::uuid`,
				`select count(*) from graph_nodes where kind = 'comment' and id = $1`, commentID)
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
			return written.Caused(ctx, tx, graphstore.NoEvent)
		},
		left: func(ctx context.Context) (int, int, error) {
			return countPair(ctx, run.database,
				`select count(*) from document_blocks where artifact_id = $2::uuid and node_address = $1`,
				`select count(*) from graph_nodes where kind = 'block' and id = $1`, blockAddress, run.specID)
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

// countPair reads how many rows each of two statements counts, on the pool and so outside any
// transaction the write ran in.
func countPair(ctx context.Context, database *store.Store, projection, node string, args ...any) (int, int, error) {
	var rows, nodes int
	if err := database.Pool.QueryRow(ctx, projection, args...).Scan(&rows); err != nil {
		return 0, 0, err
	}
	if err := database.Pool.QueryRow(ctx, node, args[0]).Scan(&nodes); err != nil {
		return 0, 0, err
	}
	return rows, nodes, nil
}

// requireOneTransactionPerWrite runs each writer in a watched transaction and reads what every
// statement it issued ran on: one backend and one transaction, the node write's. A helper that
// took a pooled connection of its own would commit its row there, which the rollback below
// leaves behind for the read that names it.
func requireOneTransactionPerWrite(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	for _, writer := range projectionWriters(t, run) {
		watched := run.runWatched(t, ctx, writer, -1, false)
		if len(watched.statements) < 2 {
			t.Fatalf("%s issued %d statement, which is no write", writer.name, len(watched.statements))
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
		rows, nodes, err := writer.left(ctx)
		if err != nil {
			t.Fatalf("read what %s left behind: %v", writer.name, err)
		}
		if rows != 0 || nodes != 0 {
			t.Errorf("%s left %d row of %s and %d node committed after its transaction rolled back, so a statement of it ran somewhere else",
				writer.name, rows, writer.table, nodes)
		}
		t.Logf("%s ran %d statements, all on backend %d in transaction %d", writer.name, len(watched.statements), wanted.pid, wanted.txid)
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
	watched := &watchedTx{Tx: tx, failBefore: failBefore, abort: abort}
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
				run.runWatched(t, ctx, writer, injection.point, abort)
				rows, nodes, err := writer.left(ctx)
				if err != nil {
					t.Fatalf("read what %s left: %v", writer.name, err)
				}
				if rows != 0 || nodes != 0 {
					t.Errorf("%s, failed %s while %s, left %d row of %s and %d node: the two commit together or not at all",
						writer.name, injection.what, how, rows, writer.table, nodes)
				}
			}
		}
		t.Logf("%s: neither side survives a failure before or after the projection write, injected either way", writer.name)
	}
}

// --- what the real write paths left -------------------------------------------------------

// requireEveryRowHasItsNodeAndTuples reads the plain case: every row the run's own requests
// wrote names a node the store holds, and the transaction that wrote the row wrote that node
// or stated a fact about it.
//
// A row's xmin is the transaction that wrote it, which is how the two are compared. A node may
// be written again by a later pass, so a row whose node has moved on since is held to the
// tuples its own transaction stated about that node instead.
func requireEveryRowHasItsNodeAndTuples(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	for _, table := range projectedTables {
		orphans, err := readStrings(ctx, run.database.Pool, `
			select coalesce(row.node_kind, '(none)') || ' ' || coalesce(row.node_address, '(none)') || ' at xmin ' || row.xmin::text
			from `+table+` row
			where row.node_kind is null or row.node_address is null
			   or not exists (
				select 1 from graph_nodes node
				where node.kind = row.node_kind and node.id = row.node_address
				  and (node.xmin::text = row.xmin::text or exists (
					select 1 from graph_tuples fact
					where fact.from_kind = row.node_kind and fact.from_id = row.node_address
					  and fact.xmin::text = row.xmin::text))
			   )
			order by 1
		`)
		if err != nil {
			t.Fatalf("read the rows of %s against the store: %v", table, err)
		}
		if len(orphans) != 0 {
			t.Errorf("%s holds %d row the store did not write with it: %v", table, len(orphans), orphans)
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
		node, found, err := graphstore.ReadNode(ctx, run.database.Pool, commentNodeKind, comment)
		if err != nil || !found {
			t.Fatalf("read the node of comment %s: found %v, %v", comment, found, err)
		}
		if node.Supertag != commentSupertag || node.State != graphstore.StateLive || node.SourceEvent == graphstore.NoEvent {
			t.Errorf("the node of comment %s is %+v, want the comment supertag, live, and caused by the event that stored it", comment, node)
		}
		for attribute, want := range map[string]string{
			tupleVisibility: graphstore.VisibilityProject,
			tupleState:      graphstore.StateLive,
		} {
			value, held, err := graphstore.CurrentValue(ctx, run.database.Pool, commentNodeKind, comment, attribute)
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
	anchored := run.relations(t, ctx, tupleAnchoredTo)
	if len(anchored) != 1 || anchored[0] != run.specRef {
		t.Errorf("the run's anchored_to tuples point at %v, want the spec alone", anchored)
	}
	owned := run.relations(t, ctx, tupleOwnedBy)
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
	`, commentNodeKind, kind)
	if err != nil {
		t.Fatalf("read the %s tuples of the run's comments: %v", kind, err)
	}
	return ends
}

// requireOnlyTheMovedKindsReachedTheStore reads the kinds the change moves from the schema's
// own list (graph_store_cutover, migration 20261109013400) and fails on a node of any other
// kind, less the kinds the store holds of people and agents and the two its block schema
// writes, which no projection backs.
func requireOnlyTheMovedKindsReachedTheStore(t *testing.T, ctx context.Context, run *projectionRun) {
	t.Helper()
	moved, err := readStrings(ctx, run.database.Pool, `select kind from graph_store_cutover order by kind`)
	if err != nil {
		t.Fatalf("read the kinds this change moves: %v", err)
	}
	if !slices.Equal(moved, []string{"block", "block_ref", "comment"}) {
		t.Fatalf("the schema's list of moved kinds is %v, and this test is written for the three the change names", moved)
	}
	allowed := append(slices.Clone(moved),
		graphstore.KindPerson, graphstore.KindSession, graphstore.KindRole, graphstore.KindPage,
		"supertag", "attribute")
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
			Kind: commentNodeKind, ID: held, Supertag: commentSupertag,
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
	`, commentNodeKind).Scan(&unprojected); err != nil {
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
