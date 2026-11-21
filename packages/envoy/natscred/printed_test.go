package natscred_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// leakRun is the shortest run of a credential's characters no printed text may hold: the run the
// TypeScript clients' scrub withholds (@legion/contracts/held-secrets, SCRUB_RUN).
const leakRun = 10

// credential is a made-up credential, by the words a failure names it with.
type credential struct{ name, value string }

// requirePrintsNone fails t, naming the run, when text holds leakRun or more characters in a row of
// any of credentials.
func requirePrintsNone(t *testing.T, what, text string, credentials []credential) {
	t.Helper()
	for _, c := range credentials {
		for i := 0; i+leakRun <= len(c.value); i++ {
			if run := c.value[i : i+leakRun]; strings.Contains(text, run) {
				t.Errorf("%s holds %q, a run of %d characters of %s: %s", what, run, leakRun, c.name, text)
				break
			}
		}
	}
}

// madeUpClaims are the fixed claims of the made-up platform token: daemonAccount's, issued an hour
// before it lapses at the start of 2099.
func madeUpClaims() map[string]any {
	return map[string]any{"iss": "https://issuer.example", "aud": "envoy-nats", "sub": daemonAccount, "iat": 4070905200, "exp": 4070908800}
}

// madeUpToken is a JWT of claims with a made-up signature: natscred reads a platform token
// unverified, and nothing here verifies it.
func madeUpToken(claims map[string]any) string {
	signature := sha256.Sum256([]byte("a made-up signature"))
	return segment(map[string]string{"alg": "RS256", "kid": "made-up-key", "typ": "JWT"}) + "." + segment(claims) + "." +
		base64.RawURLEncoding.EncodeToString(signature[:])
}

// segment is v as a JWT segment: its JSON, base64url-encoded.
func segment(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// without is claims less the claims named.
func without(claims map[string]any, names ...string) map[string]any {
	kept := maps.Clone(claims)
	for _, name := range names {
		delete(kept, name)
	}
	return kept
}

// madeUpKey is a P-256 DPoP key whose private scalar is a hash of fixed words, and that scalar in
// each form a key is written in: a JWK's "d", a PKCS#8 PEM's body, and the hex and decimal of a
// debug print (an ecdsa.PrivateKey's %v prints its D in decimal).
func madeUpKey(t *testing.T) (*ecdsa.PrivateKey, []credential) {
	t.Helper()
	scalar := sha256.Sum256([]byte("a made-up DPoP key"))
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar[:])
	require.NoError(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return key, []credential{
		{"the DPoP key's d", base64.RawURLEncoding.EncodeToString(scalar[:])},
		{"the DPoP key's scalar in base64", base64.StdEncoding.EncodeToString(scalar[:])},
		{"the DPoP key's scalar in hex", hex.EncodeToString(scalar[:])},
		{"the DPoP key's scalar in decimal", new(big.Int).SetBytes(scalar[:]).String()},
		{"the DPoP key's PKCS#8 PEM", base64.StdEncoding.EncodeToString(pkcs8)},
	}
}

// answeringBroker is a secrets broker whose token endpoint answers every trade with status and
// body, and its base URL.
func answeringBroker(t *testing.T, status int, body map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cipherclient.TokenPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// TestNatscredPrintsNoCredential: natscred prints no part of a credential. A made-up platform token
// (a JWT of fixed made-up claims) and a made-up DPoP key go where a person or a server could put
// them: in a service token file that cannot be read or is empty, in a token natscred reads
// unverified that is no JWT or names no expiry or no account, in the audience a Google identity
// token is minted for, beside the broker's refusal of a session's trade, as the broker's access
// token for a server that sends no nonce to sign, and as the renewed token of a reconnect the
// closed connection refuses, beside the token it presented. No error natscred answers holds a run
// of leakRun or more characters of any of them, and each names the file or the failure it is
// about. natscred writes no log line, so its errors are all it prints. natscred is handed values,
// not settings: its caller names the setting a token came from (internal/bus, credential.go), and
// a Google identity token's read names the setting its caller hands it, never agent-secrets' own
// AGENT_SECRETS_MACHINE_AUDIENCE, with a plain URL audience whole beside it (the control: the
// check sees a value where one prints). The row that reads an unreadable file skips under root,
// which reads a 0000 file.
func TestNatscredPrintsNoCredential(t *testing.T) {
	token := madeUpToken(madeUpClaims())
	parts := strings.Split(token, ".")
	cut := parts[0] + "." + parts[1]
	notBase64 := parts[0] + ".*" + parts[1] + "." + parts[2]
	notJSON := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("not JSON: "+parts[1])) + "." + parts[2]
	noExpiry, noAccount := madeUpToken(without(madeUpClaims(), "exp")), madeUpToken(without(madeUpClaims(), "sub"))
	presentedToken := madeUpToken(without(madeUpClaims(), "aud"))
	key, keyForms := madeUpKey(t)
	credentials := append([]credential{
		{"the made-up token", token}, {"the made-up token cut short of its signature", cut},
		{"the made-up token with a payload that is no base64url", notBase64}, {"the made-up token with a payload that is no JSON", notJSON},
		{"the made-up token that names no expiry", noExpiry}, {"the made-up token that names no account", noAccount},
		{"the made-up token the connection presented", presentedToken},
	}, keyForms...)

	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		return path
	}
	tokenFile := write("pod.token", token)
	_, plainServer := testnats.Start(t)
	session := func(t *testing.T, status int, answer map[string]any) error {
		client, err := cipherclient.New(cipherclient.Config{BrokerURL: cipheraddr.New(answeringBroker(t, status, answer)), Key: key,
			Subject: cipherclient.NewProjectedToken(tokenFile), Session: "ses_a"})
		require.NoError(t, err)
		option, _ := natscred.Session(client)
		_, err = connect(t, plainServer, option)
		return err
	}
	// cutting is a metadata server that closes every call it is sent, so a read fails as one that
	// reached no answer, whose url.Error quotes the URL and the audience in its query.
	cutting := func(t *testing.T) {
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(metadata.Close)
		t.Setenv(googleidentity.MetadataHostVar, metadata.Listener.Addr().String())
	}

	for _, row := range []struct {
		name    string
		printed func(t *testing.T) error
		names   []string
	}{
		{"an unreadable service token file (service.go, Read)", func(t *testing.T) error {
			if os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file")
			}
			path := write("unreadable.token", token)
			require.NoError(t, os.Chmod(path, 0))
			_, err := natscred.ServiceFile(path).Read()
			return err
		}, []string{"the service token", filepath.Join(dir, "unreadable.token"), "permission denied"}},
		{"an empty service token file (service.go, Read)", func(t *testing.T) error {
			_, err := natscred.ServiceFile(write("empty.token", " \n")).Read()
			return err
		}, []string{"the service token", filepath.Join(dir, "empty.token"), "is empty"}},
		{"a token cut short of its signature, read for its account (expiry.go, claimsOf)", func(t *testing.T) error {
			_, err := natscred.Identity(cut)
			return err
		}, []string{"the token is not a JWT"}},
		{"a token cut short of its signature, read for its expiry (expiry.go, claimsOf)", func(t *testing.T) error {
			_, err := natscred.ExpiryOf(cut)
			return err
		}, []string{"the token is not a JWT"}},
		{"a token whose payload is no base64url (expiry.go, claimsOf)", func(t *testing.T) error {
			_, err := natscred.Identity(notBase64)
			return err
		}, []string{"the token is not a JWT"}},
		{"a token whose payload is no JSON (expiry.go, claimsOf)", func(t *testing.T) error {
			_, err := natscred.Identity(notJSON)
			return err
		}, []string{"the token is not a JWT"}},
		{"a token that names no expiry (expiry.go, expiryOf)", func(t *testing.T) error {
			_, err := natscred.ExpiryOf(noExpiry)
			return err
		}, []string{"the token names no expiry"}},
		{"a token that names no account (expiry.go, Identity)", func(t *testing.T) error {
			_, err := natscred.Identity(noAccount)
			return err
		}, []string{"the token names no account"}},
		{"a token pasted where the Google audience goes, with a metadata server that cuts the call (service.go, ServiceGoogle)", func(t *testing.T) error {
			cutting(t)
			_, err := natscred.ServiceGoogle(token, "NATS_SERVICE_GOOGLE_AUDIENCE").Read()
			return err
		}, []string{"Google identity token for the audience NATS_SERVICE_GOOGLE_AUDIENCE names", googleidentity.MetadataHostVar, "could not be reached"}},
		{"a plain URL audience, named whole beside the flag it came from (service.go, ServiceGoogle)", func(t *testing.T) error {
			cutting(t)
			_, err := natscred.ServiceGoogle("https://envoy-nats.example", "--service-google-audience").Read()
			return err
		}, []string{"Google identity token for https://envoy-nats.example (--service-google-audience)", "could not be reached"}},
		{"the broker's refusal of a session's trade (session.go, sign)", func(t *testing.T) error {
			return session(t, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "this broker admits no such subject token"})
		}, []string{"no broker access token for this session", "invalid_grant"}},
		{"the broker's access token, for a server that sends no nonce to sign (session.go, sign)", func(t *testing.T) error {
			return session(t, http.StatusOK, map[string]any{
				"access_token": token, "token_type": "DPoP", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900,
			})
		}, []string{"error signing nonce", "the NATS server sent no nonce"}},
		{"a renewed token the connection refuses to reconnect with (session.go, Keep)", func(t *testing.T) error {
			nc, err := connect(t, plainServer)
			require.NoError(t, err)
			nc.Close()
			var refused error
			run := func(ctx context.Context, onToken func(context.Context, *cipherclient.Token) error) error {
				refused = onToken(ctx, &cipherclient.Token{AccessToken: token, Expiry: time.Now().Add(15 * time.Minute)})
				return refused
			}
			renewal := func(renewed *cipherclient.Token) time.Time {
				return renewed.Expiry.Add(-cipherclient.DefaultRenewBefore)
			}
			presented := &cipherclient.Token{AccessToken: presentedToken, Expiry: time.Now().Add(time.Minute)}
			natscred.SessionKeeper(run, renewal, presented).Keep(t.Context(), nc)
			return refused
		}, []string{"reconnect with the renewed token", "connection closed"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := row.printed(t)
			require.Error(t, err)
			printed := err.Error()
			requirePrintsNone(t, "the error", printed, credentials)
			for _, name := range row.names {
				require.Contains(t, printed, name, "the error does not name what it is about")
			}
			require.NotContains(t, printed, cipherclient.MachineAudienceVar, "the error names agent-secrets' own setting, which no caller of natscred reads")
		})
	}
}
