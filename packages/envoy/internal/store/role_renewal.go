// A role claim's renewal: the heartbeat that says its holder's process is still answering for
// the claim, written by the registration every client repeats while it runs (RenewRoleClaims,
// renewRoleClaim), the one test of it every decision that turns on a renewal reads
// (Registry.ClaimRenewedWithin), and this listener's record of the renewals that failed
// (RecordRenewalFailures, RenewalFailedWithin). It is a concern of its own, split from roles.go
// the way interest_writes.go and reaper.go are: nothing here takes a role or gives one away, and
// nothing here reads a session record - a claim's renewal is the holder's own word, and a role
// moves on the claim alone.
//
// The roles a registration renews are the role topics of the interest record its own write
// returned and the roles the role claim copy names for its session, and each is renewed only as
// its own claim record, read for that renewal, says. Neither the record nor the copy decides a
// renewal: a claim the copy lost is renewed from the record, and a claim the copy names that the
// store gives another session, or nobody, is left as the store holds it.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// RoleRenewal is what one registration's renewal of its role claims weighed and what it wrote
// (RenewRoleClaims), which the registration route logs at DEBUG and the tests read to tell where
// a renewal took a role from.
type RoleRenewal struct {
	// Set is every role whose claim record the renewal read, sorted and without repeats: the
	// role topics of the interest record it was given (rolesOfTopics) and the roles the role
	// claim copy names for the session (RolesHeldBy) while that copy can be served (weighedRoles).
	Set []string
	// Renewed is the roles of Set whose claim the renewal wrote, sorted. A role of Set it did
	// not renew is one whose claim record names another session, a process this registration is
	// not or a hand-back, one with no claim record at all, one whose write lost its
	// compare-and-swap, and one whose read or write failed, which the error RenewRoleClaims
	// returns beside it names.
	Renewed []string
}

// RenewRoleClaims stamps the moment clientInstanceID's process last answered on every claim
// sessionID holds that names that process (RoleClaim.RenewedAt), which is what makes a claim's
// renewal its holder's heartbeat rather than the instant the role was taken: the listener calls
// it from POST /v1/interests/subscribe, the registration every client repeats while it runs.
//
// The roles it weighs (RoleRenewal.Set) are the role topics of record and the roles the role
// claim copy names for sessionID (RolesHeldBy), sorted and without repeats, and each is renewed
// only as its own claim record, read here, says (renewRoleClaim). record is the interest row the
// registration's own write returned (UpsertPlanned, UpsertWithin), which that write read from
// the bucket and merged with the registration's plan, so this call makes no interest-record read
// of its own and its set does not wait on a cache that has not yet followed the bucket. The
// record is what renews a claim the copy lost, since a grant writes the role's lane topic into
// its holder's record beside the claim (SetRoleWithPrevious). The copy is what renews a claim the
// record does not name: a record this build could not decode, which the registration's write
// replaced with its own topics, or one an earlier build stored without the lane topic. While the
// copy cannot be served (ErrRoleViewNotReady) the set is the record's roles alone, and that is
// no error and no failure: every role there is still decided on its claim record.
//
// A registration that states no process (an empty clientInstanceID) renews the claims of its own
// session that name no process, and adopts none: an absent instance names no process, so it
// matches no claim that names one, and a claim naming none is adopted by the process that
// registers and never by the one that cannot say which it is. That arm is what keeps a claim
// standing at a deploy renewed by the client that holds it: such a claim names no process until
// a registration that states one adopts it, a client built before registrations carried an
// instance states none, and nothing else moves that claim's renewal. Without it that renewal
// would age out while the holder is still registering, and POST /v1/roles/set would hand the
// role to a second process claiming it under the same session id, since that route decides a
// claim held under the claimant's own session id on the renewal alone and reads no session
// record (roleOwnSessionVerdict, through Registry.ClaimRenewedWithin).
//
// A hand-back is read off the claim and not out of this registration. A release that hands one
// role back and may not free it marks the claim instead (RoleClaim.HandedBack, written by
// Registry.Remove's release path), and renewRoleClaim renews no marked claim, so that claim's
// renewal lapses and the role claim reaper frees the role at its first pass past the live
// window: one live window plus up to one reap interval, about ten minutes with the shipped
// defaults. The mark is what says the hand-back happened, so the record's topics say only which
// claims to read: a lane topic the hand-back left on the record renews nothing, a registration
// carrying no topic list is no case of its own, and a holder the copy names is renewed whatever
// topics it registers.
//
// Every failure is returned, not raised: a claim read or write that did not answer or failed, a
// record this build cannot read, a key the bus refuses. Each role is attempted whatever the roles
// before it did, and the errors are joined, one per role and each naming its role, so one
// unreadable claim does not cost the session its other renewals. Each failure is also recorded
// against its role and sessionID (RenewalFailedWithin), until a renewal of that role for that
// session writes. The caller is a heartbeat and must not fail over any of them (subscribeHandler
// logs and goes on).
//
// Each bucket call waits no longer than ctx (bus.Bounded): the route passes its request's own
// context. A write ctx abandoned may still land after this answer, which is safe - it is
// conditional on the revision it read and renews that claim alone.
func (r *Registry) RenewRoleClaims(ctx context.Context, sessionID, clientInstanceID string, record Interest) (RoleRenewal, error) {
	// A view that cannot be served leaves the record's roles alone and is no failure of the renewal.
	set, _ := r.weighedRoles(sessionID, record.Topics)
	renewal := RoleRenewal{Set: set}
	var failures []error
	for _, role := range renewal.Set {
		renewed, err := r.renewRoleClaim(ctx, role, sessionID, clientInstanceID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if renewed {
			renewal.Renewed = append(renewal.Renewed, role)
		}
	}
	return renewal, errors.Join(failures...)
}

// weighedRoles is the roles an act on sessionID's claims weighs: the role topics of topics, an
// interest record's (rolesOfTopics), and the roles the role claim view names for sessionID
// (RolesHeldBy), sorted and without repeats. It reads nothing from either bucket. Each source only
// names a claim to read, and the claim records decide every role of the set.
//
// A view that cannot be served (ErrRoleViewNotReady) names no roles, so the set is the record's
// roles alone, and the view's refusal is returned beside it. A renewal weighs that set and reads
// the refusal as no error (RenewRoleClaims, RecordRenewalFailures). A deregistration releases
// that set and then falls back to the scan of the bucket the refusal calls for
// (releaseAllRoleClaims), since a set without the view's roles is not every claim the session
// holds.
func (r *Registry) weighedRoles(sessionID string, topics []string) ([]string, error) {
	roles := rolesOfTopics(topics)
	held, err := r.RolesHeldBy(sessionID)
	if err != nil {
		return roles, err
	}
	roles = append(roles, held...)
	slices.Sort(roles)
	return slices.Compact(roles), nil
}

// rolesOfTopics is the roles whose lane topics topics names (roleOfTopic), sorted and without
// repeats: the roles an interest record says its session was given, since a grant writes the
// role's lane topic into its holder's record beside the claim (SetRoleWithPrevious). It is the
// one reading of a record's roles, so every act that takes roles from an interest record reads
// them the same way.
func rolesOfTopics(topics []string) []string {
	var roles []string
	for _, topic := range topics {
		if role, ok := roleOfTopic(topic); ok {
			roles = append(roles, role)
		}
	}
	slices.Sort(roles)
	return slices.Compact(roles)
}

// roleOfTopic is the role whose lane topic (contracts.RoleTopicPrefix) topic is, and false for a
// topic that is not a role lane's. A lane topic whose role is empty, or holds a subscription
// wildcard (* or >), names no one role and answers false too. It is the one reading of a topic's
// role, so a release of named topics (releaseRoleClaims) and a record's roles (rolesOfTopics)
// read a topic the same way.
func roleOfTopic(topic string) (string, bool) {
	role, ok := strings.CutPrefix(topic, contracts.RoleTopicPrefix)
	if !ok || role == "" || strings.ContainsAny(role, "*>") {
		return "", false
	}
	return role, true
}

// renewRoleClaim writes role's claim back with its renewal moved to now, conditional on the very
// revision this call read it at, and only while the stored claim still names sessionID as its
// holder and the process it names answers to clientInstanceID. It reports whether it wrote. Which
// roles it is asked about is RenewRoleClaims' set, from the registration's interest record and
// the role claim copy; whether one is renewed is this read of its record alone. A registration
// that states a process renews the claim naming that very process (RoleClaim.HeldByProcess), and
// adopts one naming none by writing its instance in: the first registration of the session
// holding a claim written before claims named a process, after which every renewal takes the
// first arm. A registration that states no process renews the claims of its session that name
// none, leaving them naming none, and matches no claim that names one - an absent instance names
// no process, so it can neither be a holder's heartbeat nor adopt what another process holds.
//
// A claim marked handed back (RoleClaim.HandedBack) is renewed by nobody, its own process
// included: the mark is the release's word that the holder gave the role up, so the claim ages
// out and the role claim reaper frees the role one live window and up to one reap interval
// later. A claim naming another session or another process is left exactly as it stands - the
// renewal is the holder's own heartbeat, and no other process's heartbeat may move it - and so
// is a role with no claim record at all, which the read answers as a zero claim and so a zero
// revision (RoleClaim).
//
// Only RenewedAt, and on adoption ClientInstanceID, are written: HolderSessionID, ClaimedAt and
// PreviousSessionID are the claim's own provenance and are carried over untouched, so a renewal
// never re-dates a claim or rewrites who took it from whom.
//
// A lost compare-and-swap (nats.ErrKeyExists for the wrong last sequence, nats.ErrKeyNotFound for
// a claim deleted in between) means another claim landed after this call read the record. The
// renewal is dropped and logged, never read again and retried: the record is now another
// claimant's, and a renewal retried into it would stamp this process's heartbeat on a claim it
// does not hold. The next registration renews the claim that stands, if it is still this
// session's. That is what this write says by allowing one attempt of writeRoleClaim, the loop
// the release's delete and the hand-back's mark send their writes through, where each of those
// reads a lost race as a record to weigh again.
//
// A read of the claim that did not answer or failed - ErrMalformedRoleClaim and a key the bus
// refuses included - and a write that did not answer or failed are this renewal's failures: each
// is returned naming role and recorded against role and sessionID (recordRenewalFailure), since
// such a claim may be sessionID's and is then not renewed. A lost compare-and-swap, a claim of
// another session or process, a handed-back claim and a role with no claim record are none: the
// record said what stands, and nothing of sessionID's went unrenewed. A write that landed clears
// the record of role for sessionID (clearRenewalFailure).
func (r *Registry) renewRoleClaim(ctx context.Context, role, sessionID, clientInstanceID string) (bool, error) {
	claim, err := r.RoleClaim(ctx, role)
	if err != nil {
		err = fmt.Errorf("renew role %s's claim held by %s: %w", role, sessionID, err)
		r.recordRenewalFailure(role, sessionID, err)
		return false, err
	}
	outcome, _, err := r.writeRoleClaim(ctx, claim, roleClaimWrite{
		role:     role,
		attempts: 1,
		writes: func(claim RoleClaim) bool {
			return claim.HolderSessionID == sessionID && !claim.HandedBack &&
				(claim.ClientInstanceID == "" || claim.HeldByProcess(clientInstanceID))
		},
		send: func(claim RoleClaim) (uint64, error) {
			renewed := claim
			renewed.ClientInstanceID = clientInstanceID
			renewed.RenewedAt = r.now().UnixMilli()
			data, err := json.Marshal(renewed)
			if err != nil {
				return 0, err
			}
			return r.roles().Update(role, data, claim.Revision)
		},
		landed: func(_ RoleClaim, revision, epoch uint64) { r.setOwnRoleHolder(role, sessionID, revision, epoch) },
	})
	switch {
	case err != nil:
		err = fmt.Errorf("renew role %s's claim held by %s: %w", role, sessionID, err)
		r.recordRenewalFailure(role, sessionID, err)
		return false, err
	case outcome == roleClaimWriteLanded:
		r.clearRenewalFailure(role, sessionID)
		return true, nil
	case outcome == roleClaimWriteRaced, outcome == roleClaimWriteMissing:
		r.logger().Info("registry dropped a role claim renewal a claim landed under",
			slog.String("role", role),
			slog.String("session_id", sessionID),
			slog.String("client_instance_id", clientInstanceID),
			slog.Uint64("read_revision", claim.Revision),
		)
	}
	return false, nil
}

// ClaimRenewedWithin reports whether claim's holder answered for it less than liveWindow ago
// (RoleClaim.LastAnsweredAt), as of this registry's clock. It is the one test of a claim's
// renewal, and every decision that turns on one reads it through this method - this registry's
// own role claim reaper (ReapRoleClaims) and POST /v1/roles/set weighing a claim held under the
// claimant's own session id - so the sweep and the route can never disagree over which claims a
// holder still renews, nor over the instant they weigh them at: there is one clock, this
// registry's, which a test moves (WithClock) and both readings then move with.
//
// A liveWindow of zero is a caller that configured no live window, and keeps nothing; so is a
// claim that carries neither a renewal nor a claim time, whose renewal is older than any window.
// A claim an earlier build wrote carries no renewal and is weighed at the moment it was claimed,
// which LastAnsweredAt says: it is renewed while that moment is inside the window, and stale
// once it is older.
func (r *Registry) ClaimRenewedWithin(claim RoleClaim, liveWindow time.Duration) bool {
	return liveWindow > 0 && r.now().Sub(time.UnixMilli(claim.LastAnsweredAt())) < liveWindow
}

// RenewalFailure is one renewal of a role's claim for one holder session that failed, as this
// registry recorded it (RenewalFailedWithin).
type RenewalFailure struct {
	// At is when the failure was recorded, on the registry's clock (WithClock).
	At time.Time
	// Err is the failure, naming the role and the session: the renewal's own read or write
	// (renewRoleClaim), or the registration's store work that failed before its renewal could
	// run (RecordRenewalFailures).
	Err error
}

// renewalFailureKey names the renewal a failure is recorded against: one role's claim, for one
// holder session.
type renewalFailureKey struct {
	role      string
	sessionID string
}

// renewalFailures is this registry's record of the renewals that failed, keyed by role and
// holder session: the one place that says a claim this listener should have renewed may have
// gone unrenewed, since the claim record itself cannot say a heartbeat was missed. It is in
// memory and this listener's own: another listener's failures are not in it, and a restart
// forgets them, as it forgets the registrations behind them. Its mutex is its own and not
// holdersMu, since nothing in it is the claim copy's, and every caller reads the copy before it
// takes this lock and never under it.
type renewalFailures struct {
	mu      sync.Mutex
	entries map[renewalFailureKey]RenewalFailure
	// window is the live window the role claim reaper's last pass pruned the record at
	// (ReapRoleClaims), which each write of the record prunes at too. It is 0 before any pass,
	// and a write then prunes nothing: the record cannot know the deployment's live window
	// before the reaper hands it one, and an entry pruned at a shorter window than the reader's
	// would answer a failure inside that window as none.
	window time.Duration
}

// record records err against key at now, in place of any earlier failure of that key, and prunes
// the entries as old as the reaper's last window or older.
func (f *renewalFailures) record(now time.Time, key renewalFailureKey, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[renewalFailureKey]RenewalFailure{}
	}
	f.entries[key] = RenewalFailure{At: now, Err: err}
	if f.window > 0 {
		f.pruneLocked(now, f.window)
	}
}

// clear removes key's failure.
func (f *renewalFailures) clear(key renewalFailureKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

// get answers key's failure, if one is recorded.
func (f *renewalFailures) get(key renewalFailureKey) (RenewalFailure, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	failure, ok := f.entries[key]
	return failure, ok
}

// prune removes every entry window old or older as of now and keeps window for the prune of each
// later write.
func (f *renewalFailures) prune(now time.Time, window time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.window = window
	f.pruneLocked(now, window)
}

// pruneLocked is prune's removal, under f.mu: an entry window old or older is outside the
// window RenewalFailedWithin reads it in, so no reader at that window can be answered with it.
func (f *renewalFailures) pruneLocked(now time.Time, window time.Duration) {
	maps.DeleteFunc(f.entries, func(_ renewalFailureKey, failure RenewalFailure) bool {
		return now.Sub(failure.At) >= window
	})
}

// recordRenewalFailure records err, the failure of a renewal of role's claim for sessionID, on the
// registry's clock (renewRoleClaim, RecordRenewalFailures).
func (r *Registry) recordRenewalFailure(role, sessionID string, err error) {
	r.renewalFailures.record(r.now(), renewalFailureKey{role: role, sessionID: sessionID}, err)
}

// clearRenewalFailure removes the failure recorded of role's claim for sessionID: a renewal of
// that claim for that session wrote (renewRoleClaim), so the claim carries this listener's
// heartbeat again.
func (r *Registry) clearRenewalFailure(role, sessionID string) {
	r.renewalFailures.clear(renewalFailureKey{role: role, sessionID: sessionID})
}

// pruneRenewalFailures removes every renewal failure recorded liveWindow ago or longer, on the
// registry's clock, and keeps liveWindow for the prune each later write of the record makes. The
// role claim reaper's pass calls it (ReapRoleClaims), the one caller handed the deployment's live
// window.
func (r *Registry) pruneRenewalFailures(liveWindow time.Duration) {
	r.renewalFailures.prune(r.now(), liveWindow)
}

// RecordRenewalFailures records cause as a renewal failure of every role sessionID may hold, for a
// registration whose store work failed before its renewal could run: the interest record's read
// or write, or the session entry's write. Such a registration renews nothing, so every claim it
// would have renewed goes unrenewed as surely as one whose own read failed. The roles are the ones
// the renewal would have weighed but for the record it never got (weighedRoles): the role claim
// copy's for sessionID while it can be served, and the role topics of the session's cached
// interest row (Cached), so a role the copy lost still gets its record when the registration's
// own read of the interest record is what failed. It reads nothing from either bucket, since the
// bus it would read is the one that just failed the registration.
func (r *Registry) RecordRenewalFailures(sessionID string, cause error) {
	var topics []string
	if row, ok := r.Cached(sessionID); ok {
		topics = row.Topics
	}
	roles, _ := r.weighedRoles(sessionID, topics)
	for _, role := range roles {
		r.recordRenewalFailure(role, sessionID, fmt.Errorf("renew role %s's claim held by %s: the registration failed before its renewal: %w", role, sessionID, cause))
	}
}

// RenewalFailedWithin answers the failure this registry recorded of a renewal of role's claim for
// sessionID, when it was recorded less than window ago on the registry's clock, and false when no
// failure was or the last one is older. A renewal of that claim for that session that wrote clears
// it, so a true answer is a claim this listener should have renewed and has not renewed since. A
// window of zero answers false, as ClaimRenewedWithin keeps nothing at a zero window.
func (r *Registry) RenewalFailedWithin(role, sessionID string, window time.Duration) (RenewalFailure, bool) {
	failure, ok := r.renewalFailures.get(renewalFailureKey{role: role, sessionID: sessionID})
	if !ok || r.now().Sub(failure.At) >= window {
		return RenewalFailure{}, false
	}
	return failure, true
}
