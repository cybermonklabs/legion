package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// mintedPublishKey is the listener's minted publish key, `publish.` and 32 lowercase hex digits,
// one of the shapes MINTED_DEDUPE_KEY_PATTERN in packages/contracts accepts, written out here
// rather than read from updateAskKeyShape, so the test does not check the code against itself.
var mintedPublishKey = regexp.MustCompile(`^publish\.[0-9a-f]{32}$`)

// roleForward is the mark the Envoy listener puts in front of the key of a message it forwards
// to a role's holder (contracts.RoleForwardDedupePrefix in the envoy module).
const roleForward = "envoy.role.forward."

// updateAskKeyFixture is packages/contracts/fixtures/daemon-api/update-ask-key.json: one claim,
// one round, one opening to the microsecond, and the key the daemon derives from them, which the
// contracts test holds the Envoy client's dedupe rule to.
type updateAskKeyFixture struct {
	ClaimToken claim.Token `json:"claimToken"`
	Round      string      `json:"round"`
	OpenedAt   time.Time   `json:"openedAt"`
	Key        string      `json:"key"`
}

func readUpdateAskKeyFixture(t *testing.T) updateAskKeyFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", "update-ask-key.json"))
	if err != nil {
		t.Fatalf("read the update ask key fixture: %v", err)
	}
	var fixture updateAskKeyFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the update ask key fixture: %v", err)
	}
	if fixture.ClaimToken == "" || fixture.Round == "" || fixture.OpenedAt.IsZero() || fixture.Key == "" {
		t.Fatalf("the update ask key fixture is %+v, want a claim, a round, an opening and a key", fixture)
	}
	return fixture
}

// The key the daemon derives for the fixture's claim, round and opening is the fixture's key, of
// the listener's minted publish shape, and IsUpdateAskKey knows it as the claim's own update ask
// bare, as the listener keeps it, and behind the role-forward mark, as a host may hand it on. The
// contracts test holds the Envoy client to keeping that same key on the card it hands the session
// (dispatch://INFRA-2040 D9), so a change to the derivation reds both sides.
func TestUpdateAskKeyIsTheContractFixturesKey(t *testing.T) {
	fixture := readUpdateAskKeyFixture(t)
	key := UpdateAskKey(fixture.ClaimToken, fixture.Round, &fixture.OpenedAt)
	if key != fixture.Key {
		t.Fatalf("UpdateAskKey(%s, %s, %s) = %s, want the fixture's %s", fixture.ClaimToken, fixture.Round,
			fixture.OpenedAt.Format(time.RFC3339Nano), key, fixture.Key)
	}
	if !mintedPublishKey.MatchString(key) {
		t.Fatalf("the update ask key %s is not of the listener's minted publish shape", key)
	}
	for _, reported := range []string{key, roleForward + key} {
		if !IsUpdateAskKey(fixture.ClaimToken, reported) {
			t.Errorf("IsUpdateAskKey(%s, %s) = false, want the claim's own update ask", fixture.ClaimToken, reported)
		}
	}
}

// One claim and one round opening give one key: the opening as the daemon that opened the round
// holds it, to the nanosecond, and as a daemon that restored the claim reads it back from the store,
// to the microsecond and in the store's own zone, are one key, so a report of the ask is known
// across a restart and a second row of one ask is its repeat. Another opening of the round, another
// round and another claim each give another key, and a claim that records no opening for its round
// still gets a key, of the same shape and of the same claim.
func TestUpdateAskKeyIsOneKeyForOneRoundOpening(t *testing.T) {
	fixture := readUpdateAskKeyFixture(t)
	token, round := fixture.ClaimToken, fixture.Round
	held := fixture.OpenedAt.Add(789 * time.Nanosecond)
	readBack := held.Truncate(time.Microsecond).In(time.FixedZone("store", -5*60*60))
	if held.Equal(readBack) || !held.Truncate(time.Microsecond).Equal(readBack) {
		t.Fatalf("the opening %s read back as %s is not the same opening cut to the microsecond", held.Format(time.RFC3339Nano), readBack.Format(time.RFC3339Nano))
	}
	key := UpdateAskKey(token, round, &held)
	if again := UpdateAskKey(token, round, &readBack); again != key {
		t.Fatalf("the opening to the nanosecond gives %s and the same opening read back gives %s, want one key", key, again)
	}
	if key != fixture.Key {
		t.Fatalf("the opening %s gives %s, want the fixture's %s: the nanoseconds are cut", held.Format(time.RFC3339Nano), key, fixture.Key)
	}

	later := fixture.OpenedAt.Add(time.Microsecond)
	other, err := claim.NewToken("legion", "LEGION-208", claim.RoleTester)
	if err != nil {
		t.Fatalf("mint the tester's token: %v", err)
	}
	for name, different := range map[string]string{
		"another opening of the round": UpdateAskKey(token, round, &later),
		"another round":                UpdateAskKey(token, "pod:51c2e8aa-0b6d-4c1e-8f2a-7d3e9b4c6a10", &held),
		"another claim":                UpdateAskKey(other, round, &held),
	} {
		if different == key {
			t.Errorf("%s gives %s, the key of the fixture's opening", name, different)
		}
	}

	unopened := UpdateAskKey(token, round, nil)
	if !mintedPublishKey.MatchString(unopened) || unopened == key || unopened != UpdateAskKey(token, round, nil) {
		t.Fatalf("the round with no opening gives %s, want one key of the minted publish shape that is not %s", unopened, key)
	}
	if !IsUpdateAskKey(token, unopened) {
		t.Fatalf("IsUpdateAskKey(%s, %s) = false, want the claim's own update ask with no opening", token, unopened)
	}
}

// IsUpdateAskKey reads the claim's tag alone, so every round and opening of the claim's ask is its
// own, and nothing else is: another claim's ask, a key the listener minted for another publish, an
// outbox row's key, no key, the ask behind two role-forward marks, and keys that are not of the
// minted shape although they begin with the claim's tag.
func TestIsUpdateAskKeyKnowsTheClaimsOwnAskAndNothingElse(t *testing.T) {
	fixture := readUpdateAskKeyFixture(t)
	token := fixture.ClaimToken
	later := fixture.OpenedAt.Add(time.Hour)
	for _, own := range []string{
		fixture.Key,
		UpdateAskKey(token, "pod:51c2e8aa-0b6d-4c1e-8f2a-7d3e9b4c6a10", &later),
		roleForward + UpdateAskKey(token, fixture.Round, nil),
	} {
		if !IsUpdateAskKey(token, own) {
			t.Errorf("IsUpdateAskKey(%s, %s) = false, want the claim's own update ask", token, own)
		}
	}

	other, err := claim.NewToken("legion", "LEGION-208", claim.RoleTester)
	if err != nil {
		t.Fatalf("mint the tester's token: %v", err)
	}
	for name, key := range map[string]string{
		"another claim's ask":           UpdateAskKey(other, fixture.Round, &fixture.OpenedAt),
		"another claim's forwarded ask": roleForward + UpdateAskKey(other, fixture.Round, &fixture.OpenedAt),
		"a key the listener minted":     "publish.0123456789abcdef0123456789abcdef",
		"an outbox row's key":           OutboxKey(41),
		"no key":                        "",
		"two role-forward marks":        roleForward + roleForward + fixture.Key,
		"upper-case hex":                fixture.Key[:len("publish.")+16] + strings.ToUpper(fixture.Key[len("publish.")+16:]),
		"a short key":                   fixture.Key[:len(fixture.Key)-1],
		"a long key":                    fixture.Key + "0",
		"the tag alone":                 fixture.Key[:len("publish.")+16],
		"another prefix":                "agent.ses_a." + fixture.Key[len("publish."):],
	} {
		if IsUpdateAskKey(token, key) {
			t.Errorf("IsUpdateAskKey(%s, %q) = true for %s, want false", token, key, name)
		}
	}
}
