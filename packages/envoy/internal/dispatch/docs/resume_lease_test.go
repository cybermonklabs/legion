package docs

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// Two envoy-dispatch processes run the settlement resumption, each through
// RunSettlementResumption itself and each on a pool of its own (storetest.SecondProcess), and
// both tick at once with a settlement owed past settlementResumeAge: one runs the pass and arms
// it, the other skips, so the owed settlement is resumed once in the period rather than once per
// process (plan D9, audit finding 9).
func TestTwoResumptionRunnersResumeAnOwedSettlementOncePerPeriod(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
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
	first, _ := newTestServiceInstance(t, database)
	other, _ := newTestServiceInstance(t, second)
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))

	// Both first ticks land in one period of the database's clock, not on either side of a
	// boundary, where each would serve a period of its own.
	if untilBoundary := time.Until(time.Now().Truncate(SettlementResumeInterval).Add(SettlementResumeInterval)); untilBoundary < 10*time.Second {
		time.Sleep(untilBoundary + time.Second)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{}, 2)
	for _, runner := range []struct {
		service *Service
		leases  *store.RawPool
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
	waitFor(t, roomWait(t), "both runners' first tick to run or skip", func() bool { return ran()+skipped() >= 2 })
	stop()
	<-done
	<-done

	resumed := strings.Count(logs.String(), `msg="dispatch: resumed owed document settlements"`)
	if ran() != 1 || skipped() != 1 || resumed != 1 {
		t.Fatalf("two resumption runners in one period: %d passes ran, %d skipped, the owed settlement resumed %d times; want one, one and once:\n%s",
			ran(), skipped(), resumed, logs.Lines("lease="))
	}
}

// A resumption pass is background work: no request waits on it, so the one statement that picks
// and stamps the owed rows (resumeOwedSettlements) runs on the background pool and takes no
// connection of the request pool, which admission counts for requests. Pointed back at the request
// pool, as main's statement was written, it moves that pool's acquires and this fails.
func TestAResumptionPassTakesNoRequestPoolConnection(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t)
	background, err := service.store.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	requests, backgroundPicks := service.store.Pool.Stat().AcquireCount(), background.Stat().AcquireCount()
	if err := service.resumeOwedSettlements(context.Background(), settlementResumeAge); err != nil {
		t.Fatalf("run a resumption pass: %v", err)
	}
	if took := service.store.Pool.Stat().AcquireCount() - requests; took != 0 {
		t.Errorf("the resumption pass took %d request-pool connection(s), want none: it picks the owed rows on the background pool", took)
	}
	if background.Stat().AcquireCount() == backgroundPicks {
		t.Error("the resumption pass took no background-pool connection, so it picked the owed rows somewhere else")
	}
}
