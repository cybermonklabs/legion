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

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
// database. Its verifier judges an assertion's times on clock (oidc.NewIAPVerifierOn), and every
// assertion it mints is stamped from that clock's present too.
type iapSocketKeys struct {
	keys     *oidctest.Issuer
	signing  *oidctest.Key
	identity identity.IAPIdentity
	clock    clock.Clock
}

// newIAPSocketKeys serves a key set whose verifier judges an assertion's times on judged, a fake
// the row moves past an assertion's expiry itself (pastExpiry), so no wall clock decides when an
// assertion expires.
func newIAPSocketKeys(t *testing.T, database *store.Store, judged clock.Clock) iapSocketKeys {
	t.Helper()
	keys := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	keys.Publish(signing)
	return iapSocketKeys{keys: keys, signing: signing, clock: judged, identity: identity.IAPIdentity{
		Verifier: oidc.NewIAPVerifierOn(iapSocketAudience, oidc.NewKeysAt(keys.URL()+"/keys"), judged),
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
		"iat": k.clock.Now().Add(-10 * time.Minute).Unix(), "exp": exp.Unix(),
	}))
	return header
}

// expiring is an assertion that stays valid at least lifetime more on the verifier's clock, and at
// most a second beyond it: its exp is already past, and the skew is what still admits it. An
// assertion carries its exp in whole seconds (assertion's exp.Unix()), so the exp is rounded up to
// the next whole second; rounded down, the fraction of the second at the mint would come off
// lifetime, and an open that took longer than what was left of a one-second lifetime was refused
// 401.
func (k iapSocketKeys) expiring(t *testing.T, lifetime time.Duration) http.Header {
	t.Helper()
	exp := k.clock.Now().Add(-oidc.IAPClockSkew + lifetime).Truncate(time.Second).Add(time.Second)
	return k.assertion(t, exp)
}

// pastExpiry is a second past the last moment the verifier admits header's assertion: the
// assertion's own exp, as the verifier reads it (Claims.Expiry, the whole second the assertion
// carries), plus oidc.IAPClockSkew, the skew the verifier allows. The verifier still admits the
// assertion at exactly that exp plus the skew (its check is a strict Before), so a row that moves
// the verifier's fake here has the assertion expired by the exp it carries, whatever moment the row
// started at. It asks the verifier while the verifier still admits the assertion.
func (k iapSocketKeys) pastExpiry(t *testing.T, header http.Header) time.Time {
	t.Helper()
	claims, err := k.identity.Verifier.Verify(t.Context(), header.Get(identity.IAPAssertionHeader))
	if err != nil {
		t.Fatalf("the verifier refused the assertion whose expiry the row reads: %v", err)
	}
	return claims.Expiry.Add(oidc.IAPClockSkew + time.Second)
}

// IAP signs an assertion for every request and Google gives one at most ten minutes, but a live
// socket's heartbeat re-checks the assertion its upgrade carried. Once that assertion expires the
// socket is rotated, not cut: it closes with 4001, the close the page answers by reopening at once,
// and never with the 1008 that says its person is gone. The reopened socket carries a fresh
// assertion, which is judged again: the expired one opens nothing, and a fresh one opens a socket.
// The control, the same person on a nine-minute assertion, stays open throughout.
//
// The heartbeats are the server's, on its clock (whileCallerResolves), a fake the row moves to each
// one. The assertion's expiry is the IAP verifier's, judged on a fake of its own
// (oidc.NewIAPVerifierOn) that stands still while the sockets open, so no upgrade comes after the
// short assertion expired however slow the machine is. The row moves that fake a second past the
// short assertion's exp, read from the assertion itself (pastExpiry), and the skew, before it moves
// the heartbeats' fake to the heartbeat that re-checks it, and no wall clock decides when the
// assertion expires. The sockets' read deadlines run on a fake no one moves (liveSocketClock), so
// no pong the runner holds back ends a socket.
func TestALiveSocketBehindIAPRotatesWhenItsUpgradesAssertionExpires(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	judged := clocktest.New(time.Now())
	iap := newIAPSocketKeys(t, database, judged)
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	rig := newLiveSocketRig(t, testServerOptions{
		store: database, streamHeartbeat: heartbeat, identity: iap.identity, clock: fake, liveSocketClock: clocktest.Held(),
	}, nil)

	const lifetime = 2 * time.Second
	expiring := iap.expiring(t, lifetime)
	short := rig.dial(t, expiring)
	long := rig.dial(t, iap.assertion(t, judged.Now().Add(9*time.Minute)))

	// The verifier admits the short assertion until its exp plus the skew, and the exp is rounded up
	// to a whole second (expiring), so that moment lies up to a second beyond lifetime past judged's
	// present. The row moves judged a second past the moment the assertion itself names.
	judged.Set(iap.pastExpiry(t, expiring))
	// Both sockets, the short and the control, arm the first heartbeat before the clock moves to it.
	next := driveHeartbeats(t, fake, opened.Add(heartbeat), 1, heartbeat, 2)
	closed := short.closeError(t)
	if closed.Code != contracts.LiveSocketMaxAgeClose || closed.Text != liveSocketCredentialExpiredReason {
		t.Fatalf("the socket whose upgrade's assertion expired closed with %d %q, want %d %q, which a page answers by reopening at once",
			closed.Code, closed.Text, contracts.LiveSocketMaxAgeClose, liveSocketCredentialExpiredReason)
	}
	// Three more heartbeats: the control is re-checked on each and stays open. Each heartbeat the
	// row sees armed proves the checks two or more heartbeats before it returned with the control
	// still resolving (driveHeartbeats), and a check that closed the socket would arm no more.
	driveHeartbeats(t, fake, next, 3, heartbeat, 1)
	select {
	case <-long.ended:
		t.Fatalf("control: the socket on a nine-minute assertion ended: %v", long.err)
	default:
	}
	if status := rig.refused(t, expiring); status != http.StatusUnauthorized {
		t.Fatalf("a reopen that carried the expired assertion again was refused %d, want 401", status)
	}
	reopened := rig.dial(t, iap.assertion(t, judged.Now().Add(9*time.Minute)))
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
		changed.Verifier = oidc.NewIAPVerifierOn(iapSocketAudience, oidc.NewKeysAt(rotated.URL()+"/keys"), iap.clock)
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
// fresh assertion is judged again and refused. As in
// TestALiveSocketBehindIAPRotatesWhenItsUpgradesAssertionExpires, the heartbeats are on the
// server's clock and the assertion's expiry on the verifier's, two fakes the row moves: the
// verifier's a second past the assertion's exp and the skew (pastExpiry), with the world changed,
// before the heartbeats' to the first heartbeat, which meets both. No wall clock decides whether
// the upgrade came in time or when the assertion expired.
func TestALiveSocketBehindIAPClosesWhenItsExpiredAssertionIsAlsoWrong(t *testing.T) {
	t.Parallel()
	for _, change := range iapChanges {
		t.Run(change.name, func(t *testing.T) {
			database := storetest.Open(t)
			judged := clocktest.New(time.Now())
			iap := newIAPSocketKeys(t, database, judged)
			switched := newSwitchedIdentity(iap.identity)
			const heartbeat = time.Second
			fake := clocktest.New(time.Now())
			opened := fake.Now()
			rig := newLiveSocketRig(t, testServerOptions{
				store: database, streamHeartbeat: heartbeat, identity: switched, clock: fake, liveSocketClock: clocktest.Held(),
			}, nil)
			expiring := iap.expiring(t, time.Second)
			socket := rig.dial(t, expiring)
			judged.Set(iap.pastExpiry(t, expiring))
			switched.swap(change.apply(t, iap))
			// The socket's one heartbeat on the server's clock; the socket it closes arms no other.
			first := opened.Add(heartbeat)
			awaitArmedAt(t, fake, "the socket's first heartbeat", first)
			fake.Set(first)
			if closed := socket.closeError(t); closed.Code != gws.ClosePolicyViolation {
				t.Fatalf("the socket whose upgrade's assertion expired, %s, closed with %d %q, want %d: it is refused, not rotated",
					change.name, closed.Code, closed.Text, gws.ClosePolicyViolation)
			}
			if status := rig.refused(t, iap.assertion(t, judged.Now().Add(9*time.Minute))); status != http.StatusUnauthorized {
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

// newIAPDocumentRig starts the rig with its server's heartbeats on clk and its IAP verifier
// judging an assertion's times on judged (newIAPSocketKeys), each a fake the row moves.
func newIAPDocumentRig(t *testing.T, heartbeat time.Duration, clk, judged clock.Clock) *iapDocumentRig {
	t.Helper()
	database := storetest.Open(t)
	rig := &iapDocumentRig{iap: newIAPSocketKeys(t, database, judged)}
	rig.switched = newSwitchedIdentity(rig.iap.identity)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Identity: rig.switched, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: rig.switched, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: testServerURL, Docs: documents, Events: broker, StreamHeartbeat: heartbeat, Clock: clk,
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
		for name, values := range rig.iap.assertion(t, rig.iap.clock.Now().Add(9*time.Minute)) {
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
// silent drop that ends a socket whose person no longer resolves. The control, a nine-minute
// assertion, keeps its socket open and answering its editor. The heartbeats are on the server's
// clock, a fake the row moves, and the assertion's expiry on a fake of the IAP verifier's own,
// which the row moves a second past the short assertion's exp and skew (pastExpiry) before the
// heartbeat that re-checks it, as in
// TestALiveSocketBehindIAPRotatesWhenItsUpgradesAssertionExpires.
func TestADocumentSocketBehindIAPReopensWhenItsUpgradesAssertionExpires(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	fake := clocktest.New(time.Now())
	opened := fake.Now()
	judged := clocktest.New(time.Now())
	rig := newIAPDocumentRig(t, heartbeat, fake, judged)
	const lifetime = 3 * time.Second
	expiring := rig.iap.expiring(t, lifetime)
	short := rig.openDocument(t, expiring)
	long := rig.openDocument(t, rig.iap.assertion(t, judged.Now().Add(9*time.Minute)))

	// The verifier admits the short assertion until its exp plus the skew, up to a second beyond
	// lifetime past judged's present (TestALiveSocketBehindIAPRotatesWhenItsUpgradesAssertionExpires);
	// the row moves judged a second past the moment the assertion itself names.
	judged.Set(rig.iap.pastExpiry(t, expiring))
	// Both sockets, the short and the control, arm the first heartbeat before the clock moves to it.
	next := driveHeartbeats(t, fake, opened.Add(heartbeat), 1, heartbeat, 2)
	testwait.Receive(t, "the document socket whose upgrade's assertion expired to close", short.connection().Ended)
	if code := short.connection().CloseFrame(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("the document socket whose upgrade's assertion expired ended with close frame %d (0 for none), want a close frame of %d, on which the provider reconnects",
			code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	// Three more heartbeats, each a check of the control that returned with it still resolving
	// (driveHeartbeats); the barrier below then shows it still answers.
	driveHeartbeats(t, fake, next, 3, heartbeat, 1)
	select {
	case <-long.connection().Ended:
		t.Fatalf("control: the document socket on a nine-minute assertion ended with %d", long.connection().CloseCode())
	default:
	}
	long.barrier(t)
	rig.openDocument(t, rig.iap.assertion(t, judged.Now().Add(9*time.Minute))).barrier(t)
}

// The document socket's side of the same rule: an expired assertion that IAP's keys no longer
// verify, or whose person is no longer admitted, ends the socket as any refusal does, with no close
// frame, never the 4412 on which the provider reconnects as after a rotation.
// Both end of stream and reset can drop a socket without a frame.
// The test reads the frame the peer received (CloseFrame), not gorilla's status for the drop.
//
// The test moves the verifier's fake a second past the assertion's exp and skew (pastExpiry).
// It changes what the verifier accepts before it moves the heartbeat clock, as
// TestALiveSocketBehindIAPClosesWhenItsExpiredAssertionIsAlsoWrong does.
func TestADocumentSocketBehindIAPClosesWhenItsExpiredAssertionIsAlsoWrong(t *testing.T) {
	t.Parallel()
	for _, change := range iapChanges {
		t.Run(change.name, func(t *testing.T) {
			const heartbeat = time.Second
			fake := clocktest.New(time.Now())
			opened := fake.Now()
			judged := clocktest.New(time.Now())
			rig := newIAPDocumentRig(t, heartbeat, fake, judged)
			expiring := rig.iap.expiring(t, 2*time.Second)
			peer := rig.openDocument(t, expiring)
			judged.Set(rig.iap.pastExpiry(t, expiring))
			rig.switched.swap(change.apply(t, rig.iap))
			// The socket's one heartbeat on the server's clock; the socket it ends arms no other.
			first := opened.Add(heartbeat)
			awaitArmedAt(t, fake, "the document socket's first heartbeat", first)
			fake.Set(first)
			testwait.Receive(t, fmt.Sprintf("the document socket whose upgrade's assertion expired, %s, to close", change.name), peer.connection().Ended)
			connection := peer.connection()
			dropped, err := connection.DroppedWithoutCloseFrame()
			if code := connection.CloseFrame(); code != 0 {
				t.Fatalf("the document socket whose upgrade's assertion expired, %s, closed with a close frame of %d (its last read: %v), want none: it is refused, not rotated",
					change.name, code, err)
			}
			// Do not count a parser error or a timed-out read as the server dropping the socket.
			if !dropped {
				t.Fatalf("the document socket whose upgrade's assertion expired, %s, did not end in a transport disconnect: last read: %v",
					change.name, err)
			}
		})
	}
}

// A socket whose person no longer resolves is still cut as before, whatever identity it runs
// under: only the credential's own expiry (identity.ErrCredentialExpired) is a rotation.
func TestOnlyAnExpiredCredentialRotatesASocket(t *testing.T) {
	t.Parallel()
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
