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
// statement that writes that item's windows and by nothing else. Both halves are read as
// behaviour - an item written, indexed, written again and windowed, with ItemsOwedWindows asked
// at each step - rather than from the text of a statement.
//
// The two writes of the windows are both exercised: the one an index of the item's blocks makes
// (indexedItemState, which settles the block index in the same statement) and the one the
// window arm makes over blocks that already stand (storedItemState, which settles the marker
// alone).
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

			indexOwedItem(t, database, item.kind, id, "the insert")
			requireWindowsOwed(t, database, item.kind, id, false, "the index of the "+string(item.kind)+"'s blocks")
			requireListed(t, database, item.kind, id, false, "the indexed "+string(item.kind))
			if windows := windowIDsOf(t, database, item.kind, id); len(windows) == 0 {
				t.Fatalf("the indexed %s holds no window", item.kind)
			}

			// A write of a column the blocks are not read from owes neither arm.
			if _, err := database.Pool.Exec(ctx, item.writeElse, id); err != nil {
				t.Fatalf("write another column of the %s: %v", item.kind, err)
			}
			requireWindowsOwed(t, database, item.kind, id, false, "a write of another column of the "+string(item.kind))

			// A write of the text moves the blocks, so the windows written before it no longer
			// stand for them: the arm lists the item until the next index writes them again.
			if item.writeText != "" {
				if _, err := database.Pool.Exec(ctx, item.writeText, id); err != nil {
					t.Fatalf("write the %s's text again: %v", item.kind, err)
				}
				requireWindowsOwed(t, database, item.kind, id, true, "a write of the "+string(item.kind)+"'s text")
				requireListed(t, database, item.kind, id, true, "the rewritten "+string(item.kind))
				indexOwedItem(t, database, item.kind, id, "the rewritten text")
				requireWindowsOwed(t, database, item.kind, id, false, "the index of the rewritten "+string(item.kind))
				requireListed(t, database, item.kind, id, false, "the reindexed "+string(item.kind))
			}

			// The window arm's own write, over blocks that already stand: the window rows are
			// removed and the owner marked, as a route that removes one must mark it, and the
			// arm writes them again and clears the marker in the one statement.
			if _, err := database.Pool.Exec(ctx, `
				delete from embeddings where kind = $1 and split_part(id, '#', 1) = $2
			`, item.kind.WindowKind(), item.kind.WindowOwnerKey(id)); err != nil {
				t.Fatalf("remove the %s's window rows: %v", item.kind, err)
			}
			markWindowsOwed(t, database, item.kind, id)
			requireListed(t, database, item.kind, id, true, "the "+string(item.kind)+" whose window rows were removed")

			armTx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the window arm's transaction: %v", err)
			}
			defer armTx.Rollback(ctx)
			written, err := WriteItemWindows(ctx, armTx, item.kind, id)
			if err != nil || !written {
				t.Fatalf("write the %s's windows = %v, %v; want them written", item.kind, written, err)
			}
			if err := armTx.Commit(ctx); err != nil {
				t.Fatalf("commit the window arm's transaction: %v", err)
			}
			requireWindowsOwed(t, database, item.kind, id, false, "the window arm's write of the "+string(item.kind)+"'s windows")
			requireListed(t, database, item.kind, id, false, "the "+string(item.kind)+" the window arm windowed")
			if windows := windowIDsOf(t, database, item.kind, id); len(windows) == 0 {
				t.Fatalf("the %s holds no window after the window arm wrote them", item.kind)
			}
		})
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
// index it: its blocks, and the windows those blocks stand in, in one transaction.
func indexOwedItem(t *testing.T, database *store.Store, kind ItemKind, id, what string) {
	t.Helper()
	indexed, err := IndexOwedItemBlocks(context.Background(), database.Pool, OwedItemBlock{Kind: kind, ID: id}, askPartServer)
	if err != nil || !indexed {
		t.Fatalf("index the %s owed a block index after %s = %v, %v; want it indexed", kind, what, indexed, err)
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
