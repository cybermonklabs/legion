package daemon

// The silence sweep leaves alone a phase worker the record gives a reason of the daemon's own to
// take no turn (dispatch://INFRA-2040 R1 and R3): a merger the closed design gate holds, and a
// worker whose wait stands. Each case drives the pod-replacement rig's real supervision and
// machines over a real Postgres, with the workflow the daemon attaches to supervision: the sweep's
// facts applied through the engine (applySweepFacts) and the sweep's own read of the record before
// it mints any (quietByDesign), on an engine of the issue records' own project.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// podUpdateArtifact is the spec document the rig's tree root registers when a case gates it.
const podUpdateArtifact = "artifact-484"

// quietSweep is the rig with the workflow attached as the daemon attaches it, and every fact the
// sweep minted since it was attached.
type quietSweep struct {
	*podUpdateRig
	w      *workflowRuntime
	minted []sweptFact
}

// attachesWorkflow gives the rig's supervision the workflow the daemon's attach gives it
// (workflowRuntime.attach): an engine of the issue records' project, LEGION, whose deployment
// gates steps, behind both the sweep's facts and its read of the record. Every fact the sweep
// mints is kept before it is applied.
func attachesWorkflow(rig *podUpdateRig, steps []record.DesignStep) *quietSweep {
	rig.t.Helper()
	engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps,
		Clock: rig.clock.Now}, rig.log)
	q := &quietSweep{podUpdateRig: rig, w: &workflowRuntime{pool: rig.pool, records: rig.records, log: rig.log,
		engine: engine, handlers: []intake.Handler{engine}}}
	rig.s.applySweepFacts = func(ctx context.Context, facts []sweptFact) error {
		q.minted = append(q.minted, facts...)
		return q.w.applySweepFacts(ctx, facts)
	}
	rig.s.quietByDesign = q.w.quietByDesign
	return q
}

// applies applies fact as the Dispatch event id through the attached workflow, failing the test
// on an error or a refusal.
func (q *quietSweep) applies(id string, fact intake.Fact) {
	q.t.Helper()
	result, err := intake.ApplyFact(q.ctx, q.pool, "dispatch", id, fact, q.w.handlers...)
	if err != nil || result.Refusal != nil {
		q.t.Fatalf("apply %s = %+v, %v; want it applied", id, result.Refusal, err)
	}
}

// mintedFor is every fact the sweep minted for role's claim since the workflow was attached.
func (q *quietSweep) mintedFor(role claim.Role) []sweptFact {
	var minted []sweptFact
	for _, fact := range q.minted {
		switch f := fact.Fact.(type) {
		case intake.WorkerSilent:
			if f.Role == role {
				minted = append(minted, fact)
			}
		case intake.WorkerBackgroundStalled:
			if f.Role == role {
				minted = append(minted, fact)
			}
		}
	}
	return minted
}

// noticesOf is every notice of the issue of kind the outbox holds.
func (q *quietSweep) noticesOf(kind record.NoticeKind) []record.Notice {
	var found []record.Notice
	for _, notice := range q.silenceNotices() {
		if notice.Kind == kind {
			found = append(found, notice)
		}
	}
	return found
}

// stands moves the rig's issue to at, with at's own status, as a transition records it.
func (q *quietSweep) stands(at phase.Phase) {
	q.t.Helper()
	issue := q.issue()
	issue.Phase, issue.Status = at, workflow.PhaseStatus(at)
	if err := pgx.BeginFunc(q.ctx, q.pool, func(tx pgx.Tx) error { return q.records.PutIssue(q.ctx, tx, issue) }); err != nil {
		q.t.Fatalf("move %s to %s: %v", podUpdateIssue, at, err)
	}
}

// gatesSpec records the tree root's spec row at version 1, approved: an open gate.
func (q *quietSweep) gatesSpec() {
	q.t.Helper()
	approved := 1
	if err := pgx.BeginFunc(q.ctx, q.pool, func(tx pgx.Tx) error {
		return q.records.PutGate(q.ctx, tx, record.DesignGate{Issue: podUpdateIssue, Step: record.DesignStepSpec,
			ArtifactID: podUpdateArtifact, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true})
	}); err != nil {
		q.t.Fatalf("register the spec of %s: %v", podUpdateIssue, err)
	}
}

// aQuietMerger is the issue in merging with its merger up between turns, level with the entry its
// phases row names, the root's spec open, under a deployment gating steps.
func aQuietMerger(t *testing.T, steps []record.DesignStep) *quietSweep {
	t.Helper()
	q := attachesWorkflow(newPodUpdateRig(t, silentWorker(claim.RoleMerger)), steps)
	q.assigns(claim.RoleMerger, silenceEntry)
	q.stands(phase.Merging)
	q.gatesSpec()
	return q
}

// A merger the closed design gate holds gets no task or notice from the silence sweep (R1): the
// sweep reads the hold before it mints anything, so it mints no fact for that claim, and writes no
// start row and no notice, whether the merger's last step is past `worker_silence_minutes` or its
// session's report of background work is past `worker_background_hours`. The gate closes while the
// merger works merging (the spec's version 2, unapproved); after a person approves version 2 the
// gate opens, and the next sweep acts on the merger as on any open phase: its silence sends it the
// phase's task again and tells its architect, and its standing report is told to its architect.
func TestTheSilenceSweepHoldsOffAMergerTheDesignGateHoldsUntilTheGateOpens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background bool
		kind       record.NoticeKind
	}{
		{name: "past worker_silence_minutes", kind: "worker-silent"},
		{name: "a background report past worker_background_hours", background: true, kind: "worker-background-stalled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := aQuietMerger(t, config.DesignGateRootIssues.Steps())
			merger := claim.RoleMerger
			q.goesQuiet(merger)
			q.applies("spec-2", intake.DispatchArtifact{Key: podUpdateIssue, ArtifactID: podUpdateArtifact,
				Kind: intake.DispatchArtifactVersion, Version: 2})
			if tc.background {
				q.s.cfg.WorkerBackground = podUpdateBackground
				q.reportsBackgroundWork(merger, "async-job")
				q.clock.advance(podUpdateBackground + time.Minute)
			}

			q.sweepForSilence()

			if got := q.starts(); len(got) != 0 {
				t.Fatalf("the supervise rows of %s = %+v, want none while the gate holds the merger", podUpdateIssue, got)
			}
			if got := q.noticesOf(tc.kind); len(got) != 0 {
				t.Fatalf("the %s notices of %s = %+v, want none while the gate holds the merger", tc.kind, podUpdateIssue, got)
			}
			if minted := q.mintedFor(merger); len(minted) != 0 {
				t.Fatalf("the sweep minted %+v for the held merger, want nothing: the hold explains its quiet", minted)
			}

			q.applies("approved-2", intake.DispatchArtifact{Key: podUpdateIssue, ArtifactID: podUpdateArtifact,
				Kind: intake.DispatchArtifactApproved, Version: 2})
			q.sweepForSilence()

			if got := q.noticesOf(tc.kind); len(got) != 1 || got[0].Role != merger {
				t.Fatalf("the %s notices of %s = %+v once the gate opened, want the merger's one", tc.kind, podUpdateIssue, got)
			}
			starts := q.starts()
			if tc.background {
				if len(starts) != 0 {
					t.Fatalf("the supervise rows of %s = %+v, want none: a worker whose session says work of its own is running is told of and never prompted", podUpdateIssue, starts)
				}
				return
			}
			if len(starts) != 1 || starts[0].Op != "start" || starts[0].Role != merger || !starts[0].ResumeTask {
				t.Fatalf("the supervise rows of %s = %+v once the gate opened, want the one start that sends the merger its phase's task again", podUpdateIssue, starts)
			}
		})
	}
}

// A gated step that has no gate row holds no merger (R1): the outbox's fence lets that merger
// merge, although the tree's gate (record.TreeGate.Open) reads closed. The deployment gates the
// spec and the decisions, the root's spec row is approved and the decisions have none, so the only
// unapproved gated step has no row, and the sweep acts on the merger quiet past the bound as on
// any open phase: it sends the merger its phase's task again and tells its architect.
func TestTheSilenceSweepRepairsAMergerWhoseOnlyUnapprovedGatedStepHasNoRow(t *testing.T) {
	q := aQuietMerger(t, []record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions})
	merger := claim.RoleMerger
	q.goesQuiet(merger)

	q.sweepForSilence()

	if minted := q.mintedFor(merger); len(minted) != 1 {
		t.Fatalf("the sweep minted %+v for the merger, want its one silence: a step with no row holds no merger", minted)
	}
	if starts := q.starts(); len(starts) != 1 || starts[0].Role != merger || !starts[0].ResumeTask {
		t.Fatalf("the supervise rows of %s = %+v, want the one start that sends the merger its phase's task again", podUpdateIssue, starts)
	}
	if got := q.noticesOf("worker-silent"); len(got) != 1 {
		t.Fatalf("the worker-silent notices of %s = %+v, want the merger's one", podUpdateIssue, got)
	}
}

// A worker whose wait stands gets no task or notice from the silence sweep (R3): the sweep mints
// no fact for its claim, so the wait costs no fact's transaction each minute, and writes no start
// row and no notice, for a silence past the bound and for a background report past its own bound
// alike. The implementer declared a wait in the session it runs, on the run, phase and entry it
// works. Once news reaches its session the wait ends, and the next sweep acts on the implementer
// as before it declared.
func TestTheSilenceSweepWritesNothingForAWorkerWhoseWaitStands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background bool
		kind       record.NoticeKind
	}{
		{name: "past worker_silence_minutes", kind: "worker-silent"},
		{name: "a background report past worker_background_hours", background: true, kind: "worker-background-stalled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := attachesWorkflow(aSilentPhaseWorker(t), config.DesignGateRootIssues.Steps())
			worker := claim.RoleImplementer
			q.goesQuiet(worker)
			wait := q.declaresWait(worker)
			if tc.background {
				q.s.cfg.WorkerBackground = podUpdateBackground
				q.reportsBackgroundWork(worker, "async-job")
				q.clock.advance(podUpdateBackground + time.Minute)
			}

			q.sweepForSilence()
			q.sweepForSilence()

			if got := q.starts(); len(got) != 0 {
				t.Fatalf("the supervise rows of %s = %+v, want none while the wait stands", podUpdateIssue, got)
			}
			if got := q.noticesOf(tc.kind); len(got) != 0 {
				t.Fatalf("the %s notices of %s = %+v, want none while the wait stands", tc.kind, podUpdateIssue, got)
			}
			if minted := q.mintedFor(worker); len(minted) != 0 {
				t.Fatalf("the sweep minted %+v for the waiting worker, want nothing: its wait explains its quiet", minted)
			}

			q.applies("wait-news:"+wait.ID, intake.WaitNews{Token: wait.Token, Issue: podUpdateIssue, Role: worker, WaitID: wait.ID})
			q.sweepForSilence()

			if got := q.noticesOf(tc.kind); len(got) != 1 || got[0].Role != worker {
				t.Fatalf("the %s notices of %s = %+v once the wait ended, want the implementer's one", tc.kind, podUpdateIssue, got)
			}
			starts := q.starts()
			if tc.background {
				if len(starts) != 0 {
					t.Fatalf("the supervise rows of %s = %+v, want none for a background report", podUpdateIssue, starts)
				}
				return
			}
			if len(starts) != 1 || starts[0].Role != worker || !starts[0].ResumeTask {
				t.Fatalf("the supervise rows of %s = %+v once the wait ended, want the one start that sends the implementer its phase's task again", podUpdateIssue, starts)
			}
		})
	}
}

// declaresWait records a wait of role's claim as a declaration records it (record.Store.DeclareWait):
// in the session the claim runs, at the issue's run and phase and on the entry its phases row names.
func (q *quietSweep) declaresWait(role claim.Role) record.ClaimWait {
	q.t.Helper()
	issue := q.issue()
	now := q.clock.Now().UTC()
	wait := record.ClaimWait{ID: "wait-" + string(role), Project: podUpdateProject, Token: q.tokens[role], Issue: issue.Key,
		Role: role, Generation: issue.Generation, Phase: issue.Phase, PhaseEntry: silenceEntry, TakenEntry: silenceEntry,
		Session: q.claimOf(role).Session, WaitingOn: "the run on the branch's head", WakeAt: now.Add(24 * time.Hour), DeclaredAt: now}
	if err := pgx.BeginFunc(q.ctx, q.pool, func(tx pgx.Tx) error {
		_, err := q.records.DeclareWait(q.ctx, tx, wait)
		return err
	}); err != nil {
		q.t.Fatalf("declare the %s's wait: %v", role, err)
	}
	return wait
}
