package requests

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// resultEntryDigest stands for the results entry a broker grants under and serves (cmd/cipherd's
// resultEntryDigest): 64 hex digits, as result_grants holds every grant's. resultGrant writes each
// grant under it, and withResultGrants serves under it.
const resultEntryDigest = "3333333333333333333333333333333333333333333333333333333333333333"

// anotherEntryDigest is the results entry of a broker of another profile DISPATCH_RESULTS_TOKEN or
// another private destination.
const anotherEntryDigest = "4444444444444444444444444444444444444444444444444444444444444444"

// TestAResultGrantOfAnotherEntryIsNotLive: a broker pod serves a result grant only under the
// results entry the grant was made under. A grant of another entry is answered ErrGrantNotLive,
// saying it was made under another results profile or private destination, by the proxy's read
// of the connection and by its read of the values, and nothing is minted for it; so is every grant
// on a broker with no results entry. The grant is refused, not ended: a pod of its own entry
// serves it afterwards. The pod's own grant, in the same database, opens its connection and is
// minted its token.
func TestAResultGrantOfAnotherEntryIsNotLive(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, proxyrules.Rules{})
	tokens := &mintedTokens{}
	withResultGrants(m, tokens)
	ownHeld, own := resultGrant(t, m, "97", time.Now().Add(15*time.Minute))
	otherHeld, other := resultGrant(t, m, "98", time.Now().Add(15*time.Minute))
	if _, err := m.Store.Pool.Exec(t.Context(), `update result_grants set entry_digest = $1 where id = $2`, anotherEntryDigest, other.ID); err != nil {
		t.Fatal(err)
	}
	refused := func(what, held string, grant oauthstore.ResultGrant) {
		t.Helper()
		const why = "another results profile or private destination"
		conn, err := m.AirlockConnection(t.Context(), held)
		if !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), why) || conn.GrantID != "" {
			t.Errorf("AirlockConnection(%s) = %+v, %v; want ErrGrantNotLive saying %q, and no grant", what, conn, err, why)
		}
		values, version, err := m.AirlockValues(t.Context(), oauthwire.ResultGrantIDPrefix+grant.ID)
		if !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), why) || values != nil || version != "" {
			t.Errorf("AirlockValues(%s) = %v, %q, %v; want ErrGrantNotLive saying %q, and no values", what, values, version, err, why)
		}
	}
	refused("a grant of another entry", otherHeld, other)
	m.ResultEntryDigest = ""
	refused("its own entry's grant, on a broker with no results entry", ownHeld, own)
	m.ResultEntryDigest = resultEntryDigest
	if len(tokens.mints) != 0 {
		t.Fatalf("the refused reads minted %v; want no token", tokens.mints)
	}

	// Control: the pod's own grant opens its connection and is minted its token.
	if conn, err := m.AirlockConnection(t.Context(), ownHeld); err != nil || conn.GrantID != oauthwire.ResultGrantIDPrefix+own.ID {
		t.Fatalf("AirlockConnection(the pod's own entry's grant) = %+v, %v; want the grant %s", conn, err, own.ID)
	}
	if values, _, err := m.AirlockValues(t.Context(), oauthwire.ResultGrantIDPrefix+own.ID); err != nil || len(values) != 1 || len(tokens.mints) != 1 {
		t.Fatalf("AirlockValues(the pod's own entry's grant) = %v, %v after %d mints; want its token, minted once", values, err, len(tokens.mints))
	}
	// And the refused grant was not ended: a pod of its entry serves it.
	m.ResultEntryDigest = anotherEntryDigest
	if conn, err := m.AirlockConnection(t.Context(), otherHeld); err != nil || conn.GrantID != oauthwire.ResultGrantIDPrefix+other.ID {
		t.Fatalf("AirlockConnection(the refused grant, on a pod of its own entry) = %+v, %v; want the grant %s, still live", conn, err, other.ID)
	}
}
