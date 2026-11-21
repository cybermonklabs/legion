package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// backendState is the state pg_stat_activity holds for backend pid, and whether that backend is
// still connected at all. A connection pgx killed rather than rolled back has no row here: its
// socket was closed under the server, so Postgres ended the backend.
func backendState(t *testing.T, database *store.Store, pid int32) (state string, connected bool) {
	t.Helper()
	err := database.Pool.QueryRow(context.Background(), `
		select coalesce(state, '') from pg_stat_activity where pid = $1
	`, pid).Scan(&state)
	switch {
	case err == nil:
		return state, true
	case errors.Is(err, pgx.ErrNoRows):
		return "", false
	default:
		t.Fatalf("read the state of backend %d: %v", pid, err)
		return "", false
	}
}

// A write whose client goes away while its transaction is open ends that transaction at once:
// nothing of the write is committed, and the connection it held goes back to the pool out of its
// transaction rather than being destroyed.
//
// The cancel lands inside the transaction, in a document edit's read phase, at the first of its
// two document reports (supersededApprovalAdvice). There the edit holds the document's writer gate,
// its owner lock and its request-pool connection in a transaction, and its version and event are
// queued on the document ledger, not yet written.
//
// What the write does then is all-or-nothing and immediate: a request already gone reaches no
// commit - its next statement is refused on the cancelled context, and the commit is not started
// for it either (store.CommitRefusal) - so no version lands, and the deferred rollback sends its
// ROLLBACK on a context of its own (store.RollbackContext), so the backend leaves its transaction
// and the pooled connection survives. Before this, the commit ran on anyway - it is detached from
// the client by design - and committed a write whose answer nobody would read, holding every one
// of those locks for as long as it took.
func TestAWriteWhoseClientGoesAwayEndsItsTransactionAtOnce(t *testing.T) {
	var canceled context.CancelFunc
	var backend int32
	entered := make(chan struct{})
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		settle: time.Hour,
		adviceQueryHook: func(ctx context.Context, q pgx.Tx, query string) error {
			if query != "superseded_approval" {
				return nil
			}
			if err := q.QueryRow(ctx, `select pg_backend_pid()`).Scan(&backend); err != nil {
				return err
			}
			canceled()
			close(entered)
			return nil
		},
	})
	issue := createInteractionIssue(t, handler, "TEST", "A write whose client leaves", "Before.")

	data, err := json.Marshal(map[string]any{
		"ops":     []map[string]string{{"op": "insert", "markdown": "Written by a client that leaves.", "after": "end"}},
		"summary": "The edit nobody waits for",
		"actor":   adviceSessionActor("session-leaves"),
	})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled = cancel
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", bytes.NewReader(data)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer agent-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		answered <- response
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never reached its superseded-approval advice")
	}

	if response := awaitResponse(t, answered); response.Body.Len() != 0 {
		t.Fatalf("the write whose client left was answered %d %s, want nothing written",
			response.Code, response.Body.String())
	}
	// The handler has returned, so its deferred rollback has run. The backend is out of its
	// transaction and still connected: a rollback that was never sent would have taken the
	// connection with it.
	pollUntil(t, 5*time.Second, "the cancelled write's backend leaves its transaction", func() bool {
		state, connected := backendState(t, database, backend)
		return connected && state != "idle in transaction"
	})
	if state, connected := backendState(t, database, backend); !connected {
		t.Fatalf("the cancelled write's backend %d is gone: its ROLLBACK was never sent and the pooled connection was destroyed", backend)
	} else if state == "idle in transaction" {
		t.Fatalf("the cancelled write's backend %d is %q", backend, state)
	}
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, issue.PrimaryArtifactID).Scan(&versions); err != nil {
		t.Fatalf("count the document's versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("the document holds %d versions after a write whose client left, want the seeded one alone: the write committed for a client that had gone", versions)
	}
}

// Write advice does not hold the event horizon: it runs in the write's read phase (writeAdvice),
// before the write's first statement gives its transaction an id, so a reader waiting for a
// horizon proven after the advice began (events.Horizon.Await, the event stream relay's start, a
// `GET /api/v1/references?since=` read) waits on nothing the advice does, however long its five
// to nine round trips take.
//
// The hook holds the session's open-asks query, inside the comment's transaction, until the test
// releases it. Await must answer meanwhile, at or above every event committed before it. Run
// after the write's statements, as the advice once was, that hold would keep a transaction with
// an id open below Await's observation, and Await could not answer until it ended.
func TestWriteAdviceDoesNotHoldTheEventHorizon(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		settle: time.Hour,
		adviceQueryHook: func(ctx context.Context, q pgx.Tx, query string) error {
			if query != "your_open_asks" {
				return nil
			}
			close(entered)
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
			return nil
		},
	})
	issue := createInteractionIssue(t, handler, "TEST", "Advice and the horizon", "A spec")

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body":  "A comment whose advice is held",
			"actor": adviceSessionActor("session-horizon"),
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the comment never reached its open-asks advice query")
	}

	// The comment's transaction is open and its advice held: every event committed so far is in
	// the log, read on a connection of its own.
	committed := eventHead(t, database)
	awaiting, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	safe, err := served.horizon().Await(awaiting)
	if err != nil {
		t.Fatalf("await the event horizon while the write's advice is held: %v", err)
	}
	if safe < committed {
		t.Fatalf("the event horizon is %d while the write's advice is held, want it at or above the head %d committed before it", safe, committed)
	}
	close(release)
	if response := awaitResponse(t, answered); response.Code != http.StatusCreated {
		t.Fatalf("the comment whose advice was held: status=%d body=%s", response.Code, response.Body.String())
	}
}
