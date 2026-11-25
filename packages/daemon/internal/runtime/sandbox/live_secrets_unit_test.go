// Ordinary, always-visible unit tests for the Stage 4a agent-secrets checks' pure logic
// that live_secrets_pure_test.go defines with no e2e build tag: shellJoin's shell
// quoting, agentSecretsBlockReason's blocking-reason string (what live_secrets_test.go's
// secretsBlocked delegates to) and deploymentBrokerAudience. The e2e-tagged checks these back
// (checkSecretsTwoPodsEnrolled and friends) need a real cluster and a real broker and stay
// untestable here, exactly as the plan anticipates (SKIPPED-BLOCKED without them).
package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// configMapList is `kubectl get configmaps -o json` for ConfigMaps of these names and data.
func configMapList(t *testing.T, data map[string]map[string]string) []byte {
	t.Helper()
	type item struct {
		Metadata map[string]string `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	list := struct {
		Items []item `json:"items"`
	}{Items: []item{}}
	for name, values := range data {
		list.Items = append(list.Items, item{Metadata: map[string]string{"name": name}, Data: values})
	}
	out, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Stage 4a enrolls its pods for the broker audience the namespace's daemons use: the one their
// legion.yaml names, the daemon's default where it names none, and no audience at all where two
// daemons use different ones. Each row that reads one audience has a neighbour that reads another
// from the same ConfigMaps changed in one line.
func TestStage4aEnrollsForTheDeploymentsBrokerAudience(t *testing.T) {
	const enrolled = "runtime:\n  kubernetes:\n    agent_secrets:\n      url: https://secrets.internal.example\n"
	named := enrolled + "      audience: agent-secrets\n"
	unenrolled := "runtime:\n  kubernetes:\n    namespace: legion\n"
	ca := map[string]string{"ca.crt": "not a daemon's configuration"}
	for name, tc := range map[string]struct {
		configMaps map[string]map[string]string
		want       string
		refused    []string
	}{
		"a daemon that names agent-secrets": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": named}, "kube-root-ca.crt": ca},
			want:       "agent-secrets",
		},
		"the same daemon naming no audience takes the default": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": enrolled}, "kube-root-ca.crt": ca},
			want:       config.DefaultAgentSecretsAudience,
		},
		"no daemon enrolls its pods": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": unenrolled}, "kube-root-ca.crt": ca},
			want:       config.DefaultAgentSecretsAudience,
		},
		"two daemons that agree": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": named}, "legion-acme-config": {"legion.yaml": named}},
			want:       "agent-secrets",
		},
		"two daemons that disagree": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": named}, "legion-acme-config": {"legion.yaml": enrolled}},
			refused: []string{"different broker audiences", "agent-secrets (legion-config)",
				config.DefaultAgentSecretsAudience + " (legion-acme-config)"},
		},
		"a legion.yaml that does not parse": {
			configMaps: map[string]map[string]string{"legion-config": {"legion.yaml": "runtime: [\n"}},
			refused:    []string{"ConfigMap legion-config: legion.yaml"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := deploymentBrokerAudience(configMapList(t, tc.configMaps))
			if tc.refused == nil {
				if err != nil || got != tc.want {
					t.Fatalf("deploymentBrokerAudience = %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("deploymentBrokerAudience = %q; want a refusal naming %q", got, tc.refused)
			}
			for _, part := range tc.refused {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("refusal %q does not name %q", err, part)
				}
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no args":              {nil, ""},
		"plain args":           {[]string{"self", "--json"}, "'self' '--json'"},
		"single quote in arg":  {[]string{"it's"}, `'it'\''s'`},
		"arg with a space":     {[]string{"s4a cross-issue probe"}, "'s4a cross-issue probe'"},
		"empty string arg":     {[]string{""}, "''"},
		"shell metacharacters": {[]string{"$(rm -rf /)", "a;b"}, `'$(rm -rf /)' 'a;b'`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shellJoin(tc.args); got != tc.want {
				t.Fatalf("shellJoin(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestAgentSecretsBlockReason(t *testing.T) {
	const url, sha, bin, token = "https://secrets.internal.example", "deadbeef", "/bin/agent-secrets", "/tmp/agent-secrets-token"
	for name, tc := range map[string]struct {
		url, sha, bin, token string
		wantBlocked          bool
		wantNames            []string
	}{
		"everything set": {url, sha, bin, token, false, nil},
		"everything unset": {
			"", "", "", "", true,
			[]string{"LEGION_E2E_AGENT_SECRETS_URL", "LEGION_E2E_AGENT_SECRETS_AUTO_SHA256", "LEGION_E2E_AGENT_SECRETS_BIN", "LEGION_E2E_AGENT_SECRETS_ACCOUNT"},
		},
		"only URL unset":     {"", sha, bin, token, true, []string{"LEGION_E2E_AGENT_SECRETS_URL"}},
		"only sha unset":     {url, "", bin, token, true, []string{"LEGION_E2E_AGENT_SECRETS_AUTO_SHA256"}},
		"only bin unset":     {url, sha, "", token, true, []string{"LEGION_E2E_AGENT_SECRETS_BIN"}},
		"only account unset": {url, sha, bin, "", true, []string{"LEGION_E2E_AGENT_SECRETS_ACCOUNT"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := agentSecretsBlockReason(tc.url, tc.sha, tc.bin, tc.token)
			if (got != "") != tc.wantBlocked {
				t.Fatalf("agentSecretsBlockReason(%q, %q, %q, %q) = %q, want blocked=%t", tc.url, tc.sha, tc.bin, tc.token, got, tc.wantBlocked)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(got, name) {
					t.Fatalf("reason %q does not name the unset %s", got, name)
				}
			}
			if !tc.wantBlocked && got != "" {
				t.Fatalf("reason = %q, want empty (nothing missing)", got)
			}
		})
	}
}
