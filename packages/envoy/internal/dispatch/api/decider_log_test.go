package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A decision, mint or scope change at a browser under IAP identity whose caller's standing cannot be
// read (browserDeciderHeld) logs one WARN line naming the caller by their login's hash and the
// failure by its kind, and answers 503 IDENTITY_UNAVAILABLE as it did. The failure quotes the
// address, as the people store's errors do, and the line holds neither the address, in any letter
// case, nor any word of that error.
//
// Control: log "error", err again on browserDeciderHeld's line, and the row fails on the address.
func TestADecidersUnreadableStandingLogsItsKindAndNoAddress(t *testing.T) {
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	_, database, deps := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessDave)
	directory := &standingFake{directory: true}
	directory.answers(identity.Standing{}, fmt.Errorf("read membership of %q: %w", accessDave,
		&pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"}))
	deps.Standing = directory
	handler := servedWith(t, deps)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]any{"name": "dave's token", "scopes": []string{model.ScopePerson}}, accessDave)
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusServiceUnavailable || refusal.Code != "IDENTITY_UNAVAILABLE" ||
		refusal.Error != "whether this caller may still decide could not be read; try again in a minute" {
		t.Fatalf("dave's mint while his standing cannot be read = %d %s, want 503 IDENTITY_UNAVAILABLE", response.Code, response.Body.String())
	}
	const message = "dispatch: a decision, a mint or a scope change at a browser was refused: whether Dispatch holds its caller could not be read"
	lines := logged.Lines(message)
	if strings.Count(lines, "\n") != 1 {
		t.Fatalf("logged %q; want one line holding %q", logged.String(), message)
	}
	for _, want := range []string{"level=WARN", "login_hash=" + model.LoginHash(accessDave), `error_kind="database SQLSTATE 57P01"`} {
		if !strings.Contains(lines, want) {
			t.Errorf("the line %q does not hold %s", lines, want)
		}
	}
	for _, word := range []string{accessDave, "read membership", "the database is down"} {
		if strings.Contains(strings.ToLower(lines), strings.ToLower(word)) {
			t.Errorf("the line %q holds %q", lines, word)
		}
	}
}
