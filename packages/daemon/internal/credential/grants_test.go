package credential

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A grant is the credential for one call the `legion` tool or the operator CLI makes, which may
// reach the daemon more than once (`legion issue status` mints, then posts): it serves every redemption until it expires.
func TestGrantServesEveryRedemptionUntilItExpires(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	grants := New(func() time.Time { return now })
	issued, err := grants.Mint(supervise.Claim{
		Token:          "legion-legion-legion-208-implementer",
		Project:        "legion",
		Tree:           "LEGION-208",
		Issue:          "LEGION-208",
		Role:           claim.RoleImplementer,
		CapabilityHash: []byte("live-session-capability"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if issued.ExpiresAt != now.Add(60*time.Second) {
		t.Fatalf("ExpiresAt = %s, want %s", issued.ExpiresAt, now.Add(60*time.Second))
	}
	for _, at := range []time.Duration{0, 59 * time.Second} {
		now = issued.ExpiresAt.Add(-60 * time.Second).Add(at)
		redeemed, err := grants.Redeem(issued.ID)
		if err != nil {
			t.Fatalf("Redeem %s after mint: %v", at, err)
		}
		if redeemed.Claim != issued.Claim || redeemed.Role != claim.RoleImplementer || redeemed.Issue != "LEGION-208" {
			t.Fatalf("Redeem %s after mint = %+v, want the minted claim's grant", at, redeemed)
		}
	}

	now = issued.ExpiresAt
	for range 2 {
		if _, err := grants.Redeem(issued.ID); !errors.Is(err, ErrExpired) {
			t.Fatalf("Redeem at expiry = %v, want ErrExpired", err)
		}
	}
	if _, err := grants.Redeem("never-minted"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Redeem of an unknown id = %v, want ErrUnavailable", err)
	}

	// An expired record is kept for an hour, so a command that outran its grant hears it expired;
	// the first mint after that hour prunes it.
	now = issued.ExpiresAt.Add(time.Hour)
	if _, err := grants.MintController(0); err != nil {
		t.Fatalf("MintController: %v", err)
	}
	if _, err := grants.Redeem(issued.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("Redeem an hour after expiry = %v, want ErrExpired", err)
	}
	now = issued.ExpiresAt.Add(time.Hour + time.Second)
	if _, err := grants.MintController(0); err != nil {
		t.Fatalf("MintController: %v", err)
	}
	if _, err := grants.Redeem(issued.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Redeem of a pruned grant = %v, want ErrUnavailable", err)
	}
}

func TestGrantsRequireAnAuthenticatedClaim(t *testing.T) {
	grants := New(func() time.Time { return time.Unix(0, 0) })
	if _, err := grants.Mint(supervise.Claim{Role: claim.RoleImplementer}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Mint unauthenticated claim = %v, want ErrUnauthenticated", err)
	}
}

func TestGrantStillMatchesTheClaimThatMintedIt(t *testing.T) {
	grants := New(func() time.Time { return time.Unix(0, 0) })
	issued, err := grants.Mint(supervise.Claim{
		Token:          "legion-legion-legion-208-tester",
		Project:        "legion",
		Tree:           "LEGION-208",
		Issue:          "LEGION-208",
		Role:           claim.RoleTester,
		CapabilityHash: []byte("capability-at-mint"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !grants.StillMatches(issued, supervise.Claim{Token: issued.Claim, CapabilityHash: []byte("capability-at-mint")}) {
		t.Fatal("StillMatches rejected the unchanged authenticated claim")
	}
	if grants.StillMatches(issued, supervise.Claim{Token: issued.Claim, CapabilityHash: []byte("replacement-capability")}) {
		t.Fatal("StillMatches accepted a claim whose capability changed after the grant was minted")
	}
}

// A confirmed controller mint ends every controller grant of a lower generation and no other: the
// incumbent controller's in-flight command loses its credential once the mint that replaced it is
// confirmed, a newer mint's grants stand when an older mint's confirmation comes late, and the
// phase workers' commands run on.
func TestARevocationEndsOnlyGrantsOfLowerGenerations(t *testing.T) {
	grants := New(nil)
	mint := func(generation uint64) Grant {
		t.Helper()
		grant, err := grants.MintController(generation)
		if err != nil {
			t.Fatalf("MintController(%d): %v", generation, err)
		}
		return grant
	}
	redeems := func(grant Grant, want error) {
		t.Helper()
		if _, err := grants.Redeem(grant.ID); !errors.Is(err, want) {
			t.Fatalf("Redeem of the grant of generation %d = %v, want %v", grant.generation, err, want)
		}
	}
	first, second := mint(1), mint(2)
	worker, err := grants.Mint(supervise.Claim{
		Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, CapabilityHash: []byte("tester-capability"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	grants.RevokeControllersBelow(2)
	redeems(first, ErrUnavailable)
	redeems(second, nil)

	operator := mint(0)
	if operator.generation != 2 {
		t.Fatalf("the operator's grant is recorded under generation %d, want the confirmed 2", operator.generation)
	}

	grants.RevokeControllersBelow(1)
	redeems(second, nil)
	redeems(operator, nil)
	redeems(worker, nil)
}
