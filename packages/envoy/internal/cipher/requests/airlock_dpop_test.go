package requests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// podThumbprint stands for the pod key's RFC 7638 thumbprint, which Machine.MintedKey holds.
const podThumbprint = "the-pods-key-thumbprint"

// The minted kind these tests serve: its airlock name, its word and the digest of the entry this
// broker serves it under.
const (
	testMintedName   = "TEST_MINTED_TOKEN"
	testMintedWord   = "testkind"
	testEntryDigest  = "1111111111111111111111111111111111111111111111111111111111111111"
	otherEntryDigest = "2222222222222222222222222222222222222222222222222222222222222222"
)

// mintedTokens is a Machine.MintedTokens that mints a token naming the grant and the key it was
// asked for, and counts its mints.
type mintedTokens struct {
	mu    sync.Mutex
	mints []string
}

func (m *mintedTokens) MintGrantToken(_ context.Context, grant oauthstore.MintedGrant, jkt string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token := "minted:" + grant.ID + ":" + jkt
	m.mints = append(m.mints, token)
	return token, nil
}

func (m *mintedTokens) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.mints)
}

// memoryGrants is a MintedGrants that holds its grants in memory, keyed by the placeholder's hash,
// and answers a grant past its end as not live, as a kind's table does by the database's clock.
type memoryGrants struct {
	mu     sync.Mutex
	grants map[string]oauthstore.MintedGrant
}

func (g *memoryGrants) live(match func(oauthstore.MintedGrant) bool) (oauthstore.MintedGrant, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, grant := range g.grants {
		if match(grant) && grant.ExpiresAt.After(time.Now()) {
			return grant, nil
		}
	}
	return oauthstore.MintedGrant{}, oauthstore.ErrMintedGrantNotLive
}

func (g *memoryGrants) MintedByPlaceholder(_ context.Context, sha256Hex string) (oauthstore.MintedGrant, error) {
	return g.live(func(grant oauthstore.MintedGrant) bool { return grant.PlaceholderSHA256 == sha256Hex })
}

func (g *memoryGrants) MintedByID(_ context.Context, id string) (oauthstore.MintedGrant, error) {
	return g.live(func(grant oauthstore.MintedGrant) bool { return grant.ID == id })
}

// grant writes one grant of the test kind, made under digest, that ends at expires, and answers
// its placeholder and the grant.
func (g *memoryGrants) grant(t *testing.T, digest string, expires time.Time) (string, oauthstore.MintedGrant) {
	t.Helper()
	held, err := placeholder.Mint(placeholder.MintedPrefix(testMintedWord))
	if err != nil {
		t.Fatal(err)
	}
	grant := oauthstore.MintedGrant{
		ID: uuid.NewString(), PlaceholderSHA256: placeholderHash(held),
		Subject: "ada@example.com", Session: "ses_minted_test", Principal: "ada@example.com",
		Scopes: []string{"test-scope"}, Extra: map[string]any{"person": "ada@example.com"},
		ExpiresAt: expires.UTC().Truncate(time.Microsecond), EntryDigest: digest,
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grants == nil {
		g.grants = map[string]oauthstore.MintedGrant{}
	}
	g.grants[grant.PlaceholderSHA256] = grant
	return held, grant
}

// withMintedKind is m serving the test kind from grants under the entry digest testEntryDigest,
// minting through tokens with the pod key podThumbprint names.
func withMintedKind(m *Machine, grants *memoryGrants, tokens *mintedTokens) *Machine {
	m.Minted = []MintedKind{{Name: testMintedName, Word: testMintedWord, Grants: grants, EntryDigest: testEntryDigest}}
	m.MintedTokens, m.MintedKey = tokens, podThumbprint
	return m
}

// TestAStoredSecretUnderADPoPEntryIsNeverReleased: a stored secret whose name a dpop block binds -
// the block of a capability the broker mints and holds - is never released, to the session or to
// the proxy: the one read both releases share refuses it ErrMintedOnly, a refusal, before anything
// of it is read or a placeholder row written. The live policy serves no secret by a name a dpop
// block holds (the profiles' apply refuses the name, policy.ReasonProfileMinted), so the refusal
// is the release's own second line, reached here with the set the loader builds before that apply:
// in it the stored AUTO_TOKEN is served by its own name and the dpop block of its name binds it,
// under the grant the session got of AUTO_TOKEN while no block held its name.
func TestAStoredSecretUnderADPoPEntryIsNeverReleased(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example")))
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	checked, err := m.checkRelease(t.Context(), grantID, enr, "")
	if err != nil {
		t.Fatalf("checkRelease: %v", err)
	}
	loader := policytest.Loader(fixtureStore(m), fixtureService)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(`{"name": "AUTO_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`), fixtureService)
	unapplied, err := loader.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, served := unapplied.Secrets["AUTO_TOKEN"]; !served {
		t.Fatalf("the set before the profiles' apply does not serve the stored AUTO_TOKEN; refused %+v", unapplied.Refused)
	}
	if binding := unapplied.ProxyRules().Lookup("AUTO_TOKEN"); binding == nil || binding.Way != proxyrules.WayDPoP {
		t.Fatalf("AUTO_TOKEN's binding in the set before the profiles' apply = %+v; want the dpop block's", binding)
	}
	checked.set = unapplied
	for _, want := range []releaseRead{{}, {airlockOnly: true}} {
		read, err := m.readRelease(t.Context(), grantID, checked, want)
		if !errors.Is(err, ErrMintedOnly) || !IsRefusal(err) || read != nil {
			t.Fatalf("the release (proxy's: %t) of a stored secret under a dpop block = %v, %v; want ErrMintedOnly, a refusal, and nothing read", want.airlockOnly, read, err)
		}
	}
	if rows := placeholderRows(t, m, grantID); len(rows) != 0 {
		t.Fatalf("the refused release wrote placeholder rows %v; want none", rows)
	}
}

// TestAnEndedMintedPlaceholderIsNeverReadAsASecretPlaceholder: a minted kind's placeholder is
// answered from its kind's grants alone. Once its grant has ended it opens nothing, even where a
// row of airlock_placeholders holds the same hash for a live secret grant: the connection read
// never falls through to the secret store's placeholders.
func TestAnEndedMintedPlaceholderIsNeverReadAsASecretPlaceholder(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example")))
	grants := &memoryGrants{}
	withMintedKind(m, grants, &mintedTokens{})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatal(err)
	}
	held, _ := grants.grant(t, testEntryDigest, time.Now().Add(-time.Second))
	if _, err := m.Store.Pool.Exec(t.Context(), `update airlock_placeholders set placeholder_sha256 = $1 where grant_id = $2`, placeholderHash(held), grantID); err != nil {
		t.Fatal(err)
	}
	if conn, err := m.AirlockConnection(t.Context(), held); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection(an ended grant's minted placeholder) = %+v, %v; want ErrGrantNotLive and no grant, never the secret grant %s", conn, err, grantID)
	}
}

// TestAPlaceholderOfNoMintedKindOpensNothing: a placeholder that holds a kind's word and hyphen is
// a minted grant's, so one whose word no kind of this broker serves opens nothing and is never
// read as a secret grant's placeholder, even where airlock_placeholders holds its hash.
func TestAPlaceholderOfNoMintedKindOpensNothing(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example")))
	withMintedKind(m, &memoryGrants{}, &mintedTokens{})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatal(err)
	}
	held, err := placeholder.Mint(placeholder.MintedPrefix("otherkind"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.Pool.Exec(t.Context(), `update airlock_placeholders set placeholder_sha256 = $1 where grant_id = $2`, placeholderHash(held), grantID); err != nil {
		t.Fatal(err)
	}
	if conn, err := m.AirlockConnection(t.Context(), held); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection(a placeholder of a kind no one serves) = %+v, %v; want ErrGrantNotLive and no grant", conn, err)
	}
	if _, _, err := m.AirlockValues(t.Context(), "otherkind:"+uuid.NewString()); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockValues(a grant id of a kind no one serves) = %v; want ErrGrantNotLive", err)
	}
}

// TestAMintedPlaceholderIsAnsweredFromItsGrant: a live minted grant's placeholder opens a
// connection of that grant - its kind's word and its id, its session, no enrollment, marked
// minted, and the kind's one name with its one slot - and the proxy's read of its values mints
// the grant's token for the pod's key, with no end of its own, since the grant's end is the
// connection read's. A minted grant is answered no access-token placeholder.
func TestAMintedPlaceholderIsAnsweredFromItsGrant(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	grants, tokens := &memoryGrants{}, &mintedTokens{}
	withMintedKind(m, grants, tokens)
	held, grant := grants.grant(t, testEntryDigest, time.Now().Add(15*time.Minute))
	conn, err := m.AirlockConnection(t.Context(), held)
	if err != nil {
		t.Fatalf("AirlockConnection(a live grant's minted placeholder) = %v", err)
	}
	id := testMintedWord + ":" + grant.ID
	slots := conn.Names[testMintedName].Slots
	if conn.GrantID != id || conn.SessionID != grant.Session || !conn.Minted || conn.EnrollmentID != "" ||
		conn.Expires == nil || !conn.Expires.Equal(grant.ExpiresAt) || len(conn.Names) != 1 || len(slots) != 1 || slots[SlotValue] != held {
		t.Fatalf("the minted connection = %+v; want grant %s, session %s, minted, no enrollment, its end and %s's value slot alone", conn, id, grant.Session, testMintedName)
	}
	values, version, err := m.AirlockValues(t.Context(), id)
	if err != nil || version == "" {
		t.Fatalf("AirlockValues(%s) = %v, %q, %v", id, values, version, err)
	}
	want := "minted:" + grant.ID + ":" + podThumbprint
	got := values[testMintedName]
	if len(values) != 1 || got.Slots[SlotValue] != want || !got.Expires.IsZero() || tokens.count() != 1 {
		t.Fatalf("the minted values = %+v after %d mints; want the token minted for the grant and the pod key, %q, with no end, minted once", values, tokens.count(), want)
	}
	if _, err := m.AirlockAccessPlaceholder(t.Context(), id, testMintedName, SlotAzureAccessPrefix+"scope"); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockAccessPlaceholder(%s) = %v; want ErrGrantNotLive", id, err)
	}
	if _, _, err := m.AirlockValues(t.Context(), testMintedWord+":"+uuid.NewString()); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockValues(a grant no row holds) = %v; want ErrGrantNotLive", err)
	}
}

// TestAMintedGrantOfAnotherEntryIsNotLive: a grant made under another entry or private
// destination than this broker's (its EntryDigest is not the kind's) opens no connection and has
// no values, and no token is minted for it; nor does any grant on a broker whose kind serves under
// no digest at all.
func TestAMintedGrantOfAnotherEntryIsNotLive(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	grants, tokens := &memoryGrants{}, &mintedTokens{}
	withMintedKind(m, grants, tokens)
	held, grant := grants.grant(t, otherEntryDigest, time.Now().Add(15*time.Minute))
	id := testMintedWord + ":" + grant.ID
	if conn, err := m.AirlockConnection(t.Context(), held); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection(a grant of another entry) = %+v, %v; want ErrGrantNotLive and no grant", conn, err)
	}
	if values, _, err := m.AirlockValues(t.Context(), id); !errors.Is(err, ErrGrantNotLive) || values != nil {
		t.Fatalf("AirlockValues(a grant of another entry) = %v, %v; want ErrGrantNotLive and no values", values, err)
	}
	ownHeld, own := grants.grant(t, testEntryDigest, time.Now().Add(15*time.Minute))
	m.Minted[0].EntryDigest = ""
	if conn, err := m.AirlockConnection(t.Context(), ownHeld); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection on a broker that serves the kind under no digest = %+v, %v; want ErrGrantNotLive", conn, err)
	}
	if _, _, err := m.AirlockValues(t.Context(), testMintedWord+":"+own.ID); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockValues on a broker that serves the kind under no digest = %v; want ErrGrantNotLive", err)
	}
	// A grant whose own digest is empty too: a broker with no digest for the kind serves none of
	// its grants, whatever the row says, so an empty digest never matches an empty one.
	blankHeld, blank := grants.grant(t, "", time.Now().Add(15*time.Minute))
	if conn, err := m.AirlockConnection(t.Context(), blankHeld); !errors.Is(err, ErrGrantNotLive) || conn.GrantID != "" {
		t.Fatalf("AirlockConnection of a grant of no digest on a broker of no digest = %+v, %v; want ErrGrantNotLive", conn, err)
	}
	if _, _, err := m.AirlockValues(t.Context(), testMintedWord+":"+blank.ID); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockValues of a grant of no digest on a broker of no digest = %v; want ErrGrantNotLive", err)
	}
	if n := tokens.count(); n != 0 {
		t.Fatalf("%d tokens were minted for grants this broker does not serve; want none", n)
	}
}
