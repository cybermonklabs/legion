package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// endableIdentity names one person until their session ends.
type endableIdentity struct{ ended atomic.Bool }

func (i *endableIdentity) Login(*http.Request) (string, error) {
	if i.ended.Load() {
		return "", identity.ErrNoIdentity
	}
	return "alice@d.example", nil
}

// lookupHeartbeat is the heartbeat of the servers whose sockets close once their caller ends: two
// seconds, a second more than the slack a closing lookup's start is allowed (heartbeatSlack), so a
// heartbeat that skips a tick or keeps an answer past a beat starts its closing lookup a whole
// heartbeat late, past the heartbeat and the slack unless the process stalled that second away
// meanwhile. It is long enough, too, for the live socket's page to meet its pong deadline (two
// heartbeats) on a loaded runner.
const lookupHeartbeat = 2 * time.Second

// heartbeatLookups is the API's Identity with the lookups a socket's heartbeat makes recorded. A
// socket's heartbeat (whileCallerResolves) asks its lookup again on every StreamHeartbeat: the live
// socket's callerStillResolves and the document socket's signInStillResolves, each of which, for a
// caller with no bearer, is one Login of the API's Identity. end runs the change that ends the
// caller under the lock every Login takes, so each Login is wholly before the end or wholly after
// it.
//
// A socket that closes on its first lookup after the end has made exactly one Login after it, and
// that Login began within a heartbeat of the end of the Login before it: the heartbeat's ticker
// fires once a heartbeat, and keeps a tick waiting while a lookup runs long, so the next lookup
// starts as soon as the one before it returns. The gap is read off the product's own calls, from
// the return of the last Login before the end to the start of the first after it, never off the
// test's clock around them, and is allowed the heartbeat, heartbeatSlack for scheduling, and the
// longest single stall the process saw meanwhile (requireGapWithin), the delay of the one goroutine
// wake inside it: the heartbeat's own, on its tick. A refused Login the lookup or the heartbeat let
// pass is a second Login after the end. A heartbeat that skips a tick, answers from a stored answer
// past a beat, or asks on a ticker of its own slower than the heartbeat and the slack leaves a gap
// past what is allowed; a ticker of its own no slower than that, with the stall, passes, since the
// check cannot tell it from a scheduling delay.
//
// The refusal then closes the socket within a heartbeat: from the return of the Login that refused,
// stamped on the heartbeat's goroutine, to the server's own end of the connection (servedSpans: the
// start of its close frame, or its close of the connection), made by the socket's goroutine that
// the heartbeat's cancel wakes. One goroutine wake lies between the two records, so the span is
// allowed the heartbeat and one longest stall (requireClosedWithin, one hop): a server that waits on
// its own after the refusal fails, and a machine that stops the process meanwhile does not.
type heartbeatLookups struct {
	identity.Identity
	heartbeat time.Duration

	mu     sync.Mutex
	logins int
	ended  bool
	after  int
	// returned is when the last Login before the end returned, closing when the first Login after
	// it began, and refused when that Login returned: each is the product's call, timed at the
	// Identity it asks.
	returned, closing, refused time.Time
}

// newHeartbeatLookups records the Logins of inner that a socket whose heartbeat is heartbeat asks.
func newHeartbeatLookups(inner identity.Identity, heartbeat time.Duration) *heartbeatLookups {
	return &heartbeatLookups{Identity: inner, heartbeat: heartbeat}
}

func (l *heartbeatLookups) Login(r *http.Request) (string, error) {
	began := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logins++
	login, err := l.Identity.Login(r)
	switch {
	case !l.ended:
		l.returned = time.Now()
	case l.after == 0:
		l.closing, l.refused = began, time.Now()
		l.after++
	default:
		l.after++
	}
	return login, err
}

// count is how many Logins the Identity has been asked so far.
func (l *heartbeatLookups) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logins
}

// end runs ending, which ends the caller, and records every Login after it apart.
func (l *heartbeatLookups) end(ending func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ending()
	l.ended = true
}

// requireClosedAtTheFirstLookup fails the test unless socket, which closed at closedAt by its
// server's own record, did so on its first heartbeat lookup after the end: exactly one Login came
// after it, it began within the heartbeat and heartbeatSlack of the return of the Login before it,
// and the server ended the connection within a heartbeat of that Login's return, each span allowing
// the longest single stall m saw inside it once (one goroutine wake in each).
func (l *heartbeatLookups) requireClosedAtTheFirstLookup(t *testing.T, m *stallMeter, socket string, closedAt time.Time) {
	t.Helper()
	l.mu.Lock()
	after, returned, closing, refused := l.after, l.returned, l.closing, l.refused
	l.mu.Unlock()
	if after != 1 {
		t.Fatalf("%s closed after %d lookups of its caller once the caller had ended, want it closed on the first", socket, after)
	}
	if returned.IsZero() {
		t.Fatalf("%s's heartbeat made no lookup of its caller before the end, so the lookup that closed it has nothing to be timed from", socket)
	}
	requireGapWithin(t, m, socket+"'s heartbeat did not ask on every beat: from the return of its last lookup before the end to the start of the one after it", returned, closing, l.heartbeat+heartbeatSlack)
	requireClosedWithin(t, m, socket+"'s end by its server after the lookup that refused its caller", refused, closedAt, l.heartbeat, 1)
}

// acceptedConns is how many connections the server behind spans has accepted so far: the index the
// next one it accepts records its span under.
func acceptedConns(spans *servedSpans) int {
	spans.mu.Lock()
	defer spans.mu.Unlock()
	return len(spans.conns)
}

// serverEndOf is when the server ended the nth connection it accepted, by its own calls on that
// connection (servedSpans): the start of its close frame, else its close of the connection. It
// fails the test when the server has done neither.
func serverEndOf(t *testing.T, spans *servedSpans, n int) time.Time {
	t.Helper()
	spans.mu.Lock()
	defer spans.mu.Unlock()
	if n < len(spans.conns) {
		switch conn := spans.conns[n]; {
		case !conn.closeFrame.IsZero():
			return conn.closeFrame
		case !conn.closed.IsZero():
			return conn.closed
		}
	}
	t.Fatalf("the server has not ended connection %d it accepted (of %d): no close frame, no close", n, len(spans.conns))
	return time.Time{}
}

// spannedServer serves handler on a test server whose every accepted connection records the
// server's own calls on it (servedSpans), so a row reads when the server ended a socket.
func spannedServer(t *testing.T, handler http.Handler) (*httptest.Server, *servedSpans) {
	t.Helper()
	spans := &servedSpans{}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = spans.listen(server.Listener)
	server.Start()
	t.Cleanup(server.Close)
	return server, spans
}

// A person whose session ends (logout, a refresh the sign-in pool refuses, a lost group) stops
// receiving the event feed they subscribed to while signed in: the live socket resolves them again
// on each heartbeat (whileCallerResolves) and closes with 1008 on the first of those lookups after
// they no longer resolve, read off the lookups themselves (heartbeatLookups). The heartbeat is
// lookupHeartbeat, so the socket lasts until the session ends.
func TestTheEventFeedClosesOnceItsPersonNoLongerResolves(t *testing.T) {
	t.Parallel()
	m := startStallMeter(t)
	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	person := &endableIdentity{}
	lookups := newHeartbeatLookups(person, lookupHeartbeat)
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: lookups, AgentTokens: sharedAgentTokens(t, "agent-token"), Docs: documents, Events: broker,
		StreamHeartbeat: lookupHeartbeat,
		// Its horizon warns as every test server's does (newRegisteredTestServer).
		HorizonStallAfter: horizontest.StallWarning, HorizonWarnEvery: horizontest.StallWarning,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	server, spans := spannedServer(t, mux)

	socketConn := acceptedConns(spans)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", nil).servedBy(database).read()
	subscribeEvents(t, client, "events", nil)
	// The end follows a lookup of the heartbeat's own, so the lookup that closes the socket is
	// timed against the one before it rather than against the upgrade.
	subscribed := lookups.count()
	waitUntil(t, "a heartbeat lookup of the person while they resolve", func() bool { return lookups.count() > subscribed })

	lookups.end(func() { person.ended.Store(true) })
	if closed := client.closeError(t); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the event feed's socket closed with %d %q once its person stopped resolving, want 1008", closed.Code, closed.Text)
	}
	lookups.requireClosedAtTheFirstLookup(t, m, "the event feed's socket", serverEndOf(t, spans, socketConn))
}

// The same holds for a human watching an agent's live conversation: the socket that carries the
// agent view resolves its person on each heartbeat and closes with 1008 on the first of those
// lookups after their session has ended, so the view stops relaying.
func TestAgentStreamClosesOnceItsViewerNoLongerResolves(t *testing.T) {
	t.Parallel()
	m := startStallMeter(t)
	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	person := &endableIdentity{}
	lookups := newHeartbeatLookups(person, lookupHeartbeat)
	source := agentstream.NewMemory()
	source.SetResponder(plannerSessionID, true)
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: lookups, AgentTokens: sharedAgentTokens(t, "agent-token"), Docs: documents, Events: broker,
		AgentStream: source, StreamHeartbeat: lookupHeartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	server, spans := spannedServer(t, mux)

	socketConn := acceptedConns(spans)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", nil).read()
	client.subscribeAgent(t, "agent", plannerSessionID)
	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":true}` {
		t.Fatalf("the agent view's first frame has data %s, want the session responding", data)
	}
	subscribed := lookups.count()
	waitUntil(t, "a heartbeat lookup of the viewer while they resolve", func() bool { return lookups.count() > subscribed })

	lookups.end(func() { person.ended.Store(true) })
	if closed := client.closeError(t); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the agent view's socket closed with %d %q once its viewer stopped resolving, want 1008", closed.Code, closed.Text)
	}
	lookups.requireClosedAtTheFirstLookup(t, m, "the agent view's socket", serverEndOf(t, spans, socketConn))
	waitUntil(t, "the closed socket's view leaves the session's frames", func() bool { return source.Viewers(plannerSessionID) == 0 })
}

// A person whose session ends while their editor holds a document open loses the live document
// too: the document socket resolves them again on every heartbeat, as the live socket does, and
// closes on the first of those lookups after they no longer resolve, so the editor neither
// receives the document's edits nor sends its own. Each case ends the session the way it ends in
// production: the hourly membership check finding the person out of the group, and a sign-out.
func TestDocumentSocketClosesOnceItsPersonNoLongerResolves(t *testing.T) {
	t.Parallel()
	const heartbeat = lookupHeartbeat
	for name, end := range map[string]func(*testing.T, *documentSocketRig){
		"the sign-in pool drops them from the group": func(t *testing.T, rig *documentSocketRig) {
			// The pool drops the group before the confirmation lapses, so the first refresh after
			// the hour is the one that finds the person outside it.
			rig.pool.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
			if _, found, err := rig.people.Membership(context.Background(), rig.email); err != nil || !found {
				t.Fatalf("the member's membership: found %t, %v", found, err)
			}
			if _, err := rig.database.Pool.Exec(context.Background(), `update people set confirmed_at = now() - interval '2 hours' where email = $1`, rig.email); err != nil {
				t.Fatalf("date the confirmation two hours back: %v", err)
			}
		},
		"they sign out": func(t *testing.T, rig *documentSocketRig) {
			if err := rig.sessions.RevokeSessions(context.Background(), rig.email); err != nil {
				t.Fatalf("sign out: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := startStallMeter(t)
			rig := newDocumentSocketRig(t, heartbeat)
			peer := &syncedPeer{
				wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + rig.artifactID, sockets: rig.sockets,
				headers: http.Header{"Cookie": []string{rig.cookie.Name + "=" + rig.cookie.Value}}, artifactID: rig.artifactID, doc: crdt.New(),
			}
			before := rig.lookups.count()
			socketConn := acceptedConns(rig.spans)
			peer.connect(t)
			t.Cleanup(peer.close)

			// While they are a member the socket outlives several heartbeats, each resolving them
			// again (three of its lookups, counted at the Identity they ask), and stays writable:
			// their edit reaches the room.
			waitUntil(t, "three heartbeat lookups of the member while they are one", func() bool { return rig.lookups.count()-before >= 3 })
			peer.appendParagraph(t, "typed while a member")
			peer.barrier(t)
			if refreshes := rig.pool.Refreshes(); refreshes != 0 {
				t.Fatalf("the sign-in pool was asked %d times within the hour, want 0", refreshes)
			}

			ended := time.Now()
			rig.lookups.end(func() { end(t, rig) })
			testwait.Receive(t, "the document socket's close once its person's session ended", peer.connection().Ended)
			rig.lookups.requireClosedAtTheFirstLookup(t, m, "the document socket", serverEndOf(t, rig.spans, socketConn))
			t.Logf("the document socket closed %s after the session ended (heartbeat %s)", time.Since(ended).Round(time.Millisecond), heartbeat)
			if generation, _, err := rig.sessions.CurrentSessionGeneration(context.Background(), rig.email); err != nil || generation != rig.generation+1 {
				t.Fatalf("session generation after the session ended = %d (%v), want %d", generation, err, rig.generation+1)
			}

			// The document goes on without them: an edit after the close reaches the room and not
			// their editor, and their editor cannot reconnect.
			if _, err := replaceDocumentText(rig.database, rig.documents, rig.artifactID, "written after the session ended", model.Actor{Kind: "user", ID: "bob@d.example"}); err != nil {
				t.Fatalf("edit the document after the close: %v", err)
			}
			if text, err := rig.documents.Text(context.Background(), rig.artifactID); err != nil || !strings.Contains(text, "written after the session ended") {
				t.Fatalf("room text after the edit = %q (%v)", text, err)
			}
			peer.assertTextLacks(t, "written after the session ended")
			connection, response, err := dialLive(t, gws.DefaultDialer, peer.wsURL, peer.headers)
			if connection != nil {
				_ = connection.Close()
			}
			if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("reconnect after the session ended: response=%#v err=%v, want a 401 handshake", response, err)
			}
		})
	}
}

// documentSocketRig is the server main serves a person signed in through a sign-in pool: cookie
// identity confirming membership with the pool, and the API and document socket Register mounts.
// lookups is the API's Identity, which the document socket's heartbeat asks (signInStillResolves);
// the document service keeps the bare one, so only the heartbeat's lookups and the API's own
// requests are counted. spans records the server's own calls on each connection it accepts, its
// end of a socket among them.
type documentSocketRig struct {
	database   *store.Store
	pool       *oidctest.Issuer
	people     *store.PgPeopleStore
	sessions   *store.PgSessionStore
	documents  *docs.Service
	lookups    *heartbeatLookups
	server     *httptest.Server
	spans      *servedSpans
	sockets    *servedSockets
	email      string
	generation int64
	cookie     *http.Cookie
	artifactID string
}

// newDocumentSocketRig signs alice@d.example in through a fake pool as a member of
// dispatch-members, as the sign-in callback does, and gives her an issue whose spec she can open.
func newDocumentSocketRig(t *testing.T, heartbeat time.Duration) *documentSocketRig {
	t.Helper()
	ctx := context.Background()
	rig := &documentSocketRig{pool: oidctest.New(t), email: "alice@d.example"}
	const redirect = "https://dispatch.example/auth/callback"
	rig.pool.EnableCodeFlow(rig.pool.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret", redirect)
	flow := oidc.NewCodeFlow(rig.pool.URL(), "dispatch-client", "dispatch-secret", "")
	rig.pool.SignInAs(map[string]any{
		"sub":              "person-subject",
		"cognito:username": "ExampleIdP_" + rig.email,
		"cognito:groups":   []string{"dispatch-members"},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})
	authURL, err := flow.AuthURL(ctx, redirect, "state", "nonce")
	if err != nil {
		t.Fatalf("start the sign-in: %v", err)
	}
	code, _ := rig.pool.Authorize(t, authURL)
	signIn, err := flow.Exchange(ctx, redirect, code, "nonce")
	if err != nil {
		t.Fatalf("exchange the sign-in code: %v", err)
	}

	database := storetest.Open(t)
	rig.database = database
	rig.people = store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	rig.sessions = store.NewPgSessionStore(database.Pool)
	if err := rig.people.SignIn(ctx, rig.email, signIn.RefreshToken); err != nil {
		t.Fatalf("record the sign-in: %v", err)
	}
	if rig.generation, err = rig.sessions.EnsureSession(ctx, rig.email); err != nil {
		t.Fatalf("establish the session: %v", err)
	}
	if rig.cookie, err = http.ParseSetCookie(auth.IssueSessionCookie(rig.email, rig.generation, testSigningKey, false)); err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	person := identity.CookieIdentity{
		SigningKey: testSigningKey,
		Sessions:   rig.sessions,
		Membership: &identity.Membership{People: rig.people, Sessions: rig.sessions, SignIn: flow, Rule: identity.GroupRule{Group: "dispatch-members"}},
	}
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = rig.documents.Shutdown(context.Background()) })
	rig.lookups = newHeartbeatLookups(person, heartbeat)
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: rig.lookups, AgentTokens: sharedAgentTokens(t, "agent-token"), Docs: rig.documents, Events: broker,
		StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.server, rig.spans = spannedServer(t, rig.sockets.serve(mux.ServeHTTP))

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

// A person whose confirmation has lapsed while the sign-in issuer cannot answer is refused 503
// SIGNIN_UNAVAILABLE by the API and by the document socket's handshake, never 401 or 500, and stays
// signed in: their session generation and refresh token are kept, and once the issuer answers the
// same cookie is served again after one refresh.
func TestASignInIssuerOutageAnswersTheAPIAndTheDocumentSocket503AndKeepsTheSession(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketRig(t, time.Hour)
	ctx := context.Background()
	signedIn, found, err := rig.people.Membership(ctx, rig.email)
	if err != nil || !found {
		t.Fatalf("the member's membership: found %t, %v", found, err)
	}
	if _, err := rig.database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, rig.email); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
	rig.pool.FailToken(http.StatusServiceUnavailable, "")

	whoami := func() (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, rig.server.URL+"/api/v1/whoami", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(rig.cookie)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /api/v1/whoami: %v", err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	socket := func() (int, string) {
		t.Helper()
		header := http.Header{"Cookie": []string{rig.cookie.Name + "=" + rig.cookie.Value}}
		connection, response, err := dialLive(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(rig.server.URL, "http")+"/ws/doc/"+rig.artifactID, header)
		if connection != nil {
			_ = connection.Close()
		}
		if response == nil {
			t.Fatalf("the document socket's handshake got no answer: %v", err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	if status, body := whoami(); status != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"SIGNIN_UNAVAILABLE"`) {
		t.Fatalf("GET /api/v1/whoami while the issuer answers 503: %d %s, want 503 SIGNIN_UNAVAILABLE", status, body)
	}
	if status, body := socket(); status != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"SIGNIN_UNAVAILABLE"`) {
		t.Fatalf("the document socket's handshake while the issuer answers 503: %d %s, want 503 SIGNIN_UNAVAILABLE", status, body)
	}
	if generation, _, err := rig.sessions.CurrentSessionGeneration(ctx, rig.email); err != nil || generation != rig.generation {
		t.Fatalf("session generation during the outage = %d (%v), want %d", generation, err, rig.generation)
	}
	if kept, _, err := rig.people.Membership(ctx, rig.email); err != nil || kept.RefreshToken != signedIn.RefreshToken {
		t.Fatalf("the membership during the outage kept its refresh token: %t (%v), want true", kept.RefreshToken == signedIn.RefreshToken, err)
	}

	rig.pool.FailToken(0, "")
	if status, body := whoami(); status != http.StatusOK || !strings.Contains(body, rig.email) {
		t.Fatalf("GET /api/v1/whoami once the issuer answers: %d %s, want 200 naming %s", status, body, rig.email)
	}
	if status, body := socket(); status != http.StatusSwitchingProtocols {
		t.Fatalf("the document socket's handshake once the issuer answers: %d %s, want 101", status, body)
	}
}
