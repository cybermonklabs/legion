package api

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// oldSearchIndexBuild is the commit the boot step runs for in this test, so an announcement the
// test writes under another commit names the build a rollback reaches.
const oldSearchIndexBuild = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"

const (
	// oldSearchMeaning is the query whose meaning both documents' windows are near, and which no
	// word of the corpus holds.
	oldSearchMeaning = "wibbleflorp"
	// oldSearchWord is the word both documents hold, which a search by words finds them by.
	oldSearchWord = "quernstone"
)

// oldSearchObjectsSQL is every one of the stored objects of document search that the catalog
// holds, as "<catalog> <name>": the tables document_search and document_search_backfill in
// pg_class, the functions artifact_versions_document_search() and document_search_term_queue() in
// pg_proc, and the triggers of those names in pg_trigger.
const oldSearchObjectsSQL = `
select 'pg_class ' || relname from pg_class
 where relname in ('document_search', 'document_search_backfill') and relkind = 'r'
union all
select 'pg_proc ' || proname from pg_proc
 where proname in ('artifact_versions_document_search', 'document_search_term_queue')
union all
select 'pg_trigger ' || tgname from pg_trigger
 where tgname in ('artifact_versions_document_search', 'document_search_term_queue')
order by 1`

// oldSearchObjects is what oldSearchObjectsSQL answers over a database that holds all six.
var oldSearchObjects = []string{
	"pg_class document_search", "pg_class document_search_backfill",
	"pg_proc artifact_versions_document_search", "pg_proc document_search_term_queue",
	"pg_trigger artifact_versions_document_search", "pg_trigger document_search_term_queue",
}

// The stored objects of document search and the embedding windows the old owner of a converted
// document holds go in a step of every boot (startup.SearchIndexRetirement), never in a migration.
//
// The boot's fill of document_search goes with no condition: no file of internal/dispatch/store
// declares store.FillDocumentSearch and cmd/dispatch/main.go calls it nowhere, read after the
// same reads find a planted declaration and a planted call in a scratch copy.
//
// The step runs through startup.Retire over three databases:
//   - one whose newest announcement of another build lists the issue-room release's relation
//     kinds and its capability issue_room: the six objects go, and so do the windows held under a
//     converted document's own artifact id, while the windows of a document that is its own room
//     stay; a search by meaning and a search by words each answer what they answered before;
//   - one whose newest such announcement is of the release before, which lists none of those:
//     every object and every old window stays through a first boot, a second process and a
//     restart, each logging the announcement it read, and the server serves;
//   - one this boot created from empty: the six objects go at once.
//
// It prints the count of windows the step deleted and of windows it kept, and fails where a count
// it expects above zero is zero.
func TestTheOldSearchIndexGoesWithItsOldWindows(t *testing.T) {
	t.Run("the boot fill", requireTheBootFillGone)
	t.Run("released", requireTheReleasedDatabaseLosesTheOldIndex)
	t.Run("release before", requireTheReleaseBeforeKeepsTheOldIndex)
	t.Run("from empty", requireAnEmptyDatabaseLosesTheOldIndexAtOnce)
}

// envoyModuleRoot is packages/envoy, three directories above this package's.
func envoyModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// funcDeclaredIn reports whether a non-test Go file of dir declares a function or a method named
// name.
func funcDeclaredIn(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declared := range file.Decls {
			if function, isFunction := declared.(*ast.FuncDecl); isFunction && function.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// callsNamedIn is how many calls the Go file at path makes of a function or method named name.
func callsNamedIn(t *testing.T, path, name string) int {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	calls := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch called := call.Fun.(type) {
		case *ast.SelectorExpr:
			if called.Sel.Name == name {
				calls++
			}
		case *ast.Ident:
			if called.Name == name {
				calls++
			}
		}
		return true
	})
	return calls
}

func requireTheBootFillGone(t *testing.T) {
	root := envoyModuleRoot(t)
	storeDir := filepath.Join(root, "internal", "dispatch", "store")
	mainFile := filepath.Join(root, "cmd", "dispatch", "main.go")

	plantedStore := t.TempDir()
	if err := os.WriteFile(filepath.Join(plantedStore, "planted.go"), []byte("package store\n\n"+
		"func (s *Store) FillDocumentSearch(ctx context.Context) (int64, error) { return 0, nil }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !funcDeclaredIn(t, plantedStore, "FillDocumentSearch") {
		t.Fatal("the scan finds no FillDocumentSearch in a scratch copy that declares it: it cannot find what it looks for")
	}
	source, err := os.ReadFile(mainFile)
	if err != nil {
		t.Fatal(err)
	}
	plantedMain := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(plantedMain, append(source, []byte("\nfunc planted() { _, _ = database.FillDocumentSearch(ctx) }\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if callsNamedIn(t, plantedMain, "FillDocumentSearch") == 0 {
		t.Fatal("the scan finds no call of FillDocumentSearch in a scratch copy of main.go that calls it: it cannot find what it looks for")
	}

	if funcDeclaredIn(t, storeDir, "FillDocumentSearch") {
		t.Errorf("internal/dispatch/store declares FillDocumentSearch, the boot fill of document_search, which reads " +
			"document_search_backfill at every boot and ends the process where that read fails; the issue-room release deletes it")
	}
	if calls := callsNamedIn(t, mainFile, "FillDocumentSearch"); calls > 0 {
		t.Errorf("cmd/dispatch/main.go calls FillDocumentSearch %d times, want none: a boot after the step dropped "+
			"document_search_backfill would end there", calls)
	}
}

// oldSearchFixture is one database with the server over it, holding an issue born converted and
// two documents of that issue, each holding oldSearchWord: one windowed while it was its own room
// and then joined to the issue's room, as the conversion joins a document, so its windows stand
// under its own artifact id, which is the old owner's; and one that is still its own room, whose
// windows are its current owner's. Each document's windows are near oldSearchMeaning.
type oldSearchFixture struct {
	handler   http.Handler
	database  *store.Store
	converted model.Artifact
	current   model.Artifact
}

func seedOldSearchIndex(t *testing.T, project string) oldSearchFixture {
	t.Helper()
	embedder := &fakeEmbedder{vectors: map[string][]float32{oldSearchMeaning: queryVector()}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, project, "The old search index", "Body text.")
	fixture := oldSearchFixture{
		handler:   handler,
		database:  database,
		converted: uploadSearchDocument(t, handler, issue.Key, "converted.md", "The "+oldSearchWord+" grinds the converted grain.\n"),
		current:   uploadSearchDocument(t, handler, issue.Key, "current.md", "The "+oldSearchWord+" grinds the current grain.\n"),
	}
	windowOwedOwners(t, deps)
	embedWindowsAs(t, database, embedder, "document_block", fixture.converted.ID, angledVector(0.9, 2))
	embedWindowsAs(t, database, embedder, "document_block", fixture.current.ID, angledVector(0.8, 3))
	processAllPending(t, database, embedder)
	joined, err := database.Pool.Exec(context.Background(), `
		insert into document_rooms (artifact_id, room)
		select $1::uuid, artifact_id from issue_trees where issue_key = $2`, fixture.converted.ID, issue.Key)
	if err != nil || joined.RowsAffected() != 1 {
		t.Fatalf("join %s to the room of %s: %d rows, %v; want its one row", fixture.converted.ID, issue.Key, joined.RowsAffected(), err)
	}
	return fixture
}

// announce writes an announcement of another build, under commit, listing kinds.
func (f oldSearchFixture) announce(t *testing.T, commit string, kinds []string) {
	t.Helper()
	if _, err := f.database.Pool.Exec(context.Background(),
		`insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`, commit, kinds); err != nil {
		t.Fatalf("announce %s: %v", commit, err)
	}
}

// retireOldSearchIndex runs the boot step over pool with the old search index's entry alone, as a
// process of oldSearchIndexBuild whose boot created the database from empty where fromEmpty, and
// answers what it logged.
func retireOldSearchIndex(t *testing.T, pool *store.Pool, fromEmpty bool) string {
	t.Helper()
	var logged bytes.Buffer
	startup.Retire(context.Background(), pool, oldSearchIndexBuild, fromEmpty, slog.New(slog.NewTextHandler(&logged, nil)),
		[]startup.Retirement{startup.SearchIndexRetirement})
	t.Logf("the boot step logged:\n%s", logged.String())
	return logged.String()
}

// standingOldSearchObjects is what oldSearchObjectsSQL answers over pool, which the test prints
// with the query.
func standingOldSearchObjects(t *testing.T, pool *store.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), oldSearchObjectsSQL)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	defer rows.Close()
	var standing []string
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatalf("read the catalog: %v", err)
		}
		standing = append(standing, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	t.Logf("%s\n=> %d rows: %v", strings.TrimSpace(oldSearchObjectsSQL), len(standing), standing)
	return standing
}

// documentWindows is the id of every document window and whole-text document row of embeddings,
// split by whether its owner is a converted document (a document_rooms row names its artifact
// id).
func documentWindows(t *testing.T, pool *store.Pool) (old, current []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select e.id, exists (select 1 from document_rooms r where r.artifact_id::text = split_part(e.id, '#', 1))
		  from embeddings e where e.kind in ('document_block', 'document') order by e.id`)
	if err != nil {
		t.Fatalf("read the document windows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var converted bool
		if err := rows.Scan(&id, &converted); err != nil {
			t.Fatalf("read the document windows: %v", err)
		}
		if converted {
			old = append(old, id)
		} else {
			current = append(current, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document windows: %v", err)
	}
	return old, current
}

// searchedItems is each item GET /api/v1/search answers query with, as "<kind> <id>", and the
// route's status and body where it answers no 200.
func searchedItems(t *testing.T, handler http.Handler, query string) ([]string, string) {
	t.Helper()
	response := searchRequest(t, handler, "q="+url.QueryEscape(query))
	if response.Code != http.StatusOK {
		return nil, response.Result().Status + " " + response.Body.String()
	}
	var items []string
	for _, result := range decodeBody[model.SearchResponse](t, response).Results {
		items = append(items, result.Kind+" "+result.ID)
	}
	slices.Sort(items)
	return items, ""
}

// requireAnswered fails unless a search answers query, at the moment when names, with every item
// of want.
func requireAnswered(t *testing.T, handler http.Handler, what, query, when string, want ...string) []string {
	t.Helper()
	items, failed := searchedItems(t, handler, query)
	if failed != "" {
		t.Fatalf("%s for %q %s: %s", what, query, when, failed)
	}
	for _, item := range want {
		if !slices.Contains(items, item) {
			t.Fatalf("%s for %q answers %v %s, want %s among them", what, query, items, when, item)
		}
	}
	return items
}

func requireTheReleasedDatabaseLosesTheOldIndex(t *testing.T) {
	fixture := seedOldSearchIndex(t, "OLDS")
	pool := fixture.database.Pool
	fixture.announce(t, "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", refs.TupleKinds())
	oldBefore, currentBefore := documentWindows(t, pool)
	if len(oldBefore) == 0 || len(currentBefore) == 0 {
		t.Fatalf("the fixture holds %d windows of the old owner and %d of current owners, want some of each", len(oldBefore), len(currentBefore))
	}
	currentDocument := "document " + fixture.current.ID
	byMeaning := requireAnswered(t, fixture.handler, "a search by meaning", oldSearchMeaning, "before the step", currentDocument)
	byWords := requireAnswered(t, fixture.handler, "a search by words", oldSearchWord, "before the step", currentDocument)
	if standing := standingOldSearchObjects(t, pool); !slices.Equal(standing, oldSearchObjects) {
		t.Fatalf("before the step the catalog holds %v, want the six objects %v", standing, oldSearchObjects)
	}

	logged := retireOldSearchIndex(t, pool, false)

	if !strings.Contains(logged, "the boot's retirement deleted its objects") {
		t.Errorf("the step logged no deletion")
	}
	if standing := standingOldSearchObjects(t, pool); len(standing) != 0 {
		t.Errorf("after the step the catalog still holds %v, want none of the six objects", standing)
	}
	oldAfter, currentAfter := documentWindows(t, pool)
	deleted := len(oldBefore) - len(oldAfter)
	t.Logf("the step deleted %d windows of the old owner of a converted document and kept %d windows of current owners",
		deleted, len(currentAfter))
	if deleted == 0 {
		t.Errorf("the step deleted no window of the old owner, want each of %v gone", oldBefore)
	}
	if len(currentAfter) == 0 {
		t.Errorf("the step kept no window of a current owner, want each of %v kept", currentBefore)
	}
	for _, window := range oldAfter {
		t.Errorf("the window %s of the old owner of a converted document stands after the step", window)
	}
	for _, window := range currentBefore {
		if !slices.Contains(currentAfter, window) {
			t.Errorf("the window %s of a current owner is gone after the step, want it kept", window)
		}
	}

	for _, search := range []struct {
		what, query string
		before      []string
	}{{"a search by meaning", oldSearchMeaning, byMeaning}, {"a search by words", oldSearchWord, byWords}} {
		after, failed := searchedItems(t, fixture.handler, search.query)
		if failed != "" {
			t.Errorf("%s for %q after the step: %s, where before it answered %v", search.what, search.query, failed, search.before)
			continue
		}
		for _, item := range search.before {
			if !slices.Contains(after, item) {
				t.Errorf("%s for %q answers %v after the step, without %s, which it answered before%s",
					search.what, search.query, after, item, windowsOf(item, oldBefore, currentBefore))
			}
		}
		for _, item := range after {
			if !slices.Contains(search.before, item) {
				t.Errorf("%s for %q answers %s after the step, which it did not answer before (%v)", search.what, search.query, item, search.before)
			}
		}
	}
}

// windowsOf names the windows a document hit was answered through: those whose owner key is its
// id, among old and current.
func windowsOf(item string, old, current []string) string {
	id, isDocument := strings.CutPrefix(item, "document ")
	if !isDocument {
		return ""
	}
	var held []string
	for _, window := range slices.Concat(old, current) {
		if owner, _, _ := strings.Cut(window, "#"); owner == id {
			held = append(held, window)
		}
	}
	return " through the windows " + strings.Join(held, ", ")
}

func requireTheReleaseBeforeKeepsTheOldIndex(t *testing.T) {
	fixture := seedOldSearchIndex(t, "OLDB")
	pool := fixture.database.Pool
	const beforeCommit = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	before := slices.DeleteFunc(refs.TupleKinds(), func(kind string) bool {
		return slices.Contains(startup.SearchIndexRetirement.Kinds, kind)
	})
	fixture.announce(t, beforeCommit, before)
	oldBefore, currentBefore := documentWindows(t, pool)
	if len(oldBefore) == 0 {
		t.Fatalf("the fixture holds no window of the old owner of a converted document")
	}

	for _, boot := range []string{"the first boot", "a second process", "a restart"} {
		logged := retireOldSearchIndex(t, pool, false)
		if !strings.Contains(logged, "announcement_commit="+beforeCommit) || !strings.Contains(logged, "kept its objects") {
			t.Errorf("at %s the step's log names no announcement %s for the kept objects", boot, beforeCommit)
		}
		standing := standingOldSearchObjects(t, pool)
		for _, object := range oldSearchObjects {
			if !slices.Contains(standing, object) {
				t.Errorf("after %s the catalog holds no %s, want it kept while the guard does not hold", boot, object)
			}
		}
		oldAfter, currentAfter := documentWindows(t, pool)
		t.Logf("after %s the step deleted %d windows and kept %d of the old owner and %d of current owners",
			boot, len(oldBefore)-len(oldAfter), len(oldAfter), len(currentAfter))
		if !slices.Equal(oldAfter, oldBefore) || !slices.Equal(currentAfter, currentBefore) {
			t.Errorf("after %s the windows are %v and %v, want the old owner's %v and the current owners' %v kept",
				boot, oldAfter, currentAfter, oldBefore, currentBefore)
		}
		if len(oldAfter) == 0 {
			t.Errorf("after %s no window of the old owner stands, want the %d kept", boot, len(oldBefore))
		}
	}
	requireAnswered(t, fixture.handler, "a search by words", oldSearchWord, "after the step kept the objects", "document "+fixture.current.ID)
}

func requireAnEmptyDatabaseLosesTheOldIndexAtOnce(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, storetest.Empty(t))
	if err != nil {
		t.Fatalf("open the empty database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the empty database: %v", err)
	}
	if !database.CreatedFromEmpty() {
		t.Fatal("the migration run over an empty database read it as one an earlier release served")
	}
	if standing := standingOldSearchObjects(t, database.Pool); !slices.Equal(standing, oldSearchObjects) {
		t.Fatalf("the migrations made %v, want the six objects %v for the step to delete", standing, oldSearchObjects)
	}
	logged := retireOldSearchIndex(t, database.Pool, database.CreatedFromEmpty())
	if !strings.Contains(logged, "created_from_empty=true") {
		t.Errorf("the step's log says nothing of a database created from empty")
	}
	if standing := standingOldSearchObjects(t, database.Pool); len(standing) != 0 {
		t.Errorf("after the first boot over an empty database the catalog holds %v, want none of the six objects", standing)
	}
}
