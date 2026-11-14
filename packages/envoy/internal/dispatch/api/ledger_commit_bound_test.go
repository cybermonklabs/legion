package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A document write's ledger runs its last statements - the settlement credit and the
// announcement - inside the transaction, on the commit's own bounded context
// (docs.Ledger.commit, store.CommitContext), before the COMMIT. When the database does not
// answer one of them within that bound the transaction is rolled back and nothing of the write
// committed, so the write is refused 503 DATABASE_BUSY with Retry-After, and never 500 INTERNAL
// with an 'API handler failed' line. Ten-times load with the database proxy frozen answered
// POST /api/v1/artifacts/{id}/edits 500 INTERNAL this way ('announce the documents the
// transaction appended to: timeout: context deadline exceeded').
func TestAWriteWhoseLedgerStatementRunsOutTheCommitBoundIsRefusedDatabaseBusy(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Ledger past the commit bound", "Before.")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create function slow_settlement_credit_for_test() returns trigger language plpgsql as $$
		begin
			perform pg_sleep(2);
			return new;
		end $$;
		create trigger slow_settlement_credit_for_test before insert or update on doc_settlements_pending
			for each row execute function slow_settlement_credit_for_test();
	`); err != nil {
		t.Fatalf("create the settlement-credit-slowing trigger: %v", err)
	}
	logs := captureLogs(t)

	data, err := json.Marshal(map[string]any{"ops": []map[string]string{
		{"op": "insert", "markdown": "Written while the settlement credit is slow.", "after": "end"},
	}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", bytes.NewReader(data)).
		WithContext(store.WithCommitBoundForTest(context.Background(), 300*time.Millisecond))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(response, request) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the edit was still unanswered 20 s into a 300 ms commit bound")
	}

	if failed := strings.Contains(logs.String(), "API handler failed"); failed || response.Code == http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s handler-failed=%v, want 503 DATABASE_BUSY", response.Code, response.Body.String(), failed)
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
}
