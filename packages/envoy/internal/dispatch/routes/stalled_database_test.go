package routes

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stalledStatement is the error one of the store's pools answers a statement it ran outside any
// transaction that reached the database and was never answered with: the shape INFRA-592's load
// step met, which may have committed (store.ErrStatementOutcomeUnknown, poolBusy).
func stalledStatement(what string) error {
	return fmt.Errorf("%s: %s pool: %w: %w", what, store.RequestsPool, store.ErrStatementOutcomeUnknown,
		errors.New("timeout: read tcp 127.0.0.1:50000->127.0.0.1:5432: i/o timeout"))
}

// These routes are mounted on GET and write anyway - the sign-in callback and the dev sign-in
// record the person, their sign-in and their session generation - so a write of theirs the
// database stopped answering may have committed. It is answered 500 COMMIT_OUTCOME_UNKNOWN with
// no Retry-After, as every other write whose outcome nobody knows is, rather than the retryable
// 503 that would record a sign-in or advance a session generation twice. Before this it was a
// 500 naming only what the server had been doing, which a client cannot act on.
func TestASignInWriteTheDatabaseStoppedAnsweringIsNotOfferedARetry(t *testing.T) {
	sessions := &memorySessionStore{generations: map[string]int64{}}
	handler, people := newDevSignInRouterWithPeople(t, devSignInOrigin, sessions)
	people.recordFails = stalledStatement(`record person "sami@example.com"`)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, devSignInRequest(t, "/auth/_dev/signin?login=sami@example.com"))

	if response.Code != http.StatusInternalServerError || errorCodeOf(t, response) != admission.CodeCommitOutcomeUnknown {
		t.Fatalf("status=%d body=%s, want 500 %s", response.Code, response.Body.String(), admission.CodeCommitOutcomeUnknown)
	}
	if retry := response.Header().Get("Retry-After"); retry != "" {
		t.Fatalf("Retry-After = %q, want none: the insert may have committed", retry)
	}
	if cookie := sessionCookieOf(response); cookie != nil {
		t.Fatalf("the refused sign-in issued a cookie: %v", cookie)
	}
}

// A read of theirs is the other half of the same rule: the sign-in callback's check that the
// address still names the account it signed in (SignInRule.ConfirmCookie) commits nothing
// whatever the database did with it, so it is refused 503 DATABASE_BUSY with Retry-After and the
// browser may start the sign-in again. Before this it was a 500 too.
func TestASignInReadTheDatabaseStoppedAnsweringIsRefusedDatabaseBusy(t *testing.T) {
	response := httptest.NewRecorder()
	request := devSignInRequest(t, "/auth/callback?code=c&state=s")

	writeUnservedOr500(response, request, stalledStatement("read the account the sign-in recorded"),
		false, slog.LevelError, "dispatch: read the account the sign-in recorded failed", "record sign-in")

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) {
		t.Fatalf("status=%d body=%s, want 503 %s", response.Code, response.Body.String(), admission.CodeDatabaseBusy)
	}
	if retry := response.Header().Get("Retry-After"); retry != admission.RetryAfter {
		t.Fatalf("Retry-After = %q, want %s: the read committed nothing", retry, admission.RetryAfter)
	}
}
