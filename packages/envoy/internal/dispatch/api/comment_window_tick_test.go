package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A comment's write records its blocks and writes no embedding window: the windows of those
// blocks are the block-index tick's window arm's to write, in a transaction of its own
// (refs.recordItemIndexed, docs.windowItem, INFRA-988). So the write answers with the comment
// holding no window and listed as owed them (refs.ItemsOwedWindows), one pass of the
// reconciliation writes them, and once the embed queue has run every block of the comment is
// embedded - the comment is found by meaning search one tick after it is written rather than
// the moment its write answers.
//
// The control is the shape before this change, which wrote the windows inside the write's own
// transaction. Run against it, this test fails at its first assertion, naming the window the
// write left:
//
//	--- FAIL: TestACommentLeavesItsWindowsToTheBlockIndexTick (6.34s)
//	    the write of comment 724343d1-fac5-43d1-ad22-fc57840a794e left the windows [b1],
//	    want none: the block-index tick's window arm writes an item's windows
//
// The assertion under it never runs there, and would fail too: the statement that wrote that
// window cleared windows_owed_at in the same breath, so the arm was owed nothing.
func TestACommentLeavesItsWindowsToTheBlockIndexTick(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "CWIN", "Window the comment", "A spec")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "The first paragraph.\n\nThe second paragraph.",
	}, "alice")

	ownerKey := refs.ItemComment.WindowOwnerKey(comment.ID)
	if windows := itemWindowIDs(t, database, refs.ItemComment, comment.ID); len(windows) != 0 {
		t.Fatalf("the write of comment %s left the windows %v, want none: the block-index tick's window arm writes an item's windows",
			comment.ID, windows)
	}
	// Its blocks are indexed, so it is this arm's to window and not the block arm's to index.
	blocks := commentBlockIDs(t, database, comment.ID)
	if len(blocks) != 2 {
		t.Fatalf("the write of comment %s indexed the blocks %v, want its two paragraphs", comment.ID, blocks)
	}
	if owed := commentsOwedWindows(t, database); !slices.Contains(owed, comment.ID) {
		t.Fatalf("the window arm is not owed comment %s, which it has yet to window: %v", comment.ID, owed)
	}

	tick, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
	if err != nil {
		t.Fatalf("one pass of the block-index reconciliation: %v", err)
	}
	if tick.Windows < 1 {
		t.Fatalf("the pass windowed %d owners (%+v), want the comment among them", tick.Windows, tick)
	}
	windows := itemWindowIDs(t, database, refs.ItemComment, comment.ID)
	if len(windows) == 0 {
		t.Fatalf("one pass of the block-index reconciliation left comment %s unwindowed", comment.ID)
	}
	if owed := commentsOwedWindows(t, database); slices.Contains(owed, comment.ID) {
		t.Fatalf("the windowed comment %s is still owed its windows: %v", comment.ID, owed)
	}

	processAllPending(t, database, &fakeEmbedder{})
	states, err := refs.BlockEmbeddingStates(ctx, database.Pool, refs.ItemComment.WindowKind(), ownerKey)
	if err != nil {
		t.Fatalf("read where meaning search stands on the blocks of comment %s: %v", comment.ID, err)
	}
	for _, block := range blocks {
		if states[block] != model.BlockEmbedded {
			t.Fatalf("block %s of comment %s is %q, want %q once the tick has windowed it and the queue has run",
				block, comment.ID, states[block], model.BlockEmbedded)
		}
	}
}

// What the write's transaction touches, from the issue row it locks to its commit: no statement
// of it writes an embedding window. That lock is `select status, closed_at is null from issues
// where key = $1 for no key update` (issueOpenStatus under ownerRowLock), the statement every
// other writer of the issue waits on, and the window write held it for 114.3 ms in the mean on
// the load-test deployment (INFRA-988). The work is not dropped with it: the comment is left
// owed its windows for the tick's window arm.
//
// It reads the statements rather than the time, so it fails for the shape and not for a slow
// run. The control is the shape before this change, which wrote the windows in the write's own
// transaction, where it fails naming that statement:
//
//	--- FAIL: TestACommentWriteWritesNoWindowUnderTheIssueRow (0.77s)
//	    statement 11, sent while the write held the row of issue CWLK-1, writes
//	    embeddings_enqueue_window:
//	            with state as (
//	                    update comment_block_state s set revision = $6, next_block = $7,
//	                           body_sha256 = $8, owed_at = null, windows_owed_at = null
//	                    where s.comment_id = $5::uuid returning …
//	            ), stamped as (insert into embedding_window_states …),
//	            dropped as (delete from embeddings …)
//	            select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
//	            from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
func TestACommentWriteWritesNoWindowUnderTheIssueRow(t *testing.T) {
	database := storetest.Open(t)
	sent := &sentStatements{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, sent, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so what is recorded is the
		// request's own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the issue's spec is database work of its own on the same pool; a
		// delay no test run reaches keeps it out of the recording.
		settle: time.Hour,
	})
	issue := createInteractionIssue(t, handler, "CWLK", "Hold the issue row", "A spec")
	sent.take()

	comment := countedRequest[model.Comment](t, handler, "create the comment",
		"/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "The first paragraph.\n\nThe second paragraph."})
	statements := sent.take()
	locked := slices.IndexFunc(statements, func(statement string) bool {
		return strings.Contains(statement, "from issues where key = $1 for no key update")
	})
	if locked < 0 {
		t.Fatalf("the comment's write locked no issue row:\n%s", strings.Join(roundTripLines(statements), "\n"))
	}
	// Every statement from that lock to the commit, a pipelined batch's own included.
	for index, statement := range statements[locked:] {
		for _, write := range []string{"embeddings_enqueue_window", "embedding_window_states", "delete from embeddings"} {
			if strings.Contains(statement, write) {
				t.Fatalf("statement %d, sent while the write held the row of issue %s, writes %s:\n%s",
					locked+index+1, issue.Key, write, statement)
			}
		}
	}
	if windows := itemWindowIDs(t, database, refs.ItemComment, comment.ID); len(windows) != 0 {
		t.Fatalf("the write of comment %s left the windows %v, want none", comment.ID, windows)
	}
	if owed := commentsOwedWindows(t, database); !slices.Contains(owed, comment.ID) {
		t.Fatalf("the write left comment %s neither windowed nor owed its windows: %v", comment.ID, owed)
	}
}

// countedRequest sends one write through the handler on a request the traced pool counts
// (countedRead, issue_read_batch_test.go) and answers what the route returned: path is the
// route, payload the JSON body it is sent, and subject names the write in a failure. Every
// route this is used for answers 201, so any other status is a request the caller did not mean
// to make.
func countedRequest[T any](t *testing.T, handler http.Handler, subject, path string, payload any) T {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode %s: %v", subject, err)
	}
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s", subject, response.Code, response.Body.String())
	}
	return decodeBody[T](t, response)
}

// sentStatements records every statement a counted request sends, the statements inside a
// pipelined batch included: roundTrips counts a batch as one round trip and never names what it
// holds (issue_read_batch_test.go), and a write hidden in one would be a write this test must
// see. It is that counter with its two batch calls answered otherwise: the batch itself is no
// statement, and each statement inside it is one. take, record and what a request runs outside
// a batch are the counter's own.
type sentStatements struct{ roundTrips }

func (s *sentStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (s *sentStatements) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	s.record(ctx, data.SQL)
}

// itemWindowIDs is the id suffix of each embedding window the item holds, in id order.
func itemWindowIDs(t *testing.T, database *store.Store, kind refs.ItemKind, itemID string) []string {
	t.Helper()
	ownerKey := kind.WindowOwnerKey(itemID)
	rows, err := database.Pool.Query(context.Background(), `
		select id from embeddings where kind = $1 and split_part(id, '#', 1) = $2 order by id
	`, kind.WindowKind(), ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, itemID, err)
	}
	windows, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, itemID, err)
	}
	for index, id := range windows {
		windows[index] = strings.TrimPrefix(id, ownerKey+"#")
	}
	return windows
}

// commentBlockIDs is the block id of each current block the index holds for the comment, in
// body order.
func commentBlockIDs(t *testing.T, database *store.Store, commentID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id from comment_blocks where comment_id = $1 and position is not null order by position
	`, commentID)
	if err != nil {
		t.Fatalf("read the blocks of comment %s: %v", commentID, err)
	}
	blocks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the blocks of comment %s: %v", commentID, err)
	}
	return blocks
}

// commentsOwedWindows is every comment the block-index tick's window arm is owed.
func commentsOwedWindows(t *testing.T, database *store.Store) []string {
	t.Helper()
	owed, err := refs.ItemsOwedWindows(context.Background(), database.Pool, refs.ItemComment, "", 100)
	if err != nil {
		t.Fatalf("list the comments owed their windows: %v", err)
	}
	return owed
}
