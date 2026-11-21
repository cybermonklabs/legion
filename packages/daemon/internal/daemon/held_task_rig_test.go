package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	heldTaskRoot    = "LEGION-310"
	heldTaskProject = "LEGION"
)

// heldTaskRig is one root whose architect is up, over the real parts the daemon wires: the record
// store on a database of its own, the workflow engine, admission, the outbox runner, and a claim
// supervisor whose Deps.RootHears and Deps.RootTask are the production answers (workflowRuntime)
// and whose claim store is the daemon's own, so the task pending on the claim stands in
// `pending_task_deliveries` where a restart reads it.
type heldTaskRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	trees     *store.Store
	sup       *supervisor
	runner    *outbox
	conn      *fake.Conn
	token     claim.Token
	root      record.Issue
}

// heldTaskOption sets a part of a heldTaskRig up before newHeldTaskRig launches its architect,
// which is the last moment it can: each claim's machine copies the supervisor's deps when it is
// made (supervise.NewMachine), so a dep changed after the launch reaches no machine of the rig.
type heldTaskOption func(*heldTaskRig)

func newHeldTaskRig(t *testing.T, options ...heldTaskOption) *heldTaskRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: heldTaskProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	trees := outboxTreeStore(t, pool, heldTaskRoot)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = keptClaimStore{Store: trees}
	sup.deps.RootHears = (&workflowRuntime{pool: pool, records: records}).rootHears
	sup.deps.RootTask = (&workflowRuntime{pool: pool, records: records}).rootTask
	conn := fake.NewConn()
	token := mustClaimToken(t, heldTaskRoot, claim.RoleArchitect)
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	rig := &heldTaskRig{
		t: t, ctx: ctx, pool: pool, records: records, engine: engine,
		admission: admit.New(records, engine, 2, heldTaskProject, quietLogger()),
		trees:     trees, sup: sup, conn: conn, token: token,
		root: record.Issue{Key: heldTaskRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: heldTaskProject, Tree: heldTaskRoot, Title: "Root", Phase: phase.Admitted, Generation: 1,
			Status: "in_progress", Rank: "A", LastDispatchSeq: 1},
	}
	rig.runner = &outbox{
		log: quietLogger(), pool: pool, dispatchProject: heldTaskProject, records: records, supervisor: sup,
		notices: &outboxPublisher{}, handlers: []intake.Handler{engine, rig.admission}, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + heldTaskRoot}, nil
		},
	}
	rig.put(rig.root)
	for _, option := range options {
		option(rig)
	}
	rig.launchArchitect()
	return rig
}

func (r *heldTaskRig) put(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.records, issue)
}

// launchArchitect creates the root architect's claim, launches it and brings its agent up: the one
// session that goes on running across the root's park and its next run.
func (r *heldTaskRig) launchArchitect() {
	r.t.Helper()
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: heldTaskRoot,
		Issue: heldTaskRoot, Role: claim.RoleArchitect, State: supervise.StateQueued}, "")
	if err != nil {
		r.t.Fatalf("create the architect claim: %v", err)
	}
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: r.token}); err != nil {
		r.t.Fatalf("spawn the architect: %v", err)
	}
	r.bootArchitect(machine, "the architect")
	if got := machine.Claim().State; !supervise.SaidReady(got) {
		r.t.Fatalf("the architect after its ready = %s, want a claim that can be prompted", got)
	}
}

// bootArchitect brings the agent of the architect's current launch up: the shim's hello, the
// agent's registration and its ready, as the architect's pod does. who names the architect in a
// failure.
func (r *heldTaskRig) bootArchitect(machine *supervise.Machine, who string) {
	r.t.Helper()
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: launch},
		supervise.RequestRegister{Claim: r.token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for %s: %v", ev, who, err)
		}
	}
	machine.Wait()
}

// readmit is the root's next run, as the engine records one (workflow.Engine.NewRun): a new
// generation at admitted, so its architect has an issue to hear for.
func (r *heldTaskRig) readmit() {
	r.t.Helper()
	r.root.Generation++
	r.root.Phase, r.root.Status = phase.Admitted, "in_progress"
	r.put(r.root)
}

// enqueueStart queues the architect start of the root's generation, naming what the root keeps and
// claiming those rows for that row in one transaction, exactly as admission writes it, and returns
// the row's id.
func (r *heldTaskRig) enqueueStart() int64 {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		task, named, err := r.engine.ParkedDecisionTask(r.ctx, tx, r.root)
		if err != nil {
			return err
		}
		return r.engine.EnqueueNaming(r.ctx, tx, r.root.Key, r.root.Key, record.SuperviseRequest{Op: "start",
			Tree: r.root.Tree, Role: claim.RoleArchitect, Generation: r.root.Generation, Task: task, Kept: named},
			named, time.Now())
	}); err != nil {
		r.t.Fatalf("enqueue the architect start of generation %d: %v", r.root.Generation, err)
	}
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select max(id) from outbox").Scan(&id); err != nil {
		r.t.Fatalf("read the start row just enqueued: %v", err)
	}
	return id
}

// runRow runs one outbox row as its lane runs it: leased, executed, and finished when it acted.
func (r *heldTaskRig) runRow(id int64) {
	r.t.Helper()
	var row record.OutboxRow
	if err := r.pool.QueryRow(r.ctx, "select id, kind, issue, payload from outbox where id = $1",
		id).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read outbox row %d: %v", id, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	if err := r.runner.execute(r.ctx, row); err != nil {
		r.t.Fatalf("run outbox row %d: %v", row.ID, err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		r.t.Fatalf("finish outbox row %d: %v", row.ID, err)
	}
	if machine, found := r.sup.Machine(r.token); found {
		machine.Wait()
	}
}

// pending is the task the architect's machine holds now, nil for none.
func (r *heldTaskRig) pending() *supervise.Delivery {
	r.t.Helper()
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.t.Fatalf("%s has no architect claim", heldTaskRoot)
	}
	return machine.Claim().Pending
}

// storedDelivery is the task the claim store holds for the architect, nil for none: what a restart
// would find.
func (r *heldTaskRig) storedDelivery() *supervise.Delivery {
	r.t.Helper()
	stored, err := r.trees.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range stored {
		if c.Token == r.token {
			return c.Pending
		}
	}
	r.t.Fatalf("%s has no stored architect claim", heldTaskRoot)
	return nil
}

// named is what a delivery says it names of what its root keeps (supervise.Delivery.Kept).
func (r *heldTaskRig) named(d supervise.Delivery) record.KeptDecisions {
	r.t.Helper()
	var kept record.KeptDecisions
	if len(d.Kept) == 0 {
		return kept
	}
	if err := json.Unmarshal(d.Kept, &kept); err != nil {
		r.t.Fatalf("decode what the task names: %v", err)
	}
	return kept
}
