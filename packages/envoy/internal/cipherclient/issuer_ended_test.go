// packages/envoy/internal/cipherclient/issuer_ended_test.go
//
// What the client does once the issuer ends a person's sign-in: the device flow's renewal of the
// kept refresh token is refused 400 invalid_grant, which DeviceFlow.Refresh answers as
// ErrSignInEnded to the PersonIdentity every trade of that sign-in reads. That is as final as the
// broker's permanent refusal, whatever the client still holds: Token drops the token in hand
// though it is still live, Run stops rather than waiting to trade again, and SignInAuthority
// remembers the sign-in as refused, so the box enrols under its own machine. The controls are the
// failures a later trade can cure, which keep the token in hand and the loop trading: an issuer
// that is down, one that is down and names invalid_grant in its body
// (TestOnlyTheIssuersRefusalEndsASignIn), and the broker answering that Dispatch holds the person
// (membership_held).
//
// The named reverts: drop ErrSignInEnded from finalRefusal (brokerclient.go), and Token answers
// the live token in hand, Current the person's credential with nothing remembered, and Run waits
// to trade again, so each of the three tests below fails on its first row; read invalid_grant at
// any status as the end of the sign-in (device.go), and each fails on its third.

package cipherclient

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
)

// endedPerson is the person signed in on the box in these tests.
const endedPerson = "ada@example.com"

// renewalFailure is one way the renewal of a person's sign-in fails after its first trade landed.
type renewalFailure struct {
	name string
	// issuerStatus and issuerCode are the issuer's answer to the refresh of the kept refresh
	// token; a zero issuerStatus is an issuer that grants it.
	issuerStatus int
	issuerCode   string
	// held is the broker answering every trade after the first that Dispatch holds the person.
	held bool
	// final is whether the failure ends the sign-in, which no later trade cures.
	final bool
	// trades is how many trades the broker sees: the first, and the renewal's where the issuer
	// granted the refresh it needs.
	trades int
}

var renewalFailures = []renewalFailure{
	{name: "the issuer ends the sign-in", issuerStatus: 400, issuerCode: "invalid_grant", final: true, trades: 1},
	{name: "the issuer is down", issuerStatus: 503, issuerCode: "temporarily_unavailable", trades: 1},
	{name: "the issuer is down and names invalid_grant", issuerStatus: 503, issuerCode: "invalid_grant", trades: 1},
	{name: "the broker holds the person", held: true, trades: 2},
}

// newRenewalRig is a box with endedPerson signed in, whose broker mints access tokens that live a
// minute: inside DefaultRenewBefore, so every Token and every renewal of Run asks the issuer for a
// fresh identity token first, and live for the whole test, so the token in hand is one Token
// could still answer. Under f.held, the broker refuses every trade after the first
// membership_held.
func newRenewalRig(t *testing.T, f renewalFailure) (*signInRig, SignInRecord) {
	t.Helper()
	rig := newSignInRig(t, func(n int, _ url.Values) (int, map[string]any) {
		if f.held && n > 0 {
			return 400, map[string]any{"error": oauthwire.MembershipHeld, "error_description": heldDescription}
		}
		answer := mintedToken(n + 1)
		answer["expires_in"] = 60
		return 200, answer
	})
	return rig, rig.signIn(t, endedPerson)
}

// apply makes the issuer answer f's refusal to every refresh from now on; the broker's hold is
// already in its answers.
func (f renewalFailure) apply(rig *signInRig) {
	if f.issuerStatus != 0 {
		rig.issuer.FailToken(f.issuerStatus, f.issuerCode)
	}
}

// personClient is the client `agent-secrets login` and SignInAuthority build for record: its
// subject the person's own identity, renewed through the rig's device flow, and this machine as
// the actor.
func personClient(t *testing.T, rig *signInRig, record SignInRecord, after func(time.Duration) <-chan time.Time) *Client {
	t.Helper()
	client, err := New(Config{
		BrokerURL: rig.broker.Address(),
		Key:       testKey(t),
		Subject:   &PersonIdentity{Flow: rig.flow, RefreshToken: record.RefreshToken, Person: record.Person},
		Actor:     rig.authority.Machine,
		Session:   SessionFor(record),
		After:     after,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// TestASignInTheIssuerEndedIsNotServedFromTheTokenInHand: once the issuer ends the sign-in, Token
// answers ErrSignInEnded and holds nothing, though the token the sign-in traded for is still
// live; with the issuer then down as well, Token answers that failure rather than the old token.
// A failure a later trade can cure answers the token in hand.
func TestASignInTheIssuerEndedIsNotServedFromTheTokenInHand(t *testing.T) {
	for _, f := range renewalFailures {
		t.Run(f.name, func(t *testing.T) {
			rig, record := newRenewalRig(t, f)
			client := personClient(t, rig, record, nil)
			ctx := context.Background()
			first, err := client.Token(ctx)
			if err != nil {
				t.Fatalf("the first trade of %s's sign-in: %v", endedPerson, err)
			}
			f.apply(rig)
			token, err := client.Token(ctx)
			switch {
			case !f.final && (err != nil || token == nil || token.AccessToken != first.AccessToken):
				t.Fatalf("Token once %s = %+v, %v; want the token in hand, %q, which is still live: a later trade can cure this", f.name, token, err, first.AccessToken)
			case f.final && (token != nil || !errors.Is(err, ErrSignInEnded)):
				t.Fatalf("Token once the issuer ended the sign-in = %+v, %v; want no token and ErrSignInEnded: the sign-in is over, though the token in hand is still live", token, err)
			}
			if trades := len(rig.broker.Requests()); trades != f.trades {
				t.Errorf("the broker saw %d trades; want %d", trades, f.trades)
			}
			if !f.final {
				return
			}
			rig.issuer.FailToken(503, "temporarily_unavailable")
			if token, err := client.Token(ctx); token != nil || err == nil {
				t.Fatalf("Token after the sign-in ended, with the issuer then down = %+v, %v; want the failure: the client holds no token the ended sign-in traded for", token, err)
			}
		})
	}
}

// TestASignInTheIssuerEndedStopsTheRenewalLoop: Run renews a person's sign-in at the issuer, and
// once the issuer ends it Run returns ErrSignInEnded at once, with no wait to trade again. A
// failure a later trade can cure is waited out from defaultRetryWait, the loop ending with no
// error when its context does.
func TestASignInTheIssuerEndedStopsTheRenewalLoop(t *testing.T) {
	for _, f := range renewalFailures {
		t.Run(f.name, func(t *testing.T) {
			rig, record := newRenewalRig(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The first wait is the renewal of the token that landed, which fires at once so the
			// loop renews now. Every wait after it is a retry: it ends the loop's context, so a
			// loop that waits to trade again returns nil.
			var waits []time.Duration
			client := personClient(t, rig, record, func(d time.Duration) <-chan time.Time {
				waits = append(waits, d)
				if len(waits) == 1 {
					fired := make(chan time.Time, 1)
					fired <- time.Now()
					return fired
				}
				cancel()
				return make(chan time.Time)
			})
			landed := 0
			err := client.Run(ctx, func(context.Context, *Token) error {
				landed++
				f.apply(rig)
				return nil
			})
			if landed != 1 {
				t.Fatalf("Run handed its caller %d tokens; want one, the first trade's", landed)
			}
			switch {
			case f.final && (!errors.Is(err, ErrSignInEnded) || len(waits) != 1):
				t.Fatalf("Run once the issuer ended the sign-in = %v, after the waits %v; want ErrSignInEnded with no wait after the renewal: no retry cures it", err, waits)
			case !f.final && (err != nil || len(waits) != 2 || waits[1] != defaultRetryWait):
				t.Fatalf("Run once %s = %v, after the waits %v; want a wait of %s to trade again, and no error when its context ends", f.name, err, waits, defaultRetryWait)
			}
			if trades := len(rig.broker.Requests()); trades != f.trades {
				t.Errorf("the broker saw %d trades; want %d", trades, f.trades)
			}
		})
	}
}

// TestAnIssuerEndingASignInWhoseTokenIsStillLiveFallsBackToTheMachine: the issuer's refusal is as
// final when the authority holds a live token as when it holds none
// (TestARefreshTokenTheIssuerNoLongerHonoursFallsBackToTheMachine). The enrolment answers no
// authority and no error, so it enrols under this machine; the refusal is remembered, which the
// issuer granting again proves, since the broker sees no trade after it; and it is logged once.
// A failure a later trade can cure keeps the person's credential, logging no refusal.
func TestAnIssuerEndingASignInWhoseTokenIsStillLiveFallsBackToTheMachine(t *testing.T) {
	const refusedLine = "this machine's sign-in was refused; sessions enrol under this machine's own authority"
	for _, f := range renewalFailures {
		t.Run(f.name, func(t *testing.T) {
			rig, _ := newRenewalRig(t, f)
			logged := &logtest.Recorder{}
			rig.authority.Log = slog.New(logged)
			ctx := context.Background()
			first, err := rig.authority.Current(ctx)
			if err != nil || first == nil || first.CredentialID != "credential-1" {
				t.Fatalf("the first enrolment answered %+v, %v; want credential-1", first, err)
			}
			f.apply(rig)
			authority, err := rig.authority.Current(ctx)
			switch {
			case !f.final && (err != nil || authority == nil || authority.CredentialID != "credential-1" || authority.Operator != endedPerson):
				t.Fatalf("the enrolment once %s answered %+v, %v; want %s's credential-1, which is still live: a later trade can cure this", f.name, authority, err, endedPerson)
			case f.final && (authority != nil || err != nil):
				t.Fatalf("the enrolment once the issuer ended the sign-in answered %+v, %v; want no authority and no error, so the session enrols under this machine", authority, err)
			}
			if f.final {
				rig.issuer.FailToken(0, "")
				if authority, err := rig.authority.Current(ctx); authority != nil || err != nil {
					t.Fatalf("the issuer granting again answered %+v, %v; want the refusal remembered until somebody signs in afresh", authority, err)
				}
			}
			if trades := len(rig.broker.Requests()); trades != f.trades {
				t.Errorf("the broker saw %d trades; want %d", trades, f.trades)
			}
			var refusals []map[string]string
			for _, record := range logged.Records() {
				if record.Message == refusedLine {
					refusals = append(refusals, attrsOf(t, record))
				}
			}
			switch {
			case f.final && (len(refusals) != 1 || refusals[0]["person"] != endedPerson || refusals[0]["code"] != "invalid_grant"):
				t.Errorf("the authority logged the refusals %v; want one, naming %s and invalid_grant", refusals, endedPerson)
			case !f.final && len(refusals) != 0:
				t.Errorf("the authority logged the refusals %v once %s; want none", refusals, f.name)
			}
		})
	}
}
