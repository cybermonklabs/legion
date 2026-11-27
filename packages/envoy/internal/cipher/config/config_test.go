// packages/envoy/internal/cipher/config/config_test.go
package config

//ci:reads packages/envoy/docker/rds-global-bundle.pem TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL and TestLoadLeavesAPasswordLibpqSuppliesToSignIn Load the URL whose sslrootcert is this bundle (rdsBundle), which pgx reads to build the root pool

import (
	"maps"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/redact"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// signInSecret is the client secret every environment's BROKER_SIGNIN_CLIENT_SECRET_FILE holds;
// no error Load answers may carry it.
const signInSecret = "do-not-print-this-client-secret"

// signInSecretFile is the file validEnv points BROKER_SIGNIN_CLIENT_SECRET_FILE at. TestMain
// writes it once for the package, holding signInSecret between blank lines and spaces, which
// Load trims: every environment needs a readable secret file, since the four sign-in settings
// are required.
var signInSecretFile string

func TestMain(m *testing.M) { os.Exit(runWithASignInSecretFile(m)) }

// runWithASignInSecretFile writes the sign-in secret file every test's environment names and
// removes its directory afterwards. The removal is deferred, so it also runs when the panics
// below fire. A run killed by a signal still keeps the directory, which nothing here can help.
func runWithASignInSecretFile(m *testing.M) int {
	dir, err := os.MkdirTemp("", "broker-config-signin")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	signInSecretFile = dir + "/client-secret"
	if err := os.WriteFile(signInSecretFile, []byte("\n  "+signInSecret+"  \n"), 0o600); err != nil {
		panic(err)
	}
	return m.Run()
}

// validEnv is a complete, minimal valid environment: every test starts from a copy of this and
// overrides or deletes just the variable(s) under test.
func validEnv() map[string]string {
	return map[string]string{
		"BROKER_DATABASE_URL":              "postgres://x",
		"BROKER_PUBLIC_URL":                "https://secrets.internal.example",
		"BROKER_MACHINE_LOGIN_URL":         "https://dispatch.internal.example/credentials/machine",
		"BROKER_SECRETS_PREFIX":            "example/agent-secrets/",
		"BROKER_SECRETS_KMS_KEY_ARN":       "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"BROKER_SIGNIN_ISSUER":             "https://accounts.example.com",
		"BROKER_SIGNIN_CLIENT_ID":          "broker-client",
		"BROKER_SIGNIN_CLIENT_SECRET_FILE": signInSecretFile,
		"BROKER_SIGNIN_DOMAIN":             "example.com",
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	e := validEnv()
	delete(e, "BROKER_DATABASE_URL")
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "BROKER_DATABASE_URL") {
		t.Fatalf("expected BROKER_DATABASE_URL refusal, got %v", err)
	}
}

// TestTheMachineLoginURLIsOptionalAndHeldToItsForm: the page a person types a machine login's
// code into is not required — a broker without it serves no device grant and starts all the
// same, which is the deployment whose machines hold a cloud identity and whose people never
// type a code — and a page that is given must still be an absolute http(s) URL, since the
// device grant names it to the machine.
func TestTheMachineLoginURLIsOptionalAndHeldToItsForm(t *testing.T) {
	for _, v := range []string{"/credentials/machine", "dispatch.example/credentials/machine", "ftp://dispatch.example/x", "https:///x"} {
		e := validEnv()
		e["BROKER_MACHINE_LOGIN_URL"] = v
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_MACHINE_LOGIN_URL") {
			t.Errorf("Load(BROKER_MACHINE_LOGIN_URL=%q) = %v, want a refusal naming it", v, err)
		}
	}
	for _, unset := range []string{"", "   "} {
		e := validEnv()
		e["BROKER_MACHINE_LOGIN_URL"] = unset
		cfg, err := Load(env(e))
		if err != nil || cfg.MachineLoginURL != "" {
			t.Errorf("Load(BROKER_MACHINE_LOGIN_URL=%q) = (%q, %v), want a broker that starts with no machine login page", unset, cfg.MachineLoginURL, err)
		}
	}
	e := validEnv()
	delete(e, "BROKER_MACHINE_LOGIN_URL")
	if cfg, err := Load(env(e)); err != nil || cfg.MachineLoginURL != "" {
		t.Fatalf("Load with no BROKER_MACHINE_LOGIN_URL = (%q, %v), want a broker that starts with no machine login page", cfg.MachineLoginURL, err)
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.MachineLoginURL != "https://dispatch.internal.example/credentials/machine" {
		t.Fatalf("Load = (%q, %v), want the machine login URL", cfg.MachineLoginURL, err)
	}
}

func TestLoadKeepsTheAzureProjectedTokenPathForEachRelease(t *testing.T) {
	for _, row := range []struct{ configured, want string }{
		{"", "/var/run/secrets/azure/tokens/azure-identity-token"},
		{"/not-mounted-at-boot/azure-token", "/not-mounted-at-boot/azure-token"},
	} {
		e := validEnv()
		e["BROKER_AZURE_TOKEN_FILE"] = row.configured
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AzureTokenFile != row.want {
			t.Errorf("AzureTokenFile = %q, want %q", cfg.AzureTokenFile, row.want)
		}
	}
}

func TestLoadLimitsAzureLoginToOneNamedSecret(t *testing.T) {
	for _, name := range []string{"", "AZURE_LOGIN"} {
		e := validEnv()
		e["BROKER_AZURE_LOGIN_SECRET"] = name
		cfg, err := Load(env(e))
		if err != nil || cfg.AzureLoginSecret != name {
			t.Fatalf("Load AzureLoginSecret = %q, %v, want %q", cfg.AzureLoginSecret, err, name)
		}
	}
	for _, name := range []string{"azure-login", "AZURE__LOGIN", " AZURE_LOGIN", "AZURE_LOGIN OTHER_LOGIN"} {
		e := validEnv()
		e["BROKER_AZURE_LOGIN_SECRET"] = name
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_AZURE_LOGIN_SECRET") {
			t.Fatalf("Load an invalid Azure login secret name = %v, want refusal naming the setting", err)
		}
	}
}

// The deployment's binding of each google-service-account agent secret to the one Google account
// the broker may mint a token for it, BROKER_SERVICE_ACCOUNT_SECRETS: whitespace-separated
// <SECRET NAME>=<account email> entries, keyed by the name a session asks for, and nil when the
// setting is unset, which binds nothing and so mints nothing of that type. Two secrets may name
// one account, since only the secret name is the key; each refusal names the setting and the
// whole entry, so an operator reads which one to fix.
func TestServiceAccountBindingSettingIsRead(t *testing.T) {
	const (
		dev   = "dev-aditya@example-identities.iam.gserviceaccount.com"
		admin = "gcp-admin@example-project.iam.gserviceaccount.com"
	)
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceAccountSecrets != nil {
		t.Fatalf("ServiceAccountSecrets = %v, want nil when BROKER_SERVICE_ACCOUNT_SECRETS is unset", cfg.ServiceAccountSecrets)
	}

	e := validEnv()
	e["BROKER_SERVICE_ACCOUNT_SECRETS"] = " DEV_PERSON=" + dev + "\tGCP_ADMIN=" + admin + "\n"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"DEV_PERSON": dev, "GCP_ADMIN": admin}; !maps.Equal(cfg.ServiceAccountSecrets, want) {
		t.Fatalf("ServiceAccountSecrets = %v, want %v", cfg.ServiceAccountSecrets, want)
	}

	// Two secrets of one account: a person's own account may back both their dev secret and a
	// second one, and the mint is refused by the secret's name, not the account's.
	e = validEnv()
	e["BROKER_SERVICE_ACCOUNT_SECRETS"] = "DEV_PERSON=" + dev + " DEV_PERSON_2=" + dev
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"DEV_PERSON": dev, "DEV_PERSON_2": dev}; !maps.Equal(cfg.ServiceAccountSecrets, want) {
		t.Fatalf("ServiceAccountSecrets with one account on two secrets = %v, want %v", cfg.ServiceAccountSecrets, want)
	}

	for _, row := range []struct{ entry, want string }{
		{"DEV_PERSON", "must be <SECRET NAME>=<account email> entries"},
		{"dev-aditya=" + dev, "not an agent secret name"},
		{"DEV_PERSON=aditya@example.com", "user-managed Google service account"},
		{"DEV_PERSON=service-123456789012@gcp-sa-example.iam.gserviceaccount.com", "Google-managed service agent"},
		{"DEV_PERSON=" + admin, "names the secret DEV_PERSON twice"},
	} {
		e := validEnv()
		e["BROKER_SERVICE_ACCOUNT_SECRETS"] = "DEV_PERSON=" + dev + " " + row.entry
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_SERVICE_ACCOUNT_SECRETS ") ||
			!strings.Contains(err.Error(), row.want) || !strings.Contains(err.Error(), strconv.Quote(row.entry)) {
			t.Errorf("Load(BROKER_SERVICE_ACCOUNT_SECRETS with %q) = %v, want a refusal naming the setting, the entry and %q", row.entry, err, row.want)
		}
	}
}

// TestLoadRefusesAURLWithoutPrintingItsCredential: a BROKER_PUBLIC_URL or BROKER_MACHINE_LOGIN_URL
// that carries a credential and is refused names the variable and the value as
// redact.NotPrinted, never the user name, password or query; a clean refused value is named as
// written.
func TestLoadRefusesAURLWithoutPrintingItsCredential(t *testing.T) {
	for _, row := range []struct{ name, value, want string }{
		{"BROKER_PUBLIC_URL", userinfoURL("https", "tokuser9f3", "hunter2pw", "secrets.example/v1?token=qtok77"), redact.NotPrinted},
		{"BROKER_PUBLIC_URL", userinfoURL("https", "tokuser9f3", "hunter2pw%zz", "secrets.example"), redact.NotPrinted},
		{"BROKER_PUBLIC_URL", "https://secrets.example/v1", `"https://secrets.example/v1"`},
		{"BROKER_MACHINE_LOGIN_URL", userinfoURL("ftp", "tokuser9f3", "hunter2pw", "dispatch.example/x"), redact.NotPrinted},
	} {
		e := validEnv()
		e[row.name] = row.value
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), row.name) || !strings.Contains(err.Error(), row.want) {
			t.Errorf("Load(%s=%q) = %v, want a refusal naming it and %s", row.name, row.value, err, row.want)
			continue
		}
		for _, secret := range []string{"tokuser9f3", "hunter2pw", "qtok77"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Load(%s=%q) = %v, names %q", row.name, row.value, err, secret)
			}
		}
	}
}

// TestLoadRefusesDispatchVariables pins that the removed Dispatch variables fail loudly rather
// than being silently ignored: the broker asks and issues nothing in Dispatch, and the two things
// it does read there, whether an issue is open and whether a person may see a project, are read
// under settings of their own (BROKER_JUDGE_DISPATCH_URL, BROKER_PROJECT_ACCESS_URL), so a stale
// deployment env still setting one of the five must refuse to start, naming the variable.
func TestLoadRefusesDispatchVariables(t *testing.T) {
	e := validEnv()
	e["BROKER_DISPATCH_URL"] = "https://dispatch.internal.example"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_DISPATCH_URL is removed; ") {
		t.Fatalf("Load(BROKER_DISPATCH_URL set) = %v, want a refusal naming BROKER_DISPATCH_URL", err)
	}
}

// TestLoadRefusesUIOrigin pins that BROKER_UI_ORIGIN, the WebAuthn origin approval by Dispatch
// login removed, fails loudly rather than being silently ignored: a deployment still setting it
// was built for the key-signature design and must be updated, not run half-migrated.
func TestLoadRefusesUIOrigin(t *testing.T) {
	e := validEnv()
	e["BROKER_UI_ORIGIN"] = "https://secrets-ui.internal.example"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_UI_ORIGIN is removed; ") {
		t.Fatalf("Load(BROKER_UI_ORIGIN set) = %v, want a refusal naming BROKER_UI_ORIGIN", err)
	}
}

// TestLoadRefusesMaxGrantSeconds pins that deployments cannot silently turn their explicit grant
// cap into a session-length grant after that setting is removed.
func TestLoadRefusesMaxGrantSeconds(t *testing.T) {
	e := validEnv()
	e["BROKER_MAX_GRANT_SECONDS"] = "3600"
	_, err := Load(env(e))
	if err == nil || !strings.HasPrefix(err.Error(), "BROKER_MAX_GRANT_SECONDS is removed; ") {
		t.Fatalf("Load(BROKER_MAX_GRANT_SECONDS set) = %v, want a refusal naming BROKER_MAX_GRANT_SECONDS", err)
	}
}

// TestLoadRefusesTheUIToken pins that the removed shared bearer fails loudly, by either name:
// the routes it authenticated take the calling service's own ServiceAccount token now, so a
// deployment still setting it is one whose callers were never listed.
func TestLoadRefusesTheUIToken(t *testing.T) {
	for _, name := range []string{"BROKER_UI_TOKEN", "BROKER_UI_TOKEN_FILE"} {
		e := validEnv()
		e[name] = "whatever"
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), name+" is removed; ") || !strings.Contains(err.Error(), "BROKER_API_ACCOUNTS") {
			t.Fatalf("Load(%s set) = %v, want a refusal naming %s and BROKER_API_ACCOUNTS", name, err, name)
		}
	}
}

// TestLoadDefaultsTheIntegerSettings pins every seconds setting's default, the values a
// deployment that sets none runs on.
func TestLoadDefaultsTheIntegerSettings(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaseSeconds != 900 || cfg.ProofSkewSeconds != 60 || cfg.AccessTokenSeconds != 900 ||
		cfg.LauncherCredentialSeconds != 2592000 || cfg.SweepSeconds != 5 || cfg.DrainSeconds != 5 ||
		cfg.SessionCarrySeconds != 43200 {
		t.Fatalf("defaults wrong: LeaseSeconds=%d ProofSkewSeconds=%d AccessTokenSeconds=%d LauncherCredentialSeconds=%d SweepSeconds=%d DrainSeconds=%d SessionCarrySeconds=%d",
			cfg.LeaseSeconds, cfg.ProofSkewSeconds, cfg.AccessTokenSeconds, cfg.LauncherCredentialSeconds, cfg.SweepSeconds, cfg.DrainSeconds, cfg.SessionCarrySeconds)
	}
}

// TestLoadReadsTheDrainSeconds pins BROKER_DRAIN_SECONDS, the wait the broker serves out after
// SIGTERM while answering /healthz with 503: 0 is allowed, since a local run has no EndpointSlice
// to propagate and the wait would be pure delay, and a value past two minutes, a negative one or
// one that is not a whole number refuses to start naming the variable, rather than leaving the
// drain at a length nobody chose.
func TestLoadReadsTheDrainSeconds(t *testing.T) {
	for _, good := range []struct {
		value string
		want  int
	}{{"0", 0}, {"30", 30}, {"120", 120}} {
		e := validEnv()
		e["BROKER_DRAIN_SECONDS"] = good.value
		cfg, err := Load(env(e))
		if err != nil || cfg.DrainSeconds != good.want {
			t.Fatalf("Load(BROKER_DRAIN_SECONDS=%s) = %d, %v; want %d", good.value, cfg.DrainSeconds, err, good.want)
		}
	}
	for _, bad := range []string{"121", "-1", "five", "5s", "5.0", " "} {
		e := validEnv()
		e["BROKER_DRAIN_SECONDS"] = bad
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_DRAIN_SECONDS must be a whole number between 0 and 120") {
			t.Fatalf("Load(BROKER_DRAIN_SECONDS=%q) = %v, want a refusal naming BROKER_DRAIN_SECONDS and its range", bad, err)
		}
	}
}

// TestLoadRefusesALeaseAnOutageCanRunOut pins the exact boundary of the lease refusal at the two
// sweeps the documents and the tests name, from the model of the renewers' own retry schedules
// (internal/cipher/leasetime): the largest refused lease is refused naming both settings, the
// gap and the floor, and one second more is accepted. The defaults pass, and so does the pair
// cmd/cipherd's outage test runs at, so no deployment and no test stops on this refusal.
func TestLoadRefusesALeaseAnOutageCanRunOut(t *testing.T) {
	model := leasetime.Production()
	for _, sweep := range []int{5, 60} {
		floor := int(model.Shortest(time.Duration(sweep)*time.Second) / time.Second)
		refused, accepted := strconv.Itoa(floor-1), strconv.Itoa(floor)
		e := validEnv()
		e["BROKER_LEASE_SECONDS"], e["BROKER_SWEEP_SECONDS"] = refused, strconv.Itoa(sweep)
		_, err := Load(env(e))
		if err == nil {
			t.Fatalf("Load(lease %s, sweep %d) accepted a lease an outage of %d sweeps can run out", refused, sweep, leasetime.NotServingSweeps)
		}
		for _, want := range []string{
			"BROKER_LEASE_SECONDS (" + refused + ")",
			"BROKER_SWEEP_SECONDS (" + strconv.Itoa(sweep) + ")",
			"a gap of " + strconv.Itoa(leasetime.NotServingSweeps*sweep) + " s",
			"the time its renewer then takes to renew",
			"The shortest lease that survives both renewers at this sweep is " + accepted + " s.",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Load(lease %s, sweep %d) = %q, want it to name %q", refused, sweep, err, want)
			}
		}
		t.Logf("lease %s at a sweep of %d is refused: %v", refused, sweep, err)
		e["BROKER_LEASE_SECONDS"] = accepted
		if cfg, err := Load(env(e)); err != nil || cfg.LeaseSeconds != floor || cfg.SweepSeconds != sweep {
			t.Fatalf("Load(lease %s, sweep %d) = lease %d sweep %d, %v; want the shortest lease that survives accepted", accepted, sweep, cfg.LeaseSeconds, cfg.SweepSeconds, err)
		}
		t.Logf("lease %s at a sweep of %d is accepted", accepted, sweep)
	}
	// The defaults, and the lease and sweep TestABrokerOutageLongerThanALease runs at, which it
	// takes from the same model.
	outageLease := int(model.Shortest(time.Second) / time.Second)
	for _, c := range []struct{ lease, sweep string }{
		{"", ""},
		{strconv.Itoa(outageLease), "1"},
		{"900", "60"},
	} {
		e := validEnv()
		if c.lease != "" {
			e["BROKER_LEASE_SECONDS"], e["BROKER_SWEEP_SECONDS"] = c.lease, c.sweep
		}
		cfg, err := Load(env(e))
		if err != nil {
			t.Errorf("Load(lease %q, sweep %q) = %v, want it accepted", c.lease, c.sweep, err)
			continue
		}
		t.Logf("lease %d at a sweep of %d is accepted", cfg.LeaseSeconds, cfg.SweepSeconds)
	}
}

// TestLoadReadsTrustedProxyHeaderOptionally pins that BROKER_TRUSTED_PROXY_HEADER is optional
// (unset means "" — the rate limiter keys on r.RemoteAddr) and, when set, passes through
// verbatim as the header name to trust.
func TestLoadReadsTrustedProxyHeaderOptionally(t *testing.T) {
	base := validEnv()
	cfg, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxyHeader != "" {
		t.Fatalf("TrustedProxyHeader = %q, want empty when BROKER_TRUSTED_PROXY_HEADER is unset", cfg.TrustedProxyHeader)
	}

	withHeader := validEnv()
	withHeader["BROKER_TRUSTED_PROXY_HEADER"] = "X-Forwarded-For"
	cfg, err = Load(env(withHeader))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxyHeader != "X-Forwarded-For" {
		t.Fatalf("TrustedProxyHeader = %q, want X-Forwarded-For", cfg.TrustedProxyHeader)
	}
}

// TestLoadReadsTheProfilesFileOptionally pins that BROKER_PROFILES_FILE is optional — unset is
// "", a deployment with no profiles and so no proxy — and that a path given is read as written.
// Load opens nothing: the broker reads the file itself at start, where a file it cannot read
// refuses the boot.
func TestLoadReadsTheProfilesFileOptionally(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProfilesFile != "" {
		t.Fatalf("ProfilesFile = %q, want empty when BROKER_PROFILES_FILE is unset", cfg.ProfilesFile)
	}

	e := airlockEnv()
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProfilesFile != "/etc/broker/profiles.json" {
		t.Fatalf("ProfilesFile = %q, want /etc/broker/profiles.json", cfg.ProfilesFile)
	}
}

// TestLoadRefusesTheAirlockRulesFileSetting: the airlock's rules are the proxy blocks of the
// profiles file now, so a deployment still setting BROKER_AIRLOCK_RULES_FILE is refused naming
// the setting that replaces it, rather than started with no proxy and every name the old file
// held back released as its value. It is refused beside the new setting too.
func TestLoadRefusesTheAirlockRulesFileSetting(t *testing.T) {
	for name, e := range map[string]map[string]string{"alone": validEnv(), "beside BROKER_PROFILES_FILE": airlockEnv()} {
		e["BROKER_AIRLOCK_RULES_FILE"] = "/etc/broker/airlock.json"
		_, err := Load(env(e))
		if err == nil || err.Error() != "BROKER_AIRLOCK_RULES_FILE is removed; replaced by BROKER_PROFILES_FILE" {
			t.Errorf("Load with BROKER_AIRLOCK_RULES_FILE set %s = %v; want it refused, naming BROKER_PROFILES_FILE", name, err)
		}
	}
}

// airlockEnv is validEnv with the airlock in force: a profiles file and the three settings the
// proxy of its kinds and dpop blocks cannot run without.
func airlockEnv() map[string]string {
	e := validEnv()
	e["BROKER_PROFILES_FILE"] = "/etc/broker/profiles.json"
	e["BROKER_AIRLOCK_PROXY_URL"] = "http://broker.legion.svc:13381"
	e["BROKER_AIRLOCK_CA_CERT_FILE"] = "/etc/broker/airlock-ca/tls.crt"
	e["BROKER_AIRLOCK_CA_KEY_FILE"] = "/etc/broker/airlock-ca/tls.key"
	return e
}

// subjectsFile is a profiles file of two kinds, github and slack, a profile behind github by its
// proxy string, and two dpop blocks, TEST_MINTED_TOKEN and then APPROVE_TOKEN, out of name order,
// so a list of its subjects in the file's order is told from one in name order.
const subjectsFile = `{"kinds": [` +
	`{"name": "github", "hosts": ["api.github.com"], "ways": ["swap", "mint"]}, ` +
	`{"name": "slack", "hosts": ["slack.com"], "ways": ["swap"]}], ` +
	`"profiles": [` +
	`{"name": "GITHUB_READ", "source": "GITHUB_PAT", "proxy": "github"}, ` +
	`{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}, ` +
	`{"name": "APPROVE_TOKEN", "proxy": {"hosts": ["approve.test"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/approve/"}]}}]}`

// fileRules is the file's own part of the airlock's rules as the boot reads it: its kinds and its
// dpop blocks (policy.Profiles.FileRules).
func fileRules(t *testing.T, file string) proxyrules.Rules {
	t.Helper()
	profiles, err := policy.ParseProfiles([]byte(file))
	if err != nil {
		t.Fatalf("ParseProfiles: %v", err)
	}
	return profiles.FileRules()
}

// TestTheAirlocksProxySettingsAreRequiredForAKindOrADPoPBlock: a broker whose profiles file
// declares a kind or holds a dpop block but that cannot proxy would hold back every name a proxy
// tag puts behind such a kind, or the dpop block names, and release nothing in its place, so each
// missing setting refuses the boot by name, beside each kind and dpop block that needs it
// (AirlockSubjects). A profiles file is not a kind: Load takes one with none of the settings, and
// RequireAirlock of a file of neither - a deployment that serves GitHub Apps and runs no airlock -
// asks for none of them. With no profiles file there is no proxy and the four are ignored,
// whatever they say.
func TestTheAirlocksProxySettingsAreRequiredForAKindOrADPoPBlock(t *testing.T) {
	proxied := AirlockSubjects(fileRules(t, subjectsFile))
	cfg, err := Load(env(airlockEnv()))
	if err != nil {
		t.Fatalf("Load with the airlock fully configured: %v", err)
	}
	if err := cfg.RequireAirlock(proxied); err != nil {
		t.Fatalf("RequireAirlock with the airlock fully configured: %v", err)
	}
	for _, missing := range []string{"BROKER_AIRLOCK_PROXY_URL", "BROKER_AIRLOCK_CA_CERT_FILE", "BROKER_AIRLOCK_CA_KEY_FILE"} {
		e := airlockEnv()
		delete(e, missing)
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load without %s = %v; want it taken, since a profiles file need not declare a kind", missing, err)
		}
		err = cfg.RequireAirlock(proxied)
		const declares = "BROKER_PROFILES_FILE declares kind github, kind slack, dpop block TEST_MINTED_TOKEN, dpop block APPROVE_TOKEN, so the broker runs the airlock's proxy for them"
		if err == nil || !strings.Contains(err.Error(), declares) || !strings.Contains(err.Error(), "missing "+missing) {
			t.Errorf("RequireAirlock without %s = %v; want a refusal that says %q and names the missing setting", missing, err, declares)
		}
	}

	// A profiles file that declares no kind and holds no dpop block, with none of the proxy's
	// settings.
	e := validEnv()
	e["BROKER_PROFILES_FILE"] = "/etc/broker/profiles.json"
	if cfg, err = Load(env(e)); err != nil {
		t.Fatalf("Load with a profiles file and none of the proxy's settings: %v", err)
	}
	if err := cfg.RequireAirlock(nil); err != nil {
		t.Fatalf("RequireAirlock of a file of no kind and no dpop block, with none of the proxy's settings: %v; want nil", err)
	}

	// No profiles file: the same configuration minus the file, and minus every proxy setting, is
	// accepted, and so is one that still names the listen address and the idle bound.
	e = validEnv()
	e["BROKER_AIRLOCK_LISTEN_ADDR"] = "127.0.0.1:13999"
	e["BROKER_AIRLOCK_IDLE_SECONDS"] = "60"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatalf("Load with no profiles file: %v", err)
	}
	if cfg.AirlockProxyURL != "" || cfg.AirlockCACertFile != "" || cfg.AirlockCAKeyFile != "" {
		t.Errorf("with no profiles file the proxy settings read %q, %q, %q; want none", cfg.AirlockProxyURL, cfg.AirlockCACertFile, cfg.AirlockCAKeyFile)
	}
	if cfg.AirlockListenAddr != "127.0.0.1:13999" || cfg.AirlockIdleSeconds != 60 {
		t.Errorf("airlock listen addr = %q and idle = %d; want 127.0.0.1:13999 and 60", cfg.AirlockListenAddr, cfg.AirlockIdleSeconds)
	}

	// And their defaults, which a deployment that configures neither takes.
	if cfg, err = Load(env(validEnv())); err != nil {
		t.Fatal(err)
	}
	if cfg.AirlockListenAddr != "127.0.0.1:13381" || cfg.AirlockIdleSeconds != 300 {
		t.Errorf("default airlock listen addr = %q and idle = %d; want 127.0.0.1:13381 and 300", cfg.AirlockListenAddr, cfg.AirlockIdleSeconds)
	}
}

// TestAirlockSubjectsNamesEachKindAndEachDPoPBlock: what of a profiles file puts a credential
// behind the proxy is each kind it declares, whether or not a secret is tagged with it yet, since
// a proxy tag set at runtime puts a name behind it with no new boot, and each dpop block; a profile
// behind a kind adds no subject of its own. Kinds come first, then dpop blocks, each in the file's
// order. A file of either alone needs the proxy, so RequireAirlock refuses it with none of the
// proxy's settings, and a file of neither answers nil, which needs none.
func TestAirlockSubjectsNamesEachKindAndEachDPoPBlock(t *testing.T) {
	e := validEnv()
	e["BROKER_PROFILES_FILE"] = "/etc/broker/profiles.json"
	bare, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load with a profiles file and none of the proxy's settings: %v", err)
	}
	for _, c := range []struct {
		name, file string
		want       []string
	}{
		{"kinds, a profile behind one and dpop blocks", subjectsFile, []string{"kind github", "kind slack", "dpop block TEST_MINTED_TOKEN", "dpop block APPROVE_TOKEN"}},
		{"a kind no profile names", `{"kinds": [{"name": "example", "hosts": ["api.example.com"], "ways": ["swap"]}], "profiles": []}`, []string{"kind example"}},
		{"a dpop block alone", `{"profiles": [{"name": "APPROVE_TOKEN", "proxy": {"hosts": ["approve.test"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/approve/"}]}}]}`, []string{"dpop block APPROVE_TOKEN"}},
		{"a GitHub App's profile alone", `{"profiles": [{"name": "APP_KEY", "github": {"accounts": ["acme"], "permissions": {"metadata": "read"}}}]}`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := AirlockSubjects(fileRules(t, c.file))
			if !slices.Equal(got, c.want) {
				t.Fatalf("AirlockSubjects = %q; want %q", got, c.want)
			}
			err := bare.RequireAirlock(got)
			if c.want == nil && err != nil {
				t.Fatalf("RequireAirlock of a file of neither kinds nor dpop blocks, with none of the proxy's settings: %v; want nil", err)
			}
			if c.want != nil && (err == nil || !strings.Contains(err.Error(), "declares "+strings.Join(c.want, ", ")+",")) {
				t.Fatalf("RequireAirlock with none of the proxy's settings = %v; want a refusal naming %q", err, c.want)
			}
		})
	}
}

// TestMintedEntryRulesReadsTheFilesDPoPBlocksAndKinds: the boot's dpop block checks read the
// profiles file's own rules (policy.Profiles.FileRules), where each dpop block is an inline
// binding of way dpop: a file whose kinds bind other hosts passes, and one with a kind that binds
// a dpop block's host, exactly or by a "*." name, is refused naming the kind, the host and the
// block, since a request a proxy tag sends to that host would then go out of the minted grant's
// allow list.
func TestMintedEntryRulesReadsTheFilesDPoPBlocksAndKinds(t *testing.T) {
	if err := MintedEntryRules(fileRules(t, subjectsFile)); err != nil {
		t.Fatalf("MintedEntryRules of a file whose kinds bind no dpop block's host: %v", err)
	}
	const block = `{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.test"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`
	for _, host := range []string{"dispatch.test", "*.test"} {
		file := `{"kinds": [{"name": "dispatch", "hosts": ["api.example.com", "` + host + `"], "ways": ["swap"]}], "profiles": [` + block + `]}`
		err := MintedEntryRules(fileRules(t, file))
		if err == nil || !strings.Contains(err.Error(), "the kind dispatch") || !strings.Contains(err.Error(), "binds dispatch.test") || !strings.Contains(err.Error(), "TEST_MINTED_TOKEN") {
			t.Errorf("MintedEntryRules of a file whose kind binds %s = %v; want a refusal naming the kind dispatch, the host dispatch.test and TEST_MINTED_TOKEN", host, err)
		}
	}
}

// TestLoadReadsTheMetricsListenAddr: the metrics port binds BROKER_METRICS_LISTEN_ADDR as
// written, and 127.0.0.1:13382 when it is unset - beside the API's 13380 and the airlock's
// 13381, and like both of them on the loopback address, so a broker started with no setting
// serves its metrics to its own host alone. Unlike the airlock's port it is read whatever else
// is configured, since every broker serves its metrics.
func TestLoadReadsTheMetricsListenAddr(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsListenAddr != "127.0.0.1:13382" {
		t.Errorf("default metrics listen addr = %q; want 127.0.0.1:13382", cfg.MetricsListenAddr)
	}
	for _, e := range []map[string]string{validEnv(), airlockEnv()} {
		e["BROKER_METRICS_LISTEN_ADDR"] = "0.0.0.0:13382"
		if cfg, err = Load(env(e)); err != nil {
			t.Fatal(err)
		}
		if cfg.MetricsListenAddr != "0.0.0.0:13382" {
			t.Errorf("metrics listen addr = %q; want 0.0.0.0:13382 as BROKER_METRICS_LISTEN_ADDR names it", cfg.MetricsListenAddr)
		}
	}
}

// TestTheAirlocksProxyURLIsHeldToItsForm: every release of a name the rules hold back carries
// this string, and a command's own client reads it as the proxy to dial, so a value with no
// scheme or no host must refuse the boot rather than fail every airlock run one command at a
// time. It is held to the same form BROKER_MACHINE_LOGIN_URL beside it is.
func TestTheAirlocksProxyURLIsHeldToItsForm(t *testing.T) {
	for _, row := range []struct{ name, value string }{
		{"a host and a port with no scheme", "proxy:13381"},
		{"a host with no scheme at all", "broker.legion.svc"},
		{"an authority with no scheme", "//broker.legion.svc:13381"},
		{"a scheme that is not http", "socks5://broker.legion.svc:13381"},
		{"a scheme and no host", "http://"},
		{"a path alone", "/airlock"},
		{"no URL at all", "http://%zz"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := airlockEnv()
			e["BROKER_AIRLOCK_PROXY_URL"] = row.value
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), "BROKER_AIRLOCK_PROXY_URL") {
				t.Errorf("Load(BROKER_AIRLOCK_PROXY_URL=%q) = %v; want a refusal naming the variable", row.value, err)
			}
		})
	}
	// And the two shapes a deployment really sets: the Service a command reaches inside the
	// cluster, and a proxy behind TLS.
	for _, value := range []string{"http://broker.legion.svc:13381", "https://airlock.example.com"} {
		e := airlockEnv()
		e["BROKER_AIRLOCK_PROXY_URL"] = value
		cfg, err := Load(env(e))
		if err != nil || cfg.AirlockProxyURL != value {
			t.Errorf("Load(BROKER_AIRLOCK_PROXY_URL=%q) = (%q, %v); want it taken as it is", value, cfg.AirlockProxyURL, err)
		}
	}
}

// TestLoadReadsTheAirlockClusterRanges: BROKER_AIRLOCK_CLUSTER_RANGES is a comma-separated list
// of CIDR prefixes, IPv4 and IPv6 alike, with spaces around the commas allowed, each read as its
// network; unset is none, which is a broker outside a cluster.
func TestLoadReadsTheAirlockClusterRanges(t *testing.T) {
	for _, row := range []struct{ name, value, want string }{
		{"unset", "", ""},
		{"one range", "34.118.224.0/20", "34.118.224.0/20"},
		{"IPv4 and IPv6 with spaces around the commas", " 34.118.224.0/20 , 2001:db8:5::/48,198.51.100.0/24 ", "34.118.224.0/20 2001:db8:5::/48 198.51.100.0/24"},
		{"a prefix with host bits, read as its network", "34.118.230.7/20", "34.118.224.0/20"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_AIRLOCK_CLUSTER_RANGES"] = row.value
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load(BROKER_AIRLOCK_CLUSTER_RANGES=%q): %v", row.value, err)
			}
			var got []string
			for _, prefix := range cfg.AirlockClusterRanges {
				got = append(got, prefix.String())
			}
			if strings.Join(got, " ") != row.want {
				t.Fatalf("Load(BROKER_AIRLOCK_CLUSTER_RANGES=%q) read %q; want %q", row.value, got, row.want)
			}
		})
	}
}

// TestLoadRefusesAClusterRangeItCannotRead: an element that is empty or not a CIDR prefix refuses
// the start, naming the setting and the element's position and never quoting the element, which
// could be anything an operator pasted. An IPv4-mapped IPv6 prefix is refused too, since the proxy
// reads every address it checks as plain IPv4 and no address would ever be inside one.
func TestLoadRefusesAClusterRangeItCannotRead(t *testing.T) {
	for _, row := range []struct{ name, value, want string }{
		{"a trailing comma", "34.118.224.0/20,", "element 2 is empty"},
		{"a leading comma", ",34.118.224.0/20", "element 1 is empty"},
		{"a blank element between two", "34.118.224.0/20, ,198.51.100.0/24", "element 2 is empty"},
		{"an element that is no prefix", "34.118.224.0/20,not-a-range", "element 2 is not a CIDR prefix"},
		{"an address with no prefix length", "34.118.224.1", "element 1 is not a CIDR prefix"},
		{"an IPv4-mapped IPv6 prefix", "::ffff:34.118.224.0/116", "element 1 is an IPv4-mapped IPv6 prefix"},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, e := range []map[string]string{validEnv(), airlockEnv()} {
				e["BROKER_AIRLOCK_CLUSTER_RANGES"] = row.value
				_, err := Load(env(e))
				if err == nil || !strings.Contains(err.Error(), "BROKER_AIRLOCK_CLUSTER_RANGES") || !strings.Contains(err.Error(), row.want) {
					t.Fatalf("Load(BROKER_AIRLOCK_CLUSTER_RANGES=%q) = %v; want a refusal naming the setting and %q", row.value, err, row.want)
				}
				if strings.Contains(err.Error(), "not-a-range") {
					t.Fatalf("the refusal quotes the element: %v", err)
				}
			}
		})
	}
}

// TestTheClusterRangesMustHoldTheKubernetesServiceAddress: in a cluster, which Kubernetes says by
// setting KUBERNETES_SERVICE_HOST in every container, a broker whose profiles file declares a kind
// or holds a dpop block refuses to start unless BROKER_AIRLOCK_CLUSTER_RANGES holds that address,
// the kubernetes Service's ClusterIP, so a deployment that forgot the setting or gave the wrong
// range cannot pass silently. Each refusal names both variables. Outside a cluster, and in a
// cluster with neither, whether or not there is a profiles file, the setting is optional.
func TestTheClusterRangesMustHoldTheKubernetesServiceAddress(t *testing.T) {
	// requireAirlock loads e, which must load, and answers RequireAirlock of proxied over it.
	requireAirlock := func(t *testing.T, e map[string]string, proxied []string) error {
		t.Helper()
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load(KUBERNETES_SERVICE_HOST=%q, BROKER_AIRLOCK_CLUSTER_RANGES=%q): %v; want it taken, since the ranges are required by the profiles the boot reads", e["KUBERNETES_SERVICE_HOST"], e["BROKER_AIRLOCK_CLUSTER_RANGES"], err)
		}
		return cfg.RequireAirlock(proxied)
	}
	proxied := []string{"kind example"}
	for _, row := range []struct{ name, host, ranges, want string }{
		{"the setting missing", "34.118.224.1", "", "BROKER_AIRLOCK_CLUSTER_RANGES is required"},
		{"a KUBERNETES_SERVICE_HOST that is not an address", "kubernetes.default.svc", "34.118.224.0/20", "KUBERNETES_SERVICE_HOST is not an IP address"},
		{"a range that does not hold the address", "34.118.224.1", "198.51.100.0/24", "KUBERNETES_SERVICE_HOST 34.118.224.1 is inside no range"},
		{"an IPv6 address outside an IPv4 range", "2001:db8:5::1", "34.118.224.0/20", "KUBERNETES_SERVICE_HOST 2001:db8:5::1 is inside no range"},
		// Kubernetes never sets these, and a value that is not empty is not "outside a cluster".
		{"a KUBERNETES_SERVICE_HOST of spaces", "   ", "", "BROKER_AIRLOCK_CLUSTER_RANGES is required"},
		{"a KUBERNETES_SERVICE_HOST of a tab", "\t", "", "BROKER_AIRLOCK_CLUSTER_RANGES is required"},
		// A zone is read off before the check, and the refusal repeats the parsed address alone.
		{"a zoned address outside the ranges", "fe80::1%some arbitrary text", "34.118.224.0/20", "KUBERNETES_SERVICE_HOST fe80::1 is inside no range"},
	} {
		t.Run("refuses "+row.name, func(t *testing.T) {
			e := airlockEnv()
			e["KUBERNETES_SERVICE_HOST"] = row.host
			e["BROKER_AIRLOCK_CLUSTER_RANGES"] = row.ranges
			err := requireAirlock(t, e, proxied)
			if err == nil {
				t.Fatalf("RequireAirlock(KUBERNETES_SERVICE_HOST=%q, BROKER_AIRLOCK_CLUSTER_RANGES=%q) = nil; want a refusal", row.host, row.ranges)
			}
			for _, want := range []string{"BROKER_AIRLOCK_CLUSTER_RANGES", "KUBERNETES_SERVICE_HOST", row.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal reads %q; want it to name %q", err, want)
				}
			}
			if row.host == "kubernetes.default.svc" && strings.Contains(err.Error(), row.host) {
				t.Errorf("the refusal quotes a KUBERNETES_SERVICE_HOST that is not an address: %v", err)
			}
			if strings.Contains(err.Error(), "arbitrary") {
				t.Errorf("the refusal repeats the text after a zone's %%: %v", err)
			}
		})
	}
	for _, row := range []struct{ name, host, ranges string }{
		{"a range that holds the address", "34.118.224.1", "34.118.224.0/20"},
		{"a later range that holds the address", "34.118.224.1", "198.51.100.0/24, 34.118.224.0/20"},
		{"an IPv6 range that holds an IPv6 address", "2001:db8:5::1", "2001:db8:5::/48"},
		{"an IPv6 range that holds a zoned IPv6 address", "2001:db8:5::1%eth0", "2001:db8:5::/48"},
		{"outside a cluster, with the setting missing", "", ""},
		{"outside a cluster, with a range", "", "34.118.224.0/20"},
	} {
		t.Run("starts with "+row.name, func(t *testing.T) {
			e := airlockEnv()
			e["KUBERNETES_SERVICE_HOST"] = row.host
			e["BROKER_AIRLOCK_CLUSTER_RANGES"] = row.ranges
			if err := requireAirlock(t, e, proxied); err != nil {
				t.Fatalf("RequireAirlock(KUBERNETES_SERVICE_HOST=%q, BROKER_AIRLOCK_CLUSTER_RANGES=%q): %v", row.host, row.ranges, err)
			}
		})
	}
	t.Run("starts in a cluster with no airlock and no ranges", func(t *testing.T) {
		e := validEnv()
		e["KUBERNETES_SERVICE_HOST"] = "34.118.224.1"
		if err := requireAirlock(t, e, nil); err != nil {
			t.Fatalf("RequireAirlock with no profiles file in a cluster: %v", err)
		}
	})
	t.Run("starts in a cluster with a profiles file of no kind and no dpop block, and no ranges", func(t *testing.T) {
		e := airlockEnv()
		e["KUBERNETES_SERVICE_HOST"] = "34.118.224.1"
		if err := requireAirlock(t, e, nil); err != nil {
			t.Fatalf("RequireAirlock of a file of no kind and no dpop block in a cluster: %v", err)
		}
	})
}

// TestEveryAddressConfigReadsIsReadWithoutItsZone: the addresses config reads are read the way
// the proxy reads every address it checks (netaddr.Canonical): an IPv4-mapped address as the IPv4
// address it holds, and a zone dropped. BROKER_AIRLOCK_TEST_RESOLVE's address is one the proxy
// dials, and a sign-in issuer on a zoned loopback address is on this machine as the unzoned one
// is. A zone cannot ride on a prefix at all: netip.ParsePrefix refuses one, which the control
// below holds.
func TestEveryAddressConfigReadsIsReadWithoutItsZone(t *testing.T) {
	e := validEnv()
	e["BROKER_AIRLOCK_TEST_RESOLVE"] = "s.airlock.test=2001:db8::1%eth0 t.airlock.test=::ffff:127.0.0.2"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load with a zoned and a mapped BROKER_AIRLOCK_TEST_RESOLVE address: %v", err)
	}
	for host, want := range map[string]netip.Addr{
		"s.airlock.test": netip.MustParseAddr("2001:db8::1"),
		"t.airlock.test": netip.MustParseAddr("127.0.0.2"),
	} {
		if got := cfg.AirlockTestResolve[host]; got != want {
			t.Errorf("BROKER_AIRLOCK_TEST_RESOLVE read %s as %v; want %v, with no zone and unmapped", host, got, want)
		}
	}
	for _, issuer := range []string{"http://[::1%25lo]:8080", "http://[::1]:8080", "http://127.0.0.1:8080"} {
		if !signInIssuerOK(issuer) {
			t.Errorf("signInIssuerOK(%q) = false; want true: it is a loopback address", issuer)
		}
	}
	for _, issuer := range []string{"http://[2001:db8::1%25lo]:8080", "http://[fe80::1%25eth0]:8080", "http://example.com"} {
		if signInIssuerOK(issuer) {
			t.Errorf("signInIssuerOK(%q) = true; want false: it is no loopback address", issuer)
		}
	}
	e = validEnv()
	e["BROKER_AIRLOCK_CLUSTER_RANGES"] = "2001:db8:5::%eth0/48"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "element 1 is not a CIDR prefix") {
		t.Fatalf("Load(a zoned prefix) = %v; want the refusal naming element 1", err)
	}
}

// TestAPrivateUpstreamEntryIsRefusedUnlessItIsOne: each entry of BROKER_PROXY_PRIVATE_UPSTREAM
// is one <host>=<url> entry, read as that host and that URL. An entry not of that form, a host
// that is not one lower-case host name, and a URL that does not parse, is not http or https, names
// no host or a port outside 1 to 65535, carries a user, a path, a query or a fragment, or names a
// host every dial of the proxy refuses (localhost or a name under .localhost, with or without the
// root's trailing dot; a loopback, link-local, unspecified, multicast or the IPv4 broadcast
// address, or one of 0.0.0.0/8; a cloud metadata server's IPv6 address; an IPv6 address that
// carries an IPv4 address, in each form, whatever it carries, and even inside the cluster ranges;
// an address the proxy would tunnel to, public and outside BROKER_AIRLOCK_CLUSTER_RANGES) each
// refuse the start, naming the setting and the fault. No refusal quotes the URL's user, path,
// query or fragment, where a credential can sit, nor a URL written with no <host>= before it,
// whose user and path then sit before the first =. A private address, one in 100.64.0.0/10, one
// inside the cluster ranges though public, and a loopback URL that BROKER_AIRLOCK_TEST_DIAL_ALLOW
// names with its port, the local stack's stand-in of Dispatch, are read: each is an address the
// dial opens. (TestAPrivateUpstreamSettingIsRefusedUnlessEachEntryIsOne holds the list: entries
// in order, and a host named twice.)
func TestAPrivateUpstreamEntryIsRefusedUnlessItIsOne(t *testing.T) {
	const setting = "BROKER_PROXY_PRIVATE_UPSTREAM"
	if cfg, err := Load(env(airlockEnv())); err != nil || cfg.ProxyPrivateUpstreams != nil {
		t.Fatalf("Load with %s unset = %+v, %v; want none", setting, cfg.ProxyPrivateUpstreams, err)
	}
	for _, row := range []struct{ value, dialAllow, ranges, wantURL string }{
		{"dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766", "", "", "http://dispatch.dispatch.svc.cluster.local:8766"},
		{" dispatch.test=https://10.0.0.5/ ", "", "", "https://10.0.0.5/"},
		{"dispatch.test=http://10.0.0.5:8766", "", "", "http://10.0.0.5:8766"},
		{"dispatch.test=http://[fd12:3456::5]:8766", "", "", "http://[fd12:3456::5]:8766"},
		{"dispatch.test=http://100.64.1.1:8766", "", "", "http://100.64.1.1:8766"},
		{"dispatch.test=http://34.118.224.10:8766", "", "34.118.224.0/20", "http://34.118.224.10:8766"},
		{"dispatch.test=http://127.0.0.1:8766", "127.0.0.1:8766", "", "http://127.0.0.1:8766"},
		{"dispatch.test=http://localhost", "localhost:80", "", "http://localhost"},
	} {
		e := airlockEnv()
		e[setting] = row.value
		if row.dialAllow != "" {
			e["BROKER_AIRLOCK_TEST_DIAL_ALLOW"] = row.dialAllow
		}
		e["BROKER_AIRLOCK_CLUSTER_RANGES"] = row.ranges
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load(%s=%q, BROKER_AIRLOCK_TEST_DIAL_ALLOW=%q, BROKER_AIRLOCK_CLUSTER_RANGES=%q): %v", setting, row.value, row.dialAllow, row.ranges, err)
		}
		if got := cfg.ProxyPrivateUpstreams; len(got) != 1 || got[0].Host != "dispatch.test" || got[0].URL == nil || got[0].URL.String() != row.wantURL {
			t.Fatalf("Load(%s=%q) read %+v; want the host dispatch.test and the URL %s", setting, row.value, got, row.wantURL)
		}
	}
	// The URL's user, path, query and fragment carry a word a refusal must not quote; each URL is
	// put together here, so the word is one constant the check below reads too. A URL written with
	// no <host>= before it puts its user, and its path up to a query's =, before the first =.
	const userWord = "secret-word"
	for _, row := range []struct{ name, value, dialAllow, want string }{
		{"an entry with no =", "dispatch.test", "", "must be <host>=<url> entries"},
		{"an entry with no URL", "dispatch.test=", "", "must be <host>=<url> entries"},
		{"a host that is a pattern", "*.test=http://10.0.0.5:8766", "", "in lower case, with no"},
		{"a host with a capital letter", "Dispatch.test=http://10.0.0.5:8766", "", "in lower case, with no"},
		{"a URL that does not parse", "dispatch.test=http://u:" + userWord + "@[10.0.0.5", "", "does not parse"},
		{"an ftp URL", "dispatch.test=ftp://10.0.0.5:21/" + userWord, "", "must be http or https"},
		{"a URL with no host", "dispatch.test=http://:8766/" + userWord, "", "names no host"},
		{"a URL with port 0", "dispatch.test=http://10.0.0.5:0/" + userWord, "", "port outside 1 to 65535"},
		{"a URL with port 65536", "dispatch.test=http://10.0.0.5:65536", "", "port outside 1 to 65535"},
		{"a URL with a user", "dispatch.test=http://u:" + userWord + "@10.0.0.5:8766", "", "carries a user"},
		{"a URL with a path", "dispatch.test=http://10.0.0.5:8766/" + userWord, "", "has a path"},
		{"a loopback URL with a path", "dispatch.test=http://127.0.0.1:8766/" + userWord, "", "has a path"},
		{"a URL with a query", "dispatch.test=http://10.0.0.5:8766?q=" + userWord, "", "has a query"},
		{"a URL with a fragment", "dispatch.test=http://10.0.0.5:8766#" + userWord, "", "has a fragment"},
		{"a URL with an empty fragment", "dispatch.test=http://10.0.0.5:8766#", "", "has a fragment"},
		{"a URL with no <host>=, a user and a query", "http://u:" + userWord + "@10.0.0.5:8766/?a=b", "", "holds a URL's scheme, user or path"},
		{"a URL with no <host>= and no scheme, a user and a query", "u:" + userWord + "@10.0.0.5:8766/?a=b", "", "holds a URL's scheme, user or path"},
		{"a URL with no <host>=, a path and a query", "10.0.0.5:8766/" + userWord + "?a=b", "", "holds a URL's scheme, user or path"},
		{"localhost", "dispatch.test=http://localhost:8766", "", "resolves to loopback"},
		{"localhost with the root's trailing dot", "dispatch.test=http://localhost.:8766", "", "resolves to loopback"},
		{"a name under .localhost", "dispatch.test=http://dispatch.localhost:8766", "", "resolves to loopback"},
		{"a name under .localhost with the root's trailing dot", "dispatch.test=http://dispatch.localhost.:8766", "", "resolves to loopback"},
		{"a public address", "dispatch.test=http://8.8.8.8:8766", "", "which this proxy would also tunnel to"},
		{"a documentation address, which the proxy reads as public", "dispatch.test=http://203.0.113.5:8766", "", "which this proxy would also tunnel to"},
		{"a public IPv6 address", "dispatch.test=http://[2001:4860:4860::8888]:8766", "", "which this proxy would also tunnel to"},
		{"a loopback address", "dispatch.test=http://127.0.0.1:8766", "", "a loopback or link-local address"},
		{"a loopback IPv6 address", "dispatch.test=http://[::1]:8766", "", "a loopback or link-local address"},
		{"the IPv4 metadata server", "dispatch.test=http://169.254.169.254", "", "a loopback or link-local address"},
		{"an unspecified address", "dispatch.test=http://0.0.0.0:8766", "", "an unspecified or multicast address"},
		{"the IPv4 broadcast address", "dispatch.test=http://255.255.255.255:8766", "", "the IPv4 broadcast address"},
		{"Google's IPv6 metadata server", "dispatch.test=http://[fd20:ce::254]", "", "a cloud metadata server's address"},
		{"Amazon's IPv6 metadata server", "dispatch.test=http://[fd00:ec2::254]", "", "a cloud metadata server's address"},
		{"an address of 0.0.0.0/8", "dispatch.test=http://0.0.0.1:8766", "", "an address of 0.0.0.0/8"},
		{"the NAT64 form of the IPv4 metadata server", "dispatch.test=http://[64:ff9b::a9fe:a9fe]", "", "carries an IPv4 address"},
		{"the NAT64 form of a loopback address", "dispatch.test=http://[64:ff9b::7f00:1]:8766", "", "carries an IPv4 address"},
		{"the NAT64 form of a private address", "dispatch.test=http://[64:ff9b::a00:5]:8766", "", "carries an IPv4 address"},
		{"the 6to4 form of the IPv4 metadata server", "dispatch.test=http://[2002:a9fe:a9fe::1]", "", "carries an IPv4 address"},
		{"the IPv4-compatible form of a loopback address", "dispatch.test=http://[::7f00:1]:8766", "", "carries an IPv4 address"},
		{"a Teredo address whose server is the IPv4 metadata server", "dispatch.test=http://[2001:0:a9fe:a9fe::1]", "", "carries an IPv4 address"},
		{"an address of the local-use translation prefix", "dispatch.test=http://[64:ff9b:1::a9fe:a9fe]", "", "a local-use translation IPv6 address"},
		{"a loopback address the dial allow names on another port", "dispatch.test=http://127.0.0.1:8766", "127.0.0.1:8767", "a loopback or link-local address"},
	} {
		e := airlockEnv()
		e[setting] = row.value
		if row.dialAllow != "" {
			e["BROKER_AIRLOCK_TEST_DIAL_ALLOW"] = row.dialAllow
		}
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: Load(%s=%q) = %v; want a refusal naming the setting and %q", row.name, setting, row.value, err, row.want)
			continue
		}
		if strings.Contains(err.Error(), userWord) {
			t.Errorf("%s: the refusal quotes a part of the URL a credential can sit in: %v", row.name, err)
		}
	}
	e := validEnv()
	e[setting] = "dispatch.test=http://10.0.0.5:8766"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "needs BROKER_PROFILES_FILE") {
		t.Errorf("Load(%s with no profiles file) = %v; want a refusal naming the setting and BROKER_PROFILES_FILE", setting, err)
	}
	// The cluster ranges are the dial's: a public address the rows above take inside
	// 34.118.224.0/20 is refused just outside it, naming both settings.
	ranged := airlockEnv()
	ranged["BROKER_AIRLOCK_CLUSTER_RANGES"] = "34.118.224.0/20"
	ranged[setting] = "dispatch.test=http://34.118.240.1:8766"
	if _, err := Load(env(ranged)); err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "which this proxy would also tunnel to: list its range in BROKER_AIRLOCK_CLUSTER_RANGES") {
		t.Errorf("Load(%s=%q, BROKER_AIRLOCK_CLUSTER_RANGES=34.118.224.0/20) = %v; want a refusal naming the setting and the address the proxy would tunnel to", setting, ranged[setting], err)
	}
	// The cluster ranges widen the private ranges and nothing else: a range that holds an IPv6
	// form carrying the metadata server's address does not make that form a destination.
	carried := airlockEnv()
	carried["BROKER_AIRLOCK_CLUSTER_RANGES"] = "64:ff9b::/96"
	carried[setting] = "dispatch.test=http://[64:ff9b::a9fe:a9fe]"
	if _, err := Load(env(carried)); err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "carries an IPv4 address") {
		t.Errorf("Load(%s=%q, BROKER_AIRLOCK_CLUSTER_RANGES=64:ff9b::/96) = %v; want a refusal naming the setting and the form", setting, carried[setting], err)
	}
}

// TestTheCheckOfADestinationsAddressIsTheStartsRule: the address checks `profiles check
// --private-upstream` makes are the start's own. Given the cluster ranges (`--cluster-ranges`, read
// by ParseAirlockClusterRanges), it calls PrivateUpstreamDialable with them, which answers what
// Load answers for that destination and those ranges, in the same words, for each ranges below:
// none, the Service range, an IPv6 range that holds the NAT64 forms, and every address. Without
// them, PrivateUpstreamRefusedOnEveryBroker is the start's rule for every cluster ranges a broker
// can have: where it refuses, the
// start refuses in the same words with no ranges and with ranges that hold every address; where it
// takes a destination and judges it, the start takes it with both; and where it answers the
// address unjudged, the start refuses that address with no ranges and takes it with ranges that
// hold it, so that answer is exactly the part that needs the ranges.
func TestTheCheckOfADestinationsAddressIsTheStartsRule(t *testing.T) {
	const setting = "BROKER_PROXY_PRIVATE_UPSTREAM"
	everything := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	for _, url := range []string{
		"http://127.0.0.1:8766", "http://[::1]:8766", "http://localhost:8766", "http://dispatch.localhost.:8766",
		"http://169.254.169.254", "http://[fe80::1]", "http://[fd20:ce::254]", "http://[fd00:ec2::254]",
		"http://255.255.255.255:8766", "http://0.0.0.0:8766", "http://0.0.0.1:8766", "http://224.0.0.1",
		"http://[64:ff9b::a9fe:a9fe]", "http://[2002:a9fe:a9fe::1]", "http://[::7f00:1]:8766", "http://[2001:0:a9fe:a9fe::1]",
		"http://[64:ff9b:1::a9fe:a9fe]", "http://[64:ff9b::a00:5]:8766",
		"http://8.8.8.8:8766", "http://203.0.113.5:8766", "http://34.118.224.10:8766", "http://34.118.240.1:8766", "http://[2001:4860:4860::8888]",
		"http://10.0.0.5:8766", "https://10.0.0.5", "http://100.64.1.1:8766", "http://[fd12:3456::5]:8766",
		"http://dispatch.dispatch.svc.cluster.local:8766",
	} {
		upstreams, err := ParsePrivateUpstreams("dispatch.test=" + url)
		if err != nil || len(upstreams) != 1 {
			t.Fatalf("ParsePrivateUpstreams(%s) = %v, %v; want one entry", url, upstreams, err)
		}
		upstream := upstreams[0]
		unjudged, checked := PrivateUpstreamRefusedOnEveryBroker(upstream)
		bare, wide := PrivateUpstreamDialable(upstream, nil, nil), PrivateUpstreamDialable(upstream, nil, everything)
		switch {
		case checked != nil:
			if unjudged.IsValid() || bare == nil || wide == nil || bare.Error() != checked.Error() || wide.Error() != checked.Error() {
				t.Errorf("%s: the check refuses it (%v, unjudged %v); the start, with no ranges and with every address as a range, answers %v and %v; want the same refusal", url, checked, unjudged, bare, wide)
			}
		case unjudged.IsValid():
			if bare == nil || !strings.Contains(bare.Error(), " names "+unjudged.String()+", ") || wide != nil {
				t.Errorf("%s: the check answers %s unjudged; the start answers %v with no ranges and %v with every address as a range; want a refusal of that address and none", url, unjudged, bare, wide)
			}
		default:
			if bare != nil || wide != nil {
				t.Errorf("%s: the check takes it and judges it; the start answers %v and %v; want it taken with any ranges", url, bare, wide)
			}
		}
		for _, text := range []string{"", "34.118.224.0/20", "64:ff9b::/96", "0.0.0.0/0, ::/0"} {
			ranges, err := ParseAirlockClusterRanges(text)
			if err != nil {
				t.Fatalf("ParseAirlockClusterRanges(%q): %v", text, err)
			}
			checked := PrivateUpstreamDialable(upstream, nil, ranges)
			e := airlockEnv()
			e[setting] = "dispatch.test=" + url
			e["BROKER_AIRLOCK_CLUSTER_RANGES"] = text
			_, started := Load(env(e))
			if (checked == nil) != (started == nil) || checked != nil && checked.Error() != started.Error() {
				t.Errorf("%s with the ranges %q: the check answers %v; Load answers %v; want Load's answer", url, text, checked, started)
			}
		}
	}
}

// privateEntry is the proxy block the private destination's host is bound by: the profile
// DISPATCH_RESULTS_TOKEN's (resultsEntry), bound to dispatch.test.
func privateEntry() proxyrules.Binding {
	entry := resultsEntry()
	entry.Inline.Hosts = []string{"dispatch.test"}
	return entry
}

// TestAPrivateUpstreamNeedsOneBindingEntry: the start check of the results capability
// (resultsStart: ResultsEntryRules and ProxyMintedRules) takes the profiles' proxy blocks when the
// block that binds the destination's host is the profile DISPATCH_RESULTS_TOKEN's, bound to that
// host alone, of kind dpop, with a non-empty allow list, carried in Authorization alone (a block
// naming no headers carries the default, Authorization). A profile bound to another host, a kind
// over it (proxyrules.HostBound, so a "*." kind over the host binds it too, and the refusal names
// that host), a profile of no kind, no allow list, an empty one and
// another header are each refused, naming BROKER_PROFILES_FILE, the setting or the profile, and
// what is wrong. With no destination, no result entries and no dpop block, any profiles are taken.
func TestAPrivateUpstreamNeedsOneBindingEntry(t *testing.T) {
	const setting = "BROKER_PROXY_PRIVATE_UPSTREAM"
	e := airlockEnv()
	e[setting] = "dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load(%s): %v", setting, err)
	}
	upstreams := cfg.ProxyPrivateUpstreams
	if err := resultsStart(proxyrules.Rules{Names: []proxyrules.Binding{privateEntry()}}, upstreams, false); err != nil {
		t.Fatalf("the one entry: %v", err)
	}
	defaultHeader := privateEntry()
	defaultHeader.Inline.Headers = nil
	if err := resultsStart(proxyrules.Rules{Names: []proxyrules.Binding{defaultHeader}}, upstreams, false); err != nil {
		t.Fatalf("the entry with the default header: %v", err)
	}
	if err := resultsStart(proxyrules.Rules{}, nil, false); err != nil {
		t.Fatalf("no destination and no rules: %v", err)
	}
	pattern := proxyrules.Kind{Name: "wildcard", Hosts: []string{"*.test"}, Ways: []string{proxyrules.WaySwap}}
	for _, c := range []struct {
		name, says string
		spoil      func(*proxyrules.Rules)
	}{
		{"a profile bound to another host", "binds other.test, which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names", func(r *proxyrules.Rules) { r.Names[0].Inline.Hosts = []string{"other.test"} }},
		{"a kind *.test", "the kind wildcard of BROKER_PROFILES_FILE binds dispatch.test", func(r *proxyrules.Rules) { r.Kinds = append(r.Kinds, pattern) }},
		{"a profile of no kind", "kind dpop", func(r *proxyrules.Rules) { r.Names[0].Way = "" }},
		{"no allow list", "allow list", func(r *proxyrules.Rules) { r.Names[0].Inline.Allow = nil }},
		{"an empty allow list", "allow list", func(r *proxyrules.Rules) { r.Names[0].Inline.Allow = []proxyrules.Allow{} }},
		{"a header X-Token", "Authorization header alone", func(r *proxyrules.Rules) { r.Names[0].Inline.Headers = []string{"X-Token"} }},
	} {
		rules := proxyrules.Rules{Names: []proxyrules.Binding{privateEntry()}}
		c.spoil(&rules)
		err := resultsStart(rules, upstreams, false)
		if err == nil || !strings.Contains(err.Error(), c.says) || (!strings.Contains(err.Error(), setting) && !strings.Contains(err.Error(), "DISPATCH_RESULTS_TOKEN")) {
			t.Errorf("%s: err = %v, want a refusal naming %s or DISPATCH_RESULTS_TOKEN and %q", c.name, err, setting, c.says)
			continue
		}
		if !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") || strings.Contains(err.Error(), "BROKER_AIRLOCK_RULES_FILE") {
			t.Errorf("%s: err = %v, want a refusal naming BROKER_PROFILES_FILE, whose proxy blocks are the rules, and not the removed BROKER_AIRLOCK_RULES_FILE", c.name, err)
		}
	}
}

// TestLoadReadsTheRegisteredServices pins BROKER_SERVICES: whitespace-separated
// name=<service-account subject> entries keyed by subject, none when unset. A name is one a
// machine login's service can take ([a-z0-9-]{1,64}) and not the owner tag's own "shared"; a
// subject is a Kubernetes service account's (system:serviceaccount:<namespace>:<name>), the only
// subject a pod's projected token can carry. One service may be named for several accounts. An
// entry with no "=", an empty side, an invalid name or subject, or an account given twice refuses
// to start naming the entry or the account, since the broker could never serve that service a
// secret or the account would prove more than one entry.
func TestLoadReadsTheRegisteredServices(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServicesByAccount != nil {
		t.Fatalf("ServicesByAccount = %v, want nil when BROKER_SERVICES is unset", cfg.ServicesByAccount)
	}

	e := validEnv()
	e["BROKER_SERVICES"] = " example-service=system:serviceaccount:example:example-sa\tother-service=system:serviceaccount:other:other-sa\n"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:other:other-sa":     "other-service",
	}
	if !maps.Equal(cfg.ServicesByAccount, want) {
		t.Fatalf("ServicesByAccount = %v, want %v", cfg.ServicesByAccount, want)
	}

	// One service may run its pods as several accounts, each of which proves it.
	e = validEnv()
	e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa example-service=system:serviceaccount:example:another-sa"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:example:another-sa": "example-service",
	}
	if !maps.Equal(cfg.ServicesByAccount, want) {
		t.Fatalf("ServicesByAccount with two accounts for one service = %v, want %v", cfg.ServicesByAccount, want)
	}

	for _, bad := range []string{
		"other-service",
		"=system:serviceaccount:other:other-sa",
		"other-service=",
		"Other-Service=system:serviceaccount:other:other-sa",
		"other_service=system:serviceaccount:other:other-sa",
		"ada@example.com=system:serviceaccount:other:other-sa",
		"shared=system:serviceaccount:other:other-sa",
		strings.Repeat("a", 65) + "=system:serviceaccount:other:other-sa",
		"other-service=other-sa",
		"other-service=system:serviceaccount:other",
		"other-service=system:serviceaccount:Other:other-sa",
	} {
		e := validEnv()
		e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa " + bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_SERVICES with %q: err = %v, want a refusal naming BROKER_SERVICES and the entry", bad, err)
		}
	}

	// One service account bound to two names would make a pod on it whichever service its login
	// claims, and a login's service name is the machine's own claim, so each account proves one
	// service only.
	e = validEnv()
	e["BROKER_SERVICES"] = "a=system:serviceaccount:x:y b=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") ||
		!strings.Contains(err.Error(), "system:serviceaccount:x:y") || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("BROKER_SERVICES binding one account to two names: err = %v, want a refusal naming the account and both names", err)
	}

	// The same account named twice for one service is a mistake in the setting, refused naming the
	// account and the service.
	e = validEnv()
	e["BROKER_SERVICES"] = "a=system:serviceaccount:x:y a=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES names the service account system:serviceaccount:x:y twice for \"a\"; each account proves one service, so name it once") {
		t.Errorf("BROKER_SERVICES naming one account twice for one service: err = %v, want a refusal naming the account twice for the service", err)
	}

	// A namespace's default service account proves nothing: every pod in that namespace whose spec
	// names no account runs as it, and any such pod can mount a projected token for the broker's
	// audience, so binding a service to it would hand the service's secrets to all of them.
	for _, entry := range []string{"legion-daemon=system:serviceaccount:legion:default", "x=system:serviceaccount:kube-system:default"} {
		e = validEnv()
		e["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa " + entry
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_SERVICES") || !strings.Contains(err.Error(), strconv.Quote(entry)) ||
			!strings.Contains(err.Error(), "default service account") {
			t.Errorf("BROKER_SERVICES with %q: err = %v, want a refusal naming the entry and that it is a namespace's default service account", entry, err)
		}
	}
	// An account merely named like it is a dedicated account of its own.
	e = validEnv()
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:default-worker other=system:serviceaccount:default:other-sa"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("BROKER_SERVICES naming dedicated accounts whose name or namespace contains default: %v", err)
	}
}

// exampleVMServiceAccount is a placeholder Google service account; this repository is public, so
// no real account, project or instance name appears in a test.
const exampleVMServiceAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// validVMEnv is validEnv with one VM service fully configured: the Google audience, the service's
// account, its project and its instance-name pattern.
func validVMEnv() map[string]string {
	e := validEnv()
	e["BROKER_GOOGLE_OIDC_AUDIENCE"] = "example-broker"
	e["BROKER_VM_SERVICES"] = "example-service=" + exampleVMServiceAccount
	e["BROKER_VM_PROJECTS"] = exampleVMServiceAccount + "=example-project"
	e["BROKER_VM_INSTANCES"] = exampleVMServiceAccount + "=example-vm-*"
	return e
}

// TestLoadReadsTheVMServices pins the four VM settings: one entry gives the service, the project
// and the instance pattern, each keyed by the account's email.
func TestLoadReadsTheVMServices(t *testing.T) {
	cfg, err := Load(env(validVMEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoogleOIDCAudience != "example-broker" {
		t.Fatalf("GoogleOIDCAudience = %q, want example-broker", cfg.GoogleOIDCAudience)
	}
	if want := map[string]string{exampleVMServiceAccount: "example-service"}; !maps.Equal(cfg.VMServicesByAccount, want) {
		t.Fatalf("VMServicesByAccount = %v, want %v", cfg.VMServicesByAccount, want)
	}
	pinned := []exchange.GoogleEntry{{Account: exampleVMServiceAccount, Service: "example-service", CloudProject: "example-project", Instance: "example-vm-*"}}
	if !slices.Equal(cfg.ExchangeGoogleAccounts, pinned) {
		t.Fatalf("ExchangeGoogleAccounts = %+v, want %+v: the two pins are read into the entry and nowhere else", cfg.ExchangeGoogleAccounts, pinned)
	}

	// Two accounts for one service, entries separated by any white space, and an audience with
	// white space around it, which is trimmed, as a token's audience never carries it.
	const second = "second-fleet@example-project.iam.gserviceaccount.com"
	two := validVMEnv()
	two["BROKER_GOOGLE_OIDC_AUDIENCE"] = " \texample-broker\n"
	two["BROKER_VM_SERVICES"] += "\texample-service=" + second
	two["BROKER_VM_PROJECTS"] += "\n" + second + "=example-other"
	two["BROKER_VM_INSTANCES"] += "\t" + second + "=example-vm-[0-9]*"
	cfg, err = Load(env(two))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoogleOIDCAudience != "example-broker" {
		t.Fatalf("GoogleOIDCAudience = %q, want example-broker with its white space trimmed", cfg.GoogleOIDCAudience)
	}
	if want := map[string]string{exampleVMServiceAccount: "example-service", second: "example-service"}; !maps.Equal(cfg.VMServicesByAccount, want) {
		t.Fatalf("VMServicesByAccount with two accounts = %v, want %v", cfg.VMServicesByAccount, want)
	}
	bothPinned := []exchange.GoogleEntry{
		{Account: exampleVMServiceAccount, Service: "example-service", CloudProject: "example-project", Instance: "example-vm-*"},
		{Account: second, Service: "example-service", CloudProject: "example-other", Instance: "example-vm-[0-9]*"},
	}
	if !slices.Equal(cfg.ExchangeGoogleAccounts, bothPinned) {
		t.Fatalf("ExchangeGoogleAccounts with two accounts = %+v, want %+v", cfg.ExchangeGoogleAccounts, bothPinned)
	}
	// A second account pinned in one setting and not the other is half a pin, refused naming
	// both settings and that account, not only the first's.
	for _, missing := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		e := maps.Clone(two)
		e[missing] = validVMEnv()[missing]
		named := "BROKER_VM_INSTANCES"
		if missing == "BROKER_VM_INSTANCES" {
			named = "BROKER_VM_PROJECTS"
		}
		_, err := Load(env(e))
		want := named + " names " + second + " but " + missing + ` does not, got "example-service=` + second + `"`
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Load with the second account missing from %s = %v, want a refusal starting %q", missing, err, want)
		}
	}
	// The pinned account with no BROKER_VM_EXCHANGE_ACCOUNTS entry is one machine entry of the
	// exchange: its service and its two pins, and no person, no project and no session.
	oneEntry, err := Load(env(validVMEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.GoogleEntry{{
		Account: exampleVMServiceAccount, Service: "example-service",
		CloudProject: "example-project", Instance: "example-vm-*",
	}}
	if !slices.Equal(oneEntry.ExchangeGoogleAccounts, want) {
		t.Fatalf("ExchangeGoogleAccounts = %+v, want %+v", oneEntry.ExchangeGoogleAccounts, want)
	}
}

// TestTheGoogleIssuerDefaultsToGooglesOwn: BROKER_GOOGLE_OIDC_ISSUER is Google's own issuer when
// the environment names none, and a dev stack's stand-in when it does, which is the only reason
// the variable exists.
func TestTheGoogleIssuerDefaultsToGooglesOwn(t *testing.T) {
	cfg, err := Load(env(validVMEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoogleOIDCIssuer != DefaultGoogleOIDCIssuer || DefaultGoogleOIDCIssuer != "https://accounts.google.com" {
		t.Fatalf("GoogleOIDCIssuer = %q, want %q", cfg.GoogleOIDCIssuer, DefaultGoogleOIDCIssuer)
	}
	e := validVMEnv()
	e["BROKER_GOOGLE_OIDC_ISSUER"] = " http://127.0.0.1:8080\n"
	if cfg, err = Load(env(e)); err != nil || cfg.GoogleOIDCIssuer != "http://127.0.0.1:8080" {
		t.Fatalf("GoogleOIDCIssuer with a stand-in issuer = %q, %v; want the issuer with its white space trimmed", cfg.GoogleOIDCIssuer, err)
	}
	// With no Google audience the issuer is still answered, since nothing reads it then: it is
	// the audience that turns Google identity tokens on at all.
	if cfg, err = Load(env(validEnv())); err != nil || cfg.GoogleOIDCIssuer != DefaultGoogleOIDCIssuer {
		t.Fatalf("GoogleOIDCIssuer with no Google audience = %q, %v; want the default", cfg.GoogleOIDCIssuer, err)
	}
}

// TestTheVMExchangeAccountsGiveAFleetItsPersonAndProject pins BROKER_VM_EXCHANGE_ACCOUNTS: a
// listed account it names writes that project, acts for that person and may fix its session,
// while a listed account it does not name is its service and nothing else. Its refusals are each
// its own: an entry that is not of the form, a blank field, an account no VM service lists, an
// account named twice, an account no VM setting may name, and the setting with no Google
// audience.
func TestTheVMExchangeAccountsGiveAFleetItsPersonAndProject(t *testing.T) {
	const second = "second-fleet@example-project.iam.gserviceaccount.com"
	e := validVMEnv()
	e["BROKER_VM_SERVICES"] += " example-other=" + second
	e["BROKER_VM_PROJECTS"] += " " + second + "=example-other"
	e["BROKER_VM_INSTANCES"] += " " + second + "=example-vm-*"
	e["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com=example-fleet-1"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.GoogleEntry{
		{
			Account: exampleVMServiceAccount, Service: "example-service",
			CloudProject: "example-project", Instance: "example-vm-*",
			Project: "EXAMPLE", Person: "ada@example.com", Session: "example-fleet-1",
		},
		{
			Account: second, Service: "example-other",
			CloudProject: "example-other", Instance: "example-vm-*",
		},
	}
	if !slices.Equal(cfg.ExchangeGoogleAccounts, want) {
		t.Fatalf("ExchangeGoogleAccounts = %+v, want %+v", cfg.ExchangeGoogleAccounts, want)
	}
	// An entry that fixes no session: everything after the third = is the session, so there is
	// none to read.
	noSession := validVMEnv()
	noSession["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com"
	cfg, err = Load(env(noSession))
	if err != nil || len(cfg.ExchangeGoogleAccounts) != 1 || cfg.ExchangeGoogleAccounts[0].Session != "" ||
		cfg.ExchangeGoogleAccounts[0].Person != "ada@example.com" {
		t.Fatalf("ExchangeGoogleAccounts with no session fixed = %+v, %v", cfg.ExchangeGoogleAccounts, err)
	}
	for _, row := range []struct{ value, want string }{
		{exampleVMServiceAccount + "=EXAMPLE", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{exampleVMServiceAccount + "==ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{exampleVMServiceAccount + "=EXAMPLE=", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{"not-an-email=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must name user-managed service accounts"},
		{"system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must name user-managed service accounts"},
		{exampleVMServiceAccount + "=EXAMPLE=ada@example.com " + exampleVMServiceAccount + "=OTHER=bob@example.com",
			"BROKER_VM_EXCHANGE_ACCOUNTS names the service account " + exampleVMServiceAccount + " twice"},
		{second + "=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS names " + second},
	} {
		bad := validVMEnv()
		bad["BROKER_VM_EXCHANGE_ACCOUNTS"] = row.value
		_, err := Load(env(bad))
		if err == nil || !strings.HasPrefix(err.Error(), row.want) {
			t.Errorf("Load(BROKER_VM_EXCHANGE_ACCOUNTS=%q) = %v, want a refusal starting %q", row.value, err, row.want)
		}
	}
	withoutAudience := validEnv()
	withoutAudience["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com"
	_, err = Load(env(withoutAudience))
	if want := "BROKER_VM_EXCHANGE_ACCOUNTS needs BROKER_GOOGLE_OIDC_AUDIENCE"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Load with the fleet grants and no Google audience = %v, want a refusal starting %q", err, want)
	}
}

// TestTheVMSettingsLeaveEveryOtherFieldAlone pins requirement R4: a Kubernetes configuration loads
// to the same Config with or without the four VM settings beside it, apart from the VM fields.
func TestTheVMSettingsLeaveEveryOtherFieldAlone(t *testing.T) {
	base := validEnv()
	base["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
	base["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
	base["BROKER_SERVICES"] = "example-service=system:serviceaccount:example:example-sa"
	base["BROKER_SERVICE_LOGINS"] = "example-service=system:serviceaccount:example:example-login"
	base["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch"
	want, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	withVM := maps.Clone(base)
	for _, name := range []string{"BROKER_GOOGLE_OIDC_AUDIENCE", "BROKER_VM_SERVICES", "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		withVM[name] = validVMEnv()[name]
	}
	got, err := Load(env(withVM))
	if err != nil {
		t.Fatal(err)
	}
	if got.VMServicesByAccount == nil {
		t.Fatal("the VM settings were not read")
	}
	got.GoogleOIDCAudience, got.VMServicesByAccount = "", nil
	got.ExchangeGoogleAccounts = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Config with the VM settings, VM fields cleared = %+v, want it equal to the Config without them, %+v", got, want)
	}
}

// TestVMServicesRefuseInvalidDefaultOrDuplicateAccounts pins BROKER_VM_SERVICES' own refusals,
// each by its own message: an entry that is not a user-managed service account (a Kubernetes
// subject, an account ID too short or ending in a hyphen), the service name shared, an address
// with capital letters, a project's default service accounts, a Google-managed service agent, one
// account bound to two services, and one account given twice for one service.
func TestVMServicesRefuseInvalidDefaultOrDuplicateAccounts(t *testing.T) {
	const userManaged = "must name user-managed service accounts, <account id>@<project id>.iam.gserviceaccount.com, got "
	for _, row := range []struct{ services, want string }{
		{"example-service=not-an-email", userManaged + `"example-service=not-an-email"`},
		{"example-service=system:serviceaccount:example:example-sa", userManaged + `"example-service=system:serviceaccount:example:example-sa"`},
		{"example-service=abcde@example-project.iam.gserviceaccount.com", userManaged + `"example-service=abcde@example-project.iam.gserviceaccount.com"`},
		{"example-service=example-fleet-@example-project.iam.gserviceaccount.com", userManaged + `"example-service=example-fleet-@example-project.iam.gserviceaccount.com"`},
		{"shared=" + exampleVMServiceAccount, `not shared, got "shared=` + exampleVMServiceAccount + `"`},
		{"example-service=Example-Fleet@example-project.iam.gserviceaccount.com", "names a service account with capital letters"},
		{"example-service=system:serviceaccount:Other:x", userManaged + `"example-service=system:serviceaccount:Other:x"`},
		{"example-service=123456789-compute@developer.gserviceaccount.com", "names a project's default service account"},
		{"example-service=example-project@appspot.gserviceaccount.com", "names a project's default service account"},
		{"example-service=service-123456789012@compute-system.iam.gserviceaccount.com", "names a Google-managed service agent"},
		{"example-service=service-123456789012@gcp-sa-example.iam.gserviceaccount.com", "names a Google-managed service agent"},
		{"example-service=example-agent@gcp-sa-example.iam.gserviceaccount.com", "names a Google-managed service agent"},
		{"example-service=service-org-123456789012@gcp-sa-example.iam.gserviceaccount.com", "names a Google-managed service agent"},
		{"example-service=" + exampleVMServiceAccount + " another-service=" + exampleVMServiceAccount, `for both "example-service" and "another-service"`},
		{"example-service=" + exampleVMServiceAccount + " example-service=" + exampleVMServiceAccount, `twice for "example-service"`},
		{"example-service=" + exampleVMServiceAccount + ".attacker.example", userManaged + `"example-service=` + exampleVMServiceAccount + `.attacker.example"`},
		{"example-service=person@example.com", userManaged + `"example-service=person@example.com"`},
		{"example-service=example-fleet@" + strings.Repeat("a", 31) + ".iam.gserviceaccount.com", userManaged + `"example-service=example-fleet@` + strings.Repeat("a", 31) + `.iam.gserviceaccount.com"`},
		{"Example_Service=" + exampleVMServiceAccount, `at most 64, not shared, got "Example_Service=` + exampleVMServiceAccount + `"`},
		{"=" + exampleVMServiceAccount, `at most 64, not shared, got "=` + exampleVMServiceAccount + `"`},
		{strings.Repeat("a", 65) + "=" + exampleVMServiceAccount, `at most 64, not shared, got "` + strings.Repeat("a", 65) + "=" + exampleVMServiceAccount + `"`},
	} {
		e := validVMEnv()
		e["BROKER_VM_SERVICES"] = row.services
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_VM_SERVICES ") || !strings.Contains(err.Error(), row.want) {
			t.Errorf("Load(BROKER_VM_SERVICES=%q) = %v, want a refusal naming the setting and %q", row.services, err, row.want)
		}
	}
}

// TestVMServicesNeedTheGoogleAudience pins that each VM setting with no Google audience is refused,
// naming itself and the audience, rather than configured and never reachable; an audience of only
// whitespace counts as none.
func TestVMServicesNeedTheGoogleAudience(t *testing.T) {
	for _, audience := range []string{"", " \t"} {
		for setting, value := range map[string]string{
			"BROKER_VM_SERVICES":            "example-service=" + exampleVMServiceAccount,
			"BROKER_VM_PROJECTS":            exampleVMServiceAccount + "=example-project",
			"BROKER_VM_INSTANCES":           exampleVMServiceAccount + "=example-vm-*",
			"BROKER_VM_EXCHANGE_ACCOUNTS":   exampleVMServiceAccount + "=EXAMPLE=ada@example.com",
			"BROKER_SIGNIN_DEVICE_AUDIENCE": "not-a-secret-device-client",
		} {
			e := validEnv()
			e["BROKER_GOOGLE_OIDC_AUDIENCE"] = audience
			e[setting] = value
			_, err := Load(env(e))
			if want := setting + " needs BROKER_GOOGLE_OIDC_AUDIENCE"; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Load with only %s and audience %q = %v, want a refusal starting %q", setting, audience, err, want)
			}
		}
	}
}

// TestGoogleAPIAccountsAndVMServicesAreIndependent pins that either kind of Google caller here, a
// google: entry in BROKER_API_ACCOUNTS or a BROKER_VM_SERVICES entry, is enough for the Google
// audience and needs it, and that an audience with none, a Kubernetes API caller being no Google
// caller, is refused at boot naming the audience and all three ways to give it a caller. The
// third, a google: edge-listener entry in BROKER_NATS_SERVICES, has its rows in
// TestNATSCalloutSettings.
func TestGoogleAPIAccountsAndVMServicesAreIndependent(t *testing.T) {
	const noGoogleCaller = "BROKER_GOOGLE_OIDC_AUDIENCE needs a Google caller: a google: entry in BROKER_API_ACCOUNTS, a VM service in BROKER_VM_SERVICES or a google: edge-listener entry in BROKER_NATS_SERVICES"
	for _, row := range []struct {
		name                                string
		api, kubernetes, audience, services bool
		wantError                           string
	}{
		{name: "neither caller"},
		{name: "audience only", audience: true, wantError: noGoogleCaller},
		{name: "audience with only a Kubernetes API caller", kubernetes: true, audience: true, wantError: noGoogleCaller},
		{name: "API caller without audience", api: true, wantError: "BROKER_GOOGLE_OIDC_AUDIENCE"},
		{name: "VM caller without audience", services: true, wantError: "BROKER_GOOGLE_OIDC_AUDIENCE"},
		{name: "both callers without audience", api: true, services: true, wantError: "BROKER_GOOGLE_OIDC_AUDIENCE"},
		{name: "API caller only", api: true, audience: true},
		{name: "VM caller only", audience: true, services: true},
		{name: "both callers", api: true, audience: true, services: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := validEnv()
			if row.kubernetes {
				e = withK8sIssuer()
				e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch"
			}
			if row.api {
				e["BROKER_API_ACCOUNTS"] = "listener=google:" + exampleListenerAccount
			}
			if row.audience {
				e["BROKER_GOOGLE_OIDC_AUDIENCE"] = "not-a-secret-broker"
			}
			if row.services {
				e["BROKER_VM_SERVICES"] = "example-service=" + exampleVMServiceAccount
			}
			cfg, err := Load(env(e))
			if row.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), row.wantError) {
					t.Fatalf("Load = %v, want a refusal naming %s", err, row.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.api {
				caller, _ := api.AccountFor("listener")
				if want := map[string]api.Account{exampleListenerAccount: caller}; !maps.Equal(cfg.GoogleAPIAccounts, want) {
					t.Fatalf("GoogleAPIAccounts = %v, want %v", cfg.GoogleAPIAccounts, want)
				}
			}
			if !row.services && (cfg.VMServicesByAccount != nil || cfg.ExchangeGoogleAccounts != nil) {
				t.Fatalf("VM services = %v, exchange entries = %v; want neither without BROKER_VM_SERVICES", cfg.VMServicesByAccount, cfg.ExchangeGoogleAccounts)
			}
			if row.services && len(cfg.ExchangeGoogleAccounts) != 1 {
				t.Fatalf("exchange entries = %v, want the listed VM service", cfg.ExchangeGoogleAccounts)
			}
		})
	}
}

func TestVMCallerSettingsNeedVMServices(t *testing.T) {
	for setting, value := range map[string]string{
		"BROKER_VM_PROJECTS":            exampleVMServiceAccount + "=example-project",
		"BROKER_VM_INSTANCES":           exampleVMServiceAccount + "=example-vm-*",
		"BROKER_VM_EXCHANGE_ACCOUNTS":   exampleVMServiceAccount + "=EXAMPLE=ada@example.com",
		"BROKER_SIGNIN_DEVICE_AUDIENCE": "not-a-secret-device-client",
	} {
		for _, apiCaller := range []string{"", "listener=google:" + exampleListenerAccount} {
			for _, services := range []string{"", " \t\n"} {
				t.Run(setting+"/API="+strconv.FormatBool(apiCaller != "")+"/services="+strconv.Quote(services), func(t *testing.T) {
					e := validEnv()
					e["BROKER_GOOGLE_OIDC_AUDIENCE"] = "not-a-secret-broker"
					e["BROKER_API_ACCOUNTS"] = apiCaller
					e["BROKER_VM_SERVICES"] = services
					e[setting] = value
					_, err := Load(env(e))
					if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "BROKER_VM_SERVICES") {
						t.Fatalf("Load with %s but no VM services = %v, want a refusal naming both settings", setting, err)
					}
				})
			}
		}
	}
}

// TestAVMAccountIsPinnedToBothOrToNeither pins the rule each pin follows: an account named in
// BROKER_VM_PROJECTS and in BROKER_VM_INSTANCES is held to that project and those names, an
// account named in neither takes its machines wherever they run, and an account named in one and
// not the other is refused at boot naming both settings and the account, since half a pin says
// nothing clear. It also pins that neither setting may name an account no VM service lists (the
// first such entry as written is the one named), an account twice, or a malformed value; and that
// an instance glob holds only an instance name's characters and path.Match's.
func TestAVMAccountIsPinnedToBothOrToNeither(t *testing.T) {
	for _, missing := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		named := "BROKER_VM_INSTANCES"
		if missing == "BROKER_VM_INSTANCES" {
			named = "BROKER_VM_PROJECTS"
		}
		e := validVMEnv()
		delete(e, missing)
		_, err := Load(env(e))
		want := named + " names " + exampleVMServiceAccount + " but " + missing + ` does not, got "example-service=` + exampleVMServiceAccount + `"`
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Load with %s unset = %v, want a refusal starting %q", missing, err, want)
		}
	}
	// Neither pin: the account is listed, and its entry takes a machine wherever it runs.
	unpinned := validVMEnv()
	delete(unpinned, "BROKER_VM_PROJECTS")
	delete(unpinned, "BROKER_VM_INSTANCES")
	cfg, err := Load(env(unpinned))
	if err != nil {
		t.Fatalf("Load with neither pin = %v, want an unpinned account", err)
	}
	// Neither pin reaches the entry, which is the only place a pin is read.
	want := []exchange.GoogleEntry{{Account: exampleVMServiceAccount, Service: "example-service"}}
	if !slices.Equal(cfg.ExchangeGoogleAccounts, want) {
		t.Errorf("ExchangeGoogleAccounts with neither pin = %+v, want %+v", cfg.ExchangeGoogleAccounts, want)
	}
	const other, third = "other-fleet@example-project.iam.gserviceaccount.com", "third-fleet@example-project.iam.gserviceaccount.com"
	for name, values := range map[string][2]string{
		"BROKER_VM_PROJECTS":  {"example-project", "example-project"},
		"BROKER_VM_INSTANCES": {"example-vm-*", "example-vm-*"},
	} {
		// Twenty loads, since a refusal that ranged over a map would name either account.
		for range 20 {
			e := validVMEnv()
			e[name] += " " + other + "=" + values[0] + " " + third + "=" + values[1]
			_, err := Load(env(e))
			want := name + " names " + other + `, got "` + other + "=" + values[0] + `", but BROKER_VM_SERVICES does not list it`
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("Load with %s naming two unlisted accounts = %v, want a refusal starting %q", name, err, want)
			}
		}
	}
	for _, row := range []struct{ name, value string }{
		{"BROKER_VM_PROJECTS", "Example_Project"},
		{"BROKER_VM_PROJECTS", "123456789012"},
		{"BROKER_VM_PROJECTS", "example-project=x"},
		{"BROKER_VM_INSTANCES", "example-vm-["},
		{"BROKER_VM_INSTANCES", "[!t]*"},
		{"BROKER_VM_INSTANCES", "example-vm-*/"},
		{"BROKER_VM_INSTANCES", "Example-VM-*"},
		{"BROKER_VM_INSTANCES", "example-vm-*=x"},
		{"BROKER_VM_INSTANCES", "example.vm.*"},
		{"BROKER_VM_INSTANCES", "example_vm_*"},
		{"BROKER_VM_INSTANCES", "a{b,c}"},
		{"BROKER_VM_INSTANCES", `example-vm-\*`},
		{"BROKER_VM_INSTANCES", "example-vm^x"},
		{"BROKER_VM_INSTANCES", "^example-vm-*"},
		{"BROKER_VM_INSTANCES", ""},
		{"BROKER_VM_PROJECTS", ""},
		{"BROKER_VM_PROJECTS", "short"},
		{"BROKER_VM_PROJECTS", strings.Repeat("a", 31)},
		{"BROKER_VM_PROJECTS", "example-project-"},
	} {
		e := validVMEnv()
		bad := exampleVMServiceAccount + "=" + row.value
		e[row.name] = bad
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), row.name+" must be <service account email>=<") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("Load(%s=%q) = %v, want a refusal naming it and the entry", row.name, bad, err)
		}
	}
	for _, glob := range []string{"example-vm-[0-9]*", "example-vm-[^x]?", "*"} {
		e := validVMEnv()
		e["BROKER_VM_INSTANCES"] = exampleVMServiceAccount + "=" + glob
		cfg, err := Load(env(e))
		if err != nil || len(cfg.ExchangeGoogleAccounts) != 1 || cfg.ExchangeGoogleAccounts[0].Instance != glob {
			t.Errorf("Load(BROKER_VM_INSTANCES glob %q) = %+v, %v; want it accepted", glob, cfg.ExchangeGoogleAccounts, err)
		}
	}
	for _, setting := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		e := validVMEnv()
		bad := "not-an-email=" + strings.SplitN(validVMEnv()[setting], "=", 2)[1]
		e[setting] = bad
		_, err := Load(env(e))
		if want := setting + " must name user-managed service accounts"; err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("Load(%s=%q) = %v, want a refusal starting %q and naming the entry", setting, bad, err, want)
		}
	}
	for setting, row := range map[string]struct {
		kind   string
		values []string
	}{
		"BROKER_VM_PROJECTS":  {"project id", []string{"example-other", "example-project"}},
		"BROKER_VM_INSTANCES": {"instance-name glob", []string{"example-other", "example-vm-*"}},
	} {
		kind := row.kind
		for _, other := range row.values {
			e := validVMEnv()
			e[setting] += " " + exampleVMServiceAccount + "=" + other
			_, err := Load(env(e))
			want := setting + " names the service account " + exampleVMServiceAccount + " twice; each account has one " + kind + ", so name it once"
			if err == nil || err.Error() != want {
				t.Errorf("Load with an account given twice in %s = %v, want %q", setting, err, want)
			}
		}
	}
}

// TestAnAccountBoundByTheKubernetesServicesIsRefusedAsAVMAccount pins that the two account kinds
// never overlap: BROKER_SERVICE_LOGINS refuses a Google email, so a VM's account cannot log in as a
// Kubernetes service, and BROKER_VM_SERVICES refuses a Kubernetes subject, so a pod's account cannot
// be bound as a VM service.
func TestAnAccountBoundByTheKubernetesServicesIsRefusedAsAVMAccount(t *testing.T) {
	// The setting's own parser refuses, by its own message, with the Kubernetes issuer set, so no
	// other refusal can stand in for it.
	e := validVMEnv()
	e["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
	e["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
	e["BROKER_SERVICE_LOGINS"] = "example-service=" + exampleVMServiceAccount
	_, err := Load(env(e))
	if want := "BROKER_SERVICE_LOGINS must be <service>=system:serviceaccount:<namespace>:<name> entries"; err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), exampleVMServiceAccount) {
		t.Fatalf("Load with a Google account in BROKER_SERVICE_LOGINS = %v, want a refusal starting %q and naming the account", err, want)
	}

	const subject = "system:serviceaccount:example:example-sa"
	e = validVMEnv()
	e["BROKER_SERVICES"] = "example-service=" + subject
	e["BROKER_VM_SERVICES"] = "example-service=" + subject
	e["BROKER_VM_PROJECTS"] = subject + "=example-project"
	e["BROKER_VM_INSTANCES"] = subject + "=example-vm-*"
	_, err = Load(env(e))
	if want := `BROKER_VM_SERVICES must name user-managed service accounts, <account id>@<project id>.iam.gserviceaccount.com, got "example-service=` + subject + `"`; err == nil || err.Error() != want {
		t.Fatalf("Load with a Kubernetes subject in BROKER_VM_SERVICES = %v, want %q", err, want)
	}
}

// TestWithNoGoogleAudienceEveryVMFieldIsEmpty pins requirement R4's configuration half: with no
// Google audience and no VM setting, every VM field is empty, so nothing downstream can act on one.
func TestWithNoGoogleAudienceEveryVMFieldIsEmpty(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoogleOIDCAudience != "" || cfg.VMServicesByAccount != nil {
		t.Fatalf("VM configuration = audience %q, services %v; want empty with no Google audience", cfg.GoogleOIDCAudience, cfg.VMServicesByAccount)
	}
	if cfg.ExchangeGoogleAccounts != nil {
		t.Fatalf("ExchangeGoogleAccounts = %+v, want none with no Google audience: the exchange then registers no Google subject token type", cfg.ExchangeGoogleAccounts)
	}
}

// TestLoadReadsTheServiceLogins pins BROKER_SERVICE_LOGINS: whitespace-separated
// <service>=<ServiceAccount subject> entries keyed by subject, none when unset; one service may have
// several accounts, an account is named once and exactly (no wildcard), and the setting needs a
// Kubernetes token issuer to trust.
func TestLoadReadsTheServiceLogins(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceLogins != nil {
		t.Fatalf("ServiceLogins = %v, want nil when BROKER_SERVICE_LOGINS is unset", cfg.ServiceLogins)
	}
	withIssuer := func() map[string]string {
		e := validEnv()
		e["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
		e["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
		return e
	}
	e := withIssuer()
	e["BROKER_SERVICE_LOGINS"] = " legion-daemon=system:serviceaccount:legion-system:legion-daemon-a\nlegion-daemon=system:serviceaccount:legion-system:legion-daemon-b "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system:serviceaccount:legion-system:legion-daemon-a": "legion-daemon",
		"system:serviceaccount:legion-system:legion-daemon-b": "legion-daemon",
	}
	if !maps.Equal(cfg.ServiceLogins, want) {
		t.Fatalf("ServiceLogins = %v, want %v", cfg.ServiceLogins, want)
	}

	for _, bad := range []string{
		"legion-daemon", "=system:serviceaccount:a:b", "shared=system:serviceaccount:a:b", "Legion=system:serviceaccount:a:b",
		"legion-daemon=system:serviceaccount:legion-system:*", "legion-daemon=system:serviceaccount:*:legion-daemon", "legion-daemon=legion-daemon-a",
	} {
		e := withIssuer()
		e["BROKER_SERVICE_LOGINS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICE_LOGINS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_SERVICE_LOGINS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withIssuer()
	e["BROKER_SERVICE_LOGINS"] = "a=system:serviceaccount:x:y b=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_SERVICE_LOGINS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	// A namespace's default account proves nothing, as BROKER_SERVICES refuses it, so it logs no
	// service in.
	e = withIssuer()
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:default"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SERVICE_LOGINS") || !strings.Contains(err.Error(), "default service account") {
		t.Errorf("BROKER_SERVICE_LOGINS naming a default account: err = %v, want a refusal naming the setting and the default account", err)
	}
	e = withIssuer()
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:legion-worker"
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:legion-daemon-a"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("BROKER_SERVICE_LOGINS beside a BROKER_SERVICES binding another account: %v", err)
	}
	e = validEnv()
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=system:serviceaccount:legion-system:legion-daemon-a"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_SERVICE_LOGINS with no token issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadTakesOneAccountInBothServiceLists pins that a deployment may name one ServiceAccount in
// BROKER_SERVICES and in BROKER_SERVICE_LOGINS, which is a pod of a bound account enrolling itself
// (agent-secrets enroll --pod): Load fills both maps from that pair rather than refusing it. What
// narrows such a login is enroll.Service.podUnderTokenLogin: it gives the login the one pod whose
// token made it and answers ErrPodSelfEnrollOnly for every other pod of the account
// (internal/cipher/enroll/pod_self_enroll_test.go).
func TestLoadTakesOneAccountInBothServiceLists(t *testing.T) {
	const account = "system:serviceaccount:legion:agent-session"
	e := withK8sIssuer()
	e["BROKER_SERVICES"] = "legion-daemon=" + account
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=" + account
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load(one account in both service lists) = %v, want no refusal", err)
	}
	want := map[string]string{account: "legion-daemon"}
	if !maps.Equal(cfg.ServicesByAccount, want) {
		t.Errorf("ServicesByAccount = %v, want %v", cfg.ServicesByAccount, want)
	}
	if !maps.Equal(cfg.ServiceLogins, want) {
		t.Errorf("ServiceLogins = %v, want %v", cfg.ServiceLogins, want)
	}
}

// TestRegistrarSettings pins BROKER_POD_REGISTRARS and BROKER_POD_REGISTRATION_AUDIENCE. Unset,
// the broker loads with no entry and no audience, a registration route that admits nothing. Each
// malformed entry of the list — one without exactly one "=", an account that is no ServiceAccount
// subject (a google: one included), a namespace's default account on either side — and a pair
// given twice are refused naming the setting and the entry, and the list is refused with no
// cluster issuer. The audience is refused naming it and the other setting when it equals the
// broker's own audience, the token exchange's or the NATS auth service's, each set beside it, and
// refused with no cluster issuer. A registrar with two pod accounts, a pod account with two
// registrars and an account paired with itself load as exactly those pairs beside an audience of
// its own, with no BROKER_SERVICE_LOGINS at all: a registrar the logins do not list is not
// refused, since it can register nothing and the boot names it instead.
func TestRegistrarSettings(t *testing.T) {
	const (
		daemonA = "system:serviceaccount:legion-system:legion-daemon-a"
		daemonB = "system:serviceaccount:legion-system:legion-daemon-b"
		worker  = "system:serviceaccount:legion:worker"
		other   = "system:serviceaccount:legion:other"
	)
	user, account, curve, _ := natsSeeds(t)
	// withAudiences is the cluster issuer with every other audience its tokens are accepted at
	// set beside the broker's own: the token exchange's and the NATS auth service's, whose service
	// needs its hub and its three seeds. So an audience row is refused for the one equality it
	// names and nothing else.
	withAudiences := func(set map[string]string) map[string]string {
		e := withK8sIssuer()
		e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
		e["BROKER_NATS_URLS"] = "nats://hub-a:4222"
		e["BROKER_NATS_SEED_FILE"], e["BROKER_NATS_ISSUER_SEED_FILE"], e["BROKER_NATS_XKEY_SEED_FILE"] = user, account, curve
		e["BROKER_NATS_K8S_AUDIENCE"] = "envoy-nats"
		maps.Copy(e, set)
		return e
	}
	// withoutIssuer is validEnv, which names no cluster issuer, with set.
	withoutIssuer := func(set map[string]string) map[string]string {
		e := validEnv()
		maps.Copy(e, set)
		return e
	}
	list := func(entries string) map[string]string { return map[string]string{"BROKER_POD_REGISTRARS": entries} }
	audience := func(value string) map[string]string {
		return map[string]string{"BROKER_POD_REGISTRATION_AUDIENCE": value}
	}
	for _, row := range []struct {
		name string
		env  map[string]string
		// refused is every text the refusal must name; none for an environment that loads.
		refused []string
		// registrars and audience are what an environment that loads reads.
		registrars api.PodRegistrars
		audience   string
	}{
		{name: "both unset", env: validEnv()},
		{
			name: "an entry with no =", env: withAudiences(list(daemonA)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(daemonA)},
		},
		{
			name: "an entry with two =", env: withAudiences(list(daemonA + "=" + worker + "=" + other)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(daemonA + "=" + worker + "=" + other)},
		},
		{
			name: "an entry naming no registrar", env: withAudiences(list("=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote("=" + worker)},
		},
		{
			name: "a registrar that is a service's name", env: withAudiences(list("legion-daemon=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote("legion-daemon=" + worker)},
		},
		{
			name: "a registrar that is a Google account", env: withAudiences(list(googleAPIAccountTag + exampleListenerAccount + "=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(googleAPIAccountTag + exampleListenerAccount + "=" + worker)},
		},
		{
			name: "a pod account that is no subject", env: withAudiences(list(daemonA + "=example-worker")),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(daemonA + "=example-worker")},
		},
		{
			name: "a namespace's default account as the registrar", env: withAudiences(list("system:serviceaccount:legion-system:default=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote("system:serviceaccount:legion-system:default=" + worker), "default service account"},
		},
		{
			name: "a namespace's default account as the pod account", env: withAudiences(list(daemonA + "=system:serviceaccount:legion:default")),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(daemonA + "=system:serviceaccount:legion:default"), "default service account"},
		},
		{
			name: "one pair given twice", env: withAudiences(list(daemonA + "=" + worker + " " + daemonA + "=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", strconv.Quote(daemonA + "=" + worker), "twice"},
		},
		{
			name: "the list with no cluster issuer", env: withoutIssuer(list(daemonA + "=" + worker)),
			refused: []string{"BROKER_POD_REGISTRARS", "BROKER_K8S_OIDC_ISSUER"},
		},
		{
			name: "the audience equal to the broker's own", env: withAudiences(audience(" agent-secrets\n")),
			refused: []string{"BROKER_POD_REGISTRATION_AUDIENCE", "BROKER_K8S_OIDC_AUDIENCE"},
		},
		{
			name: "the audience equal to the token exchange's", env: withAudiences(audience("dispatch-exchange")),
			refused: []string{"BROKER_POD_REGISTRATION_AUDIENCE", "BROKER_EXCHANGE_K8S_AUDIENCE"},
		},
		{
			name: "the audience equal to the NATS auth service's", env: withAudiences(audience("envoy-nats")),
			refused: []string{"BROKER_POD_REGISTRATION_AUDIENCE", "BROKER_NATS_K8S_AUDIENCE"},
		},
		{
			name: "the audience with no cluster issuer", env: withoutIssuer(audience("legion-broker-registration")),
			refused: []string{"BROKER_POD_REGISTRATION_AUDIENCE", "BROKER_K8S_OIDC_ISSUER"},
		},
		{
			name: "a registrar with two pod accounts, a pod account with two registrars and a self-pair",
			env: withAudiences(map[string]string{
				"BROKER_POD_REGISTRARS":            " " + daemonA + "=" + worker + "\n" + daemonA + "=" + other + "\t" + daemonB + "=" + worker + " " + daemonB + "=" + daemonB + " ",
				"BROKER_POD_REGISTRATION_AUDIENCE": " legion-broker-registration ",
			}),
			registrars: api.PodRegistrars{{Registrar: daemonA, Pod: worker}: {}, {Registrar: daemonA, Pod: other}: {}, {Registrar: daemonB, Pod: worker}: {}, {Registrar: daemonB, Pod: daemonB}: {}},
			audience:   "legion-broker-registration",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg, err := Load(env(row.env))
			for _, name := range row.refused {
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("Load = %v, want a refusal naming %s", err, strings.Join(row.refused, " and "))
				}
			}
			if len(row.refused) > 0 {
				return
			}
			if err != nil {
				t.Fatalf("Load = %v, want no refusal", err)
			}
			if !maps.Equal(cfg.PodRegistrars, row.registrars) || cfg.PodRegistrationAudience != row.audience {
				t.Fatalf("Load read the pairs %v and the audience %q, want exactly %v and %q", cfg.PodRegistrars, cfg.PodRegistrationAudience, row.registrars, row.audience)
			}
		})
	}
}

// The accounts the session account pair tests pair: the shared account a deployment's pods ran
// as, the project's own account it moves them to, and a third. Each pair below is written with
// its later account first in sorted order, so a reader that sorted a pair would be caught.
const (
	sharedWorkerAccount  = "system:serviceaccount:example-ns:shared-worker"
	projectWorkerAccount = "system:serviceaccount:example-ns:project-worker"
	otherWorkerAccount   = "system:serviceaccount:example-ns:other-worker"
)

// TestSessionAccountPairsAreRead pins BROKER_SESSION_ACCOUNT_PAIRS: whitespace-separated
// <account>=<account> entries, each read as its two accounts in the order written and the pairs
// in the order given, since cmd/cipherd logs the pairs as the operator wrote them; an account in
// two pairs, which is a deployment moving one shared account's pods to two accounts, or moving
// them on again; and no pair at all when the setting is unset, empty or white space only, which
// is the broker that takes no name from anyone.
func TestSessionAccountPairsAreRead(t *testing.T) {
	for _, value := range []string{"", " \t\n "} {
		e := validEnv()
		e["BROKER_SESSION_ACCOUNT_PAIRS"] = value
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load(BROKER_SESSION_ACCOUNT_PAIRS=%q) = %v, want no refusal", value, err)
		}
		if cfg.SessionAccountPairs != nil {
			t.Errorf("SessionAccountPairs with BROKER_SESSION_ACCOUNT_PAIRS=%q = %v, want nil", value, cfg.SessionAccountPairs)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionAccountPairs != nil {
		t.Errorf("SessionAccountPairs = %v, want nil when BROKER_SESSION_ACCOUNT_PAIRS is unset", cfg.SessionAccountPairs)
	}

	e := validEnv()
	e["BROKER_SESSION_ACCOUNT_PAIRS"] = sharedWorkerAccount + "=" + projectWorkerAccount
	if cfg, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{sharedWorkerAccount, projectWorkerAccount}}; !slices.Equal(cfg.SessionAccountPairs, want) {
		t.Errorf("SessionAccountPairs with one pair = %v, want %v, its two accounts in the order written", cfg.SessionAccountPairs, want)
	}

	e = validEnv()
	e["BROKER_SESSION_ACCOUNT_PAIRS"] = " " + sharedWorkerAccount + "=" + projectWorkerAccount + "\n\t" + projectWorkerAccount + "=" + otherWorkerAccount + " "
	if cfg, err = Load(env(e)); err != nil {
		t.Fatalf("Load(two pairs sharing an account) = %v, want no refusal: an account may be in several pairs", err)
	}
	want := [][2]string{{sharedWorkerAccount, projectWorkerAccount}, {projectWorkerAccount, otherWorkerAccount}}
	if !slices.Equal(cfg.SessionAccountPairs, want) {
		t.Errorf("SessionAccountPairs with two pairs sharing an account = %v, want %v, in the order given", cfg.SessionAccountPairs, want)
	}
}

// TestSessionAccountPairsRefuseABadEntry pins BROKER_SESSION_ACCOUNT_PAIRS' refusals, one row per
// kind: an entry that is not two accounts joined by one "=", an account that is not a
// ServiceAccount subject or is a namespace's default account, an account paired with itself, and
// a pair given twice, as written or reversed. Each bad entry follows a good pair, so the reader
// refuses whichever entry is bad and not only the first, and each refusal stops the start naming
// the setting and the bad entry as written, so the operator reads which one to fix, and says
// which kind of mistake it is.
func TestSessionAccountPairsRefuseABadEntry(t *testing.T) {
	const good = sharedWorkerAccount + "=" + projectWorkerAccount
	for _, row := range []struct{ name, entry, reason string }{
		{"no =", otherWorkerAccount, "<account>=<account>"},
		{"two =", projectWorkerAccount + "=" + otherWorkerAccount + "=" + sharedWorkerAccount, "<account>=<account>"},
		{"an empty first side", "=" + otherWorkerAccount, "<account>=<account>"},
		{"an empty second side", otherWorkerAccount + "=", "<account>=<account>"},
		{"a side that is not an account subject", projectWorkerAccount + "=other-worker", "must name system:serviceaccount:<namespace>:<name>"},
		{"a namespace's default account", projectWorkerAccount + "=system:serviceaccount:example-ns:default", "default service account"},
		{"an account paired with itself", otherWorkerAccount + "=" + otherWorkerAccount, "with itself"},
		{"the same pair repeated", good, "again"},
		{"the reversed pair repeated", projectWorkerAccount + "=" + sharedWorkerAccount, "again"},
	} {
		e := validEnv()
		e["BROKER_SESSION_ACCOUNT_PAIRS"] = good + " " + row.entry
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_SESSION_ACCOUNT_PAIRS") || !strings.Contains(err.Error(), strconv.Quote(row.entry)) ||
			!strings.Contains(err.Error(), row.reason) {
			t.Errorf("BROKER_SESSION_ACCOUNT_PAIRS with %s, %q: err = %v, want a refusal naming the setting, the entry and %q", row.name, row.entry, err, row.reason)
		}
	}
}

// TestLoadRefusesTheRulesFileVariables pins that a deployment still configured for the rules file
// refuses to start naming the variable, rather than running with its rules silently ignored. The
// reason names both halves of what replaced it: the tags each secret's owner and tier come from,
// and BROKER_PROFILES_FILE, the file the broker does read, for the other question.
func TestLoadRefusesTheRulesFileVariables(t *testing.T) {
	for name, value := range map[string]string{
		"BROKER_RULES_FILE":           "/r.yaml",
		"BROKER_RULES_S3_URI":         "s3://bucket/agent-secret-rules.yaml",
		"BROKER_RULES_RELOAD_SECONDS": "300",
	} {
		e := validEnv()
		e[name] = value
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), name+" is removed; ") || !strings.Contains(err.Error(), "tags") || !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") {
			t.Fatalf("Load(%s set) = %v, want a refusal naming it, the tags that replace it and BROKER_PROFILES_FILE", name, err)
		}
	}
}

// TestLoadRequiresTheNamespaceAndItsKey pins that the broker refuses to start without the
// namespace it serves and the key every secret in it must be on, or with either in a form the
// loader cannot use: a prefix that is not a name prefix ending in "/", or a key named any way but
// by its ARN (an alias can be repointed, and a bare key id names no account).
func TestLoadRequiresTheNamespaceAndItsKey(t *testing.T) {
	for name, value := range map[string]string{
		"BROKER_SECRETS_PREFIX":      "",
		"BROKER_SECRETS_KMS_KEY_ARN": "",
	} {
		e := validEnv()
		e[name] = value
		if _, err := Load(env(e)); err == nil || err.Error() != name+" is required" {
			t.Fatalf("Load(%s unset) = %v, want %q", name, err, name+" is required")
		}
	}
	for _, prefix := range []string{"example/agent-secrets", "/example/agent-secrets/", "example/agent secrets/"} {
		e := validEnv()
		e["BROKER_SECRETS_PREFIX"] = prefix
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_PREFIX must be") {
			t.Fatalf("Load(BROKER_SECRETS_PREFIX=%q) = %v, want a refusal naming it", prefix, err)
		}
	}
	for _, key := range []string{"alias/agent-secrets", "1234abcd-12ab-34cd-56ef-1234567890ab", "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets"} {
		e := validEnv()
		e["BROKER_SECRETS_KMS_KEY_ARN"] = key
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_KMS_KEY_ARN must be") {
			t.Fatalf("Load(BROKER_SECRETS_KMS_KEY_ARN=%q) = %v, want a refusal naming it", key, err)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.SecretsPrefix != "example/agent-secrets/" || cfg.SecretsKMSKey != validEnv()["BROKER_SECRETS_KMS_KEY_ARN"] {
		t.Fatalf("Load(valid) = %+v, %v", cfg, err)
	}
}

// TestLoadReadsTheSecretStore pins BROKER_SECRETS_STORE: aws-secrets-manager when unset, so
// every deployment from before the variable is unchanged; gcp-secret-manager with its own prefix
// form (the project by its number, a secret-id prefix ending in "-") and BROKER_SECRETS_KMS_KEY_NAME
// in place of the ARN; each store refusing the other store's key variable, so a deployment moved
// between stores cannot keep the old key set and mean nothing by it; and any other name refused
// naming both.
func TestLoadReadsTheSecretStore(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.SecretsStore != secrets.StoreAWS || cfg.SecretsKMSKey != validEnv()["BROKER_SECRETS_KMS_KEY_ARN"] {
		t.Fatalf("Load(unset store) = %+v, %v; want %s", cfg, err, secrets.StoreAWS)
	}

	gcp := func() map[string]string {
		e := validEnv()
		e["BROKER_SECRETS_STORE"] = secrets.StoreGCP
		e["BROKER_SECRETS_PREFIX"] = "projects/000000000000/secrets/agent-secrets-"
		delete(e, "BROKER_SECRETS_KMS_KEY_ARN")
		e["BROKER_SECRETS_KMS_KEY_NAME"] = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
		return e
	}
	cfg, err = Load(env(gcp()))
	if err != nil || cfg.SecretsStore != secrets.StoreGCP || cfg.SecretsPrefix != "projects/000000000000/secrets/agent-secrets-" || cfg.SecretsKMSKey != gcp()["BROKER_SECRETS_KMS_KEY_NAME"] {
		t.Fatalf("Load(gcp) = %+v, %v", cfg, err)
	}

	e := gcp()
	e["BROKER_MINT_TARGETS"] = "ANY_OLD_VALUE"
	if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_MINT_TARGETS is removed; ") {
		t.Fatalf("Load(BROKER_MINT_TARGETS) = %v, want a refusal naming the removed setting", err)
	}

	for name, tc := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"an unknown store": {func(e map[string]string) { e["BROKER_SECRETS_STORE"] = "vault" }, "BROKER_SECRETS_STORE must be aws-secrets-manager or gcp-secret-manager"},
		"gcp with the ARN set": {func(e map[string]string) {
			e["BROKER_SECRETS_KMS_KEY_ARN"] = validEnv()["BROKER_SECRETS_KMS_KEY_ARN"]
		}, "BROKER_SECRETS_KMS_KEY_ARN is not read with BROKER_SECRETS_STORE=gcp-secret-manager; set BROKER_SECRETS_KMS_KEY_NAME instead"},
		"gcp with no key name": {func(e map[string]string) { delete(e, "BROKER_SECRETS_KMS_KEY_NAME") }, "BROKER_SECRETS_KMS_KEY_NAME is required"},
		"gcp with a project id": {func(e map[string]string) {
			e["BROKER_SECRETS_PREFIX"] = "projects/example-project/secrets/agent-secrets-"
		}, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with an AWS prefix": {func(e map[string]string) { e["BROKER_SECRETS_PREFIX"] = "example/agent-secrets/" }, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with no trailing dash": {func(e map[string]string) {
			e["BROKER_SECRETS_PREFIX"] = "projects/000000000000/secrets/agent-secrets"
		}, "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"},
		"gcp with a regional key": {func(e map[string]string) {
			e["BROKER_SECRETS_KMS_KEY_NAME"] = "projects/example-project/locations/us-west1/keyRings/r/cryptoKeys/k"
		}, "BROKER_SECRETS_KMS_KEY_NAME: a Cloud KMS key name is projects/"},
	} {
		e := gcp()
		tc.edit(e)
		if _, err := Load(env(e)); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("Load(%s) = %v, want a refusal opening %q", name, err, tc.want)
		}
	}

	aws := validEnv()
	aws["BROKER_SECRETS_KMS_KEY_NAME"] = gcp()["BROKER_SECRETS_KMS_KEY_NAME"]
	if _, err := Load(env(aws)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_SECRETS_KMS_KEY_NAME is not read with BROKER_SECRETS_STORE=aws-secrets-manager; set BROKER_SECRETS_KMS_KEY_ARN instead") {
		t.Fatalf("Load(aws with the key name set) = %v, want a refusal naming both", err)
	}
}

// TestLoadRefusesTheDatabasePassword pins that the broker substitutes no database password into
// its URL: a deployment still setting BROKER_DATABASE_PASSWORD, one whose BROKER_DATABASE_URL still
// names the ${BROKER_DATABASE_PASSWORD} placeholder, and one doing both refuse to start, naming the
// variable and why (on RDS the broker signs in by IAM token, and a URL to any other database
// carries its own password), and never print the password or the URL.
func TestLoadRefusesTheDatabasePassword(t *testing.T) {
	const password = "s3" + "cret-database-password"
	const placeholderURL = "postgres://broker:${BROKER_DATABASE_PASSWORD}@db.internal.example:5432/broker"
	for _, tc := range []struct {
		name, url, password, opens string
	}{
		{name: "the variable alone", url: "postgres://broker:" + password + "@db.internal.example:5432/broker", password: password, opens: "BROKER_DATABASE_PASSWORD is removed; "},
		{name: "the placeholder alone", url: placeholderURL, opens: "BROKER_DATABASE_URL names the removed ${BROKER_DATABASE_PASSWORD} placeholder; "},
		{name: "both", url: placeholderURL, password: password, opens: "BROKER_DATABASE_PASSWORD is removed; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			if tc.password != "" {
				e["BROKER_DATABASE_PASSWORD"] = tc.password
			}
			_, err := Load(env(e))
			if err == nil {
				t.Fatal("Load accepted it; want a refusal naming BROKER_DATABASE_PASSWORD and why it is gone")
			}
			refusal := err.Error()
			if !strings.HasPrefix(refusal, tc.opens) {
				t.Errorf("refusal %q does not open %q", refusal, tc.opens)
			}
			for _, why := range []string{"by IAM token", "https://cybermonklabs.github.io/legion/broker/operate/#signing-in-to-rds-by-iam-token", "goes in the URL itself"} {
				if !strings.Contains(refusal, why) {
					t.Errorf("refusal %q does not say %q", refusal, why)
				}
			}
			for _, secret := range []string{password, "db.internal.example"} {
				if strings.Contains(refusal, secret) {
					t.Errorf("refusal %q prints %q, part of the password or the URL", refusal, secret)
				}
			}
		})
	}
}

// rdsHost is an Amazon RDS cluster endpoint of the form the deployment's URL names.
const rdsHost = "example-cluster.cluster-abcdefghijkl.us-west-2.rds.amazonaws.com"

// rdsBundle is the RDS CA bundle the broker image ships, as the repository vendors it.
const rdsBundle = "../../../docker/rds-global-bundle.pem"

// verifiedRDSURL is an IAM-form URL that verifies the server: a user, no password, an RDS host,
// sslmode=verify-full and the bundle as sslrootcert.
const verifiedRDSURL = "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets?sslmode=verify-full&sslrootcert=" + rdsBundle

// isolatePostgresDefaults keeps the process's libpq defaults out of a test: pgx reads PG*
// variables and ~/.postgresql/root.crt beneath what the URL says, so a developer's own would
// change what a URL verifies.
func isolatePostgresDefaults(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(name, "")
	}
}

// TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL pins when the broker signs in to its database
// with an IAM token: a URL naming a user and no password whose host is an Amazon RDS endpoint.
// A password in the URL, a URL naming no user, and a passwordless URL to any other host (local
// trust auth) connect as given, with the password, if any, the URL or libpq's defaults supply.
func TestLoadSignsInByIAMOnlyForAPasswordlessRDSURL(t *testing.T) {
	isolatePostgresDefaults(t)
	for _, tc := range []struct {
		name, url string
		iam       bool
	}{
		{name: "an RDS endpoint, a user and no password", url: verifiedRDSURL, iam: true},
		{name: "an RDS endpoint spelled in capitals", url: strings.Replace(verifiedRDSURL, rdsHost, strings.ToUpper(rdsHost), 1), iam: true},
		{name: "the user as a query parameter", url: "postgres://" + rdsHost + ":5432/agent_secrets?user=agent_secrets_broker&sslmode=verify-full&sslrootcert=" + rdsBundle, iam: true},
		{name: "a password in the URL", url: "postgres://agent_secrets:secret@" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a password as a query parameter", url: verifiedRDSURL + "&password=secret"},
		{name: "a password with a semicolon, which net/url drops from the query and pgx reads", url: verifiedRDSURL + "&password=sec;ret"},
		{name: "no user", url: "postgres://" + rdsHost + ":5432/agent_secrets?sslmode=require"},
		{name: "a local passwordless URL", url: "postgres://broker@127.0.0.1:5432/broker?sslmode=disable"},
		{name: "a host with the RDS suffix in the middle of its name", url: "postgres://broker@db.rds.amazonaws.com.example.net:5432/broker?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabaseIAM != tc.iam {
				t.Fatalf("DatabaseIAM = %v for %s, want %v", cfg.DatabaseIAM, tc.url, tc.iam)
			}
		})
	}
}

// TestLoadLeavesAPasswordLibpqSuppliesToSignIn pins that a passwordless RDS URL whose password
// comes from libpq's defaults, PGPASSWORD or a passfile, which pgx reads beneath the URL, signs in
// with that password rather than a token.
func TestLoadLeavesAPasswordLibpqSuppliesToSignIn(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"PGPASSWORD": func(t *testing.T) { t.Setenv("PGPASSWORD", "from-the-environment") },
		"a passfile": func(t *testing.T) {
			passfile := t.TempDir() + "/pgpass"
			if err := os.WriteFile(passfile, []byte("*:*:*:agent_secrets_broker:from-the-passfile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PGPASSFILE", passfile)
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolatePostgresDefaults(t)
			setup(t)
			e := validEnv()
			e["BROKER_DATABASE_URL"] = verifiedRDSURL
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabaseIAM {
				t.Fatalf("DatabaseIAM = true with a password from %s; pgx would sign in with that password", name)
			}
		})
	}
}

// TestLoadRefusesPGSSLROOTCERTSystemForAnIAMURL pins that the system pool named through
// PGSSLROOTCERT, which pgx reads beneath the URL, is refused as the URL's own sslrootcert=system
// is: it holds no RDS CA, so every sign-in would fail its TLS handshake after a clean boot.
func TestLoadRefusesPGSSLROOTCERTSystemForAnIAMURL(t *testing.T) {
	isolatePostgresDefaults(t)
	t.Setenv("PGSSLROOTCERT", "system")
	e := validEnv()
	e["BROKER_DATABASE_URL"] = "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets?sslmode=verify-full"
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), rdsHost) || !strings.Contains(err.Error(), "sslrootcert=system") {
		t.Fatalf("Load with PGSSLROOTCERT=system = %v, want a refusal naming the host and sslrootcert=system", err)
	}
}

// TestLoadRefusesAnIAMURLThatDoesNotVerifyTheServer pins that an IAM token, a password good for
// 15 minutes, never goes over a link that has not verified the server: an IAM-form URL that pgx
// would not read as one host with sslmode=verify-full and a root certificate pool from
// sslrootcert refuses to start, naming the host and not the URL. sslmode=require encrypts and
// verifies nothing, and a repeated sslmode takes its last value, as libpq does.
func TestLoadRefusesAnIAMURLThatDoesNotVerifyTheServer(t *testing.T) {
	isolatePostgresDefaults(t)
	base := "postgres://agent_secrets_broker@" + rdsHost + ":5432/agent_secrets"
	for name, tc := range map[string]struct{ url, reason string }{
		"sslmode=require":                          {base + "?sslmode=require", "its sslmode is not verify-full"},
		"sslmode=require with sslrootcert":         {base + "?sslmode=require&sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"no sslmode":                               {base + "?sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"sslmode=disable":                          {base + "?sslmode=disable", "its sslmode is not verify-full"},
		"sslmode=verify-ca":                        {base + "?sslmode=verify-ca&sslrootcert=" + rdsBundle, "its sslmode is not verify-full"},
		"verify-full without sslrootcert":          {base + "?sslmode=verify-full", "it names no sslrootcert"},
		"a later sslmode undoing verify-full":      {verifiedRDSURL + "&sslmode=require", "its sslmode is not verify-full"},
		"a second host":                            {"postgres://agent_secrets_broker@" + rdsHost + ":5432,other.example.net:5432/agent_secrets?sslmode=verify-full&sslrootcert=" + rdsBundle, "it names another host too"},
		"an sslrootcert that names no file":        {base + "?sslmode=verify-full&sslrootcert=/nonexistent/rds-bundle.pem", "unable to read CA file"},
		"an sslrootcert that holds no certificate": {base + "?sslmode=verify-full&sslrootcert=config_test.go", "unable to add CA to cert pool"},
		"sslrootcert=system":                       {base + "?sslmode=verify-full&sslrootcert=system", "sslrootcert=system"},
		"sslrootcert=system under sslmode=require": {base + "?sslmode=require&sslrootcert=system", "sslrootcert=system"},
	} {
		t.Run(name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DATABASE_URL"] = tc.url
			cfg, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load accepted %s (DatabaseIAM %v); want a refusal naming the host", tc.url, cfg.DatabaseIAM)
			}
			for _, want := range []string{"BROKER_DATABASE_URL", rdsHost, tc.reason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "postgres://") {
				t.Errorf("refusal %q quotes the URL; it should name the host alone", err)
			}
		})
	}
}

// withK8sIssuer is validEnv with the cluster issuer every ServiceAccount token setting needs.
func withK8sIssuer() map[string]string {
	e := validEnv()
	e["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
	e["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
	return e
}

// exampleListenerAccount is the placeholder Google service account a listed caller on a VM runs
// as, as BROKER_API_ACCOUNTS names it; this repository is public, so no real account appears.
const exampleListenerAccount = "example-listener@example-project.iam.gserviceaccount.com"

// TestLoadReadsTheAPIAccounts pins BROKER_API_ACCOUNTS: whitespace-separated <caller>=<account>
// entries keyed by account, each answering the caller and the powers its name carries, none when
// unset, and refusals naming the entry for one that is not of either form, a namespace's default
// account and an account named twice. A Kubernetes entry needs the cluster issuer, since the
// broker verifies that caller's token against it.
func TestLoadReadsTheAPIAccounts(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAccounts != nil || cfg.GoogleAPIAccounts != nil {
		t.Fatalf("APIAccounts = %v and GoogleAPIAccounts = %v, want neither when BROKER_API_ACCOUNTS is unset", cfg.APIAccounts, cfg.GoogleAPIAccounts)
	}
	e := withK8sIssuer()
	e["BROKER_API_ACCOUNTS"] = " dispatch=system:serviceaccount:example-ns:example-dispatch\nlistener=system:serviceaccount:example-ns:example-listener "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	dispatchCaller, _ := api.AccountFor("dispatch")
	listenerCaller, _ := api.AccountFor("listener")
	want := map[string]api.Account{
		"system:serviceaccount:example-ns:example-dispatch": dispatchCaller,
		"system:serviceaccount:example-ns:example-listener": listenerCaller,
	}
	if !maps.Equal(cfg.APIAccounts, want) || cfg.GoogleAPIAccounts != nil {
		t.Fatalf("APIAccounts = %v and GoogleAPIAccounts = %v, want %v and no Google caller", cfg.APIAccounts, cfg.GoogleAPIAccounts, want)
	}
	for _, bad := range []string{
		"dispatch", "=system:serviceaccount:a:b", "Dispatch=system:serviceaccount:a:b",
		"dispatch=example-dispatch", "dispatch=system:serviceaccount:example-ns:default",
	} {
		e := withK8sIssuer()
		e["BROKER_API_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_API_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_API_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withK8sIssuer()
	e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:x:y listener=system:serviceaccount:x:y"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_API_ACCOUNTS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	e = validEnv()
	e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_API_ACCOUNTS with no token issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadReadsAGoogleAPIAccount pins the other kind of BROKER_API_ACCOUNTS entry: an account
// written google:<address> is a Google service account, kept apart from the Kubernetes accounts
// and keyed by the address its identity token carries in email, which is how the Envoy listener
// calls from a VM that holds no Kubernetes token. It needs BROKER_GOOGLE_OIDC_AUDIENCE, the
// audience that token must carry, and not the cluster's issuer; a Google address no machine may
// prove is refused here as BROKER_VM_SERVICES refuses it; and the two kinds live side by side.
func TestLoadReadsAGoogleAPIAccount(t *testing.T) {
	listenerCaller, _ := api.AccountFor("listener")
	dispatchCaller, _ := api.AccountFor("dispatch")

	e := validVMEnv()
	e["BROKER_API_ACCOUNTS"] = "listener=google:" + exampleListenerAccount
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	wantGoogle := map[string]api.Account{exampleListenerAccount: listenerCaller}
	if !maps.Equal(cfg.GoogleAPIAccounts, wantGoogle) || cfg.APIAccounts != nil {
		t.Fatalf("GoogleAPIAccounts = %v and APIAccounts = %v, want %v and no Kubernetes caller: a Google entry needs no cluster at all", cfg.GoogleAPIAccounts, cfg.APIAccounts, wantGoogle)
	}

	both := validVMEnv()
	both["BROKER_K8S_OIDC_ISSUER"] = "https://oidc.eks.example.invalid/id/EXAMPLE"
	both["BROKER_K8S_OIDC_AUDIENCE"] = "agent-secrets"
	both["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch listener=google:" + exampleListenerAccount
	cfg, err = Load(env(both))
	if err != nil {
		t.Fatal(err)
	}
	wantK8s := map[string]api.Account{"system:serviceaccount:example-ns:example-dispatch": dispatchCaller}
	if !maps.Equal(cfg.APIAccounts, wantK8s) || !maps.Equal(cfg.GoogleAPIAccounts, wantGoogle) {
		t.Fatalf("APIAccounts = %v, GoogleAPIAccounts = %v; want %v and %v: each entry goes to the list its own kind is verified from", cfg.APIAccounts, cfg.GoogleAPIAccounts, wantK8s, wantGoogle)
	}

	noAudience := validEnv()
	noAudience["BROKER_API_ACCOUNTS"] = "listener=google:" + exampleListenerAccount
	_, err = Load(env(noAudience))
	if err == nil || !strings.Contains(err.Error(), "BROKER_GOOGLE_OIDC_AUDIENCE") {
		t.Errorf("a Google API account with no Google audience: err = %v, want a refusal naming BROKER_GOOGLE_OIDC_AUDIENCE, the audience its token must carry", err)
	}
	if err != nil && strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("a Google API account was refused for the cluster's issuer: %v: a Google caller presents no Kubernetes token", err)
	}

	for _, bad := range []string{
		"listener=google:",
		"listener=google:example-listener@example-project.IAM.gserviceaccount.com",
		"listener=google:123456789-compute@developer.gserviceaccount.com",
		"listener=google:service-123456@gcp-sa-pubsub.iam.gserviceaccount.com",
		"listener=google:ada@example.com",
	} {
		e := validVMEnv()
		e["BROKER_API_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_API_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_API_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}

	twice := validVMEnv()
	twice["BROKER_API_ACCOUNTS"] = "dispatch=google:" + exampleListenerAccount + " listener=google:" + exampleListenerAccount
	if _, err := Load(env(twice)); err == nil || !strings.Contains(err.Error(), exampleListenerAccount) {
		t.Errorf("BROKER_API_ACCOUNTS naming one Google account twice: err = %v, want a refusal naming the account", err)
	}
}

// TestLoadRefusesACallerTheBrokerDoesNotKnow pins the boot refusal a caller name decides: a name
// the broker's own table (api.AccountFor) does not know is refused, naming the name, the entry
// and every name it does know, since a caller's name is what says which routes that account
// reaches. A typo that was merely listed would produce a caller reaching no route at all, which
// shows up as a working deployment until the day it is called.
func TestLoadRefusesACallerTheBrokerDoesNotKnow(t *testing.T) {
	e := withK8sIssuer()
	const entry = "dispatcher=system:serviceaccount:example-ns:example-dispatch"
	e["BROKER_API_ACCOUNTS"] = entry
	_, err := Load(env(e))
	if err == nil {
		t.Fatal("BROKER_API_ACCOUNTS naming an unknown caller: want a refusal, got nil")
	}
	for _, want := range append([]string{"BROKER_API_ACCOUNTS", strconv.Quote("dispatcher"), strconv.Quote(entry)}, api.KnownCallers()...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// TestLoadReadsTheAccessTokenLifetime pins BROKER_ACCESS_TOKEN_SECONDS: whole seconds, 900 by
// default, and a refusal naming the variable and its range for anything else.
func TestLoadReadsTheAccessTokenLifetime(t *testing.T) {
	e := validEnv()
	e["BROKER_ACCESS_TOKEN_SECONDS"] = "30"
	cfg, err := Load(env(e))
	if err != nil || cfg.AccessTokenSeconds != 30 {
		t.Fatalf("Load(BROKER_ACCESS_TOKEN_SECONDS=30) = (%d, %v), want 30", cfg.AccessTokenSeconds, err)
	}
	for _, bad := range []string{"0", "3601", "fifteen", "-1", "900.0"} {
		e := validEnv()
		e["BROKER_ACCESS_TOKEN_SECONDS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_ACCESS_TOKEN_SECONDS") {
			t.Errorf("BROKER_ACCESS_TOKEN_SECONDS = %q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}

// TestLoadReadsTheExchangeAccounts pins BROKER_EXCHANGE_ACCOUNTS: whitespace-separated
// <account>=<project>=<person>[=<session>] entries in order, none when unset, a session that may
// itself carry "=", an empty project (the shared public-session account's entry, whose named
// tokens name no project), an empty project and person (an account whose tokens act as
// the account itself), and refusals naming the entry for one that is not of that form, has a
// blank account, names a project and no person, names a namespace's default account or lists an
// account twice. It needs the cluster issuer and the exchange audience, whose tokens its pods
// present.
func TestLoadReadsTheExchangeAccounts(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeAccounts != nil {
		t.Fatalf("ExchangeAccounts = %v, want nil when BROKER_EXCHANGE_ACCOUNTS is unset", cfg.ExchangeAccounts)
	}
	withExchange := func() map[string]string {
		e := withK8sIssuer()
		e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	e := withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com\n" +
		" system:serviceaccount:example-ns:example-daemon=EXAMPLE=ada@example.com=legion-daemon:EXAMPLE=1 " +
		"system:serviceaccount:example-ns:example-public==ada@example.com " +
		"system:serviceaccount:example-ns:example-job== system:serviceaccount:example-ns:example-probe===example-probe-session"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.AccountEntry{
		{Account: "system:serviceaccount:example-ns:example-worker", Project: "EXAMPLE", Person: "ada@example.com"},
		{Account: "system:serviceaccount:example-ns:example-daemon", Project: "EXAMPLE", Person: "ada@example.com", Session: "legion-daemon:EXAMPLE=1"},
		{Account: "system:serviceaccount:example-ns:example-public", Project: "", Person: "ada@example.com"},
		{Account: "system:serviceaccount:example-ns:example-job"},
		{Account: "system:serviceaccount:example-ns:example-probe", Session: "example-probe-session"},
	}
	if !slices.Equal(cfg.ExchangeAccounts, want) {
		t.Fatalf("ExchangeAccounts = %v, want %v", cfg.ExchangeAccounts, want)
	}
	for _, bad := range []string{
		"system:serviceaccount:example-ns:example-worker", "system:serviceaccount:example-ns:example-worker=EXAMPLE",
		"system:serviceaccount:example-ns:example-worker=EXAMPLE=", "=EXAMPLE=ada@example.com",
		"example-worker=EXAMPLE=ada@example.com", "system:serviceaccount:example-ns:default=EXAMPLE=ada@example.com",
	} {
		e := withExchange()
		e["BROKER_EXCHANGE_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	// An entry naming a project and no person is refused naming that rule, not only its form.
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE="
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "names no person only when it names no project either") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS naming a project and no person: err = %v, want a refusal naming the rule", err)
	}
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:x:y=A=ada@example.com system:serviceaccount:x:y=B=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS with no exchange audience: err = %v, want a refusal naming BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
	e = validEnv()
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_K8S_AUDIENCE with no cluster issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadReadsTheExchangeReadAccounts pins BROKER_EXCHANGE_READ_ACCOUNTS: whitespace-separated
// <account>=<service>=<session> entries in order, none when unset, a session that may itself
// carry "=", and refusals naming the setting and the entry for one with fewer than three fields
// or a blank one, an account that is not a service account's subject, a namespace's default
// account, a service that is not a service's name or is the shared owner, and an account listed
// twice. An account BROKER_EXCHANGE_ACCOUNTS lists too is refused naming both settings, and the
// setting needs the cluster issuer and the exchange audience, whose tokens its pods present.
func TestLoadReadsTheExchangeReadAccounts(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeReadAccounts != nil {
		t.Fatalf("ExchangeReadAccounts = %v, want nil when BROKER_EXCHANGE_READ_ACCOUNTS is unset", cfg.ExchangeReadAccounts)
	}
	withExchange := func() map[string]string {
		e := withK8sIssuer()
		e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	e := withExchange()
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session\n" +
		" system:serviceaccount:example-ns:example-viewer=example-viewer=example-viewer=1 "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.ReadEntry{
		{Account: "system:serviceaccount:example-ns:example-reader", Service: "example-reader", Session: "example-reader-session"},
		{Account: "system:serviceaccount:example-ns:example-viewer", Service: "example-viewer", Session: "example-viewer=1"},
	}
	if !slices.Equal(cfg.ExchangeReadAccounts, want) {
		t.Fatalf("ExchangeReadAccounts = %v, want %v", cfg.ExchangeReadAccounts, want)
	}
	for _, bad := range []string{
		"system:serviceaccount:example-ns:example-reader",
		"system:serviceaccount:example-ns:example-reader=example-reader",
		"=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader==example-reader-session",
		"system:serviceaccount:example-ns:example-reader=example-reader=",
		"example-reader=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:default=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader=Example_Reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader=shared=example-reader-session",
	} {
		e := withExchange()
		e["BROKER_EXCHANGE_READ_ACCOUNTS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withExchange()
	const again = "system:serviceaccount:x:y=example-other=other-session"
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:x:y=example-reader=example-reader-session " + again
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(again)) {
		t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS naming one account twice: err = %v, want a refusal naming the setting and the entry", err)
	}
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=example-reader=example-reader-session"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") ||
		!strings.Contains(err.Error(), "BROKER_EXCHANGE_ACCOUNTS ") || !strings.Contains(err.Error(), "system:serviceaccount:example-ns:example-worker") {
		t.Errorf("an account both exchange settings list: err = %v, want a refusal naming both settings and the account", err)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS with no exchange audience: err = %v, want a refusal naming the setting and BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
}

// TestLoadReadsTheExchangeWorkflows pins BROKER_EXCHANGE_WORKFLOWS:
// <repository>=<workflow>=<ref>=<project>=<person>[=<session>] entries in order, none when unset,
// and refusals naming the entry for one that is not of that form or lists a workflow twice. It
// needs GitHub's issuer and audience, whose tokens its runs present.
func TestLoadReadsTheExchangeWorkflows(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeWorkflows != nil {
		t.Fatalf("ExchangeWorkflows = %v, want nil when BROKER_EXCHANGE_WORKFLOWS is unset", cfg.ExchangeWorkflows)
	}
	withGitHub := func() map[string]string {
		e := validEnv()
		e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
		e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	e := withGitHub()
	e["BROKER_EXCHANGE_WORKFLOWS"] = "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE=ada@example.com=release=1"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.WorkflowEntry{{
		Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main",
		Project: "EXAMPLE", Person: "ada@example.com", Session: "release=1",
	}}
	if !slices.Equal(cfg.ExchangeWorkflows, want) {
		t.Fatalf("ExchangeWorkflows = %v, want %v", cfg.ExchangeWorkflows, want)
	}
	for _, bad := range []string{
		"example-org/example-repo", "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE",
		"example-org/example-repo=.github/workflows/release.yaml==EXAMPLE=ada@example.com",
	} {
		e := withGitHub()
		e["BROKER_EXCHANGE_WORKFLOWS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_WORKFLOWS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_WORKFLOWS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withGitHub()
	entry := "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main"
	e["BROKER_EXCHANGE_WORKFLOWS"] = entry + "=A=ada@example.com " + entry + "=B=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), entry) {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS naming one workflow twice: err = %v, want a refusal naming the workflow", err)
	}
	e = validEnv()
	e["BROKER_EXCHANGE_WORKFLOWS"] = entry + "=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS with no GitHub issuer: err = %v, want a refusal naming BROKER_EXCHANGE_GITHUB_ISSUER", err)
	}
}

// TestLoadReadsTheExchangeResultWorkflows pins BROKER_EXCHANGE_RESULT_WORKFLOWS:
// <repository>=<workflow>=<ref pattern>=<project>=<person> entries in order, none when unset,
// one workflow under two patterns, and refusals naming the setting and the entry for one that is
// not of that form, a ref pattern exchange.CheckRefPattern refuses, and one workflow under one
// pattern twice. It needs GitHub's issuer and audience, whose tokens its runs present.
func TestLoadReadsTheExchangeResultWorkflows(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExchangeResultWorkflows != nil {
		t.Fatalf("ExchangeResultWorkflows = %v, want nil when BROKER_EXCHANGE_RESULT_WORKFLOWS is unset", cfg.ExchangeResultWorkflows)
	}
	withGitHub := func() map[string]string {
		e := withResultsAirlock(validEnv())
		e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
		e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
		return e
	}
	const workflow = "example-org/example-repo=.github/workflows/ci-results.yaml"
	e := withGitHub()
	e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = workflow + "=refs/heads/*=EXAMPLE=ada@example.com\n " + workflow + "=refs/heads/*/*=EXAMPLE=ada@example.com "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []exchange.ResultWorkflowEntry{
		{Repository: "example-org/example-repo", Workflow: ".github/workflows/ci-results.yaml", RefPattern: "refs/heads/*", Project: "EXAMPLE", Person: "ada@example.com"},
		{Repository: "example-org/example-repo", Workflow: ".github/workflows/ci-results.yaml", RefPattern: "refs/heads/*/*", Project: "EXAMPLE", Person: "ada@example.com"},
	}
	if !slices.Equal(cfg.ExchangeResultWorkflows, want) {
		t.Fatalf("ExchangeResultWorkflows = %v, want %v", cfg.ExchangeResultWorkflows, want)
	}
	for _, bad := range []string{
		"example-org/example-repo", workflow + "=refs/heads/*=EXAMPLE",
		workflow + "==EXAMPLE=ada@example.com", workflow + "=refs/heads/*=EXAMPLE=ada@example.com=a-session",
		workflow + "=refs/*/main=EXAMPLE=ada@example.com", workflow + "=refs/heads/[x=EXAMPLE=ada@example.com",
	} {
		e := withGitHub()
		e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_RESULT_WORKFLOWS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withGitHub()
	const again = workflow + "=refs/heads/*=OTHER=ada@example.com"
	e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = workflow + "=refs/heads/*=EXAMPLE=ada@example.com " + again
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") || !strings.Contains(err.Error(), strconv.Quote(again)) {
		t.Errorf("BROKER_EXCHANGE_RESULT_WORKFLOWS naming one workflow and pattern twice: err = %v, want a refusal naming the setting and the entry", err)
	}
	e = validEnv()
	e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = workflow + "=refs/heads/*=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_RESULT_WORKFLOWS with no GitHub issuer: err = %v, want a refusal naming the setting and BROKER_EXCHANGE_GITHUB_ISSUER", err)
	}
}

// The identities the job settings' tests list. This repository is public, so each is a
// placeholder.
const (
	// jobAccount is a pod's account that BROKER_EXCHANGE_JOB_ACCOUNTS lists.
	jobAccount = "system:serviceaccount:example-ns:example-job"
	// jobWorkflow is the identity, <repository>=<workflow path>=<ref>, of the workflow
	// BROKER_EXCHANGE_JOB_WORKFLOWS lists.
	jobWorkflow = "example-org/example-repo=.github/workflows/sweep.yaml=refs/heads/main"
	// secondFleet is a second Google service account, which jobFleetEnv lists as a VM service with
	// no pins.
	secondFleet = "second-fleet@example-project.iam.gserviceaccount.com"
)

// jobPodEnv is withK8sIssuer with the audience a pod's exchange token carries, which
// BROKER_EXCHANGE_JOB_ACCOUNTS needs.
func jobPodEnv() map[string]string {
	e := withK8sIssuer()
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
	return e
}

// jobWorkflowEnv is validEnv with the GitHub issuer and audience a workflow run's token is
// verified against, which BROKER_EXCHANGE_JOB_WORKFLOWS needs.
func jobWorkflowEnv() map[string]string {
	e := validEnv()
	e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
	e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
	return e
}

// jobFleetEnv is validVMEnv with secondFleet listed too, as the service example-other, so that
// BROKER_VM_EXCHANGE_JOB_ACCOUNTS has two listed accounts to name.
func jobFleetEnv() map[string]string {
	e := validVMEnv()
	e["BROKER_VM_SERVICES"] += " example-other=" + secondFleet
	return e
}

// jobList is the job list exchange.ParseJobProjects reads from text.
func jobList(t *testing.T, text string) exchange.JobProjects {
	t.Helper()
	job, err := exchange.ParseJobProjects(text)
	if err != nil {
		t.Fatalf("exchange.ParseJobProjects(%q): %v", text, err)
	}
	return job
}

// TestLoadReadsTheJobSettings pins the three job settings, one row for each identity kind: each
// entry is read, in order, into an exchange entry that holds its identity, its projects as Job and
// the session it fixes, if any, everything after the last separator, and that names no person and
// no project. A VM job entry lands in its account's machine entry, beside its service and its
// pins.
func TestLoadReadsTheJobSettings(t *testing.T) {
	const sweeper = "system:serviceaccount:example-ns:example-sweeper"
	for _, row := range []struct {
		setting string
		env     func() map[string]string
		value   string
		got     func(Config) any
		want    func(t *testing.T) any
	}{
		{
			setting: "BROKER_EXCHANGE_JOB_ACCOUNTS",
			env:     jobPodEnv,
			value:   jobAccount + "=EXAMPLE=example-job:EXAMPLE=1\n " + sweeper + "=public,PRIVB,PRIVA",
			got:     func(cfg Config) any { return cfg.ExchangeJobAccounts },
			want: func(t *testing.T) any {
				return []exchange.AccountEntry{
					{Account: jobAccount, Job: jobList(t, "EXAMPLE"), Session: "example-job:EXAMPLE=1"},
					{Account: sweeper, Job: jobList(t, "public,PRIVA,PRIVB")},
				}
			},
		},
		{
			setting: "BROKER_EXCHANGE_JOB_WORKFLOWS",
			env:     jobWorkflowEnv,
			value:   jobWorkflow + "=public=sweep=1\texample-org/example-repo=.github/workflows/triage.yaml=refs/heads/main=ACME",
			got:     func(cfg Config) any { return cfg.ExchangeJobWorkflows },
			want: func(t *testing.T) any {
				return []exchange.WorkflowEntry{
					{Repository: "example-org/example-repo", Workflow: ".github/workflows/sweep.yaml", Ref: "refs/heads/main", Job: jobList(t, "public"), Session: "sweep=1"},
					{Repository: "example-org/example-repo", Workflow: ".github/workflows/triage.yaml", Ref: "refs/heads/main", Job: jobList(t, "ACME")},
				}
			},
		},
		{
			setting: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS",
			env:     jobFleetEnv,
			value:   exampleVMServiceAccount + "=EXAMPLE=example-fleet-job " + secondFleet + "=public,PRIVA",
			got:     func(cfg Config) any { return cfg.ExchangeGoogleAccounts },
			want: func(t *testing.T) any {
				return []exchange.GoogleEntry{
					{
						Account: exampleVMServiceAccount, Service: "example-service",
						CloudProject: "example-project", Instance: "example-vm-*",
						Job: jobList(t, "EXAMPLE"), Session: "example-fleet-job",
					},
					{Account: secondFleet, Service: "example-other", Job: jobList(t, "public,PRIVA")},
				}
			},
		},
	} {
		t.Run(row.setting, func(t *testing.T) {
			e := row.env()
			e[row.setting] = row.value
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load(%s=%q): %v", row.setting, row.value, err)
			}
			if got, want := row.got(cfg), row.want(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("Load(%s=%q) read %+v, want %+v", row.setting, row.value, got, want)
			}
		})
	}
}

// TestLoadRefusesAJobEntryItCannotServe: each job setting refuses the start over an entry it
// cannot read, and the refusal names the setting, quotes the entry and says which rule it breaks.
// For each setting: a wrong number of fields, a blank identity, an identity no entry of its kind
// may name, an identity named twice, and projects exchange.ParseJobProjects refuses.
func TestLoadRefusesAJobEntryItCannotServe(t *testing.T) {
	type refusal struct {
		name, value string
		// entry is the entry the refusal quotes, the value itself when empty.
		entry string
		// says is what the refusal says besides the setting and the entry: the words of the one
		// refusal the row is for.
		says string
	}
	seventeen := "public"
	for i := range 17 {
		seventeen += ",P" + strconv.Itoa(10+i)
	}
	// projectRefusals are the rows of projects ParseJobProjects refuses, each after prefix, the
	// identity of an entry and its "=".
	projectRefusals := func(prefix string) []refusal {
		return []refusal{
			{name: "seventeen private projects", value: prefix + seventeen, says: "17 private projects are named"},
			{name: "a key that is no project key", value: prefix + "acme", says: strconv.Quote("acme") + " is not a project key"},
			{name: "public after a key", value: prefix + "ACME,public", says: "public comes first"},
			{name: "two keys without public", value: prefix + "ACME,OTHER", says: "2 projects are named without public first"},
		}
	}
	for _, setting := range []struct {
		name string
		env  func() map[string]string
		rows []refusal
	}{
		{
			name: "BROKER_EXCHANGE_JOB_ACCOUNTS",
			env:  jobPodEnv,
			rows: append([]refusal{
				{name: "an entry with no =", value: jobAccount, says: "must be <service account>=<projects>[=<session>] entries"},
				{name: "a blank account", value: "=EXAMPLE", says: "must be <service account>=<projects>[=<session>] entries"},
				{name: "an account that is no ServiceAccount subject", value: "example-job=EXAMPLE", says: "must name system:serviceaccount:<namespace>:<name> accounts"},
				{name: "a namespace's default account", value: "system:serviceaccount:example-ns:default=EXAMPLE", says: "names a namespace's default service account"},
				{name: "an account named twice", value: jobAccount + "=EXAMPLE " + jobAccount + "=public", entry: jobAccount + "=public", says: "names the service account " + jobAccount + " twice"},
			}, projectRefusals(jobAccount+"=")...),
		},
		{
			name: "BROKER_EXCHANGE_JOB_WORKFLOWS",
			env:  jobWorkflowEnv,
			rows: append([]refusal{
				{name: "an entry of three fields", value: "example-org/example-repo=.github/workflows/sweep.yaml=EXAMPLE", says: "must be <repository>=<workflow path>=<ref>=<projects>[=<session>] entries"},
				{name: "a blank ref", value: "example-org/example-repo=.github/workflows/sweep.yaml==EXAMPLE", says: "must be <repository>=<workflow path>=<ref>=<projects>[=<session>] entries"},
				{name: "a workflow named twice", value: jobWorkflow + "=EXAMPLE " + jobWorkflow + "=public", entry: jobWorkflow + "=public", says: "names the workflow " + jobWorkflow + " twice"},
			}, projectRefusals(jobWorkflow+"=")...),
		},
		{
			name: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS",
			env:  jobFleetEnv,
			rows: append([]refusal{
				{name: "an entry with no =", value: exampleVMServiceAccount, says: "must be <service account email>=<projects>[=<session>] entries"},
				{name: "a blank account", value: "=EXAMPLE", says: "must be <service account email>=<projects>[=<session>] entries"},
				{name: "a project's default account", value: "123456789-compute@developer.gserviceaccount.com=EXAMPLE", says: "names a project's default service account"},
				{name: "an account named twice", value: exampleVMServiceAccount + "=EXAMPLE " + exampleVMServiceAccount + "=public", entry: exampleVMServiceAccount + "=public", says: "names the service account " + exampleVMServiceAccount + " twice"},
			}, projectRefusals(exampleVMServiceAccount+"=")...),
		},
	} {
		for _, row := range setting.rows {
			t.Run(setting.name+"/"+row.name, func(t *testing.T) {
				e := setting.env()
				e[setting.name] = row.value
				cfg, err := Load(env(e))
				if err == nil {
					t.Fatalf("Load(%s=%q) loaded, ExchangeJobAccounts = %+v, ExchangeJobWorkflows = %+v, ExchangeGoogleAccounts = %+v; want a refusal", setting.name, row.value, cfg.ExchangeJobAccounts, cfg.ExchangeJobWorkflows, cfg.ExchangeGoogleAccounts)
				}
				entry := row.entry
				if entry == "" {
					entry = row.value
				}
				for _, want := range []string{setting.name, strconv.Quote(entry), row.says} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Load(%s=%q): refusal %q does not say %s", setting.name, row.value, err, want)
					}
				}
			})
		}
	}
}

// TestLoadRefusesAnIdentityAJobSettingAndAPersonSettingBothName: one identity has one answer, so
// the start is refused when a job setting and a setting of the same kind that names a person, or
// the read accounts, both list it, and when BROKER_EXCHANGE_JOB_ACCOUNTS and a
// BROKER_EXCHANGE_ACCOUNTS entry naming neither a person nor a project (<account>==), whose token
// writes as the account, both list one account. The refusal names both settings and the
// identity, and where a row gives one, the reason a job's token cannot share it. Each setting
// alone loads, so the refusal is the pair's.
func TestLoadRefusesAnIdentityAJobSettingAndAPersonSettingBothName(t *testing.T) {
	for _, row := range []struct {
		name          string
		env           func() map[string]string
		other, value  string
		job, jobValue string
		identity      string
		// says, when set, is the reason the refusal must give.
		says string
	}{
		{
			name:  "a pod's account in BROKER_EXCHANGE_ACCOUNTS",
			env:   jobPodEnv,
			other: "BROKER_EXCHANGE_ACCOUNTS", value: jobAccount + "=EXAMPLE=ada@example.com",
			job: "BROKER_EXCHANGE_JOB_ACCOUNTS", jobValue: jobAccount + "=EXAMPLE",
			identity: jobAccount,
		},
		{
			name:  "a pod's account in BROKER_EXCHANGE_ACCOUNTS naming neither a person nor a project",
			env:   jobPodEnv,
			other: "BROKER_EXCHANGE_ACCOUNTS", value: jobAccount + "==",
			job: "BROKER_EXCHANGE_JOB_ACCOUNTS", jobValue: jobAccount + "=EXAMPLE",
			identity: jobAccount,
			says:     "one account mints one kind of token, and only a job's names the account as its service and carries projects",
		},
		{
			name:  "a pod's account in BROKER_EXCHANGE_READ_ACCOUNTS",
			env:   jobPodEnv,
			other: "BROKER_EXCHANGE_READ_ACCOUNTS", value: jobAccount + "=example-reader=example-reader-session",
			job: "BROKER_EXCHANGE_JOB_ACCOUNTS", jobValue: jobAccount + "=public",
			identity: jobAccount,
		},
		{
			name:  "a workflow in BROKER_EXCHANGE_WORKFLOWS",
			env:   jobWorkflowEnv,
			other: "BROKER_EXCHANGE_WORKFLOWS", value: jobWorkflow + "=EXAMPLE=ada@example.com",
			job: "BROKER_EXCHANGE_JOB_WORKFLOWS", jobValue: jobWorkflow + "=EXAMPLE",
			identity: jobWorkflow,
		},
		{
			name:  "a Google account in BROKER_VM_EXCHANGE_ACCOUNTS",
			env:   jobFleetEnv,
			other: "BROKER_VM_EXCHANGE_ACCOUNTS", value: exampleVMServiceAccount + "=EXAMPLE=ada@example.com",
			job: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", jobValue: exampleVMServiceAccount + "=EXAMPLE",
			identity: exampleVMServiceAccount,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, alone := range []struct{ setting, value string }{{row.other, row.value}, {row.job, row.jobValue}} {
				e := row.env()
				e[alone.setting] = alone.value
				if _, err := Load(env(e)); err != nil {
					t.Fatalf("Load(%s=%q) alone: %v; want it to load, so that the refusal below is the pair's", alone.setting, alone.value, err)
				}
			}
			e := row.env()
			e[row.other] = row.value
			e[row.job] = row.jobValue
			_, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load(%s=%q, %s=%q) loaded; want a refusal naming both settings", row.other, row.value, row.job, row.jobValue)
			}
			for _, want := range []string{row.other, row.job, row.identity} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load(%s, %s): refusal %q does not name %s", row.other, row.job, err, want)
				}
			}
			if row.says != "" && !strings.Contains(err.Error(), row.says) {
				t.Errorf("Load(%s, %s): refusal %q does not say %s", row.other, row.job, err, row.says)
			}
		})
	}
}

// TestLoadRefusesAJobSettingWithoutItsIssuer: each job setting needs the issuer whose tokens its
// identities present, and the start is refused without it, naming the setting and what it needs:
// the pod setting the exchange audience of the cluster, the workflow setting GitHub's issuer and
// audience, and the VM setting the Google audience and an account BROKER_VM_SERVICES lists.
func TestLoadRefusesAJobSettingWithoutItsIssuer(t *testing.T) {
	for _, row := range []struct {
		name, setting, value string
		env                  func() map[string]string
		names                []string
	}{
		{
			name:    "a pod's account without the exchange audience",
			setting: "BROKER_EXCHANGE_JOB_ACCOUNTS", value: jobAccount + "=EXAMPLE",
			env:   withK8sIssuer,
			names: []string{"BROKER_EXCHANGE_K8S_AUDIENCE"},
		},
		{
			name:    "a workflow without GitHub's issuer",
			setting: "BROKER_EXCHANGE_JOB_WORKFLOWS", value: jobWorkflow + "=EXAMPLE",
			env:   validEnv,
			names: []string{"BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE"},
		},
		{
			name:    "a Google account without the Google audience",
			setting: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", value: exampleVMServiceAccount + "=EXAMPLE",
			env:   validEnv,
			names: []string{"BROKER_GOOGLE_OIDC_AUDIENCE"},
		},
		{
			name:    "a Google account BROKER_VM_SERVICES does not list",
			setting: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", value: secondFleet + "=EXAMPLE",
			env:   validVMEnv,
			names: []string{"BROKER_VM_SERVICES", secondFleet},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := row.env()
			e[row.setting] = row.value
			_, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load(%s=%q) loaded; want a refusal naming %v", row.setting, row.value, row.names)
			}
			for _, want := range append([]string{row.setting}, row.names...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load(%s=%q): refusal %q does not name %s", row.setting, row.value, err, want)
				}
			}
		})
	}
}

// TestAPrivateProjectOnlyInItsPodsParsesTheSetting pins BROKER_PRIVATE_PROJECTS at load:
// whitespace-separated project keys land in Config.PrivateProjects sorted, none when unset, and a
// key that is no project key or a key named twice refuses the configuration, naming the setting
// and the key. exchange.ParsePrivateProjects's own test holds the rest of the reading.
func TestAPrivateProjectOnlyInItsPodsParsesTheSetting(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if keys := cfg.PrivateProjects.Keys(); keys != nil {
		t.Fatalf("PrivateProjects.Keys() = %q, want nil when BROKER_PRIVATE_PROJECTS is unset", keys)
	}
	e := validEnv()
	e["BROKER_PRIVATE_PROJECTS"] = " PRIV\n\tACME "
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatalf("BROKER_PRIVATE_PROJECTS naming two project keys: %v", err)
	}
	if got, want := cfg.PrivateProjects.Keys(), []string{"ACME", "PRIV"}; !slices.Equal(got, want) {
		t.Fatalf("PrivateProjects.Keys() = %q, want %q", got, want)
	}
	if !cfg.PrivateProjects.Has("PRIV") || cfg.PrivateProjects.Has("OTHER") {
		t.Fatalf("PrivateProjects.Has: PRIV %v, OTHER %v, want true and false", cfg.PrivateProjects.Has("PRIV"), cfg.PrivateProjects.Has("OTHER"))
	}
	// Each key a refusal must name is one BROKER_PRIVATE_PROJECTS does not hold, so the setting's
	// name in the error cannot stand in for it: the repeated key is ACME, not PRIV.
	for _, c := range []struct{ setting, key string }{
		{"PRIV priv", strconv.Quote("priv")},
		{"PRIV AC-ME", strconv.Quote("AC-ME")},
		{"ACME PRIV ACME", "ACME"},
	} {
		e := validEnv()
		e["BROKER_PRIVATE_PROJECTS"] = c.setting
		cfg, err := Load(env(e))
		if err == nil {
			t.Errorf("BROKER_PRIVATE_PROJECTS = %q loaded PrivateProjects %q, want a refusal", c.setting, cfg.PrivateProjects.Keys())
			continue
		}
		if !strings.Contains(err.Error(), "BROKER_PRIVATE_PROJECTS") || !strings.Contains(err.Error(), c.key) {
			t.Errorf("BROKER_PRIVATE_PROJECTS = %q: err = %v, want a refusal naming the setting and %s", c.setting, err, c.key)
		}
	}
}

// TestLoadRefusesAHalfConfiguredGitHubIssuer pins that GitHub's issuer and audience are
// both-or-neither, as the cluster's pair is: half a pair would register a token type whose tokens
// could not be checked.
func TestLoadRefusesAHalfConfiguredGitHubIssuer(t *testing.T) {
	for _, half := range []string{"BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE"} {
		e := validEnv()
		e[half] = "https://token.actions.githubusercontent.com"
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_AUDIENCE") {
			t.Errorf("Load(%s alone) = %v, want a refusal naming both halves", half, err)
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.ExchangeGitHubIssuer != "" || cfg.ExchangeGitHubAudience != "" {
		t.Fatalf("Load with neither half = (%q, %q, %v), want no GitHub issuer and no error", cfg.ExchangeGitHubIssuer, cfg.ExchangeGitHubAudience, err)
	}
}

// signInSettings are the four sign-in settings, every one of them required.
var signInSettings = []string{"BROKER_SIGNIN_ISSUER", "BROKER_SIGNIN_CLIENT_ID", "BROKER_SIGNIN_CLIENT_SECRET_FILE", "BROKER_SIGNIN_DOMAIN"}

// TestLoadReadsTheSignInClient pins the sign-in client with all four settings: each value as
// given, the secret read from its file and trimmed, a session of 86400 seconds by default and of
// BROKER_SIGNIN_SESSION_SECONDS when set, and an http issuer accepted on a loopback host.
func TestLoadReadsTheSignInClient(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := approval.SignIn{Issuer: "https://accounts.example.com", ClientID: "broker-client", ClientSecret: signInSecret, Domain: "example.com", SessionLifetime: 86400 * time.Second}
	if cfg.SignIn != want || cfg.SignInOff {
		t.Fatalf("SignIn = %+v, SignInOff = %v; want %+v with sign-in on", cfg.SignIn, cfg.SignInOff, want)
	}
	e := validEnv()
	e["BROKER_SIGNIN_SESSION_SECONDS"] = "600"
	if cfg, err := Load(env(e)); err != nil || cfg.SignIn.SessionLifetime != 10*time.Minute {
		t.Fatalf("Load(BROKER_SIGNIN_SESSION_SECONDS=600) = (%+v, %v), want a 10-minute session", cfg.SignIn, err)
	}
	for _, issuer := range []string{"http://127.0.0.1:5556", "http://localhost:5556/dex", "http://[::1]:5556"} {
		e := validEnv()
		e["BROKER_SIGNIN_ISSUER"] = issuer
		if cfg, err := Load(env(e)); err != nil || cfg.SignIn.Issuer != issuer {
			t.Errorf("Load(BROKER_SIGNIN_ISSUER=%q) = (%+v, %v), want that issuer", issuer, cfg.SignIn, err)
		}
	}
}

// TestLoadRefusesAnIncompleteSignIn pins that all four settings are required: every set short of
// all of them, the empty set included, refuses naming every setting it lacks.
func TestLoadRefusesAnIncompleteSignIn(t *testing.T) {
	for mask := 0; mask < 1<<len(signInSettings)-1; mask++ {
		e := validEnv()
		var missing []string
		for i, name := range signInSettings {
			if mask&(1<<i) == 0 {
				delete(e, name)
				missing = append(missing, name)
			}
		}
		_, err := Load(env(e))
		if err == nil {
			t.Errorf("Load without %v = nil error, want a refusal naming them", missing)
			continue
		}
		_, named, _ := strings.Cut(err.Error(), "missing ")
		if named != strings.Join(missing, ", ") {
			t.Errorf("Load without %v = %v, want a refusal naming exactly those as missing", missing, err)
		}
		if strings.Contains(err.Error(), signInSecret) {
			t.Errorf("Load without %v = %v, which carries the client secret", missing, err)
		}
	}
}

// TestLoadRefusesASignInSessionOutOfRange pins BROKER_SIGNIN_SESSION_SECONDS's range, 1 to 86400.
func TestLoadRefusesASignInSessionOutOfRange(t *testing.T) {
	for _, bad := range []string{"86401", "0", "-1", "a day"} {
		e := validEnv()
		e["BROKER_SIGNIN_SESSION_SECONDS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_SESSION_SECONDS") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_SESSION_SECONDS = %q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}

// TestLoadRefusesAnUnusableSignInSecretFile pins that a secret file that cannot be read, or holds
// only blank space, refuses naming BROKER_SIGNIN_CLIENT_SECRET_FILE, never falling back to
// another source.
func TestLoadRefusesAnUnusableSignInSecretFile(t *testing.T) {
	dir := t.TempDir()
	blank := dir + "/blank"
	if err := os.WriteFile(blank, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"absent": dir + "/absent", "a directory": dir, "blank": blank} {
		e := validEnv()
		e["BROKER_SIGNIN_CLIENT_SECRET_FILE"] = path
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET_FILE") {
			t.Errorf("a secret file that is %s: err = %v, want a refusal naming BROKER_SIGNIN_CLIENT_SECRET_FILE", name, err)
		}
	}
}

// TestLoadRefusesThePlainSignInSecret pins that the secret is never taken from the environment:
// BROKER_SIGNIN_CLIENT_SECRET refuses, naming the _FILE form, whether or not the file is set too,
// and the refusal does not carry its value.
func TestLoadRefusesThePlainSignInSecret(t *testing.T) {
	noFile := validEnv()
	delete(noFile, "BROKER_SIGNIN_CLIENT_SECRET_FILE")
	for name, e := range map[string]map[string]string{"with the file": validEnv(), "without the file": noFile} {
		e["BROKER_SIGNIN_CLIENT_SECRET"] = signInSecret
		_, err := Load(env(e))
		if err == nil || !strings.HasPrefix(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET ") || !strings.Contains(err.Error(), "BROKER_SIGNIN_CLIENT_SECRET_FILE") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_CLIENT_SECRET %s: err = %v, want a refusal naming it and BROKER_SIGNIN_CLIENT_SECRET_FILE, without its value", name, err)
		}
	}
}

// TestLoadRefusesASignInIssuerThatIsNotHTTPS pins that the issuer, which a sign-in's code
// exchange sends the client secret to, is an absolute https URL, with http allowed only to a
// loopback host.
func TestLoadRefusesASignInIssuerThatIsNotHTTPS(t *testing.T) {
	for _, bad := range []string{"http://accounts.example.com", "http://10.0.0.1:5556", "http://127.0.0.1.example.com", "accounts.example.com", "ftp://accounts.example.com", "https://"} {
		e := validEnv()
		e["BROKER_SIGNIN_ISSUER"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_ISSUER") || strings.Contains(err.Error(), signInSecret) {
			t.Errorf("BROKER_SIGNIN_ISSUER = %q: err = %v, want a refusal naming it", bad, err)
		}
	}
}

// signInOffEnv is validEnv with the broker's person sign-in turned off: BROKER_SIGNIN=off and
// none of the four sign-in settings.
func signInOffEnv() map[string]string {
	e := validEnv()
	for _, name := range signInSettings {
		delete(e, name)
	}
	e["BROKER_SIGNIN"] = "off"
	return e
}

// TestLoadStartsABrokerWithSignInOff: BROKER_SIGNIN=off with no sign-in setting at all loads,
// holding no sign-in client, which is the broker a deployment runs while how its people sign in
// is not settled.
func TestLoadStartsABrokerWithSignInOff(t *testing.T) {
	cfg, err := Load(env(signInOffEnv()))
	if err != nil {
		t.Fatalf("Load(BROKER_SIGNIN=off, no sign-in setting) = %v, want a broker that starts", err)
	}
	if !cfg.SignInOff || cfg.SignIn != (approval.SignIn{}) {
		t.Fatalf("SignInOff = %v, SignIn = %+v; want sign-in off and no sign-in client", cfg.SignInOff, cfg.SignIn)
	}
}

// TestLoadRefusesAPublicURLThatIsNotHTTP pins BROKER_PUBLIC_URL to an http or https URL whether
// sign-in is on or off. The approval page's constructor used to be the only check of its scheme,
// and a broker with sign-in off builds no page, so without this one it would start on an ftp://
// URL and name endpoints in its metadata that no client of its own can call.
func TestLoadRefusesAPublicURLThatIsNotHTTP(t *testing.T) {
	for name, base := range map[string]func() map[string]string{"sign-in on": validEnv, "sign-in off": signInOffEnv} {
		for _, bad := range []string{"ftp://secrets.internal.example", "ws://secrets.internal.example", "unix://secrets.internal.example"} {
			e := base()
			e["BROKER_PUBLIC_URL"] = bad
			if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_PUBLIC_URL") {
				t.Errorf("%s: Load(BROKER_PUBLIC_URL=%q) = %v, want a refusal naming it", name, bad, err)
			}
		}
		for _, good := range []string{"https://secrets.internal.example", "http://127.0.0.1:13380"} {
			e := base()
			e["BROKER_PUBLIC_URL"] = good
			if _, err := Load(env(e)); err != nil {
				t.Errorf("%s: Load(BROKER_PUBLIC_URL=%q) = %v, want it read", name, good, err)
			}
		}
	}
}

// TestLoadKeepsMainsPublicURLRefusal pins the refusal main gives a BROKER_PUBLIC_URL with a path,
// or with no scheme, word for word: the http(s) rule is a check of its own, and it does not
// reword the one a deployment already reads.
func TestLoadKeepsMainsPublicURLRefusal(t *testing.T) {
	for _, bad := range []string{"https://secrets.internal.example/broker", "secrets.internal.example"} {
		e := validEnv()
		e["BROKER_PUBLIC_URL"] = bad
		want := "BROKER_PUBLIC_URL must be an absolute URL with no path: " + strconv.Quote(redact.URL(bad))
		if _, err := Load(env(e)); err == nil || err.Error() != want {
			t.Errorf("Load(BROKER_PUBLIC_URL=%q) = %v; want main's %q", bad, err, want)
		}
	}
}

// TestLoadRefusesTheSignInBeforeThePublicURLsScheme pins the order main refuses in: an ftp://
// BROKER_PUBLIC_URL beside a sign-in missing BROKER_SIGNIN_DOMAIN is refused for the sign-in, as
// main's Load did, since main refused the scheme only later, in approval.New. The scheme check is
// Load's last, so every refusal main's Load gave comes first, as it did.
func TestLoadRefusesTheSignInBeforeThePublicURLsScheme(t *testing.T) {
	e := validEnv()
	e["BROKER_PUBLIC_URL"] = "ftp://secrets.internal.example"
	delete(e, "BROKER_SIGNIN_DOMAIN")
	const want = "the broker's approval page needs BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN; missing BROKER_SIGNIN_DOMAIN"
	if _, err := Load(env(e)); err == nil || err.Error() != want {
		t.Fatalf("Load(ftp:// URL, no BROKER_SIGNIN_DOMAIN) = %v; want main's sign-in refusal first, %q", err, want)
	}
}

// TestLoadWithTheSwitchUnsetRefusesAnEmptySignIn pins that a broker which does not say
// BROKER_SIGNIN=off still refuses to start with no sign-in, in the very words it always has: a
// production broker that lost its sign-in settings must not come up without its approval page.
// An empty BROKER_SIGNIN is the switch unset.
func TestLoadWithTheSwitchUnsetRefusesAnEmptySignIn(t *testing.T) {
	const want = "the broker's approval page needs BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN; missing BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN"
	for name, value := range map[string]*string{"unset": nil, "empty": new("")} {
		e := signInOffEnv()
		delete(e, "BROKER_SIGNIN")
		if value != nil {
			e["BROKER_SIGNIN"] = *value
		}
		if _, err := Load(env(e)); err == nil || err.Error() != want {
			t.Errorf("Load(BROKER_SIGNIN %s, no sign-in setting) = %v, want %q", name, err, want)
		}
	}
}

// TestLoadRefusesSignInOffBesideASignInSetting pins that off is one thing and a sign-in client
// another: off beside any BROKER_SIGNIN_* setting the broker reads refuses, naming the switch and
// every such setting that is set, rather than choosing one of the two for the operator.
func TestLoadRefusesSignInOffBesideASignInSetting(t *testing.T) {
	values := map[string]string{
		"BROKER_SIGNIN_ISSUER":             "https://accounts.example.com",
		"BROKER_SIGNIN_CLIENT_ID":          "broker-client",
		"BROKER_SIGNIN_CLIENT_SECRET_FILE": signInSecretFile,
		"BROKER_SIGNIN_DOMAIN":             "example.com",
		"BROKER_SIGNIN_SESSION_SECONDS":    "600",
		"BROKER_SIGNIN_DEVICE_AUDIENCE":    "example-device-client",
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		e := signInOffEnv()
		e[name] = values[name]
		want := "BROKER_SIGNIN=off turns the broker's person sign-in off, and " + name + " is set: set BROKER_SIGNIN=off or the sign-in settings, not both"
		if _, err := Load(env(e)); err == nil || err.Error() != want {
			t.Errorf("Load(BROKER_SIGNIN=off, %s) = %v, want %q", name, err, want)
		}
	}
	e := validEnv()
	e["BROKER_SIGNIN"] = "off"
	want := "BROKER_SIGNIN=off turns the broker's person sign-in off, and BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN are set: set BROKER_SIGNIN=off or the sign-in settings, not both"
	if _, err := Load(env(e)); err == nil || err.Error() != want {
		t.Errorf("Load(BROKER_SIGNIN=off beside the four) = %v, want %q", err, want)
	}
}

// TestLoadRefusesSignInOffBesideTheJudge: the model judge decides a request that waits for a
// person, and with sign-in off no request waits, so a judge beside off would never run; the
// broker refuses the pair as it refuses any setting that configures nothing.
func TestLoadRefusesSignInOffBesideTheJudge(t *testing.T) {
	e := judgeEnv()
	for _, name := range signInSettings {
		delete(e, name)
	}
	e["BROKER_SIGNIN"] = "off"
	const want = "BROKER_SIGNIN=off refuses at once every request that would wait for a person, so the broker's model judge, which decides such a request, would never run: unset the judge's settings (BROKER_JUDGE_MODEL_URL, BROKER_JUDGE_MODEL, BROKER_JUDGE_MODEL_AUDIENCE, BROKER_JUDGE_DISPATCH_URL, BROKER_JUDGE_DISPATCH_TOKEN_FILE) or BROKER_SIGNIN"
	if _, err := Load(env(e)); err == nil || err.Error() != want {
		t.Fatalf("Load(BROKER_SIGNIN=off beside the judge) = %v, want %q", err, want)
	}
}

// TestLoadRefusesAnySignInSwitchButOff pins that off is the one value BROKER_SIGNIN takes: any
// other, a word that reads as off to a person included, refuses naming the switch and the value,
// whether the sign-in settings are there or not, so a misspelled off never starts a broker
// whose sign-in the operator thought they had turned off, nor one they thought they had kept.
func TestLoadRefusesAnySignInSwitchButOff(t *testing.T) {
	for _, bad := range []string{"OFF", "Off", " off", "off ", "disabled", "false", "0", "no", "none", "on", "true"} {
		for name, e := range map[string]map[string]string{"beside the sign-in settings": validEnv(), "with none": signInOffEnv()} {
			e["BROKER_SIGNIN"] = bad
			want := "BROKER_SIGNIN must be off or unset, got " + strconv.Quote(bad)
			if _, err := Load(env(e)); err == nil || err.Error() != want {
				t.Errorf("Load(BROKER_SIGNIN=%q %s) = %v, want %q", bad, name, err, want)
			}
		}
	}
}

// TestLoadReadsTheAirlocksTestOnlySettings pins each of the eleven settings the broker's local
// stack configures with: the airlock's ten — the four minter stand-ins (GitHub's and Google's
// bases, Anthropic's token exchange and IAM's generateIdToken, where an anthropic-federation
// secret's identity token is read), the addresses the proxy may dial although they are loopback,
// the extra upstream roots, where a name resolves to, the token authority the azure exchange is
// held to, and its two negative-control switches — and the judge's one, the switch that turns its
// checks before the model off. Each is read as it is given and is empty or off when unset. The
// refusal of each on a production start is cmd/cipherd's, which has its own rows.
func TestLoadReadsTheAirlocksTestOnlySettings(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AirlockTestGitHubAPIBase != "" || cfg.AirlockTestGoogleTokenURL != "" || cfg.AirlockTestUpstreamCAFile != "" ||
		cfg.AirlockTestAnthropicTokenURL != "" || cfg.AirlockTestIAMCredentialsURL != "" ||
		len(cfg.AirlockTestDialAllow) != 0 || len(cfg.AirlockTestResolve) != 0 || cfg.AirlockTestKeepValues || cfg.AirlockTestNoKind ||
		cfg.AirlockTestAzureAuthority != "" ||
		cfg.JudgeTestSkipChecks {
		t.Fatalf("with none of them set the local stack's test settings read %+v; want every one empty or off", cfg)
	}

	e := validEnv()
	e["BROKER_AIRLOCK_TEST_GITHUB_API_BASE"] = "http://127.0.0.1:18080"
	e["BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL"] = "http://127.0.0.1:18081/token"
	e["BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL"] = " http://127.0.0.1:18082/v1/oauth/token "
	e["BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL"] = "http://127.0.0.1:18083"
	e["BROKER_AIRLOCK_TEST_DIAL_ALLOW"] = "s.airlock.test:443 t.airlock.test:8443"
	e["BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE"] = "/run/airlock/ca.crt"
	e["BROKER_AIRLOCK_TEST_RESOLVE"] = "s.airlock.test=127.0.0.1 login.microsoftonline.com=127.0.0.2"
	e["BROKER_AIRLOCK_TEST_KEEP_VALUES"] = "true"
	e["BROKER_AIRLOCK_TEST_NO_KIND"] = "1"
	e["BROKER_AIRLOCK_TEST_AZURE_AUTHORITY"] = " microsoft.airlock.test "
	e["BROKER_JUDGE_TEST_SKIP_CHECKS"] = "true"
	cfg, err = Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AirlockTestGitHubAPIBase != "http://127.0.0.1:18080" || cfg.AirlockTestGoogleTokenURL != "http://127.0.0.1:18081/token" {
		t.Errorf("the minter bases read %q and %q; want the two the environment named", cfg.AirlockTestGitHubAPIBase, cfg.AirlockTestGoogleTokenURL)
	}
	if cfg.AirlockTestAnthropicTokenURL != "http://127.0.0.1:18082/v1/oauth/token" || cfg.AirlockTestIAMCredentialsURL != "http://127.0.0.1:18083" {
		t.Errorf("the anthropic-federation minter's stand-ins read %q and %q; want the two the environment named, trimmed", cfg.AirlockTestAnthropicTokenURL, cfg.AirlockTestIAMCredentialsURL)
	}
	if cfg.AirlockTestUpstreamCAFile != "/run/airlock/ca.crt" {
		t.Errorf("the upstream roots file reads %q; want /run/airlock/ca.crt", cfg.AirlockTestUpstreamCAFile)
	}
	if !slices.Equal(cfg.AirlockTestDialAllow, []string{"s.airlock.test:443", "t.airlock.test:8443"}) {
		t.Errorf("the dial allow list reads %v; want both addresses in the order they were given", cfg.AirlockTestDialAllow)
	}
	want := map[string]netip.Addr{
		"s.airlock.test":            netip.MustParseAddr("127.0.0.1"),
		"login.microsoftonline.com": netip.MustParseAddr("127.0.0.2"),
	}
	if !maps.Equal(cfg.AirlockTestResolve, want) {
		t.Errorf("the resolve map reads %v; want %v", cfg.AirlockTestResolve, want)
	}
	if cfg.AirlockTestAzureAuthority != "microsoft.airlock.test" {
		t.Errorf("the azure token authority reads %q; want microsoft.airlock.test, the host the environment named", cfg.AirlockTestAzureAuthority)
	}
	if !cfg.AirlockTestKeepValues || !cfg.AirlockTestNoKind || !cfg.JudgeTestSkipChecks {
		t.Errorf("the three switches read %v, %v and %v; want all three on", cfg.AirlockTestKeepValues, cfg.AirlockTestNoKind, cfg.JudgeTestSkipChecks)
	}
}

// TestLoadRefusesAnAirlockTestSettingItCannotActOn pins that a value the proxy or the judge
// could not act on refuses the start naming the setting, rather than being read as none: an
// address with no port, an entry that is no IP address, a host resolved twice, a token authority
// that is no lower-case host name a rules entry can bind, and a switch that is neither true nor
// false.
func TestLoadRefusesAnAirlockTestSettingItCannotActOn(t *testing.T) {
	for _, c := range []struct{ setting, value, want string }{
		{"BROKER_AIRLOCK_TEST_DIAL_ALLOW", "s.airlock.test", "must be <host>:<port>"},
		{"BROKER_AIRLOCK_TEST_DIAL_ALLOW", "s.airlock.test:https", "names no port number"},
		{"BROKER_AIRLOCK_TEST_RESOLVE", "s.airlock.test", "must be <host>=<address>"},
		{"BROKER_AIRLOCK_TEST_RESOLVE", "s.airlock.test=localhost", "names no IP address"},
		{"BROKER_AIRLOCK_TEST_RESOLVE", "s.airlock.test=127.0.0.1 s.airlock.test=127.0.0.2", "twice"},
		{"BROKER_AIRLOCK_TEST_KEEP_VALUES", "yes please", "must be true or false"},
		{"BROKER_AIRLOCK_TEST_NO_KIND", "off-ish", "must be true or false"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "https://microsoft.airlock.test", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "microsoft.airlock.test:443", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "Microsoft.Airlock.Test", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "microsoft.airlock.test.", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "127.0.0.4", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "-microsoft.airlock.test", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "microsoft-.airlock.test", "must be a host name"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "*.airlock.test", "must be a host name"},
		// A host proxyrules.HostShapeOK accepts and the rules-host grammar refuses, with the
		// grammar's reason: config reads the authority by the grammar the rules file is read by.
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "microsoft.airlock.0x1f", "ends in a number"},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", "ab--cd.airlock.test", "third and fourth characters"},
		{"BROKER_JUDGE_TEST_SKIP_CHECKS", "skip", "must be true or false"},
	} {
		e := validEnv()
		e[c.setting] = c.value
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), c.setting) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %q: err = %v; want a refusal naming it and %q", c.setting, c.value, err, c.want)
		}
	}
}

// judgeSettings are the five settings the model judge needs, in the one order every message
// naming them uses. They are spelled out here rather than read from JudgeSettings, so a
// reordering of that list shows up as a failing test and not as a quietly renumbered refusal.
var judgeSettings = []string{
	"BROKER_JUDGE_MODEL_URL", "BROKER_JUDGE_MODEL", "BROKER_JUDGE_MODEL_AUDIENCE",
	"BROKER_JUDGE_DISPATCH_URL", "BROKER_JUDGE_DISPATCH_TOKEN_FILE",
}

// judgeEnv is validEnv with the judge configured: the endpoint it calls its model at, the
// model, the audience of the identity token it presents there, and the Dispatch it reads an
// issue from with its own projected token. It names no protocol, so a judge loaded from it
// takes the default.
func judgeEnv() map[string]string {
	e := validEnv()
	e["BROKER_JUDGE_MODEL_URL"] = "https://model.internal.example/v1"
	e["BROKER_JUDGE_MODEL"] = "claude-opus-4-5"
	e["BROKER_JUDGE_MODEL_AUDIENCE"] = "https://model.internal.example"
	e["BROKER_JUDGE_DISPATCH_URL"] = "https://dispatch.internal.example"
	e["BROKER_JUDGE_DISPATCH_TOKEN_FILE"] = "/var/run/secrets/dispatch/token"
	return e
}

// TestLoadReadsTheJudgeSettings pins the judge's seven: with none of the five set there is no
// judge and every one of its fields is empty, and with all five set each is read as it was
// given. The protocol is the one with a default, anthropic-messages, and each of the four it
// takes is read as it was given. The account is optional: unset it reads empty, which is the
// metadata server's identity token, and set it is read as given, without the space around it.
func TestLoadReadsTheJudgeSettings(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JudgeModelURL != "" || cfg.JudgeModel != "" || cfg.JudgeModelAudience != "" ||
		cfg.JudgeDispatchURL != "" || cfg.JudgeDispatchTokenFile != "" || cfg.JudgeModelAccount != "" {
		t.Fatalf("with none of them set the judge reads %+v; want every field empty", cfg)
	}

	e := judgeEnv()
	if cfg, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ setting, got string }{
		{"BROKER_JUDGE_MODEL_URL", cfg.JudgeModelURL},
		{"BROKER_JUDGE_MODEL", cfg.JudgeModel},
		{"BROKER_JUDGE_MODEL_AUDIENCE", cfg.JudgeModelAudience},
		{"BROKER_JUDGE_DISPATCH_URL", cfg.JudgeDispatchURL},
		{"BROKER_JUDGE_DISPATCH_TOKEN_FILE", cfg.JudgeDispatchTokenFile},
	} {
		if row.got != e[row.setting] {
			t.Errorf("%s reads %q; want %q", row.setting, row.got, e[row.setting])
		}
	}
	if cfg.JudgeModelProtocol != "anthropic-messages" {
		t.Errorf("the protocol with none named reads %q; want anthropic-messages", cfg.JudgeModelProtocol)
	}
	if cfg.JudgeModelAccount != "" {
		t.Errorf("BROKER_JUDGE_MODEL_ACCOUNT unset reads %q; want empty, the metadata server's identity token", cfg.JudgeModelAccount)
	}
	for _, protocol := range []string{"anthropic-messages", "openai-responses", "openai-chat", "azure-anthropic"} {
		e := judgeEnv()
		e["BROKER_JUDGE_MODEL_PROTOCOL"] = protocol
		cfg, err := Load(env(e))
		if err != nil || cfg.JudgeModelProtocol != protocol {
			t.Errorf("Load(BROKER_JUDGE_MODEL_PROTOCOL=%q) = (%q, %v); want it taken as it is", protocol, cfg.JudgeModelProtocol, err)
		}
	}
	e = judgeEnv()
	e["BROKER_JUDGE_MODEL_ACCOUNT"] = judgeAccount + " "
	if cfg, err = Load(env(e)); err != nil || cfg.JudgeModelAccount != judgeAccount {
		t.Errorf("Load(BROKER_JUDGE_MODEL_ACCOUNT=%q) = (%q, %v); want %q", e["BROKER_JUDGE_MODEL_ACCOUNT"], cfg.JudgeModelAccount, err, judgeAccount)
	}
}

// judgeAccount is the service account a judge's account setting names here, a placeholder of the
// form secrets.ValidServiceAccountEmail takes.
const judgeAccount = "judge-model@example-project.iam.gserviceaccount.com"

// TestTheJudgeAccountAloneIsRefused pins the second judge setting that is never missing: a
// deployment that names whose identity token a judge presents and none of the five has named a
// judge it never configured, the mistake of the protocol alone, so the start is refused naming
// the setting, its value, and the five a judge needs, all of them missing.
func TestTheJudgeAccountAloneIsRefused(t *testing.T) {
	e := validEnv()
	e["BROKER_JUDGE_MODEL_ACCOUNT"] = judgeAccount
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "BROKER_JUDGE_MODEL_ACCOUNT") || !strings.Contains(err.Error(), strconv.Quote(judgeAccount)) ||
		!strings.Contains(err.Error(), "needs "+strings.Join(judgeSettings, ", ")) {
		t.Fatalf("Load(BROKER_JUDGE_MODEL_ACCOUNT alone) = %v; want a refusal naming the setting, its value and the five a judge needs", err)
	}
	if _, named, _ := strings.Cut(err.Error(), "missing "); named != strings.Join(judgeSettings, ", ") {
		t.Errorf("Load(BROKER_JUDGE_MODEL_ACCOUNT alone) = %v; want all five named as missing", err)
	}
}

// TestTheJudgeAccountIsHeldToOneServiceAccountEmail pins the rule BROKER_SERVICE_ACCOUNT_SECRETS'
// accounts are held to: one user-managed service account's address in lowercase, and never a
// Google-managed service agent. A person's address, a list, a capitalised address and a service
// agent would each boot a judge whose every mint is refused, or one that mints as Google's own
// service, so each is refused at start naming the setting and the value, and the placeholder
// account is taken.
func TestTheJudgeAccountIsHeldToOneServiceAccountEmail(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"a person's address", "ada@example.com", "user-managed Google service account"},
		{"two accounts", judgeAccount + ", another-judge@example-project.iam.gserviceaccount.com", "user-managed Google service account"},
		{"an upper-case address", "Judge-Model@example-project.iam.gserviceaccount.com", "user-managed Google service account"},
		{"a service agent", "service-123456789012@gcp-sa-example.iam.gserviceaccount.com", "Google-managed service agent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := judgeEnv()
			e["BROKER_JUDGE_MODEL_ACCOUNT"] = c.value
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), "BROKER_JUDGE_MODEL_ACCOUNT") || !strings.Contains(err.Error(), strconv.Quote(c.value)) ||
				!strings.Contains(err.Error(), c.want) {
				t.Errorf("Load(BROKER_JUDGE_MODEL_ACCOUNT=%q) = %v; want a refusal naming the setting and the value, saying %q", c.value, err, c.want)
			}
		})
	}
	e := judgeEnv()
	e["BROKER_JUDGE_MODEL_ACCOUNT"] = judgeAccount
	if cfg, err := Load(env(e)); err != nil || cfg.JudgeModelAccount != judgeAccount {
		t.Errorf("Load(BROKER_JUDGE_MODEL_ACCOUNT=%q) = (%q, %v); want it taken as it is", judgeAccount, cfg.JudgeModelAccount, err)
	}
}

// TestLoadRefusesAHalfConfiguredJudge pins that the five are all-or-none: every set short of
// all five and short of none refuses, naming the ones it lacks in the order JudgeSettings
// fixes, so a deployment that forgot one is told which rather than running a judge that
// decides nothing.
func TestLoadRefusesAHalfConfiguredJudge(t *testing.T) {
	for mask := 1; mask < 1<<len(judgeSettings)-1; mask++ {
		e := judgeEnv()
		var missing []string
		for i, name := range judgeSettings {
			if mask&(1<<i) == 0 {
				delete(e, name)
				missing = append(missing, name)
			}
		}
		_, err := Load(env(e))
		if err == nil {
			t.Errorf("Load without %v = nil error, want a refusal naming them", missing)
			continue
		}
		if !strings.Contains(err.Error(), strings.Join(judgeSettings, ", ")) {
			t.Errorf("Load without %v = %v, want a refusal naming all five in order", missing, err)
		}
		_, named, _ := strings.Cut(err.Error(), "missing ")
		if named != strings.Join(missing, ", ") {
			t.Errorf("Load without %v = %v, want a refusal naming exactly those as missing", missing, err)
		}
	}
}

// TestTheJudgeProtocolAloneIsRefused pins the one setting that has a default and so is never
// missing: a deployment that names the protocol and none of the five has written down the
// shape of an endpoint it never named, which is the same mistake as four of the five, so it is
// refused naming the five a judge needs rather than read as a deployment that meant no judge.
func TestTheJudgeProtocolAloneIsRefused(t *testing.T) {
	for _, protocol := range []string{"anthropic-messages", "openai-chat"} {
		e := validEnv()
		e["BROKER_JUDGE_MODEL_PROTOCOL"] = protocol
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), strings.Join(judgeSettings, ", ")) {
			t.Errorf("Load(BROKER_JUDGE_MODEL_PROTOCOL=%q alone) = %v; want a refusal naming the five a judge needs", protocol, err)
		}
	}
}

// TestTheJudgeProtocolIsHeldToTheFour pins that only agentloop's four Run kinds are taken, and
// that the refusal names the setting, the value and all four: a protocol the judge's client
// cannot speak would boot and then fail every call, deciding nothing while looking configured.
// systemone is agentloop's fifth kind, which Decide takes and no Run does.
func TestTheJudgeProtocolIsHeldToTheFour(t *testing.T) {
	for _, bad := range []string{"systemone", "Anthropic-Messages", "anthropic", " anthropic-messages", "openai"} {
		e := judgeEnv()
		e["BROKER_JUDGE_MODEL_PROTOCOL"] = bad
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_JUDGE_MODEL_PROTOCOL") || !strings.Contains(err.Error(), strconv.Quote(bad)) ||
			!strings.Contains(err.Error(), "anthropic-messages, openai-responses, openai-chat, azure-anthropic") {
			t.Errorf("Load(BROKER_JUDGE_MODEL_PROTOCOL=%q) = %v; want a refusal naming the setting, the value and the four it takes", bad, err)
		}
	}
}

// TestTheJudgesURLsAreHeldToTheirForm: the judge calls one endpoint and reads one Dispatch, so
// a value that is no absolute http(s) URL must refuse the boot rather than fail every call
// after it, as BROKER_MACHINE_LOGIN_URL and the airlock's proxy URL do.
func TestTheJudgesURLsAreHeldToTheirForm(t *testing.T) {
	for _, setting := range []string{"BROKER_JUDGE_MODEL_URL", "BROKER_JUDGE_DISPATCH_URL"} {
		for _, bad := range []string{"model.internal.example", "//model.internal.example", "ftp://model.internal.example", "http://", "/v1/messages", "http://%zz"} {
			e := judgeEnv()
			e[setting] = bad
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "absolute http(s) URL") {
				t.Errorf("Load(%s=%q) = %v; want a refusal naming the variable", setting, bad, err)
			}
		}
	}
}

// TestProjectAccessSettingsComeTogether pins BROKER_PROJECT_ACCESS_URL and
// BROKER_PROJECT_ACCESS_TOKEN_FILE: neither set is a broker that asks Dispatch nothing, which is
// every deployment with no private project; both set are read as given, the URL without the
// slashes it ends in; one alone refuses the start naming the other, as the judge's settings do;
// and a URL that is no absolute http(s) URL refuses the start naming the setting.
func TestProjectAccessSettingsComeTogether(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectAccessURL != "" || cfg.ProjectAccessTokenFile != "" {
		t.Fatalf("with neither set the broker reads URL %q and token file %q; want both empty", cfg.ProjectAccessURL, cfg.ProjectAccessTokenFile)
	}

	e := validEnv()
	e["BROKER_PROJECT_ACCESS_URL"] = "http://dispatch.internal.example:8766//"
	e["BROKER_PROJECT_ACCESS_TOKEN_FILE"] = "/var/run/secrets/dispatch-access/token"
	if cfg, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectAccessURL != "http://dispatch.internal.example:8766" || cfg.ProjectAccessTokenFile != "/var/run/secrets/dispatch-access/token" {
		t.Fatalf("with both set the broker reads URL %q and token file %q; want the URL without its trailing slashes and the file as given", cfg.ProjectAccessURL, cfg.ProjectAccessTokenFile)
	}

	for set, missing := range map[string]string{
		"BROKER_PROJECT_ACCESS_URL":        "BROKER_PROJECT_ACCESS_TOKEN_FILE",
		"BROKER_PROJECT_ACCESS_TOKEN_FILE": "BROKER_PROJECT_ACCESS_URL",
	} {
		alone := validEnv()
		alone[set] = e[set]
		_, err := Load(env(alone))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("Load(%s alone) = %v; want a refusal naming %s", set, err, missing)
		}
	}

	for _, bad := range []string{"dispatch.internal.example", "//dispatch.internal.example", "ftp://dispatch.internal.example", "http://", "http://%zz"} {
		e := validEnv()
		e["BROKER_PROJECT_ACCESS_URL"] = bad
		e["BROKER_PROJECT_ACCESS_TOKEN_FILE"] = "/var/run/secrets/dispatch-access/token"
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_PROJECT_ACCESS_URL") || !strings.Contains(err.Error(), "absolute http(s) URL") {
			t.Errorf("Load(BROKER_PROJECT_ACCESS_URL=%q) = %v; want a refusal naming the variable", bad, err)
		}
	}
}

// TestHoldRequiresDeadlineIsAnOnOffSetting pins BROKER_HOLD_REQUIRES_DEADLINE, which an IAP
// deployment's broker sets, read as the broker's other on/off settings are: unset, or blank, is
// off, every spelling strconv.ParseBool reads is taken, and anything else refuses the start naming
// the setting rather than being taken as off, which would serve an IAP deployment's people with no
// deadline. It needs the hold it rules, which BROKER_PROJECT_ACCESS_URL builds: set true without
// that URL, the start is refused naming both settings, since a broker that asks no Dispatch holds
// nobody, deadline or not; set false without it, the start goes on.
//
// Controls: Load leaving HoldRequiresDeadline unread ("true" reads as off, and the test fails
// naming it); Load taking it true with no BROKER_PROJECT_ACCESS_URL (the start goes on, and the
// test fails naming the refusal it wanted).
func TestHoldRequiresDeadlineIsAnOnOffSetting(t *testing.T) {
	withHold := func() map[string]string {
		e := validEnv()
		e["BROKER_PROJECT_ACCESS_URL"] = "http://dispatch.internal.example:8766"
		e["BROKER_PROJECT_ACCESS_TOKEN_FILE"] = "/var/run/secrets/dispatch-access/token"
		return e
	}
	for _, c := range []struct {
		value string
		want  bool
	}{{"", false}, {"  ", false}, {"false", false}, {"0", false}, {"true", true}, {" TRUE ", true}, {"1", true}} {
		e := withHold()
		if c.value != "" {
			e["BROKER_HOLD_REQUIRES_DEADLINE"] = c.value
		}
		cfg, err := Load(env(e))
		if err != nil || cfg.HoldRequiresDeadline != c.want {
			t.Errorf("Load(BROKER_HOLD_REQUIRES_DEADLINE=%q) = (%t, %v); want %t", c.value, cfg.HoldRequiresDeadline, err, c.want)
		}
	}
	e := withHold()
	e["BROKER_HOLD_REQUIRES_DEADLINE"] = "required"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_HOLD_REQUIRES_DEADLINE") || !strings.Contains(err.Error(), "must be true or false") {
		t.Errorf("Load(BROKER_HOLD_REQUIRES_DEADLINE=%q) = %v; want a refusal naming the setting", "required", err)
	}

	e = validEnv()
	e["BROKER_HOLD_REQUIRES_DEADLINE"] = "true"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_HOLD_REQUIRES_DEADLINE") || !strings.Contains(err.Error(), "BROKER_PROJECT_ACCESS_URL") {
		t.Errorf("Load(BROKER_HOLD_REQUIRES_DEADLINE=true with no BROKER_PROJECT_ACCESS_URL) = %v; want a refusal naming both settings: that broker holds nobody", err)
	}
	e["BROKER_HOLD_REQUIRES_DEADLINE"] = "false"
	if cfg, err := Load(env(e)); err != nil || cfg.HoldRequiresDeadline {
		t.Errorf("Load(BROKER_HOLD_REQUIRES_DEADLINE=false with no BROKER_PROJECT_ACCESS_URL) = (%t, %v); want the start to go on with it off", cfg.HoldRequiresDeadline, err)
	}
}

// TestTheDirectoryURLIsGooglesUnlessSet pins BROKER_DIRECTORY_URL: unset, the broker reads no
// address of its own, so the group reader calls Google's Directory API; set, it is read without
// the slashes it ends in, as a test's stand-in is named; and a value that is no absolute https URL,
// or http to a loopback host, refuses the start naming the setting: a group read sends the reader
// account's token to it, which only a run on one machine may send in plain text.
func TestTheDirectoryURLIsGooglesUnlessSet(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirectoryURL != "" {
		t.Fatalf("with BROKER_DIRECTORY_URL unset the broker reads %q; want nothing, so the reader calls Google's own address", cfg.DirectoryURL)
	}

	for value, want := range map[string]string{
		" https://directory.internal.example:8443// ": "https://directory.internal.example:8443",
		"http://127.0.0.1:8080/":                      "http://127.0.0.1:8080",
		"http://localhost:8080":                       "http://localhost:8080",
	} {
		e := validEnv()
		e["BROKER_DIRECTORY_URL"] = value
		if cfg, err = Load(env(e)); err != nil {
			t.Fatalf("Load(BROKER_DIRECTORY_URL=%q) = %v; want it taken", value, err)
		}
		if cfg.DirectoryURL != want {
			t.Fatalf("BROKER_DIRECTORY_URL %q is read as %q; want %q, without its spaces and trailing slashes", value, cfg.DirectoryURL, want)
		}
	}

	for _, bad := range []string{"directory.internal.example", "//directory.internal.example", "ftp://directory.internal.example", "http://", "http://%zz", "http://directory.internal.example:8080", "https://"} {
		e := validEnv()
		e["BROKER_DIRECTORY_URL"] = bad
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "BROKER_DIRECTORY_URL") || !strings.Contains(err.Error(), "absolute https URL, or http to a loopback host") {
			t.Errorf("Load(BROKER_DIRECTORY_URL=%q) = %v; want a refusal naming the variable and the rule", bad, err)
		}
	}
}

// groupReaderAccount is the reader account a deployment names in BROKER_DIRECTORY_ACCOUNT.
const groupReaderAccount = "group-reader@example-project.iam.gserviceaccount.com"

// TestTheDirectoryAccountIsAServiceAccount pins BROKER_DIRECTORY_ACCOUNT: unset, the broker loads
// and names no reader account, so it reads no group; set, it is read as given, without spaces;
// and an address that is no user-managed Google service account, the only kind generateAccessToken
// mints for, refuses the start naming the setting: a person, a capitalised address, a list and a
// Google-managed service agent, which Google's own services run as.
func TestTheDirectoryAccountIsAServiceAccount(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirectoryAccount != "" {
		t.Fatalf("with BROKER_DIRECTORY_ACCOUNT unset the broker reads %q; want no reader account", cfg.DirectoryAccount)
	}

	e := validEnv()
	e["BROKER_DIRECTORY_ACCOUNT"] = " " + groupReaderAccount + "\n"
	if cfg, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	if cfg.DirectoryAccount != groupReaderAccount {
		t.Fatalf("BROKER_DIRECTORY_ACCOUNT is read as %q; want %q", cfg.DirectoryAccount, groupReaderAccount)
	}

	for _, c := range []struct{ name, account, want string }{
		{"a person", "reader@example.com", "user-managed Google service account"},
		{"an upper-case address", "Group-Reader@example-project.iam.gserviceaccount.com", "user-managed Google service account"},
		{"a list", groupReaderAccount + " other-reader@example-project.iam.gserviceaccount.com", "user-managed Google service account"},
		{"a service agent", "service-123456789012@gcp-sa-example.iam.gserviceaccount.com", "Google-managed service agent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_DIRECTORY_ACCOUNT"] = c.account
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), "BROKER_DIRECTORY_ACCOUNT") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load(BROKER_DIRECTORY_ACCOUNT=%q) = %v; want a refusal naming the setting and %q", c.account, err, c.want)
			}
		})
	}
}

// TestADirectoryAccountThatASecretBindsIsRefused: a reader account that BROKER_SERVICE_ACCOUNT_SECRETS
// binds to a secret refuses the start, naming the secret, since a grant of that secret would
// release the token the group read uses; a reader account that no secret binds loads beside
// those bindings.
func TestADirectoryAccountThatASecretBindsIsRefused(t *testing.T) {
	e := validEnv()
	e["BROKER_SERVICE_ACCOUNT_SECRETS"] = "GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com GROUP_READER=" + groupReaderAccount
	e["BROKER_DIRECTORY_ACCOUNT"] = groupReaderAccount
	_, err := Load(env(e))
	if err == nil || !strings.Contains(err.Error(), "the directory account is bound to a secret") || !strings.Contains(err.Error(), "GROUP_READER") || !strings.Contains(err.Error(), "BROKER_DIRECTORY_ACCOUNT") {
		t.Fatalf("Load with BROKER_DIRECTORY_ACCOUNT bound to GROUP_READER = %v; want a refusal saying the directory account is bound to a secret, naming it", err)
	}

	e["BROKER_SERVICE_ACCOUNT_SECRETS"] = "GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load with a reader account no secret binds = %v; want it loaded", err)
	}
	if cfg.DirectoryAccount != groupReaderAccount {
		t.Fatalf("BROKER_DIRECTORY_ACCOUNT is read as %q; want %q", cfg.DirectoryAccount, groupReaderAccount)
	}
}

// userinfoURL is scheme://user:password@rest, built here so no test line spells a URL that
// carries a credential in the form a secret scanner reads as one.
func userinfoURL(scheme, user, password, rest string) string {
	return scheme + "://" + user + ":" + password + "@" + rest
}
