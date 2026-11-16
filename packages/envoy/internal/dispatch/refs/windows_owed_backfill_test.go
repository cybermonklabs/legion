package refs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// windowMarkerBackfills is every migration that marks the owners whose windows were already
// stale when the marker column reached them, in the order a runner applies them: the items'
// backfill and the documents'. They are read from the files the runner embeds rather than
// copied here, so this test is about the statements a database runs (cmd/broker's
// countMigrations reads its set the same way).
var windowMarkerBackfills = []string{
	"20261107203841_item_block_state_windows_owed_backfill.up.sql",
	"20261108070100_document_block_versions_windows_owed_backfill.up.sql",
}

// applyWindowMarkerBackfill runs one of those migrations over database. Each file is several
// statements, which is why it goes through Exec with no arguments: that is the simple protocol,
// the one a runner applies a migration's whole text through.
func applyWindowMarkerBackfill(t *testing.T, database *store.Store, name string) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "store", "migrations", name))
	if err != nil {
		t.Fatalf("read the migration %s: %v", name, err)
	}
	if _, err := database.Pool.Exec(context.Background(), string(text)); err != nil {
		t.Fatalf("run the migration %s: %v", name, err)
	}
}

// markedItemsOfKind is every item of kind whose state row carries the window arm's marker.
func markedItemsOfKind(t *testing.T, database *store.Store, kind ItemKind) []string {
	t.Helper()
	tables, err := kind.tables()
	if err != nil {
		t.Fatalf("tables of %s: %v", kind, err)
	}
	rows, err := database.Pool.Query(context.Background(),
		`select `+tables.item+`::text from `+tables.state+` where windows_owed_at is not null order by 1`)
	if err != nil {
		t.Fatalf("read the marked %ss: %v", kind, err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the marked %ss: %v", kind, err)
	}
	return marked
}

// markedDocuments is every document whose version row carries the marker.
func markedDocuments(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select artifact_id::text from document_block_versions where windows_owed_at is not null order by 1`)
	if err != nil {
		t.Fatalf("read the marked documents: %v", err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the marked documents: %v", err)
	}
	return marked
}

// windowMarkers is every marker standing in the database, each with the moment it was set, so a
// run that re-stamps a row it has already marked is a difference here. The time is read as text
// because what this compares is whether the stored value changed at all.
func windowMarkers(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select 'comment/' || comment_id::text || ' ' || windows_owed_at::text from comment_block_state where windows_owed_at is not null
		union all
		select 'message/' || message_id::text || ' ' || windows_owed_at::text from message_block_state where windows_owed_at is not null
		union all
		select 'ask/' || ask_id::text || ' ' || windows_owed_at::text from ask_block_state where windows_owed_at is not null
		union all
		select 'document/' || artifact_id::text || ' ' || windows_owed_at::text from document_block_versions where windows_owed_at is not null
		order by 1`)
	if err != nil {
		t.Fatalf("read the window markers: %v", err)
	}
	markers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the window markers: %v", err)
	}
	return markers
}

// clearWindowMarkers puts every marker back to null, which is the one way a test can make rows
// that look stored before the marker column existed: every write of this build sets the marker,
// and the backfills exist for the rows no write of this build has touched since they went stale.
func clearWindowMarkers(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`update comment_block_state set windows_owed_at = null`,
		`update message_block_state set windows_owed_at = null`,
		`update ask_block_state set windows_owed_at = null`,
		`update document_block_versions set windows_owed_at = null`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("clear the markers (%s): %v", statement, err)
		}
	}
}

// windowBackfillStates is every state an item can be in when the marker column reaches it, with
// owed saying whether the backfills must mark it. The marker is false in all of them: that is
// what a row stored before the column looks like, and marking those rows is the whole of what a
// backfill is for.
//
// The third is the owner this round found unmarked: an item the block index has reached (its
// state row carries the sum of the text it was indexed from) that holds no current block and
// still holds the windows its old text left. The arm's own predicate takes it -
// `s.body_sha256 is not null or held.block is not null` (item_blocks.go:1057) - and windows it to
// no window, which deletes those stale windows and clears the marker in the one statement. The
// backfill that required a block marked nothing for it, so no arm ever listed it and the stale
// windows survived the upgrade (INFRA-988).
var windowBackfillStates = []windowState{{
	name:   "windowed, and the windows stand for its blocks",
	block:  currentBlock,
	sum:    new(indexedSum),
	stamp:  new(indexedSum),
	window: true,
}, {
	name:  "holding a current block, indexed and never windowed",
	block: currentBlock,
	sum:   new(indexedSum),
	owed:  true,
}, {
	name:   "indexed to no block, holding the windows its old text left",
	block:  noBlock,
	sum:    new(indexedSum),
	window: true,
	owed:   true,
}, {
	name:  "owed a block index, holding no block and no sum",
	block: noBlock,
	sum:   nil,
}}

// documentWindowBackfillStates is the same four states for a document, whose arm reads
// `v.version is not null or held.block is not null` (blocks.go:463): the third is the document
// the first backfill left unmarked, a version holding no block - an issue created with no spec
// carries a spec.md whose markdown is empty - that still holds the windows an earlier version
// left.
var documentWindowBackfillStates = []documentWindowState{{
	name:    "windowed, and the windows stand for its blocks",
	block:   true,
	version: new(2),
	stamp:   new("2"),
	window:  true,
}, {
	name:    "holding a block, indexed and never windowed",
	block:   true,
	version: new(1),
	owed:    true,
}, {
	name:    "indexed at a version holding no block, holding the windows its old version left",
	block:   false,
	version: new(1),
	window:  true,
	owed:    true,
}, {
	name:    "owed a block index, holding no block",
	block:   false,
	version: nil,
}}

// The backfills mark every item whose windows no longer stand for its blocks and that the window
// arm would then take, which is the arm's own predicate and not a narrower one: an item holding
// no current block is the arm's too, and the arm windows it to none.
//
// What it refuses is the gap this round measured: a backfill that required the item to hold a
// block marks no comment that holds none and one stale window, so no arm ever lists it and its
// stale windows outlive the upgrade. Run against the `where exists (select 1 from <blocks> …)`
// those statements carried before this change, the marked set below is missing exactly that
// item for each of the three kinds.
func TestTheWindowMarkerBackfillsMarkEveryItemTheArmWouldTake(t *testing.T) {
	fixture := newOwedWindowFixture(t, windowBackfillStates)
	clearWindowMarkers(t, fixture.database)
	for _, name := range windowMarkerBackfills {
		applyWindowMarkerBackfill(t, fixture.database, name)
	}

	ctx := context.Background()
	for _, kind := range []ItemKind{ItemComment, ItemMessage, ItemAsk} {
		want := fixture.owedIDs(kind)
		if marked := markedItemsOfKind(t, fixture.database, kind); !slices.Equal(marked, want) {
			t.Errorf("the backfills marked these %ss: %v, want %v:\n%s", kind, marked, want, fixture.describe(kind, marked))
		}
		// The marker is only worth what the arm does with it, so the arm is asked the same
		// question over the same rows.
		listed, err := ItemsOwedWindows(ctx, fixture.database.Pool, kind, "", 1000)
		if err != nil {
			t.Fatalf("list the %ss owed windows: %v", kind, err)
		}
		if !slices.Equal(listed, want) {
			t.Errorf("the arm lists these %ss after the backfills: %v, want %v:\n%s",
				kind, listed, want, fixture.describe(kind, listed))
		}
	}
}

// The same for the documents' backfill, whose arm reads the version where the items' reads the
// sum.
func TestTheWindowMarkerBackfillsMarkEveryDocumentTheArmWouldTake(t *testing.T) {
	fixture := newOwedDocumentFixture(t, documentWindowBackfillStates)
	clearWindowMarkers(t, fixture.database)
	for _, name := range windowMarkerBackfills {
		applyWindowMarkerBackfill(t, fixture.database, name)
	}

	want := fixture.owedIDs()
	if marked := markedDocuments(t, fixture.database); !slices.Equal(marked, want) {
		t.Errorf("the backfills marked these documents: %v, want %v:\n%s", marked, want, fixture.describe(marked))
	}
	listed, err := DocumentsOwedWindows(context.Background(), fixture.database.Pool, "", 1000)
	if err != nil {
		t.Fatalf("list the documents owed windows: %v", err)
	}
	if !slices.Equal(listed, want) {
		t.Errorf("the arm lists these documents after the backfills: %v, want %v:\n%s", listed, want, fixture.describe(listed))
	}
}

// A backfill run twice leaves every row it marked exactly as it found it: a marker carries the
// moment the work was owed, so re-stamping it is a rewrite of a value a reader had. Without the
// `windows_owed_at is null` guard the items' backfill answered UPDATE 7 over rows it had just
// marked itself (INFRA-988).
func TestTheWindowMarkerBackfillsRewriteNothingTheyHaveAlreadyMarked(t *testing.T) {
	fixture := newOwedWindowFixture(t, windowBackfillStates)
	clearWindowMarkers(t, fixture.database)
	for _, name := range windowMarkerBackfills {
		applyWindowMarkerBackfill(t, fixture.database, name)
	}

	before := windowMarkers(t, fixture.database)
	if len(before) == 0 {
		t.Fatal("the backfills marked nothing, so a second run has nothing to leave alone and this test proves nothing")
	}
	for _, name := range windowMarkerBackfills {
		applyWindowMarkerBackfill(t, fixture.database, name)
	}
	if after := windowMarkers(t, fixture.database); !slices.Equal(before, after) {
		t.Errorf("a second run of the backfills left these markers: %v, want the %v it found", after, before)
	}
}
