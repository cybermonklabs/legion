// Package identitytest holds the one contract every caller identity provider runs
// (identity/boot's TestEachRegisteredProviderRunsTheContract fails a registered provider whose
// package's tests call no Contract). It is an ordinary package rather than a _test.go file so each
// provider's own tests import it.
package identitytest

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TradeFixture is one exchange the provider decides: a subject token of TokenType carrying Claims,
// with Actor as its actor token's claims (nil for none), and the principal it must decide.
type TradeFixture struct {
	TokenType     string
	Claims, Actor map[string]any
	WantPrincipal string
}

// Fixture is one provider under the contract. New builds it trusting issuer, whose keys are keys,
// for audience: every token type it configures names that issuer and that audience. A valid token
// is the issuer's standard claims for Subject and Audience with Claims merged over them, and it
// verifies to the account Want. Trades are the exchanges it decides, at least one.
type Fixture struct {
	New      func(t *testing.T, issuer string, keys *oidc.Keys, audience string) identity.Provider
	Audience string
	Subject  string
	Claims   map[string]any
	Want     identity.Account
	Trades   []TradeFixture
}

// contractSession is the session name every trade of the contract asks for.
const contractSession = "contract-session"

// Contract holds the provider f builds to the meaning of a caller identity provider (spec R7): it
// answers its kind; a valid token verifies to the expected account and subject; a token for
// another audience, of another issuer, or past its expiry is refused with that oidc class; each
// configured subject type names the configured issuer and audience; and each trade decides its
// principal. Each case is a subtest, so `go test -v` shows every case of every provider.
func Contract(t *testing.T, f Fixture) {
	t.Helper()
	if len(f.Trades) == 0 {
		t.Fatal("identitytest: the fixture lists no trade; a provider registers a subject type, and the contract holds what it decides")
	}
	ctx := context.Background()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "contract-key")
	keys, err := oidc.NewKeys(ctx, issuer.URL())
	if err != nil {
		t.Fatalf("identitytest: read the issuer's keys: %v", err)
	}
	p := f.New(t, issuer.URL(), keys, f.Audience)
	token := func(claims map[string]any) string {
		merged := maps.Clone(claims)
		maps.Copy(merged, f.Claims)
		return issuer.Mint(t, key, merged)
	}
	verify := func(raw string) (identity.Caller, error) {
		return p.CallerVerifier(f.Audience).VerifyCaller(ctx, raw)
	}

	t.Run("its kind", func(t *testing.T) {
		if p.Kind() != f.Want.Kind {
			t.Errorf("Kind() = %q, want %q", p.Kind(), f.Want.Kind)
		}
	})
	t.Run("a valid token verifies to its account and subject", func(t *testing.T) {
		caller, err := verify(token(issuer.Claims(f.Subject, f.Audience)))
		if err != nil {
			t.Fatalf("VerifyCaller(a valid token) = %v", err)
		}
		if caller.Account != f.Want || caller.Subject != f.Subject || caller.Expiry.IsZero() {
			t.Errorf("VerifyCaller(a valid token) = %+v, want account %+v, subject %q and an expiry", caller, f.Want, f.Subject)
		}
	})
	t.Run("a token for another audience is refused", func(t *testing.T) {
		claims := issuer.Claims(f.Subject, "https://elsewhere.example")
		if _, err := verify(token(claims)); !errors.Is(err, oidc.ErrAudience) {
			t.Errorf("VerifyCaller(another audience's token) = %v, want oidc.ErrAudience", err)
		}
	})
	t.Run("a token of another issuer is refused", func(t *testing.T) {
		other := oidctest.New(t)
		otherKey := other.PublishKey(t, "contract-key")
		merged := other.Claims(f.Subject, f.Audience)
		maps.Copy(merged, f.Claims)
		if _, err := verify(other.Mint(t, otherKey, merged)); !errors.Is(err, oidc.ErrIssuer) {
			t.Errorf("VerifyCaller(another issuer's token) = %v, want oidc.ErrIssuer", err)
		}
	})
	t.Run("a token past its expiry is refused", func(t *testing.T) {
		claims := issuer.Claims(f.Subject, f.Audience)
		claims["iat"], claims["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		if _, err := verify(token(claims)); !errors.Is(err, oidc.ErrExpired) {
			t.Errorf("VerifyCaller(an expired token) = %v, want oidc.ErrExpired", err)
		}
	})
	t.Run("each subject type names the configured issuer and audience", func(t *testing.T) {
		var subjects int
		for _, tt := range p.TokenTypes() {
			if tt.ActorOnly || !tt.Configured() {
				continue
			}
			subjects++
			if tt.Issuer != issuer.URL() || tt.Audience != f.Audience {
				t.Errorf("token type %s names issuer %q and audience %q, want %q and %q", tt.Name, tt.Issuer, tt.Audience, issuer.URL(), f.Audience)
			}
		}
		if subjects == 0 {
			t.Error("the provider configured no subject type for its issuer and audience")
		}
	})
	t.Run("each trade decides its principal", func(t *testing.T) {
		for _, trade := range f.Trades {
			claims, err := p.Decide(identity.Trade{TokenType: trade.TokenType, Subject: trade.Claims, Actor: trade.Actor, Session: contractSession})
			if err != nil {
				t.Errorf("Decide(%s) = %v", trade.TokenType, err)
				continue
			}
			if claims.Principal != trade.WantPrincipal {
				t.Errorf("Decide(%s).Principal = %q, want %q", trade.TokenType, claims.Principal, trade.WantPrincipal)
			}
		}
	})
}
