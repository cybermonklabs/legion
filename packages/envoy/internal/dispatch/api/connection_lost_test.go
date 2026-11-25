package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// insertEditBody is the body of an edit that appends a paragraph, which changes the document, so
// the edit writes a version and reads its two reports in its transaction (editArtifact).
func insertEditBody(markdown string) map[string]any {
	body := insertAtEnd(markdown)
	body["actor"] = sessionActor()
	return body
}

// INFRA-592's ten-times freeze, reproduced: the database stops answering as a document edit rolls
// back the savepoint of one of its reports - in the freeze the open approval request's
// (movedApprovalRequestAdvice); here the superseded approval's, since both roll back through one
// helper (adviceReport) - and the rollback's own bound (store.RollbackContext) passes five seconds
// later. pgconn ends the rollback by closing the connection. The rollback's error was dropped, so
// the edit went on to its settlement record, which pgconn refused `conn closed`, and the edit was
// answered 500 INTERNAL ("record the document's pending settlement: conn closed", 7,755 ms, five
// seconds after the freeze began). The edit is answered 503 DATABASE_BUSY with Retry-After
// instead, at the rollback that failed: nothing of it committed, so its client may send it again.
//
// The socket is frozen, not stalled: its reads answer nothing until pgx puts the rollback's own
// deadline on it (storetest.NewFrozenSocket), so the bound that ends the call is the one that
// ended it in the freeze. The freeze is armed from inside the report (the advice seam, which the
// report runs before its reads), so the savepoint rollback it freezes is that report's own,
// whatever savepoints the edit's transaction opened and rolled back before it.
func TestAnEditWhoseReportRollbackRunsOutItsBoundIsRefusedDatabaseBusy(t *testing.T) {
	t.Parallel()
	socket := storetest.NewFrozenSocket(store.RequestsPool)
	var editing atomic.Bool
	handler, _, _ := newTestServer(t, testServerOptions{
		dialRequestPool: socket.Dial,
		settle:          time.Hour,
		adviceQueryHook: func(_ context.Context, _ pgx.Tx, query string) error {
			if query == "superseded_approval" && editing.Load() {
				socket.StallAfter("rollback to savepoint ")
			}
			return nil
		},
	})
	issue := createInteractionIssue(t, handler, "TEST", "The database stops answering a report's rollback", "Before.")

	editing.Store(true)
	started := time.Now()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
		insertEditBody("Written as the database stops answering."))
	waited := time.Since(started)

	if !socket.Fired() {
		t.Fatal("the socket never froze, so the report's rollback was answered as any other")
	}
	if waited < store.RollbackTimeout {
		t.Fatalf("answered after %v, before the rollback's own bound of %v passed", waited, store.RollbackTimeout)
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
}

// A write whose transaction's connection is closed under it before its COMMIT committed nothing:
// Postgres rolls back the transaction of a backend that ends. It is answered 503 DATABASE_BUSY
// with Retry-After (store.ConnectionLost), and never 500 INTERNAL, which told the client nothing
// to do and the operator that a handler had failed. The edit's backend is ended with
// pg_terminate_backend from inside the edit's transaction, at its first report: the report's read
// meets Postgres's FATAL, and pgconn refuses every later call on the connection `conn closed`
// (pgconn.ErrConnClosed), the report's rollback among them.
func TestAnEditWhoseBackendIsTerminatedBeforeItsCommitIsRefusedDatabaseBusy(t *testing.T) {
	t.Parallel()
	var terminated atomic.Bool
	var backend int32
	// The hook terminates the backend through the test's own handle on the store, which the
	// server is built with below.
	var database *store.Store
	handler, opened, _, _ := newRegisteredTestServer(t, testServerOptions{
		settle: time.Hour,
		adviceQueryHook: func(ctx context.Context, q pgx.Tx, query string) error {
			if query != "superseded_approval" || terminated.Swap(true) {
				return nil
			}
			if err := q.QueryRow(ctx, `select pg_backend_pid()`).Scan(&backend); err != nil {
				t.Errorf("read the edit's backend: %v", err)
				return nil
			}
			var ended bool
			if err := database.Pool.QueryRow(context.Background(), `select pg_terminate_backend($1, 5000)`, backend).Scan(&ended); err != nil || !ended {
				t.Errorf("terminate the edit's backend %d: ended=%v err=%v", backend, ended, err)
			}
			return nil
		},
	})
	database = opened
	issue := createInteractionIssue(t, handler, "TEST", "The edit's backend is terminated", "Before.")

	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
		insertEditBody("Written as its backend is terminated."))

	if !terminated.Load() {
		t.Fatal("the edit never reached its first report, so its backend was never terminated")
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
	if versions := specVersionCount(t, database, issue.PrimaryArtifactID); versions != 1 {
		t.Fatalf("the document holds %d versions, want the seeded one alone: the refused edit committed", versions)
	}
}

// A write whose connection ends under its COMMIT may have committed: the server sent the COMMIT,
// and nothing it holds says whether Postgres applied it before the session ended. It stays 500
// COMMIT_OUTCOME_UNKNOWN with no Retry-After (store.ErrCommitTimeout), never the retryable 503 a
// connection lost before the COMMIT is answered with: a client that sent it again could write
// twice. The COMMIT is held at the test's lock (holdCommits), and its backend is ended with
// pg_terminate_backend while it waits there.
func TestAnEditWhoseBackendIsTerminatedDuringItsCommitIsAnOutcomeNobodyKnows(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "The edit's backend is terminated at its COMMIT", "Before.")
	release := holdCommits(t, database)

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
			insertEditBody("Written as its COMMIT's backend is terminated."))
	}()
	waitForHeldCommit(t, database)
	var backend int32
	var ended bool
	if err := database.Pool.QueryRow(context.Background(), `
		select pid, pg_terminate_backend(pid, 5000) from pg_stat_activity
		where datname = current_database() and lower(query) = 'commit' and wait_event = 'advisory' and application_name = $1
	`, "dispatch-"+store.RequestsPool).Scan(&backend, &ended); err != nil || !ended {
		t.Fatalf("terminate the COMMIT's backend %d: ended=%v err=%v", backend, ended, err)
	}
	release()
	response := awaitResponse(t, answered)

	if response.Code != http.StatusInternalServerError || refusalCode(response.Body.Bytes()) != "COMMIT_OUTCOME_UNKNOWN" {
		t.Fatalf("status=%d body=%s, want 500 COMMIT_OUTCOME_UNKNOWN", response.Code, response.Body.String())
	}
	if retry := response.Result().Header.Get("Retry-After"); retry != "" {
		t.Fatalf("Retry-After = %q, want none: the commit may have landed", retry)
	}
}
