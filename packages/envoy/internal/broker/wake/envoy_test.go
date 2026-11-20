package wake

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/urlsetting"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

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
	e := Envoy{URL: urlsetting.New(srv.URL), Account: account, HTTP: srv.Client()}
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

	e := Envoy{URL: urlsetting.New(srv.URL), Account: staticAccount("tok"), HTTP: srv.Client()}
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

	e := Envoy{URL: urlsetting.New(srv.URL), Account: staticAccount("tok"), HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the 404 (no live session) response, got none")
	}
}

func TestNotifySwallowsUnreachableServerAndLogsIt(t *testing.T) {
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})

	// A port the test holds and nothing listens on: the connection fails.
	e := Envoy{URL: urlsetting.New("http://" + testport.Refused(t)), Account: staticAccount("tok"), HTTP: &http.Client{}}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the connection failure, got none")
	}
}

// A transport error can repeat its request URL. Notify sends the guarded setting's complete text,
// credentials included, and logs the redacted request error, never the raw user part, even where
// the transport itself puts that text in its error. The user and the password are made at each
// run, so no credential is written in this file.
func TestNotifyRedactsCredentialedEnvoyURLInTransportError(t *testing.T) {
	user, password := "wake-user-"+rand.Text(), "wake-"+rand.Text()
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})
	var sent *url.URL
	e := Envoy{
		URL:     urlsetting.New("https://" + user + ":" + password + "@listener.example"),
		Account: staticAccount("tok"),
		HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			sent = request.URL
			return nil, fmt.Errorf("transport rejected %s", request.URL)
		})},
	}

	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	// The round trip ran with the configured text, not the cleaned form: a request built from
	// String would reach no listener, and its error would hold no marker for the check below.
	if sent == nil {
		t.Fatal("Notify sent no request")
	}
	if gotPassword, _ := sent.User.Password(); sent.User.Username() != user || gotPassword != password {
		t.Errorf("request user part = %q, want the configured user and password", sent.User)
	}
	if sent.Scheme != "https" || sent.Host != "listener.example" || sent.Path != "/v1/messages/send" {
		t.Errorf("request URL = %s://%s%s, want https://listener.example/v1/messages/send", sent.Scheme, sent.Host, sent.Path)
	}

	if len(records) != 1 {
		t.Fatalf("logged %d records, want one transport warning", len(records))
	}
	var logged error
	records[0].Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			logged, _ = attr.Value.Any().(error)
		}
		return true
	})
	if logged == nil {
		t.Fatal("transport warning had no error")
	}
	for _, marker := range []string{user, password} {
		if strings.Contains(logged.Error(), marker) {
			t.Errorf("transport warning leaked %q in %q", marker, logged)
		}
	}
}

// A credentialed URL net/url cannot parse fails before any request is sent, and its parse error
// quotes the whole request URL. Notify logs the redacted request error, never that user part.
// The user and the password are made at each run, so no credential is written in this file.
func TestNotifyRedactsCredentialedEnvoyURLInBuildError(t *testing.T) {
	user, password := "build-user-"+rand.Text(), "build-pass/"+rand.Text()
	var records []slog.Record
	logtest.SetDefault(t, capturingHandler{records: &records})
	e := Envoy{
		// The / in the password ends the authority, so net/url reads the password's first part as
		// a port and refuses it.
		URL:     urlsetting.New("https://" + user + ":" + password + "@listener.example"),
		Account: staticAccount("tok"),
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("a request URL that does not parse was sent")
			return nil, errors.New("unexpected send")
		})},
	}

	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) != 1 {
		t.Fatalf("logged %d records, want one build warning", len(records))
	}
	var logged error
	records[0].Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			logged, _ = attr.Value.Any().(error)
		}
		return true
	})
	if logged == nil {
		t.Fatal("build warning had no error")
	}
	var parseErr *url.Error
	if !errors.As(logged, &parseErr) || parseErr.Op != "parse" {
		t.Errorf("build warning error = %v, want one that still unwraps to the parse *url.Error", logged)
	}
	for _, marker := range []string{user, "9q-secret"} {
		if strings.Contains(logged.Error(), marker) {
			t.Errorf("build warning leaked %q in %q", marker, logged)
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

	e := Envoy{URL: urlsetting.New(srv.URL), Account: &accountTokens{err: errors.New("the token file is missing")}, HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")
	if sent {
		t.Fatal("a notice went out with no account token")
	}
	if len(records) == 0 {
		t.Fatal("expected a warning naming the unread account token, got none")
	}
}
