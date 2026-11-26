package docs

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Two envoy-dispatch processes run the settlement resumption, each through
// RunSettlementResumption itself and each on a pool of its own (storetest.SecondProcess), and
// both tick at once with a settlement owed past settlementResumeAge: one runs the pass and arms
// it, the other skips, so the owed settlement is resumed once in the period rather than once per
// process (plan D9, audit finding 9). Both tick on one fake clock no one moves: each first tick is
// due at the fake's moment (duty.TickAtStart), so both fall in one period, whatever the machine's
// pace, and neither makes a second.
func TestTwoResumptionRunnersResumeAnOwedSettlementOncePerPeriod(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := excerptLeases(t, database), excerptLeases(t, second)
	artifactID := createDocument(t, database, "# Owed")
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, last_actor, marked_at) values ($1, null, now() - interval '2 minutes')
	`, artifactID); err != nil {
		t.Fatalf("owe the document's settlement: %v", err)
	}
	ticks := clocktest.New(time.Date(2026, time.November, 19, 12, 0, 30, 0, time.UTC))
	first, other := newResumptionService(t, database, ticks), newResumptionService(t, second, ticks)
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{}, 2)
	for _, runner := range []struct {
		service *Service
		leases  *pgxpool.Pool
	}{{first, firstLeases}, {other, secondLeases}} {
		go func() {
			defer func() { done <- struct{}{} }()
			runner.service.RunSettlementResumption(runCtx, runner.leases)
		}()
	}
	ran := func() int {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", settlementResumptionLease)
	}
	skipped := func() int {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", settlementResumptionLease)
	}
	waitFor(t, "both runners' first tick to run or skip", func() bool { return ran()+skipped() >= 2 })
	stop()
	testwait.Receive(t, "the first resumption runner to return once it was stopped", done)
	testwait.Receive(t, "the second resumption runner to return once it was stopped", done)

	resumed := strings.Count(logs.String(), `msg="dispatch: resumed owed document settlements"`)
	if ran() != 1 || skipped() != 1 || resumed != 1 {
		t.Fatalf("two resumption runners in one period: %d passes ran, %d skipped, the owed settlement resumed %d times; want one, one and once:\n%s",
			ran(), skipped(), resumed, logs.Lines("lease="))
	}
}

// newResumptionService is one more document service on database, as a process of its own serving
// it, whose clock - the one the settlement resumption ticks on - is ticks.
func newResumptionService(t *testing.T, database *store.Store, ticks clock.Clock) *Service {
	t.Helper()
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.Clock = ticks
	return newTestServiceWith(t, deps)
}
