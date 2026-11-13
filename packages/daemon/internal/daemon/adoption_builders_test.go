package daemon

// Every role of an issue works one jj workspace, and a task that adopts it runs `jj new` under
// whichever role is working there. So the daemon adopts the working copy only for a task that names
// a phase, and holds such a task while another claim of the issue is in a turn
// (cybermonklabs/legion#2539). That rule is one predicate, supervise's adopts, and it reads one
// field: the delivery's Phase. No builder of a task decides whether its task adopts. Each writes an
// outbox row, and the outbox copies the row's Phase onto the delivery it hands the claim (the
// outbox's start). Whether the rule holds for a kind of task is therefore whether that kind's
// builder writes the phase it should and the outbox carries it on: a builder that stamped a phase on
// a look or on an architect's start would adopt under a working role, and a phase task that lost its
// phase on the way would let its commits land under whichever App adopted last
// (dispatch://INFRA-2695).
//
// So each builder is pinned where it writes its row. In every case the daemon's own builder writes
// the row, through the fact the daemon applies or the pass it runs, and the real outbox executes
// that row against machines wired as the daemon wires its supervisor (adoptingAsTheDaemonDoes): an
// App identity per role, and the daemon's own answer to which claim of an issue is in a turn. An
// adoption is the fake runtime's record of it, attributed to the claim its locator names. The pod
// update round's asks are pinned beside the fleet round
// (TestPodUpdateTheRoundsAsksAdoptNothingWhileTheImplementerIsInATurn).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// adoptionProject is the Dispatch project of every issue here; its claims, its trees and its
// runtime are keyed by the normalized token "legion" (claim.ProjectToken).
const adoptionProject = "LEGION"

// The issues the cases drive, one tree each. The production check's look runs under checkRoot's
// closed root.
const (
	adoptionWaitingRoot   = "LEGION-2602"
	adoptionLingeringRoot = "LEGION-2603"
	adoptionMember        = "LEGION-2604"
	adoptionTestingRoot   = "LEGION-2605"
	adoptionPlanningRoot  = "LEGION-2606"
)

// adoptionRig is one daemon's workflow and supervision over a database of its own: the record
// store, the workflow engine and admission as the handlers every fact is applied through, the
// outbox runner, and a supervisor whose claim store is the daemon's own, so the workflow reads each
// claim and its pending task from Postgres where a live daemon leaves them. The supervisor is wired
// as daemon.go wires it: the App identity and the turn answer (adoptingAsTheDaemonDoes), the phase
// check (Deps.PhaseHolds), the root architect's two answers (Deps.RootHears, Deps.RootTask), and
// the ready and failure facts each claim's machine reports (workflowRuntime.terminal).
type adoptionRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	records   record.Store
	st        *store.Store
	engine    *workflow.Engine
	admission *admit.Admission
	sup       *supervisor
	rt        *fake.Runtime
	runner    *outbox
	// workers is the connection each claim's agent is reached over, made when the rig first boots
	// or restores the claim; it records every prompt the daemon sends that agent.
	workers map[claim.Token]*fake.Conn
	// ran is the newest start row the rig has run.
	ran int64
}

func newAdoptionRig(t *testing.T) *adoptionRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: adoptionProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 2, adoptionProject, quietLogger())
	answers := &workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine, admission}, log: quietLogger()}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.PhaseHolds = answers.phaseHolds
	sup.deps.RootHears = answers.rootHears
	sup.deps.RootTask = answers.rootTask
	adoptingAsTheDaemonDoes(sup)
	sup.OnDurableState(answers.terminal)
	return &adoptionRig{
		t: t, ctx: ctx, pool: pool, records: records, st: st, engine: engine, admission: admission, sup: sup, rt: rt,
		workers: map[claim.Token]*fake.Conn{},
		runner: &outbox{
			log: quietLogger(), pool: pool, dispatchProject: adoptionProject, records: records, supervisor: sup,
			handlers: []intake.Handler{engine, admission}, trees: st, tokens: outboxTokens{}, project: "legion",
			stateDir: t.TempDir(), now: time.Now,
			provision: func(_ context.Context, req workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + req.Issue}, nil
			},
		},
	}
}

func (r *adoptionRig) dsn() string { return r.pool.Config().ConnString() }

func (r *adoptionRig) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	return mustClaimToken(r.t, issue, role)
}

// put records each issue as the workflow holds it.
func (r *adoptionRig) put(issues ...record.Issue) {
	r.t.Helper()
	for _, issue := range issues {
		putOutboxIssue(r.t, r.pool, r.records, issue)
	}
}

// openTree opens root's tree lifecycle as admission opened it when the tree first ran, which a
// tree keeps through its linger: every claim of it is bound to that lifecycle.
func (r *adoptionRig) openTree(root string) {
	r.t.Helper()
	if _, err := r.st.OpenTreeLifecycle(r.ctx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open the lifecycle of %s: %v", root, err)
	}
}

// adoptionSession is the session the agent of issue's role registers and the file it keeps it in:
// the one a live claim is seeded with, and the one a claim that keeps none boots on.
func adoptionSession(issue string, role claim.Role) (session, file string) {
	return "ses_" + issue + "_" + string(role), "/sessions/" + issue + "-" + string(role) + ".jsonl"
}

// live is the claim of issue's role as a running agent leaves it between turns: launched in tree,
// registered and ready on its session, holding nothing.
func (r *adoptionRig) live(tree, issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	token := r.token(issue, role)
	session, file := adoptionSession(issue, role)
	return supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role, Generation: 1,
		State: supervise.StateReady, Session: session, SessionFile: file,
		PluginContract: r.sup.deps.PluginContract, Locator: workingPane(token)}
}

// suspended is the claim of issue's role as a close or a linger leaves it: suspended by the daemon
// with its session kept, so the start that brings it back resumes that session.
func (r *adoptionRig) suspended(tree, issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	c := r.live(tree, issue, role)
	c.State, c.SuspendedBy, c.Locator = supervise.StateSuspended, supervise.SuspendedByDaemon, nil
	return c
}

// admitClaims writes each claim as the daemon admits one, bound to its tree's lifecycle.
func (r *adoptionRig) admitClaims(claims ...supervise.Claim) {
	r.t.Helper()
	for _, c := range claims {
		if _, err := r.st.AdmitClaim(r.ctx, c); err != nil {
			r.t.Fatalf("admit %s: %v", c.Token, err)
		}
	}
}

// restoreClaims builds the machine of every claim the store holds, as a boot restores them, each
// with its agent's connection.
func (r *adoptionRig) restoreClaims() {
	r.t.Helper()
	claims, err := r.st.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the seeded claims: %v", err)
	}
	if _, err := r.sup.restore(r.ctx, claims); err != nil {
		r.t.Fatalf("restore the seeded claims: %v", err)
	}
	for _, c := range claims {
		r.worker(c.Token)
	}
}

// worker is the connection the agent of token's claim is reached over, registered the first time
// it is asked for.
func (r *adoptionRig) worker(token claim.Token) *fake.Conn {
	if conn, ok := r.workers[token]; ok {
		return conn
	}
	conn := fake.NewConn()
	r.sup.deps.Conns.(*fake.Conns).Register(token, conn)
	r.workers[token] = conn
	return conn
}

// apply is one fact as the daemon applies it: through the engine and admission, in one transaction
// keyed by its id.
func (r *adoptionRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.engine, r.admission)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %#v, %v", id, result, err)
	}
}

// run runs every start row the outbox holds past the last one the rig ran, oldest first, as the
// supervise lane runs one — leased, executed and finished — and waits for every send that started.
// These are the rows the builders wrote; nothing here writes one.
func (r *adoptionRig) run() {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and id > $1 order by id`, r.ran)
	if err != nil {
		r.t.Fatalf("read the start rows: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		return out, row.Scan(&out.ID, &out.Kind, &out.Issue, &out.Payload)
	})
	if err != nil {
		r.t.Fatalf("scan the start rows: %v", err)
	}
	if len(queued) == 0 {
		r.t.Fatalf("no start row past row %d: the builder wrote none", r.ran)
	}
	for _, row := range queued {
		row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
		if err := r.runner.execute(r.ctx, row); err != nil {
			r.t.Fatalf("run the start row %d of %s: %v", row.ID, row.Issue, err)
		}
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			r.t.Fatalf("finish the start row %d: %v", row.ID, err)
		}
		r.ran = row.ID
	}
	r.settle()
}

// settle waits for every send any machine started to have had its outcome handled.
func (r *adoptionRig) settle() {
	for _, m := range r.sup.supervised() {
		m.Wait()
	}
}

func (r *adoptionRig) machine(issue string, role claim.Role) *supervise.Machine {
	r.t.Helper()
	m, ok := r.sup.Machine(r.token(issue, role))
	if !ok {
		r.t.Fatalf("%s holds no claim of its %s", issue, role)
	}
	return m
}

func (r *adoptionRig) claimOf(issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	return r.machine(issue, role).Claim()
}

// handle hands the claim of issue's role ev, as a source that hands a machine its event directly
// does, and waits for the send it may start.
func (r *adoptionRig) handle(issue string, role claim.Role, ev supervise.Event) {
	r.t.Helper()
	m := r.machine(issue, role)
	if err := m.Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("%s's %s handled %T: %v", issue, role, ev, err)
	}
	m.Wait()
}

// boot is the agent of issue's role coming up on the launch its claim stands at: the shim's hello,
// the agent's registration and its ready, on the session the claim keeps, or a fresh one for a
// claim that has none. The ready is what sends a task the claim holds.
func (r *adoptionRig) boot(issue string, role claim.Role) {
	r.t.Helper()
	token := r.token(issue, role)
	r.worker(token)
	c := r.claimOf(issue, role)
	session, file := c.Session, c.SessionFile
	if session == "" {
		session, file = adoptionSession(issue, role)
	}
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: c.Generation},
		supervise.RequestRegister{Claim: token, Generation: c.Generation, Session: session, SessionFile: file},
		supervise.RequestReady{Claim: token, Generation: c.Generation, Session: session},
	} {
		r.handle(issue, role, ev)
	}
	if got := r.claimOf(issue, role).State; !supervise.SaidReady(got) {
		r.t.Fatalf("%s's %s after its ready is %s, want a claim that can be prompted", issue, role, got)
	}
}

// delivered is the delivery id of every prompt the agent of issue's role was sent, in order.
func (r *adoptionRig) delivered(issue string, role claim.Role) []string {
	r.t.Helper()
	conn, ok := r.workers[r.token(issue, role)]
	if !ok {
		return nil
	}
	var ids []string
	for _, prompt := range conn.Prompts() {
		ids = append(ids, prompt.DeliveryID)
	}
	return ids
}

// wantSent holds issue's role to one prompt, of the task delivered as id, and to exactly the
// adoptions named for its claim, by the App git name each was made as: none for a task that names
// no phase, its own App once for a phase task.
func (r *adoptionRig) wantSent(issue string, role claim.Role, id string, adopted ...string) {
	r.t.Helper()
	if got := r.delivered(issue, role); !slices.Equal(got, []string{id}) {
		r.t.Errorf("%s's %s was prompted with %v, want its task %s once", issue, role, got, id)
	}
	if made := adoptionsOf(r.rt, r.token(issue, role)); !slices.Equal(made, adopted) {
		r.t.Errorf("%s's %s had the working copy adopted as %v for its task %s, want %v", issue, role, made, id, adopted)
	}
}

// wantNothingSent holds issue's role to nothing at all: no task held, no prompt, no adoption made
// for its claim.
func (r *adoptionRig) wantNothingSent(issue string, role claim.Role) {
	r.t.Helper()
	if held := r.claimOf(issue, role).Pending; held != nil {
		r.t.Errorf("%s's %s holds %+v, want no task", issue, role, held)
	}
	if got := r.delivered(issue, role); len(got) != 0 {
		r.t.Errorf("%s's %s was prompted with %v, want nothing sent", issue, role, got)
	}
	if made := adoptionsOf(r.rt, r.token(issue, role)); len(made) != 0 {
		r.t.Errorf("%s's %s had the working copy adopted as %v, want no adoption", issue, role, made)
	}
}

// heldTask is the task the claim of issue's role holds, which the start row the rig ran gave it.
func (r *adoptionRig) heldTask(issue string, role claim.Role) supervise.Delivery {
	r.t.Helper()
	held := r.claimOf(issue, role).Pending
	if held == nil {
		r.t.Fatalf("%s's %s holds no task after the builder's start row ran", issue, role)
	}
	return *held
}

// TestOnlyThePhaseOwnersTaskAdoptsTheWorkingCopy pins adopts where each of five builders writes
// its row (see the file's comment): a look and the root architect's start adopt nothing, the three
// phase tasks are adopted once each by the phase owner alone, and a phase task waits, unadopted,
// while another role of the issue is in a turn.
func TestOnlyThePhaseOwnersTaskAdoptsTheWorkingCopy(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(r *adoptionRig)
	}{
		{"a production check's look reaches its role with no adoption", aLookAdoptsNothing},
		{"the root architect's start from admission reaches the architect with no adoption", theArchitectsStartAdoptsNothing},
		{"a mid-phase re-admission adopts for the phase owner alone", aReadmissionAdoptsForThePhaseOwnerAlone},
		{"the resume task of a phase owner that comes back ready is adopted once", aReadyOwnersResumeTaskIsAdoptedOnce},
		{"a transition's phase start waits out another role's turn and is then adopted once", aPhaseStartWaitsOutATurn},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(newAdoptionRig(t)) })
	}
}

// aLookAdoptsNothing: a production check's look is a start with no phase (workflow's startLook),
// fenced on the check and its look rather than on a phase, because it runs after the issue's close.
// The checking role reads production with the working copy as the issue's last phase left it; it
// commits nothing for a phase, so nothing adopts. The look here is the wake a pending report named,
// coming due as the fact its check_wake row applies (the outbox's checkWake), under a closed root
// whose linger suspended the implementer the check records: the look's start resumes it and its
// ready sends the look.
func aLookAdoptsNothing(r *adoptionRig) {
	root := checkRoot()
	check := openCheck(root, "a1b2c3d4")
	check.LookOpen = false
	r.put(root)
	putProductionCheck(r.t, r.pool, r.records, check)
	r.openTree(root.Key)
	r.admitClaims(r.suspended(root.Key, root.Key, claim.RoleImplementer))
	r.restoreClaims()

	r.apply("check-wake", intake.CheckWakeDue{Issue: root.Key, Look: 1, OpenedAt: check.OpenedAt, Reason: "the deploy has not carried the merge"})
	r.run()
	r.boot(root.Key, claim.RoleImplementer)

	look := r.heldTask(root.Key, claim.RoleImplementer)
	if look.Phase != "" || look.CheckLook.Look != 2 || !strings.HasPrefix(look.Task, "Production check of ") {
		r.t.Fatalf("the implementer holds %+v, want look 2 of the check, naming no phase", look)
	}
	r.wantSent(root.Key, claim.RoleImplementer, look.ID)
}

// theArchitectsStartAdoptsNothing: the start admission writes for a tree's root architect names no
// phase, since an architect works no phase of its own. That start carries a task only when the root
// keeps a decision nobody in its tree heard (ParkedDecisionTask), so a person answers an ask of the
// root while it waits in `todo` for its slot; the controller's tick then admits it, the architect's
// start launches the architect, and its ready sends the task naming that decision.
func theArchitectsStartAdoptsNothing(r *adoptionRig) {
	const ask = "ask-1"
	r.put(record.Issue{Key: adoptionWaitingRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: adoptionProject, Tree: adoptionWaitingRoot, Title: "root", Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 5})
	r.apply("ask-answered", intake.DispatchAskClosed{Key: adoptionWaitingRoot, Ask: ask, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: 11}})
	r.apply("tick", intake.ControllerTick{})
	r.run()
	r.boot(adoptionWaitingRoot, claim.RoleArchitect)

	start := r.heldTask(adoptionWaitingRoot, claim.RoleArchitect)
	if ref := fmt.Sprintf("dispatch://%s/ask/%s", adoptionWaitingRoot, ask); start.Phase != "" || !strings.Contains(start.Task, ref) {
		r.t.Fatalf("the architect holds %+v, want admission's start naming %s and no phase", start, ref)
	}
	r.wantSent(adoptionWaitingRoot, claim.RoleArchitect, start.ID)
}

// aReadmissionAdoptsForThePhaseOwnerAlone: a person sets a closed root back to `todo` inside its
// linger, and the promotion that re-admits it brings back every role of its mid-phase child that
// has run (admit's startMidPhaseMembers). The owner of the child's phase is given the task that
// carries the phase on, on a start that names the phase, and adopts for it. Every other role comes
// back on a start that names the phase too, but carries no task, so nothing is sent to it and
// nothing adopted for it: such a role is brought back so that a person can talk to it, and the
// commits of the phase are the owner's.
func aReadmissionAdoptsForThePhaseOwnerAlone(r *adoptionRig) {
	others := []claim.Role{claim.RolePlanner, claim.RoleTester}
	until := time.Now().Add(time.Hour)
	root := record.Issue{Key: adoptionLingeringRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: adoptionProject, Tree: adoptionLingeringRoot, Title: "Workflow", Phase: phase.Done, Generation: 3, Status: "done",
		Rank: "U", LingerUntil: &until, LastDispatchSeq: 5, HandedOver: true}
	parent := root.Key
	r.put(root, record.Issue{Key: adoptionMember, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: adoptionProject, Tree: root.Key, Parent: &parent, Title: "Child", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "V"})
	r.openTree(root.Key)
	r.admitClaims(r.suspended(root.Key, root.Key, claim.RoleArchitect), r.suspended(root.Key, adoptionMember, claim.RoleImplementer))
	for _, role := range others {
		r.admitClaims(r.suspended(root.Key, adoptionMember, role))
	}
	r.restoreClaims()

	r.apply("todo-again", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated",
		Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: true})
	// The owner's start waits for the child's own admission slot (workflow.StartOrOwe), which the
	// pass after the one that wrote it grants, as the controller's next tick does.
	r.apply("tick-after-the-readmission", intake.ControllerTick{})
	r.run()
	r.boot(adoptionMember, claim.RoleImplementer)
	for _, role := range others {
		if resumed := len(resumesOf(r.rt, r.token(adoptionMember, role))); resumed != 1 {
			r.t.Fatalf("the re-admission resumed the child's %s %d times, want its session brought back once by its own start", role, resumed)
		}
		r.boot(adoptionMember, role)
	}

	member := readIssue(r.t, r.pool, adoptionMember)
	resume := r.heldTask(adoptionMember, claim.RoleImplementer)
	if resume.Task != workflow.ResumePhaseTask(*member) {
		r.t.Fatalf("the child's implementer holds %+v, want its phase's resume task %q", resume, workflow.ResumePhaseTask(*member))
	}
	r.wantSent(adoptionMember, claim.RoleImplementer, resume.ID, "legion-implementer[bot]")
	for _, role := range others {
		r.wantNothingSent(adoptionMember, role)
	}
	if made := r.rt.CallsOf("AdoptWorkingCopy"); len(made) != 1 {
		r.t.Errorf("the re-admission adopted the working copy %d times, want once, for the phase owner's task", len(made))
	}
}

// aReadyOwnersResumeTaskIsAdoptedOnce: a phase owner whose claim becomes ready with nothing pending
// is given its phase's task again (workflow's readyWorkerTask), on a start that names the phase, so
// that task adopts. The tester's process dies mid-testing and the machine relaunches its session;
// the relaunched agent's ready is the claim becoming ready with nothing pending, which the daemon
// applies as a fact (workflowRuntime.terminal) read against the claim the store holds. The
// implementer, idle on the same issue, adopts nothing.
func aReadyOwnersResumeTaskIsAdoptedOnce(r *adoptionRig) {
	issue := record.Issue{Key: adoptionTestingRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: adoptionProject, Tree: adoptionTestingRoot, Title: "Workflow", Phase: phase.Testing, Generation: 1,
		Status: "in_progress", Rank: "U", HandedOver: true}
	seedWorkflowTree(r.t, r.dsn(), "legion", issue, r.live(issue.Key, issue.Key, claim.RoleImplementer),
		r.live(issue.Key, issue.Key, claim.RoleTester))
	r.restoreClaims()

	tester := r.claimOf(issue.Key, claim.RoleTester)
	r.handle(issue.Key, claim.RoleTester, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *tester.Locator, Kind: runtime.Gone, At: time.Now()}})
	r.boot(issue.Key, claim.RoleTester)
	if resumed := len(resumesOf(r.rt, tester.Token)); resumed != 1 {
		r.t.Fatalf("the tester's death relaunched its session %d times, want once", resumed)
	}
	start := theOneStartRow(r.t, r.pool, issue.Key)
	if start.role != claim.RoleTester || start.stands != phase.Testing || start.task != workflow.ResumePhaseTask(issue) {
		r.t.Fatalf("the ready wrote the start %+v, want the tester's testing resume task", start)
	}
	r.run()

	resume := r.heldTask(issue.Key, claim.RoleTester)
	if want := fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.row.ID); resume.ID != want {
		r.t.Fatalf("the tester holds %+v, want the task of the ready's start row, %s", resume, want)
	}
	r.wantSent(issue.Key, claim.RoleTester, resume.ID, "legion-tester[bot]")
	r.wantNothingSent(issue.Key, claim.RoleImplementer)
}

// aPhaseStartWaitsOutATurn: a transition starts its phase's worker on one row that carries the
// task and names the phase (Engine.started, through workflow.StartOrOwe), so that task adopts, and
// because it adopts it waits while another claim of the issue is in a turn (supervise's
// sendPending). The planner completes planning from inside its turn, as the handoff route applies
// that completion, and the implementer the transition creates comes up while that turn still runs:
// its task is held, unsent and unadopted, until the planner's turn ends, and then adopted once, as
// the implementer's App.
func aPhaseStartWaitsOutATurn(r *adoptionRig) {
	issue := record.Issue{Key: adoptionPlanningRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: adoptionProject, Tree: adoptionPlanningRoot, Title: "Workflow", Phase: phase.Planning, Generation: 1,
		Status: "in_progress", Rank: "U", HandedOver: true}
	planner := r.token(issue.Key, claim.RolePlanner)
	seedWorkflowTree(r.t, r.dsn(), "legion", issue, r.live(issue.Key, issue.Key, claim.RolePlanner))
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RolePlanner, Claim: planner})
	}); err != nil {
		r.t.Fatalf("record the planner's phases row: %v", err)
	}
	r.restoreClaims()

	// The planner is in the turn it completes planning from.
	r.handle(issue.Key, claim.RolePlanner, supervise.StreamTurnStart{Claim: planner})
	r.apply("plan-complete", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RolePlanner, Claim: planner,
		Summary: "planned", HandoffRecord: "plan-1", HandoffVersion: 1})
	start := theOneStartRow(r.t, r.pool, issue.Key)
	if start.role != claim.RoleImplementer || start.stands != phase.Implementing || start.task == "" {
		r.t.Fatalf("the planner's completion wrote the start %+v, want the implementer's, carrying its implementing task", start)
	}
	r.run()
	r.boot(issue.Key, claim.RoleImplementer)

	task := r.heldTask(issue.Key, claim.RoleImplementer)
	if want := fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.row.ID); task.ID != want {
		r.t.Fatalf("the implementer holds %+v, want the task of the transition's start row, %s", task, want)
	}
	if got, made := r.delivered(issue.Key, claim.RoleImplementer), adoptionsOf(r.rt, r.token(issue.Key, claim.RoleImplementer)); len(got) != 0 || len(made) != 0 {
		r.t.Errorf("while the planner was in a turn the implementer was prompted with %v and had the working copy adopted as %v; want its phase task held, unsent and unadopted, until that turn ends",
			got, made)
	}

	// The turn ends as Oh My Pi reports it, through the inbox the worker's stream feeds, which the
	// daemon tells the issue's other claims of (wakeIssue).
	r.sup.post(planner, supervise.StreamTurnEnd{Claim: planner})
	testwait.Eventually(r.t, "the implementer's phase task to be sent once the planner's turn ended", func() bool {
		return len(r.delivered(issue.Key, claim.RoleImplementer)) > 0
	})
	r.settle()
	r.wantSent(issue.Key, claim.RoleImplementer, task.ID, "legion-implementer[bot]")
	if made := adoptionsOf(r.rt, planner); len(made) != 0 {
		r.t.Errorf("the planner had the working copy adopted as %v, want no adoption: it was sent no task", made)
	}
}
