package requests

import (
	"errors"
	"strings"
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

// resultEntryDigest stands for the results entry a broker grants under and serves (cmd/cipherd's
// resultEntryDigest, config.MintedEntryDigest): 64 hex digits, as result_grants holds every
// grant's. resultGrant writes each grant under it, and withResultGrants serves under it.
const resultEntryDigest = "3333333333333333333333333333333333333333333333333333333333333333"

// anotherEntryDigest is the results entry of a broker of another profile DISPATCH_RESULTS_TOKEN or
// another private destination.
const anotherEntryDigest = "4444444444444444444444444444444444444444444444444444444444444444"

// resultGrant writes a CI run's result grant for check run checkRunID that ends at expires into
// the results kind's table on the fixture's database (oauthstore.ResultGrants), and answers its
// placeholder and the grant.
func resultGrant(t *testing.T, m *Machine, checkRunID string, expires time.Time) (string, oauthstore.MintedGrant) {
	t.Helper()
	held, err := placeholder.Mint(placeholder.MintedPrefix(oauthwire.AirlockResultsWord))
	if err != nil {
		t.Fatal(err)
	}
	grant := oauthstore.MintedGrant{
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

// withResultGrants is m serving the results capability as cmd/cipherd wires it: the kind of
// DISPATCH_RESULTS_TOKEN, its grants read from result_grants on m's own database, under
// resultEntryDigest, minting through tokens with the pod key podThumbprint names.
func withResultGrants(m *Machine, tokens *mintedTokens) *Machine {
	m.Minted = []MintedKind{{
		Name: oauthwire.AirlockResultsName, Word: oauthwire.AirlockResultsWord,
		Grants: oauthstore.New(m.Store.Pool).ResultGrants(), EntryDigest: resultEntryDigest,
	}}
	m.MintedTokens, m.MintedKey = tokens, podThumbprint
	return m
}

// TestAResultPlaceholderIsAnsweredFromItsGrant: a live result grant's placeholder, read from
// result_grants, opens a connection of that grant - its id, its session and the check run of its
// job, no enrollment, marked minted, and DISPATCH_RESULTS_TOKEN's one slot - and the proxy's read
// of its values mints the run's token for the pod's key, with no end of its own, since the grant's
// end is the connection read's.
func TestAResultPlaceholderIsAnsweredFromItsGrant(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	tokens := &mintedTokens{}
	withResultGrants(m, tokens)
	held, grant := resultGrant(t, m, "97", time.Now().Add(15*time.Minute))
	conn, err := m.AirlockConnection(t.Context(), held)
	if err != nil {
		t.Fatalf("AirlockConnection(a live grant's result placeholder) = %v", err)
	}
	id := oauthwire.AirlockResultsWord + ":" + grant.ID
	slots := conn.Names[oauthwire.AirlockResultsName].Slots
	if conn.GrantID != id || conn.SessionID != grant.Session || conn.CheckRunID != "97" || !conn.Minted || conn.EnrollmentID != "" ||
		conn.Expires == nil || !conn.Expires.Equal(grant.ExpiresAt) || len(conn.Names) != 1 || len(slots) != 1 || slots[SlotValue] != held {
		t.Fatalf("the result connection = %+v; want grant %s, session %s, check run 97, minted, no enrollment, its end and %s's value slot alone", conn, id, grant.Session, oauthwire.AirlockResultsName)
	}
	values, version, err := m.AirlockValues(t.Context(), id)
	if err != nil || version == "" {
		t.Fatalf("AirlockValues(%s) = %v, %q, %v", id, values, version, err)
	}
	want := "minted:" + grant.ID + ":" + podThumbprint
	got := values[oauthwire.AirlockResultsName]
	if len(values) != 1 || got.Slots[SlotValue] != want || !got.Expires.IsZero() || tokens.count() != 1 {
		t.Fatalf("the result values = %+v after %d mints; want the token minted for the grant and the pod key, %q, with no end, minted once", values, tokens.count(), want)
	}
}

// TestAResultGrantOfAnotherEntryIsNotLive: a broker pod serves a result grant only under the
// results entry the grant was made under. A grant of another entry is answered ErrGrantNotLive,
// saying it was made under another dpop block or private destination, by the proxy's read of the
// connection and by its read of the values, and nothing is minted for it; so is every grant on a
// broker with no results entry. The refusal says the grant is refused by this broker, and none of
// ErrGrantNotLive's own words, since the grant has not expired, been revoked or lost its session.
// The grant is refused, not ended: a pod of its own entry serves it afterwards. The pod's own
// grant, in the same database, opens its connection and is minted its token.
func TestAResultGrantOfAnotherEntryIsNotLive(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	tokens := &mintedTokens{}
	withResultGrants(m, tokens)
	ownHeld, own := resultGrant(t, m, "97", time.Now().Add(15*time.Minute))
	otherHeld, other := resultGrant(t, m, "98", time.Now().Add(15*time.Minute))
	if _, err := m.Store.Pool.Exec(t.Context(), `update result_grants set entry_digest = $1 where id = $2`, anotherEntryDigest, other.ID); err != nil {
		t.Fatal(err)
	}
	refused := func(what, held string, grant oauthstore.MintedGrant) {
		t.Helper()
		const why = "the grant is refused by this broker: it was made under another dpop block or private destination"
		untrue := func(err error) bool {
			return strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "revoked") || strings.Contains(err.Error(), "session ended")
		}
		conn, err := m.AirlockConnection(t.Context(), held)
		if !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), why) || untrue(err) || conn.GrantID != "" {
			t.Errorf("AirlockConnection(%s) = %+v, %v; want ErrGrantNotLive saying %q and not that the grant ended, and no grant", what, conn, err, why)
		}
		values, version, err := m.AirlockValues(t.Context(), oauthwire.AirlockResultsWord+":"+grant.ID)
		if !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), why) || untrue(err) || values != nil || version != "" {
			t.Errorf("AirlockValues(%s) = %v, %q, %v; want ErrGrantNotLive saying %q and not that the grant ended, and no values", what, values, version, err, why)
		}
	}
	refused("a grant of another entry", otherHeld, other)
	m.Minted[0].EntryDigest = ""
	refused("its own entry's grant, on a broker with no results entry", ownHeld, own)
	m.Minted[0].EntryDigest = resultEntryDigest
	if n := tokens.count(); n != 0 {
		t.Fatalf("the refused reads minted %d tokens; want no token", n)
	}

	// Control: the pod's own grant opens its connection and is minted its token.
	if conn, err := m.AirlockConnection(t.Context(), ownHeld); err != nil || conn.GrantID != oauthwire.AirlockResultsWord+":"+own.ID {
		t.Fatalf("AirlockConnection(the pod's own entry's grant) = %+v, %v; want the grant %s", conn, err, own.ID)
	}
	if values, _, err := m.AirlockValues(t.Context(), oauthwire.AirlockResultsWord+":"+own.ID); err != nil || len(values) != 1 || tokens.count() != 1 {
		t.Fatalf("AirlockValues(the pod's own entry's grant) = %v, %v after %d mints; want its token, minted once", values, err, tokens.count())
	}
	// And the refused grant was not ended: a pod of its entry serves it.
	m.Minted[0].EntryDigest = anotherEntryDigest
	if conn, err := m.AirlockConnection(t.Context(), otherHeld); err != nil || conn.GrantID != oauthwire.AirlockResultsWord+":"+other.ID {
		t.Fatalf("AirlockConnection(the refused grant, on a pod of its own entry) = %+v, %v; want the grant %s, still live", conn, err, other.ID)
	}
}

// resultsProfiles is a profiles file of the results capability alone: the profile
// oauthwire.AirlockResultsName, a name and a proxy block of kind dpop, as a deployment writes it.
var resultsProfiles = policytest.ProfilesFile(`{"name": "` + oauthwire.AirlockResultsName + `", "proxy": {"hosts": ["dispatch.example.test"],
  "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}]}}`)

// TestARequestForTheResultsCapabilityIsRefusedBeforeAnyGrant is the deployment's case: the broker
// mints the results capability's credential for a CI run's result grant from no stored secret, and
// the policy serves its profile, a name and a dpop block, as nothing (policy.ReasonProfileMinted):
// so a session's request for DISPATCH_RESULTS_TOKEN is refused before anything is written, no
// grant row and no airlock_placeholders row, with no stored secret of its name and with one, since
// a stored secret of that name is not served through the profile either. The airlock's entry of
// the name is still the profile's block, of kind dpop.
func TestARequestForTheResultsCapabilityIsRefusedBeforeAnyGrant(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(map[bool]string{false: "no stored secret of its name", true: "a stored secret of its name"}[stored], func(t *testing.T) {
			m, enr, key := newAirlockFixture(t, resultsProfiles)
			m.Airlock = policytest.Profiles(t, resultsProfiles, fixtureService).ProxyRules()
			if stored {
				retag(t, m, policytest.Secret(oauthwire.AirlockResultsName, fixtureOperator, policy.TierAgent, "stored-results-v1"))
			}
			if entry := m.Airlock.Lookup(oauthwire.AirlockResultsName); entry == nil || entry.Kind != proxyrules.KindDPoP {
				t.Fatalf("the airlock's entry %s = %+v; want the profile's block, of kind %s", oauthwire.AirlockResultsName, entry, proxyrules.KindDPoP)
			}
			request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "write the results", oauthwire.AirlockResultsName), "")
			if !errors.Is(err, policy.ErrUnknownSecret) || request.GrantID != nil {
				t.Fatalf("Create(%s) = %+v, %v; want policy.ErrUnknownSecret and no grant", oauthwire.AirlockResultsName, request, err)
			}
			if strings.Contains(err.Error(), "stored-results-v1") {
				t.Fatalf("Create(%s)'s refusal %q quotes the stored value", oauthwire.AirlockResultsName, err)
			}
			var grants, placeholders int
			if err := m.Store.Pool.QueryRow(t.Context(), `select (select count(*) from grants), (select count(*) from airlock_placeholders)`).Scan(&grants, &placeholders); err != nil {
				t.Fatal(err)
			}
			if grants != 0 || placeholders != 0 {
				t.Fatalf("the refused request left %d grant rows and %d airlock_placeholders rows; want none", grants, placeholders)
			}
		})
	}
}
