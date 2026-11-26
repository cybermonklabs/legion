// vm_identity_test.go pins how the broker boots with Google identity tokens: the Google issuer's
// key set is read, and named in one boot line, only when BROKER_GOOGLE_OIDC_AUDIENCE is set; the
// machine services reach the policy loader; and a Google audience with no Google caller, or a
// person's device sign-in with no machine service, refuses the boot. It also pins the boot's read
// of one bare cluster issuer, which the Google rows share.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/judge"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// exampleVMAccount is the one VM service account these rows bind, a placeholder: no real account,
// project or instance name appears in a test.
const exampleVMAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// vmConfig loads the broker's configuration with cluster as its one cluster issuer, a bare URL,
// and, when audience is set, one VM service: example-fleet, run by exampleVMAccount in
// example-project as example-vm-*.
func vmConfig(t *testing.T, cluster, audience string) config.Config {
	t.Helper()
	env := map[string]string{
		"BROKER_DATABASE_URL":        "postgres://unused",
		"BROKER_PUBLIC_URL":          "https://secrets.internal.example",
		"BROKER_MACHINE_LOGIN_URL":   "https://dispatch.example/credentials/machine",
		"BROKER_SECRETS_PREFIX":      testSecretsPrefix,
		"BROKER_SECRETS_KMS_KEY_ARN": testSecretsKMSKeyARN,
		"BROKER_K8S_OIDC_ISSUER":     cluster,
		"BROKER_K8S_OIDC_AUDIENCE":   "example-broker-pod",
		"BROKER_SERVICES":            "example-service=system:serviceaccount:example:example-sa",
		// The four the approval page's sign-in needs, which every broker refuses to start
		// without: this configuration is a whole one, as a deployment's is.
		"BROKER_SIGNIN_ISSUER":             testSignInIssuer,
		"BROKER_SIGNIN_CLIENT_ID":          ciphertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE": signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN":             ciphertest.SignInDomain,
	}
	if audience != "" {
		env["BROKER_GOOGLE_OIDC_AUDIENCE"] = audience
		env["BROKER_VM_SERVICES"] = "example-fleet=" + exampleVMAccount
		env["BROKER_VM_PROJECTS"] = exampleVMAccount + "=example-project"
		env["BROKER_VM_INSTANCES"] = exampleVMAccount + "=example-vm-*"
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// discovery is a keysFunc that records each issuer whose keys are asked for and answers a key
// source of its own for each, reading nothing.
type discovery struct {
	asked []string
	keys  map[string]*oidc.Keys
}

func (d *discovery) keysFor(_ context.Context, issuer string) (*oidc.Keys, error) {
	d.asked = append(d.asked, issuer)
	if d.keys == nil {
		d.keys = map[string]*oidc.Keys{}
	}
	d.keys[issuer] = &oidc.Keys{}
	return d.keys[issuer], nil
}

// localCluster is a cluster issuer this process serves, for a row that runs the boot's read of the
// clusters' issuers, which reads each one over HTTP: a placeholder host would leave that read
// waiting on a name no resolver holds.
func localCluster(t *testing.T) *oidctest.Issuer {
	t.Helper()
	cluster := oidctest.New(t)
	cluster.PublishKey(t, "cluster-key")
	return cluster
}

// captureLog sends slog's default logger to a buffer until t ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	return &logged
}

// TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer: with one bare cluster issuer URL and no
// Google audience, the broker reads that one issuer at boot and logs one line naming its URL and
// that it was read; builds the pod verifier over it, which verifies that issuer's tokens; reads no
// key set through keysFor, since there is neither an exchange audience nor a Google one; and logs
// nothing about Google.
func TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer(t *testing.T) {
	logged := captureLog(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	d := &discovery{}
	found, err := discoverVerifiers(context.Background(), vmConfig(t, cluster.URL(), ""), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.asked) != 0 {
		t.Fatalf("read the key sets of %v at boot, want none: the clusters' issuers are the pod verifier's own to read, and there is no exchange or Google issuer", d.asked)
	}
	wantLine := `msg="` + issuerBootMessage + `" url=` + cluster.URL() + ` read=true`
	if lines := strings.Count(logged.String(), `msg="`+issuerBootMessage+`"`); lines != 1 || !strings.Contains(logged.String(), wantLine) {
		t.Fatalf("log = %q, want one line holding %q: the one issuer, read at boot", logged, wantLine)
	}
	pods, ok := found.Pod.(enroll.K8sPodVerifier)
	if !ok {
		t.Fatalf("verifiers = pod %#v; want an enroll.K8sPodVerifier", found.Pod)
	}
	if _, ok := pods.Issuers.(loggedIssuers); !ok {
		t.Fatalf("the pod verifier checks tokens with %T, want loggedIssuers over the clusters' oidc.IssuerSet", pods.Issuers)
	}
	token := cluster.Mint(t, clusterKey, cluster.Claims("system:serviceaccount:example:example-sa", "example-broker-pod"))
	if claims, err := pods.VerifyAccount(context.Background(), token); err != nil || claims.Issuer != (oidc.NamedIssuer{URL: cluster.URL()}) {
		t.Fatalf("the pod verifier on a token of the cluster's issuer = %+v, %v; want it verified, naming that issuer with no name", claims, err)
	}
	if found.ExchangeK8s != nil || found.GoogleKeys != nil {
		t.Fatalf("the exchange was handed cluster issuers (%+v) or a Google key set (%p) with neither audience set; it must then register neither subject token type", found.ExchangeK8s, found.GoogleKeys)
	}
	if strings.Contains(strings.ToLower(logged.String()), "google") {
		t.Fatalf("log with no Google audience names Google:\n%s", logged)
	}
}

// TestAGoogleAudienceDiscoversGooglesIssuer: with BROKER_GOOGLE_OIDC_AUDIENCE set the broker also
// reads the Google issuer's key set and hands it to the token exchange, which is what lets the
// exchange register the Google subject token type and resolve a machine's token against Google's
// own keys, and logs one line naming the issuer, the audience, the machine services and how many
// machine entries the exchange holds.
func TestAGoogleAudienceDiscoversGooglesIssuer(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	found, err := discoverVerifiers(context.Background(), vmConfig(t, localCluster(t).URL(), "example-vm-audience"), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{config.DefaultGoogleOIDCIssuer}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if found.GoogleKeys == nil || found.GoogleKeys != d.keys[config.DefaultGoogleOIDCIssuer] {
		t.Fatalf("the exchange's Google key source = %p, want Google's own, %p", found.GoogleKeys, d.keys[config.DefaultGoogleOIDCIssuer])
	}
	lines := strings.Count(logged.String(), "google machine identity")
	wantLine := `msg="google machine identity" issuer=https://accounts.google.com audience=example-vm-audience services=example-fleet fleets=1`
	if lines != 1 || !strings.Contains(logged.String(), wantLine) {
		t.Fatalf("log = %q, want one line holding %q", logged, wantLine)
	}
}

// TestAStandInIssuerIsDiscoveredInGooglesPlace: BROKER_GOOGLE_OIDC_ISSUER names the issuer whose
// keys the broker reads and whose tokens the exchange takes, so a dev stack's stand-in
// (cmd/dev-oidc-issuer) serves in Google's place with nothing else changed.
func TestAStandInIssuerIsDiscoveredInGooglesPlace(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	cfg := vmConfig(t, localCluster(t).URL(), "example-vm-audience")
	cfg.GoogleOIDCIssuer = "http://127.0.0.1:18080"
	found, err := discoverVerifiers(context.Background(), cfg, d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{cfg.GoogleOIDCIssuer}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if found.GoogleKeys == nil || found.GoogleKeys != d.keys[cfg.GoogleOIDCIssuer] {
		t.Fatalf("the exchange's Google key source = %p, want the stand-in issuer's own, %p", found.GoogleKeys, d.keys[cfg.GoogleOIDCIssuer])
	}
	if !strings.Contains(logged.String(), "issuer=http://127.0.0.1:18080") {
		t.Fatalf("log = %q, want the stand-in issuer named", logged)
	}
}

// TestTheEnrollmentServiceCarriesTheMachineServices pins the two places the broker hands
// BROKER_VM_SERVICES on: AuthenticateLauncher, without which a machine's own launcher credential
// stops authenticating at its next call, and the request decisions, without which a machine's
// session is never its fleet's.
func TestTheEnrollmentServiceCarriesTheMachineServices(t *testing.T) {
	cfg := vmConfig(t, "https://cluster.example", "example-vm-audience")
	enr := &enroll.Service{VMServicesByAccount: cfg.VMServicesByAccount}
	if want := map[string]string{exampleVMAccount: "example-fleet"}; !maps.Equal(enr.VMServicesByAccount, want) {
		t.Fatalf("the enrollment service's VMServicesByAccount = %v, want %v", enr.VMServicesByAccount, want)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil, "", nil, nil, nil)
	if want := map[string]string{"system:serviceaccount:example:example-sa": "example-service"}; !maps.Equal(m.ServicesByAccount, want) {
		t.Fatalf("request Machine's ServicesByAccount = %v, want %v", m.ServicesByAccount, want)
	}
	if want := map[string]string{exampleVMAccount: "example-fleet"}; !maps.Equal(m.VMServicesByAccount, want) {
		t.Fatalf("request Machine's VMServicesByAccount = %v, want %v: without it no machine's session is ever its fleet's", m.VMServicesByAccount, want)
	}
}

// TestTheRequestMachineCarriesWhoIsResponsible pins the two things the broker hands the request
// decisions to find the person a shared secret's request is listed for: each fleet's person by
// account (BROKER_VM_EXCHANGE_ACCOUNTS), an entry that names none left out, and the broker's one
// read of Dispatch (judgeDispatch), which answers a Legion pod's issue's assignee, only where
// BROKER_JUDGE_DISPATCH_URL is set, and is the very read main gives the model judge.
func TestTheRequestMachineCarriesWhoIsResponsible(t *testing.T) {
	const otherAccount = "example-other@example-project.iam.gserviceaccount.com"
	cfg := vmConfig(t, "https://cluster.example", "example-vm-audience")
	cfg.ExchangeGoogleAccounts = []exchange.GoogleEntry{
		{Account: exampleVMAccount, Service: "example-fleet", Project: "ACME", Person: "ada@example.com"},
		{Account: otherAccount, Service: "example-fleet"},
	}
	if dispatch := judgeDispatch(cfg); dispatch != nil {
		t.Fatalf("judgeDispatch = %#v with no BROKER_JUDGE_DISPATCH_URL; want none", dispatch)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil, "", nil, judgeDispatch(cfg), nil)
	if want := map[string]string{exampleVMAccount: "ada@example.com"}; !maps.Equal(m.VMPersonsByAccount, want) {
		t.Fatalf("request Machine's VMPersonsByAccount = %v, want %v", m.VMPersonsByAccount, want)
	}
	if m.Assignees != nil {
		t.Fatalf("request Machine's Assignees = %#v with no BROKER_JUDGE_DISPATCH_URL; want none", m.Assignees)
	}

	cfg.JudgeDispatchURL = "https://dispatch.internal.example"
	cfg.JudgeDispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	dispatch := judgeDispatch(cfg)
	if dispatch == nil || dispatch.BaseURL != cfg.JudgeDispatchURL || dispatch.Account == nil {
		t.Fatalf("judgeDispatch = %#v; want a read of Dispatch at %s", dispatch, cfg.JudgeDispatchURL)
	}
	m = newRequestMachine(cfg, nil, nil, nil, nil, "", nil, dispatch, nil)
	if got, ok := m.Assignees.(*judge.Dispatch); !ok || got != dispatch {
		t.Fatalf("request Machine's Assignees = %#v; want the one read of Dispatch judgeDispatch built, %p", m.Assignees, dispatch)
	}
}

// TestThePolicyServesTheVMServicesSecrets: the policy loader's services are BROKER_SERVICES' and
// BROKER_VM_SERVICES', sorted and each once, so a secret a machine service owns is that
// service's and not refused as owner-tag-malformed.
func TestThePolicyServesTheVMServicesSecrets(t *testing.T) {
	if got, want := serviceNames(vmConfig(t, "https://cluster.example", "example-vm-audience")), []string{"example-fleet", "example-service"}; !slices.Equal(got, want) {
		t.Fatalf("serviceNames = %v, want %v", got, want)
	}
	if got, want := serviceNames(vmConfig(t, "https://cluster.example", "")), []string{"example-service"}; !slices.Equal(got, want) {
		t.Fatalf("serviceNames with no VM service = %v, want %v", got, want)
	}
}

// TestDeviceSignInWithNoVMServicesIsRefusedAtBoot drives the real binary: a person's device
// sign-in needs a VM service for the machine's actor token, even when a Google API caller uses
// the same audience. The refusal comes before the broker reaches its database or binds.
func TestDeviceSignInWithNoVMServicesIsRefusedAtBoot(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_GOOGLE_OIDC_AUDIENCE=example-vm-audience",
		"BROKER_SIGNIN_DEVICE_AUDIENCE=not-a-secret-device-client",
		"BROKER_API_ACCOUNTS=listener=google:"+listenerVMAccount,
	)
	if !strings.Contains(out, "BROKER_SIGNIN_DEVICE_AUDIENCE needs BROKER_VM_SERVICES") {
		t.Fatalf("broker refused, but not naming the device sign-in's missing VM services: %s", out)
	}
}

// TestAGoogleAudienceWithNoGoogleCallerIsRefusedAtBoot drives the real binary: an audience that
// no Google caller would ever present a token for — no google: entry in BROKER_API_ACCOUNTS, no
// VM service and no google: edge-listener entry in BROKER_NATS_SERVICES — refuses the boot,
// naming the audience and all three ways to give it a caller, before the broker reaches its
// database or binds.
func TestAGoogleAudienceWithNoGoogleCallerIsRefusedAtBoot(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_GOOGLE_OIDC_AUDIENCE=example-vm-audience",
	)
	if !strings.Contains(out, "BROKER_GOOGLE_OIDC_AUDIENCE needs a Google caller: a google: entry in BROKER_API_ACCOUNTS, a VM service in BROKER_VM_SERVICES or a google: edge-listener entry in BROKER_NATS_SERVICES") {
		t.Fatalf("broker refused, but not naming the audience's missing Google caller: %s", out)
	}
}

// TestWithNoGoogleAudienceTheBootLogHoldsNoGoogleLine drives the real binary with the cluster's
// issuer and no Google audience: it boots, and its boot log names neither Google's issuer nor a VM
// identity line.
func TestWithNoGoogleAudienceTheBootLogHoldsNoGoogleLine(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=example-broker-pod",
	}})
	for _, never := range []string{config.DefaultGoogleOIDCIssuer, "google machine identity"} {
		if strings.Contains(broker.Boot, never) {
			t.Fatalf("boot log with no Google audience holds %q:\n%s", never, broker.Boot)
		}
	}
}

// TestTheSettingsNameOnlyAnAudienceAMachineCanTradeFor drives the real binary twice. A broker that
// lists a VM service publishes BROKER_GOOGLE_OIDC_AUDIENCE as google_exchange_audience in GET
// /v1/settings, and one whose only Google caller is an API account publishes none, since its
// exchange refuses every machine and a client that read the audience would mint, trade and be
// refused on every command. Neither names a Kubernetes exchange audience, though both set
// BROKER_EXCHANGE_K8S_AUDIENCE: a pod's trade there mints no launcher credential, so no machine
// could register a session under it.
func TestTheSettingsNameOnlyAnAudienceAMachineCanTradeFor(t *testing.T) {
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	google.PublishKey(t, "google-key")
	published := func(t *testing.T, env []string) (string, bool) {
		t.Helper()
		broker := startBroker(t, brokerOptions{Env: env})
		broker.Await(t, "token exchange")
		resp, err := http.Get("http://" + broker.Addr + "/v1/settings")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var settings map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&settings); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/settings = %d, %v", resp.StatusCode, err)
		}
		if kubernetes, named := settings["kubernetes_exchange_audience"]; named {
			t.Fatalf("GET /v1/settings names the Kubernetes exchange audience %v; want none, since a pod's trade mints no launcher credential", kubernetes)
		}
		audience, ok := settings["google_exchange_audience"].(string)
		return audience, ok
	}

	listed := googleExchangeEnv(t, storetest.URL(t), cluster, github, google, longAccessSeconds)
	if !slices.Contains(listed, "BROKER_EXCHANGE_K8S_AUDIENCE="+exchangeAudience) {
		t.Fatalf("the broker's settings name no BROKER_EXCHANGE_K8S_AUDIENCE, so this row could not see it published: %v", listed)
	}
	if audience, ok := published(t, listed); !ok || audience != machineAudience {
		t.Fatalf("a broker listing VM services publishes google_exchange_audience %q (%v); want %q", audience, ok, machineAudience)
	}

	apiCallerOnly := append(exchangeEnv(t, storetest.URL(t), cluster, github, longAccessSeconds),
		"BROKER_GOOGLE_OIDC_ISSUER="+google.URL(),
		"BROKER_GOOGLE_OIDC_AUDIENCE="+machineAudience,
		"BROKER_API_ACCOUNTS=dispatch="+apiAccount+" listener=google:"+listenerVMAccount)
	for _, setting := range []string{"BROKER_VM_SERVICES", "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES", "BROKER_VM_EXCHANGE_ACCOUNTS"} {
		apiCallerOnly = append(apiCallerOnly, setting+"=")
	}
	if audience, ok := published(t, apiCallerOnly); ok {
		t.Fatalf("a broker whose only Google caller is an API account publishes google_exchange_audience %q; want none, since no machine can trade a token there", audience)
	}
}
