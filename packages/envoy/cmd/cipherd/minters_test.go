package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// appScope is the scope of a github-app profile of the one account that reads its repositories'
// contents, with the metadata at read every GitHub profile names, as requests' release builds it
// from a profile: secrets.GitHubApp.Mint refuses a mint with no scope before GitHub is asked
// anything, so every github-app mint below carries one.
func appScope(account string) *secrets.GitHubScope {
	return &secrets.GitHubScope{Accounts: []string{account}, Permissions: map[string]string{"contents": "read", "metadata": "read"}, Value: githubname.ValueInstallations}
}

// TestEachTypeReachesItsOwnMinter: the broker's minters send a github-app secret's value to the
// GitHub App minter, a google-login secret's to the Google login minter and a
// google-service-account secret's to the service account minter, each against an endpoint of its
// own, so wiring a type to another type's minter, which would refuse the value, fails here. The
// GitHub endpoint lists what its installation holds and names what its token holds, as GitHub
// does, since the minter cuts the profile's permissions to the one and checks the token against
// the other.
func TestEachTypeReachesItsOwnMinter(t *testing.T) {
	_, pemText := githubapptest.Key(t)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "account": map[string]string{"login": "acme-org"}, "permissions": map[string]string{"contents": "read", "metadata": "read"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "github-installation-token", "expires_at": "2026-10-24T13:00:00Z", "permissions": map[string]string{"contents": "read", "metadata": "read"}})
		default:
			t.Errorf("GitHub endpoint asked %s %s", r.Method, r.URL.Path)
		}
	}))
	defer github.Close()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "google-" + "access-token"})
	}))
	defer google.Close()
	const iamMinted, brokerOwn = "service-account-access-token", "broker-own-token"
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/-/serviceAccounts/dev-aditya@example-identities.iam.gserviceaccount.com:generateAccessToken" {
			t.Errorf("IAM endpoint asked %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": iamMinted})
	}))
	defer iam.Close()
	minters := typeMinters(
		secrets.GitHubApp{Base: github.URL, HTTP: github.Client()},
		secrets.GoogleLogin{TokenURL: google.URL, HTTP: google.Client()},
		secrets.AzureLogin{},
		&secrets.GoogleServiceAccount{
			BaseURL: iam.URL, HTTP: iam.Client(),
			Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: brokerOwn}),
			// The minter mints only for the account the deployment binds to the secret the
			// request names, so this stands in for BROKER_SERVICE_ACCOUNT_SECRETS; with no
			// binding every mint of the type would be refused before the IAM endpoint below
			// were asked anything, and this test would prove nothing about which minter the
			// type reaches.
			Accounts: map[string]string{"DEV_PERSON": "dev-aditya@example-identities.iam.gserviceaccount.com"},
		},
	)

	app, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := minters[policy.TypeGitHubApp](context.Background(), secrets.MintRequest{Value: string(app), GitHub: appScope("acme-org")})
	if err != nil || !strings.Contains(minted.Value, "github-installation-token") {
		t.Fatalf("github-app minter = %q, %v; want the GitHub endpoint's installation token", minted.Value, err)
	}
	login := `{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`
	minted, err = minters[policy.TypeGoogleLogin](context.Background(), secrets.MintRequest{Value: login})
	if err != nil || minted.Value != "google-access-token" {
		t.Fatalf("google-login minter = %q, %v; want the Google endpoint's access token", minted.Value, err)
	}
	account := `{"service_account":"dev-aditya@example-identities.iam.gserviceaccount.com"}`
	minted, err = minters[policy.TypeGoogleServiceAccount](context.Background(), secrets.MintRequest{Name: "DEV_PERSON", Source: "DEV_PERSON", Value: account})
	if err != nil || minted.Value != "service-account-access-token" {
		t.Fatalf("google-service-account minter = %q, %v; want the IAM endpoint's access token", minted.Value, err)
	}
}

// TestTypeMinters: the map typeMinters answers holds one minter for every type the policy serves
// and nothing else, so a type added to policy.Types with no minter behind it, or a minter left
// under a key no type names, fails here rather than when a session asks for that secret.
func TestTypeMinters(t *testing.T) {
	minters := typeMinters(secrets.GitHubApp{}, secrets.GoogleLogin{}, secrets.AzureLogin{}, &secrets.GoogleServiceAccount{})
	for _, secretType := range policy.Types {
		if minters[secretType] == nil {
			t.Errorf("typeMinters has no minter for type %q, which the policy serves", secretType)
		}
	}
	for secretType := range minters {
		if !policy.ValidType(secretType) {
			t.Errorf("typeMinters holds a minter under %q, which is no type the policy serves", secretType)
		}
	}
	if len(minters) != len(policy.Types) {
		t.Errorf("typeMinters has %d minters for %d types: %v", len(minters), len(policy.Types), minters)
	}
}

func TestRequestMachineReleasesAzureLoginFromTheConfiguredFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "eyJhbGciOiJSUzI1NiJ9" + "." + "eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := newRequestMachine(config.Config{AzureTokenFile: file, AzureLoginSecret: "AZURE_LOGIN"}, nil, nil, nil, nil, proxyrules.Rules{}, "", nil, nil)
	for _, secretType := range policy.Types {
		if machine.Minters[secretType] == nil {
			t.Fatalf("request machine has no minter for %s", secretType)
		}
	}
	mint := machine.Minters["azure-login"]
	if mint == nil {
		t.Fatal("request machine has no Azure login minter")
	}
	got, err := mint(t.Context(), secrets.MintRequest{Name: "AZURE_LOGIN", Source: "AZURE_LOGIN", HumanTier: true, Value: `{"tenant_id":"tenant","client_id":"client"}`})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var value map[string]string
	if json.Unmarshal([]byte(got.Value), &value) != nil || len(value) != 4 || value["tenant_id"] != "tenant" || value["client_id"] != "client" || value["federated_token"] != token || value["expires_at"] != "2100-01-01T00:00:00Z" {
		t.Fatal("request machine did not release the Azure login from its configured token file")
	}
}

// TestTypeMintersShareOneRetryBudget: the GitHub and Google minters newRequestMachine builds, as
// main builds them, hold the one Retry it is given, so their retries draw from one process-wide
// budget: thirty GitHub mints, each failing at a 503 three times, spend its 60 retries, and a
// Google login's 503 is then tried once. Two seconds on, the budget holds two retries again, and
// the same Google minter asks three times.
func TestTypeMintersShareOneRetryBudget(t *testing.T) {
	var githubCalls, googleCalls atomic.Int64
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer github.Close()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		googleCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer google.Close()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	retry := secrets.NewRetry(metrics.New())
	retry.Now = func() time.Time { return now }
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	minters := newRequestMachine(config.Config{AirlockTestGitHubAPIBase: github.URL, AirlockTestGoogleTokenURL: google.URL}, nil, nil, nil, nil, proxyrules.Rules{}, "", retry, nil).Minters
	app := secrets.MintRequest{Type: policy.TypeGitHubApp, Value: policytest.TypeValue(policy.TypeGitHubApp), GitHub: appScope("acme-org")}
	login := secrets.MintRequest{Type: policy.TypeGoogleLogin, Value: policytest.TypeValue(policy.TypeGoogleLogin)}

	for range 30 {
		if _, err := minters[policy.TypeGitHubApp](t.Context(), app); secrets.MintAttempts(err) != 3 {
			t.Fatalf("a github-app mint against a 503 = %v after %d attempts; want 3", err, secrets.MintAttempts(err))
		}
	}
	if githubCalls.Load() != 90 {
		t.Fatalf("thirty github-app mints made %d calls; want 90", githubCalls.Load())
	}
	if _, err := minters[policy.TypeGoogleLogin](t.Context(), login); googleCalls.Load() != 1 || secrets.MintAttempts(err) != 1 {
		t.Fatalf("a google-login mint once GitHub's spent the budget = %v after %d calls; want one attempt", err, googleCalls.Load())
	}
	now = now.Add(2 * time.Second)
	if _, err := minters[policy.TypeGoogleLogin](t.Context(), login); googleCalls.Load() != 4 || secrets.MintAttempts(err) != 3 {
		t.Fatalf("a google-login mint two seconds on = %v after %d calls in all; want three attempts more", err, googleCalls.Load())
	}
}

// TestRequestMachineCountsEachMintAttemptOnItsRegistry: the minters newRequestMachine builds
// retry under the Retry main builds on its registry (secrets.NewRetry). A github-app mint against
// the GitHub stand answering its token request 503 once and then as ordinary mints at the second
// attempt, and the registry's own handler serves both attempts in broker_mint_attempts_total.
func TestRequestMachineCountsEachMintAttemptOnItsRegistry(t *testing.T) {
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	stand := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}, Answer: proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensFailOnce}})
	service.AnswerWith(stand.Answer)
	registry := metrics.New()
	machine := newRequestMachine(config.Config{AirlockTestGitHubAPIBase: service.URL()}, nil, nil, nil, nil, proxyrules.Rules{}, "", secrets.NewRetry(registry), nil)

	minted, err := machine.Minters[policy.TypeGitHubApp](t.Context(), secrets.MintRequest{
		Type: policy.TypeGitHubApp, Value: policytest.TypeValue(policy.TypeGitHubApp), GitHub: appScope("acme-org"),
	})
	if err != nil || !strings.Contains(minted.Value, "ghs_standin_acme-org") {
		t.Fatalf("a github-app mint through one 503 = %q, %v; want acme-org's token", minted.Value, err)
	}
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, sample := range []string{
		`broker_mint_attempts_total{type="github-app",outcome="upstream",attempt="1"} 1`,
		`broker_mint_attempts_total{type="github-app",outcome="ok",attempt="2"} 1`,
	} {
		if !strings.Contains(recorder.Body.String(), sample+"\n") {
			t.Errorf("the registry serves\n%s\nwant the sample %s", recorder.Body.String(), sample)
		}
	}
}

// TestRequestMachineGivesBackAGitHubAppTokenAtTheMintersGitHub: newRequestMachine gives a release
// that fails after a github-app mint the GiveBack of the same GitHub its minter mints at, so the
// token that release minted is revoked there: a read made with it afterwards is refused.
func TestRequestMachineGivesBackAGitHubAppTokenAtTheMintersGitHub(t *testing.T) {
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	stand := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}})
	service.AnswerWith(stand.Answer)
	machine := newRequestMachine(config.Config{AirlockTestGitHubAPIBase: service.URL()}, nil, nil, nil, nil, proxyrules.Rules{}, "", secrets.NewRetry(metrics.New()), nil)
	giveBack := machine.GiveBacks[policy.TypeGitHubApp]
	if giveBack == nil || len(machine.GiveBacks) != 1 {
		t.Fatalf("the machine's GiveBacks hold %d types, github-app's %t; want github-app's alone", len(machine.GiveBacks), giveBack != nil)
	}
	minted, err := machine.Minters[policy.TypeGitHubApp](t.Context(), secrets.MintRequest{
		Type: policy.TypeGitHubApp, Value: policytest.TypeValue(policy.TypeGitHubApp), GitHub: appScope("acme-org"),
	})
	if err != nil {
		t.Fatalf("a github-app mint: %v", err)
	}
	var installations struct {
		Installations map[string]struct {
			Token string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(minted.Value), &installations); err != nil || installations.Installations["acme-org"].Token == "" {
		t.Fatalf("the minted value holds no token for acme-org: %v", err)
	}
	reads := func() int {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL()+"/repos/acme-org/proofs", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+installations.Installations["acme-org"].Token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := reads(); status != http.StatusOK {
		t.Fatalf("a read with the minted token answered %d, want 200", status)
	}
	giveBack(t.Context(), "APP_KEY", minted.Value, githubname.ValueInstallations)
	if status := reads(); status != http.StatusUnauthorized {
		t.Fatalf("a read with the given-back token answered %d, want 401", status)
	}
}
