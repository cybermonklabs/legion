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

func (s *server) armRelatedReadHold(hold *relatedReadHold) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
}
