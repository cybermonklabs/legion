package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
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
// status (200 when zero) and answer, runs during each exchange when set, and keeps every request
// body it got.
type anthropicStand struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	types  []string
	answer map[string]any
	status int
	during func()
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
		answer, status, during := s.answer, s.status, s.during
		s.mu.Unlock()
		if during != nil {
			during()
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// set changes what the stand answers.
func (s *anthropicStand) set(change func(*anthropicStand)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

func (s *anthropicStand) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// federationRig is a minter bound to federationAccount for FED_ADMIN, against stand-ins of the
// metadata server (the pod's own access token), IAM's generateIdToken (read through the reader
// cmd/cipherd builds, ciphertoken.GoogleAccounts, with no cache) and Anthropic's exchange, on a
// clock the test moves, counting its attempts on registry.
type federationRig struct {
	minter    *secrets.AnthropicFederation
	iam       *ciphertokentest.IAMStand
	anthropic *anthropicStand
	registry  *metrics.Registry

	mu  sync.Mutex
	now time.Time
}

func newFederationRig(t *testing.T) *federationRig {
	t.Helper()
	ciphertokentest.NewMetadataStand(t)
	r := &federationRig{iam: ciphertokentest.NewIAMStand(t), anthropic: newAnthropicStand(t), registry: metrics.New(), now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	identity := &ciphertoken.GoogleAccounts{Audience: secrets.AnthropicAudience, IAMCredentialsURL: r.iam.Server.URL}
	r.minter = &secrets.AnthropicFederation{
		Accounts:      map[string]string{"FED_ADMIN": federationAccount},
		IdentityToken: identity.Read,
		TokenURL:      r.anthropic.server.URL + "/v1/oauth/token",
		Retry:         secrets.NewRetry(r.registry),
		Now:           r.clock,
	}
	return r
}

func (r *federationRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *federationRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

func (r *federationRig) mint(t *testing.T, value string) (secrets.Minted, error) {
	t.Helper()
	return r.minter.Mint(t.Context(), secrets.MintRequest{Name: "FED_ADMIN", Source: "FED_ADMIN", Type: "anthropic-federation", HumanTier: true, Value: value})
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
// answered does, read from the moment the exchange was asked, not answered, so a slow answer
// never lengthens it; that end is what the airlock's proxy mints again five minutes before. A
// lifetime over the grant's hour (a rule's token_lifetime_seconds goes up to 86400) would outlive
// the grant, so it is refused as the rule's fault, naming the setting; a short one is taken; an
// answer with no lifetime is refused, since nothing would tell the proxy when to mint again.
func TestTheFederationGrantExpiresAtTheTokensEnd(t *testing.T) {
	r := newFederationRig(t)
	asked := r.clock()
	r.anthropic.set(func(s *anthropicStand) {
		s.answer["expires_in"] = 1800
		s.during = func() { r.advance(7 * time.Second) }
	})
	minted, err := r.mint(t, federationValue(federationAccount, ""))
	if err != nil || minted.Value != federationToken {
		t.Fatalf("Mint = %+v, %v; want the federated token", minted, err)
	}
	if want := asked.Add(30 * time.Minute); !minted.Expires.Equal(want) {
		t.Fatalf("Mint's expiry = %v, want %v: the moment the exchange was asked plus expires_in", minted.Expires, want)
	}
	r.anthropic.set(func(s *anthropicStand) { s.during = nil })
	for _, lifetime := range []int{60, 300, 3600} {
		r.anthropic.set(func(s *anthropicStand) { s.answer["expires_in"] = lifetime })
		if _, err := r.mint(t, federationValue(federationAccount, "")); err != nil {
			t.Fatalf("Mint of an answer with expires_in %d = %v, want the token", lifetime, err)
		}
	}
	for _, lifetime := range []int{3601, 86400} {
		r.anthropic.set(func(s *anthropicStand) { s.answer["expires_in"] = lifetime })
		minted, err := r.mint(t, federationValue(federationAccount, ""))
		if !errors.Is(err, secrets.ErrMintRefused) || minted.Value != "" || !strings.Contains(err.Error(), "token_lifetime_seconds") {
			t.Fatalf("Mint of an answer with expires_in %d = %+v, %v; want ErrMintRefused naming token_lifetime_seconds and no value", lifetime, minted, err)
		}
		if strings.Contains(err.Error(), federationToken) {
			t.Fatalf("the refusal quotes the token: %v", err)
		}
	}
	for _, lifetime := range []any{0, -5, nil} {
		r.anthropic.set(func(s *anthropicStand) { s.answer["expires_in"] = lifetime })
		if _, err := r.mint(t, federationValue(federationAccount, "")); !errors.Is(err, secrets.ErrMintUpstream) {
			t.Fatalf("Mint of an answer with expires_in %v = %v, want ErrMintUpstream", lifetime, err)
		}
	}
}

// TestTheFederationExchangeIsSentOnce: an exchange Anthropic fails with a 503, which every other
// minter tries again, is sent once, refused ErrMintUpstream and counted as one attempt under the
// type; and each mint presents an identity token it read for that mint, never one an earlier mint
// presented, so IAM is asked once per mint.
func TestTheFederationExchangeIsSentOnce(t *testing.T) {
	r := newFederationRig(t)
	r.anthropic.set(func(s *anthropicStand) {
		s.status = http.StatusServiceUnavailable
		s.answer = map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error"}}
	})
	if minted, err := r.mint(t, federationValue(federationAccount, "")); !errors.Is(err, secrets.ErrMintUpstream) || minted.Value != "" {
		t.Fatalf("Mint of a 503 = %+v, %v; want ErrMintUpstream and no value", minted, err)
	}
	if exchanged := r.anthropic.requests(); len(exchanged) != 1 {
		t.Fatalf("Anthropic got %d exchanges of a 503, want one", len(exchanged))
	}
	if samples := attemptSamples(r.registry); !strings.Contains(samples, `broker_mint_attempts_total{type="anthropic-federation",outcome="upstream",attempt="1"} 1`+"\n") ||
		strings.Contains(samples, `attempt="2"`) {
		t.Fatalf("the attempts counter holds\n%s\nwant one upstream attempt under anthropic-federation and no second", samples)
	}
	r.anthropic.set(func(s *anthropicStand) {
		s.status = 0
		s.answer = map[string]any{"access_token": federationToken, "token_type": "Bearer", "expires_in": 3600}
	})
	if _, err := r.mint(t, federationValue(federationAccount, "")); err != nil {
		t.Fatalf("Mint after the 503 = %v, want the token", err)
	}
	if asked := r.iam.Requests(); len(asked) != 2 {
		t.Fatalf("two mints asked IAM %d times, want twice: one identity token per exchange", len(asked))
	}
}

// TestTheFederationExchangeFollowsNoRedirect: an exchange answered with a 307 or a 308 releases
// nothing, and the body carrying the identity token never reaches the address the redirect names.
func TestTheFederationExchangeFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": federationToken, "expires_in": 3600})
	}))
	t.Cleanup(other.Close)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, status)
		}))
		r := newFederationRig(t)
		r.minter.TokenURL = redirecting.URL + "/v1/oauth/token"
		minted, err := r.mint(t, federationValue(federationAccount, ""))
		redirecting.Close()
		if err == nil || minted.Value != "" {
			t.Fatalf("Mint answered %d = %+v, %v; want it refused, with no value", status, minted, err)
		}
	}
	if got := elsewhere.Load(); got != 0 {
		t.Fatalf("the redirect's target got %d exchanges, want none", got)
	}
}

// TestTwoConcurrentFederationMintsPresentTwoIdentityTokens: two mints of one secret made at once,
// through the identity-token reader cmd/cipherd builds, each mint an identity token of their own
// while IAM holds the first, and present different ones, so no two exchanges carry one token. A
// reader that joined concurrent reads into one mint would hand both exchanges one token.
func TestTwoConcurrentFederationMintsPresentTwoIdentityTokens(t *testing.T) {
	ciphertokentest.NewMetadataStand(t)
	var minted atomic.Int64
	gate := make(chan struct{})
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := minted.Add(1)
		<-gate
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fmt.Sprintf("identity-%d", n)})
	}))
	t.Cleanup(iam.Close)
	anthropic := newAnthropicStand(t)
	minter := &secrets.AnthropicFederation{
		Accounts:      map[string]string{"FED_ADMIN": federationAccount},
		IdentityToken: (&ciphertoken.GoogleAccounts{Audience: secrets.AnthropicAudience, IAMCredentialsURL: iam.URL}).Read,
		TokenURL:      anthropic.server.URL + "/v1/oauth/token",
	}
	var done sync.WaitGroup
	for range 2 {
		done.Go(func() {
			if _, err := minter.Mint(context.Background(), secrets.MintRequest{Name: "FED_ADMIN", Source: "FED_ADMIN", Type: "anthropic-federation", HumanTier: true, Value: federationValue(federationAccount, "")}); err != nil {
				t.Errorf("Mint: %v", err)
			}
		})
	}
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool { return minted.Load() >= 2 }) {
		close(gate)
		done.Wait()
		t.Fatalf("two concurrent mints reached IAM %d times within the test's bound while it held them, want twice", minted.Load())
	}
	close(gate)
	done.Wait()
	exchanged := anthropic.requests()
	if len(exchanged) != 2 || exchanged[0]["assertion"] == exchanged[1]["assertion"] {
		t.Fatalf("the two exchanges presented %v; want two different identity tokens", exchanged)
	}
}

// TestAFederationSecretBoundToNoAccountReleasesNothing: a secret the deployment binds to no
// account, and one whose stored value names another account than the bound one, are refused
// ErrMintNotBound before any call is made; so is a value the decoder refuses, ErrMintStoredValue;
// and a minter given no identity-token reader refuses ErrMintUpstream and exchanges nothing.
func TestAFederationSecretBoundToNoAccountReleasesNothing(t *testing.T) {
	r := newFederationRig(t)
	if _, err := r.minter.Mint(t.Context(), secrets.MintRequest{Name: "UNBOUND", Source: "UNBOUND", Value: federationValue(federationAccount, "")}); !errors.Is(err, secrets.ErrMintNotBound) {
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
	unread := *r.minter
	unread.IdentityToken = nil
	if minted, err := unread.Mint(t.Context(), secrets.MintRequest{Name: "FED_ADMIN", Source: "FED_ADMIN", Type: "anthropic-federation", Value: federationValue(federationAccount, "")}); !errors.Is(err, secrets.ErrMintUpstream) || minted.Value != "" {
		t.Fatalf("Mint with no identity-token reader = %+v, %v; want ErrMintUpstream and no value", minted, err)
	}
	if asked := r.iam.Requests(); len(asked) != 0 {
		t.Fatalf("a refused mint asked IAM %d times, want none", len(asked))
	}
	if exchanged := r.anthropic.requests(); len(exchanged) != 0 {
		t.Fatalf("a refused mint exchanged %d times, want none", len(exchanged))
	}
}

// TestAnAnthropicFederationProfileMintsUnderItsSourcesBinding: the deployment binds each
// anthropic-federation secret by its stored name (BROKER_SERVICE_ACCOUNT_SECRETS), which
// MintRequest.Source carries, as it binds a google-service-account secret: a profile of a bound
// secret mints under that binding whatever the profile is called, and a profile of an unbound
// secret is refused ErrMintNotBound even where its own name is one the deployment binds, naming
// the profile and the source it is of, before IAM or Anthropic is asked.
func TestAnAnthropicFederationProfileMintsUnderItsSourcesBinding(t *testing.T) {
	r := newFederationRig(t)
	for _, req := range []secrets.MintRequest{
		{Name: "FED_ADMIN", Source: "FED_ADMIN"},
		{Name: "FED_ADMIN_READ", Source: "FED_ADMIN"},
	} {
		req.Type, req.HumanTier, req.Value = "anthropic-federation", true, federationValue(federationAccount, "")
		if minted, err := r.minter.Mint(t.Context(), req); err != nil || minted.Value != federationToken {
			t.Fatalf("Mint of %s, of %s = %+v, %v; want the token, minted under %s's binding", req.Name, req.Source, minted, err, req.Source)
		}
	}
	asked, exchanged := len(r.iam.Requests()), len(r.anthropic.requests())
	for _, req := range []secrets.MintRequest{
		{Name: "UNBOUND_READ", Source: "UNBOUND"},
		{Name: "FED_ADMIN", Source: "UNBOUND"},
	} {
		req.Type, req.HumanTier, req.Value = "anthropic-federation", true, federationValue(federationAccount, "")
		_, err := r.minter.Mint(t.Context(), req)
		if !errors.Is(err, secrets.ErrMintNotBound) || !strings.Contains(err.Error(), req.Name) || !strings.Contains(err.Error(), "UNBOUND") {
			t.Fatalf("Mint of %s, of %s = %v; want ErrMintNotBound naming both", req.Name, req.Source, err)
		}
	}
	if len(r.iam.Requests()) != asked || len(r.anthropic.requests()) != exchanged {
		t.Fatal("a mint the binding refused asked IAM or Anthropic")
	}
}
