package machine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
)

// TestAServiceTokenWhoseIssuerCannotBeReadIsErrIssuerUnavailable: a service's token that the
// verifier could not judge, because its issuer's discovery document or key set cannot be read, is
// ErrIssuerUnavailable, which wraps oidc.ErrIssuerUnavailable and is not ErrTokenInvalid, so the
// token login and the service routes answer it as an outage. A token of an issuer the broker does
// not trust is still ErrTokenInvalid.
func TestAServiceTokenWhoseIssuerCannotBeReadIsErrIssuerUnavailable(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	withTokenAccount(svc, time.Now(), time.Now().Add(time.Hour))
	for _, c := range []struct {
		name        string
		verdict     error
		unavailable bool
	}{
		{"its issuer cannot be read", fmt.Errorf("%w: the discovery document answered 503", oidc.ErrIssuerUnavailable), true},
		{"its issuer is not one the broker trusts", fmt.Errorf("%w: no issuer of the set names it", oidc.ErrIssuer), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc.Enroll.Pod = fakeTokens{err: c.verdict}
			_, err := svc.VerifyServiceToken(ctx, "the-daemon's-token")
			switch {
			case c.unavailable && (!errors.Is(err, ErrIssuerUnavailable) || !errors.Is(err, oidc.ErrIssuerUnavailable) || errors.Is(err, ErrTokenInvalid)):
				t.Fatalf("VerifyServiceToken = %v, want ErrIssuerUnavailable, wrapping oidc.ErrIssuerUnavailable and not ErrTokenInvalid", err)
			case !c.unavailable && (!errors.Is(err, ErrTokenInvalid) || errors.Is(err, ErrIssuerUnavailable)):
				t.Fatalf("VerifyServiceToken = %v, want ErrTokenInvalid", err)
			}
		})
	}
}

// TestAServiceTokenIsTakenOnlyFromTheIssuersItsEntryNames: a listed account's token is taken only
// from an issuer its BROKER_SERVICE_LOGINS entry names; the same account of another named issuer is
// ErrAccountNotListed, as an account no entry lists is. The credential a login issues records the
// URL of the issuer whose token made it.
func TestAServiceTokenIsTakenOnlyFromTheIssuersItsEntryNames(t *testing.T) {
	first := oidc.NamedIssuer{Name: "first", URL: "https://first.example"}
	second := oidc.NamedIssuer{Name: "second", URL: "https://second.example"}
	for _, c := range []struct {
		name   string
		names  []string
		issuer oidc.NamedIssuer
		listed bool
	}{
		{"the first issuer's token, its entry naming the first", []string{"first"}, first, true},
		{"the second issuer's token, its entry naming the first alone", []string{"first"}, second, false},
		{"the second issuer's token, its entry naming both", []string{"first", "second"}, second, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newFixture(t)
			ctx := context.Background()
			svc.Enroll.Issuers = []oidc.NamedIssuer{first, second}
			svc.Enroll.Pod = fakeTokens{claims: enroll.AccountClaims{
				Subject: daemonTokenAccount, IssuedAt: time.Now(), Expiry: time.Now().Add(time.Hour), Issuer: c.issuer,
			}}
			svc.Enroll.ServiceLogins = map[string]enroll.ServiceLogin{daemonTokenAccount: {Service: "legion-daemon", Issuers: c.names}}
			account, err := svc.VerifyServiceToken(ctx, "the-daemon's-token")
			if !c.listed {
				if !errors.Is(err, ErrAccountNotListed) {
					t.Fatalf("VerifyServiceToken = %v, want ErrAccountNotListed: the entry names %v, and the token is %s's", err, c.names, c.issuer.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyServiceToken = %v, want the account: the entry names %v", err, c.names)
			}
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			compact, err := record.Sign(key, testAudience, []record.AuthorizationDetail{
				{Type: "launcher_credential", Identifier: "legion-daemon", Service: "legion-daemon"},
			}, "", "", nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			issued, err := svc.TokenLogin(ctx, account, compact)
			if err != nil {
				t.Fatalf("TokenLogin: %v", err)
			}
			var recorded *string
			if err := svc.Store.Pool.QueryRow(ctx, `select issuer from launcher_credentials where id=$1`, issued.ID).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if recorded == nil || *recorded != c.issuer.URL {
				t.Fatalf("the credential records the issuer %v, want %s, whose token made it", recorded, c.issuer.URL)
			}
			assertLauncherAuthenticates(t, svc, key, issued.ID)
		})
	}
}
