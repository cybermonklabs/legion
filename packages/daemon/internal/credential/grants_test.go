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
	if _, err := grants.MintController("ses_controller"); err != nil {
		t.Fatalf("MintController: %v", err)
	}
	if _, err := grants.Redeem(issued.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("Redeem an hour after expiry = %v, want ErrExpired", err)
	}
	now = issued.ExpiresAt.Add(time.Hour + time.Second)
	if _, err := grants.MintOperatorController(); err != nil {
		t.Fatalf("MintOperatorController: %v", err)
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
	if _, err := grants.MintController(""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("MintController of no session = %v, want ErrUnauthenticated", err)
	}
}

// A controller grant says who minted it, which a project's pause records as who acted
// (dispatch://INFRA-3759): the controller session's grant names that session, the operator
// bearer's grant says it is the operator's, each redeems as minted, and a claim's grant is neither.
func TestAControllerGrantSaysWhoMintedIt(t *testing.T) {
	grants := New(nil)
	session, err := grants.MintController("ses_controller")
	if err != nil {
		t.Fatalf("MintController: %v", err)
	}
	operator, err := grants.MintOperatorController()
	if err != nil {
		t.Fatalf("MintOperatorController: %v", err)
	}
	worker, err := grants.Mint(supervise.Claim{
		Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, CapabilityHash: []byte("tester-capability"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, tc := range []struct {
		name                 string
		id                   string
		controller, operator bool
		session              string
	}{
		{name: "the controller session's", id: session.ID, controller: true, session: "ses_controller"},
		{name: "the operator bearer's", id: operator.ID, controller: true, operator: true},
		{name: "a claim's", id: worker.ID},
	} {
		redeemed, err := grants.Redeem(tc.id)
		if err != nil {
			t.Fatalf("Redeem %s grant: %v", tc.name, err)
		}
		if redeemed.Controller != tc.controller || redeemed.Operator != tc.operator || redeemed.Session != tc.session {
			t.Errorf("%s grant redeems as Controller %t, Operator %t, Session %q; want %t, %t, %q", tc.name,
				redeemed.Controller, redeemed.Operator, redeemed.Session, tc.controller, tc.operator, tc.session)
		}
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

// A new controller capability ends every controller grant the previous one minted, the operator
// bearer's as much as the controller session's, and no claim's grant: the incumbent controller's
// in-flight command loses its credential the moment it is replaced, while the phase workers'
// commands run on.
func TestRevokeControllersEndsOnlyControllerGrants(t *testing.T) {
	grants := New(nil)
	controller, err := grants.MintController("ses_controller")
	if err != nil {
		t.Fatalf("MintController: %v", err)
	}
	operator, err := grants.MintOperatorController()
	if err != nil {
		t.Fatalf("MintOperatorController: %v", err)
	}
	worker, err := grants.Mint(supervise.Claim{
		Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, CapabilityHash: []byte("tester-capability"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	grants.RevokeControllers()

	for name, id := range map[string]string{"the controller session's": controller.ID, "the operator bearer's": operator.ID} {
		if _, err := grants.Redeem(id); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Redeem of %s revoked controller grant = %v, want ErrUnavailable", name, err)
		}
	}
	if _, err := grants.Redeem(worker.ID); err != nil {
		t.Fatalf("Redeem of a claim grant after the controllers were revoked = %v, want it still served", err)
	}
}
