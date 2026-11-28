package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// withinGoingRun runs a write route's handler for the run of the command bar its request names in
// contracts.GoingRunHeader, where it names one (spec requirement r-interaction). It is
// the one reader of that header, once per request: routeHandler, the one chain register mounts
// every row behind, wraps every POST, PATCH, PUT and DELETE route in it, and the run it admits
// rides on the request's context (store.WithRun), where every write phase built for that context
// carries it (store.NewWriteBatch) and the graph store's writers that queue on it
// (graphstore.QueueWrite, graphstore.QueueCaused) append a changed_by_run tuple from each node the
// write marks changed. So no write path takes the run as an argument and none can leave it behind:
// a write path only marks the nodes it created or changed, which is what it alone knows, and a
// write route added later is held to the header with no line of its own. A helper each handler
// called would be one call in every handler that writes, each a place the header could be read
// twice or not at all.
//
// The run must be one the record holds, live - now() < ends_at, whatever frames it has stored -
// and the caller's, by the test the stream holds its reader to (goingRunIsTheCallers: the whole
// credential, model.Actor.SameCredential, of an author that names one,
// graphstore.NamesCredential). A header sent twice, one that is no uuid, a run nobody recorded,
// one past its window and one that is anybody else's are each answered what the stream answers
// for a run nobody holds (writeGoingRequestNotFound), before the handler runs, so the write
// stores nothing. A caller that does not resolve is answered as the stream answers it.
// A request that names no run runs as it always did and costs nothing new: the header is read
// from the request alone, and only a request that carries it resolves its caller here or sends a
// query (readLiveGoingRun), so a write with no header sends no statement more than it did before,
// and no write of the store on its context reads a changed_by_run (graphstore.QueueWrite and
// QueueCaused send that read only on a batch made for a run).
//
// routeHandler puts it inside the project and session scope checks and outside the key layer, so a
// caller held to another project is told that first, and a refused run claims no Idempotency-Key:
// the same intent sent again with the header mended, or without it, runs. The caller it resolves
// is put on the context for the key layer and the handler, as refuseChangedLogin does, so it is
// resolved once a request.
//
// The refusal stands in for the handler, so a request whose handler would not run is not refused
// here: a write sent again under the Idempotency-Key of one the key layer already answered is
// answered that answer before its run is checked, so a retry after the run has ended reads the
// write it made rather than this refusal (spec R4). On a route behind the key layer (keyed:
// keyedMethod), a refused request whose key holds a row that layer answers itself
// (requestKeyAnswersItself, the look refuseUploadToAClosedIssue takes for its refusal too) goes on
// with no run on its context and a goingRunRefused there instead, and the layer answers it from
// that key alone and never runs its handler (answerRefusedRun): with what the key holds when the
// layer reads it, or with this refusal where it no longer holds an answer. So every handler that
// runs for a request naming a run runs with that run admitted, a key that stops answering between
// the two looks is answered this refusal, and a refused request whose key holds nothing claims
// none. The key's digest holds the header (writeRequestFields), so a repeat names the same run:
// one that names another run, or none, is the key reused.
func (s *server) withinGoingRun(handler http.HandlerFunc, keyed bool) http.HandlerFunc {
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
		ctx := context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human})
		refuse := func() {
			if !keyed {
				writeGoingRequestNotFound(w)
				return
			}
			key, present, err := requestKeyHeader(r)
			if !present || err != nil {
				writeGoingRequestNotFound(w)
				return
			}
			login := requestKeyLogin(actor, human)
			answers, failed := s.lookUpRequestKeyAnswers(ctx, w, login, key)
			if failed {
				return
			}
			if !answers {
				writeGoingRequestNotFound(w)
				return
			}
			handler(w, r.WithContext(context.WithValue(ctx, goingRunRefusedKey{}, goingRunRefused{login: login, key: key})))
		}
		run, ok := goingRunNamed(named)
		if !ok {
			refuse()
			return
		}
		author, live, err := s.readLiveGoingRun(ctx, run)
		if errors.Is(err, pgx.ErrNoRows) {
			refuse()
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !live || !goingRunIsTheCallers(author, actor) {
			refuse()
			return
		}
		handler(w, r.WithContext(store.WithRun(ctx, run)))
	}
}

// goingRunNamed is the run named, the values of a request's contracts.GoingRunHeader, as
// withinGoingRun admits it and writeRequestFields digests it: the parsed uuid's own spelling, where
// the request names exactly one value and it parses. It is decided before any read, as the stream
// decides it: readLiveGoingRun casts $1::uuid, so an id that is not a uuid would come back as a
// Postgres error rather than pgx.ErrNoRows. What goes on is never the header's spelling: uuid.Parse
// also admits `urn:uuid:<id>`, `{<id>}` and upper case, the URN form of which Postgres refuses
// (SQLSTATE 22P02, a 500), and the run on the context is the id every changed_by_run tuple points
// at, the lowercase text the interaction node is keyed by.
func goingRunNamed(named []string) (string, bool) {
	if len(named) != 1 {
		return "", false
	}
	parsed, err := uuid.Parse(named[0])
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// goingRunRefused is a request whose run withinGoingRun refused and left to its Idempotency-Key,
// key, which login's key row held an answer for when withinGoingRun looked: the key layer answers
// it from that key or with the refusal, and never runs its handler (answerRefusedRun).
type goingRunRefused struct{ login, key string }

// goingRunRefusedKey carries a goingRunRefused to the key layer on the request's context.
type goingRunRefusedKey struct{}
