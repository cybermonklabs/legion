// packages/envoy/cmd/cipherd/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refusePortZeroPublicURLInProduction is main's own port-0 refusal outside a local run,
// extracted so this test can drive it without exiting the test process.
package main

//ci:reads packages/envoy/docker/rds-global-bundle.pem TestMainMintsTokensBeforeOpeningTheDatabase names it as sslrootcert=../../docker/rds-global-bundle.pem, which the broker it starts opens

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testSecretsPrefix and testSecretsKMSKeyARN are the name prefix every agent secret sits under
// and the key every one of them is encrypted with, as productionEnv configures every broker
// these tests start (BROKER_SECRETS_PREFIX, BROKER_SECRETS_KMS_KEY_ARN). A fake secrets file a
// test writes names its secrets under this prefix and on this key, through these constants and
// not a literal of its own (multiprocess_test.go's fakeSecretsJSON): the policy loader refuses a
// secret under another prefix or on another key, which shows up as nothing but a secret never
// served, and one constant per fact is what leaves no second value to drift from this one.
const (
	testSecretsPrefix    = "example/agent-secrets/"
	testSecretsKMSKeyARN = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// testSignInIssuer is the provider every broker these tests start signs people in at. Nothing
// dials it: the approval page reads a provider's discovery document at a person's first sign-in,
// so a broker nobody signs in to never reaches for it. approval_page_test.go's fixture overrides
// these settings with a local issuer that answers.
const testSignInIssuer = "https://signin.example"

// signInClientSecretFile writes the sign-in client's secret into a file of t's own and answers
// its path, the file BROKER_SIGNIN_CLIENT_SECRET_FILE names.
func signInClientSecretFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signin-client-secret")
	if err := os.WriteFile(path, []byte(ciphertest.SignInClientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testMachineLoginURL is the page a person types a machine login's code into, as every broker
// these tests start that serves the device grant names it (BROKER_MACHINE_LOGIN_URL). It is not
// in productionEnv: the setting is optional, and the rows that need the device grant ask for it
// by name, so the broker every other row starts is the one that serves none.
const testMachineLoginURL = "https://dispatch.example/credentials/machine"

// productionEnv is the configuration every broker needs besides its addresses and database: the
// namespace it serves, the key every secret there is on, and the four settings a person signs
// in to its approval page with, which it refuses to start without. It also empties
// KUBERNETES_SERVICE_HOST, which Kubernetes sets in every container: a test run in a pod would
// otherwise hand it to each broker it starts, and every broker with an airlock would refuse to
// start for want of BROKER_AIRLOCK_CLUSTER_RANGES. A broker these tests start is a local process,
// outside any cluster, unless its row names KUBERNETES_SERVICE_HOST itself.
func productionEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"BROKER_SECRETS_PREFIX=" + testSecretsPrefix,
		"BROKER_SECRETS_KMS_KEY_ARN=" + testSecretsKMSKeyARN,
		"BROKER_SIGNIN_ISSUER=" + testSignInIssuer,
		"BROKER_SIGNIN_CLIENT_ID=" + ciphertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN=" + ciphertest.SignInDomain,
		"KUBERNETES_SERVICE_HOST=",
	}
}

// TestAnHTTPEdgeURLIsRefusedOutsideALocalRun: a BROKER_EDGE_URL or BROKER_EDGE_MACHINE_LOGIN_URL
// that is not https, however its scheme is written, refuses a start that is not a local run,
// naming that setting and never the rest of the URL's text but what redact.URL keeps; https ones
// start, a broker with no edge is refused nothing, and a local run (BROKER_FAKE_SECRETS_FILE set)
// may use http for either.
//
// The named reverts: drop refuseHTTPEdgeURLsInProduction's scheme check, and the http rows start;
// check BROKER_EDGE_URL alone, as before, and the plain-http machine login page starts.
func TestAnHTTPEdgeURLIsRefusedOutsideALocalRun(t *testing.T) {
	const edgeURL, machineLoginURL = "https://edge.invalid", "https://dispatch.edge.invalid/credentials/machine"
	for _, plain := range []string{"http://edge.invalid", "HTTP://edge.invalid:8080", "ws://edge.invalid", "edge.invalid"} {
		for _, row := range []struct{ setting, edgeURL, machineLoginURL string }{
			{"BROKER_EDGE_URL", plain, machineLoginURL},
			{"BROKER_EDGE_MACHINE_LOGIN_URL", edgeURL, plain},
		} {
			err := refuseHTTPEdgeURLsInProduction(row.edgeURL, row.machineLoginURL, "")
			if err == nil || !strings.HasPrefix(err.Error(), row.setting+" ") || !strings.Contains(err.Error(), "must be https in production") {
				t.Errorf("%s %s with no BROKER_FAKE_SECRETS_FILE = %v, want the https refusal naming %s", row.setting, plain, err, row.setting)
			}
			if err := refuseHTTPEdgeURLsInProduction(row.edgeURL, row.machineLoginURL, "/dev/fake-secrets.json"); err != nil {
				t.Errorf("%s %s in a local run = %v, want nil", row.setting, plain, err)
			}
		}
	}
	for _, row := range [][2]string{{edgeURL, machineLoginURL}, {"HTTPS://edge.invalid:8443", "HTTPS://dispatch.edge.invalid:8443/m"}, {"", ""}} {
		if err := refuseHTTPEdgeURLsInProduction(row[0], row[1], ""); err != nil {
			t.Errorf("BROKER_EDGE_URL %q and BROKER_EDGE_MACHINE_LOGIN_URL %q with no BROKER_FAKE_SECRETS_FILE = %v, want nil", row[0], row[1], err)
		}
	}
}

// portZeroPublicURLs are BROKER_PUBLIC_URLs whose port is 0 as the bind reads a port: each spelling
// of zero, which the production refusal and main's derive-from-bind rewrite both take as port 0.
var portZeroPublicURLs = []string{"http://127.0.0.1:0", "http://127.0.0.1:00", "http://[::1]:000", "http://localhost:0000"}

// TestPortZeroPublicURLRefusedInProduction: a BROKER_PUBLIC_URL whose port is 0, however the zero
// is written, refuses a start that is not a local run.
//
// The named revert: compare the port as the text "0", and the ":00" rows load.
func TestPortZeroPublicURLRefusedInProduction(t *testing.T) {
	for _, publicURL := range portZeroPublicURLs {
		err := refusePortZeroPublicURLInProduction(publicURL, "")
		if err == nil {
			t.Errorf("BROKER_PUBLIC_URL %s with no BROKER_FAKE_SECRETS_FILE: want an error, got nil", publicURL)
			continue
		}
		want := "BROKER_PUBLIC_URL " + strconv.Quote(publicURL) + ": port 0 is never dialable in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use it as dev-broker.sh's derive-from-bind convention"
		if err.Error() != want {
			t.Errorf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestPortZeroPublicURLIsFineForALocalRun(t *testing.T) {
	for _, publicURL := range portZeroPublicURLs {
		if err := refusePortZeroPublicURLInProduction(publicURL, "/dev/fake-secrets.json"); err != nil {
			t.Errorf("BROKER_PUBLIC_URL %s with BROKER_FAKE_SECRETS_FILE set: want nil, got %v", publicURL, err)
		}
	}
}

func TestNonZeroPortPublicURLIsFineInProduction(t *testing.T) {
	for _, publicURL := range []string{"https://broker.invalid", "https://broker.invalid:443", "http://127.0.0.1:010", "http://127.0.0.1:"} {
		if err := refusePortZeroPublicURLInProduction(publicURL, ""); err != nil {
			t.Errorf("BROKER_PUBLIC_URL %s: want nil, got %v", publicURL, err)
		}
	}
}

// TestThePublicURLsPortIsReadAsTheBindReadsIt holds publicURLPortZero, the one reading of the
// port that both the production refusal and main's derive-from-bind rewrite take, to the number
// the bind reads: every spelling of zero is port 0, and a URL naming another port, or none (an
// empty port included), or not parsing, names no port 0.
//
// The named revert: compare the port as the text "0", and the ":00" rows read as no port 0.
func TestThePublicURLsPortIsReadAsTheBindReadsIt(t *testing.T) {
	for _, publicURL := range portZeroPublicURLs {
		if !publicURLPortZero(publicURL) {
			t.Errorf("publicURLPortZero(%q) = false, want true: the bind reads that port as 0", publicURL)
		}
	}
	for _, publicURL := range []string{"https://broker.invalid", "https://broker.invalid:443", "http://127.0.0.1:010", "http://127.0.0.1:", "http://127.0.0.1:%30", "://no-scheme"} {
		if publicURLPortZero(publicURL) {
			t.Errorf("publicURLPortZero(%q) = true, want false", publicURL)
		}
	}
}

// TestTheWaitLimitEndsInsideTheRequestDeadline: a held read of the longest wait the broker takes
// (GET /v1/requests/{id}?wait=) ends before requestDeadline ends its request. A deadline that fell
// to the limit or under it would end every undecided held read as a 503 DATABASE_UNAVAILABLE,
// logged at ERROR as `broker: read request failed` and counted failed. A deadline under the limit
// would also cut such a read before the wait agent-secrets asked had passed, which it answers with
// its backoff, so R1's second would become up to ten; at the limit exactly the 503 comes after that
// wait, and agent-secrets reads again at once, but the ERROR line and the failed count remain.
func TestTheWaitLimitEndsInsideTheRequestDeadline(t *testing.T) {
	if limit := time.Duration(record.MaxRequestWaitSeconds) * time.Second; limit >= requestDeadline {
		t.Fatalf("record.MaxRequestWaitSeconds is %s, want under requestDeadline (%s)", limit, requestDeadline)
	}
}

// airlockTestSettings is the eleven test-only settings of the broker's local stack, the
// airlock's ten and the judge's one, each as a Config the refusal below reads and as the
// environment a real broker is started with. One row per setting: the refusal has to name
// whichever one it found, since an operator reads from it which setting to take out.
func airlockTestSettings() []struct {
	name string
	cfg  func(*config.Config)
	env  string
} {
	return []struct {
		name string
		cfg  func(*config.Config)
		env  string
	}{
		{"BROKER_AIRLOCK_TEST_GITHUB_API_BASE", func(c *config.Config) { c.AirlockTestGitHubAPIBase = "http://127.0.0.1:18080" }, "http://127.0.0.1:18080"},
		{"BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL", func(c *config.Config) { c.AirlockTestGoogleTokenURL = "http://127.0.0.1:18081/token" }, "http://127.0.0.1:18081/token"},
		{"BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL", func(c *config.Config) { c.AirlockTestAnthropicTokenURL = "http://127.0.0.1:18082/v1/oauth/token" }, "http://127.0.0.1:18082/v1/oauth/token"},
		{"BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL", func(c *config.Config) { c.AirlockTestIAMCredentialsURL = "http://127.0.0.1:18083" }, "http://127.0.0.1:18083"},
		{"BROKER_AIRLOCK_TEST_DIAL_ALLOW", func(c *config.Config) { c.AirlockTestDialAllow = []string{"s.airlock.test:443"} }, "s.airlock.test:443"},
		{"BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE", func(c *config.Config) { c.AirlockTestUpstreamCAFile = "/run/airlock/ca.crt" }, "/run/airlock/ca.crt"},
		{"BROKER_AIRLOCK_TEST_RESOLVE", func(c *config.Config) {
			c.AirlockTestResolve = map[string]netip.Addr{"s.airlock.test": netip.MustParseAddr("127.0.0.1")}
		}, "s.airlock.test=127.0.0.1"},
		{"BROKER_AIRLOCK_TEST_KEEP_VALUES", func(c *config.Config) { c.AirlockTestKeepValues = true }, "true"},
		{"BROKER_AIRLOCK_TEST_NO_KIND", func(c *config.Config) { c.AirlockTestNoKind = true }, "true"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", func(c *config.Config) { c.AirlockTestAzureAuthority = "microsoft.airlock.test" }, "microsoft.airlock.test"},
		{"BROKER_JUDGE_TEST_SKIP_CHECKS", func(c *config.Config) { c.JudgeTestSkipChecks = true }, "true"},
	}
}

// TestEachAirlockTestSettingIsRefusedInProduction: every one of the eleven points the broker at
// a stand-in, lets it dial what no production proxy may, or turns a check off, so each on its
// own refuses a start that is not a local run, naming itself.
func TestEachAirlockTestSettingIsRefusedInProduction(t *testing.T) {
	for _, setting := range airlockTestSettings() {
		var cfg config.Config
		setting.cfg(&cfg)
		err := refuseAirlockTestSettingsInProduction(cfg, "")
		if err == nil {
			t.Errorf("%s set with no BROKER_FAKE_SECRETS_FILE: want an error, got nil", setting.name)
			continue
		}
		if !strings.Contains(err.Error(), setting.name) || !strings.Contains(err.Error(), "BROKER_FAKE_SECRETS_FILE is unset") {
			t.Errorf("%s set: error = %q; want it to name the setting and the local-run file", setting.name, err)
		}
	}
}

// TestEveryAirlockTestSettingIsFineForALocalRun: all eleven at once, with the fake secrets file
// that makes a start a local run, are accepted - and a production start naming none of them is
// accepted too, since this refusal must not stand in the way of an ordinary boot.
func TestEveryAirlockTestSettingIsFineForALocalRun(t *testing.T) {
	var cfg config.Config
	for _, setting := range airlockTestSettings() {
		setting.cfg(&cfg)
	}
	if err := refuseAirlockTestSettingsInProduction(cfg, "/dev/fake-secrets.json"); err != nil {
		t.Fatalf("all eleven with BROKER_FAKE_SECRETS_FILE set: want nil, got %v", err)
	}
	if err := refuseAirlockTestSettingsInProduction(config.Config{}, ""); err != nil {
		t.Fatalf("a production start naming none of them: want nil, got %v", err)
	}
}

// TestMainRefusesAnAirlockTestSettingInProduction drives the REAL compiled binary with each of
// the eleven, as the port-0 row above drives it: the helper's rows prove it is correct, and this
// proves the broker runs it at boot, before it migrates the database or binds a socket. The
// database it is given is unreachable, so a broker that read these settings later would fail on
// the host instead of on the setting.
func TestMainRefusesAnAirlockTestSettingInProduction(t *testing.T) {
	binary := buildBroker(t)
	for _, setting := range airlockTestSettings() {
		t.Run(setting.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary,
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				setting.name+"="+setting.env,
			)
			if !strings.Contains(out, setting.name) || !strings.Contains(out, "local stack") {
				t.Fatalf("broker refused, but its output does not name %s as a local-stack setting: %s", setting.name, out)
			}
		})
	}
}

// TestTheRequestMachineCarriesTheServicesByAccount pins the one place the broker hands
// BROKER_SERVICES' accounts to the request decisions: the Machine newRequestMachine builds from a
// config holds the config's ServicesByAccount, keyed by account so one service may have several,
// without which no pod is ever a service's.
func TestTheRequestMachineCarriesTheServicesByAccount(t *testing.T) {
	secretFile := signInClientSecretFile(t)
	cfg, err := config.Load(func(name string) string {
		return map[string]string{
			"BROKER_DATABASE_URL":              "postgres://unused",
			"BROKER_PUBLIC_URL":                "https://secrets.internal.example",
			"BROKER_MACHINE_LOGIN_URL":         "https://dispatch.example/credentials/machine",
			"BROKER_SERVICES":                  "example-service=system:serviceaccount:example:example-sa example-service=system:serviceaccount:example:another-sa",
			"BROKER_SECRETS_PREFIX":            "example/agent-secrets/",
			"BROKER_SECRETS_KMS_KEY_ARN":       "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			"BROKER_SIGNIN_ISSUER":             testSignInIssuer,
			"BROKER_SIGNIN_CLIENT_ID":          ciphertest.SignInClientID,
			"BROKER_SIGNIN_CLIENT_SECRET_FILE": secretFile,
			"BROKER_SIGNIN_DOMAIN":             ciphertest.SignInDomain,
		}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil, "", nil, nil, nil)
	want := map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:example:another-sa": "example-service",
	}
	if !maps.Equal(m.ServicesByAccount, want) {
		t.Fatalf("request Machine's ServicesByAccount = %v, want %v", m.ServicesByAccount, want)
	}
	for _, secretType := range policy.Types {
		if m.Minters[secretType] == nil {
			t.Errorf("request Machine has no minter for type %q, which the policy serves", secretType)
		}
	}
	if len(m.Minters) != len(policy.Types) {
		t.Errorf("request Machine has %d minters for %d types: %v", len(m.Minters), len(policy.Types), m.Minters)
	}
}

// TestTheAnthropicMinterMintsAtTheLocalStacksStandIns: the anthropic-federation minter
// newRequestMachine builds reads the bound account's Google identity token at the IAM that
// BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL names, for Anthropic's audience, and trades it at the
// exchange that BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL names. The local stack's check of the
// anthropic kind reads both stand-ins' records, so a minter that kept Google's and Anthropic's own
// endpoints would send the stack's mint to the internet and the check would read nothing.
func TestTheAnthropicMinterMintsAtTheLocalStacksStandIns(t *testing.T) {
	const (
		account   = "robot-1@acme-deployment.iam.gserviceaccount.com"
		standIn   = "sk-ant-oat01-the-stand-ins-token"
		secret    = "ANTHROPIC_K"
		storedVal = `{"organization_id":"00000000-0000-4000-8000-000000000000","federation_rule_id":"fdrl_01examplerule","service_account_id":"svac_01exampletarget","service_account":"` + account + `"}`
	)
	ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	var exchanged []map[string]any
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		exchanged = append(exchanged, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": standIn, "expires_in": 3600, "token_type": "bearer"})
	}))
	t.Cleanup(exchange.Close)
	secretFile := signInClientSecretFile(t)
	cfg, err := config.Load(func(name string) string {
		return map[string]string{
			"BROKER_DATABASE_URL":                     "postgres://unused",
			"BROKER_PUBLIC_URL":                       "https://secrets.internal.example",
			"BROKER_MACHINE_LOGIN_URL":                "https://dispatch.example/credentials/machine",
			"BROKER_SECRETS_PREFIX":                   "example/agent-secrets/",
			"BROKER_SECRETS_KMS_KEY_ARN":              "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			"BROKER_SIGNIN_ISSUER":                    testSignInIssuer,
			"BROKER_SIGNIN_CLIENT_ID":                 ciphertest.SignInClientID,
			"BROKER_SIGNIN_CLIENT_SECRET_FILE":        secretFile,
			"BROKER_SIGNIN_DOMAIN":                    ciphertest.SignInDomain,
			"BROKER_SERVICE_ACCOUNT_SECRETS":          secret + "=" + account,
			"BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL": exchange.URL + "/v1/oauth/token",
			"BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL": iam.Server.URL,
		}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil, "", nil, nil, nil)
	minted, err := m.Minters[policy.TypeAnthropicFederation](t.Context(), secrets.MintRequest{
		Name: secret, Source: secret, Type: policy.TypeAnthropicFederation, HumanTier: true, Value: storedVal,
	})
	if err != nil || minted.Value != standIn {
		t.Fatalf("the anthropic-federation minter answered a value of %d bytes, %v; want the stand-in exchange's token", len(minted.Value), err)
	}
	asked := iam.Requests()
	if len(asked) != 1 || !strings.Contains(asked[0].Path, "/serviceAccounts/"+account+":generateIdToken") || asked[0].Body["audience"] != secrets.AnthropicAudience {
		t.Fatalf("the stand-in IAM was asked %+v; want one generateIdToken of %s for %s", asked, account, secrets.AnthropicAudience)
	}
	if len(exchanged) != 1 || exchanged[0]["assertion"] != ciphertokentest.IAMIdentityToken || exchanged[0]["service_account_id"] != "svac_01exampletarget" {
		t.Fatalf("the stand-in exchange was sent %d bodies, the first %v; want one whose assertion is the stand-in IAM's identity token", len(exchanged), exchanged)
	}
}

// TestMainRefusesPortZeroPublicURLInProduction drives the REAL compiled binary's main(), not
// refusePortZeroPublicURLInProduction directly: the helper's own tests above prove only that it is
// correct, and with this guard's only call site moved to run after st.Migrate, the first Secrets
// Manager read, and net.Listen, a misconfigured production boot would migrate the production
// database and bind a socket before ever refusing. This builds cmd/cipherd once, execs it with
// BROKER_PUBLIC_URL=http://127.0.0.1:0 and no BROKER_FAKE_SECRETS_FILE (plus an unreachable
// BROKER_DATABASE_URL and just enough other required BROKER_* variables for config.Load to succeed
// — the refusal must run before store.Open, so no real Postgres or AWS credential, and no
// successful bind, is ever needed), and asserts the process exits non-zero naming the port-0
// refusal on stderr and never logs "broker listening": a regression that runs the guard after
// Listen would still refuse eventually, but only after already printing that line and binding a
// real socket.
func TestMainRefusesPortZeroPublicURLInProduction(t *testing.T) {
	cmd := exec.Command(buildBroker(t))
	cmd.Env = append(append(os.Environ(), productionEnv(t)...),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
	)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker BROKER_PUBLIC_URL=http://127.0.0.1:0 in production: want a nonzero exit, got err=%v output=%s", err, out)
	}
	const wantSubstring = `port 0 is never dialable in production`
	if !strings.Contains(string(out), wantSubstring) {
		t.Fatalf("broker refused to boot (exit %d) but its output didn't name the reason: %s\nwant it to contain: %s",
			exitErr.ExitCode(), out, wantSubstring)
	}
	if strings.Contains(string(out), "broker listening") {
		t.Fatalf(`broker refused to boot but still logged "broker listening" first — the guard ran after binding: %s`, out)
	}
}

// TestMainRefusesAnHTTPEdgeURLInProduction: the binary, with an edge whose BROKER_EDGE_URL is
// plain http and no BROKER_FAKE_SECRETS_FILE, exits non-zero naming the https refusal before it
// reaches the database or binds a socket (refuseHTTPEdgeURLsInProduction runs beside the port-0
// refusal, before store.Open), as TestMainRefusesPortZeroPublicURLInProduction holds for that one.
//
// The named revert: drop the call of refuseHTTPEdgeURLsInProduction from main, and the broker goes
// on to dial the unreachable database instead of naming the refusal.
func TestMainRefusesAnHTTPEdgeURLInProduction(t *testing.T) {
	cmd := exec.Command(buildBroker(t))
	cmd.Env = append(append(os.Environ(), productionEnv(t)...),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://broker.invalid",
		"BROKER_MACHINE_LOGIN_URL=https://dispatch.invalid/credentials/machine",
		"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_EDGE_URL=http://edge.invalid",
		"BROKER_EDGE_MACHINE_LOGIN_URL=https://dispatch.edge.invalid/credentials/machine",
	)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker BROKER_EDGE_URL=http://edge.invalid in production: want a nonzero exit, got err=%v output=%s", err, out)
	}
	const wantSubstring = `must be https in production`
	if !strings.Contains(string(out), wantSubstring) {
		t.Fatalf("broker refused to boot (exit %d) but its output didn't name the reason: %s\nwant it to contain: %s",
			exitErr.ExitCode(), out, wantSubstring)
	}
	if strings.Contains(string(out), "broker listening") || strings.Contains(string(out), "broker edge listening") {
		t.Fatalf(`broker refused to boot but still logged that it was listening first: %s`, out)
	}
}

// rdsTestHost is an Amazon RDS cluster endpoint's form. Nothing resolves it, so a broker that
// reached the network for it would fail on the lookup, never on the refusal a test wants.
const rdsTestHost = "example-cluster.cluster-abcdefghijkl.us-west-2.rds.amazonaws.com"

// noAWSEnv strips the AWS SDK's configuration from a broker run: no region, no credentials, no
// shared files and no instance metadata, so a run that reaches for AWS says so instead of using
// the machine's own.
func noAWSEnv(t *testing.T) []string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "aws-config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"AWS_REGION=", "AWS_DEFAULT_REGION=", "AWS_PROFILE=", "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=",
		"AWS_SESSION_TOKEN=", "AWS_CONFIG_FILE=" + empty, "AWS_SHARED_CREDENTIALS_FILE=" + empty,
		"AWS_EC2_METADATA_DISABLED=true", "AWS_CONTAINER_CREDENTIALS_FULL_URI=", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=",
		"AWS_WEB_IDENTITY_TOKEN_FILE=",
	}
}

// buildBroker builds cmd/cipherd into a temporary directory and returns its path.
func buildBroker(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "broker", Env: append(os.Environ(), "GOTOOLCHAIN=go1.27.3")})
}

// runRefusedBroker runs the broker binary with env and returns its output, failing t unless it
// exits non-zero without ever logging "broker listening".
func runRefusedBroker(t *testing.T, binPath string, env ...string) string {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = append(append(append(os.Environ(), productionEnv(t)...), noAWSEnv(t)...), env...)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker: want a nonzero exit, got err=%v output=%s", err, out)
	}
	if strings.Contains(string(out), "broker listening") {
		t.Fatalf(`broker refused to boot but logged "broker listening" first: %s`, out)
	}
	return string(out)
}

// TestMainRefusesAnIAMURLThatDoesNotVerifyItsHost drives the real binary with an IAM-form
// BROKER_DATABASE_URL (a user, no password, an RDS endpoint) that asks only sslmode=require,
// which encrypts and verifies nothing: it refuses at startup naming the host, before it reaches
// for AWS or the database.
func TestMainRefusesAnIAMURLThatDoesNotVerifyItsHost(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://agent_secrets_broker@"+rdsTestHost+":5432/agent_secrets?sslmode=require",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
	)
	for _, want := range []string{"BROKER_DATABASE_URL signs in to " + rdsTestHost + " by RDS IAM token", "its sslmode is not verify-full"} {
		if !strings.Contains(out, want) {
			t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
		}
	}
}

// signsInBy reports whether log holds the line the broker logs at boot, before it reaches for AWS
// or the database, naming how it signs in to its database: method rds-iam or password.
func signsInBy(log, method string) bool {
	return len(recordsWith(log, "database sign-in", map[string]any{"severity": "INFO", "method": method})) != 0
}

// TestMainMintsTokensBeforeOpeningTheDatabase drives the real binary with a verified IAM-form
// URL and no AWS region: it refuses naming the region, which proves the token minter is built
// from the AWS config before the database is opened. A broker that opened the database first
// would fail on the unresolvable host instead. Before the refusal it logs that it signs in by
// RDS IAM token, and never by password.
func TestMainMintsTokensBeforeOpeningTheDatabase(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://agent_secrets_broker@"+rdsTestHost+":5432/agent_secrets?sslmode=verify-full&sslrootcert=../../docker/rds-global-bundle.pem",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
	)
	if !strings.Contains(out, "needs an AWS region") {
		t.Fatalf("broker refused, but not for the missing AWS region the token minter needs: %s", out)
	}
	if !signsInBy(out, "rds-iam") || signsInBy(out, "password") {
		t.Fatalf("broker output has no INFO %q line naming method rds-iam, or names the password method too:\n%s", "database sign-in", out)
	}
}

// TestMainSignsInToALocalPasswordlessURLAsGiven drives the real binary with a passwordless URL to
// a local Postgres, with no AWS configuration at all: it is not an RDS endpoint, so the broker
// mints no token and signs in as the URL says (here with the password libpq's passfile holds), and
// boots. A broker that took it for IAM would refuse for the missing AWS region. Its boot log says
// it signs in by password, the one a passfile supplies from outside the URL, and never by token.
func TestMainSignsInToALocalPasswordlessURLAsGiven(t *testing.T) {
	databaseURL := storetest.URL(t)
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	parsed.User = url.User(parsed.User.Username())
	passfile := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(passfile, []byte("*:*:*:"+parsed.User.Username()+":"+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(noAWSEnv(t),
		"BROKER_DATABASE_URL="+parsed.String(),
		"PGPASSFILE="+passfile,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
	)})
	addr, boot := broker.Addr, broker.Boot
	requireHealthy(t, addr)
	if !signsInBy(boot, "password") || signsInBy(boot, "rds-iam") {
		t.Fatalf("boot log has no INFO %q line naming method password, or names the rds-iam method too:\n%s", "database sign-in", boot)
	}
}

// TestTheBrokerLogsTheSessionAccountPairsItRead drives the real binary twice. Started with
// BROKER_SESSION_ACCOUNT_PAIRS, it logs one "session account pairs" line at boot, at INFO and on,
// whose pairs are each pair as the setting wrote it, in the setting's order, joined by one space:
// the pairs config.Load read and not the raw value, which here carries the whitespace an
// operator's manifest may. The first pair's accounts and the two pairs are each written out of
// sorted order, so a line that sorted either does not match. Started without the setting, the
// line says off, names no pairs, and names the setting in what being off means.
func TestTheBrokerLogsTheSessionAccountPairsItRead(t *testing.T) {
	const (
		workerPair   = "system:serviceaccount:example-ns:example-worker=system:serviceaccount:example-next:example-worker"
		reviewerPair = "system:serviceaccount:example-ns:example-reviewer=system:serviceaccount:example-next:example-reviewer"
	)
	binary := buildBroker(t)
	start := func(t *testing.T, env ...string) *brokerProcess {
		t.Helper()
		return startBroker(t, brokerOptions{Binary: binary, Env: append([]string{
			"BROKER_DATABASE_URL=" + storetest.URL(t),
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		}, env...)})
	}
	// line is the one "session account pairs" line broker logs, failing t unless it logged
	// exactly one.
	line := func(t *testing.T, broker *brokerProcess) map[string]any {
		t.Helper()
		broker.AwaitRecord(t, "session account pairs", nil)
		lines := recordsWith(broker.Log(), "session account pairs", nil)
		if len(lines) != 1 {
			t.Fatalf("the broker logged %d %q lines, want one:\n%s", len(lines), "session account pairs", broker.Log())
		}
		return lines[0]
	}

	t.Run("with the setting", func(t *testing.T) {
		broker := start(t, "BROKER_SESSION_ACCOUNT_PAIRS=  "+workerPair+"\n\t"+reviewerPair+" ")
		want := map[string]any{"severity": "INFO", "state": "on", "pairs": workerPair + " " + reviewerPair}
		if got := line(t, broker); !carries(got, want) {
			t.Fatalf("the broker logged %v, want %v: each pair as written, in the order given", got, want)
		}
	})

	t.Run("without the setting", func(t *testing.T) {
		got := line(t, start(t))
		detail, _ := got["detail"].(string)
		_, pairs := got["pairs"]
		if !carries(got, map[string]any{"severity": "INFO", "state": "off"}) || pairs || !strings.Contains(detail, "BROKER_SESSION_ACCOUNT_PAIRS") {
			t.Fatalf("the broker logged %v, want state off at INFO with no pairs and a detail naming BROKER_SESSION_ACCOUNT_PAIRS", got)
		}
	})
}

// TestTheBrokerRefusesABadSessionAccountPair drives the real binary with a BROKER_SESSION_ACCOUNT_PAIRS
// whose second entry pairs an account with itself: it exits non-zero without binding, and its one
// fatal line names the setting and that entry, not the good pair before it, so an operator knows
// which entry to fix. The database it is given is unreachable, so a broker that read the setting
// after opening the store would fail on the host instead of on the entry.
func TestTheBrokerRefusesABadSessionAccountPair(t *testing.T) {
	const (
		good = "system:serviceaccount:example-ns:example-worker=system:serviceaccount:example-next:example-worker"
		bad  = "system:serviceaccount:example-ns:example-reviewer=system:serviceaccount:example-ns:example-reviewer"
	)
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_SESSION_ACCOUNT_PAIRS="+good+" "+bad,
	)
	refusals := recordsWith(out, "broker: fatal", nil)
	if len(refusals) != 1 {
		t.Fatalf("the broker logged %d %q lines, want one:\n%s", len(refusals), "broker: fatal", out)
	}
	refusal, _ := refusals[0]["error"].(string)
	if !strings.Contains(refusal, "BROKER_SESSION_ACCOUNT_PAIRS") || !strings.Contains(refusal, strconv.Quote(bad)) || strings.Contains(refusal, strconv.Quote(good)) {
		t.Fatalf("the broker refused with %q, want a refusal naming BROKER_SESSION_ACCOUNT_PAIRS and the entry %q, and not the good pair %q", refusal, bad, good)
	}
}

// stopGrace is how long a broker gets to exit after the SIGTERM its test's cleanup sends. It is
// the whole stop budget the process logs at boot — its drain plus its shutdown — so a broker
// that honours that budget is never killed mid-drain. It is a span the cleanup holds open, not a
// wait: a broker still running at the end of it is killed by cmdtest's own cleanup, and the row
// that was about its stop is the one that says so (AwaitExit).
const stopGrace = 90 * time.Second

// brokerOptions varies how startBroker starts a broker.
type brokerOptions struct {
	// Env is what the broker is started with on top of the test process's own environment and
	// productionEnv: its addresses, its database, and whatever else the row configures.
	Env []string
	// Binary is the broker to start, buildBroker's when empty, which every test of this process
	// shares (cmdtest.BuildCommand builds each command once per test process).
	Binary string
	// NoWaitForListening returns from startBroker as soon as the child is running, before it has
	// logged "broker listening" — before it has migrated, even. A row about two processes racing
	// to migrate one database needs both of them started before either has finished, which a
	// start that waits for that line cannot give it; such a caller calls AwaitListening itself.
	NoWaitForListening bool
}

// brokerProcess is a broker binary a test started and is driving: cmdtest's process — its whole
// log, its SIGTERM and the code it exited with, as cmd/dispatch's and cmd/listener's own
// shutdown rows drive theirs — plus what is the broker's alone, the addresses it logged that it
// bound.
type brokerProcess struct {
	*cmdtest.Process
	// Addr is the address its "broker listening" line named, and Boot its log up to that line.
	// Both stay empty until AwaitListening has answered, which startBroker does itself unless
	// NoWaitForListening was asked for.
	Addr string
	Boot string
	// metricsAddr is what MetricsAddr read, once it has.
	metricsAddr string
}

// metricsListenEnv is the metrics port every broker startBroker starts binds: one the kernel
// picks, since a package's rows run many brokers at once and the default 127.0.0.1:13382 is a
// port only one of them could hold. It comes before options.Env, so a row may name another.
const metricsListenEnv = "BROKER_METRICS_LISTEN_ADDR=127.0.0.1:0"

// startBroker starts the broker binary with productionEnv, metricsListenEnv and options.Env,
// waits for its "broker listening" line unless options.NoWaitForListening says not to, and stops
// it with SIGTERM when t ends, killing it if it has not gone within stopGrace.
func startBroker(t *testing.T, options brokerOptions) *brokerProcess {
	t.Helper()
	binary := options.Binary
	if binary == "" {
		binary = buildBroker(t)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(append(append(os.Environ(), productionEnv(t)...), metricsListenEnv), options.Env...)
	p := &brokerProcess{Process: cmdtest.Start(t, "broker", cmd)}
	// Cleanups run last-registered-first, so this one — registered after cmdtest.Start's own —
	// asks the broker to stop and waits its whole stop budget out before cmdtest's cleanup kills
	// the child. The signal's own error is dropped because a process that has already exited is
	// nothing for a teardown to fail on. The wait is a plain select rather than the shared wait
	// because a broker that will not stop is this teardown's to kill, never this row's to fail:
	// a row about the stop itself asks AwaitExit for it.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.Exited:
		case <-time.After(stopGrace):
		}
	})
	if !options.NoWaitForListening {
		p.AwaitListening(t)
	}
	return p
}

// Log is everything the broker has logged so far, safe to read while it is still running. Its
// output goes to a buffer os/exec's own copier drains to the end, which is what lets Log answer
// mid-run and what keeps a chatty child running at all: a child whose pipe nobody reads blocks in
// its own write once the pipe's buffer fills.
func (p *brokerProcess) Log() string {
	return p.Output.String()
}

// Await answers everything the broker has logged once a whole line carrying want has arrived, one
// a newline closes, so a test that goes on to read that line's fields reads all of them. The
// binary logs the rest of its boot — the state of its token exchange — after the bind line
// startBroker synchronises on, so a test that reads one of those later lines asks Await for it
// rather than reading Boot.
func (p *brokerProcess) Await(t *testing.T, want string) string {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("the broker to log a whole line carrying %q", want), func() (string, bool) {
		text := p.Log()
		text = text[:strings.LastIndexByte(text, '\n')+1]
		return text, strings.Contains(text, want)
	})
}

// AwaitRecord answers the first line the broker has logged whose message is message and that
// carries every field of fields (recordsWith), once one has arrived. It is Await for a line whose
// fields a test reads. What it reads is the whole log, so a wait that gives up prints that.
func (p *brokerProcess) AwaitRecord(t *testing.T, message string, fields map[string]any) map[string]any {
	t.Helper()
	var found []map[string]any
	testwait.Until(t, fmt.Sprintf("the broker to log a %q line carrying %v", message, fields), func() (string, bool) {
		log := p.Log()
		found = recordsWith(log, message, fields)
		return log, len(found) != 0
	})
	return found[0]
}

// MetricsAddr answers the address the broker's "broker metrics listening" line names, waiting for
// that line as AwaitRecord does the first time it is asked and answering what it read after that.
// It is how a test finds a broker's own metrics port when every broker it starts binds one the
// kernel picks (metricsListenEnv).
func (p *brokerProcess) MetricsAddr(t *testing.T) string {
	t.Helper()
	if p.metricsAddr == "" {
		addr, _ := p.AwaitRecord(t, "broker metrics listening", nil)["addr"].(string)
		if addr == "" {
			t.Fatalf("the broker's %q line names no addr:\n%s", "broker metrics listening", p.Log())
		}
		p.metricsAddr = addr
	}
	return p.metricsAddr
}

// scrapeMetric GETs /metrics at addr, a broker's metrics port, and answers each sample of metric
// by the label text between its braces - `read="list"` for the sample
// broker_policy_store_reads_total{read="list"}, "" for a sample with no labels - as the
// Prometheus text format writes it. It fails t unless the port answers 200.
func scrapeMetric(t *testing.T, addr, metric string) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET http://%s/metrics: %v", addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET http://%s/metrics = %d, %v; want 200:\n%s", addr, resp.StatusCode, err, body)
	}
	samples := map[string]float64{}
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSuffix(line, "\n")
		// The value is the line's last field: a label value may hold a space, never a line's end.
		cut := strings.LastIndexByte(line, ' ')
		if strings.HasPrefix(line, "#") || cut < 0 {
			continue
		}
		series, labels := line[:cut], ""
		if series != metric {
			inner, ok := strings.CutPrefix(series, metric+"{")
			if !ok || !strings.HasSuffix(inner, "}") {
				continue
			}
			labels = strings.TrimSuffix(inner, "}")
		}
		value, err := strconv.ParseFloat(line[cut+1:], 64)
		if err != nil {
			t.Fatalf("the scrape at %s holds a %s sample whose value is no number: %q", addr, metric, line)
		}
		samples[labels] = value
	}
	return samples
}

// AwaitListening waits for the broker's "broker listening" line and records the address it names
// in Addr and the log up to it in Boot. It fails t when the broker exits without ever logging
// that line.
func (p *brokerProcess) AwaitListening(t *testing.T) {
	t.Helper()
	testwait.Until(t, "the broker's listening line", func() (string, bool) {
		if addr, boot, ok := listeningIn(p.Log()); ok {
			p.Addr, p.Boot = addr, boot
			return boot, true
		}
		select {
		case <-p.Exited:
			// Read the log once more rather than judging an exited broker by the read taken
			// before the exit: everything it wrote is in the buffer by the time Exited closes, so
			// a broker that logged its address and exited straight after is read here, where it
			// did bind, instead of being failed for the exit.
			if addr, boot, ok := listeningIn(p.Log()); ok {
				p.Addr, p.Boot = addr, boot
				return boot, true
			}
			t.Fatalf("the broker exited without ever logging %q:\n%s", "broker listening", p.Log())
		default:
		}
		return p.Log(), false
	})
}

// listeningIn finds the broker's first "broker listening" line in log and answers the address it
// names and the log up to the end of that line, which is the boot log: everything the broker
// logged before it was serving.
func listeningIn(log string) (addr, boot string, ok bool) {
	end := 0
	for line := range strings.Lines(log) {
		end += len(line)
		if record, isRecord := logRecord(line); isRecord && record["message"] == "broker listening" {
			if addr, _ := record["addr"].(string); addr != "" {
				return addr, log[:end], true
			}
		}
	}
	return "", "", false
}

// logRecord reads one line of a broker's log as the JSON object logging.NewCloudHandler writes:
// time, severity and message under those names, and each attribute a field of its own. A line
// that is not one answers false, and so does a line still being copied out of the child, which
// is no JSON object until it is whole: half a line never hands back half a value, an address of
// 127.0.0.1:4 for a broker bound to port 41337 or a duration of 2.2s for one sized at 2.25s.
func logRecord(line string) (map[string]any, bool) {
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil || record == nil {
		return nil, false
	}
	return record, true
}

// recordsWith is every line of log that logRecord reads whose message is message and that carries
// every field of fields, in the order the broker wrote them; nil fields match every line of that
// message. It is how a test in this package reads a broker binary's log.
func recordsWith(log, message string, fields map[string]any) []map[string]any {
	var found []map[string]any
	for line := range strings.Lines(log) {
		if record, ok := logRecord(line); ok && record["message"] == message && carries(record, fields) {
			found = append(found, record)
		}
	}
	return found
}

// carries reports whether record holds every field of fields, each compared as the JSON value the
// line holds it as: an int a test names is the number JSON reads as a float64.
func carries(record, fields map[string]any) bool {
	for key, want := range fields {
		if number, isInt := want.(int); isInt {
			want = float64(number)
		}
		if got, ok := record[key]; !ok || !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

// rereadRateLine is what one of a broker's "broker reread rate" lines says: the replica count
// the process counted at a beat of its serving clock, and the shared reread bucket it derived
// from that count (api.RereadRate.SetReplicas). A process logs one at its first beat and again
// whenever the count it sees changes, so the LAST such line is the sizing it serves at now.
type rereadRateLine struct {
	Replicas int
	Every    time.Duration
	Burst    int
}

// rereadRateIn finds the last "broker reread rate" line in log and answers what it says. The
// message is matched whole because the warning a count above api.MaxSupportedReplicas logs opens
// with the same words and carries replicas too, and what tells the two apart is that the
// warning's message goes on ("above the replica count this sizing supports") where this one ends.
// every is the duration string the broker's handler writes a time.Duration as (2.25s), which
// time.ParseDuration reads back.
func rereadRateIn(log string) (rate rereadRateLine, ok bool) {
	for _, record := range recordsWith(log, "broker reread rate", nil) {
		replicas, replicasOK := record["replicas"].(float64)
		text, _ := record["every"].(string)
		every, everyErr := time.ParseDuration(text)
		burst, burstOK := record["burst"].(float64)
		if !replicasOK || everyErr != nil || !burstOK {
			continue
		}
		rate, ok = rereadRateLine{Replicas: int(replicas), Every: every, Burst: int(burst)}, true
	}
	return rate, ok
}

// AwaitExit waits for the broker to exit and answers what its exit was: nil for a clean one, an
// *exec.ExitError naming the code otherwise, which is what Wait itself answers.
func (p *brokerProcess) AwaitExit(t *testing.T) error {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the broker at %s to exit after it was asked to stop", p.Addr), func() (string, bool) {
		select {
		case <-p.Exited:
			return "", true
		default:
			return p.Log(), false
		}
	})
	if p.Cmd.ProcessState.Success() {
		return nil
	}
	return &exec.ExitError{ProcessState: p.Cmd.ProcessState}
}

// requireHealthy fails t unless GET /healthz at addr answers 200.
func requireHealthy(t *testing.T, addr string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET http://%s/healthz: %v", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET http://%s/healthz: status %d, want 200", addr, resp.StatusCode)
	}
}

// refusedSecretEnv is what TestMainLogsRealBoundAddress and TestTheBinaryLogsOneJSONObjectPerLine
// start a broker with: a database of its own, a port the kernel picks, and a fake Secrets Manager
// holding one secret it serves, one whose owner tag names nobody, which it refuses
// owner-tag-malformed at boot, and one owned by example-service, which BROKER_SERVICES registers,
// on the database at databaseURL.
func refusedSecretEnv(t *testing.T, databaseURL string) []string {
	t.Helper()
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	const key = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	const fakeSecrets = `{"secrets": [
		{"name": "example/agent-secrets/demo-key", "kms_key_id": "` + key + `", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo"},
		{"name": "example/agent-secrets/untagged-key", "kms_key_id": "` + key + `", "tags": {"owner": "sjawhar", "tier": "agent"}, "value": "nope"},
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + key + `", "tags": {"owner": "example-service", "tier": "agent"}, "value": "service"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_SERVICES=example-service=system:serviceaccount:example:example-sa",
	}
}

// TestMainLogsRealBoundAddress drives the REAL compiled binary with BROKER_LISTEN_ADDR=127.0.0.1:0
// (dev-broker.sh's own setting after the bind fix) and BROKER_PUBLIC_URL=http://127.0.0.1:0
// (dev-broker.sh's "derive my public URL from whatever I actually bind" convention, see
// cmd/cipherd/main.go's own comment beside its url.Parse check). It asserts the "broker listening"
// log line names a real, nonzero port on 127.0.0.1 — never the configured placeholder — and then
// proves that exact logged address is the one actually serving, by getting /healthz there and
// requiring 200. Before this fix, a losing instance in a port collision could print "ready" while
// a completely different, already-running instance answered its healthz check; binding
// synchronously before logging anything means the address this test reads can only ever name this
// process's own listener.
// It also holds a fake Secrets Manager with one secret whose owner tag names nobody, and asserts
// the real binary refuses it at boot with one RefusedMessage line, at ERROR with its name, its
// reason, an empty owner (the tag is the fault) and first_load, and no second one for the boot's
// catch-up load, which finds it refused as the first load did, while still booting to serve the
// rest; and one owned by example-service, which BROKER_SERVICES registers, and asserts the binary
// serves it: the policy loader main builds carries the configured services.
func TestMainLogsRealBoundAddress(t *testing.T) {
	databaseURL := storetest.URL(t)
	broker := startBroker(t, brokerOptions{Env: refusedSecretEnv(t, databaseURL)})
	addr, boot := broker.Addr, broker.Boot
	refused := map[string]any{"severity": "ERROR", "name": "example/agent-secrets/untagged-key", "reason": "owner-tag-malformed", "owner": "", "first_load": true}
	if lines := recordsWith(boot, policy.RefusedMessage, map[string]any{"name": "example/agent-secrets/untagged-key"}); len(lines) != 1 || !carries(lines[0], refused) {
		t.Fatalf("boot log holds %v as its %q lines for untagged-key; want one, carrying %v:\n%s", lines, policy.RefusedMessage, refused, boot)
	}
	if served := recordsWith(boot, policy.RefusedMessage, map[string]any{"name": "example/agent-secrets/service-key"}); len(served) != 0 {
		t.Fatalf("boot log refuses the secret example-service owns, which BROKER_SERVICES registers: %v", served)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("logged address %q did not parse as host:port: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("logged address %q: want host 127.0.0.1, got %q", addr, host)
	}
	if port == "" || port == "0" {
		t.Fatalf("logged address %q: want a real, nonzero port, got %q", addr, port)
	}

	requireHealthy(t, addr)

	if status, _, answer := broker.sessionReread(t, databaseURL, "SERVICE_KEY"); status != http.StatusOK || answer["served"] != true {
		t.Fatalf("a session's reread of SERVICE_KEY = status %d, %v; want served, its owner a service BROKER_SERVICES registers", status, answer)
	}
}

// TestTheBinaryLogsOneJSONObjectPerLine drives the REAL compiled binary on
// TestMainLogsRealBoundAddress's fixture and reads three lines of its log as Cloud Logging reads
// them: the boot's "broker listening" line at INFO, its refusal of the secret whose owner tag
// names nobody at ERROR, and the record of a refused answer (answer.RefusalMessage) at WARNING,
// for a reread of a name no secret can carry sent to the address the listening line names. Each
// is one JSON object, with time (RFC 3339, in UTC), severity and message under those names and
// each of its attributes a field of its own, which is what makes an entry's severity its level
// rather than the stream's. The lines are found by their text, not through startBroker's read of
// the address, so a broker writing any other shape fails here, naming the line, rather than in a
// wait for an address it never reads.
func TestTheBinaryLogsOneJSONObjectPerLine(t *testing.T) {
	broker := startBroker(t, brokerOptions{NoWaitForListening: true, Env: refusedSecretEnv(t, storetest.URL(t))})
	// line reads the first line of log carrying message as a record, failing t unless it is one
	// JSON object with time, message and severity as wanted and fields among its fields.
	line := func(log, message, severity string, fields map[string]any) map[string]any {
		t.Helper()
		var text string
		for candidate := range strings.Lines(log) {
			if strings.Contains(candidate, message) {
				text = candidate
				break
			}
		}
		if text == "" {
			t.Fatalf("the broker logged no line carrying %q:\n%s", message, log)
		}
		record, ok := logRecord(text)
		if !ok {
			t.Fatalf("the broker's %q line is not one JSON object:\n%s", message, text)
		}
		stamp, _ := record["time"].(string)
		if at, err := time.Parse(time.RFC3339Nano, stamp); err != nil || at.Location() != time.UTC {
			t.Errorf("the %q line's time is %q (%v); want RFC 3339 in UTC: %s", message, stamp, err, text)
		}
		if record["message"] != message || record["severity"] != severity || !carries(record, fields) {
			t.Errorf("the %q line reads message %v, severity %v; want message %q, severity %s and the fields %v: %s",
				message, record["message"], record["severity"], message, severity, fields, text)
		}
		return record
	}

	log := broker.Await(t, "broker listening")
	addr, _ := line(log, "broker listening", "INFO", nil)["addr"].(string)
	if addr == "" {
		t.Fatalf("the %q line carries no addr field:\n%s", "broker listening", log)
	}
	line(log, policy.RefusedMessage, "ERROR", map[string]any{"name": "example/agent-secrets/untagged-key", "reason": "owner-tag-malformed", "owner": "", "first_load": true})

	status, header, _ := post(t, "http://"+addr+"/v1/secrets/bad%20name/reread")
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/secrets/bad%%20name/reread = %d; want 400", status)
	}
	line(broker.Await(t, answer.RefusalMessage), answer.RefusalMessage, "WARNING", map[string]any{
		"answer_id": header.Get(answer.Header), "code": "SECRET_NAME_INVALID", "secrets": []any{"bad name"}, "caller_class": answer.CallerPublic,
	})
}

// TestTheBrokerServesItsMetricsOnAPortOfItsOwn drives the REAL compiled binary and reads its
// metrics where a deployment scrapes them. GET /metrics on the address its "broker metrics
// listening" line names - a port of its own on 127.0.0.1, which the kernel picked
// (metricsListenEnv), and never the API's - answers the Prometheus text format, and its
// broker_policy_store_reads_total holds the boot's whole-namespace loads; one reread through the
// API is one more read="describe" there and no list. broker_unservable_secrets holds one series,
// the secret whose owner tag names nobody, labelled by its store id, its reason and the empty
// owner, and none for the secrets the broker serves. That port answers GET /livez too, 200 for a
// broker that runs no NATS auth service, and nothing else; the API's address answers GET /metrics
// and GET /livez 404, so no caller of the API reaches either.
func TestTheBrokerServesItsMetricsOnAPortOfItsOwn(t *testing.T) {
	databaseURL := storetest.URL(t)
	broker := startBroker(t, brokerOptions{Env: refusedSecretEnv(t, databaseURL)})
	metricsAddr := broker.MetricsAddr(t)
	host, port, err := net.SplitHostPort(metricsAddr)
	if err != nil || host != "127.0.0.1" || port == "0" || metricsAddr == broker.Addr {
		t.Fatalf("the broker logged its metrics port as %q (%v); want a port of its own on 127.0.0.1, apart from the API's %s", metricsAddr, err, broker.Addr)
	}

	reads := scrapeMetric(t, metricsAddr, "broker_policy_store_reads_total")
	// The first load, and the catch-up once the reread listener's LISTEN is in place.
	if reads[`read="list"`] < 1 {
		t.Fatalf("after its boot the broker counts %v store reads; want its boot's whole-namespace loads as read=\"list\"", reads)
	}
	if status, _, _ := broker.sessionReread(t, databaseURL, "SERVICE_KEY"); status != http.StatusOK {
		t.Fatalf("a session's POST /v1/secrets/SERVICE_KEY/reread = %d; want 200", status)
	}
	after := scrapeMetric(t, metricsAddr, "broker_policy_store_reads_total")
	if after[`read="describe"`] != reads[`read="describe"`]+1 || after[`read="list"`] != reads[`read="list"`] {
		t.Fatalf("one reread moved the store reads from %v to %v; want one more read=\"describe\" and no more read=\"list\"", reads, after)
	}
	unservable := scrapeMetric(t, metricsAddr, "broker_unservable_secrets")
	if want := map[string]float64{`secret="example/agent-secrets/untagged-key",reason="owner-tag-malformed",owner=""`: 1}; !maps.Equal(unservable, want) {
		t.Fatalf("broker_unservable_secrets reads %v; want the one refused secret alone, %v", unservable, want)
	}

	for _, row := range []struct {
		name, method, url string
		status            int
	}{
		{"the scrape", http.MethodGet, "http://" + metricsAddr + "/metrics", http.StatusOK},
		{"the liveness of a broker with no auth service", http.MethodGet, "http://" + metricsAddr + "/livez", http.StatusOK},
		{"another path on the metrics port", http.MethodGet, "http://" + metricsAddr + "/healthz", http.StatusNotFound},
		{"another method on the metrics port", http.MethodPost, "http://" + metricsAddr + "/metrics", http.StatusMethodNotAllowed},
		{"the scrape on the API's port", http.MethodGet, "http://" + broker.Addr + "/metrics", http.StatusNotFound},
		{"the liveness on the API's port", http.MethodGet, "http://" + broker.Addr + "/livez", http.StatusNotFound},
	} {
		req, err := http.NewRequest(row.method, row.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %s %s: %v", row.name, row.method, row.url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != row.status {
			t.Errorf("%s: %s %s = %d; want %d", row.name, row.method, row.url, resp.StatusCode, row.status)
		}
		if row.status == http.StatusOK && strings.HasSuffix(row.url, "/metrics") && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
			t.Errorf("%s: Content-Type %q; want the Prometheus text format's text/plain; version=0.0.4", row.name, resp.Header.Get("Content-Type"))
		}
	}
}

// TestFakeSecretsFileServesUnderTheGCPStore pins that the local stand-in serves its secrets with the
// GCP store selected: the file's kms_key_id names the agent-secrets key as that store configures
// it (BROKER_SECRETS_KMS_KEY_NAME), and its names sit under the GCP prefix.
func TestFakeSecretsFileServesUnderTheGCPStore(t *testing.T) {
	const (
		prefix = "projects/000000000000/secrets/agent-secrets-"
		key    = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": [
		{"name": "`+prefix+`demo-key", "kms_key_id": "`+key+`", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	databaseURL := storetest.URL(t)
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_SECRETS_STORE=gcp-secret-manager",
		"BROKER_SECRETS_PREFIX=" + prefix,
		"BROKER_SECRETS_KMS_KEY_ARN=",
		"BROKER_SECRETS_KMS_KEY_NAME=" + key,
	}})
	boot := broker.Boot
	if strings.Contains(boot, "agent secret policy refused") {
		t.Fatalf("boot log refuses a fake secret on the configured key:\n%s", boot)
	}
	if status, _, answer := broker.sessionReread(t, databaseURL, "DEMO_KEY"); status != http.StatusOK || answer["served"] != true {
		t.Fatalf("a session's reread of DEMO_KEY = status %d, %v; want served under the GCP store's key", status, answer)
	}
}

// profilesFile writes profiles into a file of t's own and answers its path, the file
// BROKER_PROFILES_FILE names.
func profilesFile(t *testing.T, profiles string) string {
	t.Helper()
	return writeTempFile(t, "profiles.json", []byte(profiles))
}

// airlockCAFiles writes a self-signed certificate authority and its key into files of t's own
// and answers the two paths, the files BROKER_AIRLOCK_CA_CERT_FILE and
// BROKER_AIRLOCK_CA_KEY_FILE name. Self-signed, since the boot reads the certificate as a
// certificate and trusts nothing about who signed it; the key is beside it because the proxy
// port signs each bound host's certificate with it and refuses to start without it.
func airlockCAFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "broker airlock test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "airlock-ca.crt")
	keyFile = filepath.Join(dir, "airlock-ca.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// writeTempFile writes content into a file named name under a directory of t's own.
func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// airlockEnv is what a broker with an airlock is started with besides its profiles file: the proxy
// the release names, the certificate authority it hands over and the key behind it, without which
// the boot refuses a broker whose profiles file declares a kind or a dpop block it could not proxy
// (config.Config.RequireAirlock), and an address of its own for the proxy port, since a fixed one
// would be a port two rows of this package could not both hold.
func airlockEnv(t *testing.T) []string {
	t.Helper()
	certFile, keyFile := airlockCAFiles(t)
	return []string{
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
	}
}

// TestMainRefusesAProfilesFileItCannotServe drives the real binary with each kind of profiles
// file it must refuse — one holding a proxy block of a kind other than dpop, the form the kinds
// replaced, one whose dpop block still sets a judge, one declaring a kind that names no host, one
// whose profile has a name no session can ask for, one naming a requester service the deployment
// does not register (CheckRequesters, the boot's own read), one for each host spelling of
// ruleHostRows (the rows TestProfilesCheckRefusesAHostTheProxyNeverCompares drives through
// `profiles check`, which read that the host the grammar reads is the host the file writes),
// today's airlock rules file, one that is not JSON, and a path that is not there — and asserts it
// exits non-zero naming the setting and what is wrong with the file, without ever binding. The
// database it is given is unreachable, so a broker that read the file after opening the store
// would fail on the host instead of on the file.
func TestMainRefusesAProfilesFileItCannotServe(t *testing.T) {
	binary := buildBroker(t)
	missing := filepath.Join(t.TempDir(), "absent.json")
	refused := func(t *testing.T, path string, want, absent []string) {
		t.Helper()
		out := runRefusedBroker(t, binary, append(airlockEnv(t),
			"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
			"BROKER_PUBLIC_URL=https://secrets.internal.example",
			"BROKER_PROFILES_FILE="+path,
		)...)
		if !strings.Contains(out, "BROKER_PROFILES_FILE") {
			t.Fatalf("broker refused, but its output does not name BROKER_PROFILES_FILE: %s", out)
		}
		// The refusal names the file, whose path is a temporary directory: it holds the test's or
		// the subtest's name and whatever the temporary directory's own path holds, so every want
		// but the path itself, and the text that must be absent, is looked for with the path
		// taken out.
		text := strings.ReplaceAll(out, path, "")
		for _, want := range want {
			in := text
			if want == path {
				in = out
			}
			if !strings.Contains(in, want) {
				t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
			}
		}
		for _, absent := range absent {
			if absent != "" && strings.Contains(text, absent) {
				t.Fatalf("broker refused, but its output repeats %q as the file writes it: %s", absent, out)
			}
		}
	}
	for name, c := range map[string]struct {
		path string
		want []string
	}{
		// The broker logs a refusal as a JSON string in its error field, so a quotation mark
		// inside it comes out escaped; these rows look for the parts around it, or write it
		// escaped.
		//
		// A proxy block of a kind other than dpop is the form the kinds replaced, so it is
		// refused naming them and the source to tag, rather than served with its rules missing.
		"a proxy block of a kind the kinds replaced": {
			path: profilesFile(t, `{"profiles": [{"name": "DEEL_API_KEY", "proxy": {"hosts": ["api.deel.com"], "kind": "gcp"}}]}`),
			want: []string{"profiles[0]", "DEEL_API_KEY", "proxy is a proxy block, and a proxy block is no longer read", "declare the kind under kinds", "tag the source, DEEL_API_KEY, proxy="},
		},
		// Who decides a request is the key's own judge tag, and a dpop block is made from no
		// stored key, its grant decided by its kind, so a dpop block that still sets a judge is
		// refused, naming the field, rather than served with a judge that decides nothing.
		"a dpop block that sets a judge": {
			path: profilesFile(t, `{"profiles": [{"name": "RESULTS_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"],
				"allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop", "judge": 1800}}]}`),
			want: []string{"profiles[0]", "RESULTS_TOKEN", "proxy.judge", "a profile of kind dpop has no judge"},
		},
		// A kind binds its credential to the hosts it names, so one that names none is refused,
		// where it would hold back the value of every secret of the kind and carry it nowhere.
		"a kind with no host": {
			path: profilesFile(t, `{"kinds": [{"name": "deel", "headers": ["x-api-key"], "ways": ["swap"]}], "profiles": []}`),
			want: []string{`kinds[0] \"deel\": names no host`},
		},
		// A profile whose name is not of the policy's form matches no request, so the file
		// would serve nothing of the secret a reviewer read it as covering.
		"a profile name in lower case": {
			path: profilesFile(t, `{"profiles": [{"name": "deel_api_key"}]}`),
			want: []string{"profiles[0]", "deel_api_key", "not an agent secret name", "no caller can ask for it"},
		},
		"a profile name written with hyphens": {
			path: profilesFile(t, `{"profiles": [{"name": "DEEL-API-KEY"}]}`),
			want: []string{"profiles[0]", "DEEL-API-KEY", "not an agent secret name", "no caller can ask for it"},
		},
		// `profiles check` holds a requester to its shape alone; the boot holds the services
		// setting, and refuses a service it does not register.
		"a requester service the deployment does not register": {
			path: profilesFile(t, `{"profiles": [{"name": "DEEL_API_KEY", "requesters": ["unregistered-service"]}]}`),
			want: []string{"profiles[0]", "DEEL_API_KEY", "requesters", "unregistered-service", "no registered service"},
		},
		// A deployment that points the new setting at today's rules file is refused at its first
		// field, rather than served with no profile.
		"today's airlock rules file": {
			path: profilesFile(t, `{"secrets": [{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"]}]}`),
			want: []string{"unknown field", "secrets"},
		},
		"a file that is not JSON": {
			path: profilesFile(t, "these are not the profiles you are looking for\n"),
			want: []string{"not JSON", "byte offset 2, line 1, column 2"},
		},
		"a path that cannot be read": {
			path: missing,
			want: []string{missing, "no such file"},
		},
	} {
		t.Run(name, func(t *testing.T) { refused(t, c.path, c.want, nil) })
	}
	// The broker logs a refusal as a JSON string in its error field, so a quotation mark inside
	// it comes out escaped: the kind's name is written escaped here, and no row's own want holds
	// one. A host is refused as the kind's that binds it (profilesWithHosts).
	for _, c := range ruleHostRows {
		t.Run("a host: "+c.name, func(t *testing.T) {
			path := profilesFile(t, profilesWithHosts(t, "DEEL_API_KEY", "api.deel.com", c.host))
			refused(t, path, append([]string{`kinds[0] \"` + hostsKind + `\": hosts[1]`}, c.want...), []string{c.absent})
		})
	}
	// The binary's own log (setUpLogging, the first thing main does): a host the file writes in
	// upper case with a trailing dot is refused naming the name without the dot, in lower case,
	// and a host made of a placeholder is written masked, as one JSON line of the broker's shape.
	// A broker whose main does not set its logging up writes the refusal as text, with the
	// placeholder in it.
	t.Run("a host made of a placeholder in upper case, which the broker's log masks", func(t *testing.T) {
		placeholder := strings.ToUpper(placeholder.Prefix + "zzyyxxwwvvuuttssrrqqppoonnmmllkk")
		path := profilesFile(t, profilesWithHosts(t, "DEEL_API_KEY", "api.deel.com", placeholder+".EXAMPLE."))
		refused(t, path, []string{`"severity":"ERROR"`, `"message":"broker: fatal"`, "write it as [placeholder].example"},
			[]string{placeholder, strings.ToLower(placeholder)})
	})
}

// TestTheBootRefusesTheRemovedRulesSetting: BROKER_AIRLOCK_RULES_FILE is gone, its entries now
// the kinds of BROKER_PROFILES_FILE, so a deployment that still sets it is refused by
// config.Load before anything binds, naming the setting that replaces it, rather than started
// with no proxy rules and every credential it named released as its value.
func TestTheBootRefusesTheRemovedRulesSetting(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t), append(airlockEnv(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_AIRLOCK_RULES_FILE="+writeTempFile(t, "airlock.json", []byte(`{"secrets": [{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"]}]}`)),
	)...)
	for _, want := range []string{"BROKER_AIRLOCK_RULES_FILE is removed", "replaced by BROKER_PROFILES_FILE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
		}
	}
}

// TestMainStartsWithAProfilesFile: a file the broker parses and validates is no obstacle to the
// boot, and the broker goes on to serve: a kind with an allow list, and a profile whose own allow
// narrows its source at the proxy within the kind's.
func TestMainStartsWithAProfilesFile(t *testing.T) {
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_PROFILES_FILE="+profilesFile(t, `{"kinds": [
			{"name": "deel", "hosts": ["api.deel.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/rest/"}]}
		], "profiles": [
			{"name": "DEEL_READ", "source": "DEEL_API_KEY", "allow": [{"method": "GET", "pathPrefix": "/rest/v1/"}]}
		]}`),
	)})
	requireHealthy(t, broker.Addr)
}

// TestMainStartsWithAProxyHostWrittenAsTheProxyReadsIt is the control of the host rows above:
// hosts the one grammar a rules host is written in serves, in a case a reader of the file could
// fold (foldedRuleHosts), are no obstacle to the boot, and the broker goes on to serve.
func TestMainStartsWithAProxyHostWrittenAsTheProxyReadsIt(t *testing.T) {
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_PROFILES_FILE="+profilesFile(t, profilesWithHosts(t, "DEEL_API_KEY", foldedRuleHosts...)),
	)})
	requireHealthy(t, broker.Addr)
}

// TestMainRefusesAnAirlockCertificateItCannotRead drives the real binary with each kind of
// certificate file it must refuse — a path that is not there, a file that is not PEM, and a PEM
// block that is a key rather than a certificate — and asserts it exits non-zero naming the
// variables, without ever binding. The proxy signs every bound host's certificate with this
// pair and every release hands the command the certificate of it, so the pod that mounted the
// wrong file has to find out, not the commands it releases to.
func TestMainRefusesAnAirlockCertificateItCannotRead(t *testing.T) {
	binary := buildBroker(t)
	missing := filepath.Join(t.TempDir(), "absent.crt")
	// A key that matches nothing here: what each row is about is the certificate file beside
	// it, and the authority reads both.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, keyFile := airlockCAFiles(t)
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"a path that cannot be read": {path: missing, want: "no such file"},
		"a file that is not PEM":     {path: writeTempFile(t, "plain.crt", []byte("not a certificate\n")), want: "PEM data in certificate input"},
		"a PEM block that is a key":  {path: writeTempFile(t, "key.crt", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), want: "did find a private key"},
	} {
		t.Run(name, func(t *testing.T) {
			out := runRefusedBroker(t, binary,
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_PROFILES_FILE="+profilesFile(t, deelKindFile),
				"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
				"BROKER_AIRLOCK_CA_CERT_FILE="+c.path,
				"BROKER_AIRLOCK_CA_KEY_FILE="+keyFile,
			)
			if !strings.Contains(out, "BROKER_AIRLOCK_CA_CERT_FILE") || !strings.Contains(out, c.want) {
				t.Fatalf("broker refused, but its output does not name BROKER_AIRLOCK_CA_CERT_FILE and %q: %s", c.want, out)
			}
		})
	}
}

// TestTheReleasesCertificateIsTheAuthoritysOwnAndCarriesNoKey: a release hands the command the
// one certificate it trusts the proxy by, and that has to be the certificate this process signs
// each bound host with - not the bytes of the file it was read from. A certificate and its key
// in one PEM file is a shape an operator really mounts, and one the authority accepts, so a
// reader that answered the file would put the authority's private key in every release, with
// which a command could sign a certificate for any host that every other airlock command trusts.
func TestTheReleasesCertificateIsTheAuthoritysOwnAndCarriesNoKey(t *testing.T) {
	certFile, keyFile := airlockCAFiles(t)
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	combined := writeTempFile(t, "combined.pem", append(append([]byte{}, certPEM...), keyPEM...))

	ca, err := proxy.NewAuthority(combined, keyFile)
	if err != nil {
		t.Fatalf("the authority on a file holding the certificate and the key: %v; the row is about a file the boot accepts", err)
	}
	released := airlockCACertificatePEM(ca)
	if strings.Contains(released, "PRIVATE KEY") {
		t.Fatalf("the release carries a private key:\n%s", released)
	}
	block, rest := pem.Decode([]byte(released))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatalf("the release carries %q and %d bytes after it; want one PEM certificate and nothing else", released, len(rest))
	}
	if !bytes.Equal(block.Bytes, ca.Certificate().Raw) {
		t.Fatal("the release's certificate is not the one the proxy signs with")
	}
	// And a deployment with no airlock releases no certificate at all.
	if released := airlockCACertificatePEM(nil); released != "" {
		t.Fatalf("a broker with no airlock released %q; want nothing", released)
	}
}
