package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

type relatedReadHoldInput struct {
	Duration string `json:"duration" required:"true"`
}

type relatedReadHolds struct {
	neighbourhood, search relatedReadHold
}

// Each arm lasts for one read on this process. Waiting takes no pool connection.
type relatedReadHold struct {
	duration   atomic.Int64
	beforeWait func(context.Context) // set only by in-process tests before arming
}

func (hold *relatedReadHold) wait(ctx context.Context) error {
	duration := time.Duration(hold.duration.Swap(0))
	if duration == 0 {
		return nil
	}
	if hold.beforeWait != nil {
		hold.beforeWait(ctx)
	}
	waitFor(ctx, duration)
	return ctx.Err()
}

// armNeighbourhoodReadHold and armRelatedSearchHold are the two hold routes' handlers, each a
// method of its own: the routes table names a handler by its method, which is how a reader of the
// table's source follows a row to what it runs (TestEveryRouteThatWritesADocumentChecksItsWriter).
func (s *server) armNeighbourhoodReadHold(w http.ResponseWriter, r *http.Request) {
	s.armRelatedReadHold(w, r, &s.relatedReadHolds.neighbourhood)
}

func (s *server) armRelatedSearchHold(w http.ResponseWriter, r *http.Request) {
	s.armRelatedReadHold(w, r, &s.relatedReadHolds.search)
}

func (s *server) armRelatedReadHold(w http.ResponseWriter, r *http.Request, hold *relatedReadHold) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input relatedReadHoldInput
	if err := decodeJSONTestHook(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	duration, err := time.ParseDuration(input.Duration)
	if err != nil || duration <= 0 {
		writeError(w, "INVALID_HOLD", http.StatusBadRequest, "duration must be a positive Go duration")
		return
	}
	hold.duration.Store(int64(duration))
	w.WriteHeader(http.StatusNoContent)
}
