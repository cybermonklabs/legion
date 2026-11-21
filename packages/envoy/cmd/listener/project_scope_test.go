package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The privacy source the required mode reads (GET /v1/project-privacy), as plan E's t-e-63 broker
// route answers it: Dispatch's committed rows unchanged, {"projects":[]} when none is private,
// 502 dispatch_unavailable when Dispatch does not answer, 403 not_listener for an identity it does
// not bind, 503 busy past 64 calls in flight, 429 RATE_LIMITED past the per-address bucket.
const (
	privacyNone        = `{"projects":[]}`
	privacyListingPRIV = `{"projects":[{"project":"PRIV","slack_channels":["C0PRIV00001"],"slack_channels_changed_at":"2026-11-18T00:00:00Z"}]}`
)

// privacySource stands in for the broker's GET /v1/project-privacy: it answers body, or status
// while one is set, or holds every request until released while held; it counts the requests it
// was sent and keeps the bearer each presented.
type privacySource struct {
	server *httptest.Server
	mu     sync.Mutex
	body   string
	status int
	// held, when not nil, holds each request until it is closed; arrived is told of each request
	// as it arrives.
	held    chan struct{}
	arrived chan struct{}
	bearers []string
	count   atomic.Uint64
}

func newPrivacySource(t *testing.T, body string) *privacySource {
	t.Helper()
	source := &privacySource{body: body, arrived: make(chan struct{}, 1024)}
	source.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != k7Path || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		source.count.Add(1)
		source.mu.Lock()
		source.bearers = append(source.bearers, r.Header.Get("Authorization"))
		held, status, answer := source.held, source.status, source.body
		source.mu.Unlock()
		source.arrived <- struct{}{}
		// A held read answers what the source held when the read arrived, as a read of Dispatch's
		// rows answers the rows committed when it was made.
		if held != nil {
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"stand-in","code":"stand_in"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(source.server.Close)
	return source
}

// answer makes every later read answer body.
func (s *privacySource) answer(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.status = body, 0
}

// failWith makes every later read answer status.
func (s *privacySource) failWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// hold holds every later read until release is called; each held read then answers what the
// source held when it arrived.
func (s *privacySource) hold() (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := make(chan struct{})
	s.held = held
	return func() {
		s.mu.Lock()
		if s.held == held {
			s.held = nil
		}
		s.mu.Unlock()
		close(held)
	}
}

// reads is how many reads the source was sent.
func (s *privacySource) reads() uint64 { return s.count.Load() }

// awaitArrival waits for the next read to reach the source.
func (s *privacySource) awaitArrival(t *testing.T) {
	t.Helper()
	select {
	case <-s.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no privacy read reached the source within 10 s")
	}
}

// testK7Reader is K7's reader of source with the timeout given.
func testK7Reader(source string, timeout time.Duration) *k7Reader {
	reader := newK7Reader(source, listenerBrokerAccount(edgeAccountToken), logging.New("test-machine"))
	reader.timeout = timeout
	return reader
}

// TestK7ReaderAnswersUnknownOnAnyError: a read answers a set only for a whole answer - a 200 whose
// projects is an array, each project a key with a slack_channels array, slack_channels_changed_at
// null or a time - and every other outcome is unknown for every project, naming why: a refused
// connection, a timeout, each non-2xx answer the broker gives (403, 429, 500, 502, 503), a body
// that does not parse, projects null, missing or not an array, a project without slack_channels,
// a key that is no project key, and a key named twice. A read presents the listener's own
// platform identity as its bearer.
func TestK7ReaderAnswersUnknownOnAnyError(t *testing.T) {
	source := newPrivacySource(t, privacyNone)
	reader := testK7Reader(source.server.URL, k7ReadTimeout)
	for _, whole := range []struct {
		name, body string
		private    bool
	}{
		{"no private project", privacyNone, false},
		{"PRIV private", privacyListingPRIV, true},
		{"PRIV private, its channels never changed", `{"projects":[{"project":"PRIV","slack_channels":[],"slack_channels_changed_at":null}]}`, true},
	} {
		source.answer(whole.body)
		set, err := reader.Read(t.Context())
		if err != nil || set.private("PRIV") != whole.private || set.private("PUB") {
			t.Errorf("%s: the read answered PRIV private %v, PUB private %v, %v; want PRIV private %v, PUB public, no error",
				whole.name, set.private("PRIV"), set.private("PUB"), err, whole.private)
		}
	}
	if bearer := source.bearers[0]; bearer != "Bearer "+edgeAccountToken {
		t.Errorf("the read presented %q, want the listener's own identity", bearer)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for _, unknown := range []struct {
		name, reason string
		read         func() (privateSet, error)
	}{
		{"a refused connection", unknownTransport, func() (privateSet, error) { return testK7Reader(closedURL, time.Second).Read(t.Context()) }},
		{"a timeout", unknownTimeout, func() (privateSet, error) {
			release := source.hold()
			defer release()
			return testK7Reader(source.server.URL, 200*time.Millisecond).Read(t.Context())
		}},
		{"403 not_listener", unknownRefused, statusRead(t, source, reader, http.StatusForbidden)},
		{"429 RATE_LIMITED", unknownRateLimited, statusRead(t, source, reader, http.StatusTooManyRequests)},
		{"500", unknownStatus, statusRead(t, source, reader, http.StatusInternalServerError)},
		{"502 dispatch_unavailable", unknownUnavailable, statusRead(t, source, reader, http.StatusBadGateway)},
		{"503 busy", unknownBusy, statusRead(t, source, reader, http.StatusServiceUnavailable)},
		{"a body that does not parse", unknownUndecodable, bodyRead(source, reader, `not json`)},
		{"projects null", unknownIncomplete, bodyRead(source, reader, `{"projects":null}`)},
		{"projects missing", unknownIncomplete, bodyRead(source, reader, `{}`)},
		{"projects not an array", unknownUndecodable, bodyRead(source, reader, `{"projects":{}}`)},
		{"a project without slack_channels", unknownIncomplete, bodyRead(source, reader, `{"projects":[{"project":"PRIV"}]}`)},
		{"slack_channels null", unknownIncomplete, bodyRead(source, reader, `{"projects":[{"project":"PRIV","slack_channels":null}]}`)},
		{"a key that is no project key", unknownIncomplete, bodyRead(source, reader, `{"projects":[{"project":"priv","slack_channels":[]}]}`)},
		{"a key named twice", unknownIncomplete, bodyRead(source, reader, `{"projects":[{"project":"PRIV","slack_channels":[]},{"project":"PRIV","slack_channels":[]}]}`)},
	} {
		t.Run(unknown.name, func(t *testing.T) {
			set, err := unknown.read()
			if !errors.Is(err, errPrivacyUnknown) || unknownReasonOf(err) != unknown.reason || !set.none() {
				t.Fatalf("%s: the read answered %v with %v (reason %q); want unknown for every project, reason %q",
					unknown.name, set.channels, err, unknownReasonOf(err), unknown.reason)
			}
		})
	}
}

// statusRead is a read of reader while source answers status.
func statusRead(t *testing.T, source *privacySource, reader *k7Reader, status int) func() (privateSet, error) {
	return func() (privateSet, error) {
		source.failWith(status)
		defer source.answer(privacyNone)
		return reader.Read(t.Context())
	}
}

// bodyRead is a read of reader while source answers body with 200.
func bodyRead(source *privacySource, reader *k7Reader, body string) func() (privateSet, error) {
	return func() (privateSet, error) {
		source.answer(body)
		defer source.answer(privacyNone)
		return reader.Read(context.Background())
	}
}

// TestAReadServesOnlyEarlierFrames: a decision never joins a read whose request was sent before
// it began. With one read held at the source, a switch of PRIV to private lands, and then a second
// decision begins: the held read answers the first decision what Dispatch held when it was sent,
// and the second decision gets a read of its own, sent after it began, which answers PRIV private.
// Decisions that begin together while no read is in flight share one.
func TestAReadServesOnlyEarlierFrames(t *testing.T) {
	source := newPrivacySource(t, privacyNone)
	reader := testK7Reader(source.server.URL, 10*time.Second)
	release := source.hold()
	first := make(chan k7Result, 1)
	go func() {
		set, err := reader.Read(context.Background())
		first <- k7Result{set: set, err: err}
	}()
	source.awaitArrival(t)
	source.answer(privacyListingPRIV)
	second := make(chan k7Result, 1)
	go func() {
		set, err := reader.Read(context.Background())
		second <- k7Result{set: set, err: err}
	}()
	// The second decision is queued behind the read in flight, never joined to it.
	testwait.Eventually(t, "the second decision to wait for a read of its own", func() bool {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		return len(reader.waiting) == 1
	})
	release()
	early, late := <-first, <-second
	if early.err != nil || early.set.private("PRIV") {
		t.Fatalf("the first decision read PRIV private %v, %v; want what its read was sent before: public", early.set.private("PRIV"), early.err)
	}
	if late.err != nil || !late.set.private("PRIV") {
		t.Fatalf("the decision that began after the first read was sent read PRIV private %v, %v; want the switch, from a read of its own", late.set.private("PRIV"), late.err)
	}
	if reads := source.reads(); reads != 2 {
		t.Fatalf("the source was read %d times, want 2: one per read, the second sent after the second decision began", reads)
	}
}

// TestTheReadinessRouteReadsAfresh: GET /v1/privacy-readiness answers only an
// ENVOY_ALL_PROJECTS_ACCOUNTS caller, and every call makes its own read: 20 calls make 20 reads,
// counted at the source. A source answering 500, one timing out, one answering a project without
// slack_channels and one answering {"projects": null} each give 503 with read failed. Any other
// caller gets 403. No answer carries a project key. An all-public listener answers required
// false, source "" and read none, and reads nothing.
func TestTheReadinessRouteReadsAfresh(t *testing.T) {
	const dispatchAccount = "system:serviceaccount:dispatch:dispatch"
	source := newPrivacySource(t, privacyListingPRIV)
	privacy := &listenerPrivacy{required: true, k7: testK7Reader(source.server.URL, 300*time.Millisecond), allProjects: []string{dispatchAccount}}
	handler := privacyReadinessHandler(privacy)
	call := func(subject string) (int, privacyReadiness, string) {
		request := httptest.NewRequest(http.MethodGet, "/v1/privacy-readiness", nil)
		if subject != "" {
			request = request.WithContext(withVerifiedPrincipal(request.Context(), subject))
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var answer privacyReadiness
		_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
		return recorder.Code, answer, recorder.Body.String()
	}
	for i := range 20 {
		status, answer, body := call(dispatchAccount)
		if status != http.StatusOK || !answer.Required || answer.Read != readinessAnswered || answer.Source != source.server.URL || answer.Instance == "" {
			t.Fatalf("call %d: status %d, %s; want 200 required, read answered, source %s", i+1, status, body, source.server.URL)
		}
		if strings.Contains(body, "PRIV") || strings.Contains(body, "C0PRIV00001") {
			t.Fatalf("call %d: the answer %s carries a project or a channel", i+1, body)
		}
	}
	if reads := source.reads(); reads != 20 {
		t.Fatalf("20 calls made %d reads at the source, want 20: each call reads afresh", reads)
	}
	for _, failed := range []struct {
		name string
		set  func() (restore func())
	}{
		{"500", func() func() {
			source.failWith(http.StatusInternalServerError)
			return func() { source.answer(privacyListingPRIV) }
		}},
		{"a timeout", func() func() { return source.hold() }},
		{"a project without slack_channels", func() func() {
			source.answer(`{"projects":[{"project":"PRIV"}]}`)
			return func() { source.answer(privacyListingPRIV) }
		}},
		{"projects null", func() func() {
			source.answer(`{"projects": null}`)
			return func() { source.answer(privacyListingPRIV) }
		}},
	} {
		restore := failed.set()
		status, answer, body := call(dispatchAccount)
		restore()
		if status != http.StatusServiceUnavailable || answer.Read != readinessFailed || !answer.Required {
			t.Fatalf("%s: status %d, %s; want 503 with read failed", failed.name, status, body)
		}
	}
	for _, subject := range []string{"", "system:serviceaccount:legion:legion-daemon"} {
		if status, _, body := call(subject); status != http.StatusForbidden {
			t.Fatalf("a caller %q: status %d, %s; want 403", subject, status, body)
		}
	}
	public := privacyReadinessHandler(&listenerPrivacy{allProjects: []string{dispatchAccount}})
	request := httptest.NewRequest(http.MethodGet, "/v1/privacy-readiness", nil)
	request = request.WithContext(withVerifiedPrincipal(request.Context(), dispatchAccount))
	recorder := httptest.NewRecorder()
	public.ServeHTTP(recorder, request)
	var answer privacyReadiness
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || answer.Required || answer.Source != "" || answer.Read != readinessNone {
		t.Fatalf("the all-public listener answered %d %s, want 200 required false, source \"\", read none", recorder.Code, recorder.Body.String())
	}
}

// TestAnOldTokenGetsOnlyItsList: in the required mode with PUB public, a token whose broker answer
// carries no restricted claim (one a broker minted before the claim existed) reads as a restricted
// member's: a session registered under it, listing PRIV, is recorded restricted, receives its PRIV
// frame and no PUB frame, and is refused a PUB topic 403 project_not_listed; the same list under a
// token whose answer says "restricted": false receives PUB's frame. A session record without the
// restricted key (an earlier build's shape, still naming its key) reads the same way: its
// session receives no PUB frame. Control (a mutant, run apart): an absent key read as not
// restricted gives the old token's session, and the keyless record's, PUB's frame, and this test
// fails naming each.
func TestAnOldTokenGetsOnlyItsList(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(privacyListingPRIV)
	const old, full, keyless = "ses_old_token", "ses_full_token", "ses_keyless_record"
	oldStandIn := startSessionStandIn(t, rig.url, old, answerDone)
	oldToken := rig.personToken(old, "PRIV")
	requireStatus(t, rig.subscribe(t, oldToken, old, everyIssueTopic), http.StatusOK, "the old token's registration")
	if entry, err := rig.deps.sessions.Get(old); err != nil || entry.Restricted == nil || !*entry.Restricted {
		t.Errorf("the old token's session is recorded %+v, %v; want restricted true", entry, err)
	}
	fullStandIn := rig.session(t, full, false, []string{"PRIV"}, everyIssueTopic)
	keylessStandIn := rig.session(t, keyless, false, []string{"PRIV"}, everyIssueTopic)
	stored, err := rig.deps.sessions.Get(keyless)
	if err != nil {
		t.Fatalf("read the keyless session's record: %v", err)
	}
	stored.Restricted = nil
	if err := rig.deps.sessions.Put(keyless, stored); err != nil {
		t.Fatalf("rewrite the record without its restricted key: %v", err)
	}

	priv, _ := rig.storeEvent(t, privIssueSubject)
	pub, _ := rig.storeEvent(t, pubIssueSubject)
	expectOnly(t, oldStandIn, "the old token's session, listing PRIV", priv)
	expectOnly(t, keylessStandIn, "the session whose record has no restricted key", priv)
	expectOnly(t, fullStandIn, "the same list under \"restricted\": false", priv, pub)

	answer := rig.subscribe(t, oldToken, old, everyIssueTopic, "notifications.dispatch.issue.PUB.>")
	if answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), projectNotListed) {
		t.Fatalf("the old token's PUB topic answered %d %s, want 403 %s", answer.Code, answer.Body.String(), projectNotListed)
	}
}
