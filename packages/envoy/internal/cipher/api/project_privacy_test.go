// project_privacy_test.go drives GET /v1/project-privacy, the route a listener reads at each privacy
// decision. It answers only the BROKER_NATS_SERVICES entries natscallout decides read project
// privacy, asks Dispatch afresh on every call through the broker's real projectaccess client,
// answers no list at all when that read fails, and holds at most 64 calls at once, 16 of one
// account's. Dispatch is a stub of its private-projects route, which each test drives; the bound
// tests hold their calls in a stand-in for that read instead, so no clock of Dispatch's is in the
// way.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

const (
	// privacyListenerAccount is a hub's listener and dispatchListenerAccount Dispatch's listener
	// Deployment; secondListenerAccount and thirdListenerAccount are two more hubs' listeners, so
	// the bound test holds 64 calls from four accounts of 16. Each is a listener entry with the
	// whole grant.
	privacyListenerAccount  = "system:serviceaccount:example-hub:example-listener"
	dispatchListenerAccount = "system:serviceaccount:example-ns:example-dispatch-listener"
	secondListenerAccount   = "system:serviceaccount:example-hub:example-listener-b"
	thirdListenerAccount    = "system:serviceaccount:example-hub:example-listener-c"
	// scopedListenerAccount is a listener held to its exchange projects (-), and measureAccount
	// the measurement job.
	scopedListenerAccount = "system:serviceaccount:example-hub:example-scoped-listener"
	measureAccount        = "system:serviceaccount:example-ns:example-measure"
	// privacyMeasureAccount is plan C's privacy measurement Job, which reads the route too.
	privacyMeasureAccount = "system:serviceaccount:example-ns:example-privacy-measure"
	// privacyStubToken is the bearer the broker presents to the stub, as it presents its own
	// account token to Dispatch.
	privacyStubToken = "dispatch-access-example"
	// privacyPath is the route under test.
	privacyPath = "/v1/project-privacy"
)

// privacyServices is the BROKER_NATS_SERVICES the rig reads, parsed as the broker parses it: every
// kind the auth service admits, and both scopes of a listener. Dispatch's listener is a listener
// entry, the kind the auth service answers it with today.
var privacyServices = strings.Join([]string{
	"listener/example-hub/*=" + privacyListenerAccount,
	"listener/example-dispatch/*=" + dispatchListenerAccount,
	"listener/example-hub-b/*=" + secondListenerAccount,
	"listener/example-hub-c/*=" + thirdListenerAccount,
	"listener/example-vm/*=google:" + listenerGoogleAccount,
	"listener/example-scoped/-=" + scopedListenerAccount,
	"dispatch/example-dispatch/*=" + dispatchAccount,
	"daemon/-/*=" + daemonAccount,
	"measure/-/*=" + measureAccount,
	"privacy-measure/project-privacy-measure/*=" + privacyMeasureAccount,
}, " ")

// privacyStub stands in for Dispatch's GET /api/v1/access/private-projects. Each request is counted,
// then answered by the answer that holds when it arrives, so a test can change Dispatch between two
// calls or make Dispatch fail.
type privacyStub struct {
	url   string
	calls atomic.Int64

	mu     sync.Mutex
	answer http.HandlerFunc
}

// newPrivacyStub serves answer until t ends.
func newPrivacyStub(t *testing.T, answer http.HandlerFunc) *privacyStub {
	t.Helper()
	stub := &privacyStub{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/access/private-projects" || r.Header.Get("Authorization") != "Bearer "+privacyStubToken {
			t.Errorf("the broker asked Dispatch %s %s with %q, want GET /api/v1/access/private-projects with its own bearer", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		stub.calls.Add(1)
		stub.mu.Lock()
		answer := stub.answer
		stub.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

// set makes answer Dispatch's answer to every request from the next one on.
func (s *privacyStub) set(answer http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

// answerWith answers status with body as Dispatch writes JSON.
func answerWith(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// dispatchAt is the broker's projectaccess client of stub, on httpClient (nil for the client's own
// default).
func dispatchAt(stub *privacyStub, httpClient *http.Client) api.FreshPrivateProjects {
	return projectaccess.New(stub.url, projectaccesstest.StaticAccount(privacyStubToken), httpClient)
}

// newPrivacyServer is the broker's test server reading Dispatch through source (nil for a broker
// with no BROKER_PROJECT_ACCESS_URL), with privacyServices as its BROKER_NATS_SERVICES, read as
// cmd/cipherd reads it (natscallout.PrivacyReaders), a Google issuer of its own for the VM
// listener's and the operator's identity tokens, and each of adjust applied after.
func newPrivacyServer(t *testing.T, source api.FreshPrivateProjects, adjust ...func(*api.Deps)) *googleCallers {
	t.Helper()
	services, err := natscallout.ParseServices(privacyServices)
	if err != nil {
		t.Fatalf("parse BROKER_NATS_SERVICES: %v", err)
	}
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	verifier, err := oidc.New(context.Background(), google.URL(), machineAudience)
	if err != nil {
		t.Fatalf("oidc.New for Google: %v", err)
	}
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.GoogleAccountTokens = api.GoogleTokens{Verifier: verifier}
		d.PrivacyReaders = natscallout.PrivacyReaders(services)
		d.ProjectPrivacy = source
		for _, a := range adjust {
			a(d)
		}
	})
	return &googleCallers{testServer: ts, issuer: google, key: key}
}

// privacyAnswer is one answer of the route: its status, its body and the code a refusal names.
type privacyAnswer struct {
	status int
	body   []byte
	code   string
}

// privacyCall makes one GET /v1/project-privacy presenting bearer. It is safe to call from any
// goroutine: a failure to send is its error, never a failed test.
func privacyCall(url, bearer string) (privacyAnswer, error) {
	request, err := http.NewRequest(http.MethodGet, url+privacyPath, nil)
	if err != nil {
		return privacyAnswer{}, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return privacyAnswer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return privacyAnswer{}, err
	}
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return privacyAnswer{status: resp.StatusCode, body: body, code: refusal.Code}, nil
}

// privacy is privacyCall from the test's own goroutine.
func (g *googleCallers) privacy(t *testing.T, bearer string) privacyAnswer {
	t.Helper()
	answer, err := privacyCall(g.URL, bearer)
	if err != nil {
		t.Fatalf("GET %s: %v", privacyPath, err)
	}
	return answer
}

// privacyInto makes one call from a goroutine of its own and sends its answer on into, so a test
// holds calls open, or bounds how long it waits for one.
func (g *googleCallers) privacyInto(into chan<- privacyAnswer, bearer string) {
	go func() {
		answer, err := privacyCall(g.URL, bearer)
		if err != nil {
			answer = privacyAnswer{body: []byte(err.Error())}
		}
		into <- answer
	}()
}

// sameJSON reports whether got and want are the same JSON value.
func sameJSON(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the test's own JSON does not decode: %v", err)
	}
	return reflect.DeepEqual(g, w)
}

// heldReads stands in for the broker's read of Dispatch in the bound tests. It counts each read,
// and while holding it parks each one, after saying so on arrived, until a release, so a test
// holds calls open for as long as it needs and no clock of Dispatch's ends them.
type heldReads struct {
	calls   atomic.Int64
	holding atomic.Bool
	arrived chan struct{}
	release chan struct{}
}

// newHeldReads is a stand-in that holds every read until released (releaseAllWhenDone).
func newHeldReads() *heldReads {
	h := &heldReads{arrived: make(chan struct{}, 128), release: make(chan struct{})}
	h.holding.Store(true)
	return h
}

func (h *heldReads) PrivateProjectsFresh(ctx context.Context) ([]projectaccess.PrivateProject, error) {
	h.calls.Add(1)
	if h.holding.Load() {
		h.arrived <- struct{}{}
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []projectaccess.PrivateProject{}, nil
}

// releaseAllWhenDone closes release when t ends; registered after the server, it runs first.
func (h *heldReads) releaseAllWhenDone(t *testing.T) *sync.Once {
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(h.release) }) })
	return &once
}

// hold starts n calls presenting bearer, answering on into, and waits until each has reached the
// read.
func (g *googleCallers) hold(t *testing.T, reads *heldReads, into chan<- privacyAnswer, bearer string, n int) {
	t.Helper()
	for range n {
		g.privacyInto(into, bearer)
	}
	for i := range n {
		select {
		case <-reads.arrived:
		case <-time.After(time.Minute):
			t.Fatalf("only %d of %d calls reached the read", i, n)
		}
	}
}

// within waits at most d for one answer on from: a call the route held open, which no clock ends
// here, fails the test rather than hanging it.
func within(from <-chan privacyAnswer, d time.Duration) (privacyAnswer, bool) {
	select {
	case answer := <-from:
		return answer, true
	case <-time.After(d):
		return privacyAnswer{}, false
	}
}

// TestOnlyAListenerReadsProjectPrivacy: a listener (the hub's, Dispatch's listener Deployment's,
// and a VM's presenting a Google identity token) and the privacy measurement are answered
// Dispatch's rows unchanged. Every other caller whose platform identity verifies is refused 403
// not_listener before Dispatch is asked: Dispatch's app account, a session pod's own token, a
// daemon's identity, the operator's Google identity, the measurement job, a listener held to its
// exchange projects and an account listed nowhere. A listener's token that does not verify, and
// no token at all, are refused 401, Dispatch not asked either.
func TestOnlyAListenerReadsProjectPrivacy(t *testing.T) {
	const body = `{"projects":[` +
		`{"project":"ALPHA","slack_channels":["C0A","C0B"],"slack_channels_changed_at":"2026-11-19T10:00:00Z"},` +
		`{"project":"BETA","slack_channels":[],"slack_channels_changed_at":null}]}`
	stub := newPrivacyStub(t, answerWith(http.StatusOK, body))
	g := newPrivacyServer(t, dispatchAt(stub, nil))

	for _, row := range []struct{ name, token string }{
		{"the hub's listener", g.accountToken(t, privacyListenerAccount, "", time.Hour)},
		{"Dispatch's listener", g.accountToken(t, dispatchListenerAccount, "", time.Hour)},
		{"a listener on a VM", g.live(t, listenerGoogleAccount)},
		{"the privacy measurement", g.accountToken(t, privacyMeasureAccount, "", time.Hour)},
	} {
		got := g.privacy(t, row.token)
		if got.status != http.StatusOK {
			t.Errorf("%s: GET %s = %d %s, want 200", row.name, privacyPath, got.status, got.body)
			continue
		}
		if !sameJSON(t, got.body, body) {
			t.Errorf("%s was answered %s, want the stub's body unchanged, %s", row.name, got.body, body)
		}
	}
	asked := stub.calls.Load()
	if asked != 4 {
		t.Errorf("four readers' calls asked Dispatch %d times, want 4", asked)
	}

	for _, row := range []struct {
		name, token string
		status      int
		code        string
	}{
		{"Dispatch's app account", g.uiToken, http.StatusForbidden, "not_listener"},
		{"a session token", g.podToken(t, workerAccount, uuid.NewString()), http.StatusForbidden, "not_listener"},
		{"a daemon's identity", g.accountToken(t, daemonAccount, "", time.Hour), http.StatusForbidden, "not_listener"},
		{"the operator's identity", g.live(t, testApprover), http.StatusForbidden, "not_listener"},
		{"the measurement job", g.accountToken(t, measureAccount, "", time.Hour), http.StatusForbidden, "not_listener"},
		{"a listener held to its exchange projects", g.accountToken(t, scopedListenerAccount, "", time.Hour), http.StatusForbidden, "not_listener"},
		{"an account listed nowhere", g.accountToken(t, unlistedAccount, "", time.Hour), http.StatusForbidden, "not_listener"},
		// A listener's account with a token that does not verify is refused as on every route
		// that takes an identity token, before its account is looked up.
		{"the hub's listener's expired token", g.accountToken(t, privacyListenerAccount, "", -time.Minute), http.StatusUnauthorized, "ACCOUNT_TOKEN_EXPIRED"},
		{"no token", "", http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID"},
	} {
		got := g.privacy(t, row.token)
		if got.status != row.status || got.code != row.code {
			t.Errorf("%s: GET %s = %d %s, want %d %s", row.name, privacyPath, got.status, got.body, row.status, row.code)
		}
	}
	if after := stub.calls.Load(); after != asked {
		t.Errorf("the refused callers asked Dispatch %d times, want none", after-asked)
	}
}

// TestTheMeasurementIsCountedApartFromListeners: a listener's read of the route is counted in
// broker_answers_total under caller_class="listener", and the privacy measurement's under
// caller_class="privacy-measure", so a measurement run's reads stay out of the listeners' series.
func TestTheMeasurementIsCountedApartFromListeners(t *testing.T) {
	stub := newPrivacyStub(t, answerWith(http.StatusOK, `{"projects":[]}`))
	g := newPrivacyServer(t, dispatchAt(stub, nil))
	const listened, measured = `outcome="served",code="HTTP_200",caller_class="listener"`, `outcome="served",code="HTTP_200",caller_class="privacy-measure"`
	listenersBefore, measurementsBefore := g.answersCounted(t, listened), g.answersCounted(t, measured)
	if got := g.privacy(t, g.accountToken(t, privacyListenerAccount, "", time.Hour)); got.status != http.StatusOK {
		t.Fatalf("the listener's call = %d %s, want 200", got.status, got.body)
	}
	if got := g.privacy(t, g.accountToken(t, privacyMeasureAccount, "", time.Hour)); got.status != http.StatusOK {
		t.Fatalf("the measurement's call = %d %s, want 200", got.status, got.body)
	}
	if listeners, measurements := g.answersCounted(t, listened)-listenersBefore, g.answersCounted(t, measured)-measurementsBefore; listeners != 1 || measurements != 1 {
		t.Fatalf("a listener's call and the measurement's were counted %d under caller_class=listener and %d under caller_class=privacy-measure, want 1 and 1", listeners, measurements)
	}
}

// TestOnlyTheReadersNatscalloutNamesReadProjectPrivacy: for every entry of the rig's
// BROKER_NATS_SERVICES, every kind the auth service admits and a listener of either scope, the
// route answers 200 exactly when natscallout decides the entry reads project privacy (its kind's
// ReadsProjectPrivacy, with the whole grant), and 403 not_listener otherwise. The route keeps no
// list of kinds, so it answers a kind natscallout does not name, or refuses one it names, only by
// failing here.
func TestOnlyTheReadersNatscalloutNamesReadProjectPrivacy(t *testing.T) {
	services, err := natscallout.ParseServices(privacyServices)
	if err != nil {
		t.Fatalf("parse BROKER_NATS_SERVICES: %v", err)
	}
	stub := newPrivacyStub(t, answerWith(http.StatusOK, `{"projects":[]}`))
	g := newPrivacyServer(t, dispatchAt(stub, nil))

	seen := map[natscallout.Kind]bool{}
	readers, others := 0, 0
	for _, entry := range services {
		seen[entry.Kind] = true
		token := g.accountToken(t, entry.Account, "", time.Hour)
		if entry.Google {
			token = g.live(t, entry.Account)
		}
		scope := "-"
		if entry.AllProjects {
			scope = "*"
		}
		got := g.privacy(t, token)
		if entry.AllProjects && entry.Kind.ReadsProjectPrivacy() {
			readers++
			if got.status != http.StatusOK {
				t.Errorf("a %s entry with scope %s (%s): GET %s = %d %s, want 200: natscallout says it reads project privacy", entry.Kind, scope, entry.Account, privacyPath, got.status, got.body)
			}
			continue
		}
		others++
		if got.status != http.StatusForbidden || got.code != "not_listener" {
			t.Errorf("a %s entry with scope %s (%s): GET %s = %d %s, want 403 not_listener: natscallout says it does not read project privacy", entry.Kind, scope, entry.Account, privacyPath, got.status, got.body)
		}
	}
	for _, kind := range []natscallout.Kind{natscallout.KindDispatch, natscallout.KindListener, natscallout.KindDaemon, natscallout.KindMeasure, natscallout.KindPrivacyMeasure} {
		if !seen[kind] {
			t.Errorf("the rig's BROKER_NATS_SERVICES names no %s entry, so this test does not see that kind", kind)
		}
	}
	if readers == 0 || others == 0 {
		t.Fatalf("this test walked %d readers and %d others, want some of each", readers, others)
	}
}

// TestProjectPrivacyIsBounded: with 64 calls held open at the read, the 65th, from a listener
// holding none of them, is answered 503 busy at once, while all 64 are still held, and makes no
// read; once one held call returns, the next call is served.
func TestProjectPrivacyIsBounded(t *testing.T) {
	reads := newHeldReads()
	g := newPrivacyServer(t, reads)
	releaseAll := reads.releaseAllWhenDone(t)

	held := make(chan privacyAnswer, 64)
	for _, account := range []string{privacyListenerAccount, dispatchListenerAccount, secondListenerAccount, thirdListenerAccount} {
		g.hold(t, reads, held, g.accountToken(t, account, "", time.Hour), 16)
	}
	vm := g.live(t, listenerGoogleAccount)
	next := make(chan privacyAnswer, 1)
	g.privacyInto(next, vm)
	got, answered := within(next, 30*time.Second)
	if returned := len(held); returned != 0 {
		t.Fatalf("%d held calls had returned when the 65th call was answered, want none: the held reads ended", returned)
	}
	if n := reads.calls.Load(); n != 64 {
		t.Fatalf("with 64 calls held open, the read was made %d times, want 64: the 65th call reached it", n)
	}
	if !answered || got.status != http.StatusServiceUnavailable || got.code != "busy" {
		t.Fatalf("the 65th call = %d %s (answered: %v), want 503 busy at once", got.status, got.body, answered)
	}

	reads.holding.Store(false)
	select {
	case reads.release <- struct{}{}:
	case <-time.After(30 * time.Second):
		t.Fatal("no held call was left at the read to release")
	}
	if returned, ok := within(held, time.Minute); !ok || returned.status != http.StatusOK {
		t.Fatalf("the released call = %d %s (returned: %v), want 200", returned.status, returned.body, ok)
	}
	if served := g.privacy(t, vm); served.status != http.StatusOK || !sameJSON(t, served.body, `{"projects":[]}`) {
		t.Fatalf("once one held call returned, the next call = %d %s, want 200 {\"projects\":[]}", served.status, served.body)
	}
	if n := reads.calls.Load(); n != 65 {
		t.Fatalf("the read was made %d times, want 65: the 64 held and the one served after", n)
	}

	releaseAll.Do(func() { close(reads.release) })
	for range 63 {
		if returned := <-held; returned.status != http.StatusOK {
			t.Errorf("a held call = %d %s once released, want 200", returned.status, returned.body)
		}
	}
}

// TestOneListenerHoldsAtMostItsShare: with 16 calls of one listener's account held open, its 17th
// is answered 503 busy at once and makes no read, while another listener's call is served.
func TestOneListenerHoldsAtMostItsShare(t *testing.T) {
	reads := newHeldReads()
	g := newPrivacyServer(t, reads)
	releaseAll := reads.releaseAllWhenDone(t)
	hub := g.accountToken(t, privacyListenerAccount, "", time.Hour)

	held := make(chan privacyAnswer, 16)
	g.hold(t, reads, held, hub, 16)
	next := make(chan privacyAnswer, 1)
	g.privacyInto(next, hub)
	got, answered := within(next, 30*time.Second)
	if returned := len(held); returned != 0 {
		t.Fatalf("%d held calls had returned when the 17th was answered, want none: the held reads ended", returned)
	}
	if n := reads.calls.Load(); n != 16 {
		t.Fatalf("with 16 of one account's calls held open, the read was made %d times, want 16: its 17th call reached it", n)
	}
	if !answered || got.status != http.StatusServiceUnavailable || got.code != "busy" {
		t.Fatalf("the account's 17th call = %d %s (answered: %v), want 503 busy at once", got.status, got.body, answered)
	}

	reads.holding.Store(false)
	if served := g.privacy(t, g.accountToken(t, secondListenerAccount, "", time.Hour)); served.status != http.StatusOK {
		t.Fatalf("another listener's call = %d %s, want 200: one account's 16 leave the rest their slots", served.status, served.body)
	}

	releaseAll.Do(func() { close(reads.release) })
	for range 16 {
		if returned := <-held; returned.status != http.StatusOK {
			t.Errorf("a held call = %d %s once released, want 200", returned.status, returned.body)
		}
	}
}

// TestProjectPrivacyIsAFreshRead: 50 calls against a stub whose answer changes between calls each
// return the stub's answer at that call, and the stub counts 50 requests. Dispatch lists each
// answer's two projects out of key order, and the route answers them in key order.
func TestProjectPrivacyIsAFreshRead(t *testing.T) {
	var at atomic.Int64
	entry := func(i int64, suffix string) string {
		return fmt.Sprintf(`{"project":"P%02d-%s","slack_channels":["C%02d%s"],"slack_channels_changed_at":"2026-11-20T00:00:%02dZ"}`, i, suffix, i, suffix, i)
	}
	stub := newPrivacyStub(t, func(w http.ResponseWriter, r *http.Request) {
		i := at.Load()
		answerWith(http.StatusOK, `{"projects":[`+entry(i, "B")+`,`+entry(i, "A")+`]}`)(w, r)
	})
	g := newPrivacyServer(t, dispatchAt(stub, nil))
	token := g.accountToken(t, privacyListenerAccount, "", time.Hour)

	for i := range int64(50) {
		at.Store(i)
		want := `{"projects":[` + entry(i, "A") + `,` + entry(i, "B") + `]}`
		got := g.privacy(t, token)
		if got.status != http.StatusOK {
			t.Fatalf("call %d = %d %s, want 200", i, got.status, got.body)
		}
		if !sameJSON(t, got.body, want) {
			t.Fatalf("call %d was answered %s, want the stub's answer at that call in key order, %s", i, got.body, want)
		}
	}
	if n := stub.calls.Load(); n != 50 {
		t.Fatalf("the stub counted %d requests for 50 calls, want 50", n)
	}
}

// TestProjectPrivacyFailsClosed: with Dispatch answering 500, timing out, or answering a project
// without slack_channels, and on a broker with no project-access URL, a listener is answered 502
// dispatch_unavailable, never 200 and never a list. Each failure is a read of the stub of its own,
// and the next call once Dispatch answers again is served: the route keeps no failure, as it keeps
// no answer.
func TestProjectPrivacyFailsClosed(t *testing.T) {
	healthy := answerWith(http.StatusOK, `{"projects":[]}`)
	failsClosed := func(t *testing.T, name string, got privacyAnswer) {
		t.Helper()
		if got.status != http.StatusBadGateway || got.code != "dispatch_unavailable" || strings.Contains(string(got.body), `"projects"`) {
			t.Errorf("%s: GET %s = %d %s, want 502 dispatch_unavailable", name, privacyPath, got.status, got.body)
		}
	}
	fails := func(t *testing.T, g *googleCallers, stub *privacyStub, name string, answer http.HandlerFunc) {
		t.Helper()
		token := g.accountToken(t, privacyListenerAccount, "", time.Hour)
		stub.set(answer)
		before := stub.calls.Load()
		failsClosed(t, name, g.privacy(t, token))
		if stub.calls.Load() == before {
			t.Errorf("%s: the call asked Dispatch nothing", name)
		}
		stub.set(healthy)
		if got := g.privacy(t, token); got.status != http.StatusOK {
			t.Errorf("after %s, with Dispatch answering again, GET %s = %d %s, want 200", name, privacyPath, got.status, got.body)
		}
	}

	stub := newPrivacyStub(t, healthy)
	g := newPrivacyServer(t, dispatchAt(stub, nil))
	fails(t, g, stub, "Dispatch answers 500", answerWith(http.StatusInternalServerError, `{"code":"INTERNAL","error":"internal error"}`))
	fails(t, g, stub, "Dispatch answers a project without slack_channels", answerWith(http.StatusOK, `{"projects":[{"project":"ALPHA","slack_channels_changed_at":null}]}`))

	// The broker's client gives up on Dispatch after a second here rather than its own ten, on a
	// rig of its own, so that no other row can time out instead of failing as it says.
	slowStub := newPrivacyStub(t, healthy)
	slow := newPrivacyServer(t, dispatchAt(slowStub, &http.Client{Timeout: time.Second}))
	fails(t, slow, slowStub, "Dispatch times out", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

	unset := newPrivacyServer(t, nil)
	failsClosed(t, "a broker with no project-access URL", unset.privacy(t, unset.accountToken(t, privacyListenerAccount, "", time.Hour)))
}

// TestProjectPrivacySpendsTheAddressBucket: the route spends its caller's source address's bucket
// before the token is verified, as every route that takes an identity token does, so a listener
// past it is answered 429 RATE_LIMITED and Dispatch is not asked.
func TestProjectPrivacySpendsTheAddressBucket(t *testing.T) {
	stub := newPrivacyStub(t, answerWith(http.StatusOK, `{"projects":[]}`))
	g := newPrivacyServer(t, dispatchAt(stub, nil), func(d *api.Deps) {
		d.AccountLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1}
	})
	token := g.accountToken(t, privacyListenerAccount, "", time.Hour)
	if got := g.privacy(t, token); got.status != http.StatusOK {
		t.Fatalf("the first call = %d %s, want 200", got.status, got.body)
	}
	if got := g.privacy(t, token); got.status != http.StatusTooManyRequests || got.code != "RATE_LIMITED" {
		t.Fatalf("the second call past a bucket of one = %d %s, want 429 RATE_LIMITED", got.status, got.body)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("the stub counted %d requests, want 1: the refused call asked Dispatch", n)
	}
}
