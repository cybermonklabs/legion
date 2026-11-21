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

// identityHolding makes a client present token as Dispatch's identity: the account token file
// WithAccountTokenFile names, holding token. A token that is not a compact JWT is presented as it
// is, so a test names the bearer the listener stand-in expects.
func identityHolding(t *testing.T, token string) Option {
	t.Helper()
	path := filepath.Join(t.TempDir(), "listener-account-token")
	writeAccountToken(t, path, token)
	return WithAccountTokenFile(path)
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
// marks a send for a role with the role, and a client that presents none refuses a send for a role
// (ErrNoListenerIdentity) rather than make it unmarked, since the session that takes the role from
// the target would not receive an unmarked one: the refusal makes no listener call, names the
// setting that gives Dispatch an identity and the README section, and writes one Error line naming
// the role. The control is each client's send with no role, which every client makes, unmarked,
// and which the client with no identity makes with no credential at all.
func TestASendForARoleIsMadeOnlyOnDispatchsOwnIdentity(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var presented []string
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode send: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		presented = append(presented, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
	}))
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "listener-account-token")
	writeAccountToken(t, path, accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour)))
	var logs bytes.Buffer
	logger := WithLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	for i, client := range []*Client{New(listener.URL, WithAccountTokenFile(path), logger), New(listener.URL, logger)} {
		_, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"})
		if identity := i == 0; identity && err != nil {
			t.Fatalf("the identity client's send for a role: %v", err)
		} else if !identity && (!errors.Is(err, ErrNoListenerIdentity) || errors.Is(err, ErrUnavailable) ||
			!strings.Contains(err.Error(), "ENVOY_ACCOUNT_TOKEN_FILE") || !strings.Contains(err.Error(), "Dispatch's identity at the Envoy listener")) {
			t.Fatalf("client %d's send for a role with no identity = %v, want ErrNoListenerIdentity naming ENVOY_ACCOUNT_TOKEN_FILE and the README section, and not ErrUnavailable", i, err)
		}
		if _, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"}); err != nil {
			t.Fatalf("control: client %d's send with no role: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("the listener was sent %d sends, want 3: the identity client's two and the other client's send with no role: %v", len(bodies), bodies)
	}
	for i, body := range bodies {
		role, marked := body["role"]
		if wantMarked := i == 0; marked != wantMarked || (marked && role != "reviewer") {
			t.Fatalf("send %d carried role %v (present %t), want role reviewer on the identity client's role send alone: %v", i, role, marked, bodies)
		}
	}
	if presented[2] != "" {
		t.Fatalf("the client with no identity sent Authorization %q, want none", presented[2])
	}
	if n := logLines(&logs, "ERROR", refusedRoleSendLine, map[string]any{"role": "reviewer"}); n != 1 {
		t.Fatalf("the client with no identity wrote %d Error lines %q for its refused send for a role, want one:\n%s", n, refusedRoleSendLine, logs.String())
	}
}

// refusedRoleSendLine is the Error line a client with no identity writes for each send for a role
// it refuses (Send).
const refusedRoleSendLine = "dispatch: a send for a role is refused: this Dispatch presents no identity to the Envoy listener"

// logLines counts the JSON log lines in logs at level whose msg is msg and whose fields include
// each of fields.
func logLines(logs *bytes.Buffer, level, msg string, fields map[string]any) int {
	count := 0
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["level"] != level || record["msg"] != msg {
			continue
		}
		matched := true
		for key, want := range fields {
			matched = matched && record[key] == want
		}
		if matched {
			count++
		}
	}
	return count
}

// A send the listener kept on the role's kept lane, because the role's claim named another
// session than the one Dispatch resolved, is answered 202 with kept and kept_until beside the
// send's answer. The client reads it as a send the listener took, naming the copy it kept.
func TestASendTheListenerKeptForTheRoleNamesTheKeptCopy(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"event_id":"kept-envelope","recipient":"s1","kept":true,"kept_until":1800000000000}`))
	}))
	defer listener.Close()
	result, err := New(listener.URL).Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("a kept send: %v", err)
	}
	if result.EnvelopeID != "kept-envelope" || result.Recipient != "s1" || result.State != "" {
		t.Fatalf("a kept send = %+v, want the kept copy's envelope for s1 and no state", result)
	}
}

// identityStand is a listener stand-in that answers 401 to every bearer but the ones it takes,
// and records each call's Authorization and role mark.
type identityStand struct {
	mu    sync.Mutex
	takes map[string]bool
	calls []identityCall
	// before, when set, runs on each call before it is answered, with the call's number from 1.
	before func(n int)
}

type identityCall struct {
	authorization string
	role          any
}

func (s *identityStand) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls = append(s.calls, identityCall{authorization: r.Header.Get("Authorization"), role: body["role"]})
	n, before, taken := len(s.calls), s.before, s.takes[r.Header.Get("Authorization")]
	s.mu.Unlock()
	if before != nil {
		before(n)
	}
	if !taken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
}

func (s *identityStand) taken() []identityCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls
	s.calls = nil
	return calls
}

// A call the listener answered 401 on the account token is made once more on the token the file
// holds then, since the kubelet rewrites a projected token in place: a token rotated between the
// two tries is taken, and a send for a role keeps its mark. A token the listener refuses on both
// tries ends there for every call, with one Error line: a send for a role fails naming the role,
// and a call that carries no mark (a send to a session, a read) reports the listener's 401. No
// call is made a third time, on another credential or on none: the listener verifies Dispatch's
// identity, and no shared bearer stands behind it. A token file that cannot be read on either try
// makes no call at all.
func TestACallTheListenerRefusesOnTheAccountTokenIsMadeAgainOnAFreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener-account-token")
	account := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour))
	rotated := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(2*time.Hour))
	stand := &identityStand{takes: map[string]bool{}}
	listener := httptest.NewServer(http.HandlerFunc(stand.serve))
	defer listener.Close()
	marked := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}
	unmarked := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"}
	const refusedLine = "dispatch: the Envoy listener did not take Dispatch's identity token on two tries, so the call is not made on any other credential"
	client := func(logs *bytes.Buffer) *Client {
		return New(listener.URL, WithAccountTokenFile(path), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	}

	t.Run("a token rotated after a 401 is taken on the second try, with its role mark", func(t *testing.T) {
		writeAccountToken(t, path, account)
		stand.mu.Lock()
		stand.takes["Bearer "+rotated] = true
		stand.before = func(n int) {
			if n == 1 {
				writeAccountToken(t, path, rotated)
			}
		}
		stand.mu.Unlock()
		t.Cleanup(func() { stand.mu.Lock(); stand.before = nil; stand.mu.Unlock() })
		var logs bytes.Buffer
		if _, err := client(&logs).Send(context.Background(), marked); err != nil {
			t.Fatalf("a send whose token was rotated after its 401: %v", err)
		}
		got := stand.taken()
		if len(got) != 2 || got[0].authorization != "Bearer "+account || got[1].authorization != "Bearer "+rotated || got[1].role != "reviewer" {
			t.Fatalf("the listener was called %+v, want the first token, then the rotated one with the role mark", got)
		}
	})

	t.Run("a send for a role refused on two tries fails naming the role and is made no third time", func(t *testing.T) {
		writeAccountToken(t, path, account)
		var logs bytes.Buffer
		_, err := client(&logs).Send(context.Background(), marked)
		if err == nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "for role reviewer") {
			t.Fatalf("a send for a role refused on two tries = %v, want ErrUnavailable naming role reviewer", err)
		}
		got := stand.taken()
		if len(got) != 2 || got[0].authorization != "Bearer "+account || got[1].authorization != "Bearer "+account || got[0].role != "reviewer" || got[1].role != "reviewer" {
			t.Fatalf("the listener was called %+v, want two tries on the account token with the role mark and no third call", got)
		}
		if n := logLines(&logs, "ERROR", refusedLine, map[string]any{"role_mark": true}); n != 1 {
			t.Fatalf("the client wrote %d Error lines %q, want one:\n%s", n, refusedLine, logs.String())
		}
	})

	t.Run("a call with no role mark refused on two tries reports the 401 and is made no third time", func(t *testing.T) {
		writeAccountToken(t, path, account)
		var logs bytes.Buffer
		_, err := client(&logs).Send(context.Background(), unmarked)
		if err == nil || errors.Is(err, ErrUnreachable) || err.Error() != "unauthorized" {
			t.Fatalf("a send with no role mark refused on two tries = %v, want the listener's own 401 reason, not ErrUnreachable", err)
		}
		got := stand.taken()
		if len(got) != 2 || got[0].authorization != "Bearer "+account || got[1].authorization != "Bearer "+account || got[0].role != nil || got[1].role != nil {
			t.Fatalf("the listener was called %+v, want two tries on the account token, unmarked, and no third call", got)
		}
		if n := logLines(&logs, "ERROR", refusedLine, map[string]any{"role_mark": false}); n != 1 {
			t.Fatalf("the client wrote %d Error lines %q, want one:\n%s", n, refusedLine, logs.String())
		}
	})

	t.Run("an account token that cannot be read on two tries makes no call, for a read or a send for a role", func(t *testing.T) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove the token file: %v", err)
		}
		var logs bytes.Buffer
		unreadable := client(&logs)
		if _, err := unreadable.Send(context.Background(), unmarked); !errors.Is(err, ErrAccountToken) {
			t.Fatalf("a send with no role mark and no readable account token = %v, want ErrAccountToken", err)
		}
		if _, err := unreadable.Send(context.Background(), marked); !errors.Is(err, ErrAccountToken) {
			t.Fatalf("a send for a role with no readable account token = %v, want ErrAccountToken", err)
		}
		if got := stand.taken(); len(got) != 0 {
			t.Fatalf("the listener was called %+v with no readable account token, want no call", got)
		}
		if n := logLines(&logs, "ERROR", refusedLine, nil); n != 2 {
			t.Fatalf("the client wrote %d Error lines %q for its two calls, want two:\n%s", n, refusedLine, logs.String())
		}
	})
}

// writeFailingTransport answers the first round trip 401, as a listener refusing the account token
// does, and fails every later one before it writes anything, with a timeout: a second try that
// never reached the listener.
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

// A send whose first try the listener refused 401 and whose second try timed out before it was
// written never reached a route: the 401 came from the listener's authentication. It is the plain
// unreachable failure, not a receipt timeout, which would say the message may already be
// published. The control is a send whose one call was written and then timed out: that one is the
// receipt timeout.
func TestASecondTryThatWasNeverWrittenIsNoReceiptTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener-account-token")
	writeAccountToken(t, path, accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour)))
	input := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}

	client := New("http://listener.invalid", WithAccountTokenFile(path), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	transport := &writeFailingTransport{}
	client.httpClient.Transport = transport
	_, err := client.Send(context.Background(), input)
	if transport.calls.Load() != 2 {
		t.Fatalf("the client made %d round trips, want the refused one and its second try", transport.calls.Load())
	}
	if err == nil || errors.Is(err, ErrReceiptTimeout) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a second try that was never written = %v, want ErrUnreachable and not ErrReceiptTimeout", err)
	}

	if control := sendTransportError(context.DeadlineExceeded, true); !errors.Is(control, ErrReceiptTimeout) {
		t.Fatalf("control: a written send that timed out = %v, want ErrReceiptTimeout", control)
	}
}
