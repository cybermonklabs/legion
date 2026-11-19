package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// iapSocketAudience is the audience IAP signs for the test's load balancer backend service.
const iapSocketAudience = "/projects/123456789012/global/backendServices/9876543210"

// iapSocketKeys is a key set served on this machine in IAP's place, so no test calls Google, and
// the identity a Dispatch behind IAP names its people with: the people of example.com, recorded in
// database.
type iapSocketKeys struct {
	keys     *oidctest.Issuer
	signing  *oidctest.Key
	identity identity.IAPIdentity
}

func newIAPSocketKeys(t *testing.T, database *store.Store) iapSocketKeys {
	t.Helper()
	keys := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	keys.Publish(signing)
	return iapSocketKeys{keys: keys, signing: signing, identity: identity.IAPIdentity{
		Verifier: oidc.NewIAPVerifier(iapSocketAudience, oidc.NewKeysAt(keys.URL()+"/keys")),
		Rule:     identity.DomainRule{Domain: "example.com"},
		People:   store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
	}}
}

// assertion is the header of a request IAP passed for a.person@example.com, carrying an assertion
// IAP signed that expires at exp. Google allows 30 seconds of skew past it (oidc.IAPClockSkew).
// Set writes the name in its canonical form, which is what a request read off the wire carries
// and what Header.Get looks up.
func (k iapSocketKeys) assertion(t *testing.T, exp time.Time) http.Header {
	t.Helper()
	header := http.Header{}
	header.Set(identity.IAPAssertionHeader, k.keys.Mint(t, k.signing, map[string]any{
		"iss": oidc.IAPIssuer, "aud": iapSocketAudience, "sub": "accounts.google.com:118000000000000000001",
		"email": "a.person@example.com", "hd": "example.com",
		"iat": time.Now().Add(-10 * time.Minute).Unix(), "exp": exp.Unix(),
	}))
	return header
}

// expiring is an assertion that stays valid about lifetime more: its exp is already past, and the
// skew is what still admits it.
func (k iapSocketKeys) expiring(t *testing.T, lifetime time.Duration) http.Header {
	t.Helper()
	return k.assertion(t, time.Now().Add(-oidc.IAPClockSkew+lifetime))
}

// IAP signs an assertion for every request and Google gives one at most ten minutes, but a live
// socket's heartbeat re-checks the assertion its upgrade carried. Once that assertion expires the
// socket is rotated, not cut: it closes with 4001, the close the page answers by reopening at once,
// and never with the 1008 that says its person is gone. The reopened socket carries a fresh
// assertion, which is judged again: the expired one opens nothing, and a fresh one opens a socket.
// The control, the same person on a nine-minute assertion, stays open throughout.
func TestALiveSocketBehindIAPRotatesWhenItsUpgradesAssertionExpires(t *testing.T) {
	database := storetest.Open(t)
	iap := newIAPSocketKeys(t, database)
	const heartbeat = 200 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{store: database, streamHeartbeat: heartbeat, identity: iap.identity}, nil)

	expiring := iap.expiring(t, 2*time.Second)
	short := rig.dial(t, expiring)
	long := rig.dial(t, iap.assertion(t, time.Now().Add(9*time.Minute)))

	closed := short.closeError(t)
	if closed.Code != contracts.LiveSocketMaxAgeClose || closed.Text != liveSocketCredentialExpiredReason {
		t.Fatalf("the socket whose upgrade's assertion expired closed with %d %q, want %d %q, which a page answers by reopening at once",
			closed.Code, closed.Text, contracts.LiveSocketMaxAgeClose, liveSocketCredentialExpiredReason)
	}
	// A few more heartbeats: the control is re-checked on each and stays open.
	testwait.Span(t, 3*heartbeat, "the heartbeat is the server's own ticker (streamHeartbeat), whose ticks the test "+
		"cannot count, and the control's verdict is that it stays open across them: no state, no synctest bubble "+
		"(real sockets) and no later action shows the control's heartbeats ran")
	select {
	case <-long.ended:
		t.Fatalf("control: the socket on a nine-minute assertion ended: %v", long.err)
	default:
	}
	if status := rig.refused(t, expiring); status != http.StatusUnauthorized {
		t.Fatalf("a reopen that carried the expired assertion again was refused %d, want 401", status)
	}
	reopened := rig.dial(t, iap.assertion(t, time.Now().Add(9*time.Minute)))
	reopened.send(t, LiveUnsubscribe{Type: "unsubscribe"})
	reopened.refusal(t, "", "INVALID_FRAME")
}

// switchedIdentity is a Dispatch's IAP identity, which a test swaps while sockets are open, so a
// heartbeat meets the world after a change: IAP's key withdrawn, a person no longer admitted.
type switchedIdentity struct {
	current atomic.Pointer[identity.IAPIdentity]
}

func newSwitchedIdentity(initial identity.IAPIdentity) *switchedIdentity {
	switched := &switchedIdentity{}
	switched.swap(initial)
	return switched
}

func (s *switchedIdentity) Login(r *http.Request) (string, error) { return s.current.Load().Login(r) }

func (s *switchedIdentity) swap(next identity.IAPIdentity) { s.current.Store(&next) }

// iapChange is one change in the world an open socket's heartbeat meets.
type iapChange struct {
	name  string
	apply func(t *testing.T, iap iapSocketKeys) identity.IAPIdentity
}

// iapChanges are the two ways an assertion IAP signed stops admitting its person, besides its age:
// IAP withdrew the key that signed it (the key set now holds another key under that key id, so the
// signature no longer verifies), or the person is no longer of the domain the rule admits.
var iapChanges = []iapChange{
	{"IAP's key withdrawn", func(t *testing.T, iap iapSocketKeys) identity.IAPIdentity {
		rotated := oidctest.New(t)
		rotated.Publish(oidctest.NewES256Key(t, iap.signing.KeyID()))
		changed := iap.identity
		changed.Verifier = oidc.NewIAPVerifier(iapSocketAudience, oidc.NewKeysAt(rotated.URL()+"/keys"))
		return changed
	}},
	{"the person no longer admitted", func(t *testing.T, iap iapSocketKeys) identity.IAPIdentity {
		changed := iap.identity
		changed.Rule = identity.DomainRule{Domain: "other.example"}
		return changed
	}},
}

// An assertion that has expired AND that IAP's keys no longer verify, or whose person is no longer
// admitted, is wrong, not merely old: the heartbeat that meets it closes the live socket with 1008,
// the close that says its caller no longer resolves, never the 4001 rotation, and the reopen with a
// fresh assertion is judged again and refused. The heartbeat is 3 s, so by the first one the
// upgrade's assertion, which had about a second left, is past its exp and the skew.
func TestALiveSocketBehindIAPClosesWhenItsExpiredAssertionIsAlsoWrong(t *testing.T) {
	for _, change := range iapChanges {
		t.Run(change.name, func(t *testing.T) {
			database := storetest.Open(t)
			iap := newIAPSocketKeys(t, database)
			switched := newSwitchedIdentity(iap.identity)
			rig := newLiveSocketRig(t, testServerOptions{store: database, streamHeartbeat: 3 * time.Second, identity: switched}, nil)
			socket := rig.dial(t, iap.expiring(t, time.Second))
			switched.swap(change.apply(t, iap))
			if closed := socket.closeError(t); closed.Code != gws.ClosePolicyViolation {
				t.Fatalf("the socket whose upgrade's assertion expired, %s, closed with %d %q, want %d: it is refused, not rotated",
					change.name, closed.Code, closed.Text, gws.ClosePolicyViolation)
			}
			if status := rig.refused(t, iap.assertion(t, time.Now().Add(9*time.Minute))); status != http.StatusUnauthorized {
				t.Fatalf("%s: a reopen with a fresh assertion was answered %d, want 401", change.name, status)
			}
		})
	}
}

// iapDocumentRig is a Dispatch behind IAP serving document sockets: the API and the document
// service share one IAP identity, as cmd/dispatch hands them one, which a test may swap
// (switched), and the sockets re-check their caller every heartbeat.
type iapDocumentRig struct {
	iap        iapSocketKeys
	switched   *switchedIdentity
	server     *httptest.Server
	sockets    *servedSockets
	artifactID string
}

func newIAPDocumentRig(t *testing.T, heartbeat time.Duration) *iapDocumentRig {
	t.Helper()
	database := storetest.Open(t)
	rig := &iapDocumentRig{iap: newIAPSocketKeys(t, database)}
	rig.switched = newSwitchedIdentity(rig.iap.identity)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Identity: rig.switched, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: rig.switched, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: testServerURL, Docs: documents, Events: broker, StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.server = httptest.NewServer(rig.sockets.serve(mux.ServeHTTP))
	t.Cleanup(rig.server.Close)

	viaIAP := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
		for name, values := range rig.iap.assertion(t, time.Now().Add(9*time.Minute)) {
			request.Header[name] = values
		}
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	if response := viaIAP(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := viaIAP(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Live document", "spec": "The spec."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	rig.artifactID = decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created).PrimaryArtifactID
	return rig
}

// openDocument opens the spec's document socket as the dashboard's editor does, through IAP, whose
// assertion rides in headers, and closes it when the test ends.
func (rig *iapDocumentRig) openDocument(t *testing.T, headers http.Header) *syncedPeer {
	t.Helper()
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + rig.artifactID, sockets: rig.sockets,
		headers: headers, artifactID: rig.artifactID, doc: crdt.New(),
	}
	peer.connect(t)
	t.Cleanup(peer.close)
	return peer
}

// The document socket's heartbeat re-checks the assertion its upgrade carried too, and once that
// assertion expires the socket closes with the document socket's own reopen code, 4412, on which
// the provider reconnects and the reconnect is admitted again on a fresh assertion; never the
// silent drop (1006) that ends a socket whose person no longer resolves. The control, a nine-minute
// assertion, keeps its socket open and answering its editor.
func TestADocumentSocketBehindIAPReopensWhenItsUpgradesAssertionExpires(t *testing.T) {
	const heartbeat = 250 * time.Millisecond
	rig := newIAPDocumentRig(t, heartbeat)
	short := rig.openDocument(t, rig.iap.expiring(t, 3*time.Second))
	long := rig.openDocument(t, rig.iap.assertion(t, time.Now().Add(9*time.Minute)))

	testwait.Receive(t, "the document socket whose upgrade's assertion expired to close", short.connection().Ended)
	if code := short.connection().CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("the document socket whose upgrade's assertion expired closed with %d, want %d, on which the provider reconnects",
			code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	testwait.Span(t, 3*heartbeat, "the heartbeat is the document socket's own ticker, whose ticks the test cannot "+
		"count, and the control's verdict is that it stays open across them: no state, no synctest bubble (real "+
		"sockets) and no later action shows the control's heartbeats ran; the barrier below then shows it still answers")
	select {
	case <-long.connection().Ended:
		t.Fatalf("control: the document socket on a nine-minute assertion ended with %d", long.connection().CloseCode())
	default:
	}
	long.barrier(t)
	rig.openDocument(t, rig.iap.assertion(t, time.Now().Add(9*time.Minute))).barrier(t)
}

// The document socket's side of the same rule: an expired assertion that IAP's keys no longer
// verify, or whose person is no longer admitted, ends the socket as any refusal does, with no close
// frame (1006), never the 4412 on which the provider reconnects as after a rotation. The heartbeat
// is 4 s, so by the first one the upgrade's assertion, which had about 2 s left, is past its exp.
func TestADocumentSocketBehindIAPClosesWhenItsExpiredAssertionIsAlsoWrong(t *testing.T) {
	for _, change := range iapChanges {
		t.Run(change.name, func(t *testing.T) {
			rig := newIAPDocumentRig(t, 4*time.Second)
			peer := rig.openDocument(t, rig.iap.expiring(t, 2*time.Second))
			rig.switched.swap(change.apply(t, rig.iap))
			testwait.Receive(t, fmt.Sprintf("the document socket whose upgrade's assertion expired, %s, to close", change.name), peer.connection().Ended)
			if code := peer.connection().CloseCode(); code != gws.CloseAbnormalClosure {
				t.Fatalf("the document socket whose upgrade's assertion expired, %s, closed with %d, want %d: it is refused, not rotated",
					change.name, code, gws.CloseAbnormalClosure)
			}
		})
	}
}

// A socket whose person no longer resolves is still cut as before, whatever identity it runs
// under: only the credential's own expiry (identity.ErrCredentialExpired) is a rotation.
func TestOnlyAnExpiredCredentialRotatesASocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"an expired IAP assertion", identity.ErrCredentialExpired, errCredentialExpired},
		{"no identity", identity.ErrNoIdentity, context.Canceled},
		{"the sign-in issuer down", identity.ErrSignInUnavailable, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, deps := newTestServer(t, testServerOptions{streamHeartbeat: 10 * time.Millisecond})
			served := directServer(deps)
			cause := make(chan error, 1)
			handler := served.whileCallerResolves(func(*http.Request) error { return tc.err }, func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				cause <- context.Cause(r.Context())
			})
			handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws/live", nil))
			if got := <-cause; !errors.Is(got, tc.want) {
				t.Fatalf("the heartbeat's refusal %v cancelled the socket with %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
