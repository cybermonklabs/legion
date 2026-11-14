package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What a document write's transaction touches, from the issue row it locks to its commit: no
// statement of it writes an embedding window. The version write records the blocks and leaves
// their windows to the block-index tick's window arm (refs.IndexBlocks, docs.windowDocument,
// INFRA-988), so the write holds `select status, closed_at is null from issues where key = $1
// for no key update` for its own rows alone. That window write was the statement INFRA-988
// measured as the write path's blocker: 9.7% of an hour's database time over 31,241 calls at a
// 114.3 ms mean, with every other writer of the issue queued behind it on that row.
//
// The work is not dropped with it: the document is left owed its windows
// (refs.DocumentsOwedWindows), since the version its blocks are indexed at - written in this
// same transaction - is no longer the state its windows were stamped from.
//
// It reads the statements rather than the time, so it fails for the shape and not for a slow
// run. The control is the shape before this change, which wrote the windows in the version
// write's own transaction, where it fails naming that statement:
//
//	--- FAIL: TestADocumentWriteWritesNoWindowUnderTheIssueRow (3.81s)
//	    statement 24, sent while the write held the row of issue DWLK-1, writes
//	    embeddings_enqueue_window:
//	            with state as (
//	                    select coalesce(v.version::text, '') as block_state
//	                      from document_block_versions v where v.artifact_id = $5::uuid
//	            ), stamped as (insert into embedding_window_states …),
//	            dropped as (delete from embeddings …)
//	            select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
//	            from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
func TestADocumentWriteWritesNoWindowUnderTheIssueRow(t *testing.T) {
	database := storetest.Open(t)
	sent := &sentStatements{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, sent, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, and a settlement no test run
		// reaches keeps its own statements out of the recording, as the approval request's
		// count does (approval_request_batch_test.go).
		identity: headerIdentity(database),
		settle:   time.Hour,
	})
	issue := createInteractionIssue(t, handler, "DWLK", "Hold the issue row", "A spec")
	sent.take()

	document := countedRequest[uploadedArtifact](t, handler, "upload notes.md to "+issue.Key,
		"/api/v1/issues/"+issue.Key+"/artifacts",
		map[string]string{"name": "notes.md", "content": "# Notes\n\nThe stored paragraph.\n"})
	statements := sent.take()
	locked := slices.IndexFunc(statements, func(statement string) bool {
		return strings.Contains(statement, "from issues where key = $1 for no key update")
	})
	if locked < 0 {
		t.Fatalf("the document write locked no issue row:\n%s", strings.Join(roundTripLines(statements), "\n"))
	}
	for index, statement := range statements[locked:] {
		for _, write := range []string{"embeddings_enqueue_window", "embedding_window_states", "delete from embeddings"} {
			if strings.Contains(statement, write) {
				t.Fatalf("statement %d, sent while the write held the row of issue %s, writes %s:\n%s",
					locked+index+1, issue.Key, write, statement)
			}
		}
	}
	if windows := documentWindowTexts(t, database, document.Artifact.ID); len(windows) != 0 {
		t.Fatalf("the write of document %s left the windows %v, want none", document.Artifact.ID, windows)
	}
	if owed := documentsOwedWindows(t, database); !slices.Contains(owed, document.Artifact.ID) {
		t.Fatalf("the write left document %s neither windowed nor owed its windows: %v", document.Artifact.ID, owed)
	}
}

// documentWindowTexts is the text of each embedding window the document holds, in window id
// order.
func documentWindowTexts(t *testing.T, database *store.Store, artifactID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select text_snapshot from embeddings
		where kind = $1 and split_part(id, '#', 1) = $2 order by id
	`, refs.DocumentWindowKind, artifactID)
	if err != nil {
		t.Fatalf("read the windows of document %s: %v", artifactID, err)
	}
	windows, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the windows of document %s: %v", artifactID, err)
	}
	return windows
}

// documentsOwedWindows is every document the block-index tick's window arm is owed.
func documentsOwedWindows(t *testing.T, database *store.Store) []string {
	t.Helper()
	owed, err := refs.DocumentsOwedWindows(context.Background(), database.Pool, "", 100)
	if err != nil {
		t.Fatalf("list the documents owed their windows: %v", err)
	}
	return owed
}
