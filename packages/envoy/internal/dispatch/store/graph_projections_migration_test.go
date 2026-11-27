package store

import (
	"context"
	"strings"
	"testing"
)

// graphProjectionsVersion is the migration that gives document_blocks, block_refs and comments
// the address of the node they project. It takes ACCESS EXCLUSIVE on all three in one
// transaction, in that order.
const graphProjectionsVersion = 20261109022248

// 20261109022248 bounds its own lock wait below deadlock_timeout. It holds document_blocks while
// it waits for comments, and a comment write that goes on to write document_blocks closes a cycle
// with it - acceptComment reads comments first (api/comment_actions.go's lockedComment) and writes
// document_blocks through AcceptSuggestion's version. The migration gives up first, applying
// nothing, before Postgres's deadlock check runs on either side: the write commits, and the
// runner's next attempt applies the migration. Under the runner's five-second bound that cycle
// aborts one side with 40P01, which the runner does not retry.
func TestMigrateGraphProjectionsGivesUpBeforeACommentWriteThatWritesBlocksDeadlocks(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, graphStoreVersion)
	logs := captureLogs(t)
	var comment, artifact string
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
	`); err != nil {
		t.Fatalf("seed an issue: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifact); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Accept it.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("seed the comment: %v", err)
	}
	writer, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment write: %v", err)
	}
	defer writer.Rollback(ctx)
	// The prologue of every change to an existing comment: its row, taken for update.
	var held string
	if err := writer.QueryRow(ctx, `select id::text from comments where id = $1::uuid for update`, comment).Scan(&held); err != nil {
		t.Fatalf("take the comment's row: %v", err)
	}
	// The runner is given the set up to this migration and not the whole set: this database is
	// seeded at the migration before it, and the later ones are no part of the cycle under test.
	// It is the runner's own path either way (Store.Migrate is migrate over every embedded
	// file), so the lock timeout, the retry and the log line it is read for are the same.
	through := migrationsThrough(t, graphProjectionsVersion)
	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(ctx, through) }()
	waitForQueuedLock(t, store, "comments", "AccessExclusiveLock")
	// The suggestion's version, which writes the document's blocks while the migration holds
	// document_blocks and waits for this transaction's comment row.
	if _, err := writer.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'b1', 'CORE-1/spec#b1', 'paragraph', '', 'Accepted.', 1, now())
	`, artifact); err != nil {
		t.Fatalf("the comment write's block write failed while the migration waited for comments: %v", err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the comment write: %v", err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("the runner should have applied 20261109022248 on a later attempt once the comment write ended, got: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "the runner tries it again") ||
		!strings.Contains(out, "migration=20261109022248_graph_projections.up.sql") || !strings.Contains(out, "attempt=1") {
		t.Fatalf("20261109022248 should have given up at its own lock timeout on its first attempt; logs:\n%s", out)
	}
	var applied bool
	if err := store.Pool.QueryRow(ctx,
		`select exists(select 1 from schema_migrations where version = $1)`, graphProjectionsVersion).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !applied {
		t.Fatal("20261109022248 was not applied by the run after the comment write ended")
	}
}
