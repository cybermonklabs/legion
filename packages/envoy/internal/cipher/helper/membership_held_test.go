// packages/envoy/internal/cipher/helper/membership_held_test.go
//go:build linux

// What the helper keeps while Dispatch holds the person its credential names. A hold is not a
// revocation: it ends at the person's next Dispatch sign-in, or the next refresh that confirms
// them, and the broker then serves the credential the helper already holds. So the helper drops
// nothing because of it. In machine-login mode the broker refuses the launcher routes 403
// MEMBERSHIP_HELD and still answers the refresh grant, so the login stays and its expiry moves
// on. In identity mode the trade itself is refused `membership_held`: the trade loop goes on
// trading with its backoff, the one-hour credential lapses at its expiry as it does every hour,
// and the first trade after the release brings one back.
//
// Like identitywake_test.go's, these drive Broker's launcher calls against an httptest server
// directly: no unix socket and no pidfd.

package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldPerson is the operator Dispatch holds in these tests.
const heldPerson = "ada@example.com"

// heldDescription is the token endpoint's description of its membership_held refusal.
const heldDescription = heldPerson + "'s credentials are held by Dispatch; they work again when " + heldPerson + " signs in to Dispatch, or when Dispatch can confirm " + heldPerson + " with Google again"

// TestMembershipHeldHelperKeepsItsLogin: the helper discards no credential because Dispatch holds
// its person, in either mode, and is served again after the release with no new login.
func TestMembershipHeldHelperKeepsItsLogin(t *testing.T) {
	t.Parallel()
	t.Run("machine login", func(t *testing.T) {
		ctx := context.Background()
		f := newFakeBroker(t)
		b := loggedInBroker(t, f, heldPerson)
		cred := b.cred.Load()
		before, err := LoadMachineFile(b.MachineFile)
		if err != nil {
			t.Fatalf("the machine file after the login: %v", err)
		}
		sess, _ := newSession(1, 1, "h:1:1", "", nil, nil)

		f.mu.Lock()
		f.enrollHeldFor = heldPerson
		f.mu.Unlock()
		_, _, err = b.EnrollIdentity(ctx, sess.identity())
		var be *BrokerError
		if !errors.As(err, &be) || be.Status != http.StatusForbidden || be.Code != "MEMBERSHIP_HELD" {
			t.Fatalf("an enrolment while %s is held answered %v; want the broker's 403 MEMBERSHIP_HELD", heldPerson, err)
		}
		if b.cred.Load() != cred {
			t.Fatal("the helper dropped its machine login on 403 MEMBERSHIP_HELD; a hold ends at the person's next sign-in, and the login it holds is the one the broker serves again then")
		}
		if got := b.clearOnInvalid(ctx, cred, err); got != err || b.cred.Load() != cred {
			t.Fatalf("clearOnInvalid on 403 MEMBERSHIP_HELD answered %v and the helper holds %p; want the refusal as it came and the login %p kept", got, b.cred.Load(), cred)
		}
		if status := b.LoginStatus(); status.State != "issued" || status.Refused || !status.CredentialHeld {
			t.Fatalf("login-status while %s is held: %+v; want the login issued and held, not refused", heldPerson, status)
		}

		// The refresh grant is upkeep, which the broker serves through a hold, and the expiry it
		// answers is later than the one the login had.
		f.mu.Lock()
		f.loginLifetime = 8 * 24 * time.Hour
		f.mu.Unlock()
		if err := b.refresh(ctx); err != nil {
			t.Fatalf("the refresh while %s is held: %v", heldPerson, err)
		}
		after, err := LoadMachineFile(b.MachineFile)
		if err != nil {
			t.Fatalf("the machine file after the refresh: %v", err)
		}
		if after.CredentialID != before.CredentialID || !after.ExpiresAt.After(before.ExpiresAt) {
			t.Fatalf("the machine file after the refresh holds %s until %s; want %s, the same login, past %s", after.CredentialID, after.ExpiresAt, before.CredentialID, before.ExpiresAt)
		}
		if held := b.cred.Load(); held == nil || held.id != before.CredentialID || !held.expiresAt.Equal(after.ExpiresAt) {
			t.Fatalf("the helper holds %+v after the refresh; want %s until %s", held, before.CredentialID, after.ExpiresAt)
		}
		if _, _, err := b.EnrollIdentity(ctx, sess.identity()); !errors.As(err, &be) || be.Code != "MEMBERSHIP_HELD" || !b.HasCredential() {
			t.Fatalf("an enrolment after the refresh, still held, answered %v with a credential held %t; want 403 MEMBERSHIP_HELD and the login kept", err, b.HasCredential())
		}

		f.mu.Lock()
		f.enrollHeldFor = ""
		f.mu.Unlock()
		if _, _, err := b.EnrollIdentity(ctx, sess.identity()); err != nil {
			t.Fatalf("the first enrolment after the release: %v", err)
		}
		if sub := f.lastProofSubject(t); sub.LauncherID != before.CredentialID {
			t.Fatalf("the first enrolment after the release was signed for %q; want %q, the login the helper kept", sub.LauncherID, before.CredentialID)
		}
	})

	t.Run("identity", func(t *testing.T) {
		f := newEnrolBroker(t)
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		thumbprint, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		// The trade stand-in answers membership_held while held is set, and mints otherwise.
		trader := http.NewServeMux()
		trades := serveTokenExchange(t, f, trader, thumbprint)
		var held atomic.Bool
		var refusals atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
			if held.Load() {
				refusals.Add(1)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "membership_held", "error_description": heldDescription})
				return
			}
			trader.ServeHTTP(w, r)
		})
		mux.Handle("/", f.srv.Config.Handler)
		f.srv.Config.Handler = mux

		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
			t.Fatal(err)
		}
		waits := &tradeWaits{}
		machine, err := cipherclient.New(cipherclient.Config{
			BrokerURL: cipheraddr.New(f.srv.URL), Key: key, HTTP: f.srv.Client(),
			Subject: cipherclient.NewProjectedToken(tokenFile),
			After:   waits.after,
		})
		if err != nil {
			t.Fatal(err)
		}
		var out syncBuffer
		b := &Broker{
			URL: cipheraddr.New(f.srv.URL), Identity: machine, HTTP: f.srv.Client(),
			OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
			Log:          slog.New(slog.NewJSONHandler(&out, nil)),
		}
		t.Cleanup(b.Close)
		expiries := installExpiryTimers(b)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := b.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		first := b.cred.Load()
		if first == nil {
			t.Fatal("the helper listens with no credential, so this test proves nothing about keeping one")
		}
		// The helper answers a machine command's sign-launcher with its mode, so a command the
		// broker refuses LAUNCHER_INVALID names the cure of identity mode.
		served := serveBroker(t, b, "", b.Log)
		if signed := served.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: f.srv.URL + "/v1/operator/machines"}); !signed.OK ||
			!signed.MachineIdentity || signed.IdentityStopped || signed.CredentialID != first.id {
			t.Fatalf("sign-launcher of a helper in identity mode answered %+v; want a proof for %s with machine_identity set and identity_stopped not", signed, first.id)
		}

		// The renewal comes round while the person is held: each trade is refused, and the loop
		// trades again after each wait, the wait doubling.
		held.Store(true)
		waits.fire(t, 0)
		retry := waits.fire(t, 1)
		again := waits.wait(t, 2).d
		if n := refusals.Load(); n != 2 {
			t.Fatalf("the broker refused %d trades while held; want two, one after each wait the loop ended", n)
		}
		if retry <= 0 || again != 2*retry {
			t.Fatalf("the trade loop waited %s, then %s, after membership_held; want its backoff, each wait double the last", retry, again)
		}

		// The credential the last served trade minted lapses at its own expiry, as it does every
		// hour; nothing about the hold ends it sooner.
		if !b.HasCredential() {
			t.Fatal("the helper dropped its credential at a membership_held trade, before the credential's own expiry")
		}
		expiries.fire(t, first, 0)
		waitFor(t, func() bool { return !b.HasCredential() })
		if status := b.LoginStatus(); status.Dropped != dropExpired {
			t.Fatalf("login-status once the credential lapsed: %+v; want it dropped at its expiry (%q)", status, dropExpired)
		}
		// What the helper says while it holds none names the cure of identity mode, its own trade
		// loop, and no machine login a human approves: the drop line, and the answer to a session
		// that asks to enrol during the lapse.
		const tradeLoop = "the helper's trade loop is trading this machine's identity for a new credential"
		lapsed := waitForRecord(t, &out, dropExpired+"; cleared: no session can enroll under this machine's credential; "+tradeLoop)
		if lapsed["level"] != "ERROR" || lapsed["credential_id"] != first.id {
			t.Fatalf("the drop line in identity mode: %v; want an ERROR naming %s", lapsed, first.id)
		}
		_, _, lapseErr := b.EnrollSessionKey(ctx, "box-during-the-lapse", sessionThumbprint(t), nil, nil)
		if !errors.Is(lapseErr, errNoCredential) || lapseErr.Error() != "no machine credential; "+tradeLoop {
			t.Fatalf("an enrolment during the lapse answered %v; want %q", lapseErr, "no machine credential; "+tradeLoop)
		}
		// login-status tells the CLI the helper's mode, so `machine login-status` names the same
		// cure the helper does.
		if status := (&Server{Broker: b}).loginStatus(); !status.MachineIdentity || cipherclient.Hint(status.NoCredentialFailure()) != tradeLoop {
			t.Fatalf("login-status during the lapse: %+v; want machine_identity set, so its hint is %q", status, tradeLoop)
		}
		if strings.Contains(out.String(), "machine login") {
			t.Fatalf("the helper in identity mode told someone to run a machine login, which is not how this machine gets a credential:\n%s", out.String())
		}

		// The release: the trade the loop is waiting to make brings a credential back.
		held.Store(false)
		waits.fire(t, 2)
		waitFor(t, b.HasCredential)
		next := b.cred.Load()
		if next.id == first.id || trades() != 2 {
			t.Fatalf("after the release the helper holds %s after %d served trades; want a credential the first trade after the release minted, the second served trade", next.id, trades())
		}
		if _, _, err := b.EnrollIdentity(ctx, identity{RuntimeID: "this-box", Thumbprint: sessionThumbprint(t)}); err != nil {
			t.Fatalf("an enrolment after the release: %v", err)
		}
		if enrols, _ := f.calls(); len(enrols) != 1 || enrols[0].credential != next.id {
			t.Fatalf("the enrolments after the release were signed with %+v; want one, with %s", enrols, next.id)
		}
	})
}

// tradeWaits stands in for the trade loop's waits (cipherclient.Config.After): each wait the
// loop asks for is recorded with how long it asked, and ends only when the test ends it.
type tradeWaits struct {
	mu    sync.Mutex
	asked []tradeWait
}

type tradeWait struct {
	d  time.Duration
	ch chan time.Time
}

func (w *tradeWaits) after(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := make(chan time.Time, 1)
	w.asked = append(w.asked, tradeWait{d, ch})
	return ch
}

// wait answers the n-th wait the loop asked for, failing the test when the loop asks for none
// within 5 s: a loop that stopped trading asks for no more.
func (w *tradeWaits) wait(t *testing.T, n int) tradeWait {
	t.Helper()
	var got tradeWait
	if !testwait.Holds(5*time.Second, 5*time.Millisecond, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.asked) <= n {
			return false
		}
		got = w.asked[n]
		return true
	}) {
		t.Fatalf("the trade loop asked for %d waits; want a wait %d: after membership_held it stopped trading, so the credential could come back only at a restart", w.count(), n+1)
	}
	return got
}

// fire ends the n-th wait, once the loop has asked for it, and answers how long that wait was.
// A wait the loop ends with a trade is followed by the next wait it asks for.
func (w *tradeWaits) fire(t *testing.T, n int) time.Duration {
	t.Helper()
	asked := w.wait(t, n)
	asked.ch <- time.Now()
	return asked.d
}

func (w *tradeWaits) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.asked)
}
