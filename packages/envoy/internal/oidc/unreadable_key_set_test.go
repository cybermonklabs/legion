package oidc

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// An issuer's key set that cannot be read reaches no verdict on a token, and the error says how long
// to wait before asking again: the FailureSpacing of the Keys whose read failed (5 s unless set),
// which Key puts on its ErrKeySet and Verify's ErrIssuerUnavailable keeps, for RetryAfter to read.
// A token the issuer's keys settle carries no wait: one naming a key id the readable set does not
// hold is a signature failure, and an expired token is expired while the set fails too, since
// classify reads the token's own claims before the key set's failure.
func TestAnUnreadableKeySetCarriesItsFailureSpacingAsTheRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spacing time.Duration // zero keeps the Keys' own
		want    time.Duration
	}{
		{"default spacing", 0, 5 * time.Second},
		{"2.5 s spacing", 2500 * time.Millisecond, 2500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := oidctest.New(t)
			keys, signing := keysFor(t, issuer, "signing-key")
			if tc.spacing != 0 {
				keys.FailureSpacing = tc.spacing
			}
			issuer.FailKeys(http.StatusServiceUnavailable)

			_, err := NewWithKeys(issuer.URL(), testAudience, keys).Verify(context.Background(), issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience)))
			if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, ErrSignature) {
				t.Fatalf("Verify while the key set answers 503: err = %v, want ErrIssuerUnavailable", err)
			}
			if wait, ok := RetryAfter(err); !ok || wait != tc.want {
				t.Errorf("RetryAfter(Verify's error) = %s, %t; want %s, true", wait, ok, tc.want)
			}

			_, err = keys.Key(context.Background(), signing.KeyID(), testAlgorithm)
			if !errors.Is(err, ErrKeySet) {
				t.Fatalf("Key while the key set answers 503: err = %v, want ErrKeySet", err)
			}
			if wait, ok := RetryAfter(err); !ok || wait != tc.want {
				t.Errorf("RetryAfter(Key's error) = %s, %t; want %s, true", wait, ok, tc.want)
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		issuer := oidctest.New(t)
		keys, _ := keysFor(t, issuer, "signing-key")
		forged := issuer.Mint(t, oidctest.NewKey(t, "never-published"), issuer.Claims(testSubject, testAudience))
		_, err := NewWithKeys(issuer.URL(), testAudience, keys).Verify(context.Background(), forged)
		if !errors.Is(err, ErrSignature) || errors.Is(err, ErrIssuerUnavailable) {
			t.Fatalf("Verify of a token under a key id the readable set does not hold: err = %v, want ErrSignature", err)
		}
		if wait, ok := RetryAfter(err); ok {
			t.Errorf("RetryAfter(Verify's error) = %s, true; want no wait", wait)
		}
	})

	t.Run("expired while unreadable", func(t *testing.T) {
		issuer := oidctest.New(t)
		keys, signing := keysFor(t, issuer, "signing-key")
		issuer.FailKeys(http.StatusServiceUnavailable)
		expired := issuer.Claims(testSubject, testAudience)
		expired["iat"], expired["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		_, err := NewWithKeys(issuer.URL(), testAudience, keys).Verify(context.Background(), issuer.Mint(t, signing, expired))
		if !errors.Is(err, ErrExpired) || errors.Is(err, ErrIssuerUnavailable) {
			t.Fatalf("Verify of an expired token while the key set answers 503: err = %v, want ErrExpired", err)
		}
		if wait, ok := RetryAfter(err); ok {
			t.Errorf("RetryAfter(Verify's error) = %s, true; want no wait", wait)
		}
		// go-oidc checks the signature before any claim, so the failing set was asked.
		if issuer.JWKSFetches() == 0 {
			t.Fatal("the key set was never read: the row did not meet the outage")
		}
	})
}

// A sign-in's ID token is checked against keys the code flow reads through a Keys of its own, so a
// code exchange whose ID-token check meets a key set that cannot be read is the issuer unavailable,
// carrying that Keys' FailureSpacing as its wait, for either exchange: Exchange (Dispatch's
// sign-in) and OnlineExchange (the broker's approval sign-in). An ID token signed by another key
// under the published key id, while the set reads, is a signature failure, never the issuer
// unavailable, and carries no wait.
func TestACodeFlowExchangeThroughAnUnreadableKeySetIsIssuerUnavailable(t *testing.T) {
	exchanges := []struct {
		name string
		// exchange signs a person in through flow, from the authorization to the verified ID token.
		exchange func(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow) error
	}{
		{"Exchange", func(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow) error {
			issuer.SignInAs(personClaims("dispatch-members"))
			code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
			_, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
			return err
		}},
		{"OnlineExchange", func(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow) error {
			issuer.SignInAs(googleClaims(true))
			signIn := onlineSignIn("nonce-1")
			code, _ := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
			_, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn)
			return err
		}},
	}

	for _, each := range exchanges {
		t.Run(each.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			if _, err := flow.discovery.discover(context.Background()); err != nil {
				t.Fatalf("discover: %v", err)
			}
			issuer.FailKeys(http.StatusServiceUnavailable)
			err := each.exchange(t, issuer, flow)
			if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, ErrSignature) {
				t.Fatalf("%s whose ID-token check meets a key set answering 503: err = %v, want ErrIssuerUnavailable", each.name, err)
			}
			if wait, ok := RetryAfter(err); !ok || wait != DefaultKeysFailureSpacing {
				t.Errorf("RetryAfter(%s's error) = %s, %t; want the flow's key set's FailureSpacing, %s, true", each.name, wait, ok, DefaultKeysFailureSpacing)
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		for _, each := range exchanges {
			issuer := oidctest.New(t)
			published := issuer.PublishKey(t, "signing-key")
			// A foreign key under the published key's id: the key set reads and resolves, and only
			// the signature fails.
			issuer.EnableCodeFlow(oidctest.NewKey(t, published.KeyID()), flowClientID, flowClientSecret, flowRedirect)
			flow := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, flowDomain)
			err := each.exchange(t, issuer, flow)
			if !errors.Is(err, ErrSignature) || errors.Is(err, ErrIssuerUnavailable) {
				t.Errorf("%s of an ID token signed by another key under the published key id: err = %v, want ErrSignature", each.name, err)
			}
			if wait, ok := RetryAfter(err); ok {
				t.Errorf("RetryAfter(%s's error) = %s, true; want no wait", each.name, wait)
			}
		}
	})
}
