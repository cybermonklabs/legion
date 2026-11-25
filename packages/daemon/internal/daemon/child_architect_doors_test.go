package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The issues of the tree a child's architect is started in: the live root, the child each door
// brings in, an issue under the child, and an issue outside the tree a child can wait on.
const (
	doorRoot       = "LEGION-1"
	doorChild      = "LEGION-2"
	doorGrandchild = "LEGION-3"
	doorBlocker    = "LEGION-9"
)

// childArchitectRig is one daemon's workflow and supervision over a database of its own, wired as
// the daemon wires them: the Dispatch mirror, the workflow engine and admission as the handlers
// every fact is applied through (workflowRuntime's handlers); the real outbox runner over a
// supervisor whose claim store is the daemon's own, on the fake runtime; and the ready and failure
// facts each claim's machine reports, applied through the same handlers (workflowRuntime.terminal).
// Its tree is live: the root stands in implementing with its admission slot, its design gate open,
// its architect working and its implementer holding its phase's task, so admission's pass starts
// no worker of the root's. The claims of the root's roles are the store's alone: a notice the walk
// sends to the root's architect is published to its topic all the same.
type childArchitectRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	handlers  []intake.Handler
	sup       *supervisor
	rt        *fake.Runtime
	runner    *outbox
	published *outboxPublisher
	// dispatch is Dispatch as the runner reads and writes the child in it: the status a route's
	// own write left (written) or the last status row the runner wrote.
	dispatch *outboxDispatch
}

func newChildArchitectRig(t *testing.T) *childArchitectRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: doorRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: doorRoot,
		Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true, LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, root)
	approved := 1
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutGate(ctx, tx, record.DesignGate{Issue: doorRoot, ArtifactID: "artifact-1", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("open the root's design gate: %v", err)
	}
	holdSlot(t, pool, doorRoot)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", doorRoot, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree of %s: %v", doorRoot, err)
	}
	for _, c := range []supervise.Claim{
		{Token: mustClaimToken(t, doorRoot, claim.RoleArchitect), Project: "legion", Tree: doorRoot, TreeEpoch: 1, Issue: doorRoot, Role: claim.RoleArchitect,
			State: supervise.StateWorking, Generation: 1, Session: "ses-root-architect", SessionFile: "/sessions/root-architect.jsonl"},
		{Token: mustClaimToken(t, doorRoot, claim.RoleImplementer), Project: "legion", Tree: doorRoot, TreeEpoch: 1, Issue: doorRoot, Role: claim.RoleImplementer,
			State: supervise.StateWorking, Generation: 1, Session: "ses-root-implementer", SessionFile: "/sessions/root-implementer.jsonl",
			Pending: &supervise.Delivery{ID: "outbox:1", Task: "Continue root. Issue: LEGION-1. Phase: implementing.", Generation: 1,
				Phase: phase.Implementing, QueuedAt: time.Now(), DeliveredAt: time.Now(), ConfirmedAt: time.Now()}},
	} {
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("seed the claim %s: %v", c.Token, err)
		}
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	admission := admit.New(records, engine, 3, "LEGION", quietLogger())
	handlers := []intake.Handler{blockers.NewMirror(records, "LEGION"), engine, admission}
	answers := &workflowRuntime{pool: pool, records: records, handlers: handlers, log: quietLogger()}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.PhaseHolds = answers.phaseHolds
	sup.deps.RootHears = answers.rootHears
	sup.deps.RootTask = answers.rootTask
	sup.OnDurableState(answers.terminal)
	published := &outboxPublisher{}
	client := &outboxDispatch{issue: dispatch.Issue{Key: doorChild, Status: "todo"}, statusApplied: true}
	return &childArchitectRig{
		t: t, ctx: ctx, pool: pool, records: records, handlers: handlers, sup: sup, rt: rt, published: published, dispatch: client,
		runner: &outbox{
			log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, trees: st, tokens: outboxTokens{},
			project: "legion", stateDir: t.TempDir(), githubAPI: newBranchGitHub(t, nil, branchExists).url,
			dispatch: client, notices: published, handlers: handlers, now: time.Now,
			provision: func(_ context.Context, req workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + req.Issue}, nil
			},
			remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
		},
	}
}

// put records each issue as the workflow holds it.
func (r *childArchitectRig) put(issues ...record.Issue) {
	r.t.Helper()
	for _, issue := range issues {
		putOutboxIssue(r.t, r.pool, r.records, issue)
	}
}

// member is an issue of the rig's tree under parent, standing at at with status.
func (r *childArchitectRig) member(key, parent string, at phase.Phase, status, rank string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: doorRoot,
		Parent: &parent, Title: key, Phase: at, Generation: 1, Status: status, Rank: rank, LastDispatchSeq: 1}
}

// apply is one fact as the daemon applies it: through the mirror, the engine and admission, in one
// transaction keyed by its id.
func (r *childArchitectRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.handlers...)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %#v, %v", id, result, err)
	}
}

// written is a route of the daemon's own write of the child's status in Dispatch — an architect's
// release_children or rerun_child — as Dispatch answered it (issue), and the fact the route then
// applies (api's recordOwnStatusWrite): Dispatch's view of the issue, which applies to an issue the
// record does not hold, carrying the write's own event, which applies to one it holds.
func (r *childArchitectRig) written(id string, issue dispatch.Issue) {
	r.t.Helper()
	r.dispatch.issue = issue
	event := intake.OwnStatusWrite(issue)
	fact := intake.ObservedIssue(issue)
	fact.Delivered = &event
	r.apply(id, fact)
}

// drain runs the outbox as the lanes would until no row is due: each pass runs every row due when
// it starts (outbox.RunOnce), so a start held behind its issue's branch row runs on the pass after
// that row's. A row that waits or fails is retried on its backoff, which is due after the drain.
func (r *childArchitectRig) drain() {
	r.t.Helper()
	for range 8 {
		due, err := r.runner.listDue(r.ctx, "", nil, time.Now().UTC())
		if err != nil {
			r.t.Fatalf("list the due outbox rows: %v", err)
		}
		if len(due) == 0 {
			break
		}
		if err := r.runner.RunOnce(r.ctx); err != nil {
			r.t.Fatalf("run the outbox: %v", err)
		}
		for _, m := range r.sup.supervised() {
			m.Wait()
		}
	}
}

// machine is the machine of issue's claim of role, failing the test when the supervisor holds
// none.
func (r *childArchitectRig) machine(issue string, role claim.Role) *supervise.Machine {
	r.t.Helper()
	machine, found := r.sup.Machine(mustClaimToken(r.t, issue, role))
	if !found {
		r.t.Fatalf("the supervisor holds no %s claim of %s", role, issue)
	}
	return machine
}

// architectStarts is the number of starts of issue's architect still queued in the outbox. A row
// is deleted once it runs, so a check that a fact queued no start reads this before the next drain.
func (r *childArchitectRig) architectStarts(issue string) int {
	r.t.Helper()
	var starts int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'architect'`, issue).Scan(&starts); err != nil {
		r.t.Fatalf("count the architect starts of %s: %v", issue, err)
	}
	return starts
}

// boot is the launched agent of issue's claim of role saying hello, registering session and saying
// it is ready, at the launch the claim is on, over a connection of its own: the ready is the
// daemon's fact of it (workflowRuntime.terminal), which queues an architect's catch-up and sends a
// worker the task it holds.
func (r *childArchitectRig) boot(issue string, role claim.Role, session string) *supervise.Machine {
	r.t.Helper()
	machine := r.machine(issue, role)
	token, launch := machine.Claim().Token, machine.Claim().Generation
	r.sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	handleAll(r.t, machine,
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: session, SessionFile: doorSessionFile(session)},
		supervise.RequestReady{Claim: token, Generation: launch, Session: session})
	return machine
}

// noticesTo is every notice the outbox published to issue's architect topic, in order.
func (r *childArchitectRig) noticesTo(issue string) []record.Notice {
	r.t.Helper()
	topic := architectTopic(r.t, issue)
	var notices []record.Notice
	for _, published := range r.published.publishes() {
		if published.topic != topic {
			continue
		}
		notice, ok := published.payload.(record.Notice)
		if !ok {
			r.t.Fatalf("published to %s a %T, want a notice", topic, published.payload)
		}
		notices = append(notices, notice)
	}
	return notices
}

// catchUpKeys is the key of every issue a catch-up names, in its order: those it lists with their
// fields, then the done ones it names by key alone.
func catchUpKeys(notice record.Notice) []string {
	if notice.CatchUp == nil {
		return nil
	}
	keys := make([]string, 0, len(notice.CatchUp.Issues)+len(notice.CatchUp.Done))
	for _, listed := range notice.CatchUp.Issues {
		keys = append(keys, listed.Key)
	}
	return append(keys, notice.CatchUp.Done...)
}

// wantChildsArchitectToldItsIssues is the child's architect claim on the child, in the root's
// tree, after the rig drained what its door wrote; booted, its first published notice is its
// catch-up, naming the child and every issue under it (want), at the child's generation.
func (r *childArchitectRig) wantChildsArchitectToldItsIssues(want []string) {
	r.t.Helper()
	r.drain()
	if got := r.machine(doorChild, claim.RoleArchitect).Claim(); got.Issue != doorChild || got.Tree != doorRoot || got.Role != claim.RoleArchitect {
		r.t.Fatalf("the child's architect claim = %+v, want the architect of %s in the tree of %s", got, doorChild, doorRoot)
	}
	if told := r.noticesTo(doorChild); len(told) != 0 {
		r.t.Fatalf("before its ready the child's architect was published %+v, want nothing", told)
	}
	r.boot(doorChild, claim.RoleArchitect, "ses-child-architect")
	r.drain()
	told := r.noticesTo(doorChild)
	if len(told) == 0 || told[0].Kind != "catch-up" {
		r.t.Fatalf("the child's architect was published %+v, want its catch-up first", told)
	}
	child := readIssue(r.t, r.pool, doorChild)
	if got := catchUpKeys(told[0]); !slices.Equal(got, want) || told[0].CatchUp.Generation != child.Generation {
		r.t.Fatalf("the child's catch-up lists %v at generation %d, want %v at the child's generation %d", got, told[0].CatchUp.Generation, want, child.Generation)
	}
}

// Every door a child comes through starts its own architect when it starts the child's first
// phase, and that architect is told what it owns at its launch. The child is released by the
// architect above it (release_children's own write of todo), run again by that architect
// (rerun_child's write of todo on a done child), set todo by a person under a live tree, or freed
// by the close of the issue it waited on. Each door's facts go through the daemon's own handlers,
// the rows they write through the real outbox runner, and the architect's launch through its
// claim's machine: the start creates and spawns the child's architect claim, on the child and in
// the root's tree, and the first notice published to that architect is its catch-up, naming the
// child and every issue under it. A child that waits for the issue it is held by gets no architect
// while it waits: the architect starts with the child's first phase and not with its entry.
func TestEveryDoorStartsTheChildsArchitect(t *testing.T) {
	t.Parallel()
	t.Run("release_children", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		rig.written("release", childAt("todo", 2))
		rig.wantChildsArchitectToldItsIssues([]string{doorChild})
	})

	t.Run("rerun_child", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		rig.put(rig.member(doorChild, doorRoot, phase.Done, "done", "V"), rig.member(doorGrandchild, doorChild, phase.Done, "done", "W"))
		rig.written("rerun", childAt("todo", 2))
		rig.wantChildsArchitectToldItsIssues([]string{doorChild, doorGrandchild})
	})

	t.Run("a person's todo under a live tree", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		rig.apply("person-todo", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: doorChild, Parent: doorRoot, Rank: "V"})
		rig.wantChildsArchitectToldItsIssues([]string{doorChild})
	})

	t.Run("a blocker's close", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		rig.apply("blocker-works", intake.DispatchIssue{Key: doorBlocker, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "in_progress",
			Title: doorBlocker, Rank: "Z"})
		rig.apply("held-child-todo", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: doorChild, Parent: doorRoot, Rank: "V", Blockers: []string{doorBlocker}})
		if starts := rig.architectStarts(doorChild); starts != 0 {
			t.Fatalf("the held child's entry queued %d architect starts, want none until its first phase starts", starts)
		}
		rig.drain()
		if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Admitted {
			t.Fatalf("the held child stands at %s while %s works, want admitted", got.Phase, doorBlocker)
		}
		if _, found := rig.sup.Machine(mustClaimToken(t, doorChild, claim.RoleArchitect)); found {
			t.Fatal("the held child has an architect claim while it waits, want none until its first phase starts")
		}
		rig.apply("blocker-done", intake.DispatchIssue{Key: doorBlocker, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.closed", Status: "done",
			Title: doorBlocker, Rank: "Z"})
		if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Planning {
			t.Fatalf("the child stands at %s once %s is done, want planning", got.Phase, doorBlocker)
		}
		rig.wantChildsArchitectToldItsIssues([]string{doorChild})
	})
}

// childAt is the child as Dispatch shows it at status, at Dispatch's sequence seq: under the
// root, on the repository the project serves.
func childAt(status string, seq int64) dispatch.Issue {
	return dispatch.Issue{Key: doorChild, Status: status, Title: doorChild, Parent: new(doorRoot), Rank: "V", ResolvedRepo: "acme/widgets", LastSeq: seq}
}

// doorSessionFile is the resume path the rig's agent of session registers.
func doorSessionFile(session string) string { return fmt.Sprintf("/sessions/%s.jsonl", session) }
