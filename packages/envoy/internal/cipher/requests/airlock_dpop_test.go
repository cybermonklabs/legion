package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// podThumbprint stands for the pod key's RFC 7638 thumbprint, which Machine.ResultKey holds.
const podThumbprint = "the-pods-key-thumbprint"

// mintedTokens is a Machine.ResultTokens that mints a token naming the grant and the key it was
// asked for, and counts its mints.
type mintedTokens struct {
	mu    sync.Mutex
	mints []string
}

func (m *mintedTokens) MintResultToken(_ context.Context, grant oauthstore.ResultGrant, jkt string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token := "minted:" + grant.ID + ":" + jkt
	m.mints = append(m.mints, token)
	return token, nil
}

// resultGrant writes a CI run's result grant for check run checkRunID that ends at expires, on
// the fixture's database, and answers its placeholder and the grant.
func resultGrant(t *testing.T, m *Machine, checkRunID string, expires time.Time) (string, oauthstore.ResultGrant) {
	t.Helper()
	held, err := placeholder.Mint(placeholder.ResultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	grant := oauthstore.ResultGrant{
		ID: uuid.NewString(), PlaceholderSHA256: placeholder.Hash(held),
		Subject: "ci-results@example-org/example-repo", Session: "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml",
		Principal: "ci-results@example-org/example-repo", Scopes: []string{oauthwire.ScopeResults},
		Extra:     map[string]any{"ci": oauthwire.CIRun{Repository: "example-org/example-repo", CheckRunID: checkRunID}},
		ExpiresAt: expires.UTC().Truncate(time.Microsecond), EntryDigest: resultEntryDigest,
	}
	if err := oauthstore.New(m.Store.Pool).CreateResultGrant(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	return held, grant
}

// withResultGrants is m answering result placeholders from the broker's result_grants table on
// its own database, minting through tokens with the pod key podThumbprint names.
func withResultGrants(m *Machine, tokens *mintedTokens) *Machine {
	m.ResultGrants, m.ResultTokens, m.ResultKey, m.ResultEntryDigest = oauthstore.New(m.Store.Pool), tokens, podThumbprint, resultEntryDigest
	return m
}

// resultsProfiles is a profiles file of the results capability alone: the profile
// oauthwire.AirlockResultsName, a name and a proxy block of kind dpop, as a deployment writes it.
var resultsProfiles = policytest.ProfilesFile(`{"name": "` + oauthwire.AirlockResultsName + `", "proxy": {"hosts": ["dispatch.example.test"],
  "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}]}}`)

// withAirlock is m with the airlock in force under rules: the rules, the proxy's URL and its
// certificate, as the boot sets them from the profiles file's proxy blocks.
func withAirlock(m *Machine, rules proxyrules.Rules) *Machine {
	m.Airlock, m.AirlockProxyURL, m.AirlockCACertificate = rules, testProxyURL, testCACertificate
	return m
}

// TestAStoredSecretUnderADPoPEntryIsNeverReleased: a stored secret whose airlock entry is of kind
// dpop - the kind of a CI run's results capability, which the broker mints and holds - is never
// released, to the session or to the proxy, even where the policy serves it and a grant of it is
// made: the release is refused ErrAirlockMintedOnly, no placeholder row is written for it, and
// the answer holds nothing of its value. Two policies serve such a name:
//
//   - a profile of no kind serves AUTO_TOKEN while the airlock's entry of that name is of kind
//     dpop, so the release, and not the policy, is what stops it;
//   - the results profile, a name and a dpop block, is served when a stored secret of its name
//     exists, since its source is that secret and it passes every rule of the policy load; a
//     grant of it is made, and the airlock's entry, the profile's own block, holds the release.
func TestAStoredSecretUnderADPoPEntryIsNeverReleased(t *testing.T) {
	for _, row := range []struct {
		name, secret, value string
		// fixture builds the Machine and the session that asks for secret.
		fixture func(t *testing.T) (*Machine, string, *ecdsa.PrivateKey)
	}{
		{"a profile of no kind under a dpop entry", "AUTO_TOKEN", "auto-v1", func(t *testing.T) (*Machine, string, *ecdsa.PrivateKey) {
			m, enr, key := newAirlockFixture(t, proxied("api.example.com", "AUTO_TOKEN"))
			withAirlock(m, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", proxyrules.KindDPoP)}})
			return m, enr, key
		}},
		{"a stored secret of the results profile's name", oauthwire.AirlockResultsName, "stored-results-v1", func(t *testing.T) (*Machine, string, *ecdsa.PrivateKey) {
			m, enr, key := newAirlockFixture(t, resultsProfiles)
			withAirlock(m, policytest.Profiles(t, resultsProfiles, fixtureService).ProxyRules())
			retag(t, m, policytest.Secret(oauthwire.AirlockResultsName, fixtureOperator, policy.TierAgent, "stored-results-v1"))
			set := m.Policy.Get()
			if served, ok := set.Secrets[oauthwire.AirlockResultsName]; !ok || served.Profile == nil || served.Profile.Name != oauthwire.AirlockResultsName {
				t.Fatalf("the policy serves %s as %+v (%t), refused %+v; want it served as the results profile, whose source the stored secret is",
					oauthwire.AirlockResultsName, served, ok, set.Refused["profile:"+oauthwire.AirlockResultsName])
			}
			if entry := m.Airlock.Lookup(oauthwire.AirlockResultsName); entry == nil || entry.Kind != proxyrules.KindDPoP {
				t.Fatalf("the airlock's entry %s = %+v; want the profile's block, of kind %s", oauthwire.AirlockResultsName, entry, proxyrules.KindDPoP)
			}
			return m, enr, key
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, enr, key := row.fixture(t)
			_, grantID := grantOf(t, m, enr, key, "read it", row.secret)
			release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
			if !errors.Is(err, ErrAirlockMintedOnly) || !IsRefusal(err) {
				t.Fatalf("the session's release of %s, a stored secret under a dpop entry = %v; want ErrAirlockMintedOnly, a refusal", row.secret, err)
			}
			holdsNothingOf(t, release, row.value)
			if rows := placeholderRows(t, m, grantID); len(rows) != 0 {
				t.Fatalf("the refused release wrote placeholder rows %v; want none", rows)
			}
			values, _, err := m.AirlockValues(t.Context(), grantID)
			if !errors.Is(err, ErrAirlockMintedOnly) || !IsRefusal(err) || values != nil {
				t.Fatalf("the proxy's release of %s = %v, %v; want ErrAirlockMintedOnly, a refusal, and no values", row.secret, values, err)
			}
			if rows := placeholderRows(t, m, grantID); len(rows) != 0 {
				t.Fatalf("the proxy's refused release wrote placeholder rows %v; want none", rows)
			}
		})
	}
}

// TestARequestForTheResultsCapabilityIsRefusedBeforeAnyGrant is the deployment's case: no stored
// secret is named DISPATCH_RESULTS_TOKEN, since the broker mints that credential for a CI run's
// result grant from no stored secret. The policy load refuses the results profile as a profile
// whose source is no stored secret (profile-source-missing, its source absent), so a session's
// request for the name is refused ErrProfileRefused before anything is written: no grant row and
// no airlock_placeholders row. (Where such a stored secret exists the policy serves the profile,
// and the release is refused instead: TestAStoredSecretUnderADPoPEntryIsNeverReleased.)
func TestARequestForTheResultsCapabilityIsRefusedBeforeAnyGrant(t *testing.T) {
	m, enr, key := newAirlockFixture(t, resultsProfiles)
	withAirlock(m, policytest.Profiles(t, resultsProfiles, fixtureService).ProxyRules())
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "write the results", oauthwire.AirlockResultsName), "")
	if !errors.Is(err, policy.ErrProfileRefused) || request.GrantID != nil {
		t.Fatalf("Create(%s) = %+v, %v; want policy.ErrProfileRefused and no grant", oauthwire.AirlockResultsName, request, err)
	}
	for _, want := range []string{policy.ReasonProfileSourceMissing, "source " + oauthwire.AirlockResultsName + ": " + policy.ReasonAbsent} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Create(%s)'s refusal %q does not say %q", oauthwire.AirlockResultsName, err, want)
		}
	}
	var grants, placeholders int
	if err := m.Store.Pool.QueryRow(t.Context(), `select (select count(*) from grants), (select count(*) from airlock_placeholders)`).Scan(&grants, &placeholders); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || placeholders != 0 {
		t.Fatalf("the refused request left %d grant rows and %d airlock_placeholders rows; want none", grants, placeholders)
	}
}

// TestAnEndedResultPlaceholderIsNeverReadAsASecretPlaceholder: a result placeholder is answered
// from result_grants alone. Once its grant has ended it opens nothing, even where a row of
// airlock_placeholders holds the same hash for a live secret grant: the connection read never
// falls through to the secret store's placeholders.
func TestAnEndedResultPlaceholderIsNeverReadAsASecretPlaceholder(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", "")}})
	withResultGrants(m, &mintedTokens{})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatal(err)
	}
	held, _ := resultGrant(t, m, "97", time.Now().Add(-time.Second))
	if _, err := m.Store.Pool.Exec(t.Context(), `update airlock_placeholders set placeholder_sha256 = $1 where grant_id = $2`, placeholder.Hash(held), grantID); err != nil {
		t.Fatal(err)
	}
	if conn, err := m.AirlockConnection(t.Context(), held); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection(an ended grant's result placeholder) = %+v, %v; want ErrGrantNotLive and no grant, never the secret grant %s", conn, err, grantID)
	}
}

// TestAResultPlaceholderIsAnsweredFromItsGrant: a live result grant's placeholder opens a
// connection of that grant - its id, its session and the check run of its job, no enrollment, and
// DISPATCH_RESULTS_TOKEN's one slot - and the proxy's read of its values mints the run's token for
// the pod's key, with no end of its own, since the grant's end is the connection read's. A grant
// that is no secret's is answered no access-token placeholder.
func TestAResultPlaceholderIsAnsweredFromItsGrant(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	tokens := &mintedTokens{}
	withResultGrants(m, tokens)
	held, grant := resultGrant(t, m, "97", time.Now().Add(15*time.Minute))
	conn, err := m.AirlockConnection(t.Context(), held)
	if err != nil {
		t.Fatalf("AirlockConnection(a live grant's result placeholder) = %v", err)
	}
	id := oauthwire.ResultGrantIDPrefix + grant.ID
	slots := conn.Names[oauthwire.AirlockResultsName].Slots
	if conn.GrantID != id || conn.SessionID != grant.Session || conn.CheckRunID != "97" || conn.EnrollmentID != "" ||
		conn.Expires == nil || !conn.Expires.Equal(grant.ExpiresAt) || len(conn.Names) != 1 || len(slots) != 1 || slots[SlotValue] != held {
		t.Fatalf("the result connection = %+v; want grant %s, session %s, check run 97, no enrollment, its end and %s's value slot alone", conn, id, grant.Session, oauthwire.AirlockResultsName)
	}
	values, version, err := m.AirlockValues(t.Context(), id)
	if err != nil || version == "" {
		t.Fatalf("AirlockValues(%s) = %v, %q, %v", id, values, version, err)
	}
	want := "minted:" + grant.ID + ":" + podThumbprint
	got := values[oauthwire.AirlockResultsName]
	if len(values) != 1 || got.Slots[SlotValue] != want || !got.Expires.IsZero() || len(tokens.mints) != 1 {
		t.Fatalf("the result values = %+v after %d mints; want the token minted for the grant and the pod key, %q, with no end, minted once", values, len(tokens.mints), want)
	}
	if _, err := m.AirlockAccessPlaceholder(t.Context(), id, oauthwire.AirlockResultsName, SlotAzureAccessPrefix+"scope"); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockAccessPlaceholder(%s) = %v; want ErrGrantNotLive", id, err)
	}
	if _, _, err := m.AirlockValues(t.Context(), oauthwire.ResultGrantIDPrefix+uuid.NewString()); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockValues(a grant no row holds) = %v; want ErrGrantNotLive", err)
	}
}
