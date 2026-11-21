package textversions

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// TestAFillAndADeleteOfAPendingInsertMeetOnTheIssuesLock holds the fill's write of an issue row
// whose document version holds a pending insert, and the delete of that insert's comment, to the
// lock of the issue's owner (maskPlan): whichever takes it first, no row of the version keeps the
// insert's words once both commit. The issue's primary document's version 2 holds the insert
// (writeInsertVersion), and its comment stands (writeSuggestion). The fill's side is a batch's,
// in one transaction: the versions of the log decided, then the plan of the version's text read
// outside any lock, which still holds the word, then the lock and the check again
// (lockAndRecheck), then the insert.
//
//   - The delete first: a delete holds the issue's lock and has deleted the comment, uncommitted,
//     when the fill asks for the lock, which waits for it (storetest.AwaitOwnerLockWaiter); once
//     the delete commits, the fill renders the version again and writes a row of no word of it.
//   - The fill first: the fill holds the lock and has written the row, holding the word as it
//     stood, when the delete asks for the lock, which waits for it; once the fill commits, the
//     delete rewrites the row it wrote, and no row holds the word.
func TestAFillAndADeleteOfAPendingInsertMeetOnTheIssuesLock(t *testing.T) {
	t.Run("the delete first", func(t *testing.T) {
		database, document, suggestion, plan, fill := planPendingInsert(t)
		deleting, err := beginSuggestionDelete(t, database, document, suggestion)
		if err != nil {
			t.Fatal(err)
		}
		answered := make(chan error, 1)
		go func() {
			_, err := plan.lockAndRecheck(context.Background(), fill.tx)
			answered <- err
		}()
		if err, early, _ := storetest.AwaitOwnerLockWaiter(t, database, "the fill's lock of the issue", answered); early {
			t.Fatalf("the fill took the issue's lock while the delete held it (%v)", err)
		}
		if err := deleting.Commit(t.Context()); err != nil {
			t.Fatalf("commit the delete: %v", err)
		}
		if err := <-answered; err != nil {
			t.Fatalf("the fill's lock and check: %v", err)
		}
		fill.write(t, plan)
		if err := fill.tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit the fill's batch: %v", err)
		}
		requireNoRowMatches(t, database, 1)
	})

	t.Run("the fill first", func(t *testing.T) {
		database, document, suggestion, plan, fill := planPendingInsert(t)
		if _, err := plan.lockAndRecheck(t.Context(), fill.tx); err != nil {
			t.Fatalf("the fill's lock and check: %v", err)
		}
		fill.write(t, plan)
		var matching int
		if err := fill.tx.QueryRow(t.Context(),
			`select count(*) from search_text_versions where search @@ plainto_tsquery('english', $1)`, insertedWord).Scan(&matching); err != nil {
			t.Fatalf("count the fill's rows matching %q: %v", insertedWord, err)
		}
		if matching != 1 {
			t.Fatalf("%d rows the fill wrote while the comment stood match the insert's word %q, want 1, so the check of the delete's rewrite cannot fail", matching, insertedWord)
		}
		answered := make(chan error, 1)
		go func() {
			deleting, err := beginSuggestionDelete(t, database, document, suggestion)
			if err == nil {
				err = deleting.Commit(context.Background())
			}
			answered <- err
		}()
		if err, early, _ := storetest.AwaitOwnerLockWaiter(t, database, "the delete's lock of the issue", answered); early {
			t.Fatalf("the delete took the issue's lock while the fill held it (%v)", err)
		}
		if err := fill.tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit the fill's batch: %v", err)
		}
		if err := <-answered; err != nil {
			t.Fatalf("the delete: %v", err)
		}
		requireNoRowMatches(t, database, 1)
	})
}

// fillSide is the transaction a test writes a fill's batch in, and the versions it decided.
type fillSide struct {
	tx       pgx.Tx
	versions []version
}

// write writes the decided versions in the fill's transaction, an issue row of a version that
// holds an insert tree from the plan's text of it (insertVersions), and commits nothing.
func (f fillSide) write(t *testing.T, plan maskPlan) {
	t.Helper()
	if err := insertVersions(t.Context(), f.tx, f.versions, plan.masked(), false); err != nil {
		t.Fatalf("write the fill's rows: %v", err)
	}
}

// planPendingInsert writes textversionstest.Issue, whose primary document's version 2 holds a
// pending insert, and the insert's standing comment, and begins the fill's side: the log's
// versions decided and the plan of the versions holding an insert tree read (readSpecTrees,
// readMaskPlan), with no lock taken. It fails t unless the plan holds version 2's text with the
// insert's word.
func planPendingInsert(t *testing.T) (*store.Store, string, writtenComment, maskPlan, fillSide) {
	t.Helper()
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	createIssue(t, database, broker, 1, "Rotate the signing keys", "The brown fox jumps")
	document := primaryDocument(t, database, textversionstest.Issue)
	writeInsertVersion(t, database, broker, document, 2)
	suggestion := writeSuggestion(t, database, broker, document)
	var through int64
	if err := database.Pool.QueryRow(ctx, `select max(id) from events`).Scan(&through); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	batch := readBatch(t, database, 0, through)

	tx, _ := textversionstest.BeginTransaction(t, database)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	got, _, err := decideVersions(ctx, tx, slog.New(slog.NewTextHandler(io.Discard, nil)), &currentRules, batch, batchBytes)
	if err != nil {
		t.Fatalf("decide the versions: %v", err)
	}
	trees, err := readSpecTrees(ctx, tx, got.Versions)
	if err != nil {
		t.Fatalf("read the versions' insert trees: %v", err)
	}
	plan, err := readMaskPlan(ctx, tx, trees)
	if err != nil {
		t.Fatalf("read the versions' texts: %v", err)
	}
	text, planned := plan.texts[specVersion{artifact: document, number: 2}]
	if !planned || !strings.Contains(text, insertedWord) {
		t.Fatalf("the plan holds version 2 (%v) with text %q; want the text with the standing insert's word %q", planned, text, insertedWord)
	}
	return database, document, suggestion, plan, fillSide{tx: tx, versions: got.Versions}
}

// beginSuggestionDelete deletes suggestion's comment as this build's delete does, up to its
// commit, in a transaction it answers: the lock of the comment's owner (store.LockOwner), the
// versions of document holding the insert's words masked as they read once it commits
// (docs.MaskedVersionsOnDelete), the body it clears kept in post_body_history, the comment
// cleared and marked deleted, and the keyword rows of those versions rewritten (the statement
// store.QueueTextVersionsMask queues). A test's end rolls back a transaction it did not commit.
func beginSuggestionDelete(t *testing.T, database *store.Store, document string, suggestion writtenComment) (pgx.Tx, error) {
	ctx := context.Background()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin the delete: %w", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := store.LockOwner(ctx, tx, store.IssueOwner(textversionstest.Issue)); err != nil {
		return nil, fmt.Errorf("lock the comment's owner: %w", err)
	}
	numbers, texts, err := docs.MaskedVersionsOnDelete(ctx, tx, document, insertMark, suggestion.id)
	if err != nil {
		return nil, fmt.Errorf("mask the versions holding the insert: %w", err)
	}
	for _, statement := range []string{
		`insert into post_body_history (comment_id, body, edited_by)
		 select id, body, '{"kind":"user","id":"alice"}' from comments where id = $1::uuid`,
		`update comments set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}' where id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, statement, suggestion.id); err != nil {
			return nil, fmt.Errorf("delete the comment: %w", err)
		}
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		store.QueueTextVersionsMask(b, document, numbers, texts)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("rewrite the keyword rows of the versions holding the insert: %w", err)
	}
	return tx, nil
}

// requireNoRowMatches fails t unless the keyword table holds want issue rows of version 2 and no
// row matches the insert's word.
func requireNoRowMatches(t *testing.T, database *store.Store, want int) {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from search_text_versions where kind = 'issue' and spec_version = 2`).Scan(&rows); err != nil {
		t.Fatalf("count the issue rows of version 2: %v", err)
	}
	if rows != want {
		t.Fatalf("the table holds %d issue rows of version 2, want %d", rows, want)
	}
	if matching := rowsMatching(t, database, insertedWord); matching != 0 {
		t.Fatalf("%d rows match the deleted insert's word %q, want none", matching, insertedWord)
	}
}
