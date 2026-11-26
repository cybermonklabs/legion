// packages/envoy/internal/cipher/config/role_settings_test.go
package config

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// settingRole marks one setting Load reads: as giving a Kubernetes account a role, with the names
// of the rows of mappedAccountRefusalCases that check its refusal in mappedAccountRefusal, or as
// giving none, with why.
type settingRole struct {
	refusals []string
	none     string
}

// givesARole marks a setting that gives a Kubernetes account a role, beside the rows of
// mappedAccountRefusalCases that check its refusal.
func givesARole(rows ...string) settingRole { return settingRole{refusals: rows} }

// givesNone marks a setting that gives no Kubernetes account a role, with why.
func givesNone(why string) settingRole { return settingRole{none: why} }

// settingRoles marks every BROKER_ setting Load reads, in the order Load reads them with the
// person sign-in on. TestEveryBrokerSettingLoadReadsIsMarked holds it to what Load reads.
var settingRoles = map[string]settingRole{
	// removedVars, each read so that a deployment still setting it is refused.
	"BROKER_DISPATCH_URL":         givesNone("a removed setting, refused when set"),
	"BROKER_DISPATCH_TOKEN":       givesNone("a removed setting, refused when set"),
	"BROKER_DISPATCH_TOKEN_FILE":  givesNone("a removed setting, refused when set"),
	"BROKER_DISPATCH_PROJECT":     givesNone("a removed setting, refused when set"),
	"BROKER_ASK_POLL_SECONDS":     givesNone("a removed setting, refused when set"),
	"BROKER_UI_ORIGIN":            givesNone("a removed setting, refused when set"),
	"BROKER_UI_TOKEN":             givesNone("a removed setting, refused when set"),
	"BROKER_UI_TOKEN_FILE":        givesNone("a removed setting, refused when set"),
	"BROKER_DATABASE_PASSWORD":    givesNone("a removed setting, refused when set"),
	"BROKER_RULES_FILE":           givesNone("a removed setting, refused when set"),
	"BROKER_RULES_S3_URI":         givesNone("a removed setting, refused when set"),
	"BROKER_RULES_RELOAD_SECONDS": givesNone("a removed setting, refused when set"),
	"BROKER_MAX_GRANT_SECONDS":    givesNone("a removed setting, refused when set"),
	"BROKER_MINT_TARGETS":         givesNone("a removed setting, refused when set"),
	"BROKER_SIGNIN_CLIENT_SECRET": givesNone("a removed setting, refused when set"),
	"BROKER_ENVOY_TOKEN":          givesNone("a removed setting, refused when set"),
	"BROKER_ENVOY_TOKEN_FILE":     givesNone("a removed setting, refused when set"),
	"BROKER_AIRLOCK_RULES_FILE":   givesNone("a removed setting, refused when set"),

	"BROKER_DATABASE_URL": givesNone("the database's URL"),
	"BROKER_SIGNIN":       givesNone("the person sign-in's switch"),

	"BROKER_LISTEN_ADDR":                   givesNone("a listen address"),
	"BROKER_METRICS_LISTEN_ADDR":           givesNone("a listen address"),
	"BROKER_PUBLIC_URL":                    givesNone("the broker's own URL"),
	"BROKER_MACHINE_LOGIN_URL":             givesNone("a page's URL"),
	"BROKER_EDGE_LISTEN_ADDR":              givesNone("a listen address"),
	"BROKER_EDGE_URL":                      givesNone("the broker's own URL at its edge"),
	"BROKER_EDGE_MACHINE_LOGIN_URL":        givesNone("a page's URL"),
	"BROKER_SIGNIN_WORKSPACE_DOMAINS":      givesNone("the Workspace's domains"),
	"BROKER_SECRETS_STORE":                 givesNone("a secret store"),
	"BROKER_SECRETS_PREFIX":                givesNone("a secret-name prefix"),
	"BROKER_PROFILES_FILE":                 givesNone("a file path"),
	"BROKER_AIRLOCK_LISTEN_ADDR":           givesNone("a listen address"),
	"BROKER_AIRLOCK_PROXY_URL":             givesNone("the airlock proxy's URL"),
	"BROKER_AIRLOCK_CA_CERT_FILE":          givesNone("a file path"),
	"BROKER_AIRLOCK_CA_KEY_FILE":           givesNone("a file path"),
	"BROKER_AIRLOCK_TEST_GITHUB_API_BASE":  givesNone("a test-only URL"),
	"BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL": givesNone("a test-only URL"),
	"BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE": givesNone("a test-only file path"),
	"BROKER_JUDGE_MODEL_URL":               givesNone("the judge model's URL"),
	"BROKER_JUDGE_MODEL":                   givesNone("a model id"),
	"BROKER_JUDGE_MODEL_PROTOCOL":          givesNone("a wire protocol"),
	"BROKER_JUDGE_MODEL_AUDIENCE":          givesNone("an audience"),
	"BROKER_JUDGE_MODEL_ACCOUNT":           givesNone("a Google account"),
	"BROKER_JUDGE_DISPATCH_URL":            givesNone("a Dispatch's URL"),
	"BROKER_JUDGE_DISPATCH_TOKEN_FILE":     givesNone("a file path"),
	"BROKER_PROJECT_ACCESS_URL":            givesNone("a Dispatch's URL"),
	"BROKER_PROJECT_ACCESS_TOKEN_FILE":     givesNone("a file path"),
	"BROKER_AZURE_TOKEN_FILE":              givesNone("a file path"),
	"BROKER_AZURE_LOGIN_SECRET":            givesNone("a secret's name"),
	"BROKER_K8S_OIDC_ISSUER":               givesNone("an issuer"),
	"BROKER_K8S_OIDC_AUDIENCE":             givesNone("an audience"),
	"BROKER_GOOGLE_OIDC_ISSUER":            givesNone("an issuer"),
	"BROKER_GOOGLE_OIDC_AUDIENCE":          givesNone("an audience"),
	"BROKER_ENVOY_URL":                     givesNone("an Envoy listener's URL"),
	"BROKER_TRUSTED_PROXY_HEADER":          givesNone("a header name"),

	"BROKER_AIRLOCK_TEST_DIAL_ALLOW":      givesNone("test-only addresses"),
	"BROKER_AIRLOCK_TEST_RESOLVE":         givesNone("test-only addresses"),
	"BROKER_AIRLOCK_TEST_KEEP_VALUES":     givesNone("a test-only switch"),
	"BROKER_AIRLOCK_TEST_NO_KIND":         givesNone("a test-only switch"),
	"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY": givesNone("a test-only host"),
	"BROKER_JUDGE_TEST_SKIP_CHECKS":       givesNone("a test-only switch"),

	"BROKER_SECRETS_KMS_KEY_ARN":    givesNone("a KMS key"),
	"BROKER_SECRETS_KMS_KEY_NAME":   givesNone("a KMS key"),
	"BROKER_AIRLOCK_CLUSTER_RANGES": givesNone("address ranges"),
	"BROKER_PROXY_PRIVATE_UPSTREAM": givesNone("a host and a URL"),

	"BROKER_SERVICES": givesARole(
		"a BROKER_SERVICES entry names its login service",
		"a BROKER_SERVICES entry binds it",
	),
	"BROKER_SERVICE_LOGINS": givesARole("BROKER_SERVICE_LOGINS does not name it"),
	"BROKER_SESSION_ACCOUNT_PAIRS": givesARole(
		"a BROKER_SESSION_ACCOUNT_PAIRS pair names it as the first account",
		"a BROKER_SESSION_ACCOUNT_PAIRS pair names it as the second account",
	),
	"BROKER_SERVICE_ACCOUNT_SECRETS":  givesNone("a Google account for each secret"),
	"BROKER_DIRECTORY_URL":            givesNone("the Directory API's URL"),
	"BROKER_DIRECTORY_ACCOUNT":        givesNone("a Google account the group read mints for"),
	"BROKER_SIGNIN_DEVICE_AUDIENCE":   givesNone("an audience"),
	"BROKER_VM_SERVICES":              givesARole("a BROKER_VM_SERVICES entry names its login service"),
	"BROKER_VM_PROJECTS":              givesNone("a Google account's project"),
	"BROKER_VM_INSTANCES":             givesNone("a Google account's instance names"),
	"BROKER_VM_EXCHANGE_ACCOUNTS":     givesNone("a Google account's person and project"),
	"BROKER_VM_EXCHANGE_JOB_ACCOUNTS": givesNone("a Google account's projects"),

	"BROKER_API_ACCOUNTS":             givesARole("a BROKER_API_ACCOUNTS entry names it"),
	"BROKER_EXCHANGE_GITHUB_ISSUER":   givesNone("an issuer"),
	"BROKER_EXCHANGE_GITHUB_AUDIENCE": givesNone("an audience"),
	"BROKER_EXCHANGE_K8S_AUDIENCE":    givesNone("an audience"),
	"BROKER_EXCHANGE_ACCOUNTS":        givesARole("a BROKER_EXCHANGE_ACCOUNTS entry maps it to another person"),
	"BROKER_EXCHANGE_READ_ACCOUNTS":   givesARole("a BROKER_EXCHANGE_READ_ACCOUNTS entry names it"),
	"BROKER_EXCHANGE_JOB_ACCOUNTS":    givesARole("a BROKER_EXCHANGE_JOB_ACCOUNTS entry names it"),
	"BROKER_EXCHANGE_WORKFLOWS":       givesNone("a GitHub workflow"),
	"BROKER_EXCHANGE_JOB_WORKFLOWS":   givesNone("a GitHub workflow"),
	"BROKER_PRIVATE_PROJECTS":         givesNone("project keys"),
	"BROKER_ENVOY_ACCOUNT_TOKEN_FILE": givesNone("a file path"),

	// loadNATSCallout's.
	"BROKER_NATS_SEED_FILE":        givesNone("a file path"),
	"BROKER_NATS_ISSUER_SEED_FILE": givesNone("a file path"),
	"BROKER_NATS_XKEY_SEED_FILE":   givesNone("a file path"),
	"BROKER_NATS_SERVICES":         givesARole("a BROKER_NATS_SERVICES entry names it"),
	"BROKER_NATS_K8S_AUDIENCE":     givesNone("an audience"),
	"BROKER_NATS_GOOGLE_AUDIENCE":  givesNone("an audience"),
	"BROKER_NATS_URLS":             givesNone("the NATS hub's URLs"),
	// loadNATSHold's: the operator BROKER_NATS_HOLD_READ needs is a BROKER_NATS_SERVICES entry, which
	// that setting's row marks.
	"BROKER_NATS_HOLD_READ":         givesNone("a switch"),
	"BROKER_NATS_HOLD_SIGNING_KEYS": givesNone("public keys"),
	// Load's int.
	"BROKER_NATS_HOLD_POOL_SIZE": givesNone("a number of connections"),
	"BROKER_NATS_ACCOUNT":        givesNone("the hub account admitted clients are placed in"),

	"BROKER_POD_REGISTRARS":            givesARole("a BROKER_POD_REGISTRARS entry names it as its registrar"),
	"BROKER_POD_REGISTRATION_AUDIENCE": givesNone("an audience"),
	"BROKER_POD_OPERATORS":             givesNone("the mapping itself"),

	// Load's ints.
	"BROKER_LEASE_SECONDS":               givesNone("a number of seconds"),
	"BROKER_SESSION_CARRY_SECONDS":       givesNone("a number of seconds"),
	"BROKER_PROOF_SKEW_SECONDS":          givesNone("a number of seconds"),
	"BROKER_ACCESS_TOKEN_SECONDS":        givesNone("a number of seconds"),
	"BROKER_LAUNCHER_CREDENTIAL_SECONDS": givesNone("a number of seconds"),
	"BROKER_SWEEP_SECONDS":               givesNone("a number of seconds"),
	"BROKER_DRAIN_SECONDS":               givesNone("a number of seconds"),
	"BROKER_AIRLOCK_IDLE_SECONDS":        givesNone("a number of seconds"),
	"BROKER_SIGNIN_SESSION_SECONDS":      givesNone("a number of seconds"),
	"BROKER_GITHUB_APP_FIELD":            givesNone("a switch, off or unset"),
	"BROKER_GITHUB_APP_FIELD_REQUESTERS": givesNone("services and people a github_app field is answered for"),
	"BROKER_GITHUB_APP_FIELD_SECRETS":    givesNone("the stored secrets a github_app field hands over"),

	// parseSignIn's.
	"BROKER_SIGNIN_ISSUER":             givesNone("an issuer"),
	"BROKER_SIGNIN_CLIENT_ID":          givesNone("an OAuth client id"),
	"BROKER_SIGNIN_CLIENT_SECRET_FILE": givesNone("a file path"),
	"BROKER_SIGNIN_DOMAIN":             givesNone("a Workspace domain"),
}

// TestEveryBrokerSettingLoadReadsIsMarked holds the class of settings mappedAccountRefusal checks.
// It loads configurations through a getenv that records every name Load reads
// (settingsLoadReads), and each BROKER_ name recorded must be in settingRoles, marked either as
// giving a Kubernetes account a role, beside the rows of mappedAccountRefusalCases that check its
// refusal, or as giving none, with why. So a setting added later fails this test, which names it,
// until its author marks it. The test fails too on an entry no configuration read, so the table
// keeps no stale name, on an entry marked both ways or neither, and on a row that
// mappedAccountRefusalCases does not hold.
//
// The configurations are the two settingsLoadReads loads, one for each value of the sign-in switch,
// each setting every condition Load puts in front of a read, and together they make Load read every
// name settingRoles holds. The test sees only names read through Load's getenv on those paths. A
// setting read under a condition the two do not set, or read other than through getenv, is outside
// it: cmd/cipherd's BROKER_FAKE_SECRETS_FILE, which main reads itself, and the PG* variables
// pgconn.ParseConfig reads in databaseIAM are two today. For such a setting the class is a
// convention rather than a rule, and a branch added later that reads a setting needs its condition
// set in both configurations.
func TestEveryBrokerSettingLoadReadsIsMarked(t *testing.T) {
	cases := mappedAccountRefusalCases(t)
	read := settingsLoadReads(t)
	for _, name := range slices.Sorted(maps.Keys(read)) {
		// A name outside the BROKER_ prefix is no setting of the broker's, so the table leaves it
		// out: KUBERNETES_SERVICE_HOST, the one such name Load reads, is the address of the
		// cluster's kubernetes Service, which Kubernetes sets in every container
		// (requireClusterRanges).
		if !strings.HasPrefix(name, "BROKER_") {
			continue
		}
		if _, marked := settingRoles[name]; !marked {
			t.Errorf("Load reads %s, and settingRoles does not mark it: mark it as giving a Kubernetes account a role, beside its refusal in mappedAccountRefusal and its row in mappedAccountRefusalCases, or as giving none, with why", name)
		}
	}
	rows := make(map[string]bool, len(cases))
	for _, tc := range cases {
		rows[tc.name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(settingRoles)) {
		role := settingRoles[name]
		if !read[name] {
			t.Errorf("settingRoles marks %s, and no configuration made Load read it: take the entry out, or load a configuration that reads it", name)
		}
		if (len(role.refusals) > 0) == (role.none != "") {
			t.Errorf("settingRoles marks %s with the rows %q and the reason %q; mark it one way, with the rows that check its refusal or with why it gives no account a role", name, role.refusals, role.none)
		}
		for _, row := range role.refusals {
			if !rows[row] {
				t.Errorf("settingRoles marks %s as giving an account a role beside the row %q, which mappedAccountRefusalCases does not hold", name, row)
			}
		}
	}
}

// settingsLoadReads loads two configurations through a getenv that wraps env and records each
// name Load asks for, and answers the names recorded: one for each value of the person sign-in's
// switch, BROKER_SIGNIN unset (parseSignIn reads the sign-in's settings) and off
// (parseSignInSwitch reads each setting it refuses beside it). Each sets every condition Load
// puts in front of a read: BROKER_ENVOY_URL, which reads BROKER_ENVOY_ACCOUNT_TOKEN_FILE. Each
// must load, since a refused configuration stops reading.
func settingsLoadReads(t *testing.T) map[string]bool {
	t.Helper()
	withEveryCondition := func(e map[string]string) map[string]string {
		e["BROKER_ENVOY_URL"] = "https://listener.example"
		e["BROKER_ENVOY_ACCOUNT_TOKEN_FILE"] = "/var/run/secrets/tokens/envoy"
		return e
	}
	configurations := []struct {
		branch string
		env    map[string]string
	}{
		{"BROKER_SIGNIN unset", withEveryCondition(validEnv())},
		{"BROKER_SIGNIN=off", withEveryCondition(signInOffEnv())},
	}
	read := map[string]bool{}
	for _, c := range configurations {
		getenv := env(c.env)
		recording := func(name string) string {
			read[name] = true
			return getenv(name)
		}
		if _, err := Load(recording); err != nil {
			t.Fatalf("Load of %s: %v; want it to load, since a refused configuration stops reading", c.branch, err)
		}
	}
	return read
}
