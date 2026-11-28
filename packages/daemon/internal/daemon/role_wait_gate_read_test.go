package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// errHoldsUnanswered is what a claim store the daemon cannot reach answers a hold of a session's
// names (supervise.Store.HoldSessions): no claim.Refusal, so it says nothing of whether the claim
// holds its session.
var errHoldsUnanswered = errors.New("the claim store did not answer: connection refused")

// unansweredHolds is a claim store whose hold of a session's names answers errHoldsUnanswered while
// down is set, and is the store behind it otherwise.
type unansweredHolds struct {
	supervise.Store
	down atomic.Bool
}

func (s *unansweredHolds) HoldSessions(ctx context.Context, project string, token claim.Token, holds []supervise.Hold) ([]claim.Token, error) {
	if s.down.Load() {
		return nil, errHoldsUnanswered
	}
	return s.Store.HoldSessions(ctx, project, token, holds)
}

// A row of each of the nine kinds to a registered launch that has not said it is ready, whose gate
// cannot read whether the claim holds the session it records (supervise.Machine.HoldsItsSession
// answers the store's error, no claim.Refusal), publishes nothing and ends its run in that
// failure: the listener was not asked, so the run made no publish, and a store read stays a
// failure (dispatch://INFRA-4131/spec#b-r-capped; the architect's ruling,
// dispatch://INFRA-4131/comment/19e7d9e9-97a3-42a1-b0eb-38aaa8e2f502). The run is logged as one
// failure, `outbox row failed` at Error naming the store's error, and writes no wait line. The row
// stays queued on the outbox's own backoff (record.RetryBackoff) and took no wait: no wait start,
// no step, nothing it waits for, and no launch marked as published to, so its next run, once the
// read works again, publishes to that same launch and ends at that publish.
func TestRoleWaitCapKeepsAFailedSessionHoldReadAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, nil)
			holds := &unansweredHolds{Store: h.sup.deps.Store}
			h.sup.deps.Store = holds
			h.claim(supervise.StateRegistered, nil)
			rig := h.queue()
			holds.down.Store(true)

			rig.run()
			at := h.clock.Now()
			if got := h.publisher.count(); got != 0 {
				t.Fatalf("a row whose gate could not read the claim's session published %d times, want 0", got)
			}
			var due time.Time
			var since, ready, logged bool
			var step int
			var launch int64
			var waitsFor string
			if err := h.pool.QueryRow(context.Background(), `select next_at, wait_since is not null, wait_logged, wait_step,
				wait_ready is not null, wait_launch, wait_for from outbox where id = $1`, rig.id).
				Scan(&due, &since, &logged, &step, &ready, &launch, &waitsFor); err != nil {
				t.Fatalf("read row %d after its failed run: %v", rig.id, err)
			}
			if since || logged || step != 0 || ready || launch != 0 || waitsFor != "" {
				t.Fatalf("after its failed run row %d holds a wait (started %t, logged %t, step %d, ready %t, launch %d, waits for %q), want none",
					rig.id, since, logged, step, ready, launch, waitsFor)
			}
			if want := at.Add(record.RetryBackoff(0)); !due.Equal(want) {
				t.Fatalf("after its failed run row %d is due %s after it, want the outbox's backoff, %s", rig.id, due.Sub(at), record.RetryBackoff(0))
			}
			logs := rig.logs.String()
			var failed []map[string]any
			for _, line := range rowLines(t, logs, rig.id) {
				switch line["msg"] {
				case wantWaitStartLine, wantWaitedLongLine, wantTookNothingLine, wantWaitEndedLine:
					t.Errorf("the failed run wrote the wait line %v, want no wait:\n%s", line, logs)
				case "outbox row failed":
					failed = append(failed, line)
				}
			}
			if len(failed) != 1 || failed[0]["level"] != slog.LevelError.String() {
				t.Fatalf("the failed run wrote %v, want one `outbox row failed` line at Error:\n%s", failed, logs)
			}
			if !strings.Contains(fmt.Sprint(failed[0]["error"]), errHoldsUnanswered.Error()) {
				t.Errorf("the failure line %v does not name the store's error %q", failed[0], errHoldsUnanswered)
			}

			holds.down.Store(false)
			rig.endsAtItsNextRun()
			if got := h.publisher.count(); got != 1 {
				t.Fatalf("the run after the read worked again published %d times in all, want 1", got)
			}
		})
	}
}
