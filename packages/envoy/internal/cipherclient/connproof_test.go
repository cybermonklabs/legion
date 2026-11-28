package cipherclient_test

import (
	"crypto"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// The proof the client signs for a NATS connection is the one the auth service checks: it verifies
// with the server's nonce, the token it was signed beside and the key's thumbprint, and with no
// other nonce or token.
func TestConnectionProofIsTheOneTheServiceChecks(t *testing.T) {
	key, err := proof.NewKey()
	require.NoError(t, err)
	sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	jkt := base64.RawURLEncoding.EncodeToString(sum)

	signed, err := cipherclient.ConnectionProof(key, "the-access-token", "the-server-nonce")
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, ciphertoken.VerifyConnectionProof(signed, "the-access-token", "the-server-nonce", jkt, now))
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(signed, "the-access-token", "another-nonce", jkt, now), ciphertoken.ErrProofInvalid)
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(signed, "another-token", "the-server-nonce", jkt, now), ciphertoken.ErrProofInvalid)

	again, err := cipherclient.ConnectionProof(key, "the-access-token", "the-server-nonce")
	require.NoError(t, err)
	jti := func(signed string) string {
		t.Helper()
		parsed, err := jose.ParseSigned(signed, []jose.SignatureAlgorithm{jose.ES256})
		require.NoError(t, err)
		var claims struct {
			JTI string `json:"jti"`
		}
		require.NoError(t, json.Unmarshal(parsed.UnsafePayloadWithoutVerification(), &claims))
		require.NotEmpty(t, claims.JTI, "a proof names no 'jti'")
		return claims.JTI
	}
	require.NotEqual(t, jti(signed), jti(again), "each proof names a 'jti' of its own")
}

// A server that sends no nonce leaves a session's proof nothing to sign, so a caller that chose a
// session's credential for it is told that, as ErrNoNonce, which a caller that knows what else it
// could present tests for.
func TestConnectionProofNamesAHubWithNoAuthService(t *testing.T) {
	key, err := proof.NewKey()
	require.NoError(t, err)
	_, err = cipherclient.ConnectionProof(key, "the-access-token", "")
	require.ErrorIs(t, err, cipherclient.ErrNoNonce)
	require.EqualError(t, err, "brokerclient: the NATS server sent no nonce to sign, so it runs no auth service and takes no session's broker token")
}
