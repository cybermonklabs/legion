// The record of failed renewals is pruned on every pod: by the grant guard's read of it on a pod
// that runs no role claim reaper (Registry.RenewalFailedWithin), and by the reaper's own pass with
// no read of the record before it (Registry.ReapRoleClaims).

package store

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// recordedRenewalFailures is every key the registry's record of failed renewals holds, whatever
// its age: what the record keeps in memory, which RenewalFailedWithin's answer cannot show, since
// that answer is judged by age alone.
func recordedRenewalFailures(reg *Registry) []renewalFailureKey {
	reg.renewalFailures.mu.Lock()
	defer reg.renewalFailures.mu.Unlock()
	keys := slices.Collect(maps.Keys(reg.renewalFailures.entries))
	slices.SortFunc(keys, func(a, b renewalFailureKey) int {
		return cmp.Or(strings.Compare(a.role, b.role), strings.Compare(a.sessionID, b.sessionID))
	})
	return keys
}

// A standby runs no role claim reaper - cmd/listener's holderLoops start it on the pod holding the
// machine's delivery lease alone - yet it records renewal failures and answers the grant guard
// like every pod. Its record is pruned when the guard asks (RenewalFailedWithin): a failure as old
// as the window it is asked about leaves the record, so a pod that never runs the reaper does not
// keep every failure it ever recorded. A younger failure stays.
func TestARenewalFailureRecordIsPrunedOnAPodThatRunsNoReaper(t *testing.T) {
	const (
		holder    = "ses_standby_failed_registration"
		other     = "ses_standby_other_registration"
		role      = "standby-role"
		otherRole = "standby-other-role"
		window    = contracts.SessionLiveWindow
	)
	cause := errors.New("the interest bucket did not answer the registration's read")
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	claimRoleAs(t, rig.reg, other, "m1", otherRole, roleClaimInstance)
	waitForHeld(t, rig.reg, holder, role)
	waitForHeld(t, rig.reg, other, otherRole)
	settle(rig)

	recorded := rig.clock.Now()
	rig.reg.RecordRenewalFailures(holder, cause)
	held := renewalFailureKey{role: role, sessionID: holder}
	if got := recordedRenewalFailures(rig.reg); !slices.Equal(got, []renewalFailureKey{held}) {
		t.Fatalf("the record holds %v after %s's failed registration, want only %v", got, holder, held)
	}

	// One window later the holder's failure answers no reader, and this pod has run no reaper:
	// the grant guard's read of another session's younger failure is the only prune there is.
	rig.clock.Set(recorded.Add(window))
	otherRecorded := rig.clock.Now()
	rig.reg.RecordRenewalFailures(other, cause)
	requireRenewalFailure(t, rig.reg, otherRole, other, otherRecorded, cause)
	young := renewalFailureKey{role: otherRole, sessionID: other}
	if got := recordedRenewalFailures(rig.reg); !slices.Equal(got, []renewalFailureKey{young}) {
		t.Fatalf("after the grant guard's read one window past %s's failure the record holds %v, want only %v: a pod that runs no reaper kept a failure no reader can be answered with",
			holder, got, young)
	}
}

// The role claim reaper's pass prunes the record by itself, with no read of it before the pass
// (RenewalFailedWithin prunes too, so a row that reads the record first cannot tell whether the
// pass pruned anything): a failure as old as the pass's window leaves the record.
func TestTheRoleClaimReaperPrunesTheRenewalFailureRecordWithNoReadBeforeIt(t *testing.T) {
	const (
		holder = "ses_reaped_failed_registration"
		role   = "reaped-role"
		window = contracts.SessionLiveWindow
	)
	cause := errors.New("the interest bucket did not answer the registration's read")
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	waitForHeld(t, rig.reg, holder, role)
	settle(rig)

	recorded := rig.clock.Now()
	rig.reg.RecordRenewalFailures(holder, cause)
	held := renewalFailureKey{role: role, sessionID: holder}
	if got := recordedRenewalFailures(rig.reg); !slices.Equal(got, []renewalFailureKey{held}) {
		t.Fatalf("the record holds %v after %s's failed registration, want only %v", got, holder, held)
	}
	rig.clock.Set(recorded.Add(window))
	if _, err := rig.reg.ReapRoleClaims(context.Background(), func(string) bool { return true }, 0, window); err != nil {
		t.Fatalf("the role claim reaper's pass: %v", err)
	}
	if got := recordedRenewalFailures(rig.reg); len(got) != 0 {
		t.Fatalf("after the reaper's pass one window past %s's failure the record holds %v, want none", holder, got)
	}
}
