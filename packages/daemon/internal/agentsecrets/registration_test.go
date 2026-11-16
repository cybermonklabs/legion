package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// registrationBroker is POST /v1/oauth/exchange/pod-sessions as the broker serves it: it records
// each write's credential headers and body, and answers the next of answers, or once they run out
// a registration of pod (the pod uid the broker reads from the token every write presents) that
// lasts a day.
type registrationBroker struct {
	t   *testing.T
	pod string

	mu      sync.Mutex
	answers []enrollAnswer
	writes  []registrationWrite
}

// registrationWrite is one write the broker received and what it answered.
type registrationWrite struct {
	authorization, proof, contentType string
	body                              map[string]any
	status                            int
}

func newRegistrationBroker(t *testing.T, pod string) (*registrationBroker, *httptest.Server) {
	b := &registrationBroker{t: t, pod: pod}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/exchange/pod-sessions", b.handle)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return b, server
}

func (b *registrationBroker) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	write := registrationWrite{
		authorization: r.Header.Get("Authorization"), proof: r.Header.Get("Proof"), contentType: r.Header.Get("Content-Type"),
	}
	if err := json.Unmarshal(raw, &write.body); err != nil {
		b.t.Errorf("the registration's body %q is not JSON: %v", raw, err)
	}
	b.mu.Lock()
	answer := enrollAnswer{status: http.StatusOK, body: mustJSON(b.t, map[string]any{
		"pod_uid": b.pod, "session": write.body["session"], "expires_at": registrationExpiry,
	})}
	if len(b.answers) > 0 {
		answer, b.answers = b.answers[0], b.answers[1:]
	}
	write.status = answer.status
	b.writes = append(b.writes, write)
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	_, _ = w.Write([]byte(answer.body))
}

func (b *registrationBroker) received() []registrationWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.writes)
}

// registrationExpiry is when the stub broker's registrations lapse.
var registrationExpiry = time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)

// testPodToken is a pod's projected token as the shim's hello carries it.
const testPodToken = "eyJhbGciOiJSUzI1NiJ9.e30.pod-signature"

// manualTimers is the clock a renewal schedules by, run by hand: each After is held until the test
// fires it, so the test reads every wait the renewal chose and decides when each one ends.
type manualTimers struct {
	mu      sync.Mutex
	pending []*manualTimer
	waits   []time.Duration
}

type manualTimer struct {
	f              func()
	fired, stopped bool
}

func (m *manualTimers) after(d time.Duration, f func()) func() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	timer := &manualTimer{f: f}
	m.pending = append(m.pending, timer)
	m.waits = append(m.waits, d)
	return func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if timer.fired || timer.stopped {
			return false
		}
		timer.stopped = true
		return true
	}
}

// fire runs the oldest timer neither fired nor stopped, reporting whether there was one.
func (m *manualTimers) fire() bool {
	m.mu.Lock()
	var next *manualTimer
	for _, timer := range m.pending {
		if !timer.fired && !timer.stopped {
			next = timer
			break
		}
	}
	if next != nil {
		next.fired = true
	}
	m.mu.Unlock()
	if next == nil {
		return false
	}
	next.f()
	return true
}

// armed is how many timers are neither fired nor stopped.
func (m *manualTimers) armed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	armed := 0
	for _, timer := range m.pending {
		if !timer.fired && !timer.stopped {
			armed++
		}
	}
	return armed
}

func (m *manualTimers) scheduled() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.waits)
}

// TestRegisterPodSessionPostsThePodToken pins the registration's wire: one POST to
// /v1/oauth/exchange/pod-sessions whose body is the pod's token and the session name, and whose
// only credential is the daemon's own ServiceAccount token as a bearer — the credential the
// route's class reads (the broker's serviceAuth: serviceCaller verifies bearer(r) with
// MachineLogin.VerifyServiceToken), read from the token file at each call so the kubelet's
// rotation is picked up, and never a launcher proof. It answers the registration's expiry once the
// broker's answer names the pod the daemon registered; a refusal is an *APIError with the
// broker's code, and neither token is ever in an error.
func TestRegisterPodSessionPostsThePodToken(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-1")
	path := tokenFile(t, "the-daemon's-token")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: path}

	expires, err := c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer")
	if err != nil {
		t.Fatalf("RegisterPodSession: %v", err)
	}
	if !expires.Equal(registrationExpiry) {
		t.Errorf("RegisterPodSession answered %s, want the broker's expires_at %s", expires, registrationExpiry)
	}
	writes := broker.received()
	if len(writes) != 1 {
		t.Fatalf("the broker received %d registrations, want 1", len(writes))
	}
	got := writes[0]
	if got.authorization != "Bearer the-daemon's-token" || got.proof != "" {
		t.Errorf("the registration carried Authorization %q and Proof %q, want the daemon's own token as a bearer and no proof", got.authorization, got.proof)
	}
	if got.contentType != "application/json" {
		t.Errorf("the registration's Content-Type is %q, want application/json", got.contentType)
	}
	if want := map[string]any{"pod_token": testPodToken, "session": "ses_implementer"}; mustJSON(t, got.body) != mustJSON(t, want) {
		t.Errorf("the registration's body is %s, want exactly %s", mustJSON(t, got.body), mustJSON(t, want))
	}

	if err := os.WriteFile(path, []byte("the-rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer"); err != nil {
		t.Fatalf("RegisterPodSession after the rotation: %v", err)
	}
	if got := broker.received()[1].authorization; got != "Bearer the-rotated-token" {
		t.Errorf("after the kubelet rotated the token the registration carried %q, want the token the file holds now", got)
	}

	broker.mu.Lock()
	broker.answers = []enrollAnswer{{status: http.StatusForbidden, body: `{"code":"POD_ACCOUNT_NOT_LISTED","error":"system:serviceaccount:legion:worker is not in BROKER_EXCHANGE_ACCOUNTS"}`}}
	broker.mu.Unlock()
	_, err = c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer")
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusForbidden || api.Code != "POD_ACCOUNT_NOT_LISTED" || !IsPermanent(err) {
		t.Fatalf("a refused registration = %v, want a permanent *APIError 403 POD_ACCOUNT_NOT_LISTED", err)
	}
	for _, token := range []string{testPodToken, "the-rotated-token"} {
		if strings.Contains(err.Error(), token) {
			t.Errorf("the refusal %q repeats a token", err)
		}
	}

	missing := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: filepath.Join(t.TempDir(), "absent")}
	if _, err := missing.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer"); err == nil || !strings.Contains(err.Error(), "read the ServiceAccount token") {
		t.Errorf("a registration with no token file = %v, want a failure naming the token file", err)
	}
	if n := len(broker.received()); n != 3 {
		t.Errorf("the broker received %d registrations, want 3: a registration with no token to present sends nothing", n)
	}
}

// TestARegistrarNotListedRefusalIsRetried: the broker answers 403 POD_REGISTRAR_NOT_LISTED to a
// registrar its BROKER_POD_REGISTRARS does not pair with the pod's account, which it does until
// the deployment's registrar rows are applied (t-e-21), so that one 403 is a refusal a retry can
// change and every other 403 stays permanent. A renewal against a broker that answers it twice
// and then takes the write retries on its backoff — 15 seconds, then 30 — writes the
// registration, and then waits RegistrationRenewal for the next write. Each refused write is
// logged naming the pod and the session, and never a token.
func TestARegistrarNotListedRefusalIsRetried(t *testing.T) {
	for _, tc := range []struct {
		code      string
		permanent bool
	}{
		{"POD_REGISTRAR_NOT_LISTED", false},
		{"POD_ACCOUNT_NOT_LISTED", true},
		{"POD_CALLER_IS_THE_POD", true},
		{"ACCOUNT_NOT_LISTED", true},
		{"UNKNOWN", true},
	} {
		if got := (&APIError{Status: http.StatusForbidden, Code: tc.code}).Permanent(); got != tc.permanent {
			t.Errorf("403 %s: Permanent() = %t, want %t", tc.code, got, tc.permanent)
		}
	}

	const session = "ses_reviewer"
	notListed := enrollAnswer{status: http.StatusForbidden, body: `{"code":"POD_REGISTRAR_NOT_LISTED","error":"system:serviceaccount:legion:legion-daemon is not listed to register pods of system:serviceaccount:legion:worker"}`}
	broker, server := newRegistrationBroker(t, "pod-uid-1")
	broker.answers = []enrollAnswer{notListed, notListed}
	var logged lockedLog
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token"), Log: slog.New(slog.NewTextHandler(&logged, nil))}
	timers := &manualTimers{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := c.KeepPodSession(ctx, PodSessionRenewal{
		PodUID: "pod-uid-1", Session: session, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	if err == nil {
		t.Fatalf("the first write of session %s answered nil, want the broker's refusal", session)
	}
	for range 2 {
		if !timers.fire() {
			t.Fatalf("the renewal of session %s stopped after %d writes, want it to retry POD_REGISTRAR_NOT_LISTED until the broker takes the write", session, len(broker.received()))
		}
	}
	writes := broker.received()
	if len(writes) != 3 || writes[2].status != http.StatusOK {
		t.Fatalf("the broker received %d writes of session %s, the last answered %v; want two refusals and then the write it took", len(writes), session, writes)
	}
	if got, want := timers.scheduled(), []time.Duration{15 * time.Second, 30 * time.Second, RegistrationRenewal}; !slices.Equal(got, want) {
		t.Errorf("the renewal of session %s waited %v, want %v: the backoff after each refusal, then the renewal after the write", session, got, want)
	}
	text := logged.String()
	if strings.Count(text, "POD_REGISTRAR_NOT_LISTED") != 2 || !strings.Contains(text, "pod=pod-uid-1") || !strings.Contains(text, "session="+session) {
		t.Errorf("logged %q, want each refusal named once with the pod and the session", text)
	}
	if strings.Contains(text, testPodToken) || strings.Contains(text, "the-daemon's-token") {
		t.Errorf("logged %q, which repeats a token", text)
	}
}

// TestAPermanentRefusalEndsTheRenewal: a refusal no retry changes — here the pod's account in no
// exchange entry — is logged once, naming the pod and the session, and nothing more is written or
// scheduled for that registration.
func TestAPermanentRefusalEndsTheRenewal(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-2")
	broker.answers = []enrollAnswer{{status: http.StatusForbidden, body: `{"code":"POD_ACCOUNT_NOT_LISTED","error":"not listed"}`}}
	var logged lockedLog
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token"), Log: slog.New(slog.NewTextHandler(&logged, nil))}
	timers := &manualTimers{}
	err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: "pod-uid-2", Session: "ses_tester", Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	if !IsPermanent(err) {
		t.Fatalf("the first write answered %v, want the permanent refusal", err)
	}
	if timers.armed() != 0 || len(broker.received()) != 1 {
		t.Fatalf("after a permanent refusal %d writes were made and %d are scheduled, want 1 and none", len(broker.received()), timers.armed())
	}
	if text := logged.String(); !strings.Contains(text, "POD_ACCOUNT_NOT_LISTED") || !strings.Contains(text, "pod=pod-uid-2") || !strings.Contains(text, "session=ses_tester") {
		t.Errorf("logged %q, want the refusal named with the pod and the session", text)
	}
}

// TestTheRenewalEndsWithItsContext: a renewal is the claim's process's, and its context ends with
// it: the write scheduled next is stopped, and a timer that fires anyway writes nothing.
func TestTheRenewalEndsWithItsContext(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-3")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
	timers := &manualTimers{}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.KeepPodSession(ctx, PodSessionRenewal{
		PodUID: "pod-uid-3", Session: "ses_architect", Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	}); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if timers.armed() != 1 {
		t.Fatalf("%d writes are scheduled after the first, want the renewal", timers.armed())
	}
	cancel()
	eventually(t, func() bool { return timers.armed() == 0 }, "the scheduled renewal to be stopped once the context ended")
	if n := len(broker.received()); n != 1 {
		t.Fatalf("the broker received %d writes, want only the first", n)
	}
}

// TestARenewalReadsAFreshTokenForEachWrite: every write asks for the pod token it presents, so a
// write six hours after the hello presents the token the pod holds then, and a token that cannot
// be read is a failed write, retried on the backoff like any other.
func TestARenewalReadsAFreshTokenForEachWrite(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-4")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
	timers := &manualTimers{}
	tokens := []string{"token-at-the-hello", "", "token-six-hours-on"}
	read := 0
	err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: "pod-uid-4", Session: "ses_implementer", Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) {
			token := tokens[read]
			read++
			if token == "" {
				return "", errors.New("the shim did not answer for its projected token")
			}
			return token, nil
		},
	})
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	timers.fire()
	timers.fire()
	var presented []string
	for _, write := range broker.received() {
		presented = append(presented, write.body["pod_token"].(string))
	}
	if want := []string{"token-at-the-hello", "token-six-hours-on"}; !slices.Equal(presented, want) {
		t.Errorf("the writes presented %v, want %v", presented, want)
	}
	if got, want := timers.scheduled(), []time.Duration{RegistrationRenewal, 15 * time.Second, RegistrationRenewal}; !slices.Equal(got, want) {
		t.Errorf("the renewal waited %v, want %v", got, want)
	}
}

// TestARegistrationAnsweredForAnotherPodIsNotRenewed: the broker answers the registration as it now
// stands, the pod uid it read from the token included (handlers_pod_sessions.go,
// podSessionResponse). An answer that names another pod than the one the daemon registered
// means the token the daemon presented was not that pod's, so the write is a permanent failure:
// nothing is written again or scheduled. The one log line names the session and both uids, and
// no token.
func TestARegistrationAnsweredForAnotherPodIsNotRenewed(t *testing.T) {
	const (
		session = "ses_planner"
		mine    = "pod-uid-mine"
		other   = "pod-uid-other"
	)
	broker, server := newRegistrationBroker(t, other)
	var logged lockedLog
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token"), Log: slog.New(slog.NewTextHandler(&logged, nil))}

	_, err := c.RegisterPodSession(context.Background(), mine, testPodToken, session)
	var mismatch *PodMismatchError
	if !errors.As(err, &mismatch) || mismatch.Session != session || mismatch.Registered != mine || mismatch.Answered != other || !IsPermanent(err) {
		t.Fatalf("a registration the broker answered for pod %s, where the daemon registered %s for %s = %v, want a permanent *PodMismatchError naming both", other, mine, session, err)
	}

	timers := &manualTimers{}
	err = c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: mine, Session: session, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	if !errors.As(err, &mismatch) || !IsPermanent(err) {
		t.Fatalf("the renewal of %s for pod %s, answered for pod %s = %v, want the permanent mismatch", session, mine, other, err)
	}
	if timers.armed() != 0 || len(broker.received()) != 2 {
		t.Fatalf("after the mismatch %d writes were made and %d are scheduled, want 2 and none", len(broker.received()), timers.armed())
	}
	text := logged.String()
	for _, want := range []string{"session=" + session, mine, other} {
		if !strings.Contains(text, want) {
			t.Errorf("logged %q, want it to name %s", text, want)
		}
	}
	if strings.Contains(text, testPodToken) || strings.Contains(text, "the-daemon's-token") {
		t.Errorf("logged %q, which repeats a token", text)
	}
}
