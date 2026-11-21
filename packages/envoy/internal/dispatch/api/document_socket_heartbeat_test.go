package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	tokens     *brokerTokenRig
	sessions   *store.PgSessionStore
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
	rig := &documentSocketTokenRig{tokens: newBrokerTokenRig(t)}
	database := rig.tokens.database
	rig.sessions = store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: person, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: testServerURL, Docs: documents, Events: broker,
		BrokerTokens: rig.tokens.deps.BrokerTokens, StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.server = httptest.NewServer(rig.sockets.serve(mux.ServeHTTP))
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
// within a heartbeat of the session's end, while the broker still holds the token live and has
// not once been asked about it.
func TestADocumentSocketClosesOnceItsCookieEndsThoughALiveTokenRidesBesideIt(t *testing.T) {
	const heartbeat = 250 * time.Millisecond
	for _, beside := range []struct {
		name   string
		answer func(jkt string) ciphertoken.Introspection
	}{
		{"a read-only token", readOnlyToken},
		{"a token that writes", liveNamedToken},
	} {
		t.Run(beside.name, func(t *testing.T) {
			rig := newDocumentSocketTokenRig(t, heartbeat)
			caller := newBrokerCaller(t)
			const token = "a-token-sent-beside-the-cookie"
			rig.tokens.broker.Says(token, beside.answer(caller.jkt))
			presented := caller.presenting(t, http.MethodGet, "/ws/doc/"+rig.artifactID, token, nil)
			peer := rig.openDocument(t, presentedHeaders(presented))

			// While the cookie resolves the socket outlives several heartbeats and still
			// answers its editor.
			time.Sleep(4 * heartbeat)
			peer.barrier(t)

			if err := rig.sessions.RevokeSessions(context.Background(), documentSocketPerson); err != nil {
				t.Fatalf("sign out: %v", err)
			}
			ended := time.Now()
			select {
			case <-peer.connection().Ended:
				t.Logf("the document socket closed %s after its cookie's session ended (heartbeat %s)", time.Since(ended).Round(time.Millisecond), heartbeat)
			case <-time.After(heartbeat + time.Second):
				t.Fatalf("the document socket stayed open %s after its cookie's session ended, kept by %s the broker holds live", time.Since(ended).Round(time.Millisecond), beside.name)
			}
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
// holding it live and the answer it remembered has run out. The heartbeat is a second, so the
// client's pong meets the pong deadline on a loaded runner.
func TestALiveSocketOpenedByABrokerTokenResolvesThatTokenOnEveryHeartbeat(t *testing.T) {
	const heartbeat = time.Second
	rig := newDocumentSocketTokenRig(t, heartbeat)
	caller := newBrokerCaller(t)
	const token = "a-token-holding-the-live-socket-open"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	presented := caller.presenting(t, http.MethodGet, "/ws/live", token, nil)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(rig.server.URL, "http")+"/ws/live", presentedHeaders(presented)).read()

	client.quiet(t, 3*heartbeat+heartbeat/2, "a live socket whose token the broker holds live")

	rig.tokens.broker.Says(token, ciphertoken.Introspection{Active: false})
	if closed := client.closeError(t, ciphertoken.AffirmativeCacheTTL+2*heartbeat+time.Second); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the live socket closed with %d %q once the broker stopped holding its token live, want 1008", closed.Code, closed.Text)
	}
}
