package store

// The registry's part in a bucket deleted and created again (INFRA-4094), for both of its buckets:
// the interest rows and the role claims it owns - its own listener's sessions' - written back
// into the new bucket from the read that finds it (beginInterestRead and reassertInterests,
// beginRoleRead and reassertRoleClaims); the claim whose own result the copy dropped because the
// bucket was recreated while it was in flight, settled in the bucket there is now (claimAgain); and
// a release that finds the new bucket has not been given back the claim it releases
// (releaseUnreturned). kvwatch.Reassert says why an ack cannot name the incarnation that took it.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// beginInterestRead starts a read of the interest bucket (kvwatch.Copy.BeginRead); every interest
// stays through it. It answers the rows the read writes back (kvwatch.Reasserter.Due): for the
// first read of a recreated bucket, the rows this registry owns - registered through its own
// listener, which every row names (Interest.MachineID) - and for a later read of the same epoch,
// the rows that write-back could not write. A row another listener registered is that listener's
// to write back. None of its own is left out: the bucket has no TTL, and a row ends only by a
// delete - an unsubscribe of every topic and the admin delete mark a key the new bucket does not
// hold (bus.KeyValue.DeleteAtRead), which the write-back leaves standing - and a gone session's row
// is the interest reaper's to delete after its grace, from whatever bucket holds it then.
func (r *Registry) beginInterestRead(read kvwatch.Read) []kvwatch.Held[kvwatch.Kept[Interest]] {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.BeginRead(read)
	return r.interestReassertion.Due(read, func() []kvwatch.Held[kvwatch.Kept[Interest]] {
		if r.owner == "" {
			return nil
		}
		var held []kvwatch.Held[kvwatch.Kept[Interest]]
		for sessionID, kept := range r.cache.All() {
			if kept.Value.MachineID == r.owner && len(kept.Record) > 0 {
				held = append(held, kvwatch.HeldFrom(sessionID, kept, read.MaxAge))
			}
		}
		return held
	})
}

// reassertInterests writes held into the recreated interest bucket (kvwatch.Reasserter.Write):
// where the bucket holds nothing for the key, and over a row written before the held one
// (UpdatedAt), never over a delete or a later write. It stores each row the bucket took in the copy
// at read's epoch, so the read's sweep keeps it. It runs in the read's BeginRead with r.mu released.
func (r *Registry) reassertInterests(read kvwatch.Read, held []kvwatch.Held[kvwatch.Kept[Interest]]) {
	r.interestReassertion.Write(r.watcher, read, held,
		func(stored []byte, h kvwatch.Held[kvwatch.Kept[Interest]]) bool {
			var row Interest
			return json.Unmarshal(stored, &row) == nil && row.UpdatedAt >= h.Value.Value.UpdatedAt
		},
		func(h kvwatch.Held[kvwatch.Kept[Interest]], revision uint64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.cache.Put(r.watcher, read.Epoch, h.Key, h.Value, revision)
		})
}

// ownSessionsLocked is the sessions whose interest rows the copy holds naming this registry's
// listener (Interest.MachineID): the sessions registered through it. Callers hold r.mu.
func (r *Registry) ownSessionsLocked() map[string]bool {
	own := map[string]bool{}
	if r.owner == "" {
		return own
	}
	for sessionID, kept := range r.cache.All() {
		if kept.Value.MachineID == r.owner {
			own[sessionID] = true
		}
	}
	return own
}

// beginRoleRead starts a read of the role claim bucket (kvwatch.Copy.BeginRead); every claim stays
// through it, and whether the read repairs the copy, and so counts what it repairs, is the
// repair's to say (roleClaimRepair.begin). It answers the claims the read writes back
// (kvwatch.Reasserter.Due): for the first read of a recreated bucket, the claims this registry owns,
// which are the claims of its own sessions - a claim names its holder session and no listener, so
// it is owned by the listener whose interest row of that session names it (ownSessionsLocked) - and
// for a later read of the same epoch, the claims that write-back could not write. A claim of a
// session registered elsewhere is that listener's, and one whose holder has no interest row here is
// no listener's: its holder's client claims the role again when it reads the role unclaimed. None
// of its own is left out: a claim ends by a release, which marks a key the new bucket does not hold
// when the copy still holds the claim it frees (releaseUnreturned), or by the role claim reaper,
// whose judgement - a renewal lapsed past the live window - reads the claim's own record and so is
// the same in whatever bucket holds it. A claim handed back keeps its mark, so its renewal stays
// lapsed.
func (r *Registry) beginRoleRead(read kvwatch.Read) []kvwatch.Held[kvwatch.Kept[RoleClaim]] {
	// The interest copy is read before holdersMu is taken, as every caller reading both copies
	// does (renewalFailures says so too): no lock of the interest copy is ever taken under it.
	var own map[string]bool
	if read.Recreated {
		r.mu.RLock()
		own = r.ownSessionsLocked()
		r.mu.RUnlock()
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.BeginRead(read)
	r.roleRepair.begin(read)
	return r.roleReassertion.Due(read, func() []kvwatch.Held[kvwatch.Kept[RoleClaim]] {
		var held []kvwatch.Held[kvwatch.Kept[RoleClaim]]
		for role, kept := range r.roleHolders.All() {
			if own[kept.Value.HolderSessionID] && len(kept.Record) > 0 {
				held = append(held, kvwatch.HeldFrom(role, kept, read.MaxAge))
			}
		}
		return held
	})
}

// reassertRoleClaims writes held into the recreated role bucket (kvwatch.Reasserter.Write): where
// the bucket holds nothing for the role, and over a claim the held one is later than
// (claimedLater), never over a release or a later claim. It stores each claim the bucket took in
// the copy at read's epoch, so the read's sweep keeps it. It runs in the read's BeginRead with
// holdersMu released.
func (r *Registry) reassertRoleClaims(read kvwatch.Read, held []kvwatch.Held[kvwatch.Kept[RoleClaim]]) {
	r.roleReassertion.Write(r.roleWatcher, read, held,
		func(stored []byte, h kvwatch.Held[kvwatch.Kept[RoleClaim]]) bool {
			standing, err := DecodeRoleClaim(stored)
			return err == nil && !claimedLater(h.Value.Value, standing)
		},
		func(h kvwatch.Held[kvwatch.Kept[RoleClaim]], revision uint64) {
			r.holdersMu.Lock()
			defer r.holdersMu.Unlock()
			r.roleHolders.Put(r.roleWatcher, read.Epoch, h.Key, h.Value, revision)
		})
}

// claimedLater reports whether claim is later than standing, by the moment each was claimed
// (ClaimedAt) and, for the same claim, the moment its holder last answered for it
// (LastAnsweredAt): the order a hard claim's last-claim-wins and a renewal follow.
func claimedLater(claim, standing RoleClaim) bool {
	if claim.ClaimedAt != standing.ClaimedAt {
		return claim.ClaimedAt > standing.ClaimedAt
	}
	return claim.LastAnsweredAt() > standing.LastAnsweredAt()
}

// sameClaim reports whether a and b are one grant: the same holder session and process, claimed at
// the same moment. A renewal and a hand-back mark keep a grant the same claim.
func sameClaim(a, b RoleClaim) bool {
	return a.HolderSessionID == b.HolderSessionID && a.ClientInstanceID == b.ClientInstanceID && a.ClaimedAt == b.ClaimedAt
}

// claimAgain settles claim, req's claim encoded as data, in the bucket there is now, after the copy
// dropped its write's result: the bucket was recreated while the claim was in flight, and the ack
// cannot say which bucket took it. It reads the role's claim there and:
//
//   - a claim that is this very grant stands: the write, or the write-back of the copy's record of
//     it, landed;
//   - an empty key is written create-only: no claim has landed since the read this claim weighed,
//     and the claim it read - for a conditional claim, the one it was conditioned on - is gone
//     with the deleted bucket or not written back yet, and its owner's write-back leaves this
//     later claim standing (claimedLater);
//   - a hard claim that names no precondition is last-claim-wins: it is written over a claim
//     claimed before it, and a claim claimed after it stands, as such a claim would have
//     superseded it in the deleted bucket;
//   - a conditional claim - a soft claim, or one naming IfClaimRevision - is written over the very
//     claim it was conditioned on (sameClaim with oldClaim), whose revision in the new bucket stands
//     for the one the condition named; any other claim standing there landed since the read the
//     condition was weighed on, and the claim is answered as one that lost its compare-and-swap
//     (nats.ErrKeyExists), which SetRoleWithPrevious answers as it answers that loss.
//
// A write the copy drops again was overtaken by another recreate and is settled again, up to
// kvwatch.WriteAgainAttempts times. Each bucket call waits no longer than ctx.
func (r *Registry) claimAgain(ctx context.Context, req RoleClaimRequest, claim, oldClaim RoleClaim, data []byte) error {
	conditional := req.Soft || req.IfClaimRevision != nil
	for range kvwatch.WriteAgainAttempts {
		kv, epoch := r.roleWatcher.Handle()
		current, err := r.RoleClaim(ctx, req.Role)
		if err != nil {
			return fmt.Errorf("read role %s's claim in the recreated bucket: %w", req.Role, err)
		}
		switch {
		case current.Revision != 0 && sameClaim(current, claim):
			if r.setOwnRoleClaim(req.Role, current, data, current.Revision, epoch) != kvwatch.Dropped {
				return nil
			}
			continue
		case current.Revision == 0:
		case !conditional && claimedLater(current, claim):
			return nil
		case conditional && !sameClaim(current, oldClaim):
			return fmt.Errorf("claim role %s in the recreated bucket: another claim stands: %w", req.Role, nats.ErrKeyExists)
		}
		revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.WriteBack(ctx, req.Role, data, current.Revision, 0) })
		switch {
		case !answered:
			return fmt.Errorf("write role %s's claim to the recreated bucket: %w", req.Role, err)
		case err != nil && bus.IsCASConflict(err):
			continue
		case err != nil:
			return fmt.Errorf("write role %s's claim to the recreated bucket: %w", req.Role, err)
		}
		if r.setOwnRoleClaim(req.Role, claim, data, revision, epoch) != kvwatch.Dropped {
			return nil
		}
	}
	return fmt.Errorf("claim role %s: %w (%d sends)", req.Role, kvwatch.ErrRecreatedUnderEachWrite, kvwatch.WriteAgainAttempts)
}

// releaseUnreturned marks role's key deleted while the bucket holds nothing for it
// (bus.KeyValue.MarkDeletedIfNone), when the release found no claim record and this registry's copy
// still holds a claim the release frees: the bucket was recreated, and the read that finds it -
// this listener's or another's - would write that claim back. The marker refuses that write-back,
// so the release stands. A copy that holds no such claim marks nothing.
func (r *Registry) releaseUnreturned(ctx context.Context, sessionID, role string, release RoleReleaseAuthority) error {
	if r.roleWatcher == nil {
		return nil
	}
	r.holdersMu.RLock()
	held, holds := r.roleHolders.Get(role)
	r.holdersMu.RUnlock()
	if !holds || !release.frees(held.Value, sessionID) {
		return nil
	}
	roles := r.roles()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, roles.MarkDeletedIfNone(role) })
	if !answered || err != nil {
		return fmt.Errorf("mark role %s released in the recreated bucket: %w", role, err)
	}
	return nil
}
