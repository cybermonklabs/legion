package daemon

// A gated step that nobody registered holds no merger (dispatch://INFRA-2040 R9): every reader of
// the design gate's hold reads one rule, classify.StandingGate over the deployment's gated steps
// and the tree's gate rows (the outbox's mergerHeld, workflow.HeldByDesignGate). Such a step
// still holds the tree's work (record.TreeGate.Open), but it names no document, so nobody can be
// asked about it and no task or message can name its version. Each case drives the daemon's own
// pieces over a real PostgreSQL: the production fact handlers, the outbox's executor and its
// fence, the claim's machine, the API a worker declares its wait through, the idle release and
// its backstop, and admission's controller tick.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// noRowSpec is the spec document the tree's architect registers in these cases.
const noRowSpec = "artifact-208"

// closedGateWords are the first words of the closing message of the rigs' tree (workflow's
// tellGateMoved), the words that name its design gate closed.
const closedGateWords = "The design gate of LEGION-208 closed"

// The deployment gates the Verification document and the spec (`gates.design: [criteria, spec]`).
// The tree's architect registers the spec, a person approves it, and nobody registers the criteria
// step, so the tree's gate reads closed on that step while no step stands closed with a row. The
// issue then moves to merging, and the merger is held by nothing:
//
//   - the outbox's fence lets the merger's phase start through, and its task reaches the merger;
//   - the record shows no hold (workflow.HeldByDesignGate over the record);
//   - a silent merger is on the controller's tick list;
//   - its pod's release arms a backstop, which is taken when it comes due, and its wake reaches the
//     merger;
//   - the merger declares a wait in that turn, and the wait is recorded and stands;
//   - no task and no message the merger receives names a closed gate.
func TestNoRowHoldsNoMergerUnderAGatedStepNobodyRegistered(t *testing.T) {
	r := newWaitDeliveryRigOf(t, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Retro, Generation: 1,
		Status: workflow.PhaseStatus(phase.Retro), Rank: "U", HandedOver: true, LastDispatchSeq: 1},
		claim.RoleMerger, []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec})
	r.idleRelease()
	r.runner.backstop = testReleaseBackstop
	// The tick names a silent worker only under the bounds the boot registers (openWorkflow), and
	// only to a registered controller.
	r.w.admission.UseWorkerSilence(30*time.Minute, 4*time.Hour)
	registerProjectController(t, r.pool, r.issue.Project)

	r.applies("register-spec-1", intake.GateRegistered{Issue: r.issue.Key, Step: record.DesignStepSpec, ArtifactID: noRowSpec,
		Version: 1, DispatchVersion: 1, Since: record.EventMark{Seq: 2}})
	r.applies("spec-approved-1", intake.DispatchArtifact{Key: r.issue.Key, ArtifactID: noRowSpec, Kind: intake.DispatchArtifactApproved,
		Version: 1, Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: 3}})
	r.movesTo(phase.Merging)
	// The outbox delivers what the gate's two moves queued: their messages to the tree's workers and
	// the architect's design-approved notice, as its lanes would before the pod goes idle.
	r.drainAt(time.Now().UTC(), record.OutboxKindRoleMessage, record.OutboxKindNotice)

	gate := r.treeGate()
	if _, registered := gate.Rows[record.DesignStepCriteria]; registered || gate.Open() {
		t.Fatalf("the tree's gate = %+v, open %t; want it closed on the criteria step, which has no row", gate.Rows, gate.Open())
	}
	if held, holds := workflow.HeldByDesignGate(r.issue, claim.RoleMerger, gate); holds {
		t.Fatalf("the record holds the merger on %+v, want no hold: a gated step that nobody registered holds no merger", held)
	}

	r.enter(1, "Merge the head the review approved.")
	starts := entryStartRows(t, r.pool, r.issue.Key)
	if len(starts) != 1 || starts[0].request.Role != claim.RoleMerger || starts[0].request.Phase != phase.Merging {
		t.Fatalf("the start rows of %s = %+v, want the merger's one start at merging", r.issue.Key, starts)
	}
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("the merger's start at merging = %v, want the outbox's fence to let it through: no gated step stands closed with a row", err)
	}
	if _, err := r.pool.Exec(r.ctx, "delete from outbox where id = $1", starts[0].row.ID); err != nil {
		t.Fatalf("finish the merger's start row: %v", err)
	}
	r.settled()
	r.boot()
	merging := r.takeTask()
	r.endTurn(merging.ID)

	r.lastStepped(time.Now().Add(-2 * time.Hour))
	r.applies("controller-tick", intake.ControllerTick{})
	if silent := queuedTickNotice(t, r.pool).Silent; len(silent) != 1 || silent[0].Issue != r.issue.Key || silent[0].Role != claim.RoleMerger {
		t.Fatalf("the tick names %+v as silent, want the merger of %s: nothing holds it", silent, r.issue.Key)
	}

	r.releasesAt(time.Now().UTC().Add(time.Minute))
	armed := openBackstops(t, r.pool, r.issue.Key)
	if len(armed) != 1 || armed[0].role != claim.RoleMerger {
		t.Fatalf("the backstops after the release = %+v, want the merger's one", armed)
	}
	if resumes := r.backstopsFired(armed[0].dueAt); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeBackstop {
		t.Fatalf("the pod resumes once the backstop came due = %v, want the merger's backstop taken", resumes)
	}
	if left := openBackstops(t, r.pool, r.issue.Key); len(left) != 0 {
		t.Fatalf("the backstops once the wake tick ran = %+v, want none left unfired", left)
	}
	r.drainAt(armed[0].dueAt, record.OutboxKindSupervise)
	r.boot()
	woken := r.takeTask()

	wait := r.declareUntil("the merge queue's run on the head", time.Now().Add(2*time.Hour))
	if wait.Role != claim.RoleMerger || wait.Phase != phase.Merging {
		t.Fatalf("the wait recorded = %+v, want the merger's, in merging", wait)
	}
	r.endTurn(woken.ID)
	r.wantStanding()

	sent := r.prompts()
	if len(sent) < 2 {
		t.Fatalf("the merger was sent %+v, want its task at merging and its backstop's wake", sent)
	}
	for _, prompt := range sent {
		if strings.Contains(prompt.Message, closedGateWords) {
			t.Fatalf("the merger was sent %q, want no task that names a closed gate", prompt.Message)
		}
	}
	for _, body := range r.messagesTo(claim.RoleMerger) {
		if strings.Contains(body, closedGateWords) {
			t.Fatalf("the merger was told %q, want no message that names a closed gate", body)
		}
	}
}

// A child handed over in merging, whose old root had no gate to carry, becomes a tree of its own
// with no gate row, and its architect registers no document there: its work is past every gate it
// ran under. Its merger starts and is not held: the outbox's fence lets its start through and the
// record shows no hold, under the default deployment, which gates the spec step.
func TestNoRowHoldsNoMergerOfAChildHandedOverInMergingWithNoGateToCarry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, child = "LEGION-208", "LEGION-209"
	until := time.Now().UTC().Add(time.Hour)
	parent := root
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: "LEGION", Title: root, Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: "LEGION", Title: child, Parent: &parent, Phase: phase.Merging, Generation: 1,
				Status: workflow.PhaseStatus(phase.Merging), Rank: "B", LastDispatchSeq: 1}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/" + child,
				HeadSHA: "head-42", Generation: 1, State: record.PullRequestOpen}),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 2}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the lingering root and its child in merging: %v", err)
	}
	steps := config.DesignGateRootIssues.Steps()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps, Linger: time.Hour},
		quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true},
		factHandlers(records, "LEGION", engine, admission)...); err != nil {
		t.Fatalf("hand the child over: %v", err)
	}

	var handed *record.Issue
	var gates map[record.DesignStep]record.DesignGate
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if handed, err = records.Issue(ctx, tx, child); err != nil {
			return err
		}
		gates, err = records.GatesOf(ctx, tx, child)
		return err
	}); err != nil {
		t.Fatalf("read the handed-over child: %v", err)
	}
	if handed == nil || handed.Tree != child || handed.Phase != phase.Merging || len(gates) != 0 {
		t.Fatalf("the handed-over child = %+v with gates %+v, want its own tree, still merging, with no gate", handed, gates)
	}
	if held, holds := workflow.HeldByDesignGate(*handed, claim.RoleMerger, record.TreeGate{Steps: steps, Rows: gates}); holds {
		t.Fatalf("the record holds the child's merger on %+v, want no hold: its tree has no gate row", held)
	}

	var start *entryStartRow
	for _, row := range entryStartRows(t, pool, child) {
		if row.request.Role == claim.RoleMerger && row.request.Phase == phase.Merging {
			if start != nil {
				t.Fatalf("the hand-over queued a second start of the merger at merging, %+v", row.request)
			}
			start = &row
		}
	}
	if start == nil {
		t.Fatal("the hand-over queued no start of the merger at merging, want the carried phase's one")
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), gatedSteps: steps,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + child}, nil
		},
	}
	if err := runner.execute(ctx, start.row); err != nil {
		t.Fatalf("the child's merger start = %v, want the outbox's fence to let it through: its tree has no gate row", err)
	}
	merger := mustClaimToken(t, child, claim.RoleMerger)
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 1 || spawns[0].Spec.Claim != merger {
		t.Fatalf("the runtime spawned %+v, want the child's merger once", spawns)
	}
}

// applies applies fact as the Dispatch event id through the daemon's own fact handlers, failing the
// test on an error or a refusal.
func (r *waitDeliveryRig) applies(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", id, fact, r.w.handlers...)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %+v, %v; want it applied", id, result.Refusal, err)
	}
}

// movesTo moves the rig's issue to at, with at's own status, as a transition records it.
func (r *waitDeliveryRig) movesTo(at phase.Phase) {
	r.t.Helper()
	r.issue.Phase, r.issue.Status = at, workflow.PhaseStatus(at)
	putOutboxIssue(r.t, r.pool, r.records, r.issue)
}

// treeGate is the design gate of the rig's tree as the record holds it, under the steps the rig's
// deployment gates.
func (r *waitDeliveryRig) treeGate() record.TreeGate {
	r.t.Helper()
	var rows map[record.DesignStep]record.DesignGate
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = r.records.GatesOf(r.ctx, tx, r.issue.Tree)
		return err
	}); err != nil {
		r.t.Fatalf("read the design gate of %s: %v", r.issue.Tree, err)
	}
	return record.TreeGate{Steps: r.w.gatedSteps, Rows: rows}
}

// lastStepped records at as the moment the agent of the rig's claim last stepped, as the claim's
// activity columns hold it (record.RoleClaim.Step), the turn that step was taken in having started
// ten minutes earlier.
func (r *waitDeliveryRig) lastStepped(at time.Time) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `update claims set step_at = $2, turn_started_at = $3 where token = $1`,
		string(r.token), at.UTC(), at.UTC().Add(-10*time.Minute)); err != nil {
		r.t.Fatalf("record the last step of %s: %v", r.token, err)
	}
}

// messagesTo is the body of every role message addressed to role of the rig's issue: each the
// outbox published to the role's topic, then each it still holds.
func (r *waitDeliveryRig) messagesTo(role claim.Role) []string {
	r.t.Helper()
	token := string(mustClaimToken(r.t, r.issue.Key, role))
	var bodies []string
	for _, published := range r.runner.notices.(*outboxPublisher).publishes() {
		if published.topic == notify.RoleTopicPrefix+token {
			bodies = append(bodies, published.message)
		}
	}
	rows, err := r.pool.Query(r.ctx, `select payload->>'body' from outbox where kind = 'role_message' and issue = $1
		and payload->>'role' = $2 order by id`, r.issue.Key, token)
	if err != nil {
		r.t.Fatalf("read the role messages of %s: %v", r.issue.Key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			r.t.Fatalf("read a role message of %s: %v", r.issue.Key, err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the role messages of %s: %v", r.issue.Key, err)
	}
	return bodies
}
