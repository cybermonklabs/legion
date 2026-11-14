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

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// TestEachKindReachesItsOwnMinter: the broker's minters send a github-app secret's value to the
// GitHub App minter, a google-login secret's to the Google login minter and a
// google-service-account secret's to the service account minter, each against an endpoint of its
// own, so wiring a kind to another kind's minter, which would refuse the value, fails here.
func TestEachKindReachesItsOwnMinter(t *testing.T) {
	_, pemText := githubapptest.Key(t)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "account": map[string]string{"login": "acme-org"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "github-installation-token", "expires_at": "2026-10-24T13:00:00Z"})
		default:
			t.Errorf("GitHub endpoint asked %s %s", r.Method, r.URL.Path)
		}
	}))
	defer github.Close()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "google-access-token"})
	}))
	defer google.Close()
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/-/serviceAccounts/dev-aditya@theorem-agent-identities.iam.gserviceaccount.com:generateAccessToken" {
			t.Errorf("IAM endpoint asked %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "service-account-access-token"})
	}))
	defer iam.Close()
	minters := kindMinters(
		secrets.GitHubApp{Base: github.URL, HTTP: github.Client()},
		secrets.GoogleLogin{TokenURL: google.URL, HTTP: google.Client()},
		secrets.AzureLogin{},
		&secrets.GoogleServiceAccount{
			BaseURL: iam.URL, HTTP: iam.Client(),
			Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "broker-own-token"}),
			// The minter mints only for the account the deployment binds to the secret the
			// request names, so this stands in for BROKER_SERVICE_ACCOUNT_SECRETS; with no
			// binding every mint of the kind would be refused before the IAM endpoint below
			// were asked anything, and this test would prove nothing about which minter the
			// kind reaches.
			Accounts: map[string]string{"DEV_ADITYA": "dev-aditya@theorem-agent-identities.iam.gserviceaccount.com"},
		},
	)

	app, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := minters[policy.KindGitHubApp](context.Background(), secrets.MintRequest{Value: string(app), GitHubOwners: []string{"acme-org"}})
	if err != nil || !strings.Contains(minted.Value, "github-installation-token") {
		t.Fatalf("github-app minter = %q, %v; want the GitHub endpoint's installation token", minted.Value, err)
	}
	login := `{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`
	minted, err = minters[policy.KindGoogleLogin](context.Background(), secrets.MintRequest{Value: login})
	if err != nil || minted.Value != "google-access-token" {
		t.Fatalf("google-login minter = %q, %v; want the Google endpoint's access token", minted.Value, err)
	}
	account := `{"service_account":"dev-aditya@theorem-agent-identities.iam.gserviceaccount.com"}`
	minted, err = minters[policy.KindGoogleServiceAccount](context.Background(), secrets.MintRequest{Name: "DEV_ADITYA", Value: account})
	if err != nil || minted.Value != "service-account-access-token" {
		t.Fatalf("google-service-account minter = %q, %v; want the IAM endpoint's access token", minted.Value, err)
	}
}

// TestKindMinters: the map kindMinters answers holds one minter for every kind the policy serves
// and nothing else, so a kind added to policy.Kinds with no minter behind it, or a minter left
// under a key no kind names, fails here rather than when a session asks for that secret.
func TestKindMinters(t *testing.T) {
	minters := kindMinters(secrets.GitHubApp{}, secrets.GoogleLogin{}, secrets.AzureLogin{}, &secrets.GoogleServiceAccount{})
	for _, kind := range policy.Kinds {
		if minters[kind] == nil {
			t.Errorf("kindMinters has no minter for kind %q, which the policy serves", kind)
		}
	}
	for kind := range minters {
		if !policy.ValidKind(kind) {
			t.Errorf("kindMinters holds a minter under %q, which is no kind the policy serves", kind)
		}
	}
	if len(minters) != len(policy.Kinds) {
		t.Errorf("kindMinters has %d minters for %d kinds: %v", len(minters), len(policy.Kinds), minters)
	}
}

func TestRequestMachineReleasesAzureLoginFromTheConfiguredFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := newRequestMachine(config.Config{AzureTokenFile: file, AzureLoginSecret: "AZURE_LOGIN"}, nil, nil, nil, nil, airlock.Rules{}, "", metrics.New())
	for _, kind := range policy.Kinds {
		if machine.Minters[kind] == nil {
			t.Fatalf("request machine has no minter for %s", kind)
		}
	}
	mint := machine.Minters["azure-login"]
	if mint == nil {
		t.Fatal("request machine has no Azure login minter")
	}
	got, err := mint(t.Context(), secrets.MintRequest{Name: "AZURE_LOGIN", HumanTier: true, Value: `{"tenant_id":"tenant","client_id":"client"}`})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var value map[string]string
	if json.Unmarshal([]byte(got.Value), &value) != nil || len(value) != 4 || value["tenant_id"] != "tenant" || value["client_id"] != "client" || value["federated_token"] != token || value["expires_at"] != "2100-01-01T00:00:00Z" {
		t.Fatal("request machine did not release the Azure login from its configured token file")
	}
}

// TestKindMintersShareOneRetryBudget: the GitHub and Google minters kindMinters copies hold one
// Retry, so their retries draw from one process-wide budget: thirty GitHub mints, each failing at
// a 503 three times, spend its 60 retries, and a Google login's 503 is then tried once. Two
// seconds on, the budget holds two retries again, and the same Google minter asks three times.
func TestKindMintersShareOneRetryBudget(t *testing.T) {
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
	minters := kindMinters(
		secrets.GitHubApp{Base: github.URL, HTTP: github.Client(), Retry: retry},
		secrets.GoogleLogin{TokenURL: google.URL, HTTP: google.Client(), Retry: retry},
		secrets.AzureLogin{},
		&secrets.GoogleServiceAccount{Retry: retry},
	)
	app := secrets.MintRequest{Kind: policy.KindGitHubApp, Value: policytest.KindValue(policy.KindGitHubApp), GitHubOwners: []string{"acme-org"}}
	login := secrets.MintRequest{Kind: policy.KindGoogleLogin, Value: policytest.KindValue(policy.KindGoogleLogin)}

	for range 30 {
		if _, err := minters[policy.KindGitHubApp](t.Context(), app); secrets.MintAttempts(err) != 3 {
			t.Fatalf("a github-app mint against a 503 = %v after %d attempts; want 3", err, secrets.MintAttempts(err))
		}
	}
	if githubCalls.Load() != 90 {
		t.Fatalf("thirty github-app mints made %d calls; want 90", githubCalls.Load())
	}
	if _, err := minters[policy.KindGoogleLogin](t.Context(), login); googleCalls.Load() != 1 || secrets.MintAttempts(err) != 1 {
		t.Fatalf("a google-login mint once GitHub's spent the budget = %v after %d calls; want one attempt", err, googleCalls.Load())
	}
	now = now.Add(2 * time.Second)
	if _, err := minters[policy.KindGoogleLogin](t.Context(), login); googleCalls.Load() != 4 || secrets.MintAttempts(err) != 3 {
		t.Fatalf("a google-login mint two seconds on = %v after %d calls in all; want three attempts more", err, googleCalls.Load())
	}
}

// TestRequestMachineCountsEachMintAttemptOnItsRegistry: the minters newRequestMachine builds
// retry under the Retry it builds on the registry it is given. A github-app mint against the
// GitHub stand answering its token request 503 once and then as ordinary mints at the second
// attempt, and the registry's own handler serves both attempts in broker_mint_attempts_total.
func TestRequestMachineCountsEachMintAttemptOnItsRegistry(t *testing.T) {
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	stand := airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}, Answer: airlocktest.GitHubTokensFailOnce})
	service.AnswerWith(stand.Answer)
	registry := metrics.New()
	machine := newRequestMachine(config.Config{AirlockTestGitHubAPIBase: service.URL()}, nil, nil, nil, nil, airlock.Rules{}, "", registry)

	minted, err := machine.Minters[policy.KindGitHubApp](t.Context(), secrets.MintRequest{
		Kind: policy.KindGitHubApp, Value: policytest.KindValue(policy.KindGitHubApp), GitHubOwners: []string{"acme-org"},
	})
	if err != nil || !strings.Contains(minted.Value, "ghs_standin_acme-org") {
		t.Fatalf("a github-app mint through one 503 = %q, %v; want acme-org's token", minted.Value, err)
	}
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, sample := range []string{
		`broker_mint_attempts_total{kind="github-app",outcome="upstream",attempt="1"} 1`,
		`broker_mint_attempts_total{kind="github-app",outcome="ok",attempt="2"} 1`,
	} {
		if !strings.Contains(recorder.Body.String(), sample+"\n") {
			t.Errorf("the registry serves\n%s\nwant the sample %s", recorder.Body.String(), sample)
		}
	}
}
