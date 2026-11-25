package cipher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// registrationBroker is POST /v1/oauth/exchange/pod-sessions as the broker serves it: it records
// each write's credential headers and body, and answers the next of answers, or once they run out
// a registration of pod (the pod uid the broker reads from the token every write presents) that
// lasts a day, bound to the thumbprint the write sent — or to bound, where a test sets it, as a
// broker that kept another binding answers — at the generation the write sent, or at newer, where
// a test sets it, as a broker holding a later process's registration answers a stale write.
type registrationBroker struct {
	t     *testing.T
	pod   string
	bound string

	mu      sync.Mutex
	newer   int64
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
	thumbprint := write.body["thumbprint"]
	if b.bound != "" {
		thumbprint = b.bound
	}
	generation := write.body["generation"]
	if b.newer > 0 {
		generation = b.newer
	}
	answer := enrollAnswer{status: http.StatusOK, body: mustJSON(b.t, map[string]any{
		"pod_uid": b.pod, "session": write.body["session"], "thumbprint": thumbprint, "generation": generation, "expires_at": registrationExpiry,
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

// testThumbprint is the RFC 7638 thumbprint of a role's own key, as its shim's hello names it.
const testThumbprint = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"

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
// /v1/oauth/exchange/pod-sessions whose body is the pod's token, the session name and the
// thumbprint of the role's own key, and whose only credential is the daemon's own ServiceAccount
// token as a bearer — the credential the route's class reads (the broker's serviceAuth:
// serviceCaller verifies bearer(r) with MachineLogin.VerifyServiceToken), read from the token file
// at each call so the kubelet's rotation is picked up, and never a launcher proof. It answers the
// registration's expiry once the broker's answer names the pod and the key the daemon registered;
// a refusal is an *APIError with the broker's code, and neither token is ever in an error.
func TestRegisterPodSessionPostsThePodToken(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-1")
	path := tokenFile(t, "the-daemon's-token")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: path}

	expires, err := c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 7)
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
	if want := map[string]any{"pod_token": testPodToken, "session": "ses_implementer", "thumbprint": testThumbprint, "generation": 7}; mustJSON(t, got.body) != mustJSON(t, want) {
		t.Errorf("the registration's body is %s, want exactly %s", mustJSON(t, got.body), mustJSON(t, want))
	}

	if err := os.WriteFile(path, []byte("the-rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 7); err != nil {
		t.Fatalf("RegisterPodSession after the rotation: %v", err)
	}
	if got := broker.received()[1].authorization; got != "Bearer the-rotated-token" {
		t.Errorf("after the kubelet rotated the token the registration carried %q, want the token the file holds now", got)
	}

	broker.mu.Lock()
	broker.answers = []enrollAnswer{{status: http.StatusForbidden, body: `{"code":"POD_ACCOUNT_NOT_LISTED","error":"system:serviceaccount:legion:worker is not in BROKER_EXCHANGE_ACCOUNTS"}`}}
	broker.mu.Unlock()
	_, err = c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 7)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusForbidden || !api.HasCode("POD_ACCOUNT_NOT_LISTED") || !IsPermanent(err) {
		t.Fatalf("a refused registration = %v, want a permanent *APIError 403 POD_ACCOUNT_NOT_LISTED", err)
	}
	for _, token := range []string{testPodToken, "the-rotated-token"} {
		if strings.Contains(err.Error(), token) {
			t.Errorf("the refusal %q repeats a token", err)
		}
	}

	missing := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: filepath.Join(t.TempDir(), "absent")}
	if _, err := missing.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 7); err == nil || !strings.Contains(err.Error(), "read the ServiceAccount token") {
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
		if got := (&APIError{Status: http.StatusForbidden, said: oauth2calltest.Said(tc.code)}).Permanent(); got != tc.permanent {
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

	stop, err := c.KeepPodSession(ctx, PodSessionRenewal{
		PodUID: "pod-uid-1", Session: session, Thumbprint: testThumbprint, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	defer stop()
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
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: "pod-uid-2", Session: "ses_tester", Thumbprint: testThumbprint, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	defer stop()
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
	stop, err := c.KeepPodSession(ctx, PodSessionRenewal{
		PodUID: "pod-uid-3", Session: "ses_architect", Thumbprint: testThumbprint, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	defer stop()
	if err != nil {
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
// be read is a failed write, retried on the backoff like any other. Every write sends the
// registration's thumbprint again: the broker keeps the binding the last write named.
func TestARenewalReadsAFreshTokenForEachWrite(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-4")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
	timers := &manualTimers{}
	tokens := []string{"token-at-the-hello", "", "token-six-hours-on"}
	read := 0
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: "pod-uid-4", Session: "ses_implementer", Thumbprint: testThumbprint, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) {
			token := tokens[read]
			read++
			if token == "" {
				return "", errors.New("the shim did not answer for its projected token")
			}
			return token, nil
		},
	})
	defer stop()
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	timers.fire()
	timers.fire()
	var presented, bound []string
	for _, write := range broker.received() {
		presented = append(presented, write.body["pod_token"].(string))
		thumbprint, _ := write.body["thumbprint"].(string)
		bound = append(bound, thumbprint)
	}
	if want := []string{"token-at-the-hello", "token-six-hours-on"}; !slices.Equal(presented, want) {
		t.Errorf("the writes presented %v, want %v", presented, want)
	}
	if want := []string{testThumbprint, testThumbprint}; !slices.Equal(bound, want) {
		t.Errorf("the writes of session ses_implementer sent the thumbprints %q, want the registration's own, %s, in every write", bound, testThumbprint)
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

	timers := &manualTimers{}
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: mine, Session: session, Thumbprint: testThumbprint, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	defer stop()
	var mismatch *AnswerMismatchError
	if !errors.As(err, &mismatch) || mismatch.Field != "pod_uid" || mismatch.Pod != mine || mismatch.Session != session || mismatch.Sent != mine || mismatch.Answered != other || !IsPermanent(err) {
		t.Fatalf("the renewal of %s for pod %s, answered for pod %s = %v, want a permanent *AnswerMismatchError naming both", session, mine, other, err)
	}
	if timers.armed() != 0 || len(broker.received()) != 1 {
		t.Fatalf("after the mismatch %d writes were made and %d are scheduled, want 1 and none", len(broker.received()), timers.armed())
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

// TestRegisterPodSessionRefusesAnEmptyThumbprint: a Legion registration binds the session to the
// role's own key, and a write that names no key would leave the session open to every role of the
// pod (the broker keeps a registration with no thumbprint pod-bound), so RegisterPodSession
// refuses one before it sends anything, with a permanent error naming the session.
func TestRegisterPodSessionRefusesAnEmptyThumbprint(t *testing.T) {
	broker, server := newRegistrationBroker(t, "pod-uid-1")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}

	_, err := c.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_tester", "", 1)
	if writes := broker.received(); len(writes) != 0 {
		t.Fatalf("a registration of session ses_tester with no thumbprint sent %s, want nothing sent", mustJSON(t, writes[0].body))
	}
	var missing *MissingThumbprintError
	if !errors.As(err, &missing) || missing.Session != "ses_tester" || !IsPermanent(err) || !strings.Contains(err.Error(), "ses_tester") {
		t.Fatalf("a registration of session ses_tester with no thumbprint = %v, want a permanent *MissingThumbprintError naming the session", err)
	}
}

// TestAnAnswerForAnotherPodOrKeyIsRefused: the broker answers the registration as it stands after
// the write, the pod uid it read from the token and the thumbprint the row is bound to included.
// An answer that names another pod, or another key, than the daemon registered is a permanent
// failure naming the pod, the session and both values.
func TestAnAnswerForAnotherPodOrKeyIsRefused(t *testing.T) {
	const otherThumbprint = "0tHerKeyThumbprint-aaaaaaaaaaaaaaaaaaaaaaaa"
	for _, c := range []struct {
		name, field, answeredPod, answeredThumbprint, sent, answered string
	}{
		{"another pod", "pod_uid", "pod-uid-other", "", "pod-uid-mine", "pod-uid-other"},
		{"another key", "thumbprint", "pod-uid-mine", otherThumbprint, testThumbprint, otherThumbprint},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker, server := newRegistrationBroker(t, c.answeredPod)
			broker.bound = c.answeredThumbprint
			client := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
			_, err := client.RegisterPodSession(context.Background(), "pod-uid-mine", testPodToken, "ses_reviewer", testThumbprint, 1)
			var mismatch *AnswerMismatchError
			if !errors.As(err, &mismatch) || !IsPermanent(err) {
				t.Fatalf("a registration of pod-uid-mine for ses_reviewer answered with %s %q = %v, want a permanent *AnswerMismatchError", c.field, c.answered, err)
			}
			if mismatch.Field != c.field || mismatch.Pod != "pod-uid-mine" || mismatch.Session != "ses_reviewer" || mismatch.Sent != c.sent || mismatch.Answered != c.answered {
				t.Errorf("the mismatch is %+v, want field %s, pod pod-uid-mine, session ses_reviewer, sent %q and answered %q", *mismatch, c.field, c.sent, c.answered)
			}
			for _, want := range []string{"pod-uid-mine", "ses_reviewer", c.sent, c.answered} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
		})
	}
}

// TestAStopWaitsForTheWriteInFlight: KeepPodSession's stop stops the write scheduled next and
// returns only once a write in flight has been answered, without cancelling it, so a caller that
// writes the next registration after the stop knows the last one has landed. A write the stop
// found in flight schedules nothing more.
func TestAStopWaitsForTheWriteInFlight(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var cancelled atomic.Bool
	writes := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		writes++
		n := writes
		mu.Unlock()
		if n == 2 {
			once.Do(func() { close(held) })
			<-release
			cancelled.Store(r.Context().Err() != nil)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mustJSON(t, map[string]any{"pod_uid": "pod-uid-5", "session": "ses_merger", "thumbprint": testThumbprint, "expires_at": registrationExpiry})))
	}))
	defer server.Close()
	defer close(release)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
	timers := &manualTimers{}
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: "pod-uid-5", Session: "ses_merger", Thumbprint: testThumbprint, Bound: time.Minute, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
	})
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	go timers.fire()
	<-held
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("the stop returned while the renewal of ses_merger was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	release <- struct{}{}
	<-stopped
	if cancelled.Load() {
		t.Error("the stop cancelled the renewal of ses_merger in flight, want it answered")
	}
	if n := timers.armed(); n != 0 {
		t.Errorf("%d writes of ses_merger are scheduled after the stop, want none", n)
	}
}

// TestALateRenewalOfAnEarlierProcessStopsOnceSuperseded: every write sends the launch generation
// of the process it registers, and the broker answers the registration as it holds it, its
// generation included, writing a write only when its generation is at least the one held. A
// renewal of an earlier process (generation 2) answered with a later generation (3) — a later
// process of the claim has registered the session since — is superseded: a permanent failure
// naming the pod, the session and both generations, logged once, and nothing more is written or
// scheduled. The same key on both sides is the case only the generation tells apart: an answer
// bound to another key is refused anyway (TestAnAnswerForAnotherPodOrKeyIsRefused).
func TestALateRenewalOfAnEarlierProcessStopsOnceSuperseded(t *testing.T) {
	const (
		pod     = "pod-uid-6"
		session = "ses_superseded"
	)
	broker, server := newRegistrationBroker(t, pod)
	var logged lockedLog
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token"), Log: slog.New(slog.NewTextHandler(&logged, nil))}
	timers := &manualTimers{}
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: pod, Session: session, Thumbprint: testThumbprint, Generation: 2, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
		// The claim has moved on to generation 3, the generation the broker answers.
		Latest: func() int64 { return 3 },
	})
	defer stop()
	if err != nil {
		t.Fatalf("the first write of %s at generation 2: %v", session, err)
	}

	broker.mu.Lock()
	broker.newer = 3
	broker.mu.Unlock()
	timers.fire()
	writes := broker.received()
	if len(writes) != 2 || writes[1].body["generation"] != float64(2) {
		t.Fatalf("the broker received %d writes of %s, want the first and its renewal, both at generation 2: %v", len(writes), session, writes)
	}
	if n := timers.armed(); n != 0 {
		t.Fatalf("the renewal of %s at generation 2, answered at generation 3, scheduled %d more writes, want none: an earlier process kept writing over a later one's registration", session, n)
	}
	text := logged.String()
	for _, want := range []string{"pod=" + pod, "session=" + session, "generation 2", "generation 3", "a later process of this claim"} {
		if !strings.Contains(text, want) {
			t.Errorf("logged %q, want it to name %s", text, want)
		}
	}
	if strings.Contains(text, "never made") {
		t.Errorf("logged %q: a later process of this claim's registration read as one this daemon never made", text)
	}

	_, err = c.RegisterPodSession(context.Background(), pod, testPodToken, session, testThumbprint, 2)
	var superseded *SupersededError
	if !errors.As(err, &superseded) || superseded.Pod != pod || superseded.Session != session || superseded.Sent != 2 || superseded.Answered != 3 || !IsPermanent(err) {
		t.Fatalf("a write of %s at generation 2 answered at generation 3 = %v, want a permanent *SupersededError naming the pod, the session and both generations", session, err)
	}
}

// TestARegistrationAtAGenerationTheClaimNeverReachedIsLoggedAsForeign: the broker answers a
// write with a registration at a generation above the claim's current one — above every process
// this daemon has launched for the claim — so no process of this claim wrote it: another claim of
// this daemon registered the session id, or another process of the daemon's account wrote the row
// (review round 4 of #4153, R4-1, R4-2). That is a permanent failure like any superseded write,
// logged at Error with an error that says so: the answered generation, above the claim's current
// one as Latest reads it, and a registration this daemon never made, never a later process of this
// claim. The log line's prefix is the one every permanent refusal shares (review round 5 of #4153,
// R5-M1): the case lives in the error, which is what the caller gets back as well.
func TestARegistrationAtAGenerationTheClaimNeverReachedIsLoggedAsForeign(t *testing.T) {
	const (
		pod     = "pod-uid-10"
		session = "ses_foreign"
	)
	broker, server := newRegistrationBroker(t, pod)
	var logged lockedLog
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token"), Log: slog.New(slog.NewTextHandler(&logged, nil))}
	timers := &manualTimers{}
	stop, err := c.KeepPodSession(context.Background(), PodSessionRenewal{
		PodUID: pod, Session: session, Thumbprint: testThumbprint, Generation: 2, Bound: time.Second, After: timers.after,
		Token: func(context.Context) (string, error) { return testPodToken, nil },
		// The claim has moved on to generation 4, still below the 9 the broker will answer.
		Latest: func() int64 { return 4 },
	})
	defer stop()
	if err != nil {
		t.Fatalf("the first write of %s at generation 2: %v", session, err)
	}

	broker.mu.Lock()
	broker.newer = 9
	broker.mu.Unlock()
	timers.fire()
	if n := timers.armed(); n != 0 {
		t.Fatalf("the renewal of %s, answered at generation 9 above the claim's 4, scheduled %d more writes, want none", session, n)
	}
	const foreign = "at generation 9, above this claim's generation 4 (this write's was generation 2): a registration this daemon never made"
	var line string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, foreign) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("logged %q, want the answer at generation 9, above the claim's generation 4, logged with %q", logged.String(), foreign)
	}
	for _, want := range []string{"level=ERROR", "pod=" + pod, "session=" + session} {
		if !strings.Contains(line, want) {
			t.Errorf("logged %q, want it to name %s", line, want)
		}
	}
	if strings.Contains(line, "a later process of this claim") {
		t.Errorf("logged %q: a registration no process of this claim made read as a later process of it", line)
	}
}

// releaseBroker is POST /v1/oauth/exchange/pod-sessions/release answering every call with status
// and body, and recording each call's credential headers and raw body.
type releaseBroker struct {
	status int
	body   string

	mu    sync.Mutex
	calls []releaseCall
}

// releaseCall is one release the broker received.
type releaseCall struct {
	authorization, proof, contentType, body string
}

func newReleaseBroker(t *testing.T, status int, body string) (*releaseBroker, *httptest.Server) {
	b := &releaseBroker{status: status, body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/exchange/pod-sessions/release", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.calls = append(b.calls, releaseCall{
			authorization: r.Header.Get("Authorization"), proof: r.Header.Get("Proof"), contentType: r.Header.Get("Content-Type"), body: string(raw),
		})
		b.mu.Unlock()
		w.WriteHeader(b.status)
		_, _ = w.Write([]byte(b.body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return b, server
}

func (b *releaseBroker) received() []releaseCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// TestAReleaseIsSentAsARegistrationIsAndTakenOnlyAtItsCount: ReleasePodSessions posts the names to
// POST /v1/oauth/exchange/pod-sessions/release as RegisterPodSession posts its write, under the
// daemon's own ServiceAccount token and no launcher proof, the body exactly {"sessions": [...]} as
// given. It takes the names as released only from a 200 that counts the distinct names sent: a 200
// counting otherwise, or not at all, is an answer it cannot read and an error, and every other
// answer is the broker's refusal with its code, a 404 read as a broker with no such route.
//
// The control (the pull request records the run): ReleasePodSessions taking any 200 as released
// fails the rows of a 200 that counts every name sent and of one that counts none.
func TestAReleaseIsSentAsARegistrationIsAndTakenOnlyAtItsCount(t *testing.T) {
	names := []string{"ses_b", "ses_a", "ses_b"}
	for _, c := range []struct {
		name, body string
		status     int
		// apiStatus is the *APIError's status the release answers, 0 for none; permanent and
		// unserved are what that refusal reads as.
		apiStatus           int
		permanent, unserved bool
		// unread is a fragment of the error of a 200 the release cannot read, "" for none.
		unread string
	}{
		{name: "a 200 counting the distinct names", status: http.StatusOK, body: `{"sessions":2,"released":1}`},
		{name: "a 200 counting every name sent", status: http.StatusOK, body: `{"sessions":3,"released":1}`, unread: "releasing 3 sessions, where the daemon sent 2 distinct names"},
		{name: "a 200 counting none", status: http.StatusOK, body: `{"released":1}`, unread: "releasing no count of sessions, where the daemon sent 2 distinct names"},
		{name: "a 503", status: http.StatusServiceUnavailable, body: "no healthy upstream", apiStatus: http.StatusServiceUnavailable},
		{name: "a 404 from a broker before the route", status: http.StatusNotFound, body: "404 page not found", apiStatus: http.StatusNotFound, permanent: true, unserved: true},
		{name: "a 400", status: http.StatusBadRequest, body: `{"code":"SESSIONS_INVALID","error":"sessions holds 0 names"}`, apiStatus: http.StatusBadRequest, permanent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker, server := newReleaseBroker(t, c.status, c.body)
			client := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}

			err := client.ReleasePodSessions(context.Background(), names)

			calls := broker.received()
			if len(calls) != 1 {
				t.Fatalf("the release made %d calls, want 1", len(calls))
			}
			if call := calls[0]; call.authorization != "Bearer the-daemon's-token" || call.proof != "" || call.contentType != "application/json" ||
				call.body != `{"sessions":["ses_b","ses_a","ses_b"]}` {
				t.Fatalf("the release was sent %+v, want the daemon's own token as its bearer, no proof, and the body {\"sessions\": the names as given}", call)
			}
			var api *APIError
			switch {
			case c.apiStatus == 0 && c.unread == "":
				if err != nil {
					t.Fatalf("the release answered %d %s = %v, want the names released", c.status, c.body, err)
				}
			case c.unread != "":
				if err == nil || errors.As(err, &api) || IsPermanent(err) || !strings.Contains(err.Error(), c.unread) {
					t.Fatalf("the release answered %d %s = %v, want an answer it cannot read, saying %q", c.status, c.body, err, c.unread)
				}
			default:
				if !errors.As(err, &api) || api.Status != c.apiStatus || api.Permanent() != c.permanent || api.RouteUnserved() != c.unserved {
					t.Fatalf("the release answered %d %s = %#v, want the broker's refusal %d, permanent %t, a route unserved %t", c.status, c.body, err, c.apiStatus, c.permanent, c.unserved)
				}
			}
		})
	}
}

// TestAReleasePastTheBrokersBoundsIsNotSent: the broker refuses a release of more than 4096 names,
// or with a body longer than 1 MiB, so ReleasePodSessions sends neither, and answers a permanent
// *ReleaseTooLargeError naming the bound. A release at the count bound whose body is under the
// byte bound is sent, so neither bound is the client's own, tighter one.
//
// The control (the pull request records the run): ReleasePodSessions without its byte bound sends
// the 4096 names of 300 bytes.
func TestAReleasePastTheBrokersBoundsIsNotSent(t *testing.T) {
	named := func(n, size int) []string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("%0*d", size, i)
		}
		return names
	}
	for _, c := range []struct {
		name  string
		names []string
		bound string
	}{
		{"4097 names", named(4097, 8), "it names 4097 sessions, more than the 4096 one release carries"},
		{"4096 names of 300 bytes", named(4096, 300), "longer than the 1048576 the broker reads"},
		{"4096 names of 250 bytes", named(4096, 250), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker, server := newReleaseBroker(t, http.StatusOK, `{"sessions":4096,"released":0}`)
			client := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}

			err := client.ReleasePodSessions(context.Background(), c.names)

			sent := len(broker.received())
			if c.bound == "" {
				if err != nil || sent != 1 {
					t.Fatalf("the release within both bounds = %v after %d calls, want it sent once and released", err, sent)
				}
				return
			}
			var tooLarge *ReleaseTooLargeError
			if !errors.As(err, &tooLarge) || !IsPermanent(err) || !strings.Contains(err.Error(), c.bound) {
				t.Fatalf("the release of %s = %v, want a permanent *ReleaseTooLargeError saying %q", c.name, err, c.bound)
			}
			if sent != 0 {
				t.Fatalf("the release of %s was sent %d times, want never", c.name, sent)
			}
		})
	}
}
