package requests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
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
		ExpiresAt: expires.UTC().Truncate(time.Microsecond),
	}
	if err := oauthstore.New(m.Store.Pool).CreateResultGrant(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	return held, grant
}

// withResultGrants is m answering result placeholders from the broker's result_grants table on
// its own database, minting through tokens with the pod key podThumbprint names.
func withResultGrants(m *Machine, tokens *mintedTokens) *Machine {
	m.ResultGrants, m.ResultTokens, m.ResultKey = oauthstore.New(m.Store.Pool), tokens, podThumbprint
	return m
}

// TestAStoredSecretUnderADPoPEntryIsNeverReleased: a stored secret whose rules entry is of kind
// dpop - the kind of a CI run's results capability, which the broker mints and holds - is never
// released, to the session or to the proxy: the release is refused ErrAirlockMintedOnly, no
// placeholder row is written for it, and the answer holds nothing of its value.
func TestAStoredSecretUnderADPoPEntryIsNeverReleased(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", proxyrules.KindDPoP)}})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{})
	if !errors.Is(err, ErrAirlockMintedOnly) || !IsRefusal(err) {
		t.Fatalf("the session's release of a stored secret under a dpop entry = %v; want ErrAirlockMintedOnly, a refusal", err)
	}
	holdsNothingOf(t, release, "auto-v1")
	if rows := placeholderRows(t, m, grantID); len(rows) != 0 {
		t.Fatalf("the refused release wrote placeholder rows %v; want none", rows)
	}
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if !errors.Is(err, ErrAirlockMintedOnly) || values != nil {
		t.Fatalf("the proxy's release of it = %v, %v; want ErrAirlockMintedOnly and no values", values, err)
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
	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil {
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
