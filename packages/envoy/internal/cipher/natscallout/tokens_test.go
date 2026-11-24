package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// stubVerifier is a caller verifier of one kind and one issuer that answers what it is told and
// counts how often it was asked.
type stubVerifier struct {
	kind   identity.Kind
	issuer string
	caller identity.Caller
	err    error
	asked  int
}

func (s *stubVerifier) Kind() identity.Kind { return s.kind }
func (s *stubVerifier) Issuer() string      { return s.issuer }
func (s *stubVerifier) VerifyCaller(context.Context, string) (identity.Caller, error) {
	s.asked++
	return s.caller, s.err
}

// issuedBy is an unsigned token naming issuer, which only the choice of verifier reads.
func issuedBy(issuer string) string {
	raw, _ := json.Marshal(map[string]string{"iss": issuer})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".unsigned"
}

// A service's platform token goes to the verifier of the issuer it names, unverified, and is
// answered with that verifier's account and times: a Kubernetes token's subject, or a Google
// token's verified email. A Google token whose email is not verified, a token of an issuer this
// broker verifies no NATS tokens for, and a token whose verifier refuses it are each refused.
func TestPlatformTokensVerifyWithTheIssuersVerifier(t *testing.T) {
	now := time.Now()
	k8s := &stubVerifier{kind: kubernetes.Kind, issuer: "https://cluster.example", caller: identity.Caller{
		Account: identity.Account{Kind: kubernetes.Kind, Address: daemonAccount}, IssuedAt: now, Expiry: now.Add(time.Hour),
	}}
	googleTokens := &stubVerifier{kind: google.Kind, issuer: "https://accounts.google.com", caller: identity.Caller{
		Account: identity.Account{Kind: google.Kind, Address: measureAccount}, IssuedAt: now, Expiry: now.Add(time.Hour),
	}}
	tokens := PlatformTokens{Verifiers: []identity.CallerVerifier{k8s, googleTokens}}

	caller, err := tokens.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.NoError(t, err)
	require.Equal(t, identity.Account{Kind: kubernetes.Kind, Address: daemonAccount}, caller.Account)
	require.Equal(t, now, caller.IssuedAt)
	require.Equal(t, now.Add(time.Hour), caller.Expiry)
	require.Equal(t, 1, k8s.asked)
	require.Equal(t, 0, googleTokens.asked)

	caller, err = tokens.Verify(t.Context(), issuedBy("https://accounts.google.com"))
	require.NoError(t, err)
	require.Equal(t, identity.Account{Kind: google.Kind, Address: measureAccount}, caller.Account)

	googleTokens.err = &identity.NoAccountError{Reason: "names no verified account", Brief: "carries no verified email"}
	_, err = tokens.Verify(t.Context(), issuedBy("https://accounts.google.com"))
	require.EqualError(t, err, "natscallout: carries no verified email")

	_, err = tokens.Verify(t.Context(), issuedBy("https://elsewhere.example"))
	require.Error(t, err)
	_, err = tokens.Verify(t.Context(), "not-a-jwt")
	require.Error(t, err)

	k8s.err = errors.New("signature")
	_, err = tokens.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.Error(t, err)

	none := PlatformTokens{}
	_, err = none.Verify(t.Context(), issuedBy("https://cluster.example"))
	require.Error(t, err, "a broker with no NATS Kubernetes audience verifies no Kubernetes token")
}

// TestNATSPicksTheVerifierByTheTokensExactIssuer: the token's own iss, read before it is verified,
// picks the one verifier whose issuer is exactly that string. A token naming neither verifier's
// issuer is refused naming the issuer, and neither verifier is asked, so no token is judged by a
// verifier of another kind; a Google token is verified by the Google verifier alone (Review focus
// 1: Google's tokens name accounts.google.com both with and without a scheme, and the auth service
// takes only the spelling its verifier holds).
func TestNATSPicksTheVerifierByTheTokensExactIssuer(t *testing.T) {
	k8s := &stubVerifier{kind: kubernetes.Kind, issuer: "https://cluster.example"}
	googleTokens := &stubVerifier{kind: google.Kind, issuer: "https://accounts.google.com", caller: identity.Caller{
		Account: identity.Account{Kind: google.Kind, Address: measureAccount}, Expiry: time.Now().Add(time.Hour),
	}}
	tokens := PlatformTokens{Verifiers: []identity.CallerVerifier{k8s, googleTokens}}

	for _, issuer := range []string{"accounts.google.com", "https://cluster.example/", "https://elsewhere.example"} {
		_, err := tokens.Verify(t.Context(), issuedBy(issuer))
		require.ErrorContains(t, err, `"`+issuer+`"`, "a token of %s must be refused naming its issuer", issuer)
	}
	require.Zero(t, k8s.asked, "the Kubernetes verifier was asked about a token of another issuer")
	require.Zero(t, googleTokens.asked, "the Google verifier was asked about a token of another issuer")

	caller, err := tokens.Verify(t.Context(), issuedBy("https://accounts.google.com"))
	require.NoError(t, err)
	require.Equal(t, measureAccount, caller.Account.Address)
	require.Equal(t, 1, googleTokens.asked)
	require.Zero(t, k8s.asked, "the Kubernetes verifier was asked about a Google token")
}

// TestAGoogleTokenNamingNoVerifiedEmailIsRefusedInTodaysWords pins the refusal the auth service
// logs for a sound Google identity token that names no verified account, byte for byte as it read
// before the identity registry, through the google provider's own verifier.
func TestAGoogleTokenNamingNoVerifiedEmailIsRefusedInTodaysWords(t *testing.T) {
	ctx := context.Background()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	keys, err := oidc.NewKeys(ctx, issuer.URL())
	require.NoError(t, err)
	provider, err := google.New(google.Config{Issuer: issuer.URL(), Keys: keys, Audience: "envoy-nats-google"})
	require.NoError(t, err)
	tokens := PlatformTokens{Verifiers: []identity.CallerVerifier{provider.CallerVerifier("envoy-nats-google")}}

	claims := issuer.Claims("1234567890", "envoy-nats-google")
	claims["email"], claims["email_verified"] = measureAccount, false
	_, err = tokens.Verify(ctx, issuer.Mint(t, key, claims))
	require.EqualError(t, err, "natscallout: the Google identity token carries no verified email")

	claims["email_verified"] = true
	caller, err := tokens.Verify(ctx, issuer.Mint(t, key, claims))
	require.NoError(t, err)
	require.Equal(t, identity.Account{Kind: google.Kind, Address: measureAccount}, caller.Account)
}
