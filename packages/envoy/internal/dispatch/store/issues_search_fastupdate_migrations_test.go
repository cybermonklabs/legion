package store

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// 20261108015800 takes the pending list off issues_search, the index the keyword search's issue
// arm reads (api/search.go, searchKeywordLegs).
const issuesSearchFastupdate = 20261108015800

// issuesSearchPending is how many pages of pending entries issues_search is carrying, read by
// emptying the list: gin_clean_pending_list answers the pages it moved into the index, so a call
// over an index that batches nothing answers 0 whatever has just been written to it. It is the
// one reading of a GIN pending list a test can take without pageinspect, which this database
// does not install.
func issuesSearchPending(t *testing.T, ctx context.Context, store *Store) int {
	t.Helper()
	var pages int
	if err := store.Pool.QueryRow(ctx, `select gin_clean_pending_list('issues_search')`).Scan(&pages); err != nil {
		t.Fatalf("empty the pending list: %v", err)
	}
	return pages
}

// writeIssuesForTheIndex writes enough issues, each with a body of its own words, to put more
// than one page of entries into a GIN index that batches them. A pending list fills by bytes, so
// the text is what makes the pages, not the row count.
func writeIssuesForTheIndex(t *testing.T, ctx context.Context, store *Store, from, count int) {
	t.Helper()
	for number := from; number < from+count; number++ {
		seedIssueWithSpec(t, ctx, store, number,
			"Harbour survey", "The astrolabe measures altitude over the meridian at the quadrant's own scale.")
	}
}

// What the migration is for: with fastupdate on, which is what a GIN index is created with, a
// write of issues leaves entries in a pending list that every scan of issues_search then has to
// read before it reads anything else, and the planner prices that list above a sequential scan
// of issues. After the migration a write leaves nothing pending.
//
// The reading is gin_clean_pending_list's own answer, the pages it moved: non-zero where the
// index batches, 0 where it writes through. The control is the same writes against the same
// index with fastupdate turned back on, so a test that answered 0 because the writes were too
// small to fill a page would fail there too.
func TestIssuesSearchFastupdateMigrationStopsTheIndexBatchingItsWrites(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issuesSearchFastupdate)

	var options []string
	if err := store.Pool.QueryRow(ctx,
		`select coalesce(reloptions, '{}') from pg_class where relname = 'issues_search'`).Scan(&options); err != nil {
		t.Fatalf("read the index's options: %v", err)
	}
	if !slices.Contains(options, "fastupdate=off") {
		t.Fatalf("issues_search's options are %v, want fastupdate=off", options)
	}

	writeIssuesForTheIndex(t, ctx, store, 1, 200)
	if pages := issuesSearchPending(t, ctx, store); pages != 0 {
		t.Errorf("issues_search held %d pages of pending entries after the migration, want none", pages)
	}

	// The control: the same index batching again, and the same writes.
	if _, err := store.Pool.Exec(ctx, `alter index issues_search set (fastupdate = on)`); err != nil {
		t.Fatalf("turn fastupdate back on: %v", err)
	}
	writeIssuesForTheIndex(t, ctx, store, 1001, 200)
	if pages := issuesSearchPending(t, ctx, store); pages == 0 {
		t.Fatalf("issues_search held nothing pending with fastupdate on, so the reading above decided nothing")
	}
}

// The migration empties the list it inherits, rather than leaving it for the next autovacuum: a
// database that has been running carries entries written before the option was set, and setting
// the option stops new ones without moving those. This migrates a database that already batched
// a list and reads the index clean afterwards.
func TestIssuesSearchFastupdateMigrationEmptiesTheListItInherits(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issuesSearchFastupdate-1)

	writeIssuesForTheIndex(t, ctx, store, 1, 200)
	var pending bool
	if err := store.Pool.QueryRow(ctx,
		`select gin_clean_pending_list('issues_search') > 0`).Scan(&pending); err != nil {
		t.Fatalf("read the inherited pending list: %v", err)
	}
	// gin_clean_pending_list emptied the list to read it, so the rows are written again to leave
	// the migration one to inherit.
	writeIssuesForTheIndex(t, ctx, store, 1001, 200)
	if !pending {
		t.Fatalf("the schema before the migration batched nothing, so this test decides nothing")
	}

	migrateThrough(t, store, issuesSearchFastupdate)
	if pages := issuesSearchPending(t, ctx, store); pages != 0 {
		t.Errorf("issues_search still held %d pages of pending entries after the migration, want none", pages)
	}
}

// What the migration locks above ACCESS SHARE, read as the census reads it: issues, and nothing
// else. It names no table of its own - both its statements name the index - so the reading is
// CensusTables', which resolves an index to the table it belongs to, where TouchedTables reads
// the text alone and answers no table at all. Both statements work on the index: the alter
// writes one catalog row and the flush moves the pending entries into the index, so neither
// reads or rewrites a row of issues.
func TestTheIssuesSearchFastupdateMigrationTouchesOnlyIssues(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issuesSearchFastupdate)
	const file = "20261108015800_issues_search_fastupdate.up.sql"
	sql, err := migrationFiles.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if touched := pgmigrate.TouchedTables(string(sql)); len(touched) != 0 {
		t.Fatalf("%s names the tables %v in its own text, want none: it names the index", file, touched)
	}
	reading, err := pgmigrate.CensusTables(ctx, store.Pool, string(sql))
	if err != nil {
		t.Fatalf("read %s's census tables: %v", file, err)
	}
	var names []string
	for _, table := range reading {
		names = append(names, table.Name)
	}
	if !slices.Equal(names, []string{"issues"}) {
		t.Fatalf("%s touches %v, want issues alone", file, names)
	}
}
