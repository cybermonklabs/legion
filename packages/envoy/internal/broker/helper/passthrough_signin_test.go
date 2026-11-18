// packages/envoy/internal/broker/helper/passthrough_signin_test.go
//go:build linux

package helper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// signedInPerson is the person signedInBox signs in on the box.
const signedInPerson = "ada@example.com"

// boxMachine is this machine's own identity, the actor token of a person's trade on it.
type boxMachine struct{}

func (boxMachine) Token(context.Context) (string, string, error) {
	return "this-machine's-identity-token", oauthwire.SubjectTokenTypeGoogle, nil
}

func (boxMachine) Describe() string { return "this machine" }

// signedInBox is the sign-in `agent-secrets login` leaves on a box: signedInPerson's device
// grant at an issuer, kept with a box key of its own, read through the REAL
// brokerclient.SignInAuthority that agent-secrets-helper serves its sessions with
// (cmd/agent-secrets-helper, signInAuthority). Its trades go to tokenURL.
func signedInBox(t *testing.T, tokenURL string) *brokerclient.SignInAuthority {
	t.Helper()
	const clientID = "example-device-client"
	deviceClientWord := clientID + "-word" // the device client's shared word, which the stand-in issuer checks
	issuer := oidctest.New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "google-key"), clientID, deviceClientWord)
	issuer.SignInByDefault(map[string]any{"sub": signedInPerson, "email": signedInPerson, "email_verified": true})
	flow := &brokerclient.DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: deviceClientWord}
	authorization, err := flow.Start(context.Background())
	if err != nil {
		t.Fatalf("start the sign-in: %v", err)
	}
	if err := issuer.ApproveDeviceCode(authorization.UserCode); err != nil {
		t.Fatalf("type the code in: %v", err)
	}
	authorization.Interval = 1
	granted, err := flow.Wait(context.Background(), authorization)
	if err != nil {
		t.Fatalf("the sign-in: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	store := brokerclient.SignInStore{Dir: filepath.Join(t.TempDir(), "signins")}
	if err := store.Write(brokerclient.SignInRecord{
		Person: signedInPerson, Issuer: flow.Issuer, ClientID: clientID, RefreshToken: granted.RefreshToken,
		BoxKey:     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		Thumbprint: "thumbprint-of-" + signedInPerson, SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("keep the sign-in: %v", err)
	}
	return &brokerclient.SignInAuthority{
		Store: store, Machine: boxMachine{}, Flow: flow, BrokerURL: brokeraddr.New(tokenURL), HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// TestAPassThroughOpAnswersInsideItsBoundWhileTheBoxsSignInIsTrading: A10 says the helper
// answers every request it accepted within the time its client waits, and adds that no lock the
// helper holds for its own work outlives a caller's bound.
//
// A box a person has signed in on enrols every call under that person's authority
// (Broker.signingAuthority), and brokerclient.SignInAuthority.Current holds its lock across the
// trade that renews that authority. A registered session's enrollLoop makes that trade on the
// serve loop's context, which has no deadline. An enroll-session-key that arrives while the trade
// is in flight waits for the lock only while its own context lives (ctxlock), so the wait ends at
// the bound (Server.brokerCall) before the op has sent the broker anything, and the op answers
// what it knows: ENROLL_FAILED, timed out waiting for the person's sign-in. The row asserts that
// code and that text, which brokerCall's timer (ENROLL_OUTCOME_UNKNOWN, a second later) never
// gives, and that the fake received no enroll while the op waited. That count shows the failure
// is known, since the broker made nothing, and that the wait that ended was Current's and not
// Refused's, whose error carries the same text after a refused enroll.
//
// The trade waits at a gate the row opens once it has read its answer, so the trade lasts as long
// as the row needs and no longer. The row runs under a bound of 1 s rather than the real 20 s.
//
// The named revert: put a sync.Mutex back as SignInAuthority.mu, as it was before ctxlock. The
// op then waits out the trade with no context, and brokerCall's timer answers it
// ENROLL_OUTCOME_UNKNOWN at the bound plus 1 s (21 s under the real 20 s bound), as
// TestAnOpHeldOnALockAnswersUnknownAtTheBoundAndSendsNothingAfter reads, and not an empty reply:
// the code, the text and the time fail, though the broker made nothing.
func TestAPassThroughOpAnswersInsideItsBoundWhileTheBoxsSignInIsTrading(t *testing.T) {
	// bound is the row's bound on the op, and timerAnswers when brokerCall's timer answers an op
	// still running: the bound plus 1 s.
	const (
		bound        = time.Second
		timerAnswers = bound + time.Second
	)
	var trades atomic.Int64
	tradeStarted := make(chan struct{})
	tradeGate := make(chan struct{})
	releaseTrade := sync.OnceFunc(func() { close(tradeGate) })
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != brokerclient.TokenPath {
			http.NotFound(w, r)
			return
		}
		if trades.Add(1) == 1 {
			close(tradeStarted)
			select {
			case <-tradeGate:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("access-token-%d", trades.Load()), "token_type": "DPoP", "expires_in": 3600,
			"credential_id": "credential-1", "credential_expires_at": "2099-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(tokens.Close)

	r := startRig(t, "")
	r.srv.Broker.SignIns = signedInBox(t, tokens.URL)
	r.srv.BrokerCallBound = bound
	// Registered after the rig and the token server, so the trade is let go before their Close.
	t.Cleanup(releaseTrade)

	// A session the helper registered for itself enrols, as enrollLoop does, and its trade for
	// the person's token is the one held.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registered := make(chan error, 1)
	go func() {
		_, _, err := r.srv.Broker.EnrollIdentity(context.Background(), identity{
			RuntimeID: "session-registered-on-this-box", Thumbprint: "tp-registered", Key: key,
		})
		registered <- err
	}()
	select {
	case <-tradeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the registered session's enrollment never traded for the person's token")
	}

	started := time.Now()
	resp, err := Call(r.sock, Request{Op: "enroll-session-key", RuntimeID: "rt-during-the-trade", Thumbprint: "tp-during-the-trade"}, BrokerCallWait)
	waited := time.Since(started)
	// Read before the trade is let go: the registered session's own enroll follows its trade.
	r.fake.mu.Lock()
	arrivals := r.fake.enrollArrivals
	r.fake.mu.Unlock()
	if err != nil {
		t.Fatalf("enroll-session-key while the box's sign-in is trading: %v after %s; the client must read one JSON reply, never an empty one (the bound is %s)",
			err, waited, bound)
	}
	// Each is checked apart, so a failing row reports every one that is wrong.
	if resp.OK || resp.Code != CodeEnrollFailed {
		t.Errorf("enroll-session-key while the box's sign-in is trading: %+v after %s; want %s, a known failure: the op sent the broker nothing", resp, waited, CodeEnrollFailed)
	}
	if want := "timed out waiting for " + signedInPerson + "'s sign-in on this machine"; !strings.Contains(resp.Error, want) {
		t.Errorf("enroll-session-key while the box's sign-in is trading: error %q; want it to name the sign-in it waited on: %q", resp.Error, want)
	}
	if arrivals != 0 {
		t.Errorf("the fake received %d POST /v1/enrollments while enroll-session-key waited for the sign-in; want none", arrivals)
	}
	if waited >= timerAnswers {
		t.Errorf("enroll-session-key answered after %s, at or past brokerCall's timer (%s): the wait for the sign-in did not end with the op's context", waited, timerAnswers)
	}

	// The trade the op stood behind goes on to its own answer, and the registered session enrols
	// under the person's credential.
	releaseTrade()
	select {
	case err := <-registered:
		if err != nil {
			t.Fatalf("the registered session's enrollment, once its trade ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the registered session's enrollment did not end after its trade was let go")
	}
}
