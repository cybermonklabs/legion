package config

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// githubKindHosts is the github_hosts line every test block carries: the hosts the broker's GitHub
// kind binds.
const githubKindHosts = "      github_hosts: [api.github.com, github.com, uploads.github.com]\n"

// githubProxyBlock is an agent_secrets block whose github member is lines, each indented under it,
// beside githubKindHosts.
func githubProxyBlock(lines string) string {
	return githubProxyBlockWith(lines, githubKindHosts)
}

// githubProxyBlockWith is githubProxyBlock with hosts, the github_hosts line or "" for none, in
// place of githubKindHosts.
func githubProxyBlockWith(lines, hosts string) string {
	return "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile + "      github:\n" + lines + hosts
}

// runtime.kubernetes.agent_secrets.github maps a role to the secret names its GitHub access goes
// through the broker's proxy under, the controller among the roles; a block that names none leaves
// the map nil, which is every role's App token minted by the daemon as before.
func TestTheGitHubProxyMapSettlesByRole(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+githubProxyBlock(
		"        implementer: [LEGION_IMPLEMENT_APP]\n        reviewer: [LEGION_REVIEW_APP, LEGION_REVIEW_READ]\n        controller: [LEGION_REVIEW_APP]\n")), noEnv)
	if err != nil {
		t.Fatalf("load = %v", err)
	}
	got := cfg.Runtime.Kubernetes.AgentSecrets.GitHub
	want := map[claim.Role][]string{
		claim.RoleImplementer: {"LEGION_IMPLEMENT_APP"},
		claim.RoleReviewer:    {"LEGION_REVIEW_APP", "LEGION_REVIEW_READ"},
		claim.RoleController:  {"LEGION_REVIEW_APP"},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("agent_secrets.github settled as %v, want %v", got, want)
	}

	cfg, err = LoadForValidation(writeConfigFile(t, kubernetesFile+"    agent_secrets:\n      url: https://secrets.internal.example\n"+agentSecretsTokenFile), noEnv)
	if err != nil || cfg.Runtime.Kubernetes.AgentSecrets.GitHub != nil {
		t.Fatalf("a block with no github member settled as %v, %v; want nil", cfg.Runtime.Kubernetes.AgentSecrets.GitHub, err)
	}
}

// Each refusal of the github member names the key and the rule it breaks: a value that is no
// mapping, a key that is no Legion role or names one twice, a list that is empty or holds no
// strings, a name the broker could hold no secret under, and a name given twice.
func TestTheGitHubProxyMapRefusesWhatNoRoleCouldAskFor(t *testing.T) {
	const key = "runtime.kubernetes.agent_secrets.github"
	for _, tc := range []struct{ name, lines, want string }{
		{"not a mapping", "        - LEGION_IMPLEMENT_APP\n", key + " must be a mapping of a role to the secret names its GitHub access goes through the broker's proxy under"},
		{"an unknown role", "        coder: [LEGION_IMPLEMENT_APP]\n", key + ` names "coder", which is not a Legion role`},
		{"a role twice", "        tester: [A_APP]\n        tester: [B_APP]\n", key + " names tester twice"},
		{"an empty list", "        tester: []\n", key + ".tester names no secret: give the role at least one, or leave it out"},
		{"no strings", "        tester: [1]\n", key + ".tester must be an array of non-empty strings"},
		{"a lowercase name", "        tester: [legion_app]\n", key + `.tester entry "legion_app" is no secret name: uppercase letters, digits and single underscores, starting with a letter`},
		{"a dotted name", "        tester: [LEGION.APP]\n", key + `.tester entry "LEGION.APP" is no secret name`},
		{"a name twice", "        tester: [A_APP, A_APP]\n", key + ".tester names A_APP twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+githubProxyBlock(tc.lines)), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want a refusal holding %q", err, tc.want)
			}
		})
	}
}

// runtime.kubernetes.agent_secrets.github_hosts are the hosts the broker's GitHub kind binds, which
// each forwarder is told: before its first grant a call to one of them waits for that grant, and
// every other call goes out at once. They settle as written, lowercased.
func TestTheGitHubKindsHostsSettleAsWritten(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+githubProxyBlockWith(
		"        tester: [LEGION_REVIEW_APP]\n", "      github_hosts: [API.github.com, github.com, '*.githubusercontent.com']\n")), noEnv)
	if err != nil {
		t.Fatalf("load = %v", err)
	}
	if got, want := cfg.Runtime.Kubernetes.AgentSecrets.GitHubHosts, []string{"api.github.com", "github.com", "*.githubusercontent.com"}; !slices.Equal(got, want) {
		t.Fatalf("agent_secrets.github_hosts settled as %v, want %v", got, want)
	}
}

// A github map with no github_hosts is refused: a forwarder that knew no GitHub host would send
// every call before its first grant straight out. A host list that is empty, holds a port, a URL,
// a name that is no DNS name or one name twice is refused naming the entry.
func TestTheGitHubKindsHostsRefuseWhatNoProxyBinds(t *testing.T) {
	const key = "runtime.kubernetes.agent_secrets.github_hosts"
	for _, tc := range []struct{ name, hosts, want string }{
		{"none", "", key + " is required when runtime.kubernetes.agent_secrets.github names a role"},
		{"an empty list", "      github_hosts: []\n", key + " names no host"},
		{"a port", "      github_hosts: [api.github.com:443]\n", key + ` entry "api.github.com:443" is no host name`},
		{"a URL", "      github_hosts: ['https://api.github.com']\n", key + ` entry "https://api.github.com" is no host name`},
		{"a bare wildcard", "      github_hosts: ['*']\n", key + ` entry "*" is no host name`},
		{"a host twice", "      github_hosts: [github.com, GitHub.com]\n", key + " names github.com twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+githubProxyBlockWith("        tester: [LEGION_REVIEW_APP]\n", tc.hosts)), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want a refusal holding %q", err, tc.want)
			}
		})
	}
}
