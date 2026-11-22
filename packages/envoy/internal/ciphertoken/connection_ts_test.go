package ciphertoken_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// The TypeScript signer's committed proof (packages/cipher-client's DpopKey.connectionProof,
// written by its scripts/connection-proof-fixture.ts) verifies with this checker at the instant
// it was signed, and with no other nonce or token: the two clients sign one claim set, and each
// side's checker verifies the other's vector, as packages/cipher-client/src/__tests__/dpop.test.ts
// verifies testdata/connection-proof-go.json.
func TestATypeScriptConnectionProofVerifies(t *testing.T) {
	raw, err := os.ReadFile("testdata/connection-proof-ts.json")
	require.NoError(t, err)
	var vector connectionVector
	require.NoError(t, json.Unmarshal(raw, &vector))
	require.Len(t, vector.ProofSegments, 3)
	proof := strings.Join(vector.ProofSegments, ".")
	now := time.Unix(vector.Now, 0)
	require.NoError(t, ciphertoken.VerifyConnectionProof(proof, vector.Token, vector.Nonce, vector.Thumbprint, now))
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(proof, vector.Token, "another-nonce", vector.Thumbprint, now), ciphertoken.ErrProofInvalid)
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(proof, "another-token", vector.Nonce, vector.Thumbprint, now), ciphertoken.ErrProofInvalid)
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(proof, vector.Token, vector.Nonce, "another-thumbprint", now), ciphertoken.ErrKeyMismatch)
}
