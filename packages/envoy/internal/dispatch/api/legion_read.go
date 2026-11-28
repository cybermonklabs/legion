// legion_read.go serves GET /api/v1/projects/{key}/legion/read, the newest read of one project's
// Legion daemon as the board's header gives it (dispatch://INFRA-2918, D23), and the body the
// `legion-read` live view kind sends (live_frames.go's legionReadView). The Evals tab's header
// draws it beside the sources' freshness, so a page that shows when the daemon was last read
// holds three fields rather than the whole board.
//
// The read is the board's header's own: it is taken through readDaemonRead, the one statement the
// header and an issue's Legion read take it through, so the three answer one read. That statement
// gives the newest whole read's ok and error while the project's cursor owes a whole read, so a
// batch of state events applied after a failed whole read does not show a good read.
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// legionReadResponse is GET /api/v1/projects/{key}/legion/read: the board's header's read_at,
// read_ok and read_error, under the names of one read. ReadAt is when the newest record of the
// daemon Dispatch made was made, null where it made none; OK is whether the read succeeded, false
// where none was made; Error is why it did not, null where it did or where none was made.
type legionReadResponse struct {
	ReadAt *time.Time `json:"read_at"`
	OK     bool       `json:"ok"`
	Error  *string    `json:"error"`
}

// getLegionRead answers GET /api/v1/projects/{key}/legion/read, for any authenticated caller, as
// the board is. Its refusals are the board's: 400 INVALID_PROJECT for a key that is no project
// key and 404 PROJECT_NOT_FOUND for a project that does not exist (legionProject), and 404
// LEGION_NOT_CONFIGURED for a project that names no daemon.
func (s *server) getLegionRead(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	answer, err := s.readLegionRead(r.Context(), project)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// readLegionRead is GET /api/v1/projects/{key}/legion/read's body for project, so the route and
// the live view answer one body through one function. It reads the project's Legion setting and
// the newest read of its daemon (readDaemonRead) in one read-only, repeatable-read transaction, as
// an issue's Legion read does, so the two answer one moment. A project that names no daemon is
// legion.ErrNoSettings, which the route answers 404 and the live hub refuses the view with.
func (s *server) readLegionRead(ctx context.Context, project string) (legionReadResponse, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return legionReadResponse{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := legion.GetSettings(ctx, tx, project); err != nil {
		return legionReadResponse{}, err
	}
	read, err := readDaemonRead(ctx, tx, project)
	if err != nil {
		return legionReadResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return legionReadResponse{}, err
	}
	var answer legionReadResponse
	if read != nil {
		at := read.at.UTC()
		answer = legionReadResponse{ReadAt: &at, OK: read.ok, Error: read.failure}
	}
	return answer, nil
}
