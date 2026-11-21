package secrets_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
)

// The federation fixtures: one bound Google account and the Anthropic identifiers of one rule,
// none of them a real organization's.
const (
	federationAccount = "anthropic-admin@example-project.iam.gserviceaccount.com"
	federationOrg     = "00000000-0000-4000-8000-000000000000"
	federationRule    = "fdrl_01examplerule"
	federationTarget  = "svac_01exampletarget"
	federationSpace   = "wrkspc_01examplespace"
	federationToken   = "sk-ant-oat01-" + "example-federated-token"
)

// federationValue is an anthropic-federation secret's stored value, naming workspace when it is
// not empty.
func federationValue(account, workspace string) string {
	fields := map[string]string{
		"organization_id": federationOrg, "federation_rule_id": federationRule,
		"service_account_id": federationTarget, "service_account": account,
	}
	if workspace != "" {
		fields["workspace_id"] = workspace
	}
	value, _ := json.Marshal(fields)
	return string(value)
}

// anthropicStand is a stand-in of Anthropic's POST /v1/oauth/token: it answers each exchange with
// answer and keeps every request body it got.
type anthropicStand struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	types  []string
	answer map[string]any
}

func newAnthropicStand(t *testing.T) *anthropicStand {
	t.Helper()
	s := &anthropicStand{answer: map[string]any{"access_token": federationToken, "token_type": "Bearer", "expires_in": 3600, "scope": "org:admin"}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.types = append(s.types, r.Header.Get("Content-Type"))
		answer := s.answer
		s.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *anthropicStand) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// federationRig is a minter bound to federationAccount for FED_ADMIN, against stand-ins of the
// metadata server (the pod's own access token), IAM's generateIdToken and Anthropic's exchange,
// on a clock the test reads.
type federationRig struct {
	minter    *secrets.AnthropicFederation
	iam       *ciphertokentest.IAMStand
	anthropic *anthropicStand
	now       time.Time
}

func newFederationRig(t *testing.T) *federationRig {
	t.Helper()
	ciphertokentest.NewMetadataStand(t)
	r := &federationRig{iam: ciphertokentest.NewIAMStand(t), anthropic: newAnthropicStand(t), now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	r.minter = &secrets.AnthropicFederation{
		Accounts:          map[string]string{"FED_ADMIN": federationAccount},
		TokenURL:          r.anthropic.server.URL + "/v1/oauth/token",
		IAMCredentialsURL: r.iam.Server.URL,
		Now:               func() time.Time { return r.now },
	}
	return r
}

func (r *federationRig) mint(t *testing.T, value string) (secrets.Minted, error) {
	t.Helper()
	return r.minter.Mint(t.Context(), secrets.MintRequest{Name: "FED_ADMIN", Type: "anthropic-federation", HumanTier: true, Value: value})
}

// TestTheFederationGrantMintsTheBoundAccountsIdentityToken: the grant asks generateIdToken for the
// bound account alone, for the audience https://api.anthropic.com with includeEmail, as the pod's
// own credential, and hands that identity token to the exchange as its assertion.
func TestTheFederationGrantMintsTheBoundAccountsIdentityToken(t *testing.T) {
	r := newFederationRig(t)
	if _, err := r.mint(t, federationValue(federationAccount, "")); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	asked := r.iam.Requests()
	if len(asked) != 1 {
		t.Fatalf("IAM got %d requests, want one: %+v", len(asked), asked)
	}
	if want := "/v1/projects/-/serviceAccounts/" + federationAccount + ":generateIdToken"; asked[0].Path != want {
		t.Fatalf("IAM was asked %s, want %s", asked[0].Path, want)
	}
	if asked[0].Authorization != "Bearer "+ciphertokentest.MetadataAccessBearer {
		t.Fatalf("IAM was called with %q, want the pod's own access token", asked[0].Authorization)
	}
	if asked[0].Body["audience"] != "https://api.anthropic.com" || asked[0].Body["includeEmail"] != true {
		t.Fatalf("generateIdToken's body is %v; want audience https://api.anthropic.com and includeEmail", asked[0].Body)
	}
	if exchanged := r.anthropic.requests(); len(exchanged) != 1 || exchanged[0]["assertion"] != ciphertokentest.IAMIdentityToken {
		t.Fatalf("the exchange's assertion is %v; want the identity token IAM minted", exchanged)
	}
}

// TestTheFederationExchangeSendsTheRequiredFields: the exchange is the JSON jwt-bearer grant with
// the value's rule, organization and target service account, and its workspace only when the
// value names one.
func TestTheFederationExchangeSendsTheRequiredFields(t *testing.T) {
	r := newFederationRig(t)
	for _, workspace := range []string{"", federationSpace} {
		if _, err := r.mint(t, federationValue(federationAccount, workspace)); err != nil {
			t.Fatalf("Mint(workspace %q): %v", workspace, err)
		}
	}
	exchanged := r.anthropic.requests()
	if len(exchanged) != 2 {
		t.Fatalf("Anthropic got %d exchanges, want two", len(exchanged))
	}
	want := map[string]any{
		"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": ciphertokentest.IAMIdentityToken,
		"federation_rule_id": federationRule, "organization_id": federationOrg, "service_account_id": federationTarget,
	}
	for i, body := range exchanged {
		expect := map[string]any{}
		for k, v := range want {
			expect[k] = v
		}
		if i == 1 {
			expect["workspace_id"] = federationSpace
		}
		if len(body) != len(expect) {
			t.Fatalf("exchange %d sent %v; want exactly %v", i, body, expect)
		}
		for k, v := range expect {
			if body[k] != v {
				t.Fatalf("exchange %d sent %s=%v; want %v (body %v)", i, k, body[k], v, body)
			}
		}
		if got := r.anthropic.types[i]; !strings.HasPrefix(got, "application/json") {
			t.Fatalf("exchange %d's Content-Type is %q, want JSON", i, got)
		}
	}
}

// TestTheFederationGrantRefusesAnotherTokenShape: an answer whose access_token is no
// sk-ant-oat01- token, or that holds none, releases nothing, and the error quotes no token.
func TestTheFederationGrantRefusesAnotherTokenShape(t *testing.T) {
	// An Anthropic API key's shape, which the exchange never answers: no federated token.
	const apiKeyShape = "sk-ant-api03-" + "a-key-not-a-federated-token"
	r := newFederationRig(t)
	for _, answer := range []map[string]any{
		{"access_token": apiKeyShape, "expires_in": 3600},
		{"access_token": "", "expires_in": 3600},
		{"expires_in": 3600},
	} {
		r.anthropic.mu.Lock()
		r.anthropic.answer = answer
		r.anthropic.mu.Unlock()
		minted, err := r.mint(t, federationValue(federationAccount, ""))
		if !errors.Is(err, secrets.ErrMintUpstream) || minted.Value != "" {
			t.Fatalf("Mint answered %v = %+v, %v; want ErrMintUpstream and no value", answer, minted, err)
		}
		if token, _ := answer["access_token"].(string); token != "" && strings.Contains(err.Error(), token) {
			t.Fatalf("the refusal quotes the token it refused: %v", err)
		}
	}
}

// TestTheFederationGrantExpiresAtTheTokensEnd: the minted value expires when the token Anthropic
// answered does, the clock's now plus expires_in, which is what the airlock's proxy mints again
// five minutes before (proxy.TestAFederationTokenIsMintedAgainFiveMinutesBeforeItsEnd); an answer
// with no lifetime is refused, since nothing would tell the proxy when to mint again.
func TestTheFederationGrantExpiresAtTheTokensEnd(t *testing.T) {
	r := newFederationRig(t)
	r.anthropic.answer["expires_in"] = 1800
	minted, err := r.mint(t, federationValue(federationAccount, ""))
	if err != nil || minted.Value != federationToken {
		t.Fatalf("Mint = %+v, %v; want the federated token", minted, err)
	}
	if want := r.now.Add(30 * time.Minute); !minted.Expires.Equal(want) {
		t.Fatalf("Mint's expiry = %v, want %v: now plus expires_in", minted.Expires, want)
	}
	for _, lifetime := range []any{0, -5, nil} {
		r.anthropic.answer["expires_in"] = lifetime
		if _, err := r.mint(t, federationValue(federationAccount, "")); !errors.Is(err, secrets.ErrMintUpstream) {
			t.Fatalf("Mint of an answer with expires_in %v = %v, want ErrMintUpstream", lifetime, err)
		}
	}
}

// TestAFederationSecretBoundToNoAccountReleasesNothing: a secret the deployment binds to no
// account, and one whose stored value names another account than the bound one, are refused
// ErrMintNotBound before any call is made; so is a value the decoder refuses, ErrMintStoredValue.
func TestAFederationSecretBoundToNoAccountReleasesNothing(t *testing.T) {
	r := newFederationRig(t)
	if _, err := r.minter.Mint(t.Context(), secrets.MintRequest{Name: "UNBOUND", Value: federationValue(federationAccount, "")}); !errors.Is(err, secrets.ErrMintNotBound) {
		t.Fatalf("Mint of an unbound secret = %v, want ErrMintNotBound", err)
	}
	if _, err := r.mint(t, federationValue("someone-else@example-project.iam.gserviceaccount.com", "")); !errors.Is(err, secrets.ErrMintNotBound) {
		t.Fatalf("Mint for another account = %v, want ErrMintNotBound", err)
	}
	for _, value := range []string{
		`{"organization_id":"` + federationOrg + `","federation_rule_id":"` + federationRule + `","service_account_id":"` + federationTarget + `"}`,
		`{"organization_id":"not-a-uuid","federation_rule_id":"` + federationRule + `","service_account_id":"` + federationTarget + `","service_account":"` + federationAccount + `"}`,
		`{"organization_id":"` + federationOrg + `","federation_rule_id":"rule","service_account_id":"` + federationTarget + `","service_account":"` + federationAccount + `"}`,
		`{"organization_id":"` + federationOrg + `","federation_rule_id":"` + federationRule + `","service_account_id":"target","service_account":"` + federationAccount + `"}`,
		federationValue(federationAccount, "default"),
		"not json",
	} {
		if _, err := r.mint(t, value); !errors.Is(err, secrets.ErrMintStoredValue) {
			t.Fatalf("Mint of the value %s = %v, want ErrMintStoredValue", value, err)
		}
	}
	if asked := r.iam.Requests(); len(asked) != 0 {
		t.Fatalf("a refused mint asked IAM %d times, want none", len(asked))
	}
	if exchanged := r.anthropic.requests(); len(exchanged) != 0 {
		t.Fatalf("a refused mint exchanged %d times, want none", len(exchanged))
	}
}
