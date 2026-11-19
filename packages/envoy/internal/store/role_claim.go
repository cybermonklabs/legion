// A role's claim write: the request a claim states, the write with the revision it read and its
// retries, and the budget and rollback the write keeps (SetRoleWithPrevious).

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ErrRoleClaimContended is an outright claim (no IfClaimRevision, not soft) whose every try of its
// write met a conflict until its context ended: each try read the claim record and wrote with the
// revision it read, and each time another write of the record had landed between the two. Nothing
// was written, and the role stands as the record that is there says. The claim route answers it
// 503 with Retry-After: 1, and its client claims again.
var ErrRoleClaimContended = errors.New("the role's claim record changed under every try of this claim's write")

// RoleClaimRequest is one claim of a role, as SetRoleWithPrevious takes it: who is claiming,
// under which process, what it is claiming, and the preconditions it is claiming under. The
// fields are named because a claim states five strings in a row - the session, the machine, the
// process, the role and the predecessor - and any two of them swapped would still compile and
// would still write a claim, naming the wrong session or taking the role from the wrong one.
//
// The zero value of a field is a caller that states nothing there: no process, no predecessor,
// no precondition and nothing supersedable, which is the hard last-claim-wins claim SetRole
// makes.
type RoleClaimRequest struct {
	// SessionID is the session the claim makes the holder of Role, and MachineID the machine
	// its interest record is written for (UpsertWithin).
	SessionID string
	MachineID string
	// ClientInstanceID is the client instance of the claimant's own process, which the claim
	// records as the process that holds the role (RoleClaim.ClientInstanceID). A caller with
	// no instance to give states none, and the claim then names no process.
	ClientInstanceID string
	// Role is the role claimed, whose lane topic the claim writes onto the holder's interest
	// record (contracts.RoleTopicPrefix).
	Role string
	// PreviousSessionID is the session a soft claim continues, recorded as the claim's own
	// provenance and weighed nowhere here. It is written only on a soft claim.
	PreviousSessionID string
	// Soft is a claim that takes the role only from a holder listed in Supersedable; a hard
	// claim takes it from whoever holds it.
	Soft bool
	// IfClaimRevision, when it is not nil, is the revision the caller read the role's claim
	// at, and the claim lands only while the bucket still holds that very revision; 0 names a
	// role with no claim record at all.
	IfClaimRevision *uint64
	// Supersedable lists the holders a soft claim may take the role from, which the caller has
	// established may lose it. A soft claim that finds any other holder is refused.
	Supersedable []string
	// BeforeTry, when it is not nil, runs before each try of the claim's write, with the claim
	// record that try read and will write over, once the claim has been decided and before
	// anything of the try is written: a soft claim and a claim with IfClaimRevision make one
	// try, an outright claim one per read until a write lands (SetRoleWithPrevious). An error it
	// returns ends the claim with nothing written, the claimant's interest topic rolled back as a
	// claim that did not land rolls it back, and is returned as it is. The listener's claim
	// route publishes the takeover record of the holder that read names here, so a claim that
	// lands always has its record first.
	BeforeTry func(ctx context.Context, read RoleClaim) error
}

// SetRole makes sessionID the holder of role. A hard claim (soft=false) is
// last-claim-wins: it takes the role from whoever holds it. A soft claim
// succeeds only if the role is unheld or held by a session listed in
// supersedable; any other holder returns *ErrRoleHeld and leaves the claim
// untouched, and a holder that is sessionID itself is no exception. A session
// id names no process - a second process can resume a live session's id - so
// a claim whose holder is the claimant's own session id is weighed by the
// caller like any other holder's, and reaches here listed in supersedable or
// refused. Callers decide what "supersedable" means — the listener passes the
// ids it has established may lose the role: a holder that is not live; the
// claimant's own declared predecessor when that predecessor is not live
// either, when its record carries the claimant's own client instance id, or
// when either it or the claimant carries no client instance id; and the
// claimant's own session id when the standing claim names the claimant's own
// process, or when nobody has renewed that claim inside the live window
// (Registry.ClaimRenewedWithin). So a resumed session can recover a role its
// own earlier session held without ever taking one from a live peer, from a
// live session of another client instance, or from another process holding
// the role under the very session id it is claiming as its own.
//
// The claim it writes names no client instance (RoleClaim.ClientInstanceID): its callers hold
// none to give, so each leaves RoleClaimRequest.ClientInstanceID empty, and the claim says which
// session holds the role without saying which process does.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
		SessionID:    sessionID,
		MachineID:    machineID,
		Role:         role,
		Soft:         soft,
		Supersedable: supersedable,
	})
}

// SetRoleWithPrevious writes req's claim, and records the session ID that a successful soft claim
// continues (RoleClaimRequest.PreviousSessionID). The predecessor is provenance only: the
// listener has already evaluated the soft-claim rules against it, and a predecessor it decided
// may not lose the role reaches this call as provenance with no place in Supersedable, so this
// call answers *ErrRoleHeld naming the holder exactly as it does for any other live holder.
//
// A soft claim is refused the role whenever its holder is not listed in Supersedable, and a
// holder equal to req.SessionID is weighed like any other: the claimant's own session id buys it
// nothing here, because a session id names no process and a second process that resumed that id
// would otherwise take the role from the first by claiming under the id it shares. The one
// caller that can tell the two apart lists that holder itself (POST /v1/roles/set, which reads
// the standing claim's own process and renewal), so a claim arriving with its own session id as
// the holder and nothing in Supersedable is *ErrRoleHeld naming that holder.
//
// req.ClientInstanceID is the client instance of the claimant's own process, which the claim
// records as the process that holds the role (RoleClaim.ClientInstanceID), and the claim is
// stamped claimed and renewed at the same moment (ClaimedAt and RenewedAt, one reading of this
// registry's clock), so a claim is live from the instant it lands. A caller with no instance to
// give states none, and the claim then names no process.
//
// The record it writes is a fresh claim and never the standing one amended, so the hand-back
// mark goes with the claim it was written on (RoleClaim.HandedBack): a role granted again to
// the very process that handed it back is that process's to renew from this write on, which is
// how a holder takes back a role it gave up without waiting out the role claim reaper.
//
// Each bucket call waits no longer than ctx (bus.Bounded): POST /v1/roles/set passes its request's
// own context. A call ctx abandoned may still land after this answer. Every claim writes with the
// revision of the record it read (a create where it read none), so the record it writes over is
// the one it read. A hard claim that names no IfClaimRevision is not refused for a conflict: it
// reads the record again and writes again, until ctx ends, and the last write still wins, so two
// honest concurrent claimants both land, the later write holding the role; one that meets a
// conflict at every try until ctx ends writes nothing and is ErrRoleClaimContended. A soft
// claim's write is one try, so it lands only if no other claim did since; the loser of that race
// is answered *ErrRoleHeld for the holder it then reads, never a storage failure. A claim that
// names an IfClaimRevision is one try too, hard or soft, since that revision is the precondition
// described below. Either way the claim that lands makes req.SessionID the holder it asked to be;
// the interest write is UpsertWithin's. req.BeforeTry runs before each try, on the record that
// try read.
//
// The claim alone says who holds the role (RolesHeldBy, and role delivery), so the two cleanups
// that follow are best-effort tidying of interest topics, within ctx like the rest: the rollback of
// req.SessionID's topic when its claim did not land, and the removal of the role's topic from the
// old holder's record once it did. A topic either leaves behind gives nobody the role. A claim that
// landed is a success whatever its old holder's cleanup did: one that failed is logged at WARN, and
// one ctx ended before it finished at INFO, since nothing failed there.
//
// req.IfClaimRevision, when it is not nil, is the revision the caller read the role's claim at,
// and this call takes the role only while the bucket still holds that very revision: another one,
// a record where the caller expected none (IfClaimRevision 0), or none where it expected one is
// *ErrRoleClaimRevisionMismatch, and so is a claim that passed that check and then lost the
// write's own compare-and-swap to a claim that landed in between. It writes no claim record
// either way, so the role is left exactly as the claim that stands left it: the precondition is
// weighed before the claimant's interest record is touched, and a claim that lost the
// compare-and-swap rolls that record's topic back unless the winning claim names that same
// session, whose record then keeps the lane topic of the role it now holds. It is the
// precondition a scheduler needs that last-claim-wins and the soft rules cannot give it: a read of
// the role and the claim that follows are one decision, and a claim that landed between them
// refuses this one rather than being overwritten by it. A nil IfClaimRevision claims as before.
func (r *Registry) SetRoleWithPrevious(ctx context.Context, req RoleClaimRequest) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + req.Role
	// The claim's own work runs under tries, which ends a reserve before ctx does, so the rollback
	// of a claim that wrote nothing still has that reserve of ctx's budget (claimCleanupContext):
	// a claim whose tries ran to their end is answered inside ctx's deadline, rollback included.
	tries, cancelTries := claimTriesContext(ctx)
	defer cancelTries()
	oldClaim, err := r.RoleClaim(tries, req.Role)
	if err != nil {
		return Interest{}, err
	}
	// Before the soft rules and before any write: the caller's own precondition, weighed on the
	// read this call made itself, so the revision compared is the one its conditional write is
	// conditional on.
	if req.IfClaimRevision != nil && *req.IfClaimRevision != oldClaim.Revision {
		return Interest{}, &ErrRoleClaimRevisionMismatch{
			Role:     req.Role,
			Expected: *req.IfClaimRevision,
			Actual:   oldClaim.Revision,
			Holder:   oldClaim.HolderSessionID,
		}
	}

	oldSessionID := oldClaim.HolderSessionID
	if req.Soft && oldSessionID != "" && !slices.Contains(req.Supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: req.Role, Holder: oldSessionID}
	}

	item, err := r.UpsertWithin(tries, Interest{SessionID: req.SessionID, MachineID: req.MachineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}

	roles := r.roles()
	var (
		revision uint64
		epoch    uint64
	)
	for {
		if req.BeforeTry != nil {
			if hookErr := req.BeforeTry(tries, oldClaim); hookErr != nil {
				r.rollBackClaimTopic(ctx, req, roleTopic)
				return Interest{}, hookErr
			}
		}
		now := r.now().UnixMilli()
		claim := RoleClaim{
			HolderSessionID:   req.SessionID,
			ClaimedAt:         now,
			PreviousSessionID: "",
			ClientInstanceID:  req.ClientInstanceID,
			RenewedAt:         now,
		}
		if req.Soft {
			claim.PreviousSessionID = req.PreviousSessionID
		}
		data, err := json.Marshal(claim)
		if err != nil {
			return Interest{}, err
		}
		epoch = r.roleViewEpoch()
		// Every claim writes with the revision of the record it read: a create where it read
		// none, an update of that revision where it read one. So the record a claim writes over
		// is the record it read, and the holder it read - the one whose takeover record BeforeTry
		// published - is the holder it really replaced.
		//
		// A soft claim and a claim carrying IfClaimRevision make one try, because their refusal is
		// the point: a soft claim read a holder it was allowed to take the role from, and a claim
		// with a revision of its own required that very record, so a claim that landed since must
		// not be taken silently. Their losers are answered below.
		//
		// An outright claim has no refusal at all, so a conflict - another claim landed, or a
		// renewal or a release wrote the record, between this try's read and its write - is read
		// again and written again, each try with its own BeforeTry, until its tries end, and the
		// last write still wins. One conditional write lands per revision, so N claimants of one
		// role take N rounds, each a read and a write of a few milliseconds, well inside the claim
		// route's budget. A claim that meets a conflict at every try until its tries end writes
		// nothing and is ErrRoleClaimContended, which its client meets as a 503 to retry.
		var answered bool
		revision, answered, err = bus.Bounded(tries, func() (uint64, error) {
			if oldClaim.Revision == 0 {
				return roles.Create(req.Role, data)
			}
			return roles.Update(req.Role, data, oldClaim.Revision)
		})
		if !answered {
			err = fmt.Errorf("write role %s's claim: %w", req.Role, err)
		}
		if err == nil {
			break
		}
		if answered && !req.Soft && req.IfClaimRevision == nil && errors.Is(err, nats.ErrKeyExists) {
			var readErr error
			if tries.Err() == nil {
				var next RoleClaim
				if next, readErr = r.RoleClaim(tries, req.Role); readErr == nil {
					oldClaim = next
					continue
				}
			}
			if tries.Err() != nil {
				r.rollBackClaimTopic(ctx, req, roleTopic)
				return Interest{}, fmt.Errorf("%w: role %s: %w", ErrRoleClaimContended, req.Role, err)
			}
			err = errors.Join(err, readErr)
		}
		// The claim's tries can have ended in the write they abandoned, or before that write
		// started: the read that decides the rollback and the rollback itself still run, in the
		// reserve of ctx's budget (claimCleanupContext), as rollBackClaimTopic's do. Every branch
		// below returns, so the cleanup context is the loop's last.
		cleanup, cancelCleanup := claimCleanupContext(ctx)
		defer cancelCleanup()
		// The topic goes back only where the role did not end up with this session, which is
		// the session's and not the process's: one interest record serves every process
		// running under one session id, so a claim refused because the claim that won names
		// this session - the claimant's own process's, another process's, or a claim that
		// names none - leaves that record alone. It carries the lane topic of the role that
		// session now holds, the interest half of the claim that stands (rollBackUnlessHeld).
		current, holderErr, rollbackErr := r.rollBackUnlessHeld(cleanup, req, roleTopic)
		if rollbackErr != nil {
			return Interest{}, errors.Join(err, holderErr, rollbackErr)
		}
		holder := current.HolderSessionID
		// A conditional claim that lost the write's own compare-and-swap (nats.ErrKeyExists)
		// lost it to a claim that landed after the read this call weighed its precondition on:
		// the precondition failing a moment later, not a storage failure, and the caller is
		// told exactly that rather than reading an unexplained failure or another session's
		// name. A winner naming the claimant itself is no exception here, where it is one for
		// a write whose answer did not come just below: the revision the caller sequenced its
		// decision on is gone either way, and the claim that stands is not the one it made.
		//
		// The claim that stands is this claimant's own concurrent success only where it names
		// the claimant's session and the very process the claimant stated
		// (RoleClaim.HeldByProcess): a session id names no process, so a second process that
		// resumed this id could be the one holding that claim, and answering success over it
		// tells this claimant it holds a role another process holds - which is how two
		// processes of one session id came to answer for one role. A claimant that states no
		// process is told success by no claim at all: an absent instance names no process and
		// matches nothing, here as everywhere else a claim is weighed against a process, and a
		// claim naming none is the one record any process of that session id could have
		// written. Such a claimant is answered the failure its write met, and the role stays
		// with whatever claim stands.
		heldBySession := holderErr == nil && holder == req.SessionID
		heldByClaimant := heldBySession && current.HeldByProcess(req.ClientInstanceID)
		conditionalLoss := req.IfClaimRevision != nil && holderErr == nil && errors.Is(err, nats.ErrKeyExists)
		if heldByClaimant && !conditionalLoss {
			return item, nil
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		if conditionalLoss {
			return Interest{}, &ErrRoleClaimRevisionMismatch{
				Role:     req.Role,
				Expected: *req.IfClaimRevision,
				Actual:   current.Revision,
				Holder:   holder,
			}
		}
		// A soft claim that lost its conditional write (wrong last sequence, nats.ErrKeyExists)
		// lost to a concurrent claimant; report that as held rather than as a storage failure.
		if req.Soft && holder != "" {
			return Interest{}, &ErrRoleHeld{Role: req.Role, Holder: holder}
		}
		return Interest{}, err
	}
	oldSessionID = oldClaim.HolderSessionID
	r.setOwnRoleHolder(req.Role, req.SessionID, revision, epoch)
	// A claim of another process of this session that wrote nothing can have taken the topic back
	// off the record between this claim's merge of it and its write: its rollback decides on a read
	// of the claim made before that write landed (restoreClaimTopic). The topic is merged again,
	// in the reserve of ctx's budget, which writes nothing while the record carries it; a merge that
	// fails is logged and changes nothing the claim answers, since the claim record, which every
	// role route reads, stands.
	ensureCtx, cancelEnsure := claimCleanupContext(ctx)
	if ensured, ensureErr := r.UpsertWithin(ensureCtx, Interest{SessionID: req.SessionID, MachineID: req.MachineID}, []string{roleTopic}); ensureErr == nil {
		item = ensured
	} else {
		r.logger().Warn("registry role claim could not confirm its role topic on the claimant's interest record",
			slog.String("role", req.Role),
			slog.String("session_id", req.SessionID),
			slog.String("error", ensureErr.Error()))
	}
	cancelEnsure()
	if oldSessionID != "" && oldSessionID != req.SessionID {
		err := r.removeInterestTopics(ctx, oldSessionID, []string{roleTopic})
		switch {
		case err == nil, errors.Is(err, nats.ErrKeyNotFound):
		case errors.Is(err, bus.ErrRefused):
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The interest reaper removes
			// the old holder's interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", req.Role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		case ctx.Err() != nil:
			r.logger().Info("registry role claim left its old holder's topic: the request ended first",
				slog.String("role", req.Role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", req.SessionID),
			)
		default:
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", req.Role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", req.SessionID),
				slog.String("error", err.Error()),
			)
		}
	}
	return item, nil
}

// claimCleanupWindow is the reserve of a claim's budget its tries leave to the work after them -
// the rollback of a claim that wrote nothing, and the merge of the topic after a claim that landed
// (claimTriesContext, claimCleanupContext) - and the window that work runs in when the claim's
// context has no deadline.
const claimCleanupWindow = time.Second

// claimTriesContext is the context a claim's own reads, merge, hooks and writes run under: ctx
// ending claimCleanupWindow before ctx's deadline, or a third of the time left when that is less,
// so the work after the tries (claimCleanupContext) still has ctx's budget left. A ctx with no
// deadline is used as it is.
func claimTriesContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	reserve := min(claimCleanupWindow, time.Until(deadline)/3)
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

// claimCleanupContext is the context the work after a claim's tries runs under: ctx's own deadline,
// whether or not ctx was cancelled first, since a claim whose caller went away still owes its
// rollback, and claimCleanupWindow when ctx has no deadline. So a claim is answered inside ctx's
// deadline, rollback included, however its tries ended.
func claimCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return context.WithTimeout(detached, claimCleanupWindow)
}

// rollBackClaimTopic is rollBackUnlessHeld for a claim that wrote nothing because its BeforeTry
// refused a try or every try met a conflict until its tries ended. It runs in the reserve of ctx's
// budget (claimCleanupContext), since a claim whose tries ended still owes it, and a rollback that
// fails is logged at WARN and changes nothing the claim answers: a topic it leaves gives nobody the
// role.
func (r *Registry) rollBackClaimTopic(ctx context.Context, req RoleClaimRequest, roleTopic string) {
	cleanup, cancel := claimCleanupContext(ctx)
	defer cancel()
	if _, _, err := r.rollBackUnlessHeld(cleanup, req, roleTopic); err != nil {
		r.logger().Warn("registry role claim that wrote nothing left its claimant's role topic",
			slog.String("role", req.Role),
			slog.String("session_id", req.SessionID),
			slog.String("error", err.Error()))
	}
}

// rollBackUnlessHeld is the one rollback rule of a claim that wrote nothing: it reads the role's
// claim within cleanup and, unless that claim names the claimant's session - whose interest record
// then keeps the topic of the role it holds - takes the role's lane topic back off that record and
// merges it back if a claim of the session landed meanwhile (restoreClaimTopic). It answers the
// claim it read, the read's error, and the removal's.
func (r *Registry) rollBackUnlessHeld(cleanup context.Context, req RoleClaimRequest, roleTopic string) (RoleClaim, error, error) {
	current, holderErr := r.RoleClaim(cleanup, req.Role)
	if holderErr == nil && current.HolderSessionID == req.SessionID {
		return current, nil, nil
	}
	if err := r.removeInterestTopics(cleanup, req.SessionID, []string{roleTopic}); err != nil {
		return current, holderErr, err
	}
	r.restoreClaimTopic(cleanup, req, roleTopic)
	return current, holderErr, nil
}

// restoreClaimTopic merges the role's lane topic back into the claimant's interest record after a
// rollback removed it, when the claim record now names the claimant's session. One interest record
// serves every process of a session, and the rollback decided on a read of the claim made before
// its removal: another process of the same session can have claimed the role in between, and that
// claim's record must keep the topic of the role it holds. A claim of that process that lands after
// this read merges the topic itself once its write lands (SetRoleWithPrevious), so between the two
// the topic is back whichever lands last. A read or a merge that fails is logged at WARN.
func (r *Registry) restoreClaimTopic(ctx context.Context, req RoleClaimRequest, roleTopic string) {
	current, err := r.RoleClaim(ctx, req.Role)
	if err != nil || current.HolderSessionID != req.SessionID {
		return
	}
	if _, err := r.UpsertWithin(ctx, Interest{SessionID: req.SessionID, MachineID: req.MachineID}, []string{roleTopic}); err != nil {
		r.logger().Warn("registry role claim rollback could not restore the role topic another process of its session holds",
			slog.String("role", req.Role),
			slog.String("session_id", req.SessionID),
			slog.String("error", err.Error()))
	}
}
