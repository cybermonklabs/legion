package daemon

// The merger owes its ask only while the issue stands in merging, so a row of the ask that ended
// while the issue stood outside merging told nobody, and at the move back into merging the
// workflow tells the merger's launch the ask (the workflow's askMergerAtEntry). The launch it
// tells is the merger's claim as the claim store holds it (claims.generation), and the row it
// queues goes to the session of the launch the executor reads (deliverHeld). These tests hold the
// two together with the claim written by the supervisor's own machine, read by the move, and
// delivered by the outbox's executor, as the daemon runs them.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergerAskedAtEntryText is the whole line the workflow logs where the move into merging tells the
// merger's launch the merge forward it owes (the workflow's mergerAskedAtEntryLine), the line the
// production check of that send reads.
const mergerAskedAtEntryText = "workflow: a merge forward still owed is asked again at the move into merging"

// mergersOwedAsk is the merger's ask of owedAskHead on #86 in merging, as the move into merging
// queues it again.
var mergersOwedAsk = record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
	Phase: phase.Merging, PullRequest: 86, Why: record.ConflictMergeNext}

// askOwedInRetro is mergeForwardRunnerAt's LEGION-208 standing in retro, its pull request #86 open
// at owedAskHead with the merger's ask of it as the record's merge forward, told no launch, and no
// row of it queued: the state a row of the ask leaves that ended while the issue stood outside
// merging. The rig's supervisor writes its claims to the test database's own claim store, as the
// daemon's does, so the claims row of each claim it creates is the machine's own write; the
// workflow runs behind the supervisor, as failedWithTheAsk has it, on an engine that logs to the
// buffer it answers; the implementer's phases row of retro is recorded; and the architect's role
// topic has no holder, so the notice retro's completion queues for the architect is told to
// nobody. The rig holds no claim yet.
func askOwedInRetro(t *testing.T) (*outbox, *supervisor, *store.Pool, *holderPublisher, *workflow.Engine, *syncBuffer) {
	t.Helper()
	publisher := &holderPublisher{}
	asked := record.MergeForward{Head: owedAskHead, Base: "main", Role: claim.RoleMerger, Phase: phase.Merging,
		Why: record.ConflictMergeNext, AskedAt: time.Now()}
	runner, sup, pool, _ := mergeForwardRunnerAt(t, publisher, phase.Retro, workflow.PhaseStatus(phase.Retro), record.PullRequestOpen, &asked)
	trees, ok := runner.trees.(*store.Store)
	if !ok {
		t.Fatalf("the runner's trees are a %T, want the store the move reads the merger's claim from", runner.trees)
	}
	sup.deps.Store = trees
	logs := &syncBuffer{}
	engine := workflow.New(runner.records, workflow.Config{Project: "legion", Repositories: testRepositories}, slog.New(slog.NewJSONHandler(logs, nil)))
	sup.OnDurableState((&workflowRuntime{pool: pool, records: runner.records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return runner.records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer,
			Claim: mustClaimToken(t, "LEGION-208", claim.RoleImplementer)})
	}); err != nil {
		t.Fatalf("record the implementer's retro phase of LEGION-208: %v", err)
	}
	publisher.setAbsent(architectTopic(t, "LEGION-208"))
	return runner, sup, pool, publisher, engine, logs
}

// storedMergerClaim is LEGION-208's merger claim as the claim store holds it: its launch
// (claims.generation) and its state.
func storedMergerClaim(t *testing.T, pool *store.Pool) (uint64, supervise.ClaimState) {
	t.Helper()
	var launch uint64
	var state supervise.ClaimState
	if err := pool.QueryRow(context.Background(), `select generation, state from claims where token = $1`,
		string(mustClaimToken(t, "LEGION-208", claim.RoleMerger))).Scan(&launch, &state); err != nil {
		t.Fatalf("read the merger's stored claim: %v", err)
	}
	return launch, state
}

// retroCompletes is the implementer's completion of LEGION-208's retro at owedAskHead, applied
// through the engine as the daemon applies a fact, which moves the issue into merging. It fails
// the test unless the completion was taken and the issue stands in merging after it.
func retroCompletes(t *testing.T, pool *store.Pool, engine *workflow.Engine) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", "retro-LEGION-208", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: mustClaimToken(t, "LEGION-208", claim.RoleImplementer),
		Summary: "retro done", Commit: owedAskHead}, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply retro's completion = %+v, %v; want it taken", result.Refusal, err)
	}
	issue := readIssue(t, pool, "LEGION-208")
	if issue == nil {
		t.Fatal("after retro's completion the record holds no LEGION-208")
	}
	if issue.Phase != phase.Merging {
		t.Fatalf("after retro's completion LEGION-208 is in %s, want merging", issue.Phase)
	}
}

// The merger's ask of the head is owed again at the move into merging by retro's completion, and
// the record names no launch it was told at. The merger that has been ready at its launch since
// before the move, whose ready came in retro and sent nothing, is told the ask once at the move,
// by a row the executor delivers to that launch's session, and the workflow logs the send with
// that launch. A released merger is woken by the move's row, and its woken launch, which comes
// ready while that row is still queued, is told the ask once by that row: the ready sends no
// second one.
func TestTheMoveIntoMergingTellsTheMergersLaunchTheAskItOwes(t *testing.T) {
	t.Parallel()
	token := mustClaimToken(t, "LEGION-208", claim.RoleMerger)

	t.Run("a merger at its launch", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		runner, sup, pool, publisher, engine, logs := askOwedInRetro(t)
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: claim.RoleMerger, State: supervise.StateQueued, Session: "ses-merger", SessionFile: t.TempDir() + "/session.jsonl"}, "")
		if err != nil {
			t.Fatalf("create the merger's claim: %v", err)
		}
		sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
		if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
			t.Fatalf("launch the merger: %v", err)
		}
		comesUp(t, machine, true)
		if launch, state := storedMergerClaim(t, pool); launch != 1 || state != supervise.StateReady {
			t.Fatalf("before the move the store holds the merger %s at launch %d, want it ready at launch 1", state, launch)
		}
		publisher.holdBy(func() string { return machine.View().Claim.Session })

		retroCompletes(t, pool, engine)
		drainAt(t, runner, 0, "the attempts after the move into merging")
		drainAt(t, runner, 10*time.Minute, "a later attempt")
		askToldOnce(t, pool, publisher, mergersOwedAsk, 1)
		asked := loggedMessages(t, logs.String(), mergerAskedAtEntryText)
		if len(asked) != 1 || asked[0]["launch"] != float64(1) {
			t.Fatalf("the engine logged %v, want one %q line naming launch 1", asked, mergerAskedAtEntryText)
		}
	})

	t.Run("a released merger the ask wakes", func(t *testing.T) {
		t.Parallel()
		runner, sup, pool, publisher, engine, _ := askOwedInRetro(t)
		machine := releasedClaim(t, sup, claim.RoleMerger)
		if launch, state := storedMergerClaim(t, pool); launch != 1 || state != supervise.StateSuspended {
			t.Fatalf("before the move the store holds the merger %s at launch %d, want it released at launch 1", state, launch)
		}

		retroCompletes(t, pool, engine)
		drainAt(t, runner, 0, "the attempts after the move into merging")
		machine.Wait()
		if held := machine.Claim(); held.Released() || held.Generation < 2 {
			t.Fatalf("after the move's attempts the merger is %s at launch %d, want it woken into a new launch", held.State, held.Generation)
		}
		// The woken launch registers the session its claim records and comes ready while the
		// move's row is still queued, and that session then holds the role.
		sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
		comesUp(t, machine, true)
		publisher.holdBy(func() string { return machine.View().Claim.Session })
		drainAt(t, runner, 10*time.Minute, "the attempt once the woken session holds the role")
		drainAt(t, runner, 20*time.Minute, "a later attempt")
		askToldOnce(t, pool, publisher, mergersOwedAsk, machine.Claim().Generation)
	})
}
