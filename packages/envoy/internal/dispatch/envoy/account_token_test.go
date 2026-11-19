package envoy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// accountToken is a compact token whose payload carries exp, signed by nobody: the client reads
// the expiry a projected token states and nothing else of it, and the listener is what verifies it.
func accountToken(t *testing.T, subject string, exp time.Time) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode a token part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return encode(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." +
		encode(map[string]any{"sub": subject, "exp": exp.Unix()}) + ".c2lnbmF0dXJl"
}

// writeAccountToken writes token into path as the kubelet writes a projected token: the whole
// file replaced at once, a trailing newline after it.
func writeAccountToken(t *testing.T, path, token string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the account token: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("replace the account token: %v", err)
	}
}

// authorizations is a listener stand-in that records the Authorization of every send it is made
// and answers each as a send the stream stored.
type authorizations struct {
	mu   sync.Mutex
	seen []string
}

func (a *authorizations) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Header.Get("Authorization"))
	a.mu.Unlock()
	_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
}

func (a *authorizations) presented() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// Dispatch's own identity is the projected service-account token the kubelet writes into the
// file the client names, and rewrites before that token expires. Every call reads the file when
// it is made, so the call after a rotation presents the new token, with no restart and no 401 in
// between. A file whose token has expired by its own exp, and one that cannot be read or holds
// nothing, make no call at all: the listener would refuse such a token, and the error names the
// file rather than reading as the listener's answer.
func TestEachCallPresentsTheAccountTokenTheFileHoldsWhenTheCallIsMade(t *testing.T) {
	stand := &authorizations{}
	listener := httptest.NewServer(http.HandlerFunc(stand.serve))
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "listener-account-token")
	client := New(listener.URL, WithAccountTokenFile(path))
	send := func() error {
		_, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"})
		return err
	}

	first := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour))
	writeAccountToken(t, path, first)
	if err := send(); err != nil {
		t.Fatalf("send on the first token: %v", err)
	}
	rotated := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(2*time.Hour))
	writeAccountToken(t, path, rotated)
	if err := send(); err != nil {
		t.Fatalf("send after the rotation: %v", err)
	}
	if got, want := stand.presented(), []string{"Bearer " + first, "Bearer " + rotated}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the listener was presented %q, want the first token and then the rotated one", got)
	}

	expired := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(-time.Minute))
	writeAccountToken(t, path, expired)
	err := send()
	if !errors.Is(err, ErrAccountToken) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrReceiptTimeout) || !strings.Contains(err.Error(), path) {
		t.Fatalf("a send on an expired token = %v, want ErrAccountToken naming %s, made before any request", err, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the token file: %v", err)
	}
	if err := send(); !errors.Is(err, ErrAccountToken) || !strings.Contains(err.Error(), path) {
		t.Fatalf("a send with no token file = %v, want ErrAccountToken naming %s", err, path)
	}
	writeAccountToken(t, path, "  ")
	if _, err := client.Session(context.Background(), "s1"); !errors.Is(err, ErrAccountToken) {
		t.Fatalf("a read with an empty token file = %v, want ErrAccountToken", err)
	}
	if got := len(stand.presented()); got != 2 {
		t.Fatalf("the listener was called %d times, want the two calls made on a live token and none after", got)
	}
}

// The listener takes a send's role mark from Dispatch's own identity alone and refuses it from
// every other credential (cmd/listener, sendHandler). So a client that presents that identity
// marks a send for a role with the role, and a client that presents the shared token, or none,
// marks no send: the mark could never be accepted from it, and every role route's send would be
// refused.
func TestASendForARoleCarriesTheRoleOnlyFromDispatchsOwnIdentity(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode send: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
	}))
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "listener-account-token")
	writeAccountToken(t, path, accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour)))

	for _, client := range []*Client{New(listener.URL, WithAccountTokenFile(path)), New(listener.URL, WithToken("shared")), New(listener.URL)} {
		for _, role := range []string{"reviewer", ""} {
			if _, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: role}); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, body := range bodies {
		role, marked := body["role"]
		if wantMarked := i == 0; marked != wantMarked || (marked && role != "reviewer") {
			t.Fatalf("send %d carried role %v (present %t), want role reviewer on the identity client's role send alone: %v", i, role, marked, bodies)
		}
	}
}

// A send the listener kept on the role's kept lane, because the role's claim named another
// session than the one Dispatch resolved, is answered 202 with kept and kept_until beside the
// send's answer. The client reads it as a send the listener took, naming the copy it kept.
func TestASendTheListenerKeptForTheRoleIsReadAsKept(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"event_id":"kept-envelope","recipient":"s1","kept":true,"kept_until":1800000000000}`))
	}))
	defer listener.Close()
	result, err := New(listener.URL).Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("a kept send: %v", err)
	}
	if !result.Kept || result.KeptUntil != 1800000000000 || result.EnvelopeID != "kept-envelope" || result.Recipient != "s1" {
		t.Fatalf("a kept send = %+v, want kept until 1800000000000 naming the kept copy", result)
	}
}

// A listener of the release before Dispatch's identity existed verifies no account token: while a
// deploy rolls the listener and the app at once, a new app pod can reach an old listener pod, which
// refuses the account token 401 before its route runs. A client that holds the shared token too
// makes that call once more on it, with no role mark, which the old listener takes, so no window
// of the roll answers every send 401, and writes one WARN line saying so. The control is a client
// holding no shared token: it makes no second call, writes no such line, and reports the 401 as
// before.
func TestACallTheListenerRefusesOnTheAccountTokenIsMadeAgainOnTheSharedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener-account-token")
	account := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour))
	writeAccountToken(t, path, account)
	type call struct {
		authorization string
		role          any
	}
	var mu sync.Mutex
	var calls []call
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, call{authorization: r.Header.Get("Authorization"), role: body["role"]})
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer shared" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
	}))
	defer listener.Close()
	input := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}
	const retryLine = "dispatch: the Envoy listener refused Dispatch's account token, so the call is made again on the shared token and carries no role mark"
	warnings := func(logs *bytes.Buffer) int {
		count := 0
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "WARN" && record["msg"] == retryLine {
				count++
			}
		}
		return count
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	if _, err := New(listener.URL, WithToken("shared"), WithAccountTokenFile(path), WithLogger(logger)).Send(context.Background(), input); err != nil {
		t.Fatalf("a send an old listener refuses on the account token, with the shared token held: %v", err)
	}
	mu.Lock()
	got := append([]call(nil), calls...)
	calls = nil
	mu.Unlock()
	if len(got) != 2 || got[0].authorization != "Bearer "+account || got[0].role != "reviewer" || got[1].authorization != "Bearer shared" || got[1].role != nil {
		t.Fatalf("the listener was called %+v, want the account token with the role mark, then the shared token with none", got)
	}
	if n := warnings(&logs); n != 1 {
		t.Fatalf("the client wrote %d WARN lines %q for one call made again on the shared token, want one:\n%s", n, retryLine, logs.String())
	}

	var controlLogs bytes.Buffer
	controlLogger := slog.New(slog.NewJSONHandler(&controlLogs, nil))
	if _, err := New(listener.URL, WithAccountTokenFile(path), WithLogger(controlLogger)).Send(context.Background(), input); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("control: a send refused on the account token with no shared token held = %v, want the listener's 401", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("control: the client with no shared token called %d times, want one", len(calls))
	}
	if n := warnings(&controlLogs); n != 0 {
		t.Fatalf("control: the client with no shared token wrote %d WARN lines %q, want none", n, retryLine)
	}
}

// writeFailingTransport answers the first round trip 401, as a listener refusing the account token
// does, and fails every later one before it writes anything, with a timeout: a retry on the shared
// token that never reached the listener.
type writeFailingTransport struct {
	calls atomic.Int32
}

func (f *writeFailingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.calls.Add(1) == 1 {
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)), Header: http.Header{}, Request: r}, nil
	}
	return nil, context.DeadlineExceeded
}

// A send whose account-token call the listener refused 401 and whose retry on the shared token
// timed out before it was written never reached a route: the 401 came from the listener's
// authentication. It is the plain unreachable failure, not a receipt timeout, which would say the
// message may already be published. The control is a send whose one call was written and then
// timed out: that one is the receipt timeout.
func TestARetryOnTheSharedTokenThatWasNeverWrittenIsNoReceiptTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener-account-token")
	writeAccountToken(t, path, accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour)))
	input := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}

	client := New("http://listener.invalid", WithToken("shared"), WithAccountTokenFile(path), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	transport := &writeFailingTransport{}
	client.httpClient.Transport = transport
	_, err := client.Send(context.Background(), input)
	if transport.calls.Load() != 2 {
		t.Fatalf("the client made %d round trips, want the refused one and its retry", transport.calls.Load())
	}
	if err == nil || errors.Is(err, ErrReceiptTimeout) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a retry that was never written = %v, want ErrUnreachable and not ErrReceiptTimeout", err)
	}

	if control := sendTransportError(context.DeadlineExceeded, true); !errors.Is(control, ErrReceiptTimeout) {
		t.Fatalf("control: a written send that timed out = %v, want ErrReceiptTimeout", control)
	}
}
