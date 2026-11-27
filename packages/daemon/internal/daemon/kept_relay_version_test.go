package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A person's changes request on a tree nobody in it hears is kept for the root's next start or
// catch-up and relayed to the controller (record.RelayParkedDecision), and the two must get one
// answer to whether the request is still the person's word (dispatch://INFRA-3136/spec R2: one
// rule, asked by every sender). The request is on version 1 of the spec, at Dispatch's mark 12.
// The gate then learns, by a registration, that the person approved version 2 at Dispatch's mark
// 10: an approval of a later version than the one the request reviewed, which the one rule reads
// as the person's word in the request's place (classify.ReviewSuperseded: approved > version),
// though Dispatch marked it before the request. The generation is then cleared, of the tree or of
// the issue alone, which asks the kept row and the queued relay before it deletes the gates
// (workflow.Engine.ClearGeneration). Both are superseded: the kept row is forgotten and the relay
// is dropped. A kept row that records no version is asked at the gate's approved version and by
// its mark alone, so the request's later mark makes it stand while its relay, which carries the
// request's version, is dropped: one request, two answers.
func TestAKeptAndARelayedChangesRequestAgreeOnAnApprovalOfALaterVersionMarkedBeforeIt(t *testing.T) {
	t.Parallel()
	reviewed := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	changes := intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactChangesRequested, Version: 1, Reason: "name the rollback path",
		Actor: alice, Mark: record.EventMark{Seq: 12, At: reviewed}}
	laterApproval := intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "artifact-1", Version: 2,
		DispatchVersion: 2, ApprovedVersion: 2,
		ApprovedMark: record.EventMark{Seq: 10, At: reviewed.Add(-time.Minute)}, ReviewedBy: alice}
	for _, tc := range []struct {
		name  string
		clear func(context.Context, pgx.Tx, *workflow.Engine, time.Time) error
	}{
		{name: "the tree's clear", clear: func(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, at time.Time) error {
			return engine.ClearTreeGeneration(ctx, tx, "LEGION-1", at)
		}},
		{name: "the issue's clear", clear: func(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, at time.Time) error {
			issue, err := record.NewStore().Issue(ctx, tx, "LEGION-1")
			if err != nil {
				return err
			}
			return engine.ClearGeneration(ctx, tx, *issue, at)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1",
					Step: record.DesignStepSpec, ArtifactID: "artifact-1", LatestVersion: 1})
			}); err != nil {
				t.Fatalf("register the spec gate at version 1: %v", err)
			}
			clock := reviewed.Add(time.Hour)
			now := func() time.Time { return clock }
			steps := config.DesignGateRootIssues.Steps()
			engine := workflow.New(records, workflow.Config{Project: "LEGION",
				Repositories: testRepositories, Steps: steps, Clock: now}, quietLogger())
			applyFacts(t, pool, engine, []intake.Fact{changes})

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateFailed)
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"), notify.ControllerTopic("legion"))
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION",
				records: records, notices: publisher, supervisor: sup, project: "legion",
				gatedSteps: steps, now: now}
			// The first run relays the request, whose notice finds its architect's claim failed;
			// the second is the relay's first attempt, which waits for the controller's claim.
			for run := range 2 {
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %d before the clear: %v", run, err)
				}
			}
			relays := func() int {
				var queued int
				if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = $1
					and payload->>'kind' = $2 and payload->>'decision' = $3`,
					string(record.OutboxKindControllerNotice), string(record.ParkedDecisionNotice),
					string(record.DecisionDesignChangesRequested)).Scan(&queued); err != nil {
					t.Fatalf("count the relays of the changes request: %v", err)
				}
				return queued
			}
			keptRequest := func() bool {
				for _, d := range keptDecisions(t, pool, records, "LEGION-1") {
					if d.Decision == record.DecisionDesignChangesRequested {
						return true
					}
				}
				return false
			}
			if !keptRequest() || relays() != 1 {
				t.Fatalf("before the clear the request is kept: %v, and the outbox holds %d relays of it; "+
					"want it kept and relayed once", keptRequest(), relays())
			}

			clock = clock.Add(time.Minute)
			applyFacts(t, pool, engine, []intake.Fact{laterApproval})
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return tc.clear(ctx, tx, engine, clock)
			}); err != nil {
				t.Fatalf("clear the generation: %v", err)
			}
			if kept, relayed := keptRequest(), relays(); kept || relayed != 0 {
				t.Fatalf("after %s the request is still kept: %v, and the outbox holds %d relays of it; "+
					"want neither: the person approved version 2 after reviewing version 1, which "+
					"supersedes the request for the kept row as for its relay (one rule, one answer)",
					tc.name, kept, relayed)
			}
		})
	}
}
