package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// getProjectAccess answers who may see project key and who changes that, as the caller reads it:
// a project the caller does not see answers as a key no project holds. The owner's browser session
// gets the owner's passkey fields too where passkey sign-in is on (withOwnersPasskeys). Every read
// of the answer is one snapshot, a read-only repeatable-read transaction: an enrollment uses its
// link and stores its passkey in one commit, so the answer lists the link open or the passkey,
// never neither, and the Access tab never drops a link with no passkey to show for it.
func (s *server) getProjectAccess(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The transaction reads only, so its end is a rollback whatever happened.
	defer tx.Rollback(r.Context())
	answer, err := access.ReadProject(r.Context(), tx, r.PathValue("key"), refs.VisibilityArg(who))
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	if answer, err = s.withOwnersPasskeys(r.Context(), tx, answer, caller, who.Viewer); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.accessAnswer(answer))
}

// inOwnerWrite runs one write of project {key}'s access for its owner: the caller acts as a person,
// and is the project's owner as projects.owner reads in the write's own transaction, under the
// locks access.LockForAccessWrite takes in the one order every access write takes them (removed,
// the person a removal or a Reset takes off the list or resets, and group, the group the write
// names, each "" for none), so an owner change committed meanwhile is read rather than raced and a
// delete of the group is ordered against the write. It publishes what the write appended once it
// commits, and reports whether it did; a refusal is answered here. The write is handed the
// caller's viewer, read from the request's own credential (readerOf), which a refusal it makes
// reads to name only what that credential sees (access.SetSlackChannels).
//
// The caller's viewer is read first: a project the caller does not see answers 404
// PROJECT_NOT_FOUND exactly as a key no project holds, before the owner is read. Only a caller who
// sees the project, and so may read its access list, is then told it is not the owner, by the 403
// PROJECT_OWNER_REQUIRED that names the owner; the other order would name a hidden project's owner
// to anyone who guessed its key. The owner is read first with no lock (access.ReadOwner), and a
// caller who does not own the project is refused before the write takes any: a removal's first
// lock may be the approval lock of the person it removes, which that person's approve does not
// wait for, so a removal anyone who sees the project may send and the owner check refuses would
// otherwise hold that person's approves at 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT. The read under
// the locks is the one that decides.
func (s *server) inOwnerWrite(w http.ResponseWriter, r *http.Request, group, removed string,
	write func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) ([]model.Event, error)) bool {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return false
	}
	key := r.PathValue("key")
	who, ok := s.readerOf(w, r)
	if !ok {
		return false
	}
	if !viewerSees(who.Viewer, key) {
		s.writeAccessError(w, fmt.Errorf("%w: %s", access.ErrProjectNotFound, key))
		return false
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	defer tx.Rollback(r.Context())
	owns := func(owner *string, err error) bool {
		if err != nil {
			s.writeAccessError(w, err)
			return false
		}
		if owner == nil || *owner != actor.ID {
			writeError(w, "PROJECT_OWNER_REQUIRED", http.StatusForbidden,
				"only the project's owner changes who may see it: ask its owner, "+ownerName(owner))
			return false
		}
		return true
	}
	if !owns(access.ReadOwner(r.Context(), tx, key)) || !owns(access.LockForAccessWrite(r.Context(), tx, key, group, removed)) {
		return false
	}
	appended, err := write(r.Context(), tx, key, actor, who.Viewer)
	if err != nil {
		s.writeAccessError(w, err)
		return false
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	s.publish(r.Context(), appended...)
	return true
}

// viewerSees reports whether viewer sees project; no viewer is the server's own read.
func viewerSees(viewer *model.Viewer, project string) bool {
	return viewer == nil || viewer.All || slices.Contains(viewer.Sees, project)
}

func ownerName(owner *string) string {
	if owner == nil {
		return "whom the operator names"
	}
	return *owner
}
