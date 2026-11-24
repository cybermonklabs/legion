package main

import (
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/store"
)

// staleRoleKV is a copy of the role bucket one write behind for one role: a direct get of role
// answers stale, the claim record that copy still holds, as a replica a direct get reaches can
// (bus.KeyValue.GetFromLeader says why), and every other call goes to the bucket. The leader's read
// of a key (bus.KeyValue.GetFromLeader) is a stream message get on the bucket's own connection, not
// a call on this handle, so it answers the newest record.
type staleRoleKV struct {
	natsgo.KeyValue
	role  string
	stale natsgo.KeyValueEntry
}

func (kv staleRoleKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.role {
		return kv.stale, nil
	}
	return kv.KeyValue.Get(key)
}

// holdRoleCopyBehind has every direct get of role's claim through registry answer the record the
// bucket holds now, from now on, whatever is written after it, and returns the func that puts the
// bucket's own handle back. Until a claim lands over that record the copy is current, so a claim's
// own read and its conditional write are made against the record that stands.
func holdRoleCopyBehind(t *testing.T, registry *store.Registry, role string) (restore func()) {
	t.Helper()
	base := registryRoleKV(t, registry)
	stale, err := base.Get(role)
	if err != nil {
		t.Fatalf("read %s's claim to hold the copy at: %v", role, err)
	}
	setRegistryRoleKV(t, registry, staleRoleKV{KeyValue: base, role: role, stale: stale})
	return func() { setRegistryRoleKV(t, registry, base) }
}

// requireCopyBehind fails the row as UNKNOWN unless a direct get of lostLaneRole's claim names
// former while the role claim view, which follows the bucket on a watch of its own, names next: the
// copy is one write behind the claim, which is the state the row is about.
func requireCopyBehind(t *testing.T, registry *store.Registry, former, next string) {
	t.Helper()
	waitFor(t, 10*time.Second, "the role claim view to name "+next, func() bool {
		held, err := registry.RolesHeldBy(next)
		return err == nil && slices.Contains(held, lostLaneRole)
	})
	if claim, err := registry.RoleClaim(t.Context(), lostLaneRole); err != nil || claim.HolderSessionID != former {
		t.Fatalf("UNKNOWN: a direct get of %s's claim answers %+v, %v, not the record naming %s, so no copy is behind and the row tests nothing", lostLaneRole, claim, err, former)
	}
}

// A takeover narrows the former holder's filter whatever copy of the role bucket a direct get
// reaches. The claim route decides the former holder's row and filter from the claim it has just
// written (store.Registry.ReadLaneHolders), and a copy of a replicated role bucket one write behind
// still names the former holder: a direct get there kept the lane on the former holder's row and
// consumer, and the new holder's notices went on reaching it. The claim is read from the role
// bucket stream's leader, which holds every write, so the lane goes. Control: the copy behind is
// real for the row: a direct get of the claim names the former holder after the claim.
func TestATakeoverNarrowsTheFormerHolderWhileTheRoleCopyIsBehind(t *testing.T) {
	rig := newLostLaneRig(t)
	const former, next = "ses_behind_takeover_former", "ses_behind_takeover_next"
	rig.subscribe(t, consumerRegistration(former, "epoch-1"))
	claimThroughRoute(t, rig, former, lostLaneRole)
	rig.subscribe(t, consumerRegistration(former, "epoch-1", lostLane))
	requireFilters(t, rig.sessionConsumerRig, former, lostLane)
	rig.subscribe(t, consumerRegistration(next, "epoch-1"))
	restore := holdRoleCopyBehind(t, rig.harness.registry, lostLaneRole)
	claimThroughRoute(t, rig, next, lostLaneRole)
	filters := rig.filters(t, former)
	row, _, rowErr := rig.harness.registry.RefreshAt(t.Context(), former)
	requireCopyBehind(t, rig.harness.registry, former, next)
	restore()
	if slices.Contains(filters, lostLane) {
		t.Errorf("once %s's claim of %s was answered, with the role bucket's copy one write behind, the former holder's consumer still filters on %v", next, lostLaneRole, filters)
	}
	if rowErr != nil || slices.Contains(row.Topics, lostLane) {
		t.Errorf("once the claim was answered, the former holder's interest row is %v (%v), want one without %s", row.Topics, rowErr, lostLane)
	}
}

// A publish on a controller lane counts by the role's claim as the role bucket stream's leader
// holds it. A copy of a replicated role bucket one write behind a takeover still names the former
// holder, whose known-session record and row name the lane: judged by a direct get there, the
// publish counted the former holder, recipients 1 for a message its consumer, filtered on the
// claim's newest record, does not deliver. Control: the copy behind is real for the row.
func TestALaneCountReadsTheClaimFromTheLeaderWhileTheRoleCopyIsBehind(t *testing.T) {
	rig := newLostLaneRig(t)
	const former, next = "ses_behind_count_former", "ses_behind_count_next"
	claimRoles(t, rig.harness.registry, former, lostLaneRole)
	rig.subscribe(t, consumerRegistration(former, "epoch-1", lostLane))
	restore := holdRoleCopyBehind(t, rig.harness.registry, lostLaneRole)
	t.Cleanup(restore)
	claimRoles(t, rig.harness.registry, next, lostLaneRole)
	requireCopyBehind(t, rig.harness.registry, former, next)
	if record, ok := rig.deps.known.Get(former); !ok || !slices.Contains(record.Topics, lostLane) {
		t.Fatalf("UNKNOWN: %s's known-session record is %+v (%t), not one naming %s, so the row tests nothing", former, record, ok, lostLane)
	}
	requireNoticeCounts(t, publishLaneNotice(t, rig), 0, 0)
}
