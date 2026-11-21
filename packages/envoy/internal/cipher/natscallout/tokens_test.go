package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/oidc"
)

type stubK8s struct {
	claims enroll.AccountClaims
	err    error
	asked  int
}

func (s *stubK8s) VerifyAccount(context.Context, string) (enroll.AccountClaims, error) {
	s.asked++
	return s.claims, s.err
}

type stubGoogle struct {
	token api.GoogleToken
	err   error
	asked int
}

func (s *stubGoogle) VerifyGoogle(context.Context, string) (api.GoogleToken, error) {
	s.asked++
	return s.token, s.err
}

// issuedBy is an unsigned token naming issuer, which only the choice of verifier reads.
func issuedBy(issuer string) string {
	raw, _ := json.Marshal(map[string]string{"iss": issuer})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".unsigned"
}

// A service's platform token goes to the verifier of the issuer it names, unverified, and is
// answered with that verifier's account and times: a Kubernetes token's subject, from any issuer
// of BROKER_K8S_OIDC_ISSUER, with the name of the issuer the verifier says signed it, or a Google
// token's verified email. A Google token whose email is not verified, a token of an issuer this
// broker verifies no NATS tokens for, and a token whose verifier refuses it are each refused.
func TestPlatformTokensVerifyWithTheIssuersVerifier(t *testing.T) {
	now := time.Now()
	first, second := oidc.NamedIssuer{Name: "first", URL: "https://cluster.example"}, oidc.NamedIssuer{Name: "second", URL: "https://second.example"}
	k8s := &stubK8s{claims: enroll.AccountClaims{Subject: daemonAccount, IssuedAt: now, Expiry: now.Add(time.Hour), Issuer: first}}
	google := &stubGoogle{token: api.GoogleToken{Identity: exchange.GoogleIdentity{Account: measureAccount, EmailVerified: true}, IssuedAt: now, Expiry: now.Add(time.Hour)}}
	tokens := PlatformTokens{K8sIssuers: []oidc.NamedIssuer{first, second}, K8s: k8s, GoogleIssuer: "https://accounts.google.com", Google: google}

	identity, err := tokens.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.NoError(t, err)
	require.Equal(t, daemonAccount, identity.Account)
	require.False(t, identity.Google)
	require.Equal(t, "first", identity.Issuer)
	require.Equal(t, now, identity.Issued)
	require.Equal(t, now.Add(time.Hour), identity.Expiry)
	require.Equal(t, 1, k8s.asked)
	require.Equal(t, 0, google.asked)

	k8s.claims.Issuer = second
	identity, err = tokens.Verify(t.Context(), issuedBy("https://second.example"))
	require.NoError(t, err)
	require.Equal(t, daemonAccount, identity.Account)
	require.Equal(t, "second", identity.Issuer)
	require.Equal(t, 2, k8s.asked)

	identity, err = tokens.Verify(t.Context(), issuedBy("https://accounts.google.com"))
	require.NoError(t, err)
	require.Equal(t, measureAccount, identity.Account)
	require.True(t, identity.Google)
	require.Empty(t, identity.Issuer)

	google.token.Identity.EmailVerified = false
	_, err = tokens.Verify(t.Context(), issuedBy("https://accounts.google.com"))
	require.Error(t, err)

	_, err = tokens.Verify(t.Context(), issuedBy("https://elsewhere.example"))
	require.Error(t, err)
	_, err = tokens.Verify(t.Context(), "not-a-jwt")
	require.Error(t, err)

	k8s.err = errors.New("signature")
	_, err = tokens.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.Error(t, err)

	none := PlatformTokens{K8sIssuers: []oidc.NamedIssuer{first, second}, GoogleIssuer: "https://accounts.google.com"}
	_, err = none.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.Error(t, err, "a broker with no NATS Kubernetes audience verifies no Kubernetes token")
}

// A listed service whose kind of token has no audience could never connect, so the broker's start
// refuses it, naming the entry and the audience it needs.
func TestRequireAudiences(t *testing.T) {
	services, err := ParseServices("daemon/-/*="+daemonAccount+" measure/-/*=google:"+measureAccount, oneIssuer)
	require.NoError(t, err)
	require.NoError(t, RequireAudiences(services, "envoy-nats", "envoy-nats-google"))
	err = RequireAudiences(services, "", "envoy-nats-google")
	require.ErrorContains(t, err, "BROKER_NATS_K8S_AUDIENCE")
	require.ErrorContains(t, err, daemonAccount)
	err = RequireAudiences(services, "envoy-nats", "")
	require.ErrorContains(t, err, "BROKER_NATS_GOOGLE_AUDIENCE")
	require.ErrorContains(t, err, measureAccount)
	require.NoError(t, RequireAudiences(nil, "", ""))
}
