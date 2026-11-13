// The roles a deregistration releases come from the session's interest record, read from the
// bucket for that deregistration, and from the role claim view together, and each is released
// only as its own claim record says (Registry.Remove, Registry.releaseAllRoleClaims). The view is
// a copy of the bucket and can lose a claim, so a release that took its set from the view alone
// would leave that claim standing for as long as its session goes on heartbeating; the record is
// what names it. The release under a view that cannot be served keeps the scan of the role bucket
// as its fallback, which is what finds a claim the record does not name.
//
// The cases about the release's direction of drift and about a claim read that fails are
// subtests of TestEachActReadsTheRecordInBothDirections and TestTheActHoldsOnAFailedRead
// (role_renewal_record_test.go), beside every other act's.

package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// bucketClaims is every claim the role bucket holds, as role=holder and sorted, read from one
// scan of it: what a case records of the claims before and after a deregistration. The scan goes
// through the registry's role handle, so a case that counts that handle's calls reads its counts
// before it calls this.
func bucketClaims(t *testing.T, reg *Registry) []string {
	t.Helper()
	scanned, err := reg.roleClaimsUntil(context.Background())
	if err != nil {
		t.Fatalf("list the role bucket's claims: %v", err)
	}
	claims := make([]string, 0, len(scanned))
	for _, claim := range scanned {
		claims = append(claims, claim.Role+"="+claim.Claim.HolderSessionID)
	}
	return claims
}

// assertNoInterestRecord fails the test unless the interest bucket itself holds no record for
// sessionID, which a deregistration deletes. The bucket is read and not the registry, whose
// interest cache a case may have stopped (stopInterestCache).
func assertNoInterestRecord(t *testing.T, rig *repairRig, sessionID string) {
	t.Helper()
	if _, err := rig.interests.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the interest bucket's read of %s's record answered %v, want ErrKeyNotFound: the deregistration deletes it", sessionID, err)
	}
}

// c-release-set. A session holds two claims and the copy has lost one of them; the session's
// interest record names both, since each grant wrote its role's lane topic there. The
// deregistration frees both, and it pays one claim read per role and no scan of the role bucket:
// the record names the claim the copy lost, so nothing needs the scan that answered the route 503
// (dispatch://INFRA-1383). A release whose set came from the copy alone would leave the lost claim
// standing, and one that scanned the bucket in place of the per-role reads would show the scan.
func TestAReleaseSetFreesAClaimTheCopyLostWithOneReadPerRole(t *testing.T) {
	const (
		holder = "ses_release_set"
		kept   = "release-set-kept"
		lost   = "release-set-lost"
	)
	roles := []string{kept, lost}
	rig := newRepairRig(t, nil)
	for _, role := range roles {
		claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	}
	settle(rig)
	loseRoleClaim(rig.reg, lost)
	if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{kept}) {
		t.Fatalf("with %s taken out of the copy, RolesHeldBy(%s) = %v, want [%s]", lost, holder, got, kept)
	}
	assertInterestTopicsForSession(t, rig.reg, holder, contracts.RoleTopicPrefix+kept, contracts.RoleTopicPrefix+lost)
	counting := countRoleBucketCalls(t, rig.reg, &sweepRoleKeyValue{})

	if _, err := rig.reg.Remove(context.Background(), holder, nil, releasedByItsProcess); err != nil {
		t.Fatalf("deregister %s: %v", holder, err)
	}
	gets, deletes := counting.totals()
	scans := counting.scans()
	t.Logf("the deregistration made %d Gets, %d Deletes and %d scans of the role bucket, releasing %d roles", gets, deletes, scans, len(roles))
	if scans != 0 {
		t.Fatalf("the deregistration made %d scans of the role bucket, want none: the record and the copy name every role it releases", scans)
	}
	for _, role := range roles {
		if roleGets, roleDeletes := counting.callsOn(role); roleGets != 1 || roleDeletes != 1 {
			t.Fatalf("role %s took %d Gets and %d Deletes, want one of each: its claim is read once and released once", role, roleGets, roleDeletes)
		}
	}
	if gets != len(roles) || deletes != len(roles) {
		t.Fatalf("the deregistration made %d Gets and %d Deletes, want %d of each, one per role released", gets, deletes, len(roles))
	}
	if left := bucketClaims(t, rig.reg); len(left) != 0 {
		t.Fatalf("the role bucket still holds %v after the deregistration, want neither of %s's claims", left, holder)
	}
	assertNoInterestRecord(t, rig, holder)
}

// c-release-under-a-refusing-view. A session holds two claims and its interest record names the
// lane topic of one of them; the session deregisters with no topics. While the role claim view
// cannot be served it names no roles at all, so the record's role is released from the record and
// the other only by the scan of the role bucket this change keeps for that state: the bucket holds
// neither afterwards. The same drive with a view that can be served frees both as well, from the
// record and the view and with no scan, so the first case cannot pass for a reason of its own.
func TestAReleaseUnderARefusingViewFreesEveryClaim(t *testing.T) {
	const (
		holder     = "ses_released_under_a_refusing_view"
		recorded   = "refusing-view-recorded"
		unrecorded = "refusing-view-unrecorded"
		ownTopic   = "notifications.team.refusing-view"
	)
	// deregister gives holder both claims on reg, takes the lane topic of unrecorded back off its
	// interest record, deregisters it with every role bucket call counted, and returns the scans
	// the deregistration made. It records the bucket's claims before and after.
	deregister := func(t *testing.T, reg *Registry) int {
		t.Helper()
		ctx := context.Background()
		if _, err := reg.Upsert(Interest{SessionID: holder, MachineID: "m1"}, []string{ownTopic}); err != nil {
			t.Fatalf("register %s's interests: %v", holder, err)
		}
		claimRoleAs(t, reg, holder, "m1", recorded, roleClaimInstance)
		claimRoleAs(t, reg, holder, "m1", unrecorded, roleClaimInstance)
		if err := reg.removeInterestTopics(ctx, holder, []string{contracts.RoleTopicPrefix + unrecorded}); err != nil {
			t.Fatalf("take %s's lane topic off %s's record: %v", unrecorded, holder, err)
		}
		assertInterestTopicsForSession(t, reg, holder, ownTopic, contracts.RoleTopicPrefix+recorded)
		before := bucketClaims(t, reg)
		if want := []string{recorded + "=" + holder, unrecorded + "=" + holder}; !slices.Equal(before, want) {
			t.Fatalf("the role bucket holds %v before the deregistration, want %v", before, want)
		}
		if reg.RoleViewErr() == nil {
			if held := heldRoles(t, reg, holder); !slices.Equal(held, []string{recorded, unrecorded}) {
				t.Fatalf("the served view names %s the holder of %v, want both its claims", holder, held)
			}
		}
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{})

		if _, err := reg.Remove(ctx, holder, nil, releasedByItsProcess); err != nil {
			t.Fatalf("deregister %s: %v", holder, err)
		}
		scans := counting.scans()
		after := bucketClaims(t, reg)
		t.Logf("the role bucket held %v before the deregistration and %v after it; the deregistration made %d scans of it", before, after, scans)
		if len(after) != 0 {
			t.Fatalf("the role bucket still holds %v after the deregistration, want neither of %s's claims", after, holder)
		}
		assertInterestMissing(t, reg, holder)
		return scans
	}

	t.Run("a view that cannot be served", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		t.Cleanup(cleanup)
		reg, _ := coldRegistry(t, conn)
		if err := reg.RoleViewErr(); !errors.Is(err, ErrRoleViewNotReady) {
			t.Fatalf("the registry's role claim view refuses with %v, want ErrRoleViewNotReady", err)
		}
		if scans := deregister(t, reg); scans != 1 {
			t.Fatalf("the deregistration made %d scans of the role bucket, want the one fallback a refusing view takes", scans)
		}
	})

	t.Run("a view that can be served", func(t *testing.T) {
		rig := newRepairRig(t, nil)
		if err := rig.reg.RoleViewErr(); err != nil {
			t.Fatalf("the registry's role claim view refuses with %v, want it served", err)
		}
		if scans := deregister(t, rig.reg); scans != 0 {
			t.Fatalf("the deregistration made %d scans of the role bucket, want none: the view names the claim the record does not", scans)
		}
	})
}
