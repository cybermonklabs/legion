//go:build natslive

package main

// TestAnOutageHoldsAndRecovers is the privacy read's outage end to end, on a listener in the
// required mode. The listener reads the privacy answer from a broker tier, which reads a Dispatch
// tier. Each of four outages stops or breaks one tier while frames flow.
//
// While a read cannot answer, every project is unknown and counts as private. Then:
//
//   - a project's frame is held for a session whose token lists no project, and dropped for it at
//     the hold's limit; the session's next frame carries Envoy-Gap: true with
//     Envoy-Gap-Reason: privacy_hold;
//   - the frame never reaches a session whose token lists other projects;
//   - the Slack intake answers 503 and publishes nothing;
//   - a route whose decision needs the answer answers the 503 refilling.
//
// Once the tier answers again, the first decision reads the new answer, with at most one read in
// flight. The test runs every outage twice: once while PRIV is private, and once while no project
// is private, since the outage holds a public project's frames too (R9's outage exception).
//
// The two tiers are stand-ins until their routes are in this branch's base. Each is replaced by
// the real program on its test database:
//
//   - the Dispatch tier answers GET /api/v1/access/private-projects as Dispatch's
//     api.Server.getAccessPrivateProjects does; Dispatch's own server on
//     DISPATCH_TEST_DATABASE_URL replaces it;
//   - the broker tier answers GET /v1/project-privacy as the broker's project-privacy route does:
//     Dispatch's body unchanged, 502 dispatch_unavailable when Dispatch does not answer, and 403
//     not_listener for an identity that is not the listener's; the broker binary on
//     BROKER_TEST_DATABASE_URL replaces it.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// The hold's limits this test runs under (holdLimits): a held lane is read again every 2 s and
	// a frame still undecided at 6 s is dropped, so eight outages with their recoveries run in
	// about a minute and a half.
	outageRedecide  = 2 * time.Second
	outageUndecided = 6 * time.Second
	// outageDropWithin bounds the wait for a held frame's drop: the limit, a pass of the hold, a
	// read's timeout on either side of it, and the box's load.
	outageDropWithin = 30 * time.Second
	// outageDropLate is how long after its limit a held frame may still be dropped: the relay's
	// loop takes the drop up between pulls, and a read it makes inline ends at the oldest held
	// frame's limit at the latest (readBeforeDrop).
	outageDropLate = 1500 * time.Millisecond
	// outageArrival bounds the wait for a frame the relay sends at once.
	outageArrival = 10 * time.Second
	// outageTeam and outageChannel are the Slack event's: a channel no private project lists, so
	// while the answer is known the event is published on notifications.slack.>.
	outageTeam    = "T0OUTAGE01"
	outageChannel = "C0OUTAGE01"
	// The sessions: E's token lists no project, V's lists PRIV, and W's lists INFRA.
	outageE = "ses_outage_e"
	outageV = "ses_outage_v"
	outageW = "ses_outage_w"
)

// outageTier is one tier of the read's chain, on an address that outlives its server. It can stop
// listening (every connection refused), answer a status in place of its answer, or answer after a
// delay, and start again on the same address. It counts its requests and the most it had in
// flight at once.
type outageTier struct {
	t       *testing.T
	addr    string
	handler http.Handler

	mu     sync.Mutex
	server *http.Server
	status int
	delay  time.Duration

	inflight    atomic.Int64
	inflightMax atomic.Int64
	requests    atomic.Int64
}

func startOutageTier(t *testing.T, answer http.HandlerFunc) *outageTier {
	t.Helper()
	tier := &outageTier{t: t}
	tier.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := tier.inflight.Add(1)
		defer tier.inflight.Add(-1)
		for seen := tier.inflightMax.Load(); now > seen && !tier.inflightMax.CompareAndSwap(seen, now); seen = tier.inflightMax.Load() {
		}
		tier.requests.Add(1)
		tier.mu.Lock()
		status, delay := tier.status, tier.delay
		tier.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": fmt.Sprintf("the stand-in answers %d", status)})
			return
		}
		answer(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for a tier of the privacy read: %v", err)
	}
	tier.addr = listener.Addr().String()
	tier.serve(listener)
	t.Cleanup(tier.stop)
	return tier
}

func (p *outageTier) url() string { return "http://" + p.addr }

func (p *outageTier) serve(listener net.Listener) {
	server := &http.Server{Handler: p.handler, ReadHeaderTimeout: 5 * time.Second}
	p.mu.Lock()
	p.server = server
	p.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
}

// stop closes the tier's listener and every connection it holds.
func (p *outageTier) stop() {
	p.mu.Lock()
	server := p.server
	p.server = nil
	p.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

// start listens on the tier's address again.
func (p *outageTier) start() {
	p.t.Helper()
	listener, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatalf("listen again on %s: %v", p.addr, err)
	}
	p.serve(listener)
}

// answerStatus makes every later request answer status, 0 for the answer again.
func (p *outageTier) answerStatus(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

// answerAfter makes every later request wait delay before it answers, 0 for at once.
func (p *outageTier) answerAfter(delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delay = delay
}

// outageChain is the read's chain: the Dispatch tier, whose body the test sets, and the broker
// tier in front of it, which the listener reads.
type outageChain struct {
	dispatch *outageTier
	broker   *outageTier
	answer   atomic.Value // string
}

func newOutageChain(t *testing.T) *outageChain {
	t.Helper()
	chain := &outageChain{}
	chain.answer.Store(privacyNone)
	chain.dispatch = startOutageTier(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/access/private-projects" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chain.answer.Load().(string))
	})
	client := &http.Client{Timeout: 30 * time.Second}
	chain.broker = startOutageTier(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/project-privacy" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+edgeAccountToken {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_listener"})
			return
		}
		body, err := readTier(r, client, chain.dispatch.url()+"/api/v1/access/private-projects", "")
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "dispatch_unavailable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	return chain
}

// readTier is one GET of url within r's context, with bearer when it is set: the body of a 200,
// else an error.
func readTier(r *http.Request, client *http.Client, url, bearer string) ([]byte, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		request.Header.Set("Authorization", bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", response.StatusCode, body)
	}
	return body, nil
}

// outageCase is one way the read's chain fails, and its end. firstRead is how long the read that
// first finds the project unknown takes under it: the frame is held from that read's end, so its
// limit runs from then.
type outageCase struct {
	name      string
	start     func(*outageChain)
	end       func(*outageChain)
	firstRead time.Duration
}

func outageCases() []outageCase {
	return []outageCase{
		{"Dispatch stopped", func(c *outageChain) { c.dispatch.stop() }, func(c *outageChain) { c.dispatch.start() }, 0},
		{"the broker stopped", func(c *outageChain) { c.broker.stop() }, func(c *outageChain) { c.broker.start() }, 0},
		{"Dispatch answering 500",
			func(c *outageChain) { c.dispatch.answerStatus(http.StatusInternalServerError) },
			func(c *outageChain) { c.dispatch.answerStatus(0) }, 0},
		// Nothing is held when its frame arrives, so the first read waits out k7ReadTimeout.
		{"the broker answering after twice k7ReadTimeout",
			func(c *outageChain) { c.broker.answerAfter(2 * k7ReadTimeout) },
			func(c *outageChain) { c.broker.answerAfter(0) }, k7ReadTimeout},
	}
}

// outagePhase is what the Dispatch tier answers around the outages, and the project whose frame
// the outages hold for E.
type outagePhase struct {
	name   string
	answer string
	held   string
}

var (
	outagePrivate = outagePhase{name: "PRIV private", answer: privacyListingPRIV, held: "PRIV"}
	outagePublic  = outagePhase{name: "no project private", answer: privacyNone, held: "PUB"}
)

// outageGate is what the listener reads: the broker tier itself, or a control's faulty reader in
// front of it.
type outageGate int

const (
	// outageGateNone is the listener reading the broker tier.
	outageGateNone outageGate = iota
	// outageGateFailOpen takes a read that fails as no private project.
	outageGateFailOpen
	// outageGateKeepLast keeps the last whole answer through a read that fails.
	outageGateKeepLast
)

// source is the base URL the listener reads under gate: the broker tier's, or a gate's in front of
// it, which answers within a second, inside the listener's timeout, whatever the broker does.
func (c *outageChain) source(t *testing.T, gate outageGate) string {
	t.Helper()
	if gate == outageGateNone {
		return c.broker.url()
	}
	client := &http.Client{Timeout: time.Second}
	var last atomic.Value // []byte
	return startOutageTier(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := readTier(r, client, c.broker.url()+r.URL.Path, r.Header.Get("Authorization"))
		switch {
		case err == nil:
			last.Store(body)
		case gate == outageGateFailOpen:
			body = []byte(privacyNone)
		case last.Load() != nil:
			body = last.Load().([]byte)
		default:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "dispatch_unavailable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}).url()
}

// outageRig is a listener in the required privacy mode reading source: the routes behind the
// broker-token credential, the session relays over the in-process server, and the Slack intake,
// every piece real but the broker's introspection, which a stub answers.
type outageRig struct {
	*relayRig
	handler http.Handler
	slack   http.Handler
	broker  *brokertokentest.StubBroker
	caller  *brokertokentest.Caller
	met     *metrics.Registry
	privacy *listenerPrivacy
}

func newOutageRig(t *testing.T, source string) *outageRig {
	t.Helper()
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	met := metrics.New()
	logger := rig.harness.config.logger
	counters := newPrivacyCounters(met)
	privacy := &listenerPrivacy{
		required: true,
		k7:       newK7Reader(source, listenerBrokerAccount(edgeAccountToken), counters.unknown, logger),
		counters: counters,
		logger:   logger,
	}
	// The routes read the mode when they are registered, and the relay before it relays
	// anything, as main sets both.
	rig.deps.privacy = privacy
	rig.relays.setPrivacy(privacy)
	routes := http.NewServeMux()
	registerV1Routes(routes, rig.deps, "test-machine", logger)
	rig.relays.setOwned(true)
	broker := brokertokentest.NewStubBroker(t)
	return &outageRig{
		relayRig: rig,
		handler:  apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, broker), logger: logger}, routes),
		slack:    webhook.SlackHandler(webhookSecret, rig.harness.client, webhook.WithSlackPrivacy(privacy)),
		broker:   broker,
		caller:   brokertokentest.NewCaller(t),
		met:      met,
		privacy:  privacy,
	}
}

// token is session's broker token, acting for a person and listing projects.
func (r *outageRig) token(session string, projects ...string) string {
	token := "access-token-of-" + session
	answer := sessionAnswer(session, "outage@example.com", r.caller.JKT())
	answer.Projects = slices.Sorted(slices.Values(projects))
	r.broker.Says(token, answer)
	return token
}

// call sends one request through the credential and the route.
func (r *outageRig) call(t *testing.T, method, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, brokerTokenRequest(t, r.caller, method, path, token, body))
	return recorder
}

// counter is series as the listener's /metrics renders it, -1 for no series.
func (r *outageRig) counter(t *testing.T, series string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return counterValue(t, recorder.Body.String(), series)
}

// heldDroppedUndecided is the series of the frames the hold dropped undecided at its limit.
const heldDroppedUndecided = `envoy_listener_held_dropped_total{reason="undecided"}`

// relayDecidedUnknown is the series of the frames the relay decided while the answer was unknown,
// each of which it holds.
const relayDecidedUnknown = `envoy_listener_project_decisions_total{answer="unknown"}`

// slackWatch counts what is published on either Slack lane: notifications.slack.> and every
// private project's notifications.legion.<token>.slack.>.
type slackWatch struct {
	mu       sync.Mutex
	subjects []string
}

func watchSlack(t *testing.T, url string) *slackWatch {
	t.Helper()
	watch := &slackWatch{}
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	for _, subject := range []string{"notifications.slack.>", contracts.LegionNoticeTopicPrefix + "*.slack.>"} {
		if _, err := conn.Subscribe(subject, func(msg *natsgo.Msg) {
			watch.mu.Lock()
			defer watch.mu.Unlock()
			watch.subjects = append(watch.subjects, msg.Subject)
		}); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the Slack watch: %v", err)
	}
	return watch
}

func (w *slackWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.subjects)
}

// outageScene is one listener, its three sessions and the Slack watch, under one gate.
type outageScene struct {
	rig      *outageRig
	chain    *outageChain
	tokens   map[string]string
	sessions map[string]*sessionStandIn
	slack    *slackWatch
	issue    atomic.Int64
	findings []string
}

func newOutageScene(t *testing.T, chain *outageChain, gate outageGate) *outageScene {
	t.Helper()
	rig := newOutageRig(t, chain.source(t, gate))
	scene := &outageScene{rig: rig, chain: chain, tokens: map[string]string{}, sessions: map[string]*sessionStandIn{}, slack: watchSlack(t, rig.url)}
	for session, projects := range map[string][]string{outageE: nil, outageV: {"PRIV"}, outageW: {"INFRA"}} {
		scene.tokens[session] = rig.token(session, projects...)
		scene.sessions[session] = startSessionStandIn(t, rig.url, session, answerDone)
	}
	scene.heartbeat(t)
	return scene
}

// heartbeat registers each session as its plugin does on every heartbeat.
func (s *outageScene) heartbeat(t *testing.T) {
	t.Helper()
	for session, token := range s.tokens {
		recorder := s.rig.call(t, http.MethodPost, token, "/v1/interests/subscribe", consumerRegistration(session, "epoch-1", "notifications.dispatch.issue.>"))
		requireStatus(t, recorder, http.StatusOK, "the registration of "+session)
	}
}

// find records something a run let through that it must hold.
func (s *outageScene) find(t *testing.T, format string, args ...any) {
	t.Helper()
	finding := fmt.Sprintf(format, args...)
	t.Logf("FINDING: %s", finding)
	s.findings = append(s.findings, finding)
}

// store stores a new frame of project's issue and returns it.
func (s *outageScene) store(t *testing.T, project string) contracts.Envelope {
	t.Helper()
	key := fmt.Sprintf("%s-%d", project, s.issue.Add(1))
	item, _ := s.rig.storeEvent(t, contracts.DispatchIssueSubject(project, key, "comment"))
	return item
}

// next is session's next frame within window, and whether one came.
func (s *outageScene) next(session string, window time.Duration) (relayedFrame, bool) {
	select {
	case frame := <-s.sessions[session].frames:
		return frame, true
	case <-time.After(window):
		return relayedFrame{}, false
	}
}

// drained is every frame session has received and the test has not read.
func (s *outageScene) drained(session string) []relayedFrame {
	var frames []relayedFrame
	for {
		select {
		case frame := <-s.sessions[session].frames:
			frames = append(frames, frame)
		default:
			return frames
		}
	}
}

// slackEvent serves one Slack event and returns the answer and how long it took.
func (s *outageScene) slackEvent() (*httptest.ResponseRecorder, time.Duration) {
	body := `{"type":"event_callback","event_id":"Ev` + fmt.Sprint(s.issue.Add(1)) + `","team_id":"` + outageTeam + `",` +
		`"event":{"type":"message","channel":"` + outageChannel + `","user":"U0OUTAGE01","text":"hello","ts":"1700000000.000100"}}`
	return serveDelivery(s.rig.slack, slackDelivery(body))
}

// sessionsRead is E's GET /v1/sessions and how long it took. E's token lists no project, so the
// route reads the privacy answer to decide whether E sees V's and W's sessions.
func (s *outageScene) sessionsRead(t *testing.T) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	started := time.Now()
	recorder := s.rig.call(t, http.MethodGet, s.tokens[outageE], "/v1/sessions", "")
	return recorder, time.Since(started)
}

// baseline holds phase's answer before any outage: the frame of phase.held reaches E only while
// its project is public, the Slack event is published, and E's session list answers.
func (s *outageScene) baseline(t *testing.T, phase outagePhase) {
	t.Helper()
	s.chain.answer.Store(phase.answer)
	held := s.store(t, phase.held)
	marker := s.store(t, "PUB")
	want := []contracts.Envelope{marker}
	if phase.held == "PUB" {
		want = []contracts.Envelope{held, marker}
	}
	requireFrames(t, s.sessions[outageE].collect(t, len(want), outageArrival), want...)
	if phase.held == "PRIV" {
		requireFrames(t, s.sessions[outageV].collect(t, 1, outageArrival), held)
	}
	before := s.slack.count()
	if recorder, _ := s.slackEvent(); recorder.Code != http.StatusOK {
		t.Fatalf("%s, before the outages: the Slack intake answered %d %q, want 200", phase.name, recorder.Code, recorder.Body.String())
	}
	// The watch's own control: it sees what the intake publishes while the answer is known.
	testwait.Eventually(t, "the Slack event published before the outages to reach the watch", func() bool { return s.slack.count() > before })
	if recorder, _ := s.sessionsRead(t); recorder.Code != http.StatusOK {
		t.Fatalf("%s, before the outages: E's GET /v1/sessions answered %d %s, want 200", phase.name, recorder.Code, recorder.Body.String())
	}
}

// outage runs one case of phase and records each thing it let through.
func (s *outageScene) outage(t *testing.T, phase outagePhase, c outageCase) {
	t.Helper()
	name := phase.name + ", " + c.name
	s.heartbeat(t)
	c.start(s.chain)
	dropsBefore := s.rig.counter(t, heldDroppedUndecided)
	unknownBefore := s.rig.counter(t, relayDecidedUnknown)
	stored := time.Now()
	held := s.store(t, phase.held)

	// The relay decides the frame alone, so its read is the only one in flight and the hold
	// begins c.firstRead after the store. A Slack event or a route sent first would take that
	// read, and the relay's decision would wait for the next one (k7Reader), beginning the hold
	// up to one more read later.
	delivered := false
	decideBy := time.Now().Add(outageArrival)
	for {
		if frame, ok := s.next(outageE, 100*time.Millisecond); ok {
			s.find(t, "%s: E, whose token lists no project, received %s's frame %s %s after it was stored, during the outage",
				name, projectOfFrame(frame), frame.item.EventID, frame.arrived.Sub(stored).Round(time.Millisecond))
			delivered = true
			break
		}
		if s.rig.counter(t, relayDecidedUnknown) > unknownBefore {
			break
		}
		if time.Now().After(decideBy) {
			s.find(t, "%s: the relay took no decision on %s's frame %s within %s", name, phase.held, held.EventID, outageArrival)
			break
		}
	}

	slackBefore := s.slack.count()
	slackAnswer, slackTook := s.slackEvent()
	if slackAnswer.Code != http.StatusServiceUnavailable {
		s.find(t, "%s: the Slack intake answered %d %q, want 503", name, slackAnswer.Code, strings.TrimSpace(slackAnswer.Body.String()))
	}
	routeAnswer, routeTook := s.sessionsRead(t)
	var refusal struct{ Code string }
	_ = json.Unmarshal(routeAnswer.Body.Bytes(), &refusal)
	if routeAnswer.Code != http.StatusServiceUnavailable || refusal.Code != listRefillingCode || routeAnswer.Header().Get("Retry-After") != listRefillingRetryAfter {
		s.find(t, "%s: E's GET /v1/sessions answered %d %s (Retry-After %q), want 503 refilling", name, routeAnswer.Code,
			strings.TrimSpace(routeAnswer.Body.String()), routeAnswer.Header().Get("Retry-After"))
	}

	// The frame is held for E until the limit drops it; a frame E receives meanwhile is one the
	// outage let through.
	var dropped time.Duration
	deadline := time.Now().Add(outageDropWithin)
	for !delivered && dropped == 0 {
		if frame, ok := s.next(outageE, 100*time.Millisecond); ok {
			s.find(t, "%s: E, whose token lists no project, received %s's frame %s %s after it was stored, during the outage",
				name, projectOfFrame(frame), frame.item.EventID, frame.arrived.Sub(stored).Round(time.Millisecond))
			break
		}
		if s.rig.counter(t, heldDroppedUndecided) > dropsBefore {
			dropped = time.Since(stored)
			// A drop before the limit loses a frame a read could still have decided; one long
			// after it holds a frame past the bound R9's outage exception states.
			limit := c.firstRead + outageUndecided
			if dropped < limit {
				s.find(t, "%s: %s's frame held for E was dropped %s after it was stored, before the hold's limit of %s after its first read",
					name, phase.held, dropped.Round(time.Millisecond), outageUndecided)
			}
			if dropped > limit+outageDropLate {
				s.find(t, "%s: %s's frame held for E was dropped %s after it was stored, later than %s after the hold's limit of %s after its first read",
					name, phase.held, dropped.Round(time.Millisecond), outageDropLate, outageUndecided)
			}
			break
		}
		if time.Now().After(deadline) {
			s.find(t, "%s: %s's frame %s held for E was not dropped within %s", name, phase.held, held.EventID, outageDropWithin)
			break
		}
	}
	switch frames := s.drained(outageV); {
	case phase.held == "PRIV" && (len(frames) != 1 || frames[0].item.EventID != held.EventID):
		s.find(t, "%s: V, whose token lists PRIV, received %d frames during the outage, want PRIV's frame %s at once", name, len(frames), held.EventID)
	case phase.held != "PRIV" && len(frames) > 0:
		s.find(t, "%s: V, whose token lists PRIV, received %s's frame %s", name, projectOfFrame(frames[0]), frames[0].item.EventID)
	}
	if frames := s.drained(outageW); len(frames) > 0 {
		s.find(t, "%s: W, whose token lists INFRA, received %s's frame %s", name, projectOfFrame(frames[0]), frames[0].item.EventID)
	}
	if count := s.slack.count(); count != slackBefore {
		s.find(t, "%s: %d Slack events were published during the outage, want none", name, count-slackBefore)
	}

	// After the outage: the first decision reads the new answer, one read at a time.
	c.end(s.chain)
	testwait.Eventually(t, "the broker tier to finish the reads the outage left", func() bool { return s.chain.broker.inflight.Load() == 0 })
	readsBefore := s.chain.broker.requests.Load()
	s.chain.broker.inflightMax.Store(0)
	recovered := time.Now()
	var private contracts.Envelope
	if phase.held == "PRIV" {
		private = s.store(t, "PRIV")
	}
	public := s.store(t, "PUB")
	frame, ok := s.next(outageE, outageArrival)
	switch {
	case !ok:
		s.find(t, "%s: after the outage E received no frame within %s, want PUB's frame %s", name, outageArrival, public.EventID)
	case frame.item.EventID != public.EventID:
		s.find(t, "%s: after the outage E's next frame is %s's %s, want PUB's %s", name, projectOfFrame(frame), frame.item.EventID, public.EventID)
	case dropped > 0 && (frame.msg.Header.Get(relayHeaderGap) != "true" || frame.msg.Header.Get(relayHeaderGapReason) != privacyHoldGap):
		s.find(t, "%s: E's first frame after the drop carries %s %q and %s %q, want true and %s", name,
			relayHeaderGap, frame.msg.Header.Get(relayHeaderGap), relayHeaderGapReason, frame.msg.Header.Get(relayHeaderGapReason), privacyHoldGap)
	}
	if reads := s.chain.broker.requests.Load() - readsBefore; reads == 0 {
		s.find(t, "%s: the first decisions after the outage made no read of the broker", name)
	}
	if most := s.chain.broker.inflightMax.Load(); most > 1 {
		s.find(t, "%s: %d reads were in flight at once after the outage, want at most 1", name, most)
	}
	if phase.held == "PRIV" {
		if frames := s.drained(outageV); len(frames) != 1 || frames[0].item.EventID != private.EventID {
			s.find(t, "%s: after the outage V received %d frames, want PRIV's frame %s", name, len(frames), private.EventID)
		}
	}
	t.Logf("OUTAGE %s: %s's frame held for E from its store, dropped %s after it (limit %s); Slack %d in %s; E's GET /v1/sessions %d in %s; "+
		"after the outage E's next frame in %s with %s %q",
		name, phase.held, dropped.Round(time.Millisecond), outageUndecided, slackAnswer.Code, slackTook.Round(time.Millisecond), routeAnswer.Code,
		routeTook.Round(time.Millisecond), frame.arrived.Sub(recovered).Round(time.Millisecond), relayHeaderGapReason, frame.msg.Header.Get(relayHeaderGapReason))
}

// projectOfFrame is the project frame's subject names.
func projectOfFrame(frame relayedFrame) string {
	project, _ := contracts.SubjectProject(frame.item.Topic)
	return project
}

// run holds the baseline of phase, then runs each case, and returns what the cases let through.
func (s *outageScene) run(t *testing.T, phase outagePhase, cases []outageCase) []string {
	t.Helper()
	s.findings = nil
	s.baseline(t, phase)
	for _, c := range cases {
		s.outage(t, phase, c)
	}
	return s.findings
}

// lowerHoldLimits lowers the hold's limits for this test and restores them after it; the hold
// reads them when it decides, so they are set before any relay starts.
func lowerHoldLimits(t *testing.T) {
	t.Helper()
	if holdLimits.undecided != 120*time.Second || holdLimits.redecide != 30*time.Second {
		t.Fatalf("the hold drops a frame undecided at %s and reads a held lane again every %s, want 120 s and 30 s (R9)", holdLimits.undecided, holdLimits.redecide)
	}
	saved := holdLimits
	t.Cleanup(func() { holdLimits = saved })
	holdLimits.undecided, holdLimits.redecide = outageUndecided, outageRedecide
}

func TestAnOutageHoldsAndRecovers(t *testing.T) {
	lowerHoldLimits(t)
	started := time.Now()
	chain := newOutageChain(t)
	scene := newOutageScene(t, chain, outageGateNone)
	for _, phase := range []outagePhase{outagePrivate, outagePublic} {
		if findings := scene.run(t, phase, outageCases()); len(findings) > 0 {
			t.Fatalf("%s: the outages let through:\n%s", phase.name, strings.Join(findings, "\n"))
		}
	}
	// W's quiet is a reading only if W receives what its token lists: its own project's frame.
	infra := scene.store(t, "INFRA")
	requireFrames(t, scene.sessions[outageW].collect(t, 1, outageArrival), infra)
	// "At most one read in flight" is a reading only if the broker tier counts two reads at once
	// when two are sent at once: two callers that are not the listener's reader.
	chain.broker.answerAfter(500 * time.Millisecond)
	chain.broker.inflightMax.Store(0)
	var concurrent sync.WaitGroup
	for range 2 {
		concurrent.Go(func() {
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, chain.broker.url()+"/v1/project-privacy", nil)
			request.Header.Set("Authorization", "Bearer "+edgeAccountToken)
			if response, err := http.DefaultClient.Do(request); err == nil {
				_ = response.Body.Close()
			}
		})
	}
	concurrent.Wait()
	chain.broker.answerAfter(0)
	if most := chain.broker.inflightMax.Load(); most < 2 {
		t.Fatalf("two reads sent at once were counted %d in flight at once, want 2 or more: the in-flight check could not fail", most)
	}
	t.Logf("the eight outages with their recoveries took %s", time.Since(started).Round(time.Millisecond))

	// Each control puts a faulty reader in front of the broker, on a listener of its own, and the
	// same runs fail naming each case.
	for _, control := range []struct {
		name  string
		gate  outageGate
		phase outagePhase
		want  string
	}{
		{"a reader that takes an outage as no private project", outageGateFailOpen, outagePrivate, "received PRIV's frame"},
		{"a reader that keeps the last whole answer through an outage", outageGateKeepLast, outagePublic, "received PUB's frame"},
	} {
		t.Run("control: "+control.name, func(t *testing.T) {
			scene := newOutageScene(t, newOutageChain(t), control.gate)
			findings := scene.run(t, control.phase, outageCases())
			for _, c := range outageCases() {
				named := control.phase.name + ", " + c.name + ": E, whose token lists no project, " + control.want
				if !slices.ContainsFunc(findings, func(finding string) bool { return strings.HasPrefix(finding, named) }) {
					t.Errorf("under %s the run did not fail naming %q; it found:\n%s", control.name, named, strings.Join(findings, "\n"))
				}
			}
		})
	}
}
