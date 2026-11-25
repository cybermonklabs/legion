// packages/envoy/cmd/cipher/proofkinds_test.go
//
// The four kinds of proof one role key signs, put to the five checks that judge a proof of key,
// with the real broker (ciphertest.NewRig: real handlers, real Postgres) behind the two that ask
// it. It skips without BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// The names this test gives the proof kinds and the checks, which its failures print.
const (
	requestObjectKind = "the request object (fileSigner.SignRequestObject)"
	perRequestKind    = "the per-request proof (fileSigner.Sign)"
	dpopKind          = "the DPoP proof (cipherclient.NewDPoPTransport over MachineKey's key)"
	connectionKind    = "the NATS connection proof (cipherclient.ConnectionProof)"
)

// roleEnrollment is the enrollment the role's per-request proofs name, and the one the
// per-request check's lookup answers the role's key for.
const roleEnrollment = "enrollment-of-the-role"

// TestARoleKeysProofsAreNotInterchangeable: one role key signs four kinds of proof — the
// agent-secrets request object, the per-request proof, the DPoP proof and the NATS connection
// proof — and each of the five checks that judge a proof admits only its own kind: the broker's
// request-object check (record.VerifyRequestObject), its per-request proof check (proof.Verifier),
// the token endpoint's DPoP check (the OAuth library's RFC 9449 parser, through the rig's
// oauth.Provider), a resource server's (ciphertoken.Verify) and the NATS auth service's
// (ciphertoken.VerifyConnectionProof). The DPoP proof's own checks are both of the DPoP ones.
//
// Every proof is signed by envoy's own signers over one role key directory, keygen's key.pem, and
// each HTTP-shaped proof is signed for the very request the check judges, so a refusal is the
// kind's and never an unrelated URL's: the per-request proof against the token endpoint is the
// closest pair, both carrying jti, htm, htu and iat for the same request with only typ between
// them. A connection is no HTTP request, so the HTTP-shaped proofs put to its check are the ones
// the role signs to present the same token to a resource server.
func TestARoleKeysProofsAreNotInterchangeable(t *testing.T) {
	rig := ciphertest.NewRig(t)
	ctx := context.Background()

	keyDir := t.TempDir()
	var stdout, stderr strings.Builder
	if code := cmdKeygen([]string{"--out", keyDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("keygen = exit %d, stderr %q", code, stderr.String())
	}
	thumbprint := strings.TrimSpace(stdout.String())
	keyPath := filepath.Join(keyDir, keyFile)
	fileKey, err := loadKey(keyPath, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	session := &fileSigner{key: fileKey, enrollmentID: roleEnrollment}
	pod := map[string]string{cipherclient.IdentityVar: cipherclient.IdentityKubernetes, cipherclient.KeyDirVar: keyDir}
	machineKey, err := cipherclient.MachineKey(func(name string) string { return pod[name] })
	if err != nil {
		t.Fatalf("MachineKey over the role's directory: %v", err)
	}

	// The role's access token, traded at the rig's token endpoint and bound to the role's key.
	subjectFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(subjectFile, []byte(rig.MintSubjectToken(t, ciphertest.ExchangeAccount, uuid.NewString())), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := cipherclient.New(cipherclient.Config{BrokerURL: cipheraddr.New(rig.URL), Key: machineKey, Subject: cipherclient.NewProjectedToken(subjectFile)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("the role's trade: %v", err)
	}
	const nonce = "nonce-the-server-sent-this-connection"
	const resourceOrigin = "http://dispatch.internal.example"
	resourceURL := resourceOrigin + "/api/v1/projects"

	perRequest := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return thumbprint, id == roleEnrollment, nil
		},
		LookupLauncher: func(context.Context, string) (string, bool, error) { return "", false, nil },
		Replay:         func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	resource, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: rig.URL, Account: listenerAccount(rig.ListenerToken), Origins: []string{resourceOrigin},
		Replay: newSpentProofs(), Address: func(*http.Request) string { return "192.0.2.10" },
	})
	if err != nil {
		t.Fatal(err)
	}

	checks := []proofCheck{
		{
			name: "the broker's request-object check (record.VerifyRequestObject)", owns: []string{requestObjectKind},
			method: http.MethodPost, target: rig.URL + "/v1/requests",
			admits: func(_ *testing.T, signed string) error {
				_, err := record.VerifyRequestObject(signed, rig.URL, time.Minute, time.Now())
				return err
			},
		},
		{
			name: "the broker's per-request proof check (proof.Verifier)", owns: []string{perRequestKind},
			method: http.MethodPost, target: rig.URL + "/v1/requests",
			admits: func(_ *testing.T, signed string) error {
				_, err := perRequest.Verify(ctx, signed, http.MethodPost, rig.URL+"/v1/requests", time.Now())
				return err
			},
		},
		{
			name: "the token endpoint's DPoP check (oauth.Provider)", owns: []string{dpopKind},
			method: http.MethodPost, target: rig.URL + cipherclient.TokenPath,
			admits: func(t *testing.T, signed string) error { return tradeWithProof(t, rig, signed) },
		},
		{
			name: "a resource server's check (ciphertoken.Verify)", owns: []string{dpopKind},
			method: http.MethodGet, target: resourceURL, presents: token.AccessToken,
			admits: func(_ *testing.T, signed string) error {
				r := httptest.NewRequest(http.MethodGet, resourceURL, nil)
				r.Header.Set("Authorization", "DPoP "+token.AccessToken)
				r.Header.Set("DPoP", signed)
				_, err := resource.Verify(ctx, r)
				return err
			},
		},
		{
			name: "the NATS auth service's connection check (ciphertoken.VerifyConnectionProof)", owns: []string{connectionKind},
			method: http.MethodGet, target: resourceURL, presents: token.AccessToken,
			admits: func(_ *testing.T, signed string) error {
				return ciphertoken.VerifyConnectionProof(signed, token.AccessToken, nonce, thumbprint, time.Now())
			},
		},
	}
	kinds := []struct {
		name string
		sign func(t *testing.T, check proofCheck) (string, error)
	}{
		{requestObjectKind, func(*testing.T, proofCheck) (string, error) {
			return session.SignRequestObject(cipheraddr.New(rig.URL), []string{"TEST_SECRET"}, "a role asks for its secret", nil, nil)
		}},
		{perRequestKind, func(_ *testing.T, check proofCheck) (string, error) {
			return session.Sign(check.method, check.target)
		}},
		{dpopKind, func(t *testing.T, check proofCheck) (string, error) {
			return dpopProof(t, machineKey, check.method, check.target, check.presents), nil
		}},
		{connectionKind, func(*testing.T, proofCheck) (string, error) {
			return cipherclient.ConnectionProof(machineKey, token.AccessToken, nonce)
		}},
	}

	for _, check := range checks {
		for _, kind := range kinds {
			signed, err := kind.sign(t, check)
			if err != nil {
				t.Fatalf("sign %s for %s: %v", kind.name, check.name, err)
			}
			err = check.admits(t, signed)
			switch own := slices.Contains(check.owns, kind.name); {
			case own && err != nil:
				t.Errorf("%s refused %s, its own kind: %v", check.name, kind.name, err)
			case !own && err == nil:
				var owners []string
				for _, other := range checks {
					if slices.Contains(other.owns, kind.name) {
						owners = append(owners, other.name)
					}
				}
				t.Errorf("%s admitted %s, which only %s may admit", check.name, kind.name, strings.Join(owners, " and "))
			}
		}
	}
}

// proofCheck is one check a proof is put to: what it admits, the kinds of proof that are its
// own, and the request an HTTP-shaped proof is signed for when it is put to this check — the
// method, the target and the access token presented beside it, "" for none.
type proofCheck struct {
	name                     string
	owns                     []string
	method, target, presents string
	admits                   func(t *testing.T, signed string) error
}

// tradeWithProof is one trade at the rig's token endpoint carrying signed as its DPoP proof: a
// fresh subject token of the rig's fixed-session account, so nothing but the proof can refuse it.
// It answers nil for a token and the broker's answer for anything else.
func tradeWithProof(t *testing.T, rig *ciphertest.Rig, signed string) error {
	t.Helper()
	form := url.Values{
		"grant_type":         {cipherclient.GrantTypeTokenExchange},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {rig.MintSubjectToken(t, ciphertest.ExchangeAccount, uuid.NewString())},
		"subject_token_type": {oauthwire.SubjectTokenTypeKubernetes},
	}
	request, err := http.NewRequest(http.MethodPost, rig.URL+cipherclient.TokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("DPoP", signed)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", cipherclient.TokenPath, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the token endpoint answered %d %s", response.StatusCode, body)
	}
	return nil
}

// dpopProof is the proof cipherclient's DPoP signer puts on an HTTP request of method to target,
// presenting accessToken under the DPoP scheme where it is not "", read off the request the
// transport sent.
func dpopProof(t *testing.T, key *ecdsa.PrivateKey, method, target, accessToken string) string {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "" {
		request.Header.Set("Authorization", "DPoP "+accessToken)
	}
	var sent proofCapture
	response, err := cipherclient.NewDPoPTransport(key, &sent).RoundTrip(request)
	if err != nil {
		t.Fatalf("sign a DPoP proof for %s %s: %v", method, target, err)
	}
	response.Body.Close()
	return sent.request.Header.Get("DPoP")
}

// proofCapture keeps the request a transport sent through it and answers 204.
type proofCapture struct{ request *http.Request }

func (c *proofCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.request = r
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
}

// listenerAccount is the identity a resource server introspects with: the rig's listener token.
type listenerAccount string

func (a listenerAccount) Read() (string, error) { return string(a), nil }

// spentProofs is a resource server's record of spent proofs in memory, keyed on jti and htu as
// rfc9449.DPoPReplayStorage requires.
type spentProofs struct {
	mu    sync.Mutex
	spent map[string]time.Time
}

func newSpentProofs() *spentProofs { return &spentProofs{spent: map[string]time.Time{}} }

func (s *spentProofs) CheckAndSetDPoPProofUsed(_ context.Context, jti, _, _, _, htu string, exp time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jti + "\x00" + htu
	if until, ok := s.spent[key]; ok && until.After(time.Now()) {
		return true, nil
	}
	s.spent[key] = exp
	return false, nil
}
