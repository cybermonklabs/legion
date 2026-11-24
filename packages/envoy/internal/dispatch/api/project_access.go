package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
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
