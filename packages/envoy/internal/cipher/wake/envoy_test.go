package wake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/listenerwire"
)

// accountTokens is the broker's account token as Notify reads it: each read answers the next of
// tokens, as a projected token the kubelet rotated does, or err.
type accountTokens struct {
	tokens []string
	err    error
	reads  int
}

func (a *accountTokens) Read() (string, error) {
	if a.err != nil {
		return "", a.err
	}
	token := a.tokens[min(a.reads, len(a.tokens)-1)]
	a.reads++
	return token, nil
}

// staticAccount is an account whose token never changes.
func staticAccount(token string) *accountTokens { return &accountTokens{tokens: []string{token}} }

// capturingHandler records every slog record it is asked to handle, so a test can assert that
// Notify logged a warning without caring about the exact message text.
type capturingHandler struct {
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// Notify presents the broker's own account token, read afresh for each notice, as its bearer:
// the listener admits the account it names, so the broker shares no bearer with anyone.
func TestNotifyPresentsTheAccountToken(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	// The body is read as the listener's send route reads it.
	var gotBody listenerwire.SendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	account := &accountTokens{tokens: []string{"account-token-1", "account-token-2"}}
	e := NewEnvoy(srv.URL, account, srv.Client())
	e.Notify(context.Background(), "sess-123", "req-456", "approved")
	if gotAuth != "Bearer account-token-1" {
		t.Fatalf("authorization: got %q, want the account's token", gotAuth)
	}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")
	if gotAuth != "Bearer account-token-2" {
		t.Fatalf("authorization: got %q, want the account's rotated token", gotAuth)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/v1/messages/send" {
		t.Fatalf("path: got %q, want /v1/messages/send", gotPath)
	}
	if gotBody.TargetSession != "sess-123" {
		t.Fatalf("target_session: got %q, want %q", gotBody.TargetSession, "sess-123")
	}
	if gotBody.Source != "envoy" {
		t.Fatalf("source: got %q, want %q (contracts.Envelope.Validate's enum has no agent-secrets entry)", gotBody.Source, "envoy")
	}
	if gotBody.Message != "secret request approved" {
		t.Fatalf("message: got %q, want %q", gotBody.Message, "secret request approved")
	}

	if gotBody.Payload == nil {
		t.Fatal("the send carried no payload")
	}
	var payload secretRequestPayload
	if err := json.Unmarshal([]byte(*gotBody.Payload), &payload); err != nil {
		t.Fatalf("payload is not JSON-encoded text: %v", err)
	}
	if payload.Type != "secret-request" {
		t.Fatalf("payload.type: got %q, want %q", payload.Type, "secret-request")
	}
	if payload.RequestID != "req-456" {
		t.Fatalf("payload.request_id: got %q, want %q", payload.RequestID, "req-456")
	}
	if payload.State != "approved" {
		t.Fatalf("payload.state: got %q, want %q", payload.State, "approved")
	}
}

func TestNotifySwallowsServerErrorAndLogsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})

	e := NewEnvoy(srv.URL, staticAccount("tok"), srv.Client())
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the 500 response, got none")
	}
	if records[0].Level != slog.LevelWarn {
		t.Fatalf("level: got %v, want Warn", records[0].Level)
	}
}

func TestNotifySwallowsNotFoundAndLogsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})

	e := NewEnvoy(srv.URL, staticAccount("tok"), srv.Client())
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the 404 (no live session) response, got none")
	}
}

func TestNotifySwallowsUnreachableServerAndLogsIt(t *testing.T) {
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})

	// A port the test holds and nothing listens on: the connection fails.
	e := NewEnvoy("http://"+testport.Refused(t), staticAccount("tok"), &http.Client{})
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the connection failure, got none")
	}
}

// A BROKER_ENVOY_URL with a made-up user name and password that cannot be reached logs the failure
// by its class, holding no part of either: Go's *url.Error quotes the URL with the user name.
func TestNotifyLogsNoCredentialOfTheEnvoyURL(t *testing.T) {
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})
	const password, user = "madeUpWakePassword0123", "madeUpWakeUser0123"
	configured := "http://" + user + ":" + password + "@" + testport.Refused(t)
	e := NewEnvoy(configured, staticAccount("tok"), &http.Client{})
	e.Notify(context.Background(), "sess-123", "req-456", "approved")
	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the connection failure, got none")
	}
	var logged strings.Builder
	for _, record := range records {
		logged.WriteString(record.Message)
		record.Attrs(func(a slog.Attr) bool { logged.WriteString(" " + a.Key + "=" + a.Value.String()); return true })
	}
	for _, secret := range []string{user, password} {
		if strings.Contains(logged.String(), secret[:10]) {
			t.Fatalf("the warning %q holds %q", logged.String(), secret[:10])
		}
	}
}

// An account token the broker cannot read sends nothing, since a call with no bearer would only
// be refused, and logs why without a token.
func TestNotifySendsNothingWithoutTheAccountToken(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})

	e := NewEnvoy(srv.URL, &accountTokens{err: errors.New("the token file is missing")}, srv.Client())
	e.Notify(context.Background(), "sess-123", "req-456", "approved")
	if sent {
		t.Fatal("a notice went out with no account token")
	}
	if len(records) == 0 {
		t.Fatal("expected a warning naming the unread account token, got none")
	}
}
