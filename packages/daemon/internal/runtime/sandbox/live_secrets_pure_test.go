// The Stage 4a agent-secrets checks' pure logic: whether this run's broker inputs are
// set at all, the audience its pods enroll for, and the shell-quoting `agent-secrets <args>` runs
// through kubectl exec with. None of it touches a cluster, so — unlike live_test.go and
// live_secrets_test.go, both `//go:build e2e` — this file carries no build tag: ordinary
// `go test ./...` compiles and exercises it directly (live_secrets_unit_test.go).
// live_secrets_test.go's secretsBlocked is a thin e2e-tagged wrapper over agentSecretsBlockReason
// below, reading the fields off *liveRig it needs.
package sandbox

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// agentSecretsAudienceKey is the key whose default the run enrolls its pods for.
const agentSecretsAudienceKey = "runtime.kubernetes.agent_secrets.audience"

// agentSecretsDefaultAudience is the audience a daemon gives a runtime.kubernetes.agent_secrets
// block that names none, read from the key table `legion config reference` prints, where the
// script reads it too to mint the run's login token: the run enrolls its pods as a daemon with that
// block does, and a broker that verifies another audience refuses the run's login.
func agentSecretsDefaultAudience() (string, error) {
	for _, entry := range config.ConfigReference() {
		for _, nested := range entry.Nested {
			if nested.Path+"."+nested.Name != agentSecretsAudienceKey {
				continue
			}
			audience := strings.TrimSuffix(strings.TrimPrefix(nested.Default, "`"), "`")
			if audience == "" || strings.ContainsAny(audience, " \t\n`") {
				return "", fmt.Errorf("the key table gives %s the default %q, not one audience", agentSecretsAudienceKey, nested.Default)
			}
			return audience, nil
		}
	}
	return "", fmt.Errorf("the key table has no row for %s", agentSecretsAudienceKey)
}

// agentSecretsBlockReason is secretsBlocked's logic: the four broker inputs every secrets-* check
// needs (the production broker's URL, the automatic rule's dummy value's hash, the checkout's
// agent-secrets binary, and the token file the script mints from
// LEGION_E2E_AGENT_SECRETS_ACCOUNT, which the run logs in with as the daemon does), named for
// whichever are unset. Empty means the run is not blocked.
func agentSecretsBlockReason(url, autoSHA, bin, tokenFile string) string {
	var missing []string
	for name, value := range map[string]string{
		"LEGION_E2E_AGENT_SECRETS_URL":         url,
		"LEGION_E2E_AGENT_SECRETS_AUTO_SHA256": autoSHA, "LEGION_E2E_AGENT_SECRETS_BIN": bin,
		"LEGION_E2E_AGENT_SECRETS_ACCOUNT": tokenFile,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	slices.Sort(missing)
	return "no broker for this run: " + strings.Join(missing, ", ") + " unset (the production broker with Plan D's rules and a ServiceAccount its BROKER_SERVICE_LOGINS names supply them)"
}

// shellJoin quotes args for a POSIX shell -c string, the way (*liveRig).agentSecrets builds the
// command kubectl exec runs in a pod's container.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
