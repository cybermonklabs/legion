package store

// The registry's part in a bucket deleted and created again (INFRA-4094), for both of its buckets:
// the interest rows and the role claims its copies held, written back into the new bucket from the
// read that finds it (beginInterestRead and reassertInterests, beginRoleRead and
// reassertRoleClaims); the claim whose own result the copy dropped because the bucket was recreated
// while it was in flight (claimAgain); and a release that finds the new bucket has not been given
// back the claim it releases (releaseUnreturned). kvwatch.Reassert says why an ack cannot name the
// incarnation that took it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// beginInterestRead starts a read of the interest bucket (kvwatch.Copy.BeginRead); every interest
// stays through it. For the first read of a recreated bucket it answers every row the copy holds,
// as its record, to write back into it (reassertInterests). None is left out. The bucket has no
// TTL, and a row ends only by a delete: an unsubscribe of every topic and the admin delete mark a
// key the new bucket does not hold (bus.KeyValue.DeleteAtRead), which refuses the write-back, and
// a gone session's row is the interest reaper's to delete after its grace, from whatever bucket
// holds it then.
func (r *Registry) beginInterestRead(read kvwatch.Read) []kvwatch.Held[Interest] {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.BeginRead(read)
	if !read.Recreated {
		return nil
	}
	var held []kvwatch.Held[Interest]
	for sessionID, item := range r.cache.All() {
		buf, err := json.Marshal(item)
		if err != nil {
			continue
		}
		held = append(held, kvwatch.Held[Interest]{Key: sessionID, Record: buf, Value: item})
	}
	return held
}

// reassertInterests writes held into the recreated interest bucket (kvwatch.Reassert), each only
// where that bucket holds nothing for its key, and stores each one it took in the copy at read's
// epoch, so the read's sweep keeps it. It runs in the read's BeginRead with r.mu released.
func (r *Registry) reassertInterests(read kvwatch.Read, held []kvwatch.Held[Interest]) {
	kvwatch.Reassert(r.watcher, read, held, func(h kvwatch.Held[Interest], revision uint64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cache.Put(r.watcher, read.Epoch, h.Key, h.Value, revision)
	})
}

// beginRoleRead starts a read of the role claim bucket (kvwatch.Copy.BeginRead); every claim stays
// through it, and whether the read repairs the copy, and so counts what it repairs, is the
// repair's to say (roleClaimRepair.begin). For the first read of a recreated bucket it answers
// every claim the copy holds, as its record, to write back into it (reassertRoleClaims). None is
// left out: a claim ends by a release, which marks a key the new bucket does not hold when the copy
// still holds the claim it frees (releaseUnreturned), or by the role claim reaper, whose judgement
// - a renewal lapsed past the live window - reads the claim's own record and so is the same in
// whatever bucket holds it. A claim handed back keeps its mark, so its renewal stays lapsed.
func (r *Registry) beginRoleRead(read kvwatch.Read) []kvwatch.Held[RoleClaim] {
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.BeginRead(read)
	r.roleRepair.begin(read)
	if !read.Recreated {
		return nil
	}
	var held []kvwatch.Held[RoleClaim]
	for role, claim := range r.roleHolders.All() {
		buf, err := json.Marshal(claim)
		if err != nil {
			continue
		}
		held = append(held, kvwatch.Held[RoleClaim]{Key: role, Record: buf, Value: claim})
	}
	return held
}

// reassertRoleClaims writes held into the recreated role bucket (kvwatch.Reassert), each only where
// that bucket holds nothing for its role, and stores each one it took in the copy at read's epoch,
// so the read's sweep keeps it. It runs in the read's BeginRead with holdersMu released.
func (r *Registry) reassertRoleClaims(read kvwatch.Read, held []kvwatch.Held[RoleClaim]) {
	kvwatch.Reassert(r.roleWatcher, read, held, func(h kvwatch.Held[RoleClaim], revision uint64) {
		r.holdersMu.Lock()
		defer r.holdersMu.Unlock()
		r.roleHolders.Put(r.roleWatcher, read.Epoch, h.Key, h.Value, revision)
	})
}

// ErrRoleClaimRecreated is a conditional claim - a soft claim, or one naming IfClaimRevision -
// whose write the role bucket acknowledged while the bucket was being recreated, so the copy
// dropped its result (kvwatch.Dropped). The condition was weighed against the bucket that was
// deleted, and the ack cannot say which bucket took the write, so the claim is answered as not
// made: POST /v1/roles/set answers it the bus-unavailable 503 with Retry-After: 1, and the
// claimant claims again against the bucket there is now.
var ErrRoleClaimRecreated = errors.New("the role bucket was recreated while the claim was in flight")

// claimAgain settles req's claim, encoded as data, after the copy dropped its write's result. A
// hard claim that names no precondition is last-claim-wins, and claims are ordered by the moment
// each was made (ClaimedAt), so it is written again into the bucket there is now
// (kvwatch.WriteAgain): over a claim made before it - the one the copy held, written back by the
// read that found the new bucket - and never over one made after it. A conditional claim is
// ErrRoleClaimRecreated.
func (r *Registry) claimAgain(ctx context.Context, req RoleClaimRequest, claim RoleClaim, data []byte) error {
	if req.Soft || req.IfClaimRevision != nil {
		return fmt.Errorf("claim role %s: %w", req.Role, ErrRoleClaimRecreated)
	}
	return kvwatch.WriteAgain(ctx, r.roleWatcher, req.Role, data,
		func(stored []byte) bool {
			standing, err := DecodeRoleClaim(stored)
			return err == nil && standing.ClaimedAt >= claim.ClaimedAt
		},
		func(epoch, revision uint64) kvwatch.Outcome {
			return r.setOwnRoleClaim(req.Role, claim, revision, epoch)
		},
		func(read kvwatch.Entry, epoch uint64) {
			r.holdersMu.Lock()
			defer r.holdersMu.Unlock()
			r.roleHolders.Apply(r.roleWatcher, epoch, read)
		})
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
	if !holds || !release.frees(held, sessionID) {
		return nil
	}
	roles := r.roles()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, roles.MarkDeletedIfNone(role) })
	if !answered || err != nil {
		return fmt.Errorf("mark role %s released in the recreated bucket: %w", role, err)
	}
	return nil
}
