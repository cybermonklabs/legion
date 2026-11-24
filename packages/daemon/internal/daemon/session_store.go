package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// sessionStoreFailedEventID is the identity of one decision about one session store failure: the
// claim token, the claim's launch, the moment its session first read the failure, and, for a
// decision the hour of the claim's last notice holds, that notice's moment. intake.ApplyFact's
// applied mark then makes each decision one, however many sweeps read it: a failure told of, or
// held with its one line, is decided once. The held decision has an id of its own, so the
// decision the claim is owed once that hour has passed is not the one the hold spent, and a
// failure still standing then is told; a failure that clears and comes back, or a relaunch, has a
// start or a launch of its own and so an id of its own.
func sessionStoreFailedEventID(token claim.Token, generation uint64, since time.Time, heldBy time.Time) string {
	id := fmt.Sprintf("session-store-failed:%s:%d:%s", token, generation, since.UTC().Format(time.RFC3339Nano))
	if !heldBy.IsZero() {
		id += ":held-by:" + heldBy.UTC().Format(time.RFC3339Nano)
	}
	return id
}

// tellSessionStoreFailures applies, over the claims the orphan sweep already read, one
// intake.SessionStoreFailed for each claim of an issue whose session reports its Oh My Pi
// session store refusing its writes, a failure that has stood a minute and of which nobody has
// been told (workflow.SessionStoreNoticeOwed). A claim that runs no process now (supervise's
// gone states) is passed over: its report is of a session that has stopped, and its next launch
// clears it. The sweep filters and the handler decides, on the claims of the project it reads
// inside the fact's own transaction, whether the issue's architect is told, the project's
// controller for a burst, or nobody (workflow's sessionStoreFailed).
//
// A claim of no issue (the controller's) is skipped, as the silence sweep skips it: its notice
// would name no issue and reach no architect.
func (s *supervision) tellSessionStoreFailures(ctx context.Context, claims []supervise.Claim) {
	if s.applySweepFacts == nil {
		return
	}
	now := s.plan.clock.Now()
	gone := supervise.GoneStates()
	var facts []sweptFact
	for _, c := range claims {
		if c.Issue == "" || c.Role == "" || slices.Contains(gone, c.State) {
			continue
		}
		owed, held := workflow.SessionStoreNoticeOwed(c.StoreFailedSince, c.StoreFailureToldAt, now)
		if !owed {
			continue
		}
		var heldBy time.Time
		if held {
			heldBy = c.StoreFailureToldAt
		}
		facts = append(facts, sweptFact{
			ID: sessionStoreFailedEventID(c.Token, c.Generation, c.StoreFailedSince, heldBy),
			Fact: intake.SessionStoreFailed{Token: c.Token, Issue: c.Issue, Role: c.Role, Generation: c.Generation,
				Since: c.StoreFailedSince},
		})
	}
	if len(facts) == 0 {
		return
	}
	// workflowRuntime.applySweepFacts logs every failure, and the next sweep retries it.
	_ = s.applySweepFacts(ctx, facts)
}
