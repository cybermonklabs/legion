package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestSecretShowPrintsTheGitHubCap: a github-app secret's github-repositories and
// github-permissions tags, which no flag of this CLI writes, are printed by show and carried by
// list --json, so whoever reads the secret sees the most its tokens reach and hold; a secret that
// caps neither prints a dash for each, as for every other tag it does not carry.
func TestSecretShowPrintsTheGitHubCap(t *testing.T) {
	capped := policytest.TypeSecret("CAPPED_APP", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	capped.Tags[policy.TagGitHubOwners] = "acme-org"
	capped.Tags[policy.TagGitHubRepositories] = "widgets"
	capped.Tags[policy.TagGitHubPermissions] = "checks:write"
	plain := policytest.TypeSecret("PLAIN_APP", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	local := secrets.NewLocal(capped, plain)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)

	stdout, stderr, code := runSecret("show", "CAPPED_APP")
	if code != 0 || !strings.Contains(stdout, "github_repositories: widgets\n") || !strings.Contains(stdout, "github_permissions: checks:write\n") {
		t.Fatalf("show CAPPED_APP: exit %d stdout %q stderr %q; want its github-repositories and github-permissions", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("show", "PLAIN_APP")
	if code != 0 || !strings.Contains(stdout, "github_repositories: -\n") || !strings.Contains(stdout, "github_permissions: -\n") {
		t.Fatalf("show PLAIN_APP: exit %d stdout %q stderr %q; want a dash for each cap tag", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list", "--json")
	var listed []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &listed) != nil {
		t.Fatalf("list --json: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, row := range listed {
		if row["name"] == "CAPPED_APP" && (row["github_repositories"] != "widgets" || row["github_permissions"] != "checks:write") {
			t.Fatalf("list --json CAPPED_APP = %v; want its cap", row)
		}
	}
}
