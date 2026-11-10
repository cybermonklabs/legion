package identity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// cookieRequestOn is cookieRequest with a method of its own: what the server answers a stalled
// identity lookup turns on, as it does everywhere else (admission.Reads).
func cookieRequestOn(t *testing.T, method, email string, generation int64) *http.Request {
	t.Helper()
	cookie := strings.SplitN(auth.IssueSessionCookie(email, generation, testSigningKey, true), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	request := httptest.NewRequest(method, "/api/v1/issues", nil)
	request.AddCookie(&http.Cookie{Name: name, Value: value})
	return request
}

// Resolving the caller reads the session's generation on the request pool, outside any
// transaction, so a database that stops answering that statement leaves the store naming an
// outcome nobody knows (store.ErrStatementOutcomeUnknown). That is no unknown outcome for the
// request: the statement is a read and commits nothing. A read is answered 503 DATABASE_BUSY
// with Retry-After, as every other read a stalled database meets is, and a write - whose
// renewal of a lapsed membership does write the person's row outside a transaction - is
// answered 500 COMMIT_OUTCOME_UNKNOWN with none. Before this both were 500 IDENTITY_ERROR,
// which names no cause and offers nothing to do.
func TestAStalledIdentityLookupIsAnsweredAsEveryOtherStallIs(t *testing.T) {
	const email = "sami@example.com"
	for _, test := range []struct {
		name   string
		method string
		status int
		code   string
	}{
		{"a read is told to send it again", http.MethodGet, http.StatusServiceUnavailable, admission.CodeDatabaseBusy},
		{"a write is told the outcome is unknown", http.MethodPost, http.StatusInternalServerError, admission.CodeCommitOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			socket := storetest.NewStalledSocket(store.RequestsPool)
			stalled := storetest.Reopened(t, database, func(config *pgxpool.Config) {
				config.ConnConfig.DialFunc = socket.Dial
			})
			generation, err := store.NewPgSessionStore(database.Pool).EnsureSession(t.Context(), email)
			if err != nil {
				t.Fatalf("record the person's session generation: %v", err)
			}
			request := cookieRequestOn(t, test.method, email, generation)

			socket.StallAfter(email)
			_, err = CookieIdentity{SigningKey: testSigningKey, Sessions: store.NewPgSessionStore(stalled.Pool)}.Login(request)
			if err == nil {
				t.Fatal("the session generation was read, so the socket stalled nothing")
			}
			if !socket.Fired() {
				t.Fatalf("the socket never timed out; the lookup failed with: %v", err)
			}
			response := httptest.NewRecorder()
			WriteError(response, request, err)

			if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), test.status, test.code)
			}
			retry := response.Header().Get("Retry-After")
			if test.code == admission.CodeDatabaseBusy && retry != admission.RetryAfter {
				t.Fatalf("Retry-After = %q, want %s: the read committed nothing", retry, admission.RetryAfter)
			}
			if test.code == admission.CodeCommitOutcomeUnknown && retry != "" {
				t.Fatalf("Retry-After = %q, want none: the statement may have committed", retry)
			}
		})
	}
}
