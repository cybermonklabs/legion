package ciphertoken_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

const (
	connectionToken = "access-token-of-ses_a1b2c3"
	connectionNonce = "server-nonce-of-this-connection"
)

// connectionVector is a proof committed with the token, the nonce and the thumbprint it verifies
// with, and the instant it was signed: testdata/connection-proof-go.json is the Go signer's
// (cipherclient.ConnectionProof), which the TypeScript client's checker reads too. The proof is
// kept as its three segments, joined by dots to read it: a proof of a throwaway key over an
// example token is no credential, and a joined one reads as a JWT to the push leak check.
type connectionVector struct {
	ProofSegments []string `json:"proof_segments"`
	Token         string   `json:"token"`
	Nonce         string   `json:"nonce"`
	Thumbprint    string   `json:"thumbprint"`
	Now           int64    `json:"now"`
}

// The Go signer's committed proof verifies at the instant it was signed, and with no other nonce:
// the vector a client of another language is held to is one this checker accepts.
func TestTheGoConnectionProofVectorVerifies(t *testing.T) {
	raw, err := os.ReadFile("testdata/connection-proof-go.json")
	require.NoError(t, err)
	var vector connectionVector
	require.NoError(t, json.Unmarshal(raw, &vector))
	require.Len(t, vector.ProofSegments, 3)
	proof := strings.Join(vector.ProofSegments, ".")
	now := time.Unix(vector.Now, 0)
	require.NoError(t, ciphertoken.VerifyConnectionProof(proof, vector.Token, vector.Nonce, vector.Thumbprint, now))
	require.ErrorIs(t, ciphertoken.VerifyConnectionProof(proof, vector.Token, "another-nonce", vector.Thumbprint, now), ciphertoken.ErrProofInvalid)
}

// connectionSigner signs connection proofs with a key of its own, and lets a test change any part
// of one: the claims, the 'typ' header, the algorithm and the key.
type connectionSigner struct {
	key crypto.Signer
	alg jose.SignatureAlgorithm
	typ string
}

func newConnectionSigner(t *testing.T) *connectionSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &connectionSigner{key: key, alg: jose.ES256, typ: "dpop+jwt"}
}

// jkt is the RFC 7638 thumbprint of the signer's public key, which the broker records as the key
// a token it mints is bound to.
func (s *connectionSigner) jkt(t *testing.T) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: s.key.Public()}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(sum)
}

// claims are the claims of a sound proof for token over nonce, issued at iat.
func connectionClaims(token, nonce string, iat time.Time) map[string]any {
	sum := sha256.Sum256([]byte(token))
	return map[string]any{
		"jti":   "proof-" + iat.Format(time.RFC3339Nano),
		"htm":   ciphertoken.ConnectionProofMethod,
		"htu":   ciphertoken.ConnectionProofTarget,
		"iat":   iat.Unix(),
		"ath":   base64.RawURLEncoding.EncodeToString(sum[:]),
		"nonce": nonce,
	}
}

func (s *connectionSigner) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: s.alg, Key: s.key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType(jose.ContentType(s.typ)))
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	signed, err := signer.Sign(payload)
	require.NoError(t, err)
	compact, err := signed.CompactSerialize()
	require.NoError(t, err)
	return compact
}

// A proof of the key the token is bound to, over this connection's nonce and naming the token, the
// connection's method and target and a fresh 'iat', verifies.
func TestAConnectionProofVerifies(t *testing.T) {
	signer := newConnectionSigner(t)
	// A proof's 'iat' is whole seconds, so the window's edges are read from the instant it names.
	now := time.Unix(time.Now().Unix(), 0)
	proof := signer.sign(t, connectionClaims(connectionToken, connectionNonce, now))
	require.NoError(t, ciphertoken.VerifyConnectionProof(proof, connectionToken, connectionNonce, signer.jkt(t), now))
	// The window is the HTTP check's: a proof from ProofClockSkew ahead of the server's clock,
	// and one ProofLifespan plus ProofClockSkew old, both still verify; a second past either edge
	// does not.
	for offset, verifies := range map[time.Duration]bool{
		-ciphertoken.ProofClockSkew:                                          true,
		-ciphertoken.ProofClockSkew - time.Second:                            false,
		ciphertoken.ProofLifespan + ciphertoken.ProofClockSkew:               true,
		ciphertoken.ProofLifespan + ciphertoken.ProofClockSkew + time.Second: false,
	} {
		err := ciphertoken.VerifyConnectionProof(proof, connectionToken, connectionNonce, signer.jkt(t), now.Add(offset))
		require.Equal(t, verifies, err == nil, "at iat%+v: %v", offset, err)
	}
}

// Every claim the connection binds is checked, and a refusal names the claim: the nonce (so a proof
// captured from one connection is refused on another), the token's hash, the method, the target,
// the issue time, the 'typ', the algorithm and the 'jti'. A sound proof by a key the token is not
// bound to is the key mismatch, as it is for an HTTP request.
func TestAConnectionProofIsRefused(t *testing.T) {
	now := time.Now()
	for _, row := range []struct {
		name    string
		prepare func(*connectionSigner, map[string]any)
		nonce   string
		claim   string
	}{
		{name: "another connection's nonce", nonce: "another-connection's-nonce", claim: "nonce"},
		{name: "no nonce", prepare: func(_ *connectionSigner, c map[string]any) { delete(c, "nonce") }, claim: "nonce"},
		{name: "another token's hash", prepare: func(_ *connectionSigner, c map[string]any) {
			c["ath"] = connectionClaims("another-token", connectionNonce, now)["ath"]
		}, claim: "ath"},
		{name: "no token hash", prepare: func(_ *connectionSigner, c map[string]any) { delete(c, "ath") }, claim: "ath"},
		{name: "an HTTP method", prepare: func(_ *connectionSigner, c map[string]any) { c["htm"] = "POST" }, claim: "htm"},
		{name: "an HTTP target", prepare: func(_ *connectionSigner, c map[string]any) { c["htu"] = "https://x/" }, claim: "htu"},
		{name: "two minutes old", prepare: func(_ *connectionSigner, c map[string]any) { c["iat"] = now.Add(-2 * time.Minute).Unix() }, claim: "iat"},
		{name: "two minutes ahead", prepare: func(_ *connectionSigner, c map[string]any) { c["iat"] = now.Add(2 * time.Minute).Unix() }, claim: "iat"},
		{name: "typ JWT", prepare: func(s *connectionSigner, _ map[string]any) { s.typ = "JWT" }, claim: "typ"},
		{name: "an RS256 key", prepare: func(s *connectionSigner, _ map[string]any) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			s.key, s.alg = key, jose.RS256
		}, claim: "alg"},
		{name: "no jti", prepare: func(_ *connectionSigner, c map[string]any) { delete(c, "jti") }, claim: "jti"},
	} {
		t.Run(row.name, func(t *testing.T) {
			signer := newConnectionSigner(t)
			bound := signer.jkt(t)
			claims := connectionClaims(connectionToken, connectionNonce, now)
			if row.prepare != nil {
				row.prepare(signer, claims)
			}
			nonce := connectionNonce
			if row.nonce != "" {
				nonce = row.nonce
			}
			err := ciphertoken.VerifyConnectionProof(signer.sign(t, claims), connectionToken, nonce, bound, now)
			require.ErrorIs(t, err, ciphertoken.ErrProofInvalid)
			var refused *ciphertoken.ProofClaimError
			require.True(t, errors.As(err, &refused), "%v names no claim", err)
			require.Equal(t, row.claim, refused.Claim)
			require.NotContains(t, err.Error(), connectionToken)
		})
	}
	t.Run("another key's thumbprint", func(t *testing.T) {
		signer, other := newConnectionSigner(t), newConnectionSigner(t)
		proof := signer.sign(t, connectionClaims(connectionToken, connectionNonce, now))
		err := ciphertoken.VerifyConnectionProof(proof, connectionToken, connectionNonce, other.jkt(t), now)
		require.ErrorIs(t, err, ciphertoken.ErrKeyMismatch)
	})
	t.Run("a token bound to no key", func(t *testing.T) {
		signer := newConnectionSigner(t)
		proof := signer.sign(t, connectionClaims(connectionToken, connectionNonce, now))
		err := ciphertoken.VerifyConnectionProof(proof, connectionToken, connectionNonce, "", now)
		require.ErrorIs(t, err, ciphertoken.ErrKeyMismatch)
	})
	t.Run("no proof", func(t *testing.T) {
		err := ciphertoken.VerifyConnectionProof("", connectionToken, connectionNonce, "jkt", now)
		require.ErrorIs(t, err, ciphertoken.ErrProofInvalid)
	})
}

// SessionCaller is the reading a verifier built without Options.ServesReadOnly makes of the
// broker's answer: for every answer, a live one and each one Verify refuses, it answers what such a
// verifier's Verify answers for a request presenting a token the broker answers that way with a
// sound proof. A verifier with the switch also admits a read-only token, which SessionCaller
// refuses (TestAReadOnlyTokenIsAcceptedOnlyByAVerifierThatServesOne), a token narrowed to reading
// among them: each answer of the narrowed answers' golden file is a row, refused by both
// (TestANarrowedAnswerIsACallerOnlyToAVerifierThatServesReadOnly).
func TestSessionCallerIsTheVerifyRule(t *testing.T) {
	caller := newSigner(t)
	inactive := liveToken(caller.jkt, time.Hour)
	inactive.Active = false
	unbound := liveToken(caller.jkt, time.Hour)
	unbound.Thumbprint = ""
	noSession := liveToken(caller.jkt, time.Hour)
	noSession.Session = ""
	nobody := liveToken(caller.jkt, time.Hour)
	nobody.Person = ""
	otherClient := liveToken(caller.jkt, time.Hour)
	otherClient.ClientID = "another-client"
	unknownScope := liveToken(caller.jkt, time.Hour)
	unknownScope.ProjectScope = "everything"
	// A named answer may name no project when it names a person (an account entry may
	// name none), and names at most one.
	namedNoProject := liveToken(caller.jkt, time.Hour)
	namedNoProject.ProjectScope, namedNoProject.Projects = ciphertoken.ProjectScopeNamed, nil
	namedTwoProjects := liveToken(caller.jkt, time.Hour)
	namedTwoProjects.ProjectScope, namedTwoProjects.Projects = ciphertoken.ProjectScopeNamed, []string{"ACME", "OTHER"}
	serviceWithProjects := serviceToken(caller.jkt, time.Hour)
	serviceWithProjects.Projects = []string{"ACME"}
	// Each row names whether Verify admits it, so a row that answers otherwise fails by name with
	// Verify's own error, rather than as a count that names none.
	type row struct {
		name     string
		answer   ciphertoken.Introspection
		admitted bool
	}
	rows := []row{
		{"live", liveToken(caller.jkt, time.Hour), true},
		{"service", serviceToken(caller.jkt, time.Hour), true},
		{"machine login", machineLoginToken(caller.jkt, time.Hour), false},
		{"inactive", inactive, false},
		{"bound to no key", unbound, false},
		{"no session", noSession, false},
		{"neither person nor service", nobody, false},
		{"another client", otherClient, false},
		{"unknown scope", unknownScope, false},
		{"named with no project, for a person", namedNoProject, true},
		{"named with two projects", namedTwoProjects, false},
		{"service with projects", serviceWithProjects, false},
	}
	narrowed := ciphertokentest.NarrowedAnswers(t, caller.jkt, "ses_narrowed", time.Now().Add(time.Hour).Unix())
	for _, kind := range slices.Sorted(maps.Keys(narrowed)) {
		rows = append(rows, row{"narrowed from a " + kind, narrowed[kind], false})
	}
	// Every row presents a token of its own, and the bucket admits each of them: what is under test
	// is the answer, not the address.
	rig := newRig(t, func(options *ciphertoken.Options) {
		options.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: len(rows)}
	})
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			token := "token-" + row.name
			rig.broker.Says(token, row.answer)
			verified, verifyErr := rig.verifier.Verify(t.Context(), caller.at("/api/v1/issues", token))
			read, readErr := ciphertoken.SessionCaller(row.answer)
			require.Equal(t, row.admitted, verifyErr == nil, "Verify answered %v", verifyErr)
			require.Equal(t, row.admitted, readErr == nil, "SessionCaller answered %v", readErr)
			require.Equal(t, verified, read)
			for _, sentinel := range []error{ciphertoken.ErrNotActive, ciphertoken.ErrKeyMismatch, ciphertoken.ErrNotASession} {
				require.Equal(t, errors.Is(verifyErr, sentinel), errors.Is(readErr, sentinel), "%v and %v", verifyErr, readErr)
			}
		})
	}
}
