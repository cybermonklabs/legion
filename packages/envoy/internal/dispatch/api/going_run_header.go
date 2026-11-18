package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// withinGoingRun runs a write route's handler for the run of the command bar its request names in
// contracts.GoingRunHeader, where it names one (dispatch://INFRA-1950/spec#b-r-interaction). It is
// the one reader of that header, once per request: routeHandler, the one chain register mounts
// every row behind, wraps every POST, PATCH, PUT and DELETE route in it, and the run it admits
// rides on the request's context (graphstore.WithRun), where graphstore.Write and
// graphstore.Caused read it and append a changed_by_run tuple from each node the write marks
// changed. So no write path takes the run as an argument and none can leave
// it behind: a write path only marks the nodes it created or changed, which is what it alone
// knows, and a write route added later is held to the header with no line of its own. A helper
// each handler called would be one call in every handler that writes, each a place the header
// could be read twice or not at all.
//
// The run must be one the record holds, live - now() < ends_at, whatever frames it has stored -
// and the caller's, by the test the stream holds its reader to (goingRunIsTheCallers: the whole
// credential, and an author that proves somebody). A header sent twice, one that is no uuid, a run
// nobody recorded, one past its window and one that is anybody else's are each answered what the
// stream answers for a run nobody holds (writeGoingRequestNotFound), before the handler runs, so
// the write stores nothing. A caller that does not resolve is answered as the stream answers it.
// A request that names no run runs as it always did and costs nothing new: the header is read
// from the request alone, and only a request that carries it resolves its caller here or sends a
// query (readLiveGoingRun), so a write with no header sends no statement more than it did before,
// and no write of the store on its context reads a changed_by_run (graphstore.Write and Caused
// send that read only for a context holding a run).
//
// routeHandler puts it inside the project and session scope checks and outside the key layer, so a
// caller held to another project is told that first, and a refused run claims no Idempotency-Key:
// the same intent sent again with the header mended, or without it, runs. The caller it resolves
// is put on the context for the key layer and the handler, as refuseChangedLogin does, so it is
// resolved once a request.
func (s *server) withinGoingRun(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		named := r.Header.Values(contracts.GoingRunHeader)
		if len(named) == 0 {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			s.writeAuthenticationError(w, err)
			return
		}
		// Decided before the read, as the stream decides it: readLiveGoingRun casts $1::uuid, so
		// an id that is not a uuid would come back as a Postgres error rather than pgx.ErrNoRows.
		// What goes on is the parsed uuid's own spelling and never the header's: uuid.Parse also
		// admits `urn:uuid:<id>`, `{<id>}` and upper case, the URN form of which Postgres refuses
		// (SQLSTATE 22P02, a 500), and the run on the context is the id every changed_by_run
		// tuple points at, the lowercase text the interaction node is keyed by.
		parsed, err := uuid.Parse(named[0])
		if len(named) != 1 || err != nil {
			writeGoingRequestNotFound(w)
			return
		}
		run := parsed.String()
		author, live, err := s.readLiveGoingRun(r.Context(), run)
		if errors.Is(err, pgx.ErrNoRows) {
			writeGoingRequestNotFound(w)
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !live || !goingRunIsTheCallers(author, actor) {
			writeGoingRequestNotFound(w)
			return
		}
		ctx := context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human})
		handler(w, r.WithContext(graphstore.WithRun(ctx, run)))
	}
}
