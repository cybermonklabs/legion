//go:build linux

package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// stoppedLoopRig is a helper in identity mode over a broker whose trade answers what the test
// sets: a credential, a refusal no retry cures, or a token with no credential in it. The trade
// loop's waits are the test's (tradeWaits), so it trades when the test says.
type stoppedLoopRig struct {
	broker *enrolBroker
	helper *Broker
	served *rig
	out    *syncBuffer
	expiry *expiryTimers
	waits  *tradeWaits
	trades func() int
	// requests counts every request to the token endpoint, whatever it is answered: a trade the
	// loop makes after it stopped is one, refused or not.
	requests atomic.Int32
	// cancel ends the helper's context, as a shutdown does.
	cancel  context.CancelFunc
	refusal atomic.Pointer[stopAnswer]
}

// stopAnswer is what the token endpoint answers while it is set: an OAuth refusal, or, for
// noCredential, a 200 that serves a token and no launcher credential.
type stopAnswer struct {
	status       int
	body         map[string]string
	noCredential bool
}

// newStoppedLoopRig starts the helper, with a credential already traded unless the test set a
// refusal first, and serves it on a socket of its own.
func newStoppedLoopRig(t *testing.T, first *stopAnswer) *stoppedLoopRig {
	t.Helper()
	r := &stoppedLoopRig{broker: newEnrolBroker(t)}
	r.refusal.Store(first)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	trader := http.NewServeMux()
	r.trades = serveTokenExchange(t, r.broker, trader, thumbprint)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, request *http.Request) {
		r.requests.Add(1)
		answer := r.refusal.Load()
		switch {
		case answer == nil:
			trader.ServeHTTP(w, request)
		case answer.noCredential:
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "not-a-secret-token", "token_type": "DPoP", "expires_in": 3600})
		default:
			writeJSON(w, answer.status, answer.body)
		}
	})
	mux.Handle("/", r.broker.srv.Config.Handler)
	r.broker.srv.Config.Handler = mux

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.waits = &tradeWaits{}
	machine, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(r.broker.srv.URL), Key: key, HTTP: r.broker.srv.Client(),
		Subject: cipherclient.NewProjectedToken(tokenFile), After: r.waits.after,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.out = &syncBuffer{}
	r.helper = &Broker{
		URL: cipheraddr.New(r.broker.srv.URL), Identity: machine, HTTP: r.broker.srv.Client(),
		OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
		Log:          slog.New(slog.NewJSONHandler(r.out, nil)),
	}
	t.Cleanup(r.helper.Close)
	r.expiry = installExpiryTimers(r.helper)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	if err := r.helper.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.served = serveBroker(t, r.helper, "", r.helper.Log)
	return r
}

// loginStatus is what `agent-secrets machine login-status` reads over the socket.
func (r *stoppedLoopRig) loginStatus(t *testing.T) Response {
	t.Helper()
	return r.served.call(t, Request{Op: "login-status"})
}

// signLauncher is what a machine command's sign-launcher reads over the socket.
func (r *stoppedLoopRig) signLauncher(t *testing.T) Response {
	t.Helper()
	return r.served.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: r.broker.srv.URL + "/v1/operator/machines"})
}

// stoppedLine is the log line the trade loop writes when it stops.
const stoppedLine = "the helper stopped trading this machine's identity: nothing renews its launcher credential, and no session enrols under this machine's credential once that lapses, until the helper restarts"

// TestAStoppedTradeLoopSaysNothingTradesAgain: a helper in identity mode whose trade the broker
// refuses in a way no retry cures (a machine no entry of the deployment lists) stops its trade
// loop, and from then on tells a session and the operator that, not that a trade is coming: the
// enrolment's error, the socket's NO_CREDENTIAL answer to a machine command, and login-status,
// whose answer carries identity_stopped. None names a login, which would not list the machine
// either. Its log gives the reason, the broker's own refusal.
func TestAStoppedTradeLoopSaysNothingTradesAgain(t *testing.T) {
	const refusal = "The subject token was refused: no entry lists the Google account box@fleet.iam.gserviceaccount.com, which the actor token names as the machine."
	r := newStoppedLoopRig(t, &stopAnswer{status: http.StatusBadRequest,
		body: map[string]string{"error": "invalid_request", "error_description": refusal}})
	line := waitForRecord(t, r.out, stoppedLine)
	if said, _ := line["error"].(string); !strings.Contains(said, refusal) {
		t.Fatalf("the stopped loop's line carries %v; want the broker's refusal %q", line, refusal)
	}

	stopped := "no machine credential; " + cipherclient.Hint(cipherclient.FailureIdentityStopped)
	if _, _, err := r.helper.EnrollSessionKey(context.Background(), "box-after-the-stop", sessionThumbprint(t), nil); !errors.Is(err, errNoCredential) || err.Error() != stopped {
		t.Fatalf("an enrolment after the loop stopped answered %v; want %q", err, stopped)
	}
	if signed := r.signLauncher(t); signed.Code != CodeNoCredential || signed.Error != stopped {
		t.Fatalf("sign-launcher after the loop stopped answered %+v; want NO_CREDENTIAL %q", signed, stopped)
	}
	status := r.loginStatus(t)
	if !status.MachineIdentity || !status.IdentityStopped || status.NoCredentialFailure() != cipherclient.FailureIdentityStopped {
		t.Fatalf("login-status after the loop stopped answered %+v; want machine_identity and identity_stopped set", status)
	}
	for _, said := range []string{stopped, r.out.String()} {
		if strings.Contains(said, "login") || strings.Contains(said, "is trading") {
			t.Fatalf("the helper whose loop stopped said %q: it names a login or a trade to wait for, and neither comes", said)
		}
	}
	if n := r.requests.Load(); n != 1 {
		t.Fatalf("the helper made %d token requests; want one, the refused trade that stopped its loop", n)
	}
}

// TestAStoppedTradeLoopHoldingACredentialPromisesNoRenewal: the loop stops while the credential
// it last traded for is still live, which is what a stop looks like for the hour it lasts. The
// helper keeps serving sessions with that credential, and says so: sign-launcher signs, with
// machine_identity and identity_stopped both set, and login-status is `issued` and exits 0, while
// its line says nothing renews the credential and that no session enrols under this machine's
// credential once it lapses. At its expiry the drop line, the enrolment's error and login-status
// are the stopped state. The stopping trade is the one token request made from the moment the
// test triggers it, at the stop and through the expiry.
func TestAStoppedTradeLoopHoldingACredentialPromisesNoRenewal(t *testing.T) {
	r := newStoppedLoopRig(t, nil)
	first := r.helper.cred.Load()
	if first == nil {
		t.Fatal("the helper holds no credential, so this test proves nothing about a stop that holds one")
	}
	before := r.requests.Load()
	r.refusal.Store(&stopAnswer{noCredential: true})
	r.waits.fire(t, 0)
	line := waitForRecord(t, r.out, stoppedLine)
	if said, _ := line["error"].(string); !strings.Contains(said, "carries no credential_id") {
		t.Fatalf("the stopped loop's line carries %v; want the answer's own words, which name no URL", line)
	}
	if at := r.requests.Load(); at != before+1 {
		t.Fatalf("the helper made %d token requests from the stopping trade to its stop line; want one, the trade that stopped it", at-before)
	}
	stoppedAt := before + 1

	signed := r.signLauncher(t)
	if !signed.OK || signed.CredentialID != first.id || !signed.MachineIdentity || !signed.IdentityStopped {
		t.Fatalf("sign-launcher while the stopped loop still holds its credential answered %+v; want a proof for %s with both flags set", signed, first.id)
	}
	status := r.loginStatus(t)
	if !status.CredentialHeld || status.LoginState != "issued" || status.NoCredentialFailure() != cipherclient.FailureIdentityStopped {
		t.Fatalf("login-status while the stopped loop still holds its credential answered %+v; want issued, held, and the stopped class", status)
	}
	lapse := cipherclient.Lapse(status.NoCredentialFailure(), status.CredentialExpiresAt, "59m")
	if strings.Contains(lapse, "renews") && !strings.HasPrefix(lapse, "nothing renews") {
		t.Fatalf("login-status's line while the stopped loop holds a credential reads %q; want it to promise no renewal", lapse)
	}

	r.expiry.fire(t, first, 0)
	waitFor(t, func() bool { return !r.helper.HasCredential() })
	waitForRecord(t, r.out, dropExpired+"; cleared: no session can enroll under this machine's credential; "+cipherclient.Hint(cipherclient.FailureIdentityStopped))
	stopped := "no machine credential; " + cipherclient.Hint(cipherclient.FailureIdentityStopped)
	if _, _, err := r.helper.EnrollSessionKey(context.Background(), "box-after-the-lapse", sessionThumbprint(t), nil); !errors.Is(err, errNoCredential) || err.Error() != stopped {
		t.Fatalf("an enrolment after the lapse answered %v; want %q", err, stopped)
	}
	if status := r.loginStatus(t); status.CredentialHeld || !status.IdentityStopped {
		t.Fatalf("login-status after the lapse answered %+v; want no credential held and identity_stopped still set", status)
	}
	if after := r.requests.Load(); after != stoppedAt {
		t.Fatalf("the helper made %d token requests after its loop stopped; want none past the %d before it", after-stoppedAt, stoppedAt)
	}
}

// TestAStoppedFlagIsNotSetByAShutdown: a helper whose context ends stops trading because it was
// told to, which is no fault of the deployment's, so nothing says the loop stopped.
func TestAStoppedFlagIsNotSetByAShutdown(t *testing.T) {
	r := newStoppedLoopRig(t, nil)
	if r.helper.identityStopped.Load() {
		t.Fatal("a helper that traded a credential already reports its loop stopped")
	}
	r.cancel()
	waitForRecord(t, r.out, identityShutdownLine)
	if r.helper.identityStopped.Load() {
		t.Fatal("a shutdown set the stopped flag; a helper told to stop names no fault of the deployment's")
	}
	if said := r.out.String(); strings.Contains(said, stoppedLine) {
		t.Fatalf("a shutdown logged the stopped line:\n%s", said)
	}
}
