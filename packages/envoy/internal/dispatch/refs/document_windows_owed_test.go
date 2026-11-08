package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// documentsOwedWindowsFromState is the shape DocumentsOwedWindows had before it read a marker
// of its own, kept here as the oracle the marker read is held equal to
// (TestDocumentsOwedWindowsAnswersWhatTheShapeBeforeTheMarkerAnswered) and as the negative
// control of the plan test (TestDocumentsOwedWindowsReadsThePartialIndex). It decides owedness
// from state - whether a stamp stands at the version the blocks are indexed at, and whether a
// window row exists at all - so it reads every row of document_block_versions with two index
// probes each: 112-122 ms a pass and 67,496 buffer hits over the load-test deployment's 9,881
// documents, for an answer of no rows (INFRA-988).
const documentsOwedWindowsFromState = `
	select v.artifact_id::text from document_block_versions v
	where v.artifact_id > $1::uuid
	  and exists (select 1 from document_blocks b where b.artifact_id = v.artifact_id)
	  and (not exists (
		select 1 from embedding_window_states w
		where w.kind = '` + DocumentWindowKind + `' and w.owner_key = v.artifact_id::text
		  and w.block_state = ` + documentBlockState + `
	  ) or not exists (
		select 1 from embeddings e
		where e.kind = '` + DocumentWindowKind + `' and split_part(e.id, '#', 1) = v.artifact_id::text
	  ))
	order by v.artifact_id limit $2`

// documentWindowState is one state a document's windows can be in, as the fixture writes it:
// whether the index holds a block of it, the version its blocks are indexed at (nil: none, the
// row a version write has not reached), what the stamp of the state its windows were written
// from says (nil: no row at all), whether it holds a window row, and whether its row carries
// the window arm's marker.
//
// The marker is what the read asks now, and the stamp and the window row are what the shape
// before it asked, so each state carries the marker the document would really hold in it - set
// by the version write that moved its blocks, cleared only by the write of its windows - and
// the equivalence test reads both questions over the same rows.
type documentWindowState struct {
	name    string
	block   bool
	version *int
	stamp   *string
	window  bool
	marker  bool
	owed    bool
}

// documentWindowStates is every state the window arm has to tell apart.
var documentWindowStates = []documentWindowState{{
	name:    "windowed, and the marker cleared",
	block:   true,
	version: new(2),
	stamp:   new("2"),
	window:  true,
}, {
	name:    "windowed at an older version, and marked",
	block:   true,
	version: new(2),
	stamp:   new("1"),
	window:  true,
	marker:  true,
	owed:    true,
}, {
	name:    "indexed and never windowed",
	block:   true,
	version: new(1),
	window:  false,
	marker:  true,
	owed:    true,
}, {
	name:    "stamped at its version with its window row gone",
	block:   true,
	version: new(1),
	stamp:   new("1"),
	window:  false,
	marker:  true,
	owed:    true,
}, {
	name:    "owed a block index, holding no block",
	block:   false,
	version: nil,
	marker:  true,
}, {
	name:    "owed a block index, holding the blocks of an older version",
	block:   true,
	version: nil,
	stamp:   new(""),
	window:  true,
}}

// documentWindowFillers is how many more documents whose windows stand the fixture writes, so
// the plan test reads a table whose marked rows are the few the arm is for.
const documentWindowFillers = 40

// owedDocumentFixture is one database holding one document in every state of
// documentWindowStates and documentWindowFillers more whose windows stand.
type owedDocumentFixture struct {
	database  *store.Store
	documents []owedDocumentFixtureRow
}

type owedDocumentFixtureRow struct {
	id    string
	state documentWindowState
}

func newOwedDocumentFixture(t *testing.T) owedDocumentFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document window fixture's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "DOCW")
	createReferenceIssue(t, tx, "DOCW-1", "DOCW")

	fixture := owedDocumentFixture{database: database}
	for index, state := range documentWindowStates {
		fixture.documents = append(fixture.documents, owedDocumentFixtureRow{
			id:    writeWindowDocument(t, tx, fmt.Sprintf("state-%02d", index), state),
			state: state,
		})
	}
	for index := range documentWindowFillers {
		writeWindowDocument(t, tx, fmt.Sprintf("filler-%02d", index), documentWindowStates[0])
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the document window fixture: %v", err)
	}
	// The plan this fixture is read for is the one the planner makes over row counts it knows,
	// and a table just written has none.
	if _, err := database.Pool.Exec(ctx, `analyze`); err != nil {
		t.Fatalf("analyze the document window fixture: %v", err)
	}
	return fixture
}

// writeWindowDocument writes one document in state: the artifact, its block row, the
// document_block_versions row as the version write and 0122's trigger leave it in that state,
// the stamp of the state its windows were written from, and one window row.
func writeWindowDocument(t *testing.T, tx pgx.Tx, slug string, state documentWindowState) string {
	t.Helper()
	// A project's document rather than an issue's: an issue holds one primary document, and
	// this fixture writes many.
	id, _ := createReferenceArtifact(t, tx, "", "DOCW", slug)
	if state.block {
		execFixture(t, tx, "write the block of "+slug, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, 'b1', $2 || '#b1', 'paragraph', 'sum', 'excerpt', 1, now())
		`, id, slug)
	}
	marker := "null"
	if state.marker {
		marker = "now()"
	}
	execFixture(t, tx, "write the block index row of "+slug,
		`insert into document_block_versions (artifact_id, version, windows_owed_at) values ($1::uuid, $2, `+marker+`)
		 on conflict (artifact_id) do update set version = excluded.version, windows_owed_at = excluded.windows_owed_at`,
		id, state.version)
	if state.stamp != nil {
		execFixture(t, tx, "stamp the windows of "+slug,
			`insert into embedding_window_states (kind, owner_key, block_state) values ($1, $2, $3)`,
			DocumentWindowKind, id, *state.stamp)
	}
	if state.window {
		execFixture(t, tx, "write the window of "+slug,
			`insert into embeddings (kind, id, text_snapshot, content_hash) values ($1, $2 || '#b1', 'window text', 'hash')`,
			DocumentWindowKind, id)
	}
	return id
}

// owedIDs is every document the fixture wrote whose windows do not stand for its blocks, in the
// order DocumentsOwedWindows answers them.
func (f owedDocumentFixture) owedIDs() []string {
	var owed []string
	for _, document := range f.documents {
		if document.state.owed {
			owed = append(owed, document.id)
		}
	}
	slices.Sort(owed)
	return owed
}

// describe names each state the listing answered, so a red run reads as states rather than as
// uuids.
func (f owedDocumentFixture) describe(listed []string) string {
	var lines []string
	for _, document := range f.documents {
		lines = append(lines, fmt.Sprintf("  %s: %s, owed %v, listed %v",
			document.id, document.state.name, document.state.owed, slices.Contains(listed, document.id)))
	}
	return strings.Join(lines, "\n")
}

// Every document whose windows do not stand for its blocks is listed, and every one whose
// windows do stand is not.
func TestDocumentsOwedWindowsListsTheDocumentsWhoseWindowsDoNotStand(t *testing.T) {
	fixture := newOwedDocumentFixture(t)
	listed, err := DocumentsOwedWindows(context.Background(), fixture.database.Pool, "", 1000)
	if err != nil {
		t.Fatalf("list the documents owed windows: %v", err)
	}
	if want := fixture.owedIDs(); !slices.Equal(listed, want) {
		t.Fatalf("the documents owed windows are %v, want %v:\n%s", listed, want, fixture.describe(listed))
	}
}

// The marker read answers what the shape before it answered, over every state the fixture
// writes, at every cursor and three limits. Reading a marker instead of state is where such a
// change goes wrong, so the answers are compared against the text of the reading this replaced
// rather than against what this test thinks the answer is.
func TestDocumentsOwedWindowsAnswersWhatTheShapeBeforeTheMarkerAnswered(t *testing.T) {
	fixture := newOwedDocumentFixture(t)
	ctx := context.Background()
	cursors := []string{""}
	for _, document := range fixture.documents {
		cursors = append(cursors, document.id)
	}
	for _, after := range cursors {
		for _, limit := range []int{1, 2, 1000} {
			listed, err := DocumentsOwedWindows(ctx, fixture.database.Pool, after, limit)
			if err != nil {
				t.Fatalf("list the documents owed windows after %q: %v", after, err)
			}
			rows, err := fixture.database.Pool.Query(ctx, documentsOwedWindowsFromState, uuidAfter(after), limit)
			if err != nil {
				t.Fatalf("read the shape before the marker after %q: %v", after, err)
			}
			want, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the shape before the marker after %q: %v", after, err)
			}
			if !slices.Equal(listed, want) {
				t.Errorf("the documents owed windows after %q, %d at a time, are %v, where the shape this replaced answers %v:\n%s",
					after, limit, listed, want, fixture.describe(listed))
			}
		}
	}
}

// The marker is set by the version write, in the transaction that writes the blocks, and
// cleared by the statement that writes the document's windows and by nothing else. Both halves
// are read as behaviour - a version written, listed, windowed, a second version written and
// windowed again - rather than from the text of a statement.
//
// The control is the shape before this change, where the version write wrote the document's
// windows in its own transaction and that statement cleared the marker in the same breath:
//
//	--- FAIL: TestAVersionWriteOwesADocumentItsWindowsAndWritingThemClearsIt (1.49s)
//	    after the version write, document bb64e60e-d8ba-48f5-90b4-24bbf955dd75 is owed
//	    windows = false, want true
func TestAVersionWriteOwesADocumentItsWindowsAndWritingThemClearsIt(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "marked-md")
	markdown, blocks := documentOf([]part{
		{"b1", "heading", "# Marked"},
		{"b2", "paragraph", "The first paragraph."},
	})
	if _, err := IndexBlocks(ctx, tx, artifact, 1, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	requireDocumentOwedWindows(t, tx, artifact, true, "the version write")
	if windows := windowRowsOf(t, tx, DocumentWindowKind, artifact); len(windows) != 0 {
		t.Fatalf("the version write wrote the windows %v; the window arm writes a document's windows", windows)
	}

	if err := WriteDocumentWindows(ctx, tx, artifact, markdown, blocks); err != nil {
		t.Fatalf("window the document: %v", err)
	}
	requireDocumentOwedWindows(t, tx, artifact, false, "the window arm's write")
	if windows := windowRowsOf(t, tx, DocumentWindowKind, artifact); len(windows) == 0 {
		t.Fatalf("the windowed document holds no window")
	}

	markdown, blocks = documentOf([]part{
		{"b1", "heading", "# Marked"},
		{"b2", "paragraph", "The first paragraph, rewritten."},
	})
	if _, err := IndexBlocks(ctx, tx, artifact, 2, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index version 2: %v", err)
	}
	requireDocumentOwedWindows(t, tx, artifact, true, "the second version write")
	if err := WriteDocumentWindows(ctx, tx, artifact, markdown, blocks); err != nil {
		t.Fatalf("window the second version: %v", err)
	}
	requireDocumentOwedWindows(t, tx, artifact, false, "the window arm's second write")
}

// requireDocumentOwedWindows fails unless the document's marker stands as owed says.
func requireDocumentOwedWindows(t *testing.T, tx pgx.Tx, artifactID string, owed bool, after string) {
	t.Helper()
	var marked bool
	if err := tx.QueryRow(context.Background(),
		`select windows_owed_at is not null from document_block_versions where artifact_id = $1::uuid`, artifactID,
	).Scan(&marked); err != nil {
		t.Fatalf("read the window marker of %s: %v", artifactID, err)
	}
	if marked != owed {
		t.Fatalf("after %s, document %s is owed windows = %v, want %v", after, artifactID, marked, owed)
	}
}

// The documents owed their windows, held to the partial index on windows_owed_at.
func TestDocumentsOwedWindowsReadsThePartialIndex(t *testing.T) {
	fixture := newOwedDocumentFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	keys := explaintest.NewIndexKeys(fixture.database.Pool)

	requireOwedWindowsReadsThePartialIndex(t, connection, keys, owedWindowsRead{
		subject:      "the documents owed windows",
		statement:    documentsOwedWindowsQuery,
		control:      documentsOwedWindowsFromState,
		driving:      "document_block_versions",
		blocks:       "document_blocks",
		owedIndex:    "document_block_versions_windows_owed",
		otherIndexes: []string{"document_block_versions_pkey"},
	})
}
