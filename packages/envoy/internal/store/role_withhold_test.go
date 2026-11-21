package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The sessions and the daemons these tests withhold roles between: a Legion daemon writes a
// withhold as its own session, legion-daemon:<project>.
const (
	withholdRole          = "legion-infra-infra-12-implementer"
	withheldSession       = "ses_withheld_claim"
	withholdOtherSession  = "ses_withheld_other"
	withholdingDaemon     = "legion-daemon:INFRA"
	withholdForeignDaemon = "legion-daemon:BET"
)

// withheldRegistry opens a registry over empty buckets in which withheldSession holds
// withholdRole through the process roleClaimInstance names, and then withholds that role from it.
func withheldRegistry(t *testing.T) *Registry {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	claimRoleAs(t, reg, withheldSession, "example-host", withholdRole, roleClaimInstance)
	assertRoleHolder(t, reg, withholdRole, withheldSession)
	if err := reg.WithholdRole(context.Background(), withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("withhold %s from %s: %v", withholdRole, withheldSession, err)
	}
	return reg
}

// claimAs is one claim of withholdRole by sessionID through the process instance names, soft or
// hard, as POST /v1/roles/set makes it.
func claimAs(reg *Registry, sessionID, instance string, soft bool) error {
	_, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
		SessionID: sessionID, MachineID: "example-host", ClientInstanceID: instance, Role: withholdRole, Soft: soft,
	})
	return err
}

// A role withheld from a session is a role that session cannot hold: the withhold frees the
// session's standing claim, so no holder is named, and refuses its soft claim and its hard one
// alike. Another session may still claim it, and the withheld session may again once the daemon
// that withheld it lifts the withhold.
//
// Control: free the claim without a withhold (the administrator's force, as DELETE
// /v1/interests/<session_id> does). The withheld session's soft claim then takes the freed role
// back, as its next heartbeat does, and the test fails naming the role.
func TestAWithheldRoleRefusesItsSessionAndHasNoHolder(t *testing.T) {
	reg := withheldRegistry(t)
	assertRoleHolder(t, reg, withholdRole, "")

	for _, soft := range []bool{true, false} {
		if err := claimAs(reg, withheldSession, roleClaimInstance, soft); !errors.Is(err, ErrRoleWithheld) {
			t.Fatalf("%s's claim (soft %t) of %s, which is withheld from it: %v, want ErrRoleWithheld", withheldSession, soft, withholdRole, err)
		}
		assertRoleHolder(t, reg, withholdRole, "")
	}

	if err := claimAs(reg, withholdOtherSession, roleClaimSecondInstance, true); err != nil {
		t.Fatalf("%s's soft claim of %s, withheld from another session only: %v", withholdOtherSession, withholdRole, err)
	}
	assertRoleHolder(t, reg, withholdRole, withholdOtherSession)
	if err := claimAs(reg, withheldSession, roleClaimInstance, false); !errors.Is(err, ErrRoleWithheld) {
		t.Fatalf("%s's hard claim of %s over %s's claim: %v, want ErrRoleWithheld: a claim by another session keeps the withhold", withheldSession, withholdRole, withholdOtherSession, err)
	}
	assertRoleHolder(t, reg, withholdRole, withholdOtherSession)

	if err := reg.LiftRoleWithhold(context.Background(), withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("lift %s's withhold from %s: %v", withholdRole, withheldSession, err)
	}
	if err := claimAs(reg, withheldSession, roleClaimInstance, false); err != nil {
		t.Fatalf("%s's claim of %s after the lift: %v", withheldSession, withholdRole, err)
	}
	assertRoleHolder(t, reg, withholdRole, withheldSession)
}

// A withhold is kept on the role's record through everything that writes it but a lift: a
// withhold of a role no record claims, the claim of another session, that session's renewal, its
// release, and the role claim reaper's release of it once it lapsed. Each of the two releases
// would delete the record of a claim that carries no withhold, which would let the withheld
// session take the role at its next heartbeat.
func TestAWithholdOutlivesEveryOtherWriteOfItsRole(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	ctx := context.Background()
	if err := reg.WithholdRole(ctx, withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("withhold %s, which no record claims, from %s: %v", withholdRole, withheldSession, err)
	}
	stillWithheld := func(after string) {
		t.Helper()
		claim, err := reg.RoleClaim(ctx, withholdRole)
		if err != nil {
			t.Fatalf("read %s's record after %s: %v", withholdRole, after, err)
		}
		if claim.WithheldFrom != withheldSession || claim.WithheldBy != withholdingDaemon {
			t.Fatalf("%s's record after %s withholds it from %q by %q, want from %s by %s", withholdRole, after, claim.WithheldFrom, claim.WithheldBy, withheldSession, withholdingDaemon)
		}
		if err := claimAs(reg, withheldSession, roleClaimInstance, true); !errors.Is(err, ErrRoleWithheld) {
			t.Fatalf("%s's soft claim of %s after %s: %v, want ErrRoleWithheld", withheldSession, withholdRole, after, err)
		}
	}
	stillWithheld("a withhold of a role no record claims")

	claimRoleAs(t, reg, withholdOtherSession, "example-host", withholdRole, roleClaimSecondInstance)
	stillWithheld("another session's claim")
	if renewed, err := reg.renewRoleClaim(ctx, withholdRole, withholdOtherSession, roleClaimSecondInstance); err != nil || !renewed {
		t.Fatalf("renew %s's claim of %s: renewed %t, %v", withholdOtherSession, withholdRole, renewed, err)
	}
	stillWithheld("that session's renewal")
	if _, err := reg.Remove(ctx, withholdOtherSession, nil, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}); err != nil {
		t.Fatalf("release %s's claim of %s: %v", withholdOtherSession, withholdRole, err)
	}
	assertRoleHolder(t, reg, withholdRole, "")
	stillWithheld("that session's release")

	claimRoleAs(t, reg, withholdOtherSession, "example-host", withholdRole, roleClaimSecondInstance)
	reg.now = func() time.Time { return time.Now().Add(time.Hour) }
	sweep, err := reg.ReapRoleClaims(ctx, func(string) bool { return false }, 0, time.Minute)
	reg.now = time.Now
	if err != nil || sweep.Reaped != 1 {
		t.Fatalf("reap %s's lapsed claim of %s: %+v, %v; want one claim released", withholdOtherSession, withholdRole, sweep, err)
	}
	assertRoleHolder(t, reg, withholdRole, "")
	stillWithheld("the reaper's release of a lapsed claim")
	if sweep, err := reg.ReapRoleClaims(ctx, func(string) bool { return false }, 0, time.Minute); err != nil || sweep.Reaped != 0 {
		t.Fatalf("reap a withhold no session holds: %+v, %v; want nothing released", sweep, err)
	}
	stillWithheld("a reap over the withhold alone")
}

// Only the daemon that withheld a role lifts the withhold, and no other daemon may write a
// withhold of its own over it. A lift of a role that is not withheld from the session it names
// changes nothing and is no error.
func TestOnlyTheDaemonThatWithheldARoleLiftsIt(t *testing.T) {
	reg := withheldRegistry(t)
	ctx := context.Background()
	if err := reg.LiftRoleWithhold(ctx, withholdRole, withheldSession, withholdForeignDaemon); !errors.Is(err, ErrRoleWithheldByOther) {
		t.Fatalf("%s lifts %s's withhold of %s: %v, want ErrRoleWithheldByOther", withholdForeignDaemon, withholdingDaemon, withholdRole, err)
	}
	if err := reg.WithholdRole(ctx, withholdRole, withholdOtherSession, withholdForeignDaemon); !errors.Is(err, ErrRoleWithheldByOther) {
		t.Fatalf("%s withholds %s over %s's withhold: %v, want ErrRoleWithheldByOther", withholdForeignDaemon, withholdRole, withholdingDaemon, err)
	}
	if err := reg.LiftRoleWithhold(ctx, withholdRole, withholdOtherSession, withholdingDaemon); err != nil {
		t.Fatalf("lift %s from %s, which it is not withheld from: %v, want nothing done", withholdRole, withholdOtherSession, err)
	}
	if err := reg.LiftRoleWithhold(ctx, "legion-infra-infra-13-tester", withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("lift a role no record claims: %v, want nothing done", err)
	}
	if err := claimAs(reg, withheldSession, roleClaimInstance, true); !errors.Is(err, ErrRoleWithheld) {
		t.Fatalf("%s's soft claim of %s after the refused lifts: %v, want ErrRoleWithheld", withheldSession, withholdRole, err)
	}
	if err := reg.WithholdRole(ctx, withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("the same daemon withholds %s from %s again: %v, want it to stand as it is", withholdRole, withheldSession, err)
	}
	if err := reg.LiftRoleWithhold(ctx, withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("lift %s from %s: %v", withholdRole, withheldSession, err)
	}
	if claim, err := reg.RoleClaim(ctx, withholdRole); err != nil || claim.Revision != 0 {
		t.Fatalf("%s's record after the lift of a withhold no session holds = %+v, %v; want no record", withholdRole, claim, err)
	}
}

// A record that carries a withhold and no holder is read, and names nobody the holder of its
// role: the holder view keeps no entry for it. A record that carries neither is still one this
// build cannot read.
func TestAWithholdWithNoHolderNamesNoHolder(t *testing.T) {
	claim, err := DecodeRoleClaim([]byte(`{"holder_session_id":"","claimed_at":0,"previous_session_id":"","withheld_from":"ses_withheld_claim","withheld_by":"legion-daemon:INFRA"}`))
	if err != nil || claim.HolderSessionID != "" || claim.WithheldFrom != withheldSession || claim.WithheldBy != withholdingDaemon {
		t.Fatalf("decode a withhold with no holder = %+v, %v", claim, err)
	}
	if _, err := DecodeRoleClaim([]byte(`{"holder_session_id":"","claimed_at":0,"previous_session_id":""}`)); !errors.Is(err, ErrMalformedRoleClaim) {
		t.Fatalf("decode a record with neither a holder nor a withhold: %v, want ErrMalformedRoleClaim", err)
	}

	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	claimRoleAs(t, reg, withheldSession, "example-host", withholdRole, roleClaimInstance)
	testwait.Eventually(t, "the holder view to name "+withheldSession, func() bool { return holderOf(reg, withholdRole) == withheldSession })
	if err := reg.WithholdRole(context.Background(), withholdRole, withheldSession, withholdingDaemon); err != nil {
		t.Fatalf("withhold %s from %s: %v", withholdRole, withheldSession, err)
	}
	if holder := holderOf(reg, withholdRole); holder != "" {
		t.Fatalf("the holder view names %q for %s once it is withheld, want nobody", holder, withholdRole)
	}
	reg.resetRoleHolders()
	entry, err := reg.roles().Get(withholdRole)
	if err != nil {
		t.Fatalf("read %s's record: %v", withholdRole, err)
	}
	reg.applyRoleClaim(entry)
	if holder, held := func() (string, bool) {
		reg.holdersMu.RLock()
		defer reg.holdersMu.RUnlock()
		return reg.roleHolders.Get(withholdRole)
	}(); held {
		t.Fatalf("the holder view delivered %s's withhold keeps the entry %q, want none", withholdRole, holder)
	}
}
