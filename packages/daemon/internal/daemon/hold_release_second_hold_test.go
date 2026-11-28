package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A second hold in one boot, as a repository's recovery makes after the boot's own, is released by
// the same consumer position that released the first: the poll's fact names the hold it releases,
// so it is applied again rather than taken for the first release's duplicate, and the hold ends.
// The second hold is made while every read of the position waits (park), so the test sees the
// hold before the poll can act on it; the position the poll then reads is the same one that
// released the first hold.
func TestPollHoldReleaseReleasesASecondHoldTheSamePositionReaches(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: "LEGION-EDGE", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: "CAPTURE", Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	hold := func(seq int64) {
		t.Helper()
		reconcileAdmission(t, pool, admission, []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", ResolvedRepo: "sjawhar/legion", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: seq},
		}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
		if !admission.Held() {
			t.Fatalf("Held() = false after a listing at sequence %d behind the stream, want it held", seq)
		}
	}
	reader := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 5, Idle: true}}
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, holdPollInterval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	hold(2)
	go func() { w.pollHoldReleaseWith(ctx, reader); close(done) }()
	testwait.Eventually(t, "the first hold released", func() bool { return !admission.Held() })
	time.Sleep(50 * time.Millisecond)

	release := reader.park()
	hold(3)
	release()
	testwait.Eventually(t, "the second hold released by the same position", func() bool { return !admission.Held() })
	cancel()
	<-done
}
