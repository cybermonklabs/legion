package refs

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// markedItem is one kind of item as the marker's behaviour is read over it: the insert that
// writes one, a write of the text its blocks are split from (empty for a message, which is
// never edited), and a write of a column those blocks are not read from, which must owe
// nothing.
type markedItem struct {
	kind      ItemKind
	insert    string
	writeText string
	writeElse string
}

var markedItems = []markedItem{{
	kind: ItemComment,
	insert: `insert into comments (issue_key, author, body)
	         values ('CORE-1', '{"kind":"user","id":"alice"}', 'The first paragraph.' || E'\n\n' || 'The second paragraph.')
	         returning id::text`,
	writeText: `update comments set body = 'One paragraph only, rewritten.', edited_at = now() where id = $1::uuid`,
	writeElse: `update comments set resolved = true where id = $1::uuid`,
}, {
	kind: ItemMessage,
	insert: `insert into messages (issue_key, author, body)
	         values ('CORE-1', '{"kind":"user","id":"alice"}', 'A message of one paragraph.')
	         returning id::text`,
	writeElse: `update messages set target = 'inbox' where id = $1::uuid`,
}, {
	kind: ItemAsk,
	insert: `insert into asks (issue_key, author, question, options)
	         values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which transport?', '[{"label":"REST"}]')
	         returning id::text`,
	writeText: `update asks set question = 'Which transport ships at all?' where id = $1::uuid`,
	writeElse: `update asks set urgency = 'high' where id = $1::uuid`,
}}

// The marker on the three state tables (windows_owed_at, migration 20261107194001) is set by
// the trigger on every write of the text an item's blocks are split from, and cleared by the
// one statement that writes that item's windows and by nothing else. Both halves are read as
// behaviour - an item written, indexed, windowed, written again, indexed and windowed again,
// with ItemsOwedWindows asked at each step - rather than from the text of a statement.
//
// What an index of the item's blocks leaves is read here too, since it is the whole of this
// change: the index writes the blocks, clears the block arm's owed_at and leaves this marker
// standing with no window written, so the item it indexed is the window arm's to window a
// tick later (recordItemIndexed, INFRA-988). Before it, the index wrote the windows in its
// own transaction and cleared the marker in the same statement, and the two steps below that
// read an indexed item as owed and unwindowed read it as windowed and owing nothing.
func TestTheTriggerOwesAnItemItsWindowsAndWritingThemClearsIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the marker fixture's transaction: %v", err)
	}
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the marker fixture: %v", err)
	}

	for _, item := range markedItems {
		t.Run(string(item.kind), func(t *testing.T) {
			var id string
			if err := database.Pool.QueryRow(ctx, item.insert).Scan(&id); err != nil {
				t.Fatalf("insert the %s: %v", item.kind, err)
			}

			// The write of the item's text owes the window arm, as it owes the block arm.
			requireWindowsOwed(t, database, item.kind, id, true, "the insert of the "+string(item.kind))
			// It holds no block yet, so the block arm owns it and this arm lists nothing.
			requireListed(t, database, item.kind, id, false, "the "+string(item.kind)+" before its blocks are indexed")

			// The index writes the blocks and no window, and leaves the marker: the item is
			// now this arm's, and listed for it.
			indexOwedItem(t, database, item.kind, id, "the insert")
			requireWindowsOwed(t, database, item.kind, id, true, "the index of the "+string(item.kind)+"'s blocks")
			requireListed(t, database, item.kind, id, true, "the indexed "+string(item.kind))
			if windows := windowIDsOf(t, database, item.kind, id); len(windows) != 0 {
				t.Fatalf("the index of the %s wrote the windows %v; the window arm writes an item's windows", item.kind, windows)
			}

			windowItem(t, database, item.kind, id, "the index of its blocks")
			requireWindowsOwed(t, database, item.kind, id, false, "the window arm's write of the "+string(item.kind)+"'s windows")
			requireListed(t, database, item.kind, id, false, "the windowed "+string(item.kind))
			if windows := windowIDsOf(t, database, item.kind, id); len(windows) == 0 {
				t.Fatalf("the windowed %s holds no window", item.kind)
			}

			// A write of a column the blocks are not read from owes neither arm.
			if _, err := database.Pool.Exec(ctx, item.writeElse, id); err != nil {
				t.Fatalf("write another column of the %s: %v", item.kind, err)
			}
			requireWindowsOwed(t, database, item.kind, id, false, "a write of another column of the "+string(item.kind))

			// A write of the text moves the blocks, so the windows written before it no longer
			// stand for them: the arm lists the item until it writes them again, and the index
			// of the new blocks does not take it off that list.
			if item.writeText != "" {
				if _, err := database.Pool.Exec(ctx, item.writeText, id); err != nil {
					t.Fatalf("write the %s's text again: %v", item.kind, err)
				}
				requireWindowsOwed(t, database, item.kind, id, true, "a write of the "+string(item.kind)+"'s text")
				requireListed(t, database, item.kind, id, true, "the rewritten "+string(item.kind))
				indexOwedItem(t, database, item.kind, id, "the rewritten text")
				requireWindowsOwed(t, database, item.kind, id, true, "the index of the rewritten "+string(item.kind))
				requireListed(t, database, item.kind, id, true, "the reindexed "+string(item.kind))
				windowItem(t, database, item.kind, id, "the index of the rewritten text")
				requireWindowsOwed(t, database, item.kind, id, false, "the window arm's write over the rewritten "+string(item.kind))
				requireListed(t, database, item.kind, id, false, "the rewindowed "+string(item.kind))
			}

			// The window rows are removed and the owner marked, as a route that removes one
			// must mark it, and the arm writes them again and clears the marker in the one
			// statement.
			if _, err := database.Pool.Exec(ctx, `
				delete from embeddings where kind = $1 and split_part(id, '#', 1) = $2
			`, item.kind.WindowKind(), item.kind.WindowOwnerKey(id)); err != nil {
				t.Fatalf("remove the %s's window rows: %v", item.kind, err)
			}
			markWindowsOwed(t, database, item.kind, id)
			requireListed(t, database, item.kind, id, true, "the "+string(item.kind)+" whose window rows were removed")

			windowItem(t, database, item.kind, id, "the removal of its window rows")
			requireWindowsOwed(t, database, item.kind, id, false, "the window arm's write of the "+string(item.kind)+"'s windows")
			requireListed(t, database, item.kind, id, false, "the "+string(item.kind)+" the window arm windowed")
			if windows := windowIDsOf(t, database, item.kind, id); len(windows) == 0 {
				t.Fatalf("the %s holds no window after the window arm wrote them", item.kind)
			}
		})
	}
}

// A comment the index holds at no block keeps no marker. A comment whose body is empty holds
// none - api's createCommentFor takes such a comment when it carries a suggestion - and so
// does one whose last block an edit removed, whose block rows are all `position is null`.
// Neither holds a current block row, so the semi-join this arm's listing had before this round
// dropped it while the index went on marking it, and WriteItemWindows, the only statement that
// clears the marker, runs only for an item the arm listed: the row stayed marked for ever, an
// entry in the partial index every later pass paid a probe for and no pass could ever remove.
// It is the item side of the hole INFRA-988 measured on documents.
//
// Both halves are read as behaviour: the arm lists the item once the index has reached it -
// and not before, since an item no block-aware build has indexed is the block arm's - and the
// write of its windows, of which it has none, deletes the windows its old text left and clears
// the marker. A comment is the kind this is read over because it is the kind that can hold no
// text: a message's body and an ask's question are required of the writer (api's messages.go,
// validateAskQuestion).
func TestACommentIndexedAtNoBlockIsListedAndWindowed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the marker fixture's transaction: %v", err)
	}
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the marker fixture: %v", err)
	}
	const insertComment = `insert into comments (issue_key, author, body)
	                       values ('CORE-1', '{"kind":"user","id":"alice"}', $1) returning id::text`

	// A comment with no text of its own, which holds no block from its first indexing.
	var empty string
	if err := database.Pool.QueryRow(ctx, insertComment, "").Scan(&empty); err != nil {
		t.Fatalf("insert the comment whose body is empty: %v", err)
	}
	requireWindowsOwed(t, database, ItemComment, empty, true, "the insert of the empty comment")
	requireListed(t, database, ItemComment, empty, false, "the empty comment before any index reaches it")

	indexOwedItem(t, database, ItemComment, empty, "the insert of the empty comment")
	requireWindowsOwed(t, database, ItemComment, empty, true, "the index of the empty comment")
	requireListed(t, database, ItemComment, empty, true, "the indexed empty comment")

	windowItem(t, database, ItemComment, empty, "the index of the empty comment")
	requireWindowsOwed(t, database, ItemComment, empty, false, "the window arm's write over the empty comment")
	requireListed(t, database, ItemComment, empty, false, "the empty comment the window arm windowed")
	if windows := windowIDsOf(t, database, ItemComment, empty); len(windows) != 0 {
		t.Fatalf("the empty comment holds the windows %v, want none", windows)
	}

	// A comment an edit empties holds windows of text it no longer has, and the arm's write
	// over it is what deletes them (writeWindows' `dropped`).
	var emptied string
	if err := database.Pool.QueryRow(ctx, insertComment, "The paragraph the edit removes.").Scan(&emptied); err != nil {
		t.Fatalf("insert the comment the edit empties: %v", err)
	}
	indexOwedItem(t, database, ItemComment, emptied, "the insert of the comment the edit empties")
	windowItem(t, database, ItemComment, emptied, "the index of the comment the edit empties")
	if windows := windowIDsOf(t, database, ItemComment, emptied); len(windows) == 0 {
		t.Fatalf("the comment the edit empties holds no window to be left behind")
	}

	if _, err := database.Pool.Exec(ctx,
		`update comments set body = '', edited_at = now() where id = $1::uuid`, emptied); err != nil {
		t.Fatalf("empty the comment: %v", err)
	}
	requireWindowsOwed(t, database, ItemComment, emptied, true, "the edit that emptied the comment")
	indexOwedItem(t, database, ItemComment, emptied, "the edit that emptied the comment")
	requireWindowsOwed(t, database, ItemComment, emptied, true, "the index of the emptied comment")
	requireListed(t, database, ItemComment, emptied, true, "the emptied comment, whose only block the index removed")

	windowItem(t, database, ItemComment, emptied, "the index of the emptied comment")
	requireWindowsOwed(t, database, ItemComment, emptied, false, "the window arm's write over the emptied comment")
	requireListed(t, database, ItemComment, emptied, false, "the emptied comment the window arm windowed")
	if windows := windowIDsOf(t, database, ItemComment, emptied); len(windows) != 0 {
		t.Fatalf("the emptied comment holds the windows %v of the text the edit removed, want none", windows)
	}
}

// requireWindowsOwed reads the item's state row and fails unless its marker stands as owed says.
func requireWindowsOwed(t *testing.T, database *store.Store, kind ItemKind, id string, owed bool, after string) {
	t.Helper()
	tables, err := kind.tables()
	if err != nil {
		t.Fatalf("tables of %s: %v", kind, err)
	}
	var marked bool
	if err := database.Pool.QueryRow(context.Background(),
		`select windows_owed_at is not null from `+tables.state+` where `+tables.item+` = $1::uuid`, id,
	).Scan(&marked); err != nil {
		t.Fatalf("read the window marker of %s %s: %v", kind, id, err)
	}
	if marked != owed {
		t.Fatalf("after %s, %s %s is owed windows = %v, want %v", after, kind, id, marked, owed)
	}
}

// requireListed fails unless ItemsOwedWindows lists the item exactly when listed says.
func requireListed(t *testing.T, database *store.Store, kind ItemKind, id string, listed bool, after string) {
	t.Helper()
	owed, err := ItemsOwedWindows(context.Background(), database.Pool, kind, "", 100)
	if err != nil {
		t.Fatalf("list the %ss owed windows: %v", kind, err)
	}
	if got := slices.Contains(owed, id); got != listed {
		t.Fatalf("after %s, the %ss owed windows %v list %s = %v, want %v", after, kind, owed, id, got, listed)
	}
}

// markWindowsOwed marks the item owed its windows as a route that removes one of its window rows
// has to: the marker is what the arm reads, and nothing else says the row has gone.
func markWindowsOwed(t *testing.T, database *store.Store, kind ItemKind, id string) {
	t.Helper()
	tables, err := kind.tables()
	if err != nil {
		t.Fatalf("tables of %s: %v", kind, err)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update `+tables.state+` set windows_owed_at = coalesce(windows_owed_at, now()) where `+tables.item+` = $1::uuid`,
		id); err != nil {
		t.Fatalf("mark %s %s owed its windows: %v", kind, id, err)
	}
}

// indexOwedItem runs the block-index tick's owed arm over the item, which must find it owed and
// index it: its blocks alone, in a transaction of its own, which leaves the windows those
// blocks stand in to the window arm (recordItemIndexed).
func indexOwedItem(t *testing.T, database *store.Store, kind ItemKind, id, what string) {
	t.Helper()
	indexed, err := IndexOwedItemBlocks(context.Background(), database.Pool, OwedItemBlock{Kind: kind, ID: id}, askPartServer)
	if err != nil || !indexed {
		t.Fatalf("index the %s owed a block index after %s = %v, %v; want it indexed", kind, what, indexed, err)
	}
}

// windowItem runs the block-index tick's window arm over the item, in a transaction of its
// own as docs.windowItem runs it, which must find it windowable and write its windows.
func windowItem(t *testing.T, database *store.Store, kind ItemKind, id, what string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the window arm's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	written, err := WriteItemWindows(ctx, tx, kind, id)
	if err != nil || !written {
		t.Fatalf("window the %s after %s = %v, %v; want its windows written", kind, what, written, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the window arm's transaction: %v", err)
	}
}

// windowIDsOf is the ids of the window rows the item holds.
func windowIDsOf(t *testing.T, database *store.Store, kind ItemKind, id string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id from embeddings where kind = $1 and split_part(id, '#', 1) = $2 order by id
	`, kind.WindowKind(), kind.WindowOwnerKey(id))
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, id, err)
	}
	windows, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, id, err)
	}
	return windows
}
