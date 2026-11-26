package requests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// mintLines are the attributes of each MintMessage line capture holds, each checked to be at INFO.
func mintLines(t *testing.T, capture *logtest.Recorder) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range loggedLines(t, capture, MintMessage) {
		if line.level != slog.LevelInfo {
			t.Errorf("%q was logged at %s, want INFO", MintMessage, line.level)
		}
		lines = append(lines, line.attrs)
	}
	return lines
}

// sha256Hex is the hex SHA-256 of token, as a MintMessage line's sha256 field writes it.
func sha256Hex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TestEachMintedTokenWritesOneMintLineWithItsHashAndNeverTheToken: a release writes one
// MintMessage line for each token a minter made for it, at INFO, so a scan that finds a token on a
// pod can tell, by its hash, which grant it was minted for. A github-app release behind the proxy
// writes one line for each installation's token, naming its slot and account and the App it was
// minted as; a github-app profile of one account that releases a bare token writes one line of the
// value slot, naming that account and the repositories of it the mint asked for; an
// anthropic-federation release writes one line naming the service account it was minted as. Each
// line names the enrollment, the grant, the secret, the way and the type, and its sha256 is the
// hex SHA-256 of the token. No line holds a token: the whole captured log is searched for each,
// and the same search finds one logged on purpose, so it can see a token where one is. A release
// of stored values alone writes no such line.
func TestEachMintedTokenWritesOneMintLineWithItsHashAndNeverTheToken(t *testing.T) {
	const (
		acmeToken      = "ghs_" + "AcmeInstallationTokenMinted0001"
		adaToken       = "ghs_" + "AdaInstallationTokenMinted00002"
		bareToken      = "ghs_" + "AdaBareTokenOfOneRepository0003"
		anthropicToken = "sk-ant-oat01-" + "FederationTokenMinted000004"
		appID          = "123456"                                          // policytest.TypeValue's github-app value
		serviceAccount = "robot-1@acme-deployment.iam.gserviceaccount.com" // policytest.TypeValue's anthropic-federation value
	)
	capture := captureRecords(t)
	github := `"github": {"accounts": ["acme", "ada"], "permissions": {"metadata": "read"}}`
	file := policytest.KindsFile(
		`{"name": "APP_KEY", "proxy": "example", `+github+`}`,
		`{"name": "APP_ADA_TOKEN", "source": "APP_KEY", "github": {"accounts": ["ada"], "repositories": ["ada/widgets"], "permissions": {"metadata": "read"}, "value": "token"}}`,
		`{"name": "FEDERATION_KEY", "proxy": "anthropic"}`,
	)
	m, enr, key := newAirlockFixture(t, file)
	m.AirlockProxyURL, m.AirlockCACertificate = testProxyURL, testCACertificate
	retag(t, m,
		policytest.TypeSecret("APP_KEY", fixtureOperator, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)),
		policytest.TypeSecret("FEDERATION_KEY", fixtureOperator, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)),
		policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"),
	)
	m.Minters = map[string]Minter{
		policy.TypeGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			if req.GitHub != nil && req.GitHub.Value == githubname.ValueToken {
				return secrets.Minted{Value: bareToken}, nil
			}
			return secrets.Minted{Value: `{"installations":{` +
				`"Acme":{"installation_id":41,"token":"` + acmeToken + `","expires_at":"2100-01-01T00:00:00Z"},` +
				`"Ada":{"installation_id":42,"token":"` + adaToken + `","expires_at":"2100-01-01T00:00:00Z"}}}`}, nil
		},
		policy.TypeAnthropicFederation: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
			return secrets.Minted{Value: anthropicToken}, nil
		},
	}

	// Stored values alone: no line.
	_, stored := grantOf(t, m, enr, key, "read the stored token", "SECOND_TOKEN")
	if _, err := m.Values(t.Context(), stored, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values(SECOND_TOKEN): %v", err)
	}
	if lines := mintLines(t, capture); len(lines) != 0 {
		t.Fatalf("a release of a stored value wrote %d mint lines %v; want none", len(lines), lines)
	}

	_, apps := grantOf(t, m, enr, key, "read the App's installations", "APP_KEY", "APP_ADA_TOKEN", "SECOND_TOKEN")
	if release, err := m.Values(t.Context(), apps, enr, ReleaseCall{}); err != nil || release.Airlock == nil || release.Values["APP_ADA_TOKEN"] != bareToken {
		t.Fatalf("Values(APP_KEY, APP_ADA_TOKEN, SECOND_TOKEN) = %+v, %v; want APP_KEY behind the proxy and APP_ADA_TOKEN's bare token handed over", release, err)
	}
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the organization's keys", "FEDERATION_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(FEDERATION_KEY) = %+v, %v; want a pending request", pending, err)
	}
	decided, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision(FEDERATION_KEY) = %+v, %v; want a grant", decided, err)
	}
	if release, err := m.Values(t.Context(), decided.GrantID, enr, ReleaseCall{}); err != nil || release.Airlock == nil {
		t.Fatalf("Values(FEDERATION_KEY) = %+v, %v; want it released behind the anthropic kind", release, err)
	}

	want := map[string]map[string]any{
		acmeToken: {"enrollment": enr, "grant": apps, "secret": "APP_KEY", "way": "mint", "type": policy.TypeGitHubApp,
			"slot": SlotGitHubPrefix + "acme", "app": appID, "account": "acme", "permissions": "metadata:read", "sha256": sha256Hex(acmeToken)},
		adaToken: {"enrollment": enr, "grant": apps, "secret": "APP_KEY", "way": "mint", "type": policy.TypeGitHubApp,
			"slot": SlotGitHubPrefix + "ada", "app": appID, "account": "ada", "permissions": "metadata:read", "sha256": sha256Hex(adaToken)},
		bareToken: {"enrollment": enr, "grant": apps, "secret": "APP_ADA_TOKEN", "way": "mint", "type": policy.TypeGitHubApp,
			"slot": SlotValue, "app": appID, "account": "ada", "repositories": "ada/widgets", "permissions": "metadata:read", "sha256": sha256Hex(bareToken)},
		anthropicToken: {"enrollment": enr, "grant": decided.GrantID, "secret": "FEDERATION_KEY", "way": "mint", "type": policy.TypeAnthropicFederation,
			"slot": SlotValue, "account": serviceAccount, "sha256": sha256Hex(anthropicToken)},
	}
	lines := mintLines(t, capture)
	if len(lines) != len(want) {
		t.Fatalf("the releases wrote %d mint lines %v; want %d, one for each token minted", len(lines), lines, len(want))
	}
	for _, fields := range want {
		matched := 0
		for _, line := range lines {
			if line["sha256"] == fields["sha256"] {
				matched++
				if !equalFields(line, fields) {
					t.Errorf("the mint line of %s's %s = %v; want %v", fields["secret"], fields["slot"], line, fields)
				}
			}
		}
		if matched != 1 {
			t.Errorf("%d mint lines hold the hash of %s's %s token; want one", matched, fields["secret"], fields["slot"])
		}
	}

	text := loggedText(capture)
	for token := range want {
		if strings.Contains(text, token) {
			t.Errorf("the log holds a minted token whole: %q", token)
		}
	}
	// The control: the same search, over the same capture, finds a token once one is logged.
	slog.Info("a line that holds a token, written on purpose", "token", acmeToken)
	if !strings.Contains(loggedText(capture), acmeToken) {
		t.Fatal("the search finds no token in a log that holds one, so its finding none above says nothing")
	}
}

// equalFields reports whether a logged line's attributes are fields exactly, each value as the
// line holds it in text.
func equalFields(line, fields map[string]any) bool {
	if len(line) != len(fields) {
		return false
	}
	for key, value := range fields {
		if got, held := line[key]; !held || got != value {
			return false
		}
	}
	return true
}
