package api

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedLongIssueHistory is an issue of seededEvents session writes, the history a write's advice
// counts, on a server a row can hook (server.adviceQueryHook).
func seedLongIssueHistory(t *testing.T, seededEvents int) (http.Handler, *server, string) {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createAdviceProject(t, handler, "PERF")
	spec := "# Performance\n"
	issue := createAdviceIssue(t, handler, "PERF", "Long event history", &spec)

	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin event seed: %v", err)
	}
	defer tx.Rollback(context.Background())
	// The seed appends to the issue's event log as a writer does: under its owner lock, its
	// sequence read in the statement after it.
	if err := store.LockOwner(context.Background(), tx, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("lock issue for event seed: %v", err)
	}
	var lastSeq int
	if err := tx.QueryRow(context.Background(), `
		select last_seq from issues where key = $1
	`, issue.Key).Scan(&lastSeq); err != nil {
		t.Fatalf("read issue sequence for event seed: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, $2 + ordinal, 'message.created',
		       '{"kind":"session","id":"session-performance"}'::jsonb, '{}'::jsonb, false
		from generate_series(1, $3) ordinal
	`, issue.Key, lastSeq, seededEvents); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		update issues set last_seq = $2 where key = $1
	`, issue.Key, lastSeq+seededEvents); err != nil {
		t.Fatalf("advance issue sequence: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit event seed: %v", err)
	}
	return handler, served, issue.Key
}

// A session's write on an issue with a long event history still answers its advice, with the full
// count of the session's writes. The advice's queries run under the 500 ms statement_timeout
// writeAdvice sets on its savepoint, and a count that runs past that bound is cancelled and costs
// the write its advice, so a count that is answered ran inside the bound. The hook reads that the
// bound stood on the savepoint the count runs on. The write's wall-clock time depends on the
// machine and is the timing lane's (TestWriteAdviceOnALongIssueHistoryAnswersUnderTwoSeconds).
func TestWriteAdviceStaysBoundedOnLongIssueHistory(t *testing.T) {
	t.Parallel()
	const seededEvents = 5000
	handler, served, issueKey := seedLongIssueHistory(t, seededEvents)
	bound := make(chan string, 1)
	served.adviceQueryHook = func(ctx context.Context, tx pgx.Tx, query string) error {
		if query != "session_writes_since_human" {
			return nil
		}
		var setting string
		if err := tx.QueryRow(ctx, `select current_setting('statement_timeout')`).Scan(&setting); err != nil {
			return err
		}
		select {
		case bound <- setting:
		default:
		}
		return nil
	}
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", map[string]any{
		"body": "bounded advice", "actor": adviceSessionActor("session-performance"),
	})
	advice := adviceFromResponse(t, response.Code, response.Body.String())
	if advice.SessionWritesSinceHuman != seededEvents+1 {
		t.Fatalf("session write count = %d, want %d", advice.SessionWritesSinceHuman, seededEvents+1)
	}
	select {
	case setting := <-bound:
		if setting != "500ms" {
			t.Fatalf("the advice counted the session's writes under statement_timeout %q, want the 500ms writeAdvice sets", setting)
		}
	default:
		t.Fatal("the advice answered without running its count of the session's writes")
	}
}

// TestWriteAdviceOnALongIssueHistoryAnswersUnderTwoSeconds is the row above's service level,
// which only the timing lane reads (benchtime.TimingEnv): a shared runner's clock is its load as
// much as the advice's work.
func TestWriteAdviceOnALongIssueHistoryAnswersUnderTwoSeconds(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	handler, _, issueKey := seedLongIssueHistory(t, 5000)
	started := time.Now()
	sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", map[string]any{
		"body": "bounded advice", "actor": adviceSessionActor("session-performance"),
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("advice round trip took %s, want under 2s", elapsed)
	}
}
