// cmd/cipherd/airlock_settings_test.go
// The airlock's settings against the REAL compiled broker. Only a secret behind a kind of the
// profiles file is carried by the proxy port, so the settings that port cannot run without are
// needed only when the profiles file declares a kind. config's own rows prove the check
// (Config.RequireAirlock); these prove that the boot runs it on the profiles file it read, before
// it binds anything, and that a broker whose profiles file declares no kind binds no proxy port
// and still serves its GitHub profiles.
package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// githubReadProfile is a GitHub profile of the App secret EXAMPLE_APP behind no kind: it reads
// acme-org's contents, and a grant of it is released as the installation token itself.
const githubReadProfile = `{"name": "EXAMPLE_APP_READ", "source": "EXAMPLE_APP",
	"github": {"accounts": ["acme-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}}`

// deelKind is a kind on the one host api.deel.com whose credential is a stored value. A profiles
// file that declares it puts every secret whose proxy tag names deel behind the proxy, so the boot
// needs the airlock's settings for it (config.AirlockSubjects names it "kind deel") whether or not
// any secret carries that tag yet.
const deelKind = `{"name": "deel", "hosts": ["api.deel.com"], "ways": ["swap"]}`

// deelKindFile is a profiles file that declares deelKind and holds no profile: the least file
// that turns the airlock on.
const deelKindFile = `{"kinds": [` + deelKind + `], "profiles": []}`

// TestMainServesAGitHubProfileWithoutTheAirlock: a deployment whose profiles file holds GitHub
// profiles and declares no kind, as one that serves GitHub Apps and runs no airlock writes it,
// starts with none of the airlock's settings, in a cluster and without
// BROKER_AIRLOCK_CLUSTER_RANGES, binds no proxy port, and releases a grant of its GitHub profile as
// the token GitHub minted.
func TestMainServesAGitHubProfileWithoutTheAirlock(t *testing.T) {
	t.Parallel()
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
	)
	stand, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(stand.Close)
	stand.AnswerWith(proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	value, err := json.Marshal(policytest.TypeValue(policy.TypeGitHubApp))
	if err != nil {
		t.Fatal(err)
	}
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	writeWhole(t, fakeSecretsFile, `{"secrets": [
		{"name": "`+testSecretsPrefix+`example-app", "kms_key_id": "`+testSecretsKMSKeyARN+`",
		 "tags": {"owner": "`+service+`", "tier": "agent", "`+policy.TagType+`": "`+policy.TypeGitHubApp+`", "`+policy.TagGitHubOwners+`": "acme-org"},
		 "value": `+string(value)+`}
	]}`)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	// No BROKER_AIRLOCK_* setting but the stand-in GitHub, and KUBERNETES_SERVICE_HOST set: a
	// broker that still required the airlock for any profiles file would refuse to start here.
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_SERVICE_LOGINS=" + service + "=" + loginAccount,
		"BROKER_SERVICES=" + service + "=" + podAccount,
		"BROKER_AIRLOCK_TEST_GITHUB_API_BASE=" + stand.URL(),
		"BROKER_PROFILES_FILE=" + profilesFile(t, `{"profiles": [`+githubReadProfile+`]}`),
		"KUBERNETES_SERVICE_HOST=" + clusterServiceHost,
	}})
	requireHealthy(t, broker.Addr)
	// The metrics port's line is logged after the proxy port's would be, so once it is in the log
	// a proxy port the boot bound would be too.
	broker.MetricsAddr(t)
	if strings.Contains(broker.Log(), airlockProxyLine) {
		t.Fatalf("a broker whose profiles file declares no kind bound a proxy port:\n%s", broker.Log())
	}

	api := podBroker{t: t, base: "http://" + broker.Addr, issuer: issuer, key: podKey, audience: podAudience}
	p := api.enroll(api.logIn(service, loginAccount), "app-pod", podAccount, "")
	granted := api.ask(p, "EXAMPLE_APP_READ")
	if granted.GrantID == nil {
		t.Fatalf("the pod's request for EXAMPLE_APP_READ = %+v, want a grant at once", granted)
	}
	if token := api.values(p, *granted.GrantID)["EXAMPLE_APP_READ"]; !strings.HasPrefix(token, "ghs_standin_acme-org") {
		t.Fatalf("the release of EXAMPLE_APP_READ = %q; want the installation token the stand-in GitHub minted for acme-org", token)
	}
}

// TestMainRefusesAKindWithoutTheAirlocksSettings drives the real binary with a profiles file that
// declares a kind and each of the three settings the proxy port cannot run without left out, and
// asserts it exits non-zero, without ever binding, naming the setting and the kind that needs it,
// and not the GitHub profile beside it, which is behind no kind and needs none. The database it is
// given is unreachable, so a broker that checked the settings after opening the store would fail
// on the host instead.
func TestMainRefusesAKindWithoutTheAirlocksSettings(t *testing.T) {
	binary := buildBroker(t)
	for _, missing := range airlockProxySettingNames {
		t.Run(missing, func(t *testing.T) {
			var env []string
			for _, setting := range airlockEnv(t) {
				if !strings.HasPrefix(setting, missing+"=") {
					env = append(env, setting)
				}
			}
			out := runRefusedBroker(t, binary, append(env,
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_PROFILES_FILE="+profilesFile(t, `{"kinds": [`+deelKind+`], "profiles": [`+githubReadProfile+`]}`),
			)...)
			for _, want := range []string{"BROKER_PROFILES_FILE declares kind deel", "missing " + missing} {
				if !strings.Contains(out, want) {
					t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
				}
			}
			if strings.Contains(out, "EXAMPLE_APP_READ") {
				t.Fatalf("the refusal names EXAMPLE_APP_READ, a profile behind no kind: %s", out)
			}
		})
	}
}

// airlockProxySettingNames are the settings airlockEnv gives that the proxy port cannot run
// without, each one a row of TestMainRefusesAKindWithoutTheAirlocksSettings.
var airlockProxySettingNames = []string{"BROKER_AIRLOCK_PROXY_URL", "BROKER_AIRLOCK_CA_CERT_FILE", "BROKER_AIRLOCK_CA_KEY_FILE"}
