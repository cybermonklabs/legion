package routes

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// rawWhoami answers GET /auth/whoami's raw body for login.
func rawWhoami(t *testing.T, handler http.Handler, login string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, strings.TrimSuffix(response.Body.String(), "\n")
}

// A full member's /auth/whoami is byte for byte what it was before member kinds existed: the
// three keys event_head, kind and login, and no member_kind (R9).
func TestWhoamiOfAFullMemberIsByteForByteToday(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `insert into people (email) values ('alice@example.com')`); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if kind, err := access.MemberKind(context.Background(), database.Pool, "alice@example.com"); err != nil || kind != model.MemberFull {
		t.Fatalf("MemberKind(alice) = %q, %v, want full: without a full member's row the test reads nothing", kind, err)
	}
	status, body := rawWhoami(t, storeRouter(t, database), "alice@example.com")
	if want := `{"event_head":"0","kind":"user","login":"alice@example.com"}`; status != http.StatusOK || body != want {
		t.Fatalf("whoami = %d %s, want 200 %s", status, body, want)
	}
}

// A restricted member's /auth/whoami says so, after login, so the dashboard shows them a
// restricted member's rail.
func TestWhoamiNamesARestrictedMember(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `insert into people (email, member_kind) values ('outsider@other.example', 'restricted')`); err != nil {
		t.Fatalf("seed the restricted member: %v", err)
	}
	status, body := rawWhoami(t, storeRouter(t, database), "outsider@other.example")
	if want := `{"event_head":"0","kind":"user","login":"outsider@other.example","member_kind":"restricted"}`; status != http.StatusOK || body != want {
		t.Fatalf("whoami = %d %s, want 200 %s", status, body, want)
	}
}

// A member kind whoami cannot read is a 503 IDENTITY_UNAVAILABLE, never a whoami without it,
// which would show a restricted member a full member's rail.
func TestWhoamiRefusesWhenTheKindCannotBeRead(t *testing.T) {
	database := storetest.Open(t)
	database.Pool.Close()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
		SigningKey: testSigningKey, People: people, Store: database, Lifetime: t.Context(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
	}))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ := New(ctx)
	status, body := rawWhoami(t, handler, "alice@example.com")
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || status != http.StatusServiceUnavailable || refusal.Code != "IDENTITY_UNAVAILABLE" {
		t.Fatalf("whoami = %d %s, want 503 IDENTITY_UNAVAILABLE", status, body)
	}
}

// The warning whoami logs when it cannot read the member kind names no part of the signed-in
// person's email: the line is the operator's, and the email is the person's (CodeQL
// go/clear-text-logging, alert #143). The request's own access-log line is another line, which
// this test does not read.
func TestWhoamisMemberKindWarningNamesNoPerson(t *testing.T) {
	const local, domain = "zq.hidden.person", "zq-secret.example"
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	database := storetest.Open(t)
	database.Pool.Close()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
		SigningKey: testSigningKey, People: people, Store: database, Lifetime: t.Context(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
	}))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ := New(ctx)
	if status, body := rawWhoami(t, handler, local+"@"+domain); status != http.StatusServiceUnavailable {
		t.Fatalf("whoami = %d %s, want 503 IDENTITY_UNAVAILABLE", status, body)
	}
	warning := logged.Lines("dispatch: whoami could not read the person's member kind")
	if warning == "" {
		t.Fatalf("whoami logged no member-kind warning, so this test reads nothing; the log:\n%s", logged.String())
	}
	for _, part := range []string{local + "@" + domain, local, domain} {
		if strings.Contains(strings.ToLower(warning), part) {
			t.Errorf("the whoami warning names %q of the signed-in person:\n%s", part, warning)
		}
	}
}
