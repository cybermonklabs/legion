// A role withheld from a session: the mark a Legion daemon leaves on a role whose claim does not
// hold its session, when the daemon could not stop the process that runs under it, so that no
// message on the role's topic reaches that process however it claims (RoleClaim.WithheldFrom). It
// is a concern of its own, split from roles.go the way role_release.go is: the withhold is two
// fields of the role's claim record, which every other write of the record carries over, and
// what is here is the daemon's two writes of it and the refusal a claim meets.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ErrRoleWithheld is a claim of a role by the session the role is withheld from
// (RoleClaim.WithheldFrom), soft or hard: nothing of the claim was written, and the role stands as
// its record says. POST /v1/roles/set answers it 409 role_withheld.
var ErrRoleWithheld = errors.New("the role is withheld from this session")

// ErrRoleWithheldByOther is a withhold or a lift of a role by a daemon other than the one whose
// withhold stands on it (RoleClaim.WithheldBy): a daemon writes over no other daemon's withhold
// and lifts none, so the withhold stands exactly as it was.
var ErrRoleWithheldByOther = errors.New("the role is withheld by another daemon")

// roleWithholdAttempts is how many conditional writes one withhold or one lift sends before it
// fails: the first, and two more over a record the role's own holder renewed under the one before,
// for the reason roleClaimReleaseAttempts gives a release the same bound.
const roleWithholdAttempts = roleClaimReleaseAttempts

// roleWithheldFrom is ErrRoleWithheld naming the role and the session it is withheld from, which
// is the session the refused claim named.
func roleWithheldFrom(role, sessionID string) error {
	return fmt.Errorf("%w: role %s is withheld from session %s", ErrRoleWithheld, role, sessionID)
}

// WithholdRole withholds role from sessionID on behalf of by, the daemon's own session
// (legion-daemon:<project>): it frees sessionID's standing claim of the role, if it holds one, and
// marks the record so that no claim by sessionID lands until by lifts it (LiftRoleWithhold). A
// role no record claims gets a record that names no holder and carries the withhold. Another
// session's claim of the role stands, and so does the withhold under it.
//
// The free and the mark are one conditional write of the record, so there is no moment between
// them in which the withheld session's next heartbeat could claim the freed role, which a release
// through RoleReleaseAuthority.Force followed by a second write would leave open. The role's lane
// topic is taken off sessionID's interest record afterwards, best-effort, as a claim's old
// holder's topic is (SetRoleWithPrevious): a topic left there gives nobody the role.
//
// A withhold by already wrote on the role is answered nil, its write moved to sessionID when it
// named another session; a withhold another daemon wrote is ErrRoleWithheldByOther and is left as
// it stands. Each bucket call waits no longer than ctx (bus.Bounded); a write that loses its
// condition to a write landed since its read reads the record again, up to roleWithholdAttempts
// writes in all.
func (r *Registry) WithholdRole(ctx context.Context, role, sessionID, by string) error {
	for attempt := 1; ; attempt++ {
		claim, err := r.RoleClaim(ctx, role)
		if err != nil {
			return fmt.Errorf("withhold role %s from %s: %w", role, sessionID, err)
		}
		if claim.WithheldBy != "" && claim.WithheldBy != by {
			return fmt.Errorf("%w: role %s is withheld by %s", ErrRoleWithheldByOther, role, claim.WithheldBy)
		}
		frees := claim.HolderSessionID == sessionID
		if !frees && claim.WithheldFrom == sessionID {
			return nil
		}
		next := claim
		if frees {
			next = RoleClaim{}
		}
		next.WithheldFrom, next.WithheldBy = sessionID, by
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		epoch := r.roleViewEpoch()
		revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
			if claim.Revision == 0 {
				return r.roles().Create(role, data)
			}
			return r.roles().Update(role, data, claim.Revision)
		})
		switch {
		case !answered:
			return fmt.Errorf("withhold role %s from %s: %w", role, sessionID, err)
		case err == nil:
			if next.HolderSessionID == "" {
				r.clearOwnRoleHolder(role, revision, epoch)
			} else {
				r.setOwnRoleHolder(role, next.HolderSessionID, revision, epoch)
			}
			if frees {
				r.removeWithheldTopic(ctx, role, sessionID)
			}
			return nil
		case errors.Is(err, nats.ErrKeyExists) && attempt < roleWithholdAttempts:
			continue
		default:
			return fmt.Errorf("withhold role %s from %s: %w", role, sessionID, err)
		}
	}
}

// removeWithheldTopic takes role's lane topic off the interest record of the session a withhold
// freed the role from. A failure is logged and changes nothing the withhold answers: the claim
// record, which every role route reads, no longer names that session.
func (r *Registry) removeWithheldTopic(ctx context.Context, role, sessionID string) {
	err := r.removeInterestTopics(ctx, sessionID, []string{contracts.RoleTopicPrefix + role})
	if err == nil || errors.Is(err, nats.ErrKeyNotFound) {
		return
	}
	r.logger().Warn("registry withhold left the role's topic on the withheld session's interest record",
		slog.String("role", role),
		slog.String("session_id", sessionID),
		slog.String("error", err.Error()))
}

// LiftRoleWithhold lifts the withhold of role from sessionID that by wrote (WithholdRole), so the
// session may claim the role again; the role then goes to whichever process claims it next. A
// claim of another session that stands on the record stands after it. A record that held the
// withhold alone is deleted, so the role is left as a role no record claims.
//
// A role not withheld from sessionID - no record, a record withholding it from another session,
// or one withholding it from nobody - is answered nil and nothing is written. A withhold of
// sessionID that another daemon wrote is ErrRoleWithheldByOther and stands. Each bucket call
// waits no longer than ctx (bus.Bounded); a write that loses its condition to a write landed since
// its read reads the record again, up to roleWithholdAttempts writes in all, and a record gone
// before the write is a withhold already lifted.
func (r *Registry) LiftRoleWithhold(ctx context.Context, role, sessionID, by string) error {
	for attempt := 1; ; attempt++ {
		claim, err := r.RoleClaim(ctx, role)
		if err != nil {
			return fmt.Errorf("lift role %s's withhold from %s: %w", role, sessionID, err)
		}
		if claim.WithheldFrom != sessionID {
			return nil
		}
		if claim.WithheldBy != by {
			return fmt.Errorf("%w: role %s is withheld by %s", ErrRoleWithheldByOther, role, claim.WithheldBy)
		}
		lifted := claim
		lifted.WithheldFrom, lifted.WithheldBy = "", ""
		epoch := r.roleViewEpoch()
		revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
			if lifted.HolderSessionID == "" {
				return 0, r.roles().Delete(role, nats.LastRevision(claim.Revision))
			}
			data, err := json.Marshal(lifted)
			if err != nil {
				return 0, err
			}
			return r.roles().Update(role, data, claim.Revision)
		})
		switch {
		case !answered:
			return fmt.Errorf("lift role %s's withhold from %s: %w", role, sessionID, err)
		case err == nil:
			if lifted.HolderSessionID == "" {
				r.forgetRoleHolder(role, claim.Revision, epoch)
			} else {
				r.setOwnRoleHolder(role, lifted.HolderSessionID, revision, epoch)
			}
			return nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return nil
		case errors.Is(err, nats.ErrKeyExists) && attempt < roleWithholdAttempts:
			continue
		default:
			return fmt.Errorf("lift role %s's withhold from %s: %w", role, sessionID, err)
		}
	}
}
