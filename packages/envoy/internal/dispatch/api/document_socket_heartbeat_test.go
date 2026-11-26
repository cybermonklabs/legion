package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testwait"
)

// documentSocketTokenRig is a server whose people sign in with a session cookie, as
// newCookieServerOn's do, and whose agents present tokens the secrets broker issued, as
// newBrokerTokenRig's do: the one server where a request can carry both, a cookie and a token
// beside it. The API and the document service share one cookie identity, as cmd/dispatch hands
// them one. A rig made with verifyingServiceAccounts verifies service-account tokens too
// (Deps.OIDC), as a deployment that serves its fleet's pods does.
type documentSocketTokenRig struct {
	// tokens is the rig the broker-token verifier comes from: its stub broker answers what a test
	// says a token is, and its deps.BrokerTokens is the verifier this server decides tokens with.
	// Its own server is sent no socket; a test that needs a person's agent token mints it there
	// (mintPersonalToken on its handler, readOnlyPersonalTokens), on the database this server
	// reads too.
	tokens *brokerTokenRig
	// serviceTokens is the issuer of the service-account tokens this server verifies, and
	// serviceVerifies the verifier it verifies them with, counting the tokens it is asked to
	// verify: both nil on a rig made without verifyingServiceAccounts.
	serviceTokens   *serviceTokenIssuer
	serviceVerifies *countedServiceVerifier
	// statements records each statement the server's pool runs (poolStatements), and
	// specWriterReads each read of the spec-writer rule it runs (specWriterOfIssueSQL).
	statements      *poolStatements
	specWriterReads *specWriterReads
	sessions        *store.PgSessionStore
	// logins is the API's Identity: the cookie identity the document service shares, counting
	// the lookups made through it (countedLogins).
	logins *countedLogins
	// heartbeats records the calls a socket's heartbeat makes into the product, once a test
	// starts it (heartbeatCalls): the API's lookups of the cookie, and the verifier's clock
	// reads and introspections (timeBrokerCalls).
	heartbeats *heartbeatCalls
	// spans records, for each connection the server accepts, when the server itself ended it
	// (servedSpans): the record of the socket's close a test reads.
	spans *servedSpans
	// served is the server register mounted, whose brokerTokenHeartbeat a row replaces before the
	// server takes a socket, and documents its document service.
	served     *server
	documents  *docs.Service
	cookie     *http.Cookie
	server     *httptest.Server
	sockets    *servedSockets
	artifactID string
}

// documentSocketPerson is the person signed in on a documentSocketTokenRig.
const documentSocketPerson = "alice@d.example"

// documentSocketRigConfig is what newDocumentSocketTokenRig's options set: the options its
// broker-token verifier takes after the rig's own, and whether its server verifies
// service-account tokens.
type documentSocketRigConfig struct {
	verifier        []func(*ciphertoken.Options)
	serviceAccounts bool
}

// brokerTokenOptions applies options to the rig's broker-token verifier's options after the rig's
// own, for a test that needs a bucket it can empty or a broker that does not answer.
func brokerTokenOptions(options ...func(*ciphertoken.Options)) func(*documentSocketRigConfig) {
	return func(config *documentSocketRigConfig) { config.verifier = append(config.verifier, options...) }
}

// verifyingServiceAccounts makes the rig's server verify service-account tokens (Deps.OIDC) with
// an issuer of the rig's own (serviceTokens, serviceVerifies). A rig made without it has no such
// verifier: its Deps.OIDC is nil.
func verifyingServiceAccounts(config *documentSocketRigConfig) { config.serviceAccounts = true }

// newDocumentSocketTokenRig signs documentSocketPerson in with a session cookie and gives her an
// issue whose spec she can open, on a server whose sockets resolve their caller every heartbeat.
// The verifier is newBrokerTokenRig's, taken from that rig rather than built a second time, so it
// is the one every broker-token test drives: the same stub broker, the replay table on this same
// database, and the switches cmd/dispatch sets on it. options are brokerTokenOptions and
// verifyingServiceAccounts. The server's heartbeats run on beats (Deps.Clock; nil is the real
// clock), which a row that moves through them hands a fake, and a live socket's read deadline on a
// fake no one moves (Deps.LiveSocketClock), so no pong the runner holds back ends one. The server
// is register's own, as newRegisteredTestServer's is, so a row can replace a check of it (served).
func newDocumentSocketTokenRig(t *testing.T, heartbeat time.Duration, beats clock.Clock, options ...func(*documentSocketRigConfig)) *documentSocketTokenRig {
	t.Helper()
	ctx := context.Background()
	var config documentSocketRigConfig
	for _, option := range options {
		option(&config)
	}
	rig := &documentSocketTokenRig{heartbeats: &heartbeatCalls{}, statements: &poolStatements{}, specWriterReads: &specWriterReads{}}
	rig.tokens = newBrokerTokenRigOn(t, testServerOptions{tracer: multitracer.New(rig.statements, rig.specWriterReads)},
		append([]func(*ciphertoken.Options){timeBrokerCalls(t, rig.heartbeats)}, config.verifier...)...)
	var serviceVerifier ServiceTokenVerifier
	if config.serviceAccounts {
		rig.serviceTokens = newServiceTokenIssuer(t)
		rig.serviceVerifies = &countedServiceVerifier{ServiceTokenVerifier: rig.serviceTokens.verifier(t)}
		serviceVerifier = rig.serviceVerifies
	}
	database := rig.tokens.database
	rig.sessions = store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions}
	rig.logins = &countedLogins{Identity: person, calls: rig.heartbeats}
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = rig.documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: rig.logins, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: testServerURL, Docs: rig.documents, Events: broker, OIDC: serviceVerifier,
		BrokerTokens: rig.tokens.deps.BrokerTokens, StreamHeartbeat: heartbeat, Clock: beats,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	deps.LiveSocketClock = clocktest.Held()
	mux := http.NewServeMux()
	rig.served = register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, rig.served) })
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.spans = &servedSpans{}
	rig.server = httptest.NewUnstartedServer(rig.sockets.serve(mux.ServeHTTP))
	rig.server.Listener = rig.spans.listen(rig.server.Listener)
	rig.server.Start()
	t.Cleanup(rig.server.Close)

	// A session cookie is issued at sign-in, and a sign-in records the person.
	seedPeople(t, database, documentSocketPerson)
	generation, err := rig.sessions.EnsureSession(ctx, documentSocketPerson)
	if err != nil {
		t.Fatalf("establish the session: %v", err)
	}
	if rig.cookie, err = http.ParseSetCookie(auth.IssueSessionCookie(documentSocketPerson, generation, testSigningKey, false)); err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	signedIn := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
		request.AddCookie(rig.cookie)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	if response := signedIn(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := signedIn(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Live document", "spec": "The spec."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	rig.artifactID = decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created).PrimaryArtifactID
	return rig
}

// closedAt is when the server ended the last connection it accepted - the socket, since every
// request of the rig's own setup goes to the mux directly - by its own record (servedSpans): when
// it began to write its close frame, or, where it wrote none, when it closed the connection. It is
// false until the server has done either.
func (rig *documentSocketTokenRig) closedAt() (time.Time, bool) {
	rig.spans.mu.Lock()
	defer rig.spans.mu.Unlock()
	if len(rig.spans.conns) == 0 {
		return time.Time{}, false
	}
	conn := rig.spans.conns[len(rig.spans.conns)-1]
	if !conn.closeFrame.IsZero() {
		return conn.closeFrame, true
	}
	return conn.closed, !conn.closed.IsZero()
}

// specWriterReads records each read of the spec-writer rule a traced pool runs (judgeSpecWriter's
// specWriterOfIssueSQL), by the session it judges and the issue whose chain it reads: once as the
// statement starts, and once its answer has been read, which is before the rule has decided on it.
type specWriterReads struct {
	mu                sync.Mutex
	started, answered []judgedWrite
}

// judgedWrite is what one read of the spec-writer rule judges: the session, and the issue.
type judgedWrite struct{ session, issue string }

// specWriterReadKey carries a read's judgedWrite from its start to its end.
type specWriterReadKey struct{}

func (s *specWriterReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL != specWriterOfIssueSQL || len(data.Args) != 2 {
		return ctx
	}
	session, _ := data.Args[0].(string)
	issue, _ := data.Args[1].(string)
	read := judgedWrite{session: session, issue: issue}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, read)
	return context.WithValue(ctx, specWriterReadKey{}, read)
}

func (s *specWriterReads) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if read, ok := ctx.Value(specWriterReadKey{}).(judgedWrite); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.answered = append(s.answered, read)
	}
}

// specWriterReadMark is how many reads had started and how many had been answered at a moment.
type specWriterReadMark struct{ started, answered int }

// mark is where the reads stand now, for since.
func (s *specWriterReads) mark() specWriterReadMark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return specWriterReadMark{started: len(s.started), answered: len(s.answered)}
}

// since answers the reads that started after mark, and how many reads of want were answered
// after it.
func (s *specWriterReads) since(mark specWriterReadMark, want judgedWrite) (started []judgedWrite, answered int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, read := range s.answered[mark.answered:] {
		if read == want {
			answered++
		}
	}
	return append([]judgedWrite(nil), s.started[mark.started:]...), answered
}

// countedLogins is an Identity that records the lookups made through it as heartbeat calls
// (heartbeatCalls.begin) and counts those that start once the test has said the cookie's session
// ended (end): on a documentSocketTokenRig, the API's, which the api package asks of each socket a
// cookie opens, once at its upgrade (refuseDocumentSocketCredential) and again at each of its
// heartbeats (documentSocketStillResolves, signInStillResolves). A lookup counts when it starts
// after end, so one already under way when the session ended, which may find the session either
// way, is not counted.
type countedLogins struct {
	identity.Identity
	calls    *heartbeatCalls
	ended    atomic.Bool
	afterEnd atomic.Int64
}

func (c *countedLogins) Login(r *http.Request) (string, error) {
	done := c.calls.begin()
	if c.ended.Load() {
		c.afterEnd.Add(1)
	}
	email, err := c.Identity.Login(r)
	done(err != nil)
	return email, err
}

// end marks the cookie's session ended: every lookup that starts from now on is counted.
func (c *countedLogins) end() { c.ended.Store(true) }

// heartbeatSlack is what scheduling may add to a heartbeat beside the longest single stall a stall
// meter saw: how much later than a heartbeat after the last call returned the next call of a
// socket's heartbeat may start, once that stall is allowed too (gapWithin).
const heartbeatSlack = time.Second

// heartbeatCalls records, once the test starts it (record), each call a socket's heartbeat makes
// into the product: when it started, when it returned, and whether its answer refused the caller.
// The calls are the product's own - the API Identity's lookups on a document socket
// (countedLogins), the broker-token verifier's clock reads and introspections on a live socket
// (timeBrokerCalls) - and so are the gaps between them: from the return of the calls before one
// to that call's start. whileCallerResolves arms its ticker before its first call, and a tick that
// falls during a slow call waits for it, so the next call starts at most a heartbeat after the
// last one returned however long a call takes. Each gap is judged only once the call that ends it
// is recorded, or at the close, never off the test's clock while the heartbeat may be about to
// call, and is allowed a heartbeat, heartbeatSlack, and the longest single stall the row's stall
// meter saw inside it: the delay of the one goroutine wake there, the heartbeat's own on its tick
// (requireGapWithin). A heartbeat that skips a tick, keeps an answer it stored past a beat, or asks
// on a ticker of its own slower than a heartbeat and heartbeatSlack leaves a gap past that; a
// ticker of its own no slower than that, with the stall, passes, since nothing in the gap tells it
// from a scheduling delay. A call that refused is the heartbeat's last: it closes the socket and
// asks no more, so no gap after it is judged.
type heartbeatCalls struct {
	mu        sync.Mutex
	recording bool
	calls     []heartbeatCall
	// judged is how many of calls have had the gap before them judged (newGaps).
	judged int
	// introspections are the verifier's introspections, recorded from the start, the upgrade's
	// own among them.
	introspections []heartbeatCall
}

// heartbeatCall is one call: when it started, when it returned (zero while it runs), and whether
// its answer refused the caller.
type heartbeatCall struct {
	start, end time.Time
	refused    bool
}

// heartbeatGap is a span in which no call of the heartbeat ran: from the latest return of the
// calls before a call to that call's start.
type heartbeatGap struct{ from, to time.Time }

// record starts recording the heartbeat's calls.
func (h *heartbeatCalls) record() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recording = true
}

// begin records the start of a call, once recording, and answers what records its return and
// whether its answer refused the caller.
func (h *heartbeatCalls) begin() func(refused bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.recording {
		return func(bool) {}
	}
	index := len(h.calls)
	h.calls = append(h.calls, heartbeatCall{start: time.Now()})
	return func(refused bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls[index].end, h.calls[index].refused = time.Now(), refused
	}
}

// introspected records an introspection of the verifier's, from its request to its answer.
func (h *heartbeatCalls) introspected(start, end time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.introspections = append(h.introspections, heartbeatCall{start: start, end: end})
}

// newGaps answers the gaps before the calls recorded since it last answered, and marks those calls
// judged: one before each call that started once every call before it had returned, none before
// the first call (nothing of the product's comes before it), none before a call that started while
// an earlier one ran, and none after a call that refused. A call is recorded at its start, so the
// gap before it is whole once it is recorded; a call still running when a later one is recorded
// was running at that call's start, and its return comes after.
func (h *heartbeatCalls) newGaps() []heartbeatGap {
	h.mu.Lock()
	defer h.mu.Unlock()
	var gaps []heartbeatGap
	var latest time.Time
	running, refused := false, false
	for i, call := range h.calls {
		if refused {
			break
		}
		if i >= h.judged && i > 0 && !running && call.start.After(latest) {
			gaps = append(gaps, heartbeatGap{from: latest, to: call.start})
		}
		switch {
		case call.end.IsZero():
			running = true
		case call.end.After(latest):
			latest = call.end
		}
		refused = call.refused
	}
	h.judged = len(h.calls)
	return gaps
}

// refusedAt is when the heartbeat's first refusing call returned: the refusal that orders the
// socket closed. It is false while no call has refused.
func (h *heartbeatCalls) refusedAt() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, call := range h.calls {
		if call.refused {
			return call.end, true
		}
	}
	return time.Time{}, false
}

// requireClosedAfterRefusal fails the test unless the server ended the socket (closedAt) within a
// heartbeat of the heartbeat's refusing call returning (refusedAt), both the product's own
// records, allowing for each of hops - the goroutine wakes between the two records - the longest
// single stall meter saw between them (requireClosedWithin): a refused caller's socket closes as
// soon as its refusal is read, and a product that waits on its own before closing it fails here.
func (rig *documentSocketTokenRig) requireClosedAfterRefusal(t *testing.T, meter *stallMeter, what string, heartbeat time.Duration, hops int) {
	t.Helper()
	refused, ok := rig.heartbeats.refusedAt()
	if !ok {
		t.Fatalf("%s ended with no call of its heartbeat refusing its caller", what)
	}
	closed := testwait.Until(t, "the server's own end of "+what, rig.closedAt)
	requireClosedWithin(t, meter, what, refused, closed, heartbeat, hops)
}

// count is how many calls the recording holds.
func (h *heartbeatCalls) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// returned is how many of the recorded calls have returned.
func (h *heartbeatCalls) returned() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, call := range h.calls {
		if !call.end.IsZero() {
			n++
		}
	}
	return n
}

// lastIntrospectionGap is the span from the answer of the last introspection but one to the
// request of the last, and false when there were fewer than two.
func (h *heartbeatCalls) lastIntrospectionGap() (heartbeatGap, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.introspections) < 2 {
		return heartbeatGap{}, false
	}
	last, before := h.introspections[len(h.introspections)-1], h.introspections[len(h.introspections)-2]
	return heartbeatGap{from: before.end, to: last.start}, true
}

// await waits until done holds, judging each gap of the heartbeat's calls as the call that ends it
// is recorded (requireEveryHeartbeat): a heartbeat that asks less often fails by name at its next
// call. A heartbeat that stops asking makes no call to judge, and the wait's one bound, the test
// binary's deadline, ends it.
func (h *heartbeatCalls) await(t *testing.T, meter *stallMeter, heartbeat time.Duration, what string, done func() bool) {
	t.Helper()
	testwait.UntilPolling(t, 10*time.Millisecond, what+" (the heartbeat's calls recorded)", func() (int, bool) {
		h.requireEveryHeartbeat(t, meter, heartbeat)
		return h.count(), done()
	})
}

// requireEveryHeartbeat fails the test unless each gap before a call recorded since it, or await,
// last judged is within a heartbeat and heartbeatSlack plus the longest single stall meter saw
// inside it (requireGapWithin).
func (h *heartbeatCalls) requireEveryHeartbeat(t *testing.T, meter *stallMeter, heartbeat time.Duration) {
	t.Helper()
	for _, gap := range h.newGaps() {
		requireGapWithin(t, meter, "the socket's heartbeat did not ask on every beat: from the return of its calls to the start of the next", gap.from, gap.to, heartbeat+heartbeatSlack)
	}
}

// timeBrokerCalls is the verifier option that records its calls on heartbeats: each read of its
// clock (Options.Now), which every Verify and LiveUntil call makes first (Verifier.known), as a
// call that returns at once, and each introspection, sent through a recording proxy in front of
// the broker, as a call from its request to its answer, which refused when the broker answered the
// token not live.
func timeBrokerCalls(t *testing.T, heartbeats *heartbeatCalls) func(*ciphertoken.Options) {
	return func(options *ciphertoken.Options) {
		target, err := url.Parse(options.BrokerURL)
		if err != nil {
			t.Fatalf("parse the broker's URL: %v", err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ModifyResponse = func(response *http.Response) error {
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				return err
			}
			response.Body = io.NopCloser(bytes.NewReader(body))
			var answer struct {
				Active bool `json:"active"`
			}
			if refused, ok := response.Request.Context().Value(introspectionRefusedKey{}).(*bool); ok && json.Unmarshal(body, &answer) == nil {
				*refused = !answer.Active
			}
			return nil
		}
		recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			done := heartbeats.begin()
			started := time.Now()
			refused := new(bool)
			proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), introspectionRefusedKey{}, refused)))
			heartbeats.introspected(started, time.Now())
			done(*refused)
		}))
		t.Cleanup(recorder.Close)
		options.BrokerURL = recorder.URL
		options.Now = func() time.Time {
			heartbeats.begin()(false)
			return time.Now()
		}
	}
}

// introspectionRefusedKey carries, on an introspection timeBrokerCalls's proxy forwards, where it
// writes whether the broker answered the token not live.
type introspectionRefusedKey struct{}

// openDocument opens room's document socket as the dashboard's editor does, with every header of
// beside on the upgrade, and the person's cookie beside them when withCookie, and closes it when
// the test ends.
func (rig *documentSocketTokenRig) openDocument(t *testing.T, room string, withCookie bool, beside http.Header) *syncedPeer {
	t.Helper()
	headers := beside.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if withCookie {
		headers.Set("Cookie", rig.cookie.Name+"="+rig.cookie.Value)
	}
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + room, sockets: rig.sockets,
		headers: headers, artifactID: room, doc: crdt.New(),
	}
	peer.connect(t)
	t.Cleanup(peer.close)
	return peer
}

// presentedHeaders are the credential headers of presented, the request a broker caller sends:
// its token under the DPoP scheme and the proof that goes beside it.
func presentedHeaders(presented *http.Request) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", presented.Header.Get("Authorization"))
	headers.Set("DPoP", presented.Header.Get("DPoP"))
	return headers
}

// driveHeartbeats moves fake through n of the server's heartbeats from first, each one heartbeat
// after the one before, waiting before each move for every one of the sockets open to have armed
// it there, and answers the heartbeat after them once one is armed. The server arms its next
// heartbeat before it resolves its caller again and receives that heartbeat only once the check has
// returned, so each heartbeat seen armed proves every check two or more heartbeats before it found
// the caller still resolving.
func driveHeartbeats(t *testing.T, fake *clocktest.Fake, first time.Time, n int, every time.Duration, sockets int) time.Time {
	t.Helper()
	at := first
	for range n {
		awaitArmedCountAt(t, fake, "the sockets' heartbeat", at, sockets)
		fake.Set(at)
		at = at.Add(every)
	}
	awaitArmedAt(t, fake, "the socket's heartbeat after the ones the row moved through", at)
	return at
}

// A document socket the sign-in cookie admitted re-checks that cookie on each heartbeat, and
// nothing the request carries beside it (documentSocketStillResolves, signInStillResolves). A
// bearer sent beside the cookie admitted nothing, and must not keep the socket open once the
// cookie's session has ended: not the shared bearer, and not a personal token that writes, each of
// which the API on the same server still resolves after the close. The socket stays open while the
// cookie resolves, with the bearer beside it, and closes at the first heartbeat after the session's
// end, the broker never asked. The heartbeats are on the server's clock, a fake the row moves one
// heartbeat at a time, and the close is awaited with the clock standing at the heartbeat after the
// sign-out. The heartbeat's lookups of the cookie are counted too (countedLogins, heartbeatCalls):
// every heartbeat the row moves through looks the cookie up, so a heartbeat that asks every second
// tick or keeps an answer it stored fails, and no lookup that started once the session had ended
// may find it still resolving, as one served from a cache of the session's state would. The server
// ends the socket within a heartbeat of the lookup that refused returning, by the server's own
// record of its close, allowing the longest stall between them for the one goroutine wake there,
// the socket's on the heartbeat's cancel (requireClosedAfterRefusal, one hop), so a product that
// waits before it closes a refused socket fails too.
//
// Control: callerStillResolves reused as the document socket's heartbeat resolves the bearer beside
// the cookie (optionalActor), which keeps the socket open after the sign-out, and the row fails at
// its wait for the close.
func TestACookieDocumentSocketClosesThoughABearerRidesBesideIt(t *testing.T) {
	t.Parallel()
	const heartbeat = 2 * time.Second
	for _, beside := range []struct {
		name   string
		bearer func(*testing.T, *documentSocketTokenRig) string
	}{
		{"the shared bearer", func(*testing.T, *documentSocketTokenRig) string { return "agent-token" }},
		{"a personal token that writes", func(t *testing.T, rig *documentSocketTokenRig) string {
			return mintPersonalToken(t, rig.tokens.handler, brokerTestPerson, "a writer", []string{model.ScopePerson}).Token
		}},
	} {
		t.Run(beside.name, func(t *testing.T) {
			meter := startStallMeter(t)
			fake := clocktest.New(time.Now())
			opened := fake.Now()
			rig := newDocumentSocketTokenRig(t, heartbeat, fake)
			authorization := "Bearer " + beside.bearer(t, rig)
			peer := rig.openDocument(t, rig.artifactID, true, http.Header{"Authorization": {authorization}})
			rig.heartbeats.record()

			// While the cookie resolves the socket outlives several heartbeats and still
			// answers its editor.
			next := driveHeartbeats(t, fake, opened.Add(heartbeat), 4, heartbeat, 1)
			peer.barrier(t)
			// The server arms its next heartbeat before it checks the caller, so driveHeartbeats
			// returns once the fifth is armed, before the fourth check's lookup may have run. The
			// sign-out waits for that lookup to return, so the fourth check finds the session and
			// the fifth is the one that refuses, however late a loaded machine runs the fourth.
			waitUntil(t, "the four heartbeats' lookups of the cookie to return", func() bool { return rig.heartbeats.returned() >= 4 })

			if err := rig.sessions.RevokeSessions(context.Background(), documentSocketPerson); err != nil {
				t.Fatalf("sign out: %v", err)
			}
			rig.logins.end()
			fake.Set(next)
			testwait.Receive(t, "the document socket to close at the first heartbeat after its cookie's session ended, though "+
				beside.name+" rides beside it", peer.connection().Ended)
			// Five heartbeats ran, the four the row moved through and the one after the sign-out,
			// and each looks the cookie up.
			if calls := rig.heartbeats.count(); calls < 5 {
				t.Fatalf("the socket's heartbeat looked the cookie up %d times over five heartbeats, want once each: it asks less often than StreamHeartbeat", calls)
			}
			if lookups := rig.logins.afterEnd.Load(); lookups > 1 {
				t.Fatalf("the document socket closed after %d heartbeat lookups of its cookie that started once its session had ended, want the first to close it: a heartbeat found the ended session still resolving", lookups)
			}
			rig.requireClosedAfterRefusal(t, meter, "the document socket", heartbeat, 1)
			// The heartbeat cancels the connection's context with no close code, so ygo drops
			// the socket without a close frame.
			if code := peer.connection().CloseCode(); code != gws.CloseAbnormalClosure {
				t.Fatalf("the document socket closed with %d, want %d: the server dropping it", code, gws.CloseAbnormalClosure)
			}
			// The close is the cookie's: the broker was never asked, and the bearer beside the
			// cookie still resolves on the API of the same server.
			if asked := rig.tokens.broker.Count(); asked != 0 {
				t.Fatalf("the document socket's heartbeat asked the broker %d times, want 0", asked)
			}
			if whoami := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, authorization, "")); whoami.Code != http.StatusOK {
				t.Fatalf("the bearer beside the cookie no longer resolves on the API (status=%d body=%s), so the close says nothing of the cookie",
					whoami.Code, whoami.Body.String())
			}
		})
	}
}

// A document socket whose cookie still resolves stays open across its heartbeats, each of which
// re-checks that cookie: opened with the cookie alone, and opened with the shared bearer beside
// it, which the heartbeat never reads. The broker is never asked.
//
// Control: TestACookieDocumentSocketClosesThoughABearerRidesBesideIt, the same socket once its
// cookie's session has ended.
func TestADocumentSocketOutlivesItsHeartbeatsWhileItsCookieResolves(t *testing.T) {
	t.Parallel()
	const heartbeat = 250 * time.Millisecond
	for _, beside := range []struct {
		name    string
		headers http.Header
	}{
		{"the cookie alone", nil},
		{"the cookie beside the shared bearer", http.Header{"Authorization": {"Bearer agent-token"}}},
	} {
		t.Run(beside.name, func(t *testing.T) {
			fake := clocktest.New(time.Now())
			opened := fake.Now()
			rig := newDocumentSocketTokenRig(t, heartbeat, fake)
			peer := rig.openDocument(t, rig.artifactID, true, beside.headers)

			// Four of the server's heartbeats, each of which re-checks the cookie: the heartbeat
			// armed after them proves the first three checks found it resolving, since a check that
			// had not would have closed the socket and armed no heartbeat after the next.
			driveHeartbeats(t, fake, opened.Add(heartbeat), 4, heartbeat, 1)
			peer.barrier(t)
			if asked := rig.tokens.broker.Count(); asked != 0 {
				t.Fatalf("the document socket's heartbeat asked the broker %d times, want 0", asked)
			}
		})
	}
}

// GET /ws/live keeps its own rule, which the document socket's does not change: it is admitted
// by whichever credential the request carries, a broker-issued token here with no cookie at all,
// and its heartbeat resolves that token again (callerStillResolves). The socket outlives several
// heartbeats while the broker holds the token live, and closes with 1008 once the broker stops
// holding it live and the answer it remembered has run out: by the first introspection after the
// broker's answer changed, since the heartbeats before it are answered from the remembered answer
// and the first that asks the broker hears the token is no longer live. That is read off the
// product's own calls: the broker's introspections are counted (StubBroker.Count); the one that
// closes the socket is sent within the remembered answer's life (AffirmativeCacheTTL), a heartbeat
// and heartbeatSlack of the answer of the one before it, plus the longest stall the process saw
// between them; and each call the heartbeat makes into the verifier starts within a heartbeat and
// heartbeatSlack of the return of the calls before it, plus the longest stall in that gap
// (heartbeatCalls, timeBrokerCalls), so a heartbeat that skips a tick, keeps an answer it stored
// past a beat or asks on a ticker of its own slower than that fails too. The server writes its
// close frame within a heartbeat of the introspection that answered the token not live, by the
// server's own record, allowing the longest stall between them for each of the three goroutine
// wakes there - the verifier's client reading the answer, the heartbeat's cancel, the socket's
// writer (requireClosedAfterRefusal, three hops) - so a product that waits before it closes a
// refused socket fails too. The heartbeat is two seconds, more than heartbeatSlack, so skipping a
// tick is told apart from a late tick. The socket's read deadline runs on a fake no one moves
// (newDocumentSocketTokenRig), so no pong the runner holds back ends the socket first.
func TestALiveSocketOpenedByABrokerTokenResolvesThatTokenOnEveryHeartbeat(t *testing.T) {
	t.Parallel()
	meter := startStallMeter(t)
	const heartbeat = 2 * time.Second
	rig := newDocumentSocketTokenRig(t, heartbeat, nil)
	caller := newBrokerCaller(t)
	const token = "a-token-holding-the-live-socket-open"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	presented := caller.presenting(t, http.MethodGet, "/ws/live", token, nil)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(rig.server.URL, "http")+"/ws/live", presentedHeaders(presented)).read()
	rig.heartbeats.record()

	client.quiet(t, 3*heartbeat+heartbeat/2, "a live socket whose token the broker holds live")

	rig.tokens.broker.Says(token, ciphertoken.Introspection{Active: false})
	asked := rig.tokens.broker.Count()
	rig.heartbeats.await(t, meter, heartbeat, "the live socket's end once the broker stopped holding its token live", func() bool {
		select {
		case <-client.ended:
			return true
		default:
			return false
		}
	})
	if closed := client.closeError(t); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the live socket closed with %d %q once the broker stopped holding its token live, want 1008", closed.Code, closed.Text)
	}
	if after := rig.tokens.broker.Count() - asked; after > 1 {
		t.Fatalf("the live socket closed after %d introspections once the broker stopped holding its token live, want the first: a heartbeat kept the remembered answer past the introspection that refused it", after)
	}
	gap, ok := rig.heartbeats.lastIntrospectionGap()
	if !ok {
		t.Fatalf("the live socket closed with fewer than two introspections recorded, so the one that closed it has nothing to be timed from")
	}
	requireGapWithin(t, meter, "the introspection that closed the live socket was not sent within the remembered answer's life and a heartbeat of the answer of the one before it",
		gap.from, gap.to, ciphertoken.AffirmativeCacheTTL+heartbeat+heartbeatSlack)
	rig.heartbeats.requireEveryHeartbeat(t, meter, heartbeat)
	rig.requireClosedAfterRefusal(t, meter, "the live socket", heartbeat, 3)
}

// openLiveSocket opens GET /ws/live on the rig's server with token under the DPoP scheme and a
// fresh proof of caller's key for the live socket's own target, and reads it.
func (rig *documentSocketTokenRig) openLiveSocket(t *testing.T, caller *brokerCaller, token string) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(rig.server.URL, "http")+liveSocketPattern,
		presentedHeaders(caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))).read()
}

// A document socket a broker-issued token opened is kept by the broker's answer alone
// (brokerTokenHeartbeat), never by its upgrade's proof, which the upgrade spent: a heartbeat that
// presented that proof again would be refused as the replay it is, and close a sound socket at its
// first heartbeat. Opened with the token alone and no cookie, the socket outlives three heartbeats,
// each of which asks the verifier (timeBrokerCalls records each of its clock reads), and still
// answers its editor. The heartbeats are on the server's clock, a fake the row moves: the heartbeat
// armed after the fourth proves that the first three checks found the token still live
// (driveHeartbeats).
//
// Control: the heartbeat resolving the request's proof again (Verifier.Verify in place of
// LiveUntil in brokerTokenStillLive) closes the socket at its first heartbeat, which then arms no
// other, and the row fails at its wait for the second.
func TestADocumentSocketsHeartbeatPresentsNoSpentProof(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	rig := newDocumentSocketTokenRig(t, heartbeat, fake)
	caller := newBrokerCaller(t)
	const token = "a-token-whose-upgrade-spent-its-proof"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	peer := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))
	rig.heartbeats.record()

	driveHeartbeats(t, fake, opened.Add(heartbeat), 4, heartbeat, 1)
	peer.barrier(t)
	if asked := rig.heartbeats.count(); asked < 3 {
		t.Fatalf("over three heartbeats the socket asked the verifier %d times, want once each: the heartbeat does not ask after the token", asked)
	}
}

// A token the broker revokes closes the document socket it opened within
// ciphertoken.AffirmativeCacheTTL, two heartbeats and heartbeatSlack of the revocation, the bound
// the live socket's revocation row holds (TestALiveSocketOpenedByABrokerTokenResolvesThatTokenOnEveryHeartbeat):
// the heartbeats before the answer the verifier remembers runs out are answered from it, and the
// first that asks the broker hears the token is no longer live. The socket closes with no close
// frame (1006), as a socket whose caller is refused does. The span runs from the revocation, the
// row's own act, to the server's own record of the socket's end (servedSpans), allowing the
// longest single stall the stall meter saw for each of the three goroutine wakes there - the
// heartbeat on its tick, the verifier's client reading the broker's answer, the socket on the
// heartbeat's cancel (requireClosedWithin). The heartbeats run on the real clock, two seconds
// apart, as does the verifier's memory of the answer.
//
// Control: the heartbeat ignoring the broker's answer for a token keeps the socket open past the
// bound, and the row fails at its wait for the close.
func TestADocumentSocketClosesWhenTheBrokerRevokesItsToken(t *testing.T) {
	t.Parallel()
	meter := startStallMeter(t)
	const heartbeat = 2 * time.Second
	rig := newDocumentSocketTokenRig(t, heartbeat, nil)
	caller := newBrokerCaller(t)
	const token = "a-token-the-broker-revokes"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	peer := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))
	asked := rig.tokens.broker.Count()

	rig.tokens.broker.Says(token, ciphertoken.Introspection{Active: false})
	revoked := time.Now()
	testwait.Receive(t, "the document socket whose token the broker revoked to close", peer.connection().Ended)
	closed := testwait.Until(t, "the server's own end of the document socket", rig.closedAt)
	requireClosedWithin(t, meter, "the document socket whose token the broker revoked", revoked, closed,
		ciphertoken.AffirmativeCacheTTL+2*heartbeat+heartbeatSlack, 3)
	if code := peer.connection().CloseCode(); code != gws.CloseAbnormalClosure {
		t.Fatalf("the document socket whose token the broker revoked closed with %d, want %d: the server dropping it", code, gws.CloseAbnormalClosure)
	}
	if rig.tokens.broker.Count() == asked {
		t.Fatal("the document socket closed with no introspection after the revocation, so its close says nothing of the broker's answer")
	}
}

// Both sockets ask one check of a broker-issued token on their heartbeats
// (server.brokerTokenHeartbeat), so a check a later change adds to it holds on the document socket
// as it does on GET /ws/live. The row stands that one check in, on the server register mounted,
// before the server takes a socket: the stand-in counts the requests of each socket it is asked
// about and answers what the real check answers (brokerTokenStillLive), until the row has it
// refuse. A live socket and a document socket, each opened with one token alone and no cookie,
// both ask the stand-in on their first heartbeat and outlive it; once it refuses, both close: the
// live socket with 1008, as a socket whose caller is refused does, and the document socket with no
// close frame (1006). The heartbeats are on the server's clock, a fake the row moves once both
// sockets have armed each one: both arming the third proves both first checks returned.
//
// Control: the document socket's heartbeat calling a copy of the check (brokerTokenStillLive)
// rather than the one the live socket calls asks the stand-in nothing, and the row fails once
// both first checks have returned.
func TestBothSocketsAskOneBrokerTokenHeartbeatCheck(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	rig := newDocumentSocketTokenRig(t, heartbeat, fake)
	var refusing atomic.Bool
	var liveAsked, documentAsked atomic.Int64
	rig.served.brokerTokenHeartbeat = func(r *http.Request) error {
		switch {
		case r.URL.Path == liveSocketPattern:
			liveAsked.Add(1)
		case strings.HasPrefix(r.URL.Path, "/ws/doc/"):
			documentAsked.Add(1)
		}
		if refusing.Load() {
			return errors.New("the stand-in for the broker-token check refuses the token")
		}
		return rig.served.brokerTokenStillLive(r)
	}
	caller := newBrokerCaller(t)
	const token = "a-token-holding-both-sockets-open"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	live := rig.openLiveSocket(t, caller, token)
	document := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))

	third := driveHeartbeats(t, fake, opened.Add(heartbeat), 2, heartbeat, 2)
	awaitArmedCountAt(t, fake, "both sockets' third heartbeat", third, 2)
	if liveAsked.Load() == 0 || documentAsked.Load() == 0 {
		t.Fatalf("on their first heartbeat the live socket asked the one broker-token check %d times and the document socket %d times, want each at least once: the two sockets do not ask one check",
			liveAsked.Load(), documentAsked.Load())
	}

	refusing.Store(true)
	fake.Set(third)
	if code, _ := live.closeCode(t); code != gws.ClosePolicyViolation {
		t.Fatalf("the live socket closed with %d once the one broker-token check refused, want %d", code, gws.ClosePolicyViolation)
	}
	testwait.Receive(t, "the document socket to close once the one broker-token check refused", document.connection().Ended)
	if code := document.connection().CloseCode(); code != gws.CloseAbnormalClosure {
		t.Fatalf("the document socket closed with %d once the one broker-token check refused, want %d", code, gws.CloseAbnormalClosure)
	}
}

// An emptied address bucket closes no token socket: the heartbeat asks the broker without spending
// the source address's introspection bucket (ciphertoken.Verifier.LiveUntil), so a stranger who
// empties the bucket of an address every client behind one load balancer shares closes no socket
// opened from it. The verifier's bucket holds one introspection, refilled one an hour of its
// clock. That clock counts the heartbeats of the server's clock, a fake the row moves: every read
// in one heartbeat gives the same time, and each heartbeat is ciphertoken.AffirmativeCacheTTL and
// a second past the one before, so no answer remembered at one heartbeat is still good at the
// next, each heartbeat asks the broker at least once (the second socket to check may take the
// first one's fresh answer), and the row's three heartbeats move that clock seconds, never an
// hour. A live socket and a document socket open with one live token; a live answer gives back
// what it took from the bucket (Verifier.known), so neither admission empties it. Then a token the
// broker does not hold live is presented from the same address, at the document socket's door: it
// is refused 401 BROKER_TOKEN_NOT_ACTIVE, and that refusal keeps the bucket's one introspection,
// so the bucket is empty. Both sockets stay open across three heartbeats, which ask the broker
// again, and then a fresh live token from the same address is refused 429
// BROKER_TOKEN_RATE_LIMITED: the bucket stayed empty while the heartbeats asked. Every request
// comes from the address the rig's server sees its sockets dial from.
//
// The row stands a counter in front of the heartbeat's check (brokerTokenStillLive), on the server
// register mounted, and moves the fake to a heartbeat only once both sockets have armed it and
// both checks of the heartbeat before it have found the token live. A socket arms its next
// heartbeat before it checks (driveHeartbeats), so a move made as soon as both had armed it could
// come while a check still ran; that check would then remember its answer at the next heartbeat's
// time, the next heartbeat would take that answer without asking the broker, and the count would
// fall short. Waiting for both checks keeps every read of one heartbeat at that heartbeat's time,
// so the count does not depend on how the two sockets' checks interleave.
//
// Control: the heartbeat spending the bucket finds it empty once the remembered answer has run
// out, both sockets close at their first heartbeat, and the row fails at its wait for both first
// checks to find the token live.
func TestATokenSocketOutlivesAnEmptiedAddressBucket(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	from := time.Now()
	rig := newDocumentSocketTokenRig(t, heartbeat, fake, brokerTokenOptions(func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Hour, Burst: 1}
		o.Now = func() time.Time {
			beats := fake.Now().Sub(opened) / heartbeat
			return from.Add(beats * (ciphertoken.AffirmativeCacheTTL + time.Second))
		}
	}))
	// stillLive counts the sockets' heartbeat checks that found the token live.
	var stillLive atomic.Int64
	rig.served.brokerTokenHeartbeat = func(r *http.Request) error {
		err := rig.served.brokerTokenStillLive(r)
		if err == nil {
			stillLive.Add(1)
		}
		return err
	}
	door := openedDoor(t, rig.server, rig.artifactID, rig.cookie)
	caller := newBrokerCaller(t)
	const token = "a-token-holding-both-sockets-open"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	live := rig.openLiveSocket(t, caller, token)
	document := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))

	emptying := door.knock(t, door.presenting(t, caller, "a-token-that-empties-the-bucket"))
	if emptying.opened || emptying.status != http.StatusUnauthorized || refusalCode([]byte(emptying.body)) != "BROKER_TOKEN_NOT_ACTIVE" {
		t.Fatalf("the token the broker does not hold live was answered %d %s, want 401 BROKER_TOKEN_NOT_ACTIVE", emptying.status, strings.TrimSpace(emptying.body))
	}
	asked := rig.tokens.broker.Count()

	for beat := 1; beat <= 3; beat++ {
		at := opened.Add(time.Duration(beat) * heartbeat)
		awaitArmedCountAt(t, fake, "both sockets' heartbeat", at, 2)
		fake.Set(at)
		testwait.Until(t, fmt.Sprintf("both sockets' checks at heartbeat %d to find the token live", beat), func() (int64, bool) {
			checked := stillLive.Load()
			return checked, checked >= int64(2*beat)
		})
	}
	if again := rig.tokens.broker.Count() - asked; again < 3 {
		t.Fatalf("over three heartbeats the sockets asked the broker %d times, want at least once a heartbeat: the row does not hold the bucket against the heartbeat's own introspections", again)
	}
	document.barrier(t)
	if !live.open() {
		t.Fatalf("the live socket ended (%v) with its token live and the address's bucket empty", live.err)
	}

	const fresh = "a-fresh-token-from-the-same-address"
	rig.tokens.broker.Says(fresh, liveNamedToken(caller.jkt))
	if answer := door.knock(t, door.presenting(t, caller, fresh)); answer.opened || answer.status != http.StatusTooManyRequests || refusalCode([]byte(answer.body)) != "BROKER_TOKEN_RATE_LIMITED" {
		t.Fatalf("a fresh live token from the same address was answered %d %s, want 429 BROKER_TOKEN_RATE_LIMITED: the bucket did not stay empty",
			answer.status, strings.TrimSpace(answer.body))
	}
}

// A schema switch closes every document socket its process holds, whatever admitted it
// (docs.Service.CloseDocumentSockets): a socket a broker-issued token opened and one a sign-in
// cookie opened, on one room, both close with the schema switch's close code, on which the client
// reads the document again and connects again, where admission decides afresh.
//
// Control: CloseDocumentSockets passing over a token socket leaves it open, closes one socket, and
// the row fails on the count.
func TestADocumentSocketClosesOnASchemaSwitchWhateverAdmittedIt(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	caller := newBrokerCaller(t)
	const token = "a-token-the-schema-switch-closes"
	rig.tokens.broker.Says(token, personScopedTokenAnswer(caller.jkt))
	byToken := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))
	byCookie := rig.openDocument(t, rig.artifactID, true, nil)

	if closed := rig.documents.CloseDocumentSockets(); closed != 2 {
		t.Fatalf("the schema switch closed %d document sockets, want both, the token's and the cookie's", closed)
	}
	for _, socket := range []struct {
		name string
		peer *syncedPeer
	}{{"the token's socket", byToken}, {"the cookie's socket", byCookie}} {
		testwait.Receive(t, socket.name+" to close on the schema switch", socket.peer.connection().Ended)
		if code := socket.peer.connection().CloseCode(); code != contracts.DocumentSchemaCloseCode {
			t.Errorf("%s closed with %d on the schema switch, want %d", socket.name, code, contracts.DocumentSchemaCloseCode)
		}
	}
}

// A writable token socket on the spec of an issue Legion runs stays writable only while the
// spec-writer rule still lets its token write (documentSocketStillResolves): its heartbeat judges
// the rule again, and once the rule refuses, the socket closes with 4412
// (contracts.DocumentAdmissionRevokedCloseCode), on which the provider reconnects and the new
// upgrade is admitted read-only. The issue and its role holders are the spec-writer rig's
// (specWriterServer.tree, publishRoles): R, labelled legion, whose architect and implementer are
// sessions of their own, and C, R's child, which a person then closes.
//
// The token of R's architect session opens R's spec read-write. Across two heartbeats the rule
// still passes it, and the socket's heartbeat reads the rule each time. Then R's architect row
// names another session, and the socket closes with 4412 at its first heartbeat after the change.
// The same token with a fresh proof opens R's spec again, read-only, and its edit is neither taken
// nor stored.
//
// On the same server, across the same heartbeats and one more after the change, four sockets the
// rule cannot close stay open, and their heartbeats read nothing of the rule, by the session and
// the issue of each read the traced pool runs (specWriterReads): the person's cookie socket on R's
// spec, which stays read-write and stores an edit after the change; the token socket of R's
// implementer session, which the rule opened read-only on R's spec; the architect's token socket
// on C's spec, which the rule lets write and the document service opens read-only because C is
// closed (docs.SocketCaller.Admitted); and a writable token socket on a project document that
// belongs to no issue. The heartbeats are on the server's clock, a fake the row moves once every
// open socket has armed each one: a socket that arms a heartbeat has returned from its check two
// heartbeats earlier with the socket open.
//
// Control: a heartbeat that leaves out the second step keeps the writable socket open and reads
// nothing of the rule, and the row fails once its first heartbeat has returned.
func TestAWritableTokenSocketClosesWhenTheSpecWriterRuleLaterRefusesIt(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	rig := newDocumentSocketTokenRig(t, heartbeat, fake)
	tree := (&specWriterServer{handler: rig.tokens.handler, database: rig.tokens.database, served: rig.tokens.served}).tree(t, specWriterRigShape{})
	tree.publishRoles(t)
	spec := tree.r.documents["spec.md"].id
	closedSpec := tree.c.documents["spec.md"].id
	if closed := dispatchRequest(t, rig.tokens.handler, http.MethodPatch, "/api/v1/issues/"+tree.c.key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("a person closes %s: status=%d body=%s", tree.c.key, closed.Code, closed.Body.String())
	}
	notes := uploadDocument(t, rig.tokens.handler, "/api/v1/projects/"+tree.project, "notes.md", "Notes that belong to no issue.\n").Artifact.ID
	// The documents were written through the broker rig's server, whose document service settles
	// them; the sockets below open on the rig's own server once nothing of them is left to settle.
	awaitSettled(t, rig.tokens.database, spec, closedSpec, notes)

	caller := newBrokerCaller(t)
	tokenOf := func(session string) string {
		value := "a-token-of-" + session
		answer := liveNamedToken(caller.jkt)
		answer.Session, answer.Projects = session, []string{tree.project}
		rig.tokens.broker.Says(value, answer)
		return value
	}
	architect := tokenOf(tree.rArchitect)
	writerOfNoIssue := "ses-a-writer-of-no-issue-" + strings.ToLower(tree.project)
	writable := rig.openDocument(t, spec, false, presentingAt(t, caller, spec, architect))
	person := rig.openDocument(t, spec, true, nil)
	implementer := rig.openDocument(t, spec, false, presentingAt(t, caller, spec, tokenOf(tree.rImplementer)))
	onClosedIssue := rig.openDocument(t, closedSpec, false, presentingAt(t, caller, closedSpec, architect))
	ofNoIssue := rig.openDocument(t, notes, false, presentingAt(t, caller, notes, tokenOf(writerOfNoIssue)))
	for _, socket := range []struct {
		name  string
		peer  *syncedPeer
		scope string
	}{
		{"the architect's token socket on R's spec", writable, "read-write"},
		{"the person's cookie socket on R's spec", person, "read-write"},
		{"the implementer's token socket on R's spec", implementer, "readonly"},
		{"the architect's token socket on the closed issue's spec", onClosedIssue, "readonly"},
		{"the token socket on a document of no issue", ofNoIssue, "read-write"},
	} {
		if scope := tokenMessageScope(t, socket.peer); scope != socket.scope {
			t.Fatalf("%s is answered %q, want %s", socket.name, scope, socket.scope)
		}
	}

	// Two heartbeats with the rule still passing the architect: every socket arming the third
	// proves every first check returned with its socket open, the writable socket's after it read
	// the rule.
	start := rig.specWriterReads.mark()
	ruled := judgedWrite{session: tree.rArchitect, issue: tree.r.key}
	third := driveHeartbeats(t, fake, opened.Add(heartbeat), 2, heartbeat, 5)
	awaitArmedCountAt(t, fake, "the five sockets' third heartbeat", third, 5)
	if _, answered := rig.specWriterReads.since(start, ruled); answered == 0 {
		t.Fatal("the writable socket's first heartbeat returned with the socket open having read nothing of the spec-writer rule: the heartbeat does not judge the rule again")
	}
	// The second heartbeat's check runs now. The architect moves once its read of the rule has
	// been answered, so the third heartbeat is the first to read the rule's new answer.
	testwait.Until(t, "the writable socket's second heartbeat's read of the spec-writer rule to be answered", func() (int, bool) {
		_, answered := rig.specWriterReads.since(start, ruled)
		return answered, answered >= 2
	})

	tree.publish(t, tree.r.key, []issueRoleHolder{
		{Role: "architect", Session: "ses-r-next-architect-" + strings.ToLower(tree.project)},
		{Role: "implementer", Session: tree.rImplementer},
	})
	fake.Set(third)
	testwait.Receive(t, "the writable socket to close at its first heartbeat after its session stopped being R's architect", writable.connection().Ended)
	if code := writable.connection().CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("the writable socket closed with %d once the spec-writer rule refused its token, want %d, on which the provider reconnects",
			code, contracts.DocumentAdmissionRevokedCloseCode)
	}

	// One more heartbeat: the four other sockets arming the fifth proves that each check they made
	// after the change returned with the socket open.
	fourth := third.Add(heartbeat)
	awaitArmedCountAt(t, fake, "the four other sockets' fourth heartbeat", fourth, 4)
	fake.Set(fourth)
	awaitArmedCountAt(t, fake, "the four other sockets' fifth heartbeat", fourth.Add(heartbeat), 4)
	started, _ := rig.specWriterReads.since(start, ruled)
	for _, read := range started {
		switch {
		case read.session == tree.rImplementer, read.session == writerOfNoIssue, read.session == documentSocketPerson,
			read == (judgedWrite{session: tree.rArchitect, issue: tree.c.key}):
			t.Errorf("a heartbeat read the spec-writer rule for session %s on %s: a socket the rule cannot close later reads nothing of it", read.session, read.issue)
		}
	}
	requireEditStored(t, rig.tokens.database, person, spec, "Typed by the person once R's architect moved.")
	for _, open := range []*syncedPeer{implementer, onClosedIssue, ofNoIssue} {
		open.barrier(t)
	}

	reopened := rig.openDocument(t, spec, false, presentingAt(t, caller, spec, architect))
	if scope := tokenMessageScope(t, reopened); scope != "readonly" {
		t.Fatalf("the former architect's token reconnecting to R's spec is answered %q, want readonly", scope)
	}
	requireEditDropped(t, rig.tokens.database, reopened, spec, fmt.Sprintf("Typed under %s's token once it stopped being R's architect.", tree.rArchitect))
}
