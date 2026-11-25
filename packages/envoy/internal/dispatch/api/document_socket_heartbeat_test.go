package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// documentSocketTokenRig is a server whose people sign in with a session cookie, as
// newCookieServerOn's do, and whose agents present tokens the secrets broker issued, as
// newBrokerTokenRig's do: the one server where a request can carry both, a cookie and a token
// beside it. The API and the document service share one cookie identity, as cmd/dispatch hands
// them one.
type documentSocketTokenRig struct {
	// tokens is the rig the broker-token verifier comes from: its stub broker answers what a test
	// says a token is, and its deps.BrokerTokens is the verifier this server decides tokens with.
	// Its own server is never sent a request.
	tokens   *brokerTokenRig
	sessions *store.PgSessionStore
	// limiter is what the server admits by, whose share outside admission the heartbeat's
	// checks take their places of (admission.Outside).
	limiter *admission.Limiter
	// logins is the cookie identity the document service shares, counting the lookups made
	// through it (heartbeatLogins).
	logins *heartbeatLogins
	// checks is the API's identity, over logins, counting the resolutions the API asks of it
	// (busyCallerIdentity, never set busy here): after the setup's requests, the sockets'
	// heartbeat checks (signInStillResolves).
	checks *busyCallerIdentity
	// heartbeats records the calls a socket's heartbeat makes into the product, once a test
	// starts it (heartbeatCalls): the API's lookups of the cookie, and the verifier's clock
	// reads and introspections (timeBrokerCalls).
	heartbeats *heartbeatCalls
	// spans records, for each connection the server accepts, when the server itself ended it
	// (servedSpans): the record of the socket's close a test reads.
	spans      *servedSpans
	cookie     *http.Cookie
	server     *httptest.Server
	sockets    *servedSockets
	artifactID string
}

// documentSocketPerson is the person signed in on a documentSocketTokenRig.
const documentSocketPerson = "alice@d.example"

// newDocumentSocketTokenRig signs documentSocketPerson in with a session cookie and gives her an
// issue whose spec she can open, on a server whose sockets resolve their caller every heartbeat.
// The verifier is newBrokerTokenRig's, taken from that rig rather than built a second time, so it
// is the one every broker-token test drives: the same stub broker, the replay table on this same
// database, and the switches cmd/dispatch sets on it.
func newDocumentSocketTokenRig(t *testing.T, heartbeat time.Duration) *documentSocketTokenRig {
	t.Helper()
	ctx := context.Background()
	rig := &documentSocketTokenRig{heartbeats: &heartbeatCalls{}, limiter: admission.New(admission.DefaultLimits())}
	rig.tokens = newBrokerTokenRig(t, timeBrokerCalls(t, rig.heartbeats))
	database := rig.tokens.database
	rig.sessions = store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions}
	rig.logins = &heartbeatLogins{Identity: person, calls: rig.heartbeats}
	rig.checks = &busyCallerIdentity{Identity: rig.logins}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: rig.checks, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: testServerURL, Docs: documents, Events: broker,
		BrokerTokens: rig.tokens.deps.BrokerTokens, StreamHeartbeat: heartbeat, Admission: rig.limiter,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
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

// heartbeatLogins is an Identity that records the lookups made through it as heartbeat calls
// (heartbeatCalls.begin) and counts those that start once the test has said the cookie's session
// ended (end): on a documentSocketTokenRig, the API's, whose only caller once the setup's requests
// have answered is the document socket's heartbeat (signInStillResolves). A lookup counts when it
// starts after end, so one already under way when the session ended, which may find the session
// either way, is not counted.
type heartbeatLogins struct {
	identity.Identity
	calls    *heartbeatCalls
	ended    atomic.Bool
	afterEnd atomic.Int64
}

func (c *heartbeatLogins) Login(r *http.Request) (string, error) {
	done := c.calls.begin()
	if c.ended.Load() {
		c.afterEnd.Add(1)
	}
	email, err := c.Identity.Login(r)
	done(err != nil)
	return email, err
}

// end marks the cookie's session ended: every lookup that starts from now on is counted.
func (c *heartbeatLogins) end() { c.ended.Store(true) }

// heartbeatSlack is what scheduling may add to a heartbeat beside the longest single stall a stall
// meter saw: how much later than a heartbeat after the last call returned the next call of a
// socket's heartbeat may start, once that stall is allowed too (gapWithin).
const heartbeatSlack = time.Second

// heartbeatCalls records, once the test starts it (record), each call a socket's heartbeat makes
// into the product: when it started, when it returned, and whether its answer refused the caller.
// The calls are the product's own - the API Identity's lookups on a document socket
// (heartbeatLogins), the broker-token verifier's clock reads and introspections on a live socket
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
// clock (Options.Now), which every Live call makes first (Verifier.known), as a call that returns
// at once, and each introspection, sent through a recording proxy in front of the broker, as a
// call from its request to its answer, which refused when the broker answered the token not live.
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

// openDocument opens the spec's document socket as the dashboard's editor does, with the
// person's cookie and every header of beside on the upgrade, and closes it when the test ends.
func (rig *documentSocketTokenRig) openDocument(t *testing.T, beside http.Header) *syncedPeer {
	t.Helper()
	headers := beside.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Cookie", rig.cookie.Name+"="+rig.cookie.Value)
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + rig.artifactID, sockets: rig.sockets,
		headers: headers, artifactID: rig.artifactID, doc: crdt.New(),
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

// The document socket admits its caller by the sign-in cookie alone (docs.Service.requestActor),
// so its heartbeat re-checks that cookie and nothing the request carries beside it
// (signInStillResolves). A token sent beside the cookie admitted nothing, and the broker holding
// it live must not keep the socket open once the cookie's session has ended: not a read-only
// token, which may read a document through the API's reads and never through this socket, and
// not a token that writes, which is its session's credential and still not what opened this
// socket. The socket stays open while the cookie resolves, with the token beside it, and closes
// at the first heartbeat after the session's end, while the broker still holds the token live and
// has not once been asked about it. "The first" is read off the heartbeat's own calls, its lookups
// of the cookie (heartbeatLogins): no lookup that started once the session had ended may find it
// still resolving, as one served from a cache of the session's state would, and each lookup starts
// within a heartbeat and heartbeatSlack of the return of the one before it, plus the longest stall
// the process saw in that gap (heartbeatCalls), so a heartbeat that skips a tick, keeps an answer
// it stored past a beat or asks on a ticker of its own slower than that fails too. The server ends
// the socket within a heartbeat of the lookup that refused returning, by the server's own record of
// its close, allowing the longest stall between them for the one goroutine wake there, the
// socket's on the heartbeat's cancel (requireClosedAfterRefusal, one hop), so a product that waits
// before it closes a refused socket fails too. The heartbeat is two seconds, more than
// heartbeatSlack, so skipping a tick is told apart from a late tick.
func TestADocumentSocketClosesOnceItsCookieEndsThoughALiveTokenRidesBesideIt(t *testing.T) {
	t.Parallel()
	const heartbeat = 2 * time.Second
	for _, beside := range []struct {
		name   string
		answer func(jkt string) ciphertoken.Introspection
	}{
		{"a read-only token", readOnlyToken},
		{"a token that writes", liveNamedToken},
	} {
		t.Run(beside.name, func(t *testing.T) {
			meter := startStallMeter(t)
			rig := newDocumentSocketTokenRig(t, heartbeat)
			caller := newBrokerCaller(t)
			const token = "a-token-sent-beside-the-cookie"
			rig.tokens.broker.Says(token, beside.answer(caller.jkt))
			presented := caller.presenting(t, http.MethodGet, "/ws/doc/"+rig.artifactID, token, nil)
			peer := rig.openDocument(t, presentedHeaders(presented))
			rig.heartbeats.record()

			// While the cookie resolves the socket outlives several heartbeats, each of which
			// looks the cookie up again, and still answers its editor.
			rig.heartbeats.await(t, meter, heartbeat, "three heartbeat lookups of the cookie while it resolves", func() bool {
				return rig.heartbeats.count() >= 3
			})
			peer.barrier(t)

			if err := rig.sessions.RevokeSessions(context.Background(), documentSocketPerson); err != nil {
				t.Fatalf("sign out: %v", err)
			}
			rig.logins.end()
			ended := time.Now()
			rig.heartbeats.await(t, meter, heartbeat, "the document socket's end once its cookie's session ended, with "+beside.name+" the broker holds live beside the cookie", func() bool {
				select {
				case <-peer.connection().Ended:
					return true
				default:
					return false
				}
			})
			lookups := rig.logins.afterEnd.Load()
			t.Logf("the document socket closed %s after its cookie's session ended, after %d lookups that started once it had (heartbeat %s)", time.Since(ended).Round(time.Millisecond), lookups, heartbeat)
			if lookups > 1 {
				t.Fatalf("the document socket closed after %d heartbeat lookups of its cookie that started once its session had ended, want the first to close it: a heartbeat found the ended session still resolving", lookups)
			}
			rig.heartbeats.requireEveryHeartbeat(t, meter, heartbeat)
			rig.requireClosedAfterRefusal(t, meter, "the document socket", heartbeat, 1)
			// The heartbeat cancels the connection's context with no close code, so ygo drops
			// the socket without a close frame.
			if code := peer.connection().CloseCode(); code != gws.CloseAbnormalClosure {
				t.Fatalf("the document socket closed with %d, want %d: the server dropping it", code, gws.CloseAbnormalClosure)
			}
			// The close is the cookie's: nothing asked the broker about the token, and the
			// broker holds it live still.
			if asked := rig.tokens.broker.Count(); asked != 0 {
				t.Fatalf("the document socket's heartbeat asked the broker %d times about a token it was not admitted by, want 0", asked)
			}
			if _, err := rig.tokens.deps.BrokerTokens.Live(context.Background(), presented); err != nil {
				t.Fatalf("the token beside the cookie no longer resolves (%v), so the close says nothing of the cookie", err)
			}
		})
	}
}

// A document socket whose cookie still resolves stays open across its heartbeats, each of which
// re-checks that cookie: opened with the cookie alone, and opened with a token beside it that the
// broker does not hold live at all, which the heartbeat never reads and the broker is never asked
// about.
func TestADocumentSocketOutlivesItsHeartbeatsWhileItsCookieResolves(t *testing.T) {
	t.Parallel()
	const heartbeat = 250 * time.Millisecond
	for _, beside := range []struct {
		name    string
		headers func(*testing.T, *documentSocketTokenRig) http.Header
	}{
		{"the cookie alone", func(*testing.T, *documentSocketTokenRig) http.Header { return nil }},
		{"the cookie beside a token the broker does not hold live", func(t *testing.T, rig *documentSocketTokenRig) http.Header {
			return presentedHeaders(newBrokerCaller(t).presenting(t, http.MethodGet, "/ws/doc/"+rig.artifactID, "a-token-the-broker-never-issued", nil))
		}},
	} {
		t.Run(beside.name, func(t *testing.T) {
			rig := newDocumentSocketTokenRig(t, heartbeat)
			peer := rig.openDocument(t, beside.headers(t, rig))

			time.Sleep(4 * heartbeat)
			select {
			case <-peer.connection().Ended:
				t.Fatalf("the document socket closed (code %d) within four heartbeats while its cookie resolved", peer.connection().CloseCode())
			default:
			}
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
// tick is told apart from a late tick, and the client's pong meets the pong deadline on a loaded
// runner.
func TestALiveSocketOpenedByABrokerTokenResolvesThatTokenOnEveryHeartbeat(t *testing.T) {
	t.Parallel()
	meter := startStallMeter(t)
	const heartbeat = 2 * time.Second
	rig := newDocumentSocketTokenRig(t, heartbeat)
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

// A logout closes the document socket at the first heartbeat whose check finds the sign-in gone,
// while the request pool is free: a refusal is no transient failure, so that check closes the
// socket there and then rather than checking twice more (heartbeatTransientCeiling). A check in
// flight as the session ends may still find it, so the close comes within two checks of the
// logout.
func TestALogoutClosesTheDocumentSocketAtItsNextHeartbeat(t *testing.T) {
	t.Parallel()
	const heartbeat = 250 * time.Millisecond
	rig := newDocumentSocketTokenRig(t, heartbeat)
	peer := rig.openDocument(t, nil)
	time.Sleep(2 * heartbeat)
	peer.barrier(t)

	checked := rig.checks.asked.Load()
	if err := rig.sessions.RevokeSessions(context.Background(), documentSocketPerson); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	select {
	case <-peer.connection().Ended:
	case <-time.After(3*heartbeat + 2*time.Second):
		t.Fatalf("the document socket stayed open after its sign-in ended, %d checks since", rig.checks.asked.Load()-checked)
	}
	if checks := rig.checks.asked.Load() - checked; checks > 2 {
		t.Fatalf("the document socket closed after %d checks of its caller since the logout, want it closed by the first check that found the sign-in gone", checks)
	}
}

// A document socket's heartbeat check reads the request pool, for the caller's session, so it
// takes a place of the pool's share outside admission first (signInStillResolves), and while every
// place is held the check waits for one, reading nothing: a sign-in that ended meanwhile is not
// found out yet, and the socket stays open. A place given back while the check waits is that
// check's, which then finds the sign-in gone and closes the socket at once. A check no place comes
// free for is skipped, and counts toward the heartbeat's ceiling
// (TestAHeartbeatOutlivesTwoTransientFailuresAndClosesOnTheThird).
func TestADocumentSocketsHeartbeatWaitsForAPlaceOfTheShareOutsideAdmission(t *testing.T) {
	t.Parallel()
	const heartbeat = 250 * time.Millisecond
	rig := newDocumentSocketTokenRig(t, heartbeat)
	peer := rig.openDocument(t, nil)
	time.Sleep(2 * heartbeat)
	peer.barrier(t)

	outside := rig.limiter.Outside()
	release := holdTheShareOutsideAdmission(t, outside)
	defer release()
	checked := rig.checks.asked.Load()
	if err := rig.sessions.RevokeSessions(context.Background(), documentSocketPerson); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	waitUntil(t, "a heartbeat check waiting for a place", func() bool {
		return outside.Stats().Queued == 1
	})
	select {
	case <-peer.connection().Ended:
		t.Fatal("the document socket closed while every place of the share outside admission was held: its heartbeat read the request pool beside them")
	default:
	}
	if checks := rig.checks.asked.Load() - checked; checks != 0 {
		t.Fatalf("the heartbeat checked the caller %d times while every place was held, want none", checks)
	}

	release()
	select {
	case <-peer.connection().Ended:
	case <-time.After(2*heartbeat + 2*time.Second):
		t.Fatal("the document socket stayed open once a place was given to its waiting check and its sign-in had ended")
	}
	// The waiting check's, unless the heartbeat's bound ended it as it took the place, when the
	// next heartbeat's check closes the socket instead.
	if checks := rig.checks.asked.Load() - checked; checks < 1 || checks > 2 {
		t.Fatalf("the document socket closed after %d checks of its caller once a place was given back, want the waiting check's", checks)
	}
	if refused := outside.Stats().Refused[admission.HeartbeatCheck]; refused != (admission.OutsideRefusals{}) {
		t.Fatalf("heartbeat checks refused a place %+v, want none: the waiting one was given the place given back", refused)
	}
}
