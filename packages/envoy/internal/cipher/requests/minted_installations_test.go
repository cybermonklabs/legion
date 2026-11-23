// packages/envoy/internal/cipher/requests/minted_installations_test.go
// A profile served from a github-installations secret: installation tokens a minter outside the
// broker minted ahead of any request, which the broker hands over and never mints. A target may
// choose one account of such a profile and never repositories, since a token cannot be cut after
// it is minted; Create's check before an approval reads the stored tokens as a release would.
package requests

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// storedTokens is a github-installations value holding a token of each of accounts, keyed by the
// login as written, "ghs_stored_<account>", which lapses fifty minutes from now (the least a minter
// that runs every ten minutes leaves on a token GitHub gives an hour) and holds permissions.
func storedTokens(t *testing.T, permissions map[string]string, accounts ...string) string {
	t.Helper()
	installations := map[string]any{}
	for i, account := range accounts {
		installations[account] = map[string]any{
			"installation_id": 41 + i, "token": "ghs_stored_" + strings.ToLower(account),
			"expires_at":  time.Now().Add(50 * time.Minute).UTC().Format(time.RFC3339),
			"permissions": permissions, "repository_selection": "all",
		}
	}
	value, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": installations})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// readPermissions is what TOKENS_READ asks for: contents and metadata at read.
var readPermissions = map[string]string{"contents": "read", "metadata": "read"}

// tokensRead is the profiles file of TOKENS_READ, a bare token of acme or ada at read, served from
// APP_TOKENS.
var tokensRead = policytest.ProfilesFile(`{"name": "TOKENS_READ", "source": "APP_TOKENS", "github": {"accounts": ["acme", "ada"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}}`)

// mintedFixture is newAppFixture serving APP_TOKENS, a github-installations secret of tier
// holding value, through the profiles of file, with the broker's own github-installations minter
// and no other: a release that asked GitHub for a token would find no minter to ask.
func mintedFixture(t *testing.T, tier, value, file string) (*Machine, string, *ecdsa.PrivateKey) {
	t.Helper()
	m, enrollmentID, key, _ := newAppFixture(t)
	fixtureStore(m).Put(policytest.TypeSecret("APP_TOKENS", fixtureOperator, tier, policy.TypeGitHubInstallations, value))
	m.Policy = policytest.ProfiledCurrent(t, fixtureStore(m), file, fixtureService)
	m.Minters = map[string]Minter{policy.TypeGitHubInstallations: secrets.GitHubInstallations{}.Mint}
	return m, enrollmentID, key
}

// requestRows counts the request rows and credential-request records m holds.
func requestRows(t *testing.T, m *Machine) (rows, records int) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(t.Context(), `select (select count(*) from requests), (select count(*) from credential_requests)`).Scan(&rows, &records); err != nil {
		t.Fatalf("count the request rows: %v", err)
	}
	return rows, records
}

// TestATargetNamingRepositoriesOfAMintedInstallationsProfileIsRefusedWithItsOwnError: a stored
// token was minted for its installation whole before the request, and cannot be cut to
// repositories, so a target naming a repository of a profile served from a github-installations
// secret is refused at Create with ErrTargetRepositoriesRefused, which is an ErrTargetRefused,
// before a request row or a record is written, telling the caller to name the account alone. A
// target naming the account is granted, and its release hands over that account's stored token,
// with no token minted. A value-token profile of two accounts asked for with no target tells the
// caller to name one with --target ACCOUNT, and not with a repository.
//
// The named revert: drop the minted-source rule from gitHubScope, and the repository target is
// granted an installation-wide token the caller did not ask for.
func TestATargetNamingRepositoriesOfAMintedInstallationsProfileIsRefusedWithItsOwnError(t *testing.T) {
	m, enrollmentID, key := mintedFixture(t, policy.TierAgent, storedTokens(t, readPermissions, "acme", "ada"), tokensRead)

	_, err := m.Create(t.Context(), enrollmentID, signTargetRequest(t, m, key, "TOKENS_READ", "https://github.com/ada/proofs"), "")
	if !errors.Is(err, ErrTargetRepositoriesRefused) || !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "--target ada") {
		t.Fatalf("Create(TOKENS_READ --target ada/proofs) = %v; want ErrTargetRepositoriesRefused, an ErrTargetRefused, telling the caller --target ada", err)
	}
	if rows, records := requestRows(t, m); rows != 0 || records != 0 {
		t.Fatalf("the refused target left %d request rows and %d records; want none", rows, records)
	}

	_, err = m.Create(t.Context(), enrollmentID, signTargetRequest(t, m, key, "TOKENS_READ"), "")
	if !errors.Is(err, ErrTargetRefused) || errors.Is(err, ErrTargetRepositoriesRefused) || !strings.Contains(err.Error(), "--target ACCOUNT") || strings.Contains(err.Error(), "REPOSITORY") {
		t.Fatalf("Create(TOKENS_READ) with no target = %v; want ErrTargetRefused telling the caller to name one with --target ACCOUNT alone", err)
	}

	request, err := m.Create(t.Context(), enrollmentID, signTargetRequest(t, m, key, "TOKENS_READ", "https://github.com/ada"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(TOKENS_READ --target ada) = %+v, %v; want a grant", request, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, enrollmentID, ReleaseCall{})
	if err != nil || release.Values["TOKENS_READ"] != "ghs_stored_ada" {
		t.Fatalf("Values = %+v, %v; want ada's stored token", release, err)
	}
}

// TestAMintedInstallationsProfileIsCheckedAgainstItsStoredTokensBeforeAnyoneIsAsked: Create's
// check before an approval reads a profile served from a github-installations secret as its
// release would: a stored token wider than the profile is refused before a request row or a
// record is written, so nobody is asked to approve a release the broker would refuse, while a
// stored token within the profile's permissions leaves the request pending on its approver.
//
// The named revert: keep the check to github-app names, and the wider token's request goes to
// its approver.
func TestAMintedInstallationsProfileIsCheckedAgainstItsStoredTokensBeforeAnyoneIsAsked(t *testing.T) {
	wider := maps.Clone(readPermissions)
	wider["administration"] = "write"
	m, enrollmentID, key := mintedFixture(t, policy.TierHuman, storedTokens(t, wider, "acme", "ada"), tokensRead)
	_, err := m.Create(t.Context(), enrollmentID, signTargetRequest(t, m, key, "TOKENS_READ", "https://github.com/ada"), "")
	if !errors.Is(err, secrets.ErrMintStoredWider) || !strings.Contains(err.Error(), "administration:write") {
		t.Fatalf("Create(TOKENS_READ --target ada) over a token wider than the profile = %v; want ErrMintStoredWider naming administration:write", err)
	}
	if rows, records := requestRows(t, m); rows != 0 || records != 0 {
		t.Fatalf("the refused request left %d request rows and %d records; want none", rows, records)
	}

	fixtureStore(m).Put(policytest.TypeSecret("APP_TOKENS", fixtureOperator, policy.TierHuman, policy.TypeGitHubInstallations, storedTokens(t, readPermissions, "acme", "ada")))
	request, err := m.Create(t.Context(), enrollmentID, signTargetRequest(t, m, key, "TOKENS_READ", "https://github.com/ada"), "")
	if err != nil || request.State != "pending" {
		t.Fatalf("Create(TOKENS_READ --target ada) over a token within the profile = %+v, %v; want it pending on its approver", request, err)
	}
}

// TestAMintedInstallationsValueSplitsAsAGitHubAppValue: behind the airlock, a release of a
// github-installations profile is taken apart as a github-app profile's is: a bare token is the
// one value slot, and the installations object one slot per account, so the proxy swaps the
// stored token in where the session holds its placeholder, and a command's log is redacted of it.
func TestAMintedInstallationsValueSplitsAsAGitHubAppValue(t *testing.T) {
	split, err := airlockSplitOf("", policy.TypeGitHubInstallations, githubname.ValueToken, "ghs_stored_ada")
	if err != nil || !maps.Equal(split.values, map[string]string{SlotValue: "ghs_stored_ada"}) {
		t.Fatalf("the split of a bare token = %v, %v; want the one value slot holding it", split.values, err)
	}
	installations := `{"installations":{"Ada":{"installation_id":42,"token":"ghs_stored_ada","expires_at":"2026-11-22T13:00:00Z"}}}`
	split, err = airlockSplitOf("", policy.TypeGitHubInstallations, githubname.ValueInstallations, installations)
	if err != nil || !slices.Equal(split.slots(), []string{SlotGitHubPrefix + "ada"}) || split.values[SlotGitHubPrefix+"ada"] != "ghs_stored_ada" {
		t.Fatalf("the split of an installations object = %v, %v; want ada's slot holding its token", split.values, err)
	}
}
