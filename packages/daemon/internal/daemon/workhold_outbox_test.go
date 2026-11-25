package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The work hold (`gates.hold: work`, steps 1 to 7 of its spec under `work`) and
// the merge hold's one fix (decision 13) as the outbox runs the rows the workflow writes for them:
// the close's messages and stops, every start of a phase worker of a held tree, and the opening's
// starts. Each test runs the daemon's own engine, admission and outbox over a database of its own,
// with a supervisor whose claims live in the daemon's real store and whose runtime is the fake
// one, on a clock the test holds (heldClock): every deadline the daemon names is a timer the test
// fires.

// holdSentence is the sentence the close message adds under `work` after today's first one, which
// every opening task repeats after that first sentence (spec step 2). It is written out here, never
// read from the code under test.
const holdSentence = "The deployment holds the tree's work until every gated step stands open again: no phase " +
	"starts, and the daemon stops each phase worker, with its session kept, when its turn ends or 2 minutes after this message."

// holdGrace is how long after the close message reached a claim its turn is cut (spec step 3).
const holdGrace = 2 * time.Minute

// holdTree is the tree a work-hold rig holds: its root, which is its own tree and its one member,
// the Dispatch project of its issues, the document of its gated spec and the version a person
// approved, the phase the root stands in, the deployment's hold, the steps the deployment gates,
// the spec alone where steps is nil, and the version of the spec the root's phase holder and its
// architect took (took).
//
// By default took is the version whose approval opens the gate in the rig's cases. That is above
// the approved version, a state no writer of the told rows makes: each holder took that version,
// so the record owes it no news of that approval (record.OwedNews). The default stays so because
// the tester's opening-text cases pin the opening task word for word, with no news after its work
// (TestWorkHoldStartsAStopMadeWhileTheOpeningCommitsWithTheOpeningsTask among them). So in a
// default case the merger's start waits for no news (mergerWaitsForTheDesignGate), a start carries
// no news, and the opening tells each live holder the open message with the words that nothing
// its issue holds changed since that version (openingDelivery, openedTo).
//
// A case that holds the news an opening's start carries seeds took at the approved version
// (withNews), as a phase's first start records it (record.Store.BaselineTold). Each approval of a
// later version then owes the holders its news, and the design_changes row that approval queues
// records what changed (outboxDispatch answers no changed section). The merger's start waits until
// that is recorded, and a start carries that news after its work (heldNews).
type holdTree struct {
	key, project, document string
	approved, took         int
	at                     phase.Phase
	hold                   config.Hold
	steps                  []record.DesignStep
}

// withNews is tree with its holders' told rows at the approved version, so each approval of a later
// version owes them its news (holdTree).
func (tree holdTree) withNews() holdTree {
	tree.took = tree.approved
	return tree
}

// legionTree is LEGION-208 standing in at under hold, its spec (artifact-208) approved at version
// 3, and version 4 taken by its holders.
func legionTree(at phase.Phase, hold config.Hold) holdTree {
	return holdTree{key: "LEGION-208", project: "LEGION", document: "artifact-208", approved: 3, took: 4, at: at, hold: hold}
}

// carriedTree is F3's root: ACME-2694 implementing, its spec (F3's document) approved at version
// 24, which Dispatch carried onto version 46 after an exact undo, version 46 taken by its holders.
func carriedTree() holdTree {
	return holdTree{key: "ACME-2694", project: "ACME", document: "aec8fb48-fd46-458a-99a3-a1b199a46b38", approved: 24, took: 46,
		at: phase.Implementing, hold: config.HoldWork}
}

type workHoldRig struct {
	t            *testing.T
	ctx          context.Context
	tree         holdTree
	tokenProject string
	pool         *store.Pool
	records      record.Store
	trees        *store.Store
	engine       *workflow.Engine
	admission    *admit.Admission
	sup          *supervisor
	rt           *fake.Runtime
	clock        *heldClock
	publisher    *holderPublisher
	runner       *outbox
	conns        map[claim.Role]*fake.Conn
	facts        int
	// refuseHoldRead, where set, refuses every read of a tree's work hold row while it is true, by
	// the claims of each supervisor a restart builds (supervise, refusedHoldRead).
	refuseHoldRead *atomic.Bool
}

func newWorkHoldRig(t *testing.T, tree holdTree) *workHoldRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	tokenProject, err := claim.ProjectToken(tree.project)
	if err != nil {
		t.Fatal(err)
	}
	approved := tree.approved
	if tree.steps == nil {
		tree.steps = config.DesignGateRootIssues.Steps()
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: tree.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: tree.project, Tree: tree.key, Title: "a held root", Phase: tree.at, Generation: 1, Status: workflow.PhaseStatus(tree.at),
			Rank: "U", LastDispatchSeq: 5}); err != nil {
			return err
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: tree.key, Step: record.DesignStepSpec, ArtifactID: tree.document,
			LatestVersion: tree.approved, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true}); err != nil {
			return err
		}
		// The root's phase holder and its architect took version took of the spec (holdTree).
		for _, audience := range []record.DesignAudience{record.AudienceHolder, record.AudienceArchitect} {
			if err := records.RaiseTold(ctx, tx, tree.key, audience, []record.NewsItem{{Step: record.DesignStepSpec, To: tree.took}},
				time.Now().UTC()); err != nil {
				return err
			}
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: tree.key, Repo: "acme/widgets",
			Number: 42, Branch: "legion/" + tree.key, HeadSHA: "head1", Failing: []string{}})
	}); err != nil {
		t.Fatalf("seed the held tree: %v", err)
	}
	holdSlot(t, pool, tree.key)
	trees, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(trees.Close)
	if _, err := trees.OpenTreeLifecycle(ctx, tokenProject, tree.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree of %s: %v", tree.key, err)
	}
	clock := &heldClock{at: time.Now().UTC().Truncate(time.Second)}
	r := &workHoldRig{t: t, ctx: ctx, tree: tree, tokenProject: tokenProject, pool: pool, records: records, trees: trees,
		publisher: &holderPublisher{}, conns: map[claim.Role]*fake.Conn{}}
	r.supervise(clock)
	r.runner = &outbox{
		dispatchProject: tree.project, pool: pool, records: records, supervisor: r.sup, trees: trees, tokens: outboxTokens{},
		project: tokenProject, stateDir: t.TempDir(), notices: r.publisher, dispatch: &outboxDispatch{},
		gatedSteps: tree.steps, now: func() time.Time { return r.clock.Now() }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + tree.key}, nil
		},
	}
	r.deploy(tree.hold)
	return r
}

// deploy builds the rig's engine and admission under hold, as a boot builds them from the
// deployment's config, and has the outbox run under hold with them. The engine reads the rig's
// clock, as the runner does, so the clock a restart puts in its place (restart) is the one every
// later fact reads.
func (r *workHoldRig) deploy(hold config.Hold) {
	r.t.Helper()
	podRecords, err := r.trees.PodRecordsOf(r.tree.project)
	if err != nil {
		r.t.Fatalf("read the pod records: %v", err)
	}
	r.tree.hold = hold
	r.engine = workflow.New(r.records, workflow.Config{Project: r.tree.project, Repositories: testRepositories,
		Steps: r.tree.steps, Hold: hold, Pods: podRecords, Linger: time.Hour, Clock: func() time.Time { return r.clock.Now() },
		Hooks: []workflow.Hook{{Name: "back-to-implementing", On: workflow.HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleImplementer},
			Prompt: "Read the tester's report before you write anything."}}}, quietLogger())
	r.admission = admit.New(r.records, r.engine, 2, r.tree.project, quietLogger())
	r.runner.handlers, r.runner.hold = []intake.Handler{r.engine, r.admission}, hold
}

// supervise gives the rig a supervisor over the rig's store, on clock, as a daemon's boot builds
// one, with each agent connection the rig made registered again. Its machines tell the daemon of
// a work hold that ended under a claim it stopped, which applies the fact of the hold's end with
// the rig's handlers and logs on the runner's logger (workflowRuntime.workHoldEndedUnderStop), as
// the boot wires it (openSupervision). Where the rig refuses reads of the work hold row
// (refuseHoldRead), its store refuses them.
func (r *workHoldRig) supervise(clock *heldClock) {
	sup, rt := newOutboxSupervisor(r.t, r.tokenProject, r.t.TempDir())
	sup.deps.Store, sup.deps.Clock = r.trees, clock
	if r.refuseHoldRead != nil {
		sup.deps.Store = refusedHoldRead{Store: r.trees, refuse: r.refuseHoldRead}
	}
	sup.deps.WorkHoldEndedUnderStop = func(c supervise.Claim, version int) {
		w := &workflowRuntime{pool: r.pool, handlers: []intake.Handler{r.engine, r.admission}, log: r.runner.log}
		w.workHoldEndedUnderStop(c, version)
	}
	for role, conn := range r.conns {
		sup.deps.Conns.(*fake.Conns).Register(r.token(role), conn)
	}
	r.sup, r.rt, r.clock = sup, rt, clock
}

func (r *workHoldRig) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.tokenProject, r.tree.key, role)
	if err != nil {
		r.t.Fatalf("the %s token of %s: %v", role, r.tree.key, err)
	}
	return token
}

func (r *workHoldRig) machine(role claim.Role) *supervise.Machine {
	r.t.Helper()
	machine, ok := r.sup.Machine(r.token(role))
	if !ok {
		r.t.Fatalf("no machine for the %s", role)
	}
	return machine
}

// conn is the agent connection of role's claim, made and registered the first time it is asked.
func (r *workHoldRig) conn(role claim.Role) *fake.Conn {
	if conn, ok := r.conns[role]; ok {
		return conn
	}
	conn := fake.NewConn()
	r.conns[role] = conn
	r.sup.deps.Conns.(*fake.Conns).Register(r.token(role), conn)
	return conn
}

func (r *workHoldRig) handle(role claim.Role, ev supervise.Event) {
	r.t.Helper()
	if err := r.machine(role).Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("the %s's %T: %v", role, ev, err)
	}
}

// create creates role's claim of the root as c describes it.
func (r *workHoldRig) create(role claim.Role, c supervise.Claim) *supervise.Machine {
	r.t.Helper()
	c.Token, c.Project, c.Tree, c.Issue, c.Role = r.token(role), r.tokenProject, r.tree.key, r.tree.key, role
	r.conn(role)
	machine, _, err := r.sup.Create(r.ctx, c, "")
	if err != nil {
		r.t.Fatalf("create the %s's claim: %v", role, err)
	}
	return machine
}

// live launches role's claim and makes its agent ready on a session of its own.
func (r *workHoldRig) live(role claim.Role) *supervise.Machine {
	r.t.Helper()
	machine := r.create(role, supervise.Claim{State: supervise.StateQueued})
	r.handle(role, supervise.RequestSpawn{Claim: r.token(role)})
	readyClaimAs(r.t, r.sup, r.token(role), role, "ses-"+string(role))
	return machine
}

// working is live with its agent in a turn.
func (r *workHoldRig) working(role claim.Role) *supervise.Machine {
	r.t.Helper()
	machine := r.live(role)
	r.handle(role, supervise.StreamTurnStart{Claim: r.token(role)})
	return machine
}

// released is role's claim as an idle release leaves it: suspended with the source `released`,
// its session intact, and nothing but a wake to bring its pod back.
func (r *workHoldRig) released(role claim.Role) *supervise.Machine {
	r.t.Helper()
	machine := r.create(role, supervise.Claim{Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased, Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl"})
	if !machine.Claim().Released() {
		r.t.Fatalf("the %s is %+v, want it released", role, machine.Claim())
	}
	return machine
}

// keptTask is the task a released worker holds unconfirmed in the rigs that give it one.
const keptTask = "Implement entry 1 of LEGION-208: cover the empty list."

// releasedWithATask is role's claim released while it held task, of the root's phase at entry,
// unconfirmed: its agent registered and its ready had not come when the release took its pod.
func (r *workHoldRig) releasedWithATask(role claim.Role, task string, entry int64) *supervise.Machine {
	r.t.Helper()
	machine := r.create(role, supervise.Claim{State: supervise.StateQueued})
	token := r.token(role)
	r.handle(role, supervise.RequestSpawn{Claim: token})
	generation := machine.Claim().Generation
	r.handle(role, supervise.StreamHello{Claim: token, Generation: generation})
	r.handle(role, supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-" + string(role),
		SessionFile: "/tmp/" + string(role) + ".jsonl"})
	r.handle(role, supervise.RequestDeliver{Claim: token, Task: task, ID: "outbox:9001", Row: 9001, Phase: r.tree.at,
		Generation: 1, Entry: entry})
	r.handle(role, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByReleased, Reason: "the issue's pod is released"})
	if c := machine.Claim(); !c.Released() || c.Pending == nil || c.Pending.Task != task {
		r.t.Fatalf("the %s is %+v, want it released holding its task", role, c)
	}
	return machine
}

// operatorSuspended is role's claim as an operator's suspension leaves it.
func (r *workHoldRig) operatorSuspended(role claim.Role) *supervise.Machine {
	r.t.Helper()
	return r.create(role, supervise.Claim{Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByOperator, Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl"})
}

// apply is one fact as the daemon applies it: through the engine and admission, in one
// transaction, under an id of its own.
func (r *workHoldRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	r.facts++
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", fmt.Sprintf("%s-%d", id, r.facts), fact, r.engine, r.admission)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %+v, %v", id, result.Refusal, err)
	}
}

// close writes version of the root's spec, a version no person approved, which closes the gate.
func (r *workHoldRig) close(version int) {
	r.t.Helper()
	r.apply(fmt.Sprintf("version-%d", version), intake.DispatchArtifact{Key: r.tree.key, ArtifactID: r.tree.document,
		Kind: intake.DispatchArtifactVersion, Version: version})
}

// approve is a person's approval of version of the root's spec, at Dispatch's seq.
func (r *workHoldRig) approve(version int, seq int64) {
	r.t.Helper()
	r.apply(fmt.Sprintf("approved-%d", version), r.approval(version, seq))
}

// approval is the fact of a person's approval of version of the root's spec, at Dispatch's seq.
func (r *workHoldRig) approval(version int, seq int64) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: r.tree.key, ArtifactID: r.tree.document, Kind: intake.DispatchArtifactApproved,
		Version: version, Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: seq}}
}

// carriedApproval is F3, Dispatch's carried approval of version 46 after an exact undo, as its
// events route served it (../workflow/testdata/dispatch-carried-approval.json), decoded as the
// daemon's catch-up decodes it.
func (r *workHoldRig) carriedApproval() intake.Fact {
	r.t.Helper()
	raw, err := os.ReadFile("../workflow/testdata/dispatch-carried-approval.json")
	if err != nil {
		r.t.Fatalf("read the captured carried approval: %v", err)
	}
	var captured struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		r.t.Fatalf("decode the captured carried approval: %v", err)
	}
	fact, unread, err := intake.DecodeDispatchEvent(r.tree.project, r.tree.key, captured.Event)
	if err != nil || len(unread) > 0 {
		r.t.Fatalf("decode the captured carried approval's event = %v, unread %v", err, unread)
	}
	if artifact, ok := fact.(intake.DispatchArtifact); !ok || !artifact.CarriedApproval() || artifact.Version != 46 {
		r.t.Fatalf("the captured event decodes to %#v, want the carried approval of version 46", fact)
	}
	return fact
}

// runOnce is one drain of every row due now, the clock left where it stands.
func (r *workHoldRig) runOnce() {
	r.t.Helper()
	if err := r.runner.RunOnce(r.ctx); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
}

// drain runs the outbox until what it holds stops moving: each pass runs every row due, and the
// clock then moves past the longest backoff a row can wait (64 seconds), so a row that waited
// runs again on the next pass.
func (r *workHoldRig) drain() {
	r.t.Helper()
	for range 8 {
		r.runOnce()
		r.clock.advance(65 * time.Second)
	}
}

// launches is how often the runtime started role's agent: its spawns and its resumes (launchesOf).
func (r *workHoldRig) launches(role claim.Role) int {
	return len(launchesOf(r.rt, r.token(role)))
}

// stored is role's claim as the store holds it: its state and its suspension's source.
func (r *workHoldRig) stored(role claim.Role) (supervise.ClaimState, supervise.SuspendSource) {
	r.t.Helper()
	var state, by string
	if err := r.pool.QueryRow(r.ctx, `select state, coalesce(suspended_by, '') from claims where token = $1`, string(r.token(role))).Scan(&state, &by); err != nil {
		r.t.Fatalf("read the %s's stored claim: %v", role, err)
	}
	return supervise.ClaimState(state), supervise.SuspendSource(by)
}

// wantHeld is role's claim stopped by the hold, in its machine and in the store, its agent never
// started since launched counted.
func (r *workHoldRig) wantHeld(role claim.Role, launched int, when string) {
	r.t.Helper()
	if c := r.machine(role).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByHold {
		r.t.Fatalf("%s the %s is %s by %q, want it stopped by the hold", when, role, c.State, c.SuspendedBy)
	}
	if state, by := r.stored(role); state != supervise.StateSuspended || by != supervise.SuspendedByHold {
		r.t.Fatalf("%s the %s is stored %s by %q, want it stopped by the hold", when, role, state, by)
	}
	if got := r.launches(role); got != launched {
		r.t.Fatalf("%s the runtime started the %s %d times, want %d: no turn starts while the gate is closed", when, role, got, launched)
	}
}

// wantStarted is role's claim started by the hold's opening: its agent launched once more than
// launched, and the one task it holds is task.
func (r *workHoldRig) wantStarted(role claim.Role, launched int, task string) {
	r.t.Helper()
	if got := r.launches(role); got != launched+1 {
		r.t.Fatalf("the runtime started the %s %d times since the gate opened, want once", role, got-launched)
	}
	c := r.machine(role).Claim()
	if c.WorkHeld() || c.State == supervise.StateSuspended {
		r.t.Fatalf("the started %s is %s by %q, want it running", role, c.State, c.SuspendedBy)
	}
	if c.Pending == nil || c.Pending.Task != task {
		r.t.Fatalf("the started %s holds the task %+v, want the one task\n%q", role, c.Pending, task)
	}
}

// closedSentence is today's first sentence of the close message at version of the root's spec.
func (r *workHoldRig) closedSentence(version int) string {
	return fmt.Sprintf("The design gate of %s closed: version %d of its spec document (%s) is with its reviewer, "+
		"so the design you are working to may change.", r.tree.key, version, r.tree.document)
}

// workerClose is the close message under `work` to a phase worker.
func (r *workHoldRig) workerClose(version int) string {
	return r.closedSentence(version) + " " + holdSentence + " End your turn now."
}

// mergeClose is today's close message, which `merge` keeps word for word.
func (r *workHoldRig) mergeClose(version int) string {
	return r.closedSentence(version) + " Your phase goes on; the merge waits until every gated step stands open again."
}

// openedMessage is today's open message at version of the root's spec.
func (r *workHoldRig) openedMessage(version int) string {
	return fmt.Sprintf("The design gate of %s opened: a person approved version %d of its spec document (%s), "+
		"and every step the deployment gates now stands open, so the design you are working to is the approved one. "+
		"A merge the closed gate held is released.", r.tree.key, version, r.tree.document)
}

// openedTo is the open role message at version of the root's spec as role's live claim is told it
// (openingDelivery): the open message, then the words that nothing its issue holds, or for the
// architect nothing the tree holds, changed since the version the rig's holders took (holdTree).
func (r *workHoldRig) openedTo(role claim.Role, version int) string {
	if role == claim.RoleArchitect {
		return fmt.Sprintf("%s Nothing the tree holds changed since version %d, which you took.", r.openedMessage(version), r.tree.took)
	}
	return fmt.Sprintf("%s Nothing your issue holds changed since version %d, which your phase took.", r.openedMessage(version), r.tree.took)
}

// heldNews is the news the root's phase holder in phase at is told of the spec from version from
// to version to, in a case whose holders took the approved version (withNews): the change list,
// which names no section, since the rig's Dispatch answers none (outboxDispatch.ArtifactChanges),
// and the rule its role's prompt carries.
func (r *workHoldRig) heldNews(at phase.Phase, from, to int) string {
	return fmt.Sprintf("The approved design of %[1]s changed for %[1]s, which you hold in %[2]s:\n"+
		"- The spec document (%[3]s): version %[5]d, since version %[4]d, which your phase last took.\n"+
		"  Dispatch named no changed section or block of it.\n"+
		"  Read it: `dispatch doc-read --issue %[1]s --artifact %[3]s --version %[5]d`\n\n"+
		"As your role's prompt says, read each named section and check at the new version: if your work is stale, "+
		"call `request_backward_move` with a reason that names the version and the change; if it is current, write a `dispatch comment` "+
		"on %[1]s that names the version and why; an issue whose pull request has merged never moves back.",
		r.tree.key, at, r.tree.document, from, to)
}

// opening is the sentences the one task of the opening starts with: the close message's two, that
// the hold stopped the turn, today's open message, and the check of what the turn already did.
func (r *workHoldRig) opening(closedAt, openedAt int) string {
	return r.closedSentence(closedAt) + " " + holdSentence + " The work hold stopped your turn. " + r.openedMessage(openedAt) +
		" Check what your turn already did: a tool call may not have finished."
}

// issue is the root as the record holds it now.
func (r *workHoldRig) issue() record.Issue {
	r.t.Helper()
	got := readIssue(r.t, r.pool, r.tree.key)
	if got == nil {
		r.t.Fatalf("%s is not recorded", r.tree.key)
	}
	return *got
}

// resumeTask is the resume task the engine sends the worker of the root's phase, as the record
// holds the root now (workflow.Engine.ResumeTask, the one place every resume is composed), read in a
// transaction that writes nothing.
func (r *workHoldRig) resumeTask() string {
	r.t.Helper()
	issue := r.issue()
	var task string
	r.inTx(func(tx pgx.Tx) error {
		var err error
		task, err = r.engine.ResumeTask(r.ctx, tx, issue)
		return err
	})
	return task
}

// bootRelaunch is the boot's relaunch of role's unfinished launch, as launchUnfinished makes it:
// the release of the uncertain launch (releaseUnfinishedClaim), which leaves the claim queued at
// its generation, and then the spawn of the released claim, with no fence holding it.
func (r *workHoldRig) bootRelaunch(role claim.Role) {
	r.t.Helper()
	boot, m, token := &supervision{log: quietLogger(), supervisor: r.sup}, r.machine(role), r.token(role)
	if released := boot.releaseUnfinishedClaim(r.ctx, token, m); released != releaseQueued {
		r.t.Fatalf("the boot's release of the unfinished launch of the %s ended %v, want it queued for its relaunch", role, released)
	}
	if err := m.Handle(r.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		r.t.Fatalf("the boot's relaunch of the %s: %v", role, err)
	}
}

// inTx runs write in one transaction of the rig's pool.
func (r *workHoldRig) inTx(write func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(_ context.Context, tx pgx.Tx) error { return write(tx) }); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// asAFact runs write in one transaction as the engine's handlers run inside a fact: with the
// fact's settle opened before and run after (workflow.Engine.OpenFact, SettleFact), as admission
// calls the engine's exported writers.
func (r *workHoldRig) asAFact(write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	ctx := r.engine.OpenFact(r.ctx)
	r.inTx(func(tx pgx.Tx) error {
		if err := write(ctx, tx); err != nil {
			return err
		}
		return r.engine.SettleFact(ctx, tx)
	})
}

// enqueue queues payload for the root, due now.
func (r *workHoldRig) enqueue(payload record.OutboxPayload) {
	r.t.Helper()
	enqueueOutbox(r.t, r.pool, r.records, mustOutboxRow(r.t, r.tree.key, payload, r.clock.Now()))
}

// rows is how many outbox rows of kind the root holds, and of the supervise op where kind is
// supervise and op is not empty, for role where role is not empty.
func (r *workHoldRig) rows(kind record.OutboxKind, op string, role claim.Role) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where issue = $1 and kind = $2
		and ($3 = '' or payload->>'op' = $3) and ($4 = '' or payload->>'role' = $4)`, r.tree.key, string(kind), op, string(role)).Scan(&n); err != nil {
		r.t.Fatalf("count the %s rows: %v", kind, err)
	}
	return n
}

// holdStops is every work-hold stop the outbox holds for role, oldest first.
func (r *workHoldRig) holdStops(role claim.Role) []record.SuperviseRequest {
	r.t.Helper()
	var stops []record.SuperviseRequest
	for _, request := range startRequestsOf(r.t, r.pool, r.tree.key, "suspend") {
		if request.Role == role && request.SuspendedBy == supervise.SuspendedByHold {
			stops = append(stops, request)
		}
	}
	return stops
}

// newestRow is the id of the newest row the outbox holds, 0 for none.
func (r *workHoldRig) newestRow() int64 {
	r.t.Helper()
	var id int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(max(id), 0) from outbox`).Scan(&id); err != nil {
		r.t.Fatalf("read the newest outbox row: %v", err)
	}
	return id
}

// startsSince is how many starts of role, a wake's among them, the outbox holds for the root
// above the row id since.
func (r *workHoldRig) startsSince(role claim.Role, since int64) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $2 and id > $3`, r.tree.key, string(role), since).Scan(&n); err != nil {
		r.t.Fatalf("count the %s's starts: %v", role, err)
	}
	return n
}

// armBackstop arms the release backstop of role, due a minute ago, and answers its id.
func (r *workHoldRig) armBackstop(role claim.Role) int64 {
	r.t.Helper()
	var id int64
	if err := r.pool.QueryRow(r.ctx, `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($1, $2, $3, 0, $4) returning id`, r.tokenProject, r.tree.key, string(role), r.clock.Now().Add(-time.Minute)).Scan(&id); err != nil {
		r.t.Fatalf("arm the %s's backstop: %v", role, err)
	}
	return id
}

// awaitDrained waits until token's machine has handled every event posted to it
// (supervisor.drained), and fails t naming what after 30 seconds.
func awaitDrained(t *testing.T, sup *supervisor, token claim.Token, what string) {
	t.Helper()
	done := make(chan struct{})
	sup.drained(token, done)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the machine of %s did not handle %s", token, what)
	}
}

// published is every message delivered to role's topic, in order.
func (r *workHoldRig) published(role claim.Role) []string {
	r.t.Helper()
	topic := notify.RoleTopicPrefix + string(r.token(role))
	_, delivered := r.publisher.snapshot()
	var messages []string
	for _, publish := range delivered {
		if publish.topic == topic {
			messages = append(messages, publish.message)
		}
	}
	return messages
}

// heldAtTheClose is a tree under `work` whose gate the close at version 4 has just closed: the
// root's architect is at work, its tester holds its phase and its implementer works no phase, both
// released before the close, whose stops of them are queued and not made yet.
func heldAtTheClose(t *testing.T) *workHoldRig {
	t.Helper()
	r := newWorkHoldRig(t, legionTree(phase.Testing, config.HoldWork))
	r.live(claim.RoleArchitect)
	r.released(claim.RoleTester)
	r.released(claim.RoleImplementer)
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleTester, Claim: r.token(claim.RoleTester), Entry: 1})
	})
	r.close(4)
	return r
}

// I3: every writer of a start row or a wake of a phase worker the plan lists, driven against a
// tree under the hold, starts no turn while the gate is closed, and each runs once the gate opens:
// the role it was for is started, once, with the one task the opening gives it, and nothing it
// wrote is left waiting. A writer acts after the close's stops were made, or where it says so
// before them (early), in the window in which the hold's stop of a released worker is queued and
// not made yet. The released merger's wake for the close message, the one start the closed gate
// lets through, is TestWorkHoldWakesAReleasedMerger's.
func TestWorkHoldHoldsEveryStartOfAPhaseWorker(t *testing.T) {
	type writer struct {
		name  string
		early bool
		act   func(r *workHoldRig)
		// starts is the role whose start, or wake, the writer's act queues itself, which a writer
		// that queued none would leave nothing to hold: empty for a writer whose row the test
		// queues by hand, one whose rows are left's, and one that queues no row at all.
		starts claim.Role
		// writesNone is the role whose start the writer's act writes no row of at all, because it
		// reads that role's claim stopped by the hold: admission's repair and sweep pass such a claim
		// over, and so does the task of a worker's ready. The opening's start is the role's one task.
		writesNone claim.Role
		// left are the rows the writer queued that wait for the gate, by kind, which must all have
		// run once it opens.
		left []record.OutboxKind
	}
	writers := []writer{
		{name: "a transition (a move back), and the hook prompt it fires", act: func(r *workHoldRig) {
			r.apply("back", intake.BackwardMove{Issue: r.tree.key, Requester: claim.RoleTester, To: phase.Implementing,
				Reason: "the empty list is not covered"})
			if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 1 {
				r.t.Fatalf("the move back queued %d starts of the implementer, want its one", got)
			}
			if got := r.rows(record.OutboxKindHookPrompt, "", ""); got != 1 {
				r.t.Fatalf("the move back queued %d hook prompts, want the hook's one", got)
			}
		}, left: []record.OutboxKind{record.OutboxKindHookPrompt}},
		{name: "StartPromotedWorker", starts: claim.RoleTester, act: func(r *workHoldRig) {
			r.asAFact(func(ctx context.Context, tx pgx.Tx) error {
				return r.engine.StartPromotedWorker(ctx, tx, r.issue(), claim.RoleTester)
			})
		}},
		{name: "SendOwedEntry", starts: claim.RoleTester, act: func(r *workHoldRig) {
			row := record.PhaseRow{Issue: r.tree.key, Role: claim.RoleTester, Claim: r.token(claim.RoleTester), Entry: 2,
				EntryTask: "Test entry 2 of LEGION-208."}
			r.inTx(func(tx pgx.Tx) error { return r.records.PutPhase(r.ctx, tx, row) })
			r.asAFact(func(ctx context.Context, tx pgx.Tx) error {
				return r.engine.SendOwedEntry(ctx, tx, r.issue(), claim.RoleTester, row)
			})
		}},
		{name: "the held phase's retry", starts: claim.RoleTester, act: func(r *workHoldRig) {
			r.apply("restart", intake.RestartPhase{Issue: r.tree.key, Role: claim.RoleTester})
		}},
		{name: "the boot", act: func(r *workHoldRig) { r.apply("boot", intake.DaemonBooted{}) }},
		{name: "admission's pass: owed entries and slotted workers", writesNone: claim.RoleTester, act: func(r *workHoldRig) {
			r.inTx(func(tx pgx.Tx) error {
				return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleTester, Claim: r.token(claim.RoleTester),
					Entry: 2, EntryTask: "Test entry 2 of LEGION-208."})
			})
			r.apply("tick", intake.ControllerTick{})
		}},
		{name: "startMidPhaseMembers' start of a role that works no phase", act: func(r *workHoldRig) {
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1})
		}},
		{name: "a CI settlement's wake", early: true, starts: claim.RoleTester, act: func(r *workHoldRig) {
			r.apply("settled", intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: "head1", Generation: 1,
				Snapshot: "head1-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}})
		}},
		{name: "the release backstop's wake", early: true, starts: claim.RoleTester, act: func(r *workHoldRig) {
			r.apply("backstop", intake.PodBackstopDue{ID: r.armBackstop(claim.RoleTester), Issue: r.tree.key, Role: claim.RoleTester})
		}},
		{name: "a listener's wake", early: true, starts: claim.RoleTester, act: func(r *workHoldRig) {
			r.apply("listener", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(r.token(claim.RoleTester)),
				At: r.clock.Now(), Report: "report-1"})
		}},
		{name: "a wake row of a delivery", early: true, act: func(r *workHoldRig) {
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleTester, Generation: 1,
				WakeKind: pods.WakeRoleMessage, Task: "A message reached your role."})
		}},
		{name: "wakeForDelivery and deliverHeld: a merge forward's ask", act: func(r *workHoldRig) {
			r.inTx(func(tx pgx.Tx) error {
				return r.records.PutPullRequest(r.ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: r.tree.key, Repo: "acme/widgets",
					Number: 42, Branch: "legion/" + r.tree.key, HeadSHA: "head1", Failing: []string{},
					MergeForward: &record.MergeForward{Head: "head1", Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen, AskedAt: r.clock.Now()}})
			})
			r.enqueue(record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "head1", Base: "main",
				Phase: phase.Testing, PullRequest: 42, Why: record.ConflictCheckNotGreen})
		}, left: []record.OutboxKind{record.OutboxKindMergeForward}},
		{name: "a hook prompt's wake", act: func(r *workHoldRig) {
			r.enqueue(record.HookPrompt{Hook: "back-to-testing", Role: claim.RoleTester, From: phase.Reviewing, To: phase.Testing, Count: 1,
				Counts: map[string]int{"reviewing-testing": 1}, PullRequest: "acme/widgets#42", Prompt: "Test the empty list first."})
		}, left: []record.OutboxKind{record.OutboxKindHookPrompt}},
		{name: "readyWorkerTask", writesNone: claim.RoleTester, act: func(r *workHoldRig) {
			r.apply("ready", intake.ClaimReady{Issue: r.tree.key, Role: claim.RoleTester, Launch: r.machine(claim.RoleTester).Claim().Generation})
		}},
		{name: "a pause's resume of a spawn it owed", act: func(r *workHoldRig) {
			pauses := &projectPause{supervisor: r.sup, log: quietLogger()}
			for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
				pauses.resumeOwedSpawn(r.token(role), 7)
				awaitDrained(r.t, r.sup, r.token(role), "the pause's resume")
			}
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			r := heldAtTheClose(t)
			act := func() {
				t.Helper()
				since := r.newestRow()
				w.act(r)
				if w.starts != "" && r.startsSince(w.starts, since) == 0 {
					t.Fatalf("the writer queued no start of the %s, want the one it writes", w.starts)
				}
				if w.writesNone != "" {
					if got := r.startsSince(w.writesNone, since); got != 0 {
						t.Fatalf("the writer queued %d starts of the %s, want none: the hold has its claim stopped", got, w.writesNone)
					}
				}
			}
			if w.early {
				act()
			}
			r.drain()
			if !w.early {
				act()
				r.drain()
			}
			for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
				r.wantHeld(role, 0, "while the gate is closed")
			}
			for _, kind := range w.left {
				if got := r.rows(kind, "", ""); got != 1 {
					t.Fatalf("the outbox holds %d %s rows while the gate is closed, want the writer's one waiting", got, kind)
				}
			}

			r.approve(4, 12)
			r.drain()
			opening := r.opening(4, 4)
			for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
				if got := r.launches(role); got != 1 {
					t.Fatalf("the runtime started the %s %d times once the gate opened, want once", role, got)
				}
				c := r.machine(role).Claim()
				if c.WorkHeld() || c.State == supervise.StateSuspended || c.Pending == nil || !strings.HasPrefix(c.Pending.Task, opening+"\n\n") {
					t.Fatalf("the %s once the gate opened is %s by %q holding %+v, want it started with one task behind the opening's sentences",
						role, c.State, c.SuspendedBy, c.Pending)
				}
			}
			for _, kind := range append(w.left, record.OutboxKindSupervise) {
				if got := r.rows(kind, "", ""); got != 0 {
					t.Fatalf("the outbox still holds %d %s rows once the gate opened, want every one run", got, kind)
				}
			}
		})
	}
}

// H9: the hold stops no architect, and holds none of its starts. Under `work`, with the gate
// closed, the root's live architect is told the close and that its own work goes on, and is given
// no stop; and a start of the architect launches it while the gate stays closed, where the start
// of a phase worker of the same tree, queued beside it, waits.
func TestWorkHoldStartsAnArchitect(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.live(claim.RoleArchitect)
	r.close(4)
	r.drain()
	want := r.closedSentence(4) + " " + holdSentence + " Your own work goes on."
	if got := r.published(claim.RoleArchitect); len(got) != 1 || got[0] != want {
		t.Fatalf("the architect was told %q, want the close for an architect, %q", got, want)
	}
	if stops := r.holdStops(claim.RoleArchitect); len(stops) != 0 {
		t.Fatalf("the hold queued the stops %+v of the architect, want none", stops)
	}
	if c := r.machine(claim.RoleArchitect).Claim(); c.State != supervise.StateReady || c.SuspensionHeld {
		t.Fatalf("the architect is %s, a stop held %v; want it ready and not stopped", c.State, c.SuspensionHeld)
	}

	// The root's architect started again beside the first start of the tree's implementer, neither
	// of which holds a live claim: the architect launches with the gate closed, and the implementer
	// waits for the gate.
	r.handle(claim.RoleArchitect, supervise.RequestSuspend{Claim: r.token(claim.RoleArchitect), By: supervise.SuspendedByDaemon,
		Reason: "the root's architect is taken down for the test"})
	launched := r.launches(claim.RoleArchitect)
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleArchitect, Generation: 1, Task: "Write the design."})
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
		Task: "Implement LEGION-208.", NeedsSlot: true})
	r.drain()
	if got := r.launches(claim.RoleArchitect); got != launched+1 {
		t.Fatalf("the architect's start launched it %d times with the gate closed, want once: the hold holds no architect", got-launched)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleArchitect); got != 0 {
		t.Fatalf("the architect's start still waits (%d rows), want it run", got)
	}
	if got := r.launches(claim.RoleImplementer); got != 0 {
		t.Fatalf("the implementer's start launched it %d times with the gate closed, want it held", got)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 1 {
		t.Fatalf("the implementer's start left %d rows with the gate closed, want it waiting", got)
	}
}

// mergingWithAReleasedMerger is a tree in merging under hold whose merger's pod was released
// while its pull request sat in the merge queue, its architect at work.
func mergingWithAReleasedMerger(t *testing.T, hold config.Hold) *workHoldRig {
	t.Helper()
	r := newWorkHoldRig(t, legionTree(phase.Merging, hold))
	r.live(claim.RoleArchitect)
	r.released(claim.RoleMerger)
	return r
}

// wakeTheMerger runs the close's message to the released merger to the merger: the message's
// first attempt wakes it, its agent becomes ready, and the next attempt publishes the message,
// both at the same moment of the clock. It answers that moment.
func wakeTheMerger(r *workHoldRig) time.Time {
	r.t.Helper()
	r.runOnce()
	if got := r.launches(claim.RoleMerger); got != 1 {
		r.t.Fatalf("the close's message started the released merger %d times, want once: the wake for it is the one start the closed gate lets through", got)
	}
	if got := r.published(claim.RoleMerger); len(got) != 0 {
		r.t.Fatalf("the merger was told %q before its pod came back, want nothing yet", got)
	}
	r.clock.advance(65 * time.Second)
	readyClaimAs(r.t, r.sup, r.token(claim.RoleMerger), claim.RoleMerger, "ses-merger")
	ready := r.clock.Now()
	r.runOnce()
	return ready
}

// Decision 13, H16: under `merge` a released merger is woken for the close message, so it can take
// its pull request out of GitHub's merge queue, and is told today's close word for word once its
// agent is ready. The hold stops it not. Every other start of the merger waits while the gate is
// closed — here its phase's start — and runs once the gate opens.
func TestMergeHoldWakesAReleasedMerger(t *testing.T) {
	r := mergingWithAReleasedMerger(t, config.HoldMerge)
	r.close(4)
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
		Task: "Merge the pull request."})
	wakeTheMerger(r)
	if got := r.published(claim.RoleMerger); len(got) != 1 || got[0] != r.mergeClose(4) {
		t.Fatalf("the woken merger was told %q, want today's close, %q", got, r.mergeClose(4))
	}
	r.drain()
	if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
		t.Fatalf("under merge the outbox holds the stops %+v of the merger, want none", stops)
	}
	if c := r.machine(claim.RoleMerger).Claim(); c.State != supervise.StateReady || c.SuspensionHeld {
		t.Fatalf("the woken merger is %s, a stop held %v; want it ready and not stopped", c.State, c.SuspensionHeld)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 1 {
		t.Fatalf("the merger's phase start ran with the gate closed (%d rows left), want it waiting", got)
	}
	if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 0 {
		t.Fatalf("the merger was prompted %+v with the gate closed, want nothing", prompts)
	}

	r.approve(4, 12)
	r.drain()
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 0 {
		t.Fatalf("the merger's phase start still waits (%d rows) once the gate opened, want it run", got)
	}
	if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, "Merge the pull request.") {
		t.Fatalf("the merger was prompted %+v once the gate opened, want its phase's task", prompts)
	}
}

// Decision 13, H16: under `work` a released merger is woken for the close message too, and is told
// the work close once its agent is ready; its stop is posted then, so its 2 minutes count from its
// start (M4): it is stopped by the hold when the turn the message starts ends, or 2 minutes after
// it became ready, its session kept.
func TestWorkHoldWakesAReleasedMerger(t *testing.T) {
	for _, end := range []string{"its turn ends", "2 minutes after its start"} {
		t.Run(end, func(t *testing.T) {
			r := mergingWithAReleasedMerger(t, config.HoldWork)
			r.close(4)
			ready := wakeTheMerger(r)
			if got := r.published(claim.RoleMerger); len(got) != 1 || got[0] != r.workerClose(4) {
				t.Fatalf("the woken merger was told %q, want the work close, %q", got, r.workerClose(4))
			}
			stops := r.holdStops(claim.RoleMerger)
			if len(stops) != 1 || !stops[0].Deadline.Equal(ready.Add(holdGrace)) || !stops[0].TurnAfter.Equal(ready) {
				t.Fatalf("the merger's stops are %+v, want one cut at %s, 2 minutes from its start at %s", stops, ready.Add(holdGrace), ready)
			}
			r.runOnce()
			if c := r.machine(claim.RoleMerger).Claim(); c.State != supervise.StateReady || !c.WorkHeld() {
				t.Fatalf("the merger once its stop ran is %s, held %v; want it ready, its stop held for the turn the message starts", c.State, c.WorkHeld())
			}
			if live, _ := r.clock.armed(holdGrace); live != 1 {
				t.Fatalf("%d timers are armed for the 2 minutes, want the merger's stop's one", live)
			}
			switch end {
			case "its turn ends":
				r.handle(claim.RoleMerger, supervise.StreamTurnStart{Claim: r.token(claim.RoleMerger)})
				endTurn(r.ctx, t, r.machine(claim.RoleMerger), r.token(claim.RoleMerger))
			default:
				if fired := r.clock.fire(holdGrace); fired != 1 {
					t.Fatalf("fired %d timers at the 2 minutes, want the merger's stop's one", fired)
				}
			}
			r.wantHeld(claim.RoleMerger, 1, "after the stop")
			if c := r.machine(claim.RoleMerger).Claim(); c.Session != "ses-merger" {
				t.Fatalf("the stopped merger holds the session %q, want its own kept", c.Session)
			}
		})
	}
}

// Spec step 4: a project's pause and its resume end no work hold. The resume posts the end of
// the pause to every machine, resumes each spawn the pause owed, wakes every lane and applies an
// admission pass; the claims the hold stopped stay stopped through all of it, and the opening
// starts each of them.
func TestWorkHoldIsNotEndedByAPauseResume(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.working(claim.RoleImplementer)
	r.released(claim.RoleTester)
	r.close(4)
	r.drain()
	endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the close")
	r.wantHeld(claim.RoleTester, 0, "after the close")

	st := r.trees
	podRecords, err := st.PodRecordsOf(r.tree.project)
	if err != nil {
		t.Fatal(err)
	}
	pauses := &projectPause{store: st, records: projectRecords{Store: r.records, project: r.tree.project}, pods: podRecords,
		project: r.tree.project, token: r.tokenProject, now: r.clock.Now, log: quietLogger(),
		workflow: &workflowRuntime{pool: r.pool, records: r.records, handlers: r.runner.handlers}, supervisor: r.sup}
	pauses.workflow.outboxWake.Store(r.runner)
	if pauses.service, err = pause.New(pause.Options{Store: st, Records: record.NewStore(), Project: r.tree.project,
		Config: config.Config{}, Effects: pauses, Log: quietLogger(), Now: r.clock.Now}); err != nil {
		t.Fatalf("build the pause service: %v", err)
	}
	// The pause as the service's own transaction writes it, its flag set from it as the outbox's
	// sweep sets it; nothing of the tree is live to stop. The service refuses a pause of its own
	// under this rig's runtime (pause.Unavailable), not a resume.
	if _, _, err := st.Pause(r.ctx, store.PauseRequest{Project: r.tree.project, By: pauseOperator, Reason: "the model provider is down",
		At: r.clock.Now(), StopRow: pause.StopRow}); err != nil {
		t.Fatalf("pause the project: %v", err)
	}
	if err := pauses.service.Refresh(r.ctx); err != nil {
		t.Fatalf("set the pause's flag: %v", err)
	}
	r.drain()
	// The resume posts the end of the pause to every machine and runs the rest of its effects
	// (projectPause.Resumed); each spawn a pause owed is resumed by rule besides.
	ended, _, err := pauses.service.Resume(r.ctx, pauseOperator)
	if err != nil {
		t.Fatalf("resume the project: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		pauses.resumeOwedSpawn(r.token(role), ended.ID)
		awaitDrained(t, r.sup, r.token(role), "the resume")
	}
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the pause's resume")
	r.wantHeld(claim.RoleTester, 0, "after the pause's resume")

	r.approve(4, 12)
	r.drain()
	opening := r.opening(4, 4)
	r.wantStarted(claim.RoleImplementer, 1, opening+"\n\n"+r.resumeTask())
	r.wantStarted(claim.RoleTester, 0, opening+"\n\n"+"The issue's other roles may ask you questions.")
}

// H14, check a3d61f8e: a released phase worker that is not a merger is not woken at the close; the
// hold takes it over its release at once, its session and its pending task kept. A CI settlement's
// task, the release backstop's wake and a hook prompt during the hold start none of it, and the
// opening starts it with one task that starts with the close message's sentences and carries the
// task it kept.
func TestWorkHoldKeepsAReleasedWorkerStoppedUntilTheOpening(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleImplementer,
			Claim: r.token(claim.RoleImplementer), Entry: 1, EntryTask: keptTask})
	})
	r.releasedWithATask(claim.RoleImplementer, keptTask, 1)
	launched := r.launches(claim.RoleImplementer)

	r.close(4)
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the close queued %d role messages, want none: a released worker that is not a merger is not woken", got)
	}
	r.drain()
	r.wantHeld(claim.RoleImplementer, launched, "after the close")

	// A CI settlement of the head, the release backstop coming due, and a hook prompt for the role,
	// whose topic no session holds while the hold has its claim stopped.
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(r.token(claim.RoleImplementer)))
	r.apply("settled", intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: "head1", Generation: 1,
		Snapshot: "head1-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}})
	backstop := r.armBackstop(claim.RoleImplementer)
	r.apply("backstop", intake.PodBackstopDue{ID: backstop, Issue: r.tree.key, Role: claim.RoleImplementer})
	r.enqueue(firedHook)
	r.drain()

	r.wantHeld(claim.RoleImplementer, launched, "after a settlement, a backstop and a hook prompt")
	if c := r.machine(claim.RoleImplementer).Claim(); c.Pending == nil || c.Pending.Task != keptTask || !c.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the held implementer holds %+v, want the task it kept, unconfirmed", c.Pending)
	}
	if got := r.rows(record.OutboxKindHookPrompt, "", ""); got != 1 {
		t.Fatalf("the hook prompt ran with the role held (%d rows left), want it waiting for the opening", got)
	}

	r.publisher.setAbsent()
	r.approve(4, 12)
	r.drain()
	r.wantStarted(claim.RoleImplementer, launched, r.opening(4, 4)+"\n\n"+keptTask)
	if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != firedHook.Message(r.tree.key) {
		t.Fatalf("the implementer was told %q once the gate opened, want the hook's prompt", got)
	}
}

// Spec step 3, H8, check a3d61f8e: the close message reaches a phase worker in a turn, and its
// stop is posted then, 2 minutes on: the worker is stopped by the hold when its turn ends, or its
// turn is cut 2 minutes after the message reached it, its session kept.
func TestWorkHoldStopsALiveWorkerAtItsTurnsEndOrTwoMinutesAfterTheMessage(t *testing.T) {
	for _, end := range []string{"its turn ends", "2 minutes after the message"} {
		t.Run(end, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			r.working(claim.RoleImplementer)
			r.close(4)
			reached := r.clock.Now()
			r.runOnce()
			if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.workerClose(4) {
				t.Fatalf("the implementer was told %q, want the work close, %q", got, r.workerClose(4))
			}
			stops := r.holdStops(claim.RoleImplementer)
			if len(stops) != 1 || !stops[0].Deadline.Equal(reached.Add(holdGrace)) || !stops[0].TurnAfter.Equal(reached) {
				t.Fatalf("the implementer's stops are %+v, want one cut at %s, 2 minutes after the message reached it", stops, reached.Add(holdGrace))
			}
			r.runOnce()
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateWorking || !c.WorkHeld() {
				t.Fatalf("the implementer once its stop ran is %s, held %v; want it at work with the stop held", c.State, c.WorkHeld())
			}
			switch end {
			case "its turn ends":
				endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
			default:
				if fired := r.clock.fire(holdGrace); fired != 1 {
					t.Fatalf("fired %d timers at the 2 minutes, want the stop's one", fired)
				}
			}
			r.wantHeld(claim.RoleImplementer, 1, "after the stop")
			if c := r.machine(claim.RoleImplementer).Claim(); c.Session != "ses-implementer" {
				t.Fatalf("the stopped implementer holds the session %q, want its own kept", c.Session)
			}
			r.drain()
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
				t.Fatalf("the stop still waits as %+v once made, want it done", stops)
			}
		})
	}
}

// Spec step 3, H32, check a3d61f8e: a gate that opens again before a stop is made stops nothing
// more. The stop a message would have posted is not posted, and a stop held for a turn is
// dropped, the turn going on; a released worker's stop not yet run takes nothing over. A close
// message that meets the open gate before its publish is not published, so the worker is told
// the opening alone (TestGatesHoldPublishesNoCloseMessageOnceTheGateOpensAgain). A turn that ends
// after the opening committed and before the stop's row runs again makes no stop either
// (TestWorkHoldMakesNoStopWhenTheTurnEndsAfterTheOpening).
func TestWorkHoldMakesNoStopOnceTheGateOpensAgain(t *testing.T) {
	t.Run("before the message reached the worker", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		r.working(claim.RoleImplementer)
		r.released(claim.RoleTester)
		r.close(4)
		r.approve(4, 12)
		r.drain()
		if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.openedTo(claim.RoleImplementer, 4) {
			t.Fatalf("the implementer was told %q, want the opening alone: the close message met the open gate before its publish", got)
		}
		if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
			t.Fatalf("the outbox holds the stops %+v of the implementer, want none posted once the gate opened", stops)
		}
		endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
		if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateIdle || c.WorkHeld() {
			t.Fatalf("the implementer after its turn is %s, held %v; want it idle and not stopped", c.State, c.WorkHeld())
		}
		if c := r.machine(claim.RoleTester).Claim(); !c.Released() {
			t.Fatalf("the released tester is %s by %q, want it still released: its stop was not made", c.State, c.SuspendedBy)
		}
	})
	t.Run("with the stop held for the worker's turn", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		r.working(claim.RoleImplementer)
		r.close(4)
		r.runOnce()
		r.runOnce()
		if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
			t.Fatalf("the implementer is %s, held %v; want its stop held for its turn", c.State, c.WorkHeld())
		}
		r.approve(4, 12)
		r.drain()
		if c := r.machine(claim.RoleImplementer).Claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the implementer reads held %v, a stop held %v once the gate opened; want the stop dropped", c.WorkHeld(), c.SuspensionHeld)
		}
		endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
		r.clock.fire(holdGrace)
		if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateIdle {
			t.Fatalf("the implementer after its turn and the 2 minutes is %s by %q, want it idle", c.State, c.SuspendedBy)
		}
		if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
			t.Fatalf("the stop still waits as %+v, want it finished unmade", stops)
		}
	})
}

// Spec step 3 ("A stop that the daemon has not made when the gate opens again is not made"), row
// H32 and check a3d61f8e ("A gate that opens again before a stop is made stops nothing more"),
// for the order the tester's whole-daemon drive met: the close's stop is held for the worker's
// turn, the opening commits, and the turn ends before the stop row's next attempt reads the
// opening. The stop was not made when the gate opened, so the turn's end makes none: the claim
// ends its turn idle with its process, and nothing stops and relaunches it.
func TestWorkHoldMakesNoStopWhenTheTurnEndsAfterTheOpening(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.working(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	r.runOnce()
	if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
		t.Fatalf("before the opening the implementer is %s, held %v; want its stop held for its turn", c.State, c.WorkHeld())
	}
	r.approve(4, 12)
	endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
	if c := r.machine(claim.RoleImplementer).Claim(); c.State == supervise.StateSuspended {
		t.Fatalf("the implementer's turn ended after the gate opened again, and the turn's end stopped it by %q: "+
			"a stop the daemon had not made when the gate opened was made", c.SuspendedBy)
	}
	r.drain()
	if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateIdle || c.WorkHeld() || c.SuspensionHeld {
		t.Fatalf("after the drain the implementer is %s by %q, held %v, a stop held %v; want it idle and not stopped",
			c.State, c.SuspendedBy, c.WorkHeld(), c.SuspensionHeld)
	}
	if state, by := r.stored(claim.RoleImplementer); state == supervise.StateSuspended {
		t.Fatalf("the implementer is stored suspended by %q, want it never stopped", by)
	}
	if got := r.launches(claim.RoleImplementer); got != launched {
		t.Fatalf("the runtime started the implementer %d times since the close, want none: its process was never to be stopped", got-launched)
	}
}

// openingHeldOpen applies a person's approval of version at Dispatch's seq, as approve does, with
// one more handler after the engine's and admission's. That handler holds the opening's
// transaction open: when this returns, the opening took the tree's work hold row, read the tree's
// claims and deleted the row, and it commits only once the test calls commit. opened answers the
// apply's error once it has returned. The test's cleanup commits a transaction the test left open.
func (r *workHoldRig) openingHeldOpen(version int, seq int64) (commit func(), opened <-chan error) {
	r.t.Helper()
	inside, release := make(chan struct{}), make(chan struct{})
	held := applyFunc(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
		close(inside)
		<-release
		return intake.Result{}, nil
	})
	opened = r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", fmt.Sprintf("approved-%d-held-open", version), r.approval(version, seq),
			r.engine, r.admission, held)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	commit = sync.OnceFunc(func() { close(release) })
	r.t.Cleanup(commit)
	select {
	case <-inside:
	case err := <-opened:
		r.t.Fatalf("the approval returned %v before its transaction was held open", err)
	}
	return commit, opened
}

// inBackground runs call on a goroutine of its own and answers its error once it returns. The
// test's cleanup waits for it, so no call outlives the rig's database.
func (r *workHoldRig) inBackground(call func() error) <-chan error {
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		done <- call()
	}()
	r.t.Cleanup(func() { <-finished })
	return done
}

// waitsOrReturns waits until a backend of the rig's database waits for a lock, or done answers
// first. It reports whether done answered, and its error.
func (r *workHoldRig) waitsOrReturns(what string, done <-chan error) (returned bool, err error) {
	r.t.Helper()
	testwait.Eventually(r.t, what+" to wait for a lock or return", func() bool {
		select {
		case err = <-done:
			returned = true
			return true
		default:
			return waitingOnARowLock(r.t, r.pool)
		}
	})
	return returned, err
}

// Spec step 3 ("A stop that the daemon has not made when the gate opens again is not made"), row
// H32, for a turn that ends while the opening's transaction is open. The close's stop is held for
// the implementer's turn. The opening has taken the tree's work hold row, read the claims and
// deleted the row, and it has not committed. The turn ends then: its decision waits for the row
// until the opening commits, and then reads no row, so it makes no stop. The implementer ends its
// turn idle with its process. It is never stopped and never started again.
func TestWorkHoldMakesNoStopWhenTheTurnEndsWhileTheOpeningCommits(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	machine, token := r.working(claim.RoleImplementer), r.token(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	r.runOnce()
	if c := machine.Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
		t.Fatalf("before the opening the implementer is %s, held %v; want its stop held for its turn", c.State, c.WorkHeld())
	}

	commit, opened := r.openingHeldOpen(4, 12)
	pending := machine.Claim().Pending
	ended := r.inBackground(func() error {
		if err := machine.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
			return err
		}
		if pending == nil || pending.ConfirmedAt.IsZero() {
			return nil
		}
		return machine.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true})
	})
	t.Cleanup(commit)
	returned, err := r.waitsOrReturns("the implementer's turn end", ended)
	if returned {
		t.Errorf("the implementer's turn end returned (%v) while the opening's transaction held the tree's work hold row; want it to wait for the opening's commit", err)
	}
	commit()
	if err := <-opened; err != nil {
		t.Fatalf("apply the approval: %v", err)
	}
	if !returned {
		err = <-ended
	}
	if err != nil {
		t.Fatalf("end the implementer's turn: %v", err)
	}

	r.drain()
	if c := machine.Claim(); c.State != supervise.StateIdle || c.WorkHeld() || c.SuspensionHeld {
		t.Fatalf("after the drain the implementer is %s by %q, held %v, a stop held %v; want it idle and not stopped",
			c.State, c.SuspendedBy, c.WorkHeld(), c.SuspensionHeld)
	}
	if state, by := r.stored(claim.RoleImplementer); state != supervise.StateIdle {
		t.Fatalf("the implementer is stored %s by %q, want it idle", state, by)
	}
	if stops := r.rt.CallsOf("Suspend"); len(stops) != 0 {
		t.Fatalf("the runtime was asked %d stops, want none: the implementer's process was never to be stopped", len(stops))
	}
	if got := r.launches(claim.RoleImplementer); got != launched {
		t.Fatalf("the runtime started the implementer %d times since the close, want none", got-launched)
	}
}

// Spec step 3 ("A stop that the daemon has not made when the gate opens again is not made"), H33,
// for a stop the outbox makes at once. The released tester's stop row posts the stop under `work`,
// and the opening's transaction holds the tree's work hold row: it read the tester released and
// deleted the row, and it has not committed. The stop's decision waits for the row until the
// opening commits, and then reads no row, so it makes no stop. The tester stays released, nothing
// starts it, and the row finishes with one line that names the tester and the version that closed
// the gate.
func TestWorkHoldMakesNoStopOfAReleasedWorkerWhileTheOpeningCommits(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	r.released(claim.RoleTester)
	r.close(4)
	if stops := r.holdStops(claim.RoleTester); len(stops) != 1 {
		t.Fatalf("after the close the tester's stops are %+v, want its one row", stops)
	}

	commit, opened := r.openingHeldOpen(4, 12)
	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	t.Cleanup(commit)
	returned, err := r.waitsOrReturns("the tester's stop row", ran)
	if returned {
		t.Errorf("the tester's stop row returned (%v) while the opening's transaction held the tree's work hold row; want its stop to wait for the opening's commit", err)
	}
	commit()
	if err := <-opened; err != nil {
		t.Fatalf("apply the approval: %v", err)
	}
	if !returned {
		err = <-ran
	}
	if err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}

	if c := r.machine(claim.RoleTester).Claim(); !c.Released() {
		t.Fatalf("the tester is %s by %q, want it still released: the hold's stop was not made", c.State, c.SuspendedBy)
	}
	if state, by := r.stored(claim.RoleTester); state != supervise.StateSuspended || by != supervise.SuspendedByReleased {
		t.Fatalf("the tester is stored %s by %q, want it still released", state, by)
	}
	if stops := r.holdStops(claim.RoleTester); len(stops) != 0 {
		t.Fatalf("the tester's stops are %+v, want the row finished", stops)
	}
	unmade := loggedMessages(t, logs.String(), "outbox: the design gate opened again before the work hold's stop was made; the stop is not made")
	if len(unmade) != 1 || unmade[0]["role"] != string(claim.RoleTester) || unmade[0]["version"] != float64(4) {
		t.Fatalf("the stop row logged %v, want one line naming the tester and version 4", unmade)
	}
	r.drain()
	if got := r.startsSince(claim.RoleTester, 0); got != 0 {
		t.Fatalf("the outbox holds %d starts of the tester, want none", got)
	}
	if got := r.launches(claim.RoleTester); got != 0 {
		t.Fatalf("the runtime started the tester %d times, want none", got)
	}
	if c := r.machine(claim.RoleTester).Claim(); !c.Released() {
		t.Fatalf("after the drain the tester is %s by %q, want it still released", c.State, c.SuspendedBy)
	}
}

// heldStopWrite is the rig's store with the work hold's stop of one claim held at its write: the
// decision that makes the stop has read the tree's work hold row, closes writing there and waits
// until proceed closes. Every other call goes straight to the store.
type heldStopWrite struct {
	supervise.Store
	token            claim.Token
	writing, proceed chan struct{}
}

func (s heldStopWrite) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByHold {
		select {
		case <-s.writing:
		default:
			close(s.writing)
		}
		<-s.proceed
	}
	return s.Store.PutClaim(ctx, c)
}

// heldReadAfterStop is the rig's store with the read of the tree's work hold row after the work
// hold's stop of one claim held: once the store has taken that stop's write, the next lock on a
// tree's work hold row closes reading and waits until proceed closes. That lock is the read after
// the stop (supervise's holdReadAfterStop). Every other call goes straight to the store.
type heldReadAfterStop struct {
	supervise.Store
	token            claim.Token
	written          *atomic.Bool
	reading, proceed chan struct{}
}

func (s heldReadAfterStop) PutClaim(ctx context.Context, c supervise.Claim) error {
	err := s.Store.PutClaim(ctx, c)
	if err == nil && c.Token == s.token && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByHold {
		s.written.Store(true)
	}
	return err
}

func (s heldReadAfterStop) WorkHoldStands(ctx context.Context, tree string) (bool, error) {
	if s.written.CompareAndSwap(true, false) {
		close(s.reading)
		<-s.proceed
	}
	return s.Store.WorkHoldStands(ctx, tree)
}

// Spec step 7, H33, the other order: the released tester's stop is made and committed before the
// opening reads the claims. The stop's decision read the tree's work hold row in a transaction of
// its own (supervise's readHoldForStop), and the read of the row after the stop waits here. So the
// opening waits for no lock: it reads the tester stopped by the hold, queues its start and
// commits. The read after the stop then finds the row gone and the daemon is told, but the
// opening's start covers the tester, so the tester is started once, with the opening's own task.
func TestWorkHoldOpeningStartsAStopCommittedBeforeIt(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	read := heldReadAfterStop{Store: r.trees, token: r.token(claim.RoleTester), written: &atomic.Bool{},
		reading: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(read.proceed) })
	r.sup.deps.Store = read
	r.released(claim.RoleTester)
	r.close(4)

	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	t.Cleanup(letGo)
	select {
	case <-read.reading:
	case err := <-ran:
		t.Fatalf("the tester's stop row returned %v before it read the row after its stop", err)
	}
	if state, by := r.stored(claim.RoleTester); state != supervise.StateSuspended || by != supervise.SuspendedByHold {
		t.Fatalf("before the opening the tester is stored %s by %q, want its stop committed", state, by)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-after-the-stop", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	t.Cleanup(letGo)
	returned, err := r.waitsOrReturns("the opening", opened)
	if !returned {
		t.Fatal("the opening waits for a lock while the tester's stop row runs, want it to commit at once: the stop's decision let the tree's work hold row go once it read it")
	}
	if err != nil {
		t.Fatalf("apply the approval: %v", err)
	}
	if got := r.startsSince(claim.RoleTester, 0); got != 1 {
		t.Fatalf("the opening queued %d starts of the tester, want one: it reads the tester stopped by the hold", got)
	}

	letGo()
	if err := <-ran; err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	r.drain()
	r.wantStarted(claim.RoleTester, 0, r.opening(4, 4)+"\n\n"+"The issue's other roles may ask you questions.")
	if stops := r.holdStops(claim.RoleTester); len(stops) != 0 {
		t.Fatalf("after the drain the tester's stops are %+v, want the row finished", stops)
	}
}

// Spec step 7, H33, for the one order the decision's read leaves: the released tester's stop is
// decided before the opening commits and written after it. The stop's decision reads the tree's
// work hold row standing in a transaction of its own (supervise's readHoldForStop), and the stop's
// write waits here. The opening waits for no lock: it reads the tester released, queues no start
// of it, deletes the row and commits. The stop is then made. The read of the row after the stop's
// write finds the row gone (supervise's holdReadAfterStop), so the daemon starts the tester at
// once, once, with the opening's task.
func TestWorkHoldStartsAStopDecidedBeforeTheOpeningCommitsWithTheOpeningsTask(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	write := heldStopWrite{Store: r.trees, token: r.token(claim.RoleTester), writing: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(write.proceed) })
	r.sup.deps.Store = write
	r.released(claim.RoleTester)
	r.close(4)

	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	t.Cleanup(letGo)
	select {
	case <-write.writing:
	case err := <-ran:
		t.Fatalf("the tester's stop row returned %v before it wrote the stop", err)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-before-the-stop-is-written", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	t.Cleanup(letGo)
	returned, err := r.waitsOrReturns("the opening", opened)
	if !returned {
		t.Fatal("the opening waits for a lock while the stop's decision writes the stop, want it to commit at once: the decision let the tree's work hold row go once it read it")
	}
	if err != nil {
		t.Fatalf("apply the approval: %v", err)
	}
	if got := r.startsSince(claim.RoleTester, 0); got != 0 {
		t.Fatalf("the opening queued %d starts of the tester, want none: it read the tester released", got)
	}

	letGo()
	if err := <-ran; err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	if got := r.startsSince(claim.RoleTester, 0); got != 1 {
		t.Fatalf("after the stop the outbox holds %d starts of the tester, want one: the read of the row after the stop finds the hold ended", got)
	}
	r.drain()
	r.wantStarted(claim.RoleTester, 0, r.opening(4, 4)+"\n\n"+"The issue's other roles may ask you questions.")
	if stops := r.holdStops(claim.RoleTester); len(stops) != 0 {
		t.Fatalf("after the drain the tester's stops are %+v, want the row finished", stops)
	}
}

// A decision of another source that calls the runtime while the opening runs holds no lock on the
// tree's work hold row. The close's stop is held for the turn its message starts in the ready
// implementer, so each decision of the implementer reads the tree's work hold row (supervise's
// readHoldForStop). The daemon's own suspension of the implementer, as a linger or a park asks it,
// stops the implementer's process, and the runtime holds that stop. The opening commits meanwhile
// and waits for no lock: the decision's read ended with its own transaction. The opening takes the
// row for update under the lock every fact takes, so a decision that held the row across the
// runtime's stop would hold every fact of the daemon until the stop returned.
func TestWorkHoldOpeningWaitsForNoRuntimeCallOfAnotherDecision(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	token := r.token(claim.RoleImplementer)
	slow := slowProcessStop{Runtime: r.rt, token: token, stopping: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(slow.proceed) })
	t.Cleanup(letGo)
	r.sup.deps.Runtime = slow
	machine := r.live(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	r.runOnce()
	if c := machine.Claim(); !c.WorkHeld() || c.State == supervise.StateSuspended {
		t.Fatalf("before the opening the implementer is %s, held %v; want its hold stop held for the turn its message starts", c.State, c.WorkHeld())
	}

	stopped := r.inBackground(func() error {
		return machine.Handle(r.ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: "LEGION-208 lingers"})
	})
	select {
	case <-slow.stopping:
	case err := <-stopped:
		t.Fatalf("the daemon's suspension of the implementer returned %v before the runtime stopped its process", err)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-while-another-stop-runs", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	t.Cleanup(letGo)
	returned, err := r.waitsOrReturns("the opening", opened)
	if !returned {
		t.Fatal("the opening waits for a lock while the daemon's suspension of the implementer waits on the runtime's stop, want it to commit at once")
	}
	if err != nil {
		t.Fatalf("apply the approval: %v", err)
	}

	letGo()
	if err := <-stopped; err != nil {
		t.Fatalf("suspend the implementer: %v", err)
	}
	if state, by := r.stored(claim.RoleImplementer); state != supervise.StateSuspended || by != supervise.SuspendedByDaemon {
		t.Fatalf("the implementer is stored %s by %q, want it suspended by the daemon", state, by)
	}
}

// refusedHoldRead is the rig's store with every read of a tree's work hold row refused while
// refuse is set: the decision that asks for it goes on without its read (supervise's
// readHoldForStop), so a stop held over its claim is made on no read of the row. Every other call
// goes straight to the store.
type refusedHoldRead struct {
	supervise.Store
	refuse *atomic.Bool
}

func (s refusedHoldRead) WorkHoldStands(ctx context.Context, tree string) (bool, error) {
	if s.refuse.Load() {
		return false, errors.New("the database did not answer")
	}
	return s.Store.WorkHoldStands(ctx, tree)
}

// Spec step 7, the backstop of a stop made with no read of the tree's work hold row (the stop
// asked again, supervise's readHoldForStop). The close's stop is held for the implementer's turn,
// and the opening commits while the turn runs, so it reads the implementer working and starts
// nothing of it. The turn's end is refused its read of the row, so it makes the held stop on no
// read, after the opening. The read of the row after that stop is refused too (supervise's
// holdReadAfterStop), so the turn's end answers that read's error and the daemon is not told. The
// stop's row then posts the stop again. Its decision reads no row and finds the implementer
// stopped by the hold, so the daemon applies the fact of the hold's end (intake.WorkHoldOpened).
// The row logs that the stop was made after the gate opened again, naming the implementer and the
// version that closed the gate. The fact starts the implementer once, with the opening's task:
// the close message's sentences naming version 4, that the hold stopped the turn, today's open
// message naming the person's approval of version 4, and the check of the turn.
func TestWorkHoldStartsAStopMadeWithoutTheLockAfterTheOpening(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	refuse := &atomic.Bool{}
	r.sup.deps.Store = refusedHoldRead{Store: r.trees, refuse: refuse}
	machine, token := r.working(claim.RoleImplementer), r.token(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	r.runOnce()
	if c := machine.Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
		t.Fatalf("before the opening the implementer is %s, held %v; want its stop held for its turn", c.State, c.WorkHeld())
	}

	beforeTheOpening := r.newestRow()
	r.approve(4, 12)
	// The opening's message reaches the working implementer. The stop's row waits on its backoff.
	r.runOnce()
	if got := r.startsSince(claim.RoleImplementer, beforeTheOpening); got != 0 {
		t.Fatalf("the opening queued %d starts of the implementer, want none: it read the implementer working", got)
	}
	refuse.Store(true)
	pending := machine.Claim().Pending
	if err := machine.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err == nil || !strings.Contains(err.Error(), "the database did not answer") {
		t.Fatalf("the implementer's turn end answered %v, want the refused read of the row after its stop", err)
	}
	refuse.Store(false)
	if pending != nil && !pending.ConfirmedAt.IsZero() {
		// The prompt's result behind the turn's end, as endTurn sends it.
		if err := machine.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true}); err != nil {
			t.Fatalf("report the end of the prompt the implementer ran: %v", err)
		}
	}
	r.wantHeld(claim.RoleImplementer, launched, "after its turn's end made the stop under no lock")

	r.clock.advance(65 * time.Second)
	r.runOnce()
	made := loggedMessages(t, logs.String(), "outbox: the work hold's stop was made after the design gate opened again")
	if len(made) != 1 || made[0]["role"] != string(claim.RoleImplementer) || made[0]["version"] != float64(4) {
		t.Fatalf("the stop's row logged %v, want one line naming the implementer and version 4", made)
	}
	r.drain()
	r.wantStarted(claim.RoleImplementer, launched, r.opening(4, 4)+"\n\n"+r.resumeTask())
	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("after the drain the implementer's stops are %+v, want the row finished", stops)
	}
}

// slowProcessStop is the rig's runtime with the process stop of one claim held: the stop closes
// stopping and waits until proceed closes, as the Sandbox runtime's stop waits on the pod's
// launcher for up to the boot timeout and the stop grace. Every other call goes straight to the
// fake.
type slowProcessStop struct {
	*fake.Runtime
	token             claim.Token
	stopping, proceed chan struct{}
}

func (s slowProcessStop) Suspend(ctx context.Context, loc runtime.Locator) error {
	if loc.Claim == s.token {
		select {
		case <-s.stopping:
		default:
			close(s.stopping)
		}
		<-s.proceed
	}
	return s.Runtime.Suspend(ctx, loc)
}

// R2 ("approving the version or undoing the change releases the work") and spec step 7 (the
// opening "starts each claim that the hold stopped"), for the hold's stop made now whose lock on
// the tree's work hold row the database ends while the decision runs. The implementer reads the
// close message and ends the turn it started, so the stop's row asks for the stop at once. The
// process stop takes longer than the store's idle bound (store.idleBound, 30 s), so Postgres ends
// the lock's idle transaction, and the person's approval commits the opening while the process is
// still being stopped: the opening reads the implementer live and starts nothing of it. The stop
// is then written. A claim the hold stopped while no hold stands must still be started; it must
// not wait, stopped by a hold that has ended, for the next boot of the daemon.
func TestWorkHoldStartsAStopMadeNowWhoseLockTheDatabaseEnded(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	token := r.token(claim.RoleImplementer)
	slow := slowProcessStop{Runtime: r.rt, token: token, stopping: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(slow.proceed) })
	t.Cleanup(letGo)
	r.sup.deps.Runtime = slow
	machine := r.live(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.workerClose(4) {
		t.Fatalf("the implementer was told %q, want the work close", got)
	}
	// The turn the close message starts, ended as the message asks, before the stop's row runs.
	r.clock.advance(time.Second)
	r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	endTurn(r.ctx, t, machine, token)

	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	select {
	case <-slow.stopping:
	case err := <-ran:
		t.Fatalf("the implementer's stop row returned %v before it stopped the process", err)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-during-the-stop", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("apply the approval: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the opening still waits 60 s into the process stop: the database did not end the stop decision's lock on the tree's work hold row")
	}
	letGo()
	if err := <-ran; err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	r.drain()
	r.clock.advance(65 * time.Second)
	r.drain()

	var holds int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from work_holds where tree = $1`, r.tree.key).Scan(&holds); err != nil {
		t.Fatalf("read the work holds: %v", err)
	}
	if holds != 0 {
		t.Fatalf("the tree keeps %d work hold rows after the opening, want none", holds)
	}
	if state, by := r.stored(claim.RoleImplementer); state == supervise.StateSuspended && by == supervise.SuspendedByHold {
		t.Fatalf("after the opening and the drain the implementer is stored suspended by %q with no work hold standing, "+
			"%d stop rows and %d starts queued, and the runtime started it %d times since the stop: nothing starts it before the next boot",
			by, len(r.holdStops(claim.RoleImplementer)), r.startsSince(claim.RoleImplementer, 0), r.launches(claim.RoleImplementer)-launched)
	}
}

// R2 and spec step 7, for the hold's stop made now while the opening commits. The decision of the
// stop read the tree's work hold row and let the lock go before the runtime stops the process
// (supervise's readHoldForStop). The rig's runtime holds the implementer's process stop, far
// short of the store's idle bound, and the person's approval commits the opening meanwhile
// without waiting for the row: the opening reads the implementer live and queues no start of it.
// Once the stop is written, the implementer's machine reads the row again, finds it gone, and the
// daemon applies the fact of the hold's end at once (workflowRuntime.workHoldEndedUnderStop), so a
// start of the implementer is queued before the stop's row has finished. The daemon's line names
// the issue, the role and version 4, and the drain starts the implementer once, with the
// opening's task, which names version 4 and carries today's open message.
func TestWorkHoldStartsAStopMadeWhileTheOpeningCommits(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	token := r.token(claim.RoleImplementer)
	slow := slowProcessStop{Runtime: r.rt, token: token, stopping: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(slow.proceed) })
	t.Cleanup(letGo)
	r.sup.deps.Runtime = slow
	machine := r.live(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	// The turn the close message starts, ended as the message asks, before the stop's row runs.
	r.clock.advance(time.Second)
	r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	endTurn(r.ctx, t, machine, token)

	beforeTheOpening := r.newestRow()
	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	select {
	case <-slow.stopping:
	case err := <-ran:
		t.Fatalf("the implementer's stop row returned %v before it stopped the process", err)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-while-the-process-stops", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	// The process stop is let go before the cleanup waits for either call, so a failure below
	// leaves no call waiting on the other.
	t.Cleanup(letGo)
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("apply the approval: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the opening still waits 20 s into the process stop: the stop's decision holds its lock on the tree's work hold row across the stop")
	}
	if got := r.startsSince(claim.RoleImplementer, beforeTheOpening); got != 0 {
		t.Fatalf("the opening queued %d starts of the implementer, want none: it read the implementer live", got)
	}

	letGo()
	if err := <-ran; err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	if got := r.startsSince(claim.RoleImplementer, beforeTheOpening); got != 1 {
		t.Fatalf("after the stop the outbox holds %d starts of the implementer, want one: the read of the row after the stop finds the hold ended", got)
	}
	ended := loggedMessages(t, logs.String(), "the work hold ended under a claim it stopped; the hold's opening starts the claim")
	if len(ended) != 1 || ended[0]["issue"] != r.tree.key || ended[0]["role"] != string(claim.RoleImplementer) || ended[0]["version"] != float64(4) {
		t.Fatalf("the daemon logged %v, want one line naming %s, the implementer and version 4", ended, r.tree.key)
	}
	r.drain()
	r.wantStarted(claim.RoleImplementer, launched, r.opening(4, 4)+"\n\n"+r.resumeTask())
	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("after the drain the implementer's stops are %+v, want the row finished", stops)
	}
}

// Spec step 7 ("The task says that the work hold stopped its turn, and names the version that
// closed the gate. It carries today's open message.") and check a3d61f8e ("starts each held claim
// ... with the resume task", the task the daemon composes for the opening, which starts with the
// close message's sentences), for the stop made while the opening commits: the opening reads the
// implementer live, the stop is written after it, and the daemon starts the implementer once the
// read after the stop finds the hold ended. That start is the opening's start of a claim the hold
// stopped, after a person's approval of version 4, and its task must say so: the close message's
// sentences naming version 4, that the hold stopped the turn, today's open message naming the
// person's approval of version 4, and the check of the turn. It must not tell the worker that the
// deployment no longer holds the tree's work: the deployment still holds it under `work`.
func TestWorkHoldStartsAStopMadeWhileTheOpeningCommitsWithTheOpeningsTask(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	token := r.token(claim.RoleImplementer)
	slow := slowProcessStop{Runtime: r.rt, token: token, stopping: make(chan struct{}), proceed: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(slow.proceed) })
	t.Cleanup(letGo)
	r.sup.deps.Runtime = slow
	machine := r.live(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.runOnce()
	r.clock.advance(time.Second)
	r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	endTurn(r.ctx, t, machine, token)

	ran := r.inBackground(func() error { return r.runner.RunOnce(r.ctx) })
	select {
	case <-slow.stopping:
	case err := <-ran:
		t.Fatalf("the implementer's stop row returned %v before it stopped the process", err)
	}
	opened := r.inBackground(func() error {
		result, err := intake.ApplyFact(r.ctx, r.pool, "test", "approved-4-while-the-process-stops-task", r.approval(4, 12), r.engine, r.admission)
		if err == nil && result.Refusal != nil {
			err = fmt.Errorf("the approval was refused: %+v", result.Refusal)
		}
		return err
	})
	t.Cleanup(letGo)
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("apply the approval: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the opening still waits 20 s into the process stop")
	}
	letGo()
	if err := <-ran; err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	r.drain()
	r.wantStarted(claim.RoleImplementer, launched, r.opening(4, 4)+"\n\n"+r.resumeTask())
}

// Spec step 4, H22 to H24, check a3d61f8e: with the tester stopped by the hold and the implementer
// in the turn its stop is held for, a task, a notice, a wake, a CI settlement, admission's slot
// repair, and a daemon restart during the drain and after it leave every claim the hold stopped
// stopped. A restart during the drain posts the stop again with the deadline it stored.
func TestWorkHoldLeavesEveryHeldClaimStopped(t *testing.T) {
	held := func(t *testing.T) *workHoldRig {
		t.Helper()
		r := newWorkHoldRig(t, legionTree(phase.Testing, config.HoldWork))
		r.working(claim.RoleImplementer)
		r.released(claim.RoleTester)
		r.close(4)
		r.runOnce()
		r.runOnce()
		r.wantHeld(claim.RoleTester, 0, "after the close")
		if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
			t.Fatalf("the implementer is %s, held %v; want its stop held for its turn", c.State, c.WorkHeld())
		}
		return r
	}
	for _, tc := range []struct {
		name string
		act  func(r *workHoldRig)
	}{
		{name: "a task", act: func(r *workHoldRig) {
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
				Task: "Test the empty list.", NeedsSlot: true})
		}},
		{name: "a notice", act: func(r *workHoldRig) {
			r.enqueue(record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing})
		}},
		{name: "a wake", act: func(r *workHoldRig) {
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleTester, Generation: 1,
				WakeKind: pods.WakeDelivery})
		}},
		{name: "a CI settlement", act: func(r *workHoldRig) {
			r.apply("settled", intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: "head1", Generation: 1,
				Snapshot: "head1-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: []string{"build"}})
		}},
		{name: "admission's slot repair", act: func(r *workHoldRig) { r.apply("tick", intake.ControllerTick{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := held(t)
			tc.act(r)
			r.drain()
			r.wantHeld(claim.RoleTester, 0, "after "+tc.name)
			if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() {
				t.Fatalf("after %s the implementer is %s by %q, held %v; want its stop still held", tc.name, c.State, c.SuspendedBy, c.WorkHeld())
			}
		})
	}
	t.Run("a daemon restart during the drain", func(t *testing.T) {
		r := held(t)
		deadline := r.holdStops(claim.RoleImplementer)[0].Deadline
		r.clock.advance(30 * time.Second)
		r.restart()
		// One pass posts the stops again after the restart, before the clock moves on.
		r.runOnce()
		left := deadline.Sub(r.clock.Now())
		if live, _ := r.clock.armed(left); live != 1 {
			t.Fatalf("after the restart %d timers are armed for the %s left of the stored deadline, want the stop's one", live, left)
		}
		if fired := r.clock.fire(left); fired != 1 {
			t.Fatalf("fired %d timers at the stored deadline, want the stop's one", fired)
		}
		r.drain()
		r.wantHeld(claim.RoleTester, 0, "after the restart")
		r.wantHeld(claim.RoleImplementer, 0, "after the restart and the stored deadline")
	})
	t.Run("a daemon restart after it", func(t *testing.T) {
		r := held(t)
		endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
		r.drain()
		r.restart()
		r.drain()
		r.wantHeld(claim.RoleTester, 0, "after the restart")
		r.wantHeld(claim.RoleImplementer, 0, "after the restart")
	})
}

// restart is a daemon restart: a new supervisor restores every claim of the project from the store
// on a clock of its own at the same moment, the outbox runs on it, and the boot's fact is applied.
func (r *workHoldRig) restart() {
	r.t.Helper()
	r.restartBeforeTheBoot()
	r.apply("boot", intake.DaemonBooted{})
}

// restartBeforeTheBoot is restart without the boot's fact. The daemon applies that fact beside the
// outbox (serve, workflowRuntime.runBooted), so an outbox row can run before it.
func (r *workHoldRig) restartBeforeTheBoot() {
	r.t.Helper()
	claims, err := r.trees.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	r.sup.stop()
	r.supervise(&heldClock{at: r.clock.Now()})
	if _, err := r.sup.restore(r.ctx, ofProject(claims, r.tokenProject)); err != nil {
		r.t.Fatalf("restore the claims: %v", err)
	}
	r.runner.supervisor = r.sup
}

// A restart into `merge` while the close's stop of a released tester is still queued. The daemon
// applies the boot's fact beside the outbox, so the stop's row runs before the boot ends the tree's
// hold, and the tree's work hold row still stands. Under `merge` the hold holds no running work,
// so the row makes no stop (holdStop) and finishes with one line that names the tester and the
// version. The boot then ends the hold and finds no claim that the hold stopped. The tester stays
// released, and no start tells it that the deployment no longer holds the tree's work.
func TestWorkHoldMakesNoStopAfterARestartIntoMergeBeforeTheBoot(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	r.released(claim.RoleTester)
	r.close(4)
	if stops := r.holdStops(claim.RoleTester); len(stops) != 1 {
		t.Fatalf("after the close the tester's stops are %+v, want its one row", stops)
	}

	r.restartBeforeTheBoot()
	r.deploy(config.HoldMerge)
	r.runOnce()

	if c := r.machine(claim.RoleTester).Claim(); !c.Released() {
		t.Errorf("before the boot the tester is %s by %q, want it still released: `merge` holds no running work", c.State, c.SuspendedBy)
	}
	if stops := r.holdStops(claim.RoleTester); len(stops) != 0 {
		t.Errorf("the tester's stops are %+v, want the row finished", stops)
	}
	unmade := loggedMessages(t, logs.String(), "outbox: the deployment's hold is no longer `work`, which holds no running work; the work hold's stop is not made")
	if len(unmade) != 1 || unmade[0]["role"] != string(claim.RoleTester) || unmade[0]["version"] != float64(4) {
		t.Errorf("the stop row logged %v, want one line naming the tester and version 4", unmade)
	}

	r.apply("boot", intake.DaemonBooted{})
	for _, start := range startRequestsOf(t, r.pool, r.tree.key, "start") {
		if start.Role == claim.RoleTester {
			t.Errorf("the boot queued a start of the tester with the task %q, want none: the hold never stopped it", start.Task)
		}
	}
	r.drain()
	if got := r.launches(claim.RoleTester); got != 0 {
		t.Errorf("the runtime started the tester %d times, want none", got)
	}
	if c := r.machine(claim.RoleTester).Claim(); !c.Released() {
		t.Errorf("after the boot the tester is %s by %q, want it still released", c.State, c.SuspendedBy)
	}
	if state, by := r.stored(claim.RoleTester); state != supervise.StateSuspended || by != supervise.SuspendedByReleased {
		t.Errorf("the tester is stored %s by %q, want it still released", state, by)
	}
}

// Spec step 7, H10, H11, H17, decision 18, check a3d61f8e: the opening — a person's approval, or
// Dispatch's carried approval after an exact undo (F3) — starts each claim the hold stopped, and
// no other, with one task: the phase's holder its phase's resume task behind the opening's
// sentences, a claim that holds no phase that the issue's other roles may ask it questions. The
// live architect and the operator's suspended reviewer are started by nothing.
func TestWorkHoldOpeningStartsEachHeldClaimAndNoOther(t *testing.T) {
	for _, opening := range []string{"a person's approval", "Dispatch's carried approval after an exact undo"} {
		t.Run(opening, func(t *testing.T) {
			r := newWorkHoldRig(t, carriedTree())
			r.live(claim.RoleArchitect)
			r.working(claim.RoleImplementer)
			r.released(claim.RoleTester)
			r.operatorSuspended(claim.RoleReviewer)
			r.close(45)
			r.close(46)
			r.drain()
			endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
			r.drain()
			r.wantHeld(claim.RoleImplementer, 1, "after the close")
			r.wantHeld(claim.RoleTester, 0, "after the close")
			architect, reviewer := r.launches(claim.RoleArchitect), r.launches(claim.RoleReviewer)

			if opening == "a person's approval" {
				r.approve(46, 1457)
			} else {
				r.apply("carried", r.carriedApproval())
			}
			r.drain()
			sentences := r.opening(45, 46)
			r.wantStarted(claim.RoleImplementer, 1, sentences+"\n\n"+r.resumeTask())
			r.wantStarted(claim.RoleTester, 0, sentences+"\n\n"+"The issue's other roles may ask you questions.")
			if got := r.launches(claim.RoleArchitect); got != architect {
				t.Fatalf("the opening started the live architect %d times, want none", got-architect)
			}
			if got := r.launches(claim.RoleReviewer); got != reviewer {
				t.Fatalf("the opening started the operator's suspended reviewer %d times, want none", got-reviewer)
			}
			if c := r.machine(claim.RoleReviewer).Claim(); c.SuspendedBy != supervise.SuspendedByOperator {
				t.Fatalf("the reviewer is suspended by %q, want it still the operator's", c.SuspendedBy)
			}
		})
	}
}

// The architect works the phase admitted, and the hold never holds an architect (phase.RoleFor,
// which answers the architect there): a root standing at admitted while its architect writes the
// design keeps its architect at work through the hold and is started by nothing at the opening,
// and a worker of it the hold stopped — here an implementer whose pod was released before the
// close — holds no phase of the issue, so the opening gives it the one task of a claim that holds
// none (workflow's openHeldClaims) rather than the phase's work.
func TestWorkHoldOpensAWorkerOfAnIssueAtAdmittedWithNoPhase(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Admitted, config.HoldWork))
	r.live(claim.RoleArchitect)
	r.released(claim.RoleImplementer)
	r.close(4)
	r.drain()
	r.wantHeld(claim.RoleImplementer, 0, "after the close")
	if c := r.machine(claim.RoleArchitect).Claim(); c.State != supervise.StateReady || c.WorkHeld() {
		t.Fatalf("the architect of the root at admitted is %s, held %v; want it at work and not held", c.State, c.WorkHeld())
	}
	architect := r.launches(claim.RoleArchitect)

	r.approve(4, 12)
	r.drain()
	r.wantStarted(claim.RoleImplementer, 0, r.opening(4, 4)+"\n\n"+"The issue's other roles may ask you questions.")
	if got := r.launches(claim.RoleArchitect); got != architect {
		t.Fatalf("the opening started the root's architect %d times, want none: the hold never held it", got-architect)
	}
}

// closeWhileProvisioning has the rig's next workspace provision commit the close at version before
// it provisions: the close of a start that read the gate open (the outbox's startWaitsForTheHold)
// and has not created its claim yet. It answers whether the close was made.
func (r *workHoldRig) closeWhileProvisioning(version int) func() bool {
	provision := r.runner.provision
	closed := false
	r.runner.provision = func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
		if !closed {
			closed = true
			r.close(version)
		}
		return provision(ctx, request)
	}
	return func() bool { return closed }
}

// Spec step 1 ("no phase starts"), for a start that read the gate open before the close and
// launches its claim after it. The outbox reads the gate once before a start acts, and then
// provisions, creates and launches the claim with no second read. Here the close commits while the
// implementer's first start provisions its workspace, between that read and the claim's creation,
// so the close reads no claim of the implementer, live or released. It writes the hold's stop of
// the role all the same, since a start of it is under way, and the stop runs once the start's
// lease ends: it stops the launching implementer at its ready, before the implementer takes any
// task. The opening starts it once, with the start's task behind the opening's sentences.
func TestWorkHoldStopsAStartPastTheFenceAtItsReady(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleImplementer,
			Claim: r.token(claim.RoleImplementer), Entry: 1, EntryTask: keptTask})
	})
	r.conn(claim.RoleImplementer)
	closed := r.closeWhileProvisioning(4)
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Task: keptTask, Entry: 1})
	r.runOnce()
	if !closed() {
		t.Fatal("the implementer's start provisioned no workspace, so the close was never made")
	}
	if got := r.launches(claim.RoleImplementer); got != 1 {
		t.Fatalf("the start launched the implementer %d times, want once: it read the gate open before the close", got)
	}
	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 || stops[0].HoldVersion != 4 {
		t.Fatalf("the close left the stops %+v of the implementer, want one naming version 4: a start of it was under way", stops)
	}
	r.runOnce()
	if c := r.machine(claim.RoleImplementer).Claim(); c.State == supervise.StateSuspended || !c.WorkHeld() {
		t.Fatalf("once its stop ran the implementer is %s by %q, held %v; want its launch going on with the stop held for its ready",
			c.State, c.SuspendedBy, c.WorkHeld())
	}
	readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
	r.wantHeld(claim.RoleImplementer, 1, "at its ready")
	if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 0 {
		t.Fatalf("the implementer was prompted %+v with the gate closed, want nothing: the hold stops it before any task", prompts)
	}
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the drain")
	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("the stop still waits as %+v once made, want it done", stops)
	}

	r.approve(4, 12)
	r.drain()
	r.wantStarted(claim.RoleImplementer, 1, r.opening(4, 4)+"\n\n"+keptTask)
}

// Spec step 3 ("stops each of its phase workers within 2 minutes"), for a claim the close reads
// neither live nor reached by a message, which a start that read the gate open launches after the
// close: a start that creates the claim, and a wake of a released worker that is not a merger. The
// close writes the stop of either with the deadline 2 minutes after the close and no turn, as
// every other stop of the hold stores its deadline in its row. The hold at the ready lives in
// memory alone: a daemon restart after the stop's first attempt forgets it, and a first attempt
// whose read of the tree's work hold row the store refuses holds nothing. Either way the ready
// comes first, the implementer is sent its task and starts its turn, and the stop's next attempt
// meets a working claim. It cuts the turn at the deadline the row stores, with the implementer's
// session kept, so no phase turn runs to its end under the closed gate.
func TestWorkHoldCutsAStartPastTheFenceAtTheDeadlineOfTheClose(t *testing.T) {
	for _, start := range []string{"a start that creates the claim", "a wake of a released worker"} {
		for _, miss := range []string{"a restart before its ready", "a first attempt whose read the store refused"} {
			t.Run(start+"/"+miss, func(t *testing.T) {
				r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
				refuse := &atomic.Bool{}
				var store supervise.Store = refusedHoldRead{Store: r.trees, refuse: refuse}
				r.inTx(func(tx pgx.Tx) error {
					return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleImplementer,
						Claim: r.token(claim.RoleImplementer), Entry: 1, EntryTask: keptTask})
				})
				deadline := r.clock.Now().Add(holdGrace)
				closed := func() bool { return false }
				if start == "a start that creates the claim" {
					r.sup.deps.Store = store
					r.conn(claim.RoleImplementer)
					closed = r.closeWhileProvisioning(4)
					r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1,
						Phase: phase.Implementing, Task: keptTask, Entry: 1})
				} else {
					r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1,
						WakeKind: pods.WakeDelivery})
					made := &atomic.Bool{}
					r.sup.deps.Store = closeAtAStart{Store: store, token: r.token(claim.RoleImplementer), row: r.newestRow(), made: made,
						act: func() { r.close(4) }}
					closed = made.Load
					r.releasedWithATask(claim.RoleImplementer, keptTask, 1)
				}
				r.runOnce()
				if !closed() {
					t.Fatal("the close was never made while the start ran")
				}
				stops := r.holdStops(claim.RoleImplementer)

				switch miss {
				case "a restart before its ready":
					r.runOnce()
					if c := r.machine(claim.RoleImplementer).Claim(); c.State == supervise.StateSuspended || !c.WorkHeld() {
						t.Fatalf("once its stop ran the implementer is %s by %q, held %v; want the stop held for its ready",
							c.State, c.SuspendedBy, c.WorkHeld())
					}
					r.restart()
				default:
					refuse.Store(true)
					r.runOnce()
					refuse.Store(false)
					if c := r.machine(claim.RoleImplementer).Claim(); c.WorkHeld() {
						t.Fatalf("the stop whose read the store refused left the implementer %s, held %v; want nothing held", c.State, c.WorkHeld())
					}
				}
				token := r.token(claim.RoleImplementer)
				launched := r.launches(claim.RoleImplementer)
				readyClaimAs(t, r.sup, token, claim.RoleImplementer, "ses-implementer")
				r.machine(claim.RoleImplementer).Wait()
				if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, keptTask) {
					t.Fatalf("at its ready the implementer was prompted %+v, want its task: no stop met the ready", prompts)
				}
				r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})

				r.clock.advance(65 * time.Second)
				r.runOnce()
				left := deadline.Sub(r.clock.Now())
				if live, _ := r.clock.armed(left); live != 1 {
					t.Fatalf("once the stop met the implementer's turn, %d timers are armed for the %s left of the close's 2 minutes, want the stop's one",
						live, left)
				}
				if fired := r.clock.fire(left); fired != 1 {
					t.Fatalf("fired %d timers at the close's 2 minutes, want the stop's one", fired)
				}
				r.wantHeld(claim.RoleImplementer, launched, "2 minutes after the close")
				if c := r.machine(claim.RoleImplementer).Claim(); c.Session != "ses-implementer" {
					t.Fatalf("the stopped implementer holds the session %q, want its own kept", c.Session)
				}
				if len(stops) != 1 || !stops[0].Deadline.Equal(deadline) || !stops[0].TurnAfter.IsZero() || stops[0].HoldVersion != 4 {
					t.Fatalf("the close left the stops %+v of the implementer, want one cut at %s, waiting for no turn, naming version 4",
						stops, deadline)
				}
			})
		}
	}
}

// The same window under `merge`, which stops nothing: a start of the merger that read the gate
// open before the close launches the merger with its task after it. The close read no claim of
// the merger, so its message would meet no session that holds the role and finish undelivered.
// It waits for the merger's session instead, as a woken merger's close does, and the merger is
// told today's close once its session holds its topic.
func TestMergeHoldTellsAMergerWhoseStartReadTheGateBeforeTheClose(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldMerge))
	merger := r.token(claim.RoleMerger)
	r.conn(claim.RoleMerger)
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
	closed := r.closeWhileProvisioning(4)
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
		Task: "Merge the pull request."})
	r.runOnce()
	if !closed() {
		t.Fatal("the merger's start provisioned no workspace, so the close was never made")
	}
	if got := r.launches(claim.RoleMerger); got != 1 {
		t.Fatalf("the start launched the merger %d times, want once: it read the gate open before the close", got)
	}
	r.runOnce()
	if got := r.published(claim.RoleMerger); len(got) != 0 {
		t.Fatalf("the merger was told %q before its session held its topic, want nothing yet", got)
	}
	// A role message's row names its role by the claim token.
	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
		t.Fatalf("the close message to the merger left %d rows before the merger's session held its topic, want it waiting", got)
	}

	r.clock.advance(65 * time.Second)
	readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
	r.publisher.setAbsent()
	r.runOnce()
	if got := r.published(claim.RoleMerger); len(got) != 1 || got[0] != r.mergeClose(4) {
		t.Fatalf("the merger was told %q, want today's close, %q", got, r.mergeClose(4))
	}
}

// closeAtAStart is the rig's store with one act run at the claim write that records a start of
// one claim (supervise.Machine.StartedBy), once: the start read the gate open and has not brought
// the claim back yet. Every other call goes straight to the store.
type closeAtAStart struct {
	supervise.Store
	token claim.Token
	row   int64
	made  *atomic.Bool
	act   func()
}

func (s closeAtAStart) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token && c.LastStartRow == s.row && s.made.CompareAndSwap(false, true) {
		s.act()
	}
	return s.Store.PutClaim(ctx, c)
}

// The same window under `merge`, for a merger whose claim runs nothing and is not released: the
// daemon suspended it, which a start resumes by rule, or it failed, which a start retries. The
// close commits while a lane runs the merger's start, after the start read the gate open and
// before it brings the merger back, so the close marks its message to the merger to wait for the
// merger's session (workflow's mergersToWake). The message's first attempt runs then, while the
// claim is still gone and no session holds the role. It waits for the start rather than finish
// undelivered. The start brings the merger back with its merging task, the message reaches the
// merger's session once the merger is back, and the merger is prompted with its task after that.
func TestMergeHoldTellsAGoneMergerWhoseStartRunsAtTheClose(t *testing.T) {
	suspended := supervise.Claim{Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByDaemon, Session: "ses-merger", SessionFile: "/tmp/merger.jsonl"}
	failed := supervise.Claim{Generation: 1, ServingGeneration: 1, State: supervise.StateFailed, Failure: "pane exited",
		Session: "ses-merger", SessionFile: "/tmp/merger.jsonl"}
	for _, tc := range []struct {
		name    string
		gone    supervise.Claim
		restart bool
	}{
		{name: "suspended by the daemon", gone: suspended},
		{name: "failed", gone: failed},
		{name: "suspended by the daemon/a restart before its ready", gone: suspended, restart: true},
		{name: "failed/a restart before its ready", gone: failed, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldMerge))
			merger := r.token(claim.RoleMerger)
			r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
				Task: "Merge the pull request."})
			made := &atomic.Bool{}
			r.sup.deps.Store = closeAtAStart{Store: r.trees, token: merger, row: r.newestRow(), made: made, act: func() {
				r.close(4)
				r.runOnce()
			}}
			r.create(claim.RoleMerger, tc.gone)
			r.runOnce()
			if !made.Load() {
				t.Fatal("the merger's start recorded no start, so the close was never made")
			}
			if got := r.launches(claim.RoleMerger); got != 1 {
				t.Fatalf("the start launched the merger %d times, want once: it read the gate open before the close", got)
			}
			// A role message's row names its role by the claim token.
			if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
				t.Fatalf("the close message to the merger (%s) left %d rows once its first attempt ran, want it waiting for the start", tc.name, got)
			}
			if got := r.published(claim.RoleMerger); len(got) != 0 {
				t.Fatalf("the merger was told %q before it was back, want nothing yet", got)
			}

			if tc.restart {
				// The close message's wait is its row, so a daemon restart while the merger launches
				// keeps it, and the restarted daemon publishes it to the merger that is back.
				r.restart()
			}
			r.publisher.setAbsent()
			r.clock.advance(65 * time.Second)
			r.runOnce()
			if got := r.published(claim.RoleMerger); len(got) != 1 || got[0] != r.mergeClose(4) {
				t.Fatalf("the merger back from its start was told %q, want today's close, %q", got, r.mergeClose(4))
			}
			if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 0 {
				t.Fatalf("the merger was prompted %+v before its ready, want nothing yet", prompts)
			}
			readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
			r.machine(claim.RoleMerger).Wait()
			if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, "Merge the pull request.") {
				t.Fatalf("at its ready the merger was prompted %+v, want its merging task once, after the close", prompts)
			}
			if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 0 {
				t.Fatalf("the close message to the merger left %d rows once it was told, want it finished", got)
			}
		})
	}
}

// Spec step 7 ("names the version that closed the gate. It carries today's open message."), for a
// start of the opening that is still queued across a second close and a second opening. The
// opening of version 4 queues a start of each claim the hold stopped, and neither runs before the
// close of version 5. They wait at the fence, the opening of version 5 writes no second start, and
// each start then gives the words of the newest opening, built when it runs: the close of version
// 5 and the approval of version 5, once.
func TestWorkHoldOpeningStartQueuedAcrossAnotherCloseGivesTheNewestOpening(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.working(claim.RoleImplementer)
	r.released(claim.RoleTester)
	r.close(4)
	r.drain()
	endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the first close")
	r.wantHeld(claim.RoleTester, 0, "after the first close")

	r.approve(4, 12)
	r.close(5)
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the second close")
	r.wantHeld(claim.RoleTester, 0, "after the second close")
	r.approve(5, 13)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if got := r.startsSince(role, 0); got != 1 {
			t.Fatalf("after the second opening the outbox holds %d starts of the %s, want the first opening's one", got, role)
		}
	}
	r.drain()
	opening := r.opening(5, 5)
	r.wantStarted(claim.RoleImplementer, 1, opening+"\n\n"+r.resumeTask())
	r.wantStarted(claim.RoleTester, 0, opening+"\n\n"+"The issue's other roles may ask you questions.")
}

// Spec step 7 ("one task"), for a start of the opening that carries no phase work and a newer
// start of the same role that carries its phase's. The opening starts the implementer, which
// holds no phase then, with the task of a claim that holds none, and that start waits on its
// backoff. A move back then gives the implementer its phase, and that start runs first: it does
// not give way to the opening's start, and starts the implementer with one task, the phase's work
// behind the opening's sentences. The opening's start then meets a claim the hold no longer
// holds, and finishes without acting, and admission owes the implementer nothing more.
func TestWorkHoldGivesOneTaskWhenAPhaseStartMeetsAnOpeningStartWithNoPhase(t *testing.T) {
	r := heldAtTheClose(t)
	r.drain()
	r.wantHeld(claim.RoleImplementer, 0, "after the close")
	r.approve(4, 12)
	if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = $2 where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $3`, r.tree.key, r.clock.Now().Add(10*time.Minute),
		string(claim.RoleImplementer)); err != nil {
		t.Fatalf("put the implementer's opening start on its backoff: %v", err)
	}
	r.runOnce()

	r.apply("back", intake.BackwardMove{Issue: r.tree.key, Requester: claim.RoleTester, To: phase.Implementing,
		Reason: "the empty list is not covered"})
	var work string
	for _, start := range startRequestsOf(t, r.pool, r.tree.key, "start") {
		if start.Role == claim.RoleImplementer && start.Opening == "" {
			work = start.Task
		}
	}
	if work == "" {
		t.Fatal("the move back queued no start of the implementer's phase")
	}
	r.runOnce()
	r.clock.advance(11 * time.Minute)
	r.drain()
	r.apply("tick", intake.ControllerTick{})
	r.drain()

	r.wantStarted(claim.RoleImplementer, 0, r.opening(4, 4)+"\n\n"+work)
	if got := r.startsSince(claim.RoleImplementer, 0); got != 0 {
		t.Fatalf("the outbox still holds %d starts of the implementer, want none", got)
	}
	readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
	r.drain()
	if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, work) {
		t.Fatalf("the implementer was prompted %+v, want one task, its phase's work", prompts)
	}
}

// notConfirmedOnce is the listener with the first publish to topic unconfirmed
// (notify.ErrNotDelivered), which the outbox retries on the row's backoff, and every other publish
// answered as holderPublisher answers it.
type notConfirmedOnce struct {
	*holderPublisher
	topic  string
	missed bool
}

func (p *notConfirmedOnce) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	if topic == p.topic && !p.missed {
		p.missed = true
		return fmt.Errorf("publish to %s: listener returned 504: %w", topic,
			notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-implementer", EventID: key})
	}
	return p.holderPublisher.Publish(ctx, topic, message, payload, key)
}

// A close message that meets its tree's gate open again before its publish is not published, and
// posts no stop, under both `gates.hold` words. The close's message to a released merger wakes it,
// and the gate opens while its pod comes back: the open message meets the launching merger, which
// holds no topic yet, and finishes undelivered. The close's row then finds the gate open and
// finishes with one line, so the merger's last word is not "closed". A live worker's close message
// whose publish its holder did not confirm finds the gate open at its retry in the same way: the
// worker is told the opening alone. So does the architect's close message under `work`, which
// carries no stop and no wake: it is marked as a close message (record.RoleMessage.Close).
func TestGatesHoldPublishesNoCloseMessageOnceTheGateOpensAgain(t *testing.T) {
	const unpublished = "outbox role message finished unpublished: the design gate opened again before its close message was published"
	for _, hold := range []config.Hold{config.HoldMerge, config.HoldWork} {
		t.Run("a woken merger under "+string(hold), func(t *testing.T) {
			r := mergingWithAReleasedMerger(t, hold)
			logs := &testlog.Buffer{}
			r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
			merger := r.token(claim.RoleMerger)
			r.close(4)
			r.runOnce()
			if got := r.launches(claim.RoleMerger); got != 1 {
				t.Fatalf("the close's message started the released merger %d times, want once", got)
			}
			r.approve(4, 12)
			r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
			r.runOnce()
			r.publisher.setAbsent()
			r.clock.advance(65 * time.Second)
			readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
			r.runOnce()

			if got := r.published(claim.RoleMerger); len(got) != 0 {
				t.Fatalf("the merger was told %q, want nothing: the gate opened before the close was published", got)
			}
			if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
				t.Fatalf("the outbox still holds %d role messages, want the close finished", got)
			}
			if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
				t.Fatalf("the outbox holds the stops %+v of the merger, want none", stops)
			}
			if lines := loggedMessages(t, logs.String(), unpublished); len(lines) != 1 || lines[0]["role"] != string(merger) {
				t.Fatalf("the outbox logged %v, want one line naming the merger", lines)
			}
			r.drain()
			if c := r.machine(claim.RoleMerger).Claim(); c.State != supervise.StateReady || c.WorkHeld() || c.SuspensionHeld {
				t.Fatalf("the merger is %s, held %v, a stop held %v; want it ready and not stopped", c.State, c.WorkHeld(), c.SuspensionHeld)
			}
		})
	}
	t.Run("a retried close to a live worker", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		logs := &testlog.Buffer{}
		r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
		r.working(claim.RoleImplementer)
		implementer := r.token(claim.RoleImplementer)
		r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: notify.RoleTopicPrefix + string(implementer)}
		r.close(4)
		r.runOnce()
		if got := r.published(claim.RoleImplementer); len(got) != 0 {
			t.Fatalf("the implementer was told %q, want the close unconfirmed and retried", got)
		}
		r.approve(4, 12)
		r.runOnce()
		r.clock.advance(65 * time.Second)
		r.runOnce()

		if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.openedTo(claim.RoleImplementer, 4) {
			t.Fatalf("the implementer was told %q, want the opening alone", got)
		}
		if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
			t.Fatalf("the outbox still holds %d role messages, want the close finished", got)
		}
		if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
			t.Fatalf("the outbox holds the stops %+v of the implementer, want none", stops)
		}
		if lines := loggedMessages(t, logs.String(), unpublished); len(lines) != 1 || lines[0]["role"] != string(implementer) {
			t.Fatalf("the outbox logged %v, want one line naming the implementer", lines)
		}
		if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateWorking || c.WorkHeld() {
			t.Fatalf("the implementer is %s, held %v; want it at work and not stopped", c.State, c.WorkHeld())
		}
	})
	t.Run("a retried close to a live architect under work", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		logs := &testlog.Buffer{}
		r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
		r.live(claim.RoleArchitect)
		architect := r.token(claim.RoleArchitect)
		r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: notify.RoleTopicPrefix + string(architect)}
		r.close(4)
		r.runOnce()
		if got := r.published(claim.RoleArchitect); len(got) != 0 {
			t.Fatalf("the architect was told %q, want the close unconfirmed and retried", got)
		}
		r.approve(4, 12)
		r.runOnce()
		r.clock.advance(65 * time.Second)
		r.runOnce()

		// The opening sends the architect a notice of the approval on its topic too, so the
		// role messages are read apart from it: the opening is told, and the close never is.
		closed := r.closedSentence(4) + " " + holdSentence + " Your own work goes on."
		told := r.published(claim.RoleArchitect)
		opened := false
		for _, message := range told {
			if message == closed {
				t.Fatalf("the architect was told %q, want no close: the gate opened before the close was published", told)
			}
			opened = opened || message == r.openedTo(claim.RoleArchitect, 4)
		}
		if !opened {
			t.Fatalf("the architect was told %q, want the opening", told)
		}
		if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
			t.Fatalf("the outbox still holds %d role messages, want the close finished", got)
		}
		if lines := loggedMessages(t, logs.String(), unpublished); len(lines) != 1 || lines[0]["role"] != string(architect) {
			t.Fatalf("the outbox logged %v, want one line naming the architect", lines)
		}
	})
}

// A close message reads its tree's standing gate at the head of each attempt (closeOutrun), and
// an issue the record does not hold names no tree to read. Under both `gates.hold` words the
// attempt is an error, as every read of an issue the daemon keeps no record of is: the row stays
// queued for its next attempt with that error, is not published, and is not finished as a close
// whose gate opened again.
func TestGatesHoldKeepsTheCloseMessageOfAnIssueTheRecordDoesNotHold(t *testing.T) {
	const unpublished = "outbox role message finished unpublished: the design gate opened again before its close message was published"
	for _, hold := range []config.Hold{config.HoldMerge, config.HoldWork} {
		t.Run(string(hold), func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, hold))
			logs := &testlog.Buffer{}
			r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
			const unrecorded = "LEGION-299"
			token, err := claim.NewToken(r.tokenProject, unrecorded, claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, unrecorded,
				record.RoleMessage{Role: string(token), Body: r.mergeClose(4), Close: true}, r.clock.Now()))
			r.runOnce()

			var rows, attempts int
			var lastError string
			if err := r.pool.QueryRow(r.ctx, `select count(*), coalesce(max(attempts), 0), coalesce(max(last_error), '') from outbox
				where issue = $1 and kind = $2`, unrecorded, string(record.OutboxKindRoleMessage)).Scan(&rows, &attempts, &lastError); err != nil {
				t.Fatalf("read the close message of %s: %v", unrecorded, err)
			}
			if rows != 1 || attempts != 1 || !strings.Contains(lastError, unrecorded+" is not recorded") {
				t.Fatalf("the close message of %s left %d rows after %d attempts with the error %q, want it queued with the error that the issue is not recorded",
					unrecorded, rows, attempts, lastError)
			}
			if lines := loggedMessages(t, logs.String(), unpublished); len(lines) != 0 {
				t.Fatalf("the outbox logged %v, want no close finished as one whose gate opened again", lines)
			}
			topic := notify.RoleTopicPrefix + string(token)
			_, delivered := r.publisher.snapshot()
			for _, publish := range delivered {
				if publish.topic == topic {
					t.Fatalf("the close message of %s was published to %s, want nothing published", unrecorded, topic)
				}
			}
		})
	}
}

// Under `merge`, a close message to a merger with no claim waits while a lane runs a start of the
// merger, and not while that start is only queued. Here the start runs at the
// close, so the close marks its message to wait, and its provision then fails: the start row
// waits on its backoff, and its next attempt reads the closed gate and waits at the fence through
// the hold. So the message's attempt after that finds no lane running the start and no claim,
// and the row finishes with one line and no stop rather than wait until the gate opens.
func TestMergeHoldWaitsForNoMergerWhoseStartIsOnlyQueuedAfterTheClose(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldMerge))
	merger := r.token(claim.RoleMerger)
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
	closed := false
	r.runner.provision = func(context.Context, workspace.Request) (workspace.Workspace, error) {
		if !closed {
			closed = true
			r.close(4)
		}
		return workspace.Workspace{}, errors.New("the workspace could not be provisioned")
	}
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
		Task: "Merge the pull request."})
	r.runOnce()
	if !closed {
		t.Fatal("the merger's start provisioned no workspace, so the close was never made")
	}
	r.clock.advance(65 * time.Second)
	r.runOnce()

	// A role message's row names its role by the claim token.
	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 0 {
		t.Fatalf("the close message to the merger left %d rows while its start waits at the fence, want it finished", got)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 1 {
		t.Fatalf("the merger's start left %d rows, want it waiting at the fence", got)
	}
	if got := r.launches(claim.RoleMerger); got != 0 {
		t.Fatalf("the runtime started the merger %d times, want none", got)
	}
}

// The work hold's stop of a claim that runs nothing yet and is not gone. A queued claim and a
// claim whose launch is uncertain after a restart can still launch: a start that passed the fence
// before the close spawns a queued claim, and the boot's relaunch releases an uncertain launch to
// queued and spawns it with no fence (launchUnfinished). So the stop waits for the launch on
// its row's backoff. That wait is routine: the row logs no error, and no warning however long the
// hold stands, where a wait that is not routine is a warning from its seventh attempt
// (waitWarnAttempts). The claim's own start waits at the hold's fence meanwhile. Once the gate
// opens, the stop's next decision reads no work hold row, and the row finishes with the one line
// that says the stop is not made.
func TestWorkHoldStopOfAClaimThatRunsNothingIsNoError(t *testing.T) {
	const unmade = "outbox: the design gate opened again before the work hold's stop was made; the stop is not made"
	for _, state := range []supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain} {
		t.Run(string(state), func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			logs := &testlog.Buffer{}
			r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
			r.create(claim.RoleImplementer, supervise.Claim{Generation: 1, ServingGeneration: 1, State: state,
				Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"})
			r.publisher.setAbsent(notify.RoleTopicPrefix + string(r.token(claim.RoleImplementer)))
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleImplementer, Generation: 1,
				Phase: phase.Implementing, Task: "Implement the plan."})
			r.close(4)
			r.runOnce()
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
				t.Fatalf("the close message left the stops %+v of the implementer, want one", stops)
			}
			var stop int64
			if err := r.pool.QueryRow(r.ctx, `select id from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'suspend'`,
				r.tree.key).Scan(&stop); err != nil {
				t.Fatalf("read the stop's row: %v", err)
			}
			// The lines the outbox logged of the stop's row alone, the start's row waiting beside it.
			ofTheStop := func(msg string) []map[string]any {
				var lines []map[string]any
				for _, line := range loggedMessages(t, logs.String(), msg) {
					if line["row"] == float64(stop) {
						lines = append(lines, line)
					}
				}
				return lines
			}
			// Eight attempts of the stop, 65 seconds apart: past the seventh, from which a wait that
			// is not routine is logged as a warning.
			r.drain()
			if lines := ofTheStop("outbox row failed"); len(lines) != 0 {
				t.Fatalf("the outbox logged %v, want the stop's row never failed", lines)
			}
			waits := ofTheStop("outbox row waits")
			if len(waits) == 0 {
				t.Fatal("the outbox logged no wait of the stop's row, want its first attempt's")
			}
			for _, line := range waits {
				if line["level"] != slog.LevelInfo.String() {
					t.Fatalf("the outbox logged the stop's wait as %v, want each line at INFO: the wait is routine", line)
				}
			}
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
				t.Fatalf("the stop of the %s implementer left %+v, want it waiting for the launch", state, stops)
			}
			if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 1 {
				t.Fatalf("the implementer's start left %d rows, want it waiting at the hold's fence", got)
			}
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != state {
				t.Fatalf("the implementer is %s, want it still %s", c.State, state)
			}

			r.approve(4, 12)
			r.runOnce()
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
				t.Fatalf("the stop still waits as %+v once the gate opened, want it finished", stops)
			}
			if lines := ofTheStop(unmade); len(lines) != 1 || lines[0]["role"] != string(claim.RoleImplementer) || lines[0]["version"] != float64(4) {
				t.Fatalf("the stop's row logged %v, want one line that its stop is not made, naming the implementer and version 4", lines)
			}
			if lines := ofTheStop("outbox row failed"); len(lines) != 0 {
				t.Fatalf("the outbox logged %v, want the stop's row never failed", lines)
			}
		})
	}
}

// unresponsiveHolder is the listener with every publish to topic refused as a holder past its live
// window and inside its gone horizon (notify.ErrHolderUnresponsive), and every other publish
// answered as holderPublisher answers it.
type unresponsiveHolder struct {
	*holderPublisher
	topic string
}

func (p *unresponsiveHolder) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	if topic == p.topic {
		return fmt.Errorf("publish to %s: listener returned 404: %w", topic, notify.ErrHolderUnresponsive)
	}
	return p.holderPublisher.Publish(ctx, topic, message, payload, key)
}

// Spec step 3, for a worker whose holder the listener reports unresponsive: its session
// is not dead and can still be in a turn. The close message to it does not wait for that session
// with its stop unposted: the row ends, and the stop is posted with its deadline 2 minutes on, so
// the worker's turn is cut then, its session kept.
func TestWorkHoldStopsAWorkerWhoseHolderIsUnresponsive(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.working(claim.RoleImplementer)
	launched := r.launches(claim.RoleImplementer)
	r.runner.notices = &unresponsiveHolder{holderPublisher: r.publisher, topic: notify.RoleTopicPrefix + string(r.token(claim.RoleImplementer))}
	r.close(4)
	reached := r.clock.Now()
	r.runOnce()
	stops := r.holdStops(claim.RoleImplementer)
	if len(stops) != 1 || !stops[0].Deadline.Equal(reached.Add(holdGrace)) {
		t.Fatalf("the close message to the unresponsive holder left the stops %+v, want one cut at %s", stops, reached.Add(holdGrace))
	}
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the outbox still holds %d role messages, want the close finished", got)
	}
	r.runOnce()
	if fired := r.clock.fire(holdGrace); fired != 1 {
		t.Fatalf("fired %d timers at the 2 minutes, want the stop's one", fired)
	}
	r.wantHeld(claim.RoleImplementer, launched, "2 minutes after the close message")
}

// A close message whose stop a newer close outran. The close of version 4 reaches the
// working implementer unconfirmed, and its row waits on its backoff. The gate opens, the open
// message reaches the implementer, and the close of version 5 reaches it with its own stop. The
// version 4 row then meets the standing hold of version 5: it is not published, so the implementer
// is not told the older close after the newer one, and it posts no second stop.
func TestWorkHoldPublishesNoCloseThatANewerCloseOutran(t *testing.T) {
	const outran = "outbox role message finished unpublished: a newer close of the design gate stands"
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	r.working(claim.RoleImplementer)
	implementer := r.token(claim.RoleImplementer)
	r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: notify.RoleTopicPrefix + string(implementer)}
	r.close(4)
	r.runOnce()
	r.approve(4, 12)
	r.runOnce()
	r.close(5)
	r.runOnce()
	r.clock.advance(65 * time.Second)
	r.runOnce()

	if got := r.published(claim.RoleImplementer); len(got) != 2 || got[0] != r.openedTo(claim.RoleImplementer, 4) || got[1] != r.workerClose(5) {
		t.Fatalf("the implementer was told %q, want the opening and then the close of version 5 alone", got)
	}
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the outbox still holds %d role messages, want the older close finished", got)
	}
	if lines := loggedMessages(t, logs.String(), outran); len(lines) != 1 || lines[0]["role"] != string(implementer) {
		t.Fatalf("the outbox logged %v, want one line naming the implementer", lines)
	}
	for _, stop := range r.holdStops(claim.RoleImplementer) {
		if stop.HoldVersion != 5 {
			t.Fatalf("the outbox holds the stop %+v, want only the stop of version 5", stop)
		}
	}
}

// Spec step 7 ("one task"), for a claim that an opening started and a later close stopped before
// any turn took the opening's task. The holders took the approved version 3 (withNews). The
// opening of version 4 starts the implementer, with the news of version 4 after its work, and the
// close of version 5 reaches it while it launches: its close message meets no session that holds
// the role, and its stop is held until the implementer's ready and made there, before the
// implementer takes the task. The claim keeps that task, the first opening's sentences in front of
// its work, and the daemon restarts, so the claim is read back from the store. The opening of
// version 5 then starts it with one task: the newest opening's sentences once, then the work once,
// and not the first opening's sentences behind them, then the news the claim is owed now, of
// version 5, in place of the kept task's news of version 4.
func TestWorkHoldOpeningOverAnEarlierOpeningsTaskGivesTheNewestSentencesOnce(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork).withNews())
	token := r.token(claim.RoleImplementer)
	r.working(claim.RoleImplementer)
	r.close(4)
	r.drain()
	endTurn(r.ctx, t, r.machine(claim.RoleImplementer), token)
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the first close")

	r.approve(4, 12)
	r.runOnce()
	work := r.resumeTask()
	if c := r.machine(claim.RoleImplementer).Claim(); c.State == supervise.StateSuspended || c.Pending == nil ||
		c.Pending.Task != r.opening(4, 4)+"\n\n"+work+"\n\n"+r.heldNews(phase.Implementing, 3, 4) {
		t.Fatalf("after the first opening the implementer is %s by %q holding %+v, want it launching with the first opening's task",
			c.State, c.SuspendedBy, c.Pending)
	}
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(token))
	r.close(5)
	r.drain()
	readyClaimAs(t, r.sup, token, claim.RoleImplementer, "ses-implementer")
	r.publisher.setAbsent()
	r.drain()
	r.wantHeld(claim.RoleImplementer, 2, "after the second close")
	if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 0 {
		t.Fatalf("the implementer was prompted %+v with the gate closed, want nothing: the stop is made at its ready", prompts)
	}
	if c := r.machine(claim.RoleImplementer).Claim(); c.Pending == nil || c.Pending.Task != r.opening(4, 4)+"\n\n"+work+"\n\n"+r.heldNews(phase.Implementing, 3, 4) ||
		c.Pending.Opening != r.opening(4, 4) {
		t.Fatalf("the stopped implementer keeps %+v, want the first opening's task, marked with its sentences", c.Pending)
	}
	// The restart's supervisor runs on a runtime of its own, whose launches count from none.
	r.restart()
	if c := r.machine(claim.RoleImplementer).Claim(); c.Pending == nil || c.Pending.Opening != r.opening(4, 4) {
		t.Fatalf("after the restart the implementer keeps %+v, want the first opening's task read back marked with its sentences", c.Pending)
	}
	launched := r.launches(claim.RoleImplementer)

	r.approve(5, 13)
	r.drain()
	r.wantStarted(claim.RoleImplementer, launched, r.opening(5, 5)+"\n\n"+work+"\n\n"+r.heldNews(phase.Implementing, 3, 5))
}

// The merger's news under `work`, for a merger whose cut turn left it a task no turn took. The
// holders took the approved version 3 (withNews). The merger is at work on its merge task when the
// gate closes, and its turn does not end within the 2 minutes, so the hold's stop cuts it. The
// claim keeps the task, which its cut turn did not finish. The opening's start waits until what
// changed in the approved version is recorded, as every start of a merger waits, and then starts
// the merger with one task: the opening's sentences, the kept merge task, and the news of version
// 4 after it, so the merger reads what changed before it merges. A restart before the opening
// reads the kept task back from the store, and a first attempt of the opening's start that waits
// for the news to be recorded gives the same task.
func TestWorkHoldOpeningGivesAMergerTheNewsWithTheTaskItsCutTurnKept(t *testing.T) {
	const mergeTask = "Merge the pull request of LEGION-208."
	for _, miss := range []string{"none", "a restart before the opening", "a first attempt that waits for the news"} {
		t.Run(miss, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldWork).withNews())
			token := r.token(claim.RoleMerger)
			r.live(claim.RoleMerger)
			r.handle(claim.RoleMerger, supervise.RequestDeliver{Claim: token, Task: mergeTask, ID: "outbox:9001", Row: 9001,
				Phase: phase.Merging, Generation: 1, Entry: 1})
			r.handle(claim.RoleMerger, supervise.StreamTurnStart{Claim: token})
			r.close(4)
			r.runOnce()
			r.runOnce()
			if fired := r.clock.fire(holdGrace); fired != 1 {
				t.Fatalf("fired %d timers at the 2 minutes, want the merger's stop's one", fired)
			}
			r.wantHeld(claim.RoleMerger, 1, "2 minutes after the close message")
			if c := r.machine(claim.RoleMerger).Claim(); c.Pending == nil || c.Pending.Task != mergeTask || !c.Pending.ConfirmedAt.IsZero() {
				t.Fatalf("the stopped merger keeps %+v, want the merge task its cut turn worked, unconfirmed", c.Pending)
			}
			launched := 1
			if miss == "a restart before the opening" {
				// The restart's supervisor runs on a runtime of its own, whose launches count from none.
				r.restart()
				launched = 0
			}

			r.approve(4, 12)
			if miss == "a first attempt that waits for the news" {
				if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = $2 where issue = $1 and kind = 'design_changes'`,
					r.tree.key, r.clock.Now().Add(10*time.Minute)); err != nil {
					t.Fatalf("put the reconcile of the news on its backoff: %v", err)
				}
				r.runOnce()
				r.wantHeld(claim.RoleMerger, launched, "while what changed is not recorded")
				if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 1 {
					t.Fatalf("the outbox holds %d starts of the merger, want the opening's start waiting for the news", got)
				}
				r.clock.advance(11 * time.Minute)
			}
			r.drain()
			task := r.opening(4, 4) + "\n\n" + mergeTask + "\n\n" + r.heldNews(phase.Merging, 3, 4)
			r.wantStarted(claim.RoleMerger, launched, task)
			readyClaimAs(t, r.sup, token, claim.RoleMerger, "ses-merger")
			r.machine(claim.RoleMerger).Wait()
			if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) == 0 || !strings.HasSuffix(prompts[len(prompts)-1].Message, task) {
				t.Fatalf("at its ready the merger was prompted %+v, want the one task with the news after the merge task", prompts)
			}
		})
	}
}

// Under `merge`, a merger whose start is only queued at the close, its lane running nothing of
// it, is not waited for. That start reads the closed gate at its next attempt and waits at the
// fence through the hold, so it launches nothing, and a close message that waited for the
// merger's session would wait the whole hold and keep the issue's pod from its idle release. The
// message meets no session that holds the role and finishes, the start waits for the gate, and
// once the gate opens the start launches the merger with its task.
func TestMergeHoldWaitsForNoMergerWhoseStartIsOnlyQueued(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldMerge))
	merger := r.token(claim.RoleMerger)
	r.conn(claim.RoleMerger)
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
		Task: "Merge the pull request."})
	r.close(4)
	r.drain()
	// A role message's row names its role by the claim token.
	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 0 {
		t.Fatalf("the close message to the merger left %d rows, want it finished: nothing launches the merger while the gate is closed", got)
	}
	if got := r.launches(claim.RoleMerger); got != 0 {
		t.Fatalf("the runtime started the merger %d times with the gate closed, want none", got)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 1 {
		t.Fatalf("the merger's start left %d rows with the gate closed, want it waiting at the fence", got)
	}

	r.approve(4, 12)
	r.drain()
	if got := r.launches(claim.RoleMerger); got != 1 {
		t.Fatalf("the runtime started the merger %d times once the gate opened, want once", got)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleMerger); got != 0 {
		t.Fatalf("the merger's start still waits (%d rows) once the gate opened, want it run", got)
	}
	readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
	r.publisher.setAbsent()
	r.drain()
	if got := r.published(claim.RoleMerger); len(got) != 0 {
		t.Fatalf("the merger was told %q, want nothing: it launched after the gate opened", got)
	}
	if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, "Merge the pull request.") {
		t.Fatalf("the merger was prompted %+v once the gate opened, want its phase's task", prompts)
	}
}

// Under `merge`, the close marks its message to a merger whose start a lane runs, so the message
// waits for the merger's session (workflow's mergersToWake). Here that start never creates the
// claim: the close commits while the start provisions its workspace, the provision fails, and the
// start row then goes before it runs again, as a start of an issue that left the workflow goes.
// While the start is under way, the message waits. Once no start of the merger is under way and
// no claim of it exists, nothing creates the claim while the gate stays closed, so the message
// finishes with one line and no stop rather than wait until the gate opens.
func TestMergeHoldEndsTheCloseOfAMergerWhoseStartCreatesNoClaim(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldMerge))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	merger := r.token(claim.RoleMerger)
	r.publisher.setAbsent(notify.RoleTopicPrefix + string(merger))
	closed := false
	r.runner.provision = func(context.Context, workspace.Request) (workspace.Workspace, error) {
		if !closed {
			closed = true
			r.close(4)
		}
		return workspace.Workspace{}, errors.New("the workspace could not be provisioned")
	}
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
		Task: "Merge the pull request."})
	r.runOnce()
	if !closed {
		t.Fatal("the merger's start provisioned no workspace, so the close was never made")
	}
	// A role message's row names its role by the claim token.
	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
		t.Fatalf("the close message to the merger left %d rows while its start is under way, want it waiting", got)
	}

	r.inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(r.ctx, `delete from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
			and payload->>'role' = $2`, r.tree.key, string(claim.RoleMerger))
		return err
	})
	r.clock.advance(65 * time.Second)
	r.runOnce()

	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 0 {
		t.Fatalf("the close message to the merger left %d rows once no start of it was under way, want it finished", got)
	}
	const ended = "outbox role message finished undelivered: its merger runs nothing, and no start of it can launch it while the gate is closed"
	if lines := loggedMessages(t, logs.String(), ended); len(lines) != 1 || lines[0]["role"] != string(merger) {
		t.Fatalf("the outbox logged %v, want one line naming the merger", lines)
	}
	if got := r.launches(claim.RoleMerger); got != 0 {
		t.Fatalf("the runtime started the merger %d times, want none", got)
	}
	if got := r.published(claim.RoleMerger); len(got) != 0 {
		t.Fatalf("the merger was told %q, want nothing", got)
	}
	if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
		t.Fatalf("the outbox holds the stops %+v of the merger, want none", stops)
	}
}

// launchingWithATask is the implementer's claim as a start leaves it once it spawned the claim
// with its task, the task of the root's phase at entry 1, which no turn has taken: stored
// launching at generation 1, before the daemon recorded its process. A restart in that instant
// restores it launch_uncertain (supervisor.restore). The implementer's phases row names it at
// that entry.
func (r *workHoldRig) launchingWithATask() *supervise.Machine {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleImplementer,
			Claim: r.token(claim.RoleImplementer), Entry: 1, EntryTask: keptTask})
	})
	machine := r.create(claim.RoleImplementer, supervise.Claim{Generation: 1, State: supervise.StateLaunching})
	r.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: r.token(claim.RoleImplementer), Task: keptTask, ID: "outbox:9001",
		Row: 9001, Phase: r.tree.at, Generation: 1, Entry: 1})
	if c := machine.Claim(); c.State != supervise.StateLaunching || c.Locator != nil || c.Pending == nil || c.Pending.Task != keptTask {
		r.t.Fatalf("the implementer is %s with the process %+v, holding %+v; want it launching with no process, holding its task",
			c.State, c.Locator, c.Pending)
	}
	return machine
}

// The work hold's stop holds the relaunch of a launch that a restart left uncertain. A start
// spawned the implementer with its task, and the daemon stored the claim launching before it
// recorded the process. The close message meets no session that holds the role, and its stop
// holds the launch until the agent's ready. The daemon then restarts: the hold at the ready lived
// in memory, and the claim is restored launch_uncertain. The stop's next attempt runs before the
// boot's relaunch and is refused there, so it waits on its row's backoff. The relaunch releases
// the claim and spawns it with nothing holding it (launchUnfinished), and the implementer
// reaches its ready, is sent its task and starts its turn. The stop's next attempt, at most a
// backoff of 64 seconds later, meets that turn and cuts it at the deadline the row stores, 2
// minutes after the message, or at once where the deadline has passed. A first attempt after the
// restart whose read the store refused is asked again on its backoff, and the attempt after it
// meets the uncertain launch as above. The row is a restart's own.
func TestWorkHoldStopsTheRelaunchOfAnUncertainLaunchWithinTheBackoffOfItsReady(t *testing.T) {
	for _, tc := range []struct {
		name          string
		late, refused bool
	}{
		{name: "the ready before the deadline"},
		{name: "the ready past the deadline", late: true},
		{name: "a first attempt whose read the store refused", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			r.refuseHoldRead = &atomic.Bool{}
			token := r.token(claim.RoleImplementer)
			r.launchingWithATask()
			r.publisher.setAbsent(notify.RoleTopicPrefix + string(token))
			r.close(4)
			deadline := r.clock.Now().Add(holdGrace)
			r.runOnce()
			r.runOnce()
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateLaunching || !c.WorkHeld() {
				t.Fatalf("before the restart the implementer is %s, held %v; want its stop held for its ready", c.State, c.WorkHeld())
			}

			r.restart()
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateLaunchUncertain || c.WorkHeld() {
				t.Fatalf("after the restart the implementer is %s, held %v; want its launch uncertain and nothing held", c.State, c.WorkHeld())
			}
			r.refuseHoldRead.Store(tc.refused)
			r.clock.advance(2 * time.Second)
			r.runOnce()
			r.refuseHoldRead.Store(false)
			if tc.refused {
				r.clock.advance(2 * time.Second)
				r.runOnce()
			}
			if tc.late {
				r.clock.advance(holdGrace)
				r.runOnce()
			}
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
				t.Fatalf("the stop that met the uncertain launch left %+v, want it waiting for the relaunch", stops)
			}

			r.bootRelaunch(claim.RoleImplementer)
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateLaunching || c.WorkHeld() {
				t.Fatalf("after the boot's relaunch the implementer is %s, held %v; want it launching with nothing held", c.State, c.WorkHeld())
			}
			launched := r.launches(claim.RoleImplementer)
			readyClaimAs(t, r.sup, token, claim.RoleImplementer, "ses-implementer")
			r.machine(claim.RoleImplementer).Wait()
			if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, keptTask) {
				t.Fatalf("at its ready the relaunched implementer was prompted %+v, want its task: nothing held its relaunch", prompts)
			}
			r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})

			r.clock.advance(record.RetryBackoffMost + time.Second)
			r.runOnce()
			if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() {
				t.Fatalf("a backoff after its ready the implementer is %s, held %v; want the stop to have met its turn", c.State, c.WorkHeld())
			}
			left := max(0, deadline.Sub(r.clock.Now()))
			if live, _ := r.clock.armed(left); live != 1 {
				t.Fatalf("once the stop met the turn, %d timers are armed for the %s left of the 2 minutes, want the stop's one", live, left)
			}
			if fired := r.clock.fire(left); fired != 1 {
				t.Fatalf("fired %d timers at the 2 minutes, want the stop's one", fired)
			}
			r.wantHeld(claim.RoleImplementer, launched, "at the deadline of the stop")
			if c := r.machine(claim.RoleImplementer).Claim(); c.Session != "ses-implementer" {
				t.Fatalf("the stopped implementer holds the session %q, want its own kept", c.Session)
			}
		})
	}
}

// The work hold's stop that lands between the two calls of the boot's relaunch of
// an uncertain launch (launchUnfinished): the release, which leaves the claim queued at the
// generation it had (supervise.Claim.ReleasedLaunch), and the spawn. A start spawned the
// implementer with its task, the close message met no session that holds the role and queued its
// stop, and the daemon restarted before the stop's first attempt, so the claim is restored
// launch_uncertain. The stop's first attempt meets the claim the release left queued, and waits
// for its launch on its row's backoff rather than finish. Its next attempt meets the spawned
// launch and holds it until the agent's ready, where the hold stops the implementer before it
// takes its task. A second restart between the release and the spawn makes the claim an uncertain
// launch again, which the stop waits for in the same way, and the next boot's relaunch is stopped
// at its ready. A first attempt between the two calls whose read the store refused is asked again
// on its backoff, and the attempt after it meets the queued claim as above.
func TestWorkHoldStopsTheBootsReleasedLaunchAtItsReady(t *testing.T) {
	for _, tc := range []struct {
		name             string
		restart, refused bool
	}{
		{name: "a stop between the release and the spawn"},
		{name: "a restart between the release and the spawn", restart: true},
		{name: "a first attempt whose read the store refused", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			r.refuseHoldRead = &atomic.Bool{}
			token := r.token(claim.RoleImplementer)
			r.launchingWithATask()
			r.publisher.setAbsent(notify.RoleTopicPrefix + string(token))
			r.close(4)
			r.runOnce()
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
				t.Fatalf("the close message left the stops %+v of the implementer, want one", stops)
			}
			r.restart()
			machine := r.machine(claim.RoleImplementer)
			if c := machine.Claim(); c.State != supervise.StateLaunchUncertain {
				t.Fatalf("after the restart the implementer is %s, want its launch uncertain", c.State)
			}

			// The relaunch's first call: the release.
			if released, err := machine.ReleaseUncertainLaunch(r.ctx); err != nil || !released {
				t.Fatalf("release the implementer's uncertain launch = %t, %v; want it released to queued", released, err)
			}
			r.refuseHoldRead.Store(tc.refused)
			r.runOnce()
			r.refuseHoldRead.Store(false)
			if tc.refused {
				r.clock.advance(record.RetryBackoffMost + time.Second)
				r.runOnce()
			}
			if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
				t.Fatalf("the stop that met the released launch left %+v, want it waiting for the spawn", stops)
			}
			if c := machine.Claim(); c.State != supervise.StateQueued {
				t.Fatalf("between the release and the spawn the implementer is %s, want it queued", c.State)
			}

			if tc.restart {
				r.restart()
				if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateLaunchUncertain {
					t.Fatalf("after the second restart the implementer is %s, want its launch uncertain again", c.State)
				}
				r.clock.advance(record.RetryBackoffMost + time.Second)
				r.runOnce()
				if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
					t.Fatalf("the stop that met the uncertain launch left %+v, want it waiting for the relaunch", stops)
				}
				r.bootRelaunch(claim.RoleImplementer)
			} else {
				// The relaunch's second call: the spawn, with nothing holding the claim.
				r.handle(claim.RoleImplementer, supervise.RequestSpawn{Claim: token})
			}
			if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateLaunching || c.WorkHeld() {
				t.Fatalf("after the spawn the implementer is %s, held %v; want it launching with nothing held yet", c.State, c.WorkHeld())
			}
			launched := r.launches(claim.RoleImplementer)
			r.clock.advance(record.RetryBackoffMost + time.Second)
			r.runOnce()
			if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() || c.State == supervise.StateSuspended {
				t.Fatalf("once the stop met the launch the implementer is %s, held %v; want the stop held for its ready", c.State, c.WorkHeld())
			}
			readyClaimAs(t, r.sup, token, claim.RoleImplementer, "ses-implementer")
			r.machine(claim.RoleImplementer).Wait()
			r.wantHeld(claim.RoleImplementer, launched, "at its ready")
			if prompts := r.conn(claim.RoleImplementer).Prompts(); len(prompts) != 0 {
				t.Fatalf("the implementer was prompted %+v with the gate closed, want nothing: the hold stops it before its task", prompts)
			}
			if c := r.machine(claim.RoleImplementer).Claim(); c.Pending == nil || c.Pending.Task != keptTask {
				t.Fatalf("the stopped implementer holds %+v, want the task it kept", c.Pending)
			}
		})
	}
}

// wantMergerToldThenStopped holds the merger to the close of version 4, published to its session
// at told before any stop of it: it was told the work close once and is not stopped, and its one
// stop, queued once the message reached it, waits for the turn the message starts and cuts that
// turn 2 minutes after told. The stop's row then holds the merger for that turn, and the 2 minutes
// stop it, with its session kept.
func (r *workHoldRig) wantMergerToldThenStopped(told time.Time) {
	r.t.Helper()
	if got := r.published(claim.RoleMerger); len(got) != 1 || got[0] != r.workerClose(4) {
		r.t.Fatalf("the merger was told %q, want the work close, %q", got, r.workerClose(4))
	}
	if c := r.machine(claim.RoleMerger).Claim(); c.State == supervise.StateSuspended || c.WorkHeld() {
		r.t.Fatalf("when the close reached it the merger is %s by %q, held %v; want it not stopped yet", c.State, c.SuspendedBy, c.WorkHeld())
	}
	stops := r.holdStops(claim.RoleMerger)
	if len(stops) != 1 || !stops[0].Deadline.Equal(told.Add(holdGrace)) || !stops[0].TurnAfter.Equal(told) {
		r.t.Fatalf("the merger's stops are %+v, want one queued once the close reached it, cut at %s", stops, told.Add(holdGrace))
	}
	launched := r.launches(claim.RoleMerger)
	r.runOnce()
	if c := r.machine(claim.RoleMerger).Claim(); c.State == supervise.StateSuspended || !c.WorkHeld() {
		r.t.Fatalf("once its stop ran the merger is %s, held %v; want its stop held for the turn the close starts", c.State, c.WorkHeld())
	}
	if fired := r.clock.fire(holdGrace); fired != 1 {
		r.t.Fatalf("fired %d timers at the 2 minutes, want the merger's stop's one", fired)
	}
	r.wantHeld(claim.RoleMerger, launched, "2 minutes after the close reached it")
	if c := r.machine(claim.RoleMerger).Claim(); c.Session != "ses-merger" {
		r.t.Fatalf("the stopped merger holds the session %q, want its own kept", c.Session)
	}
}

// Under `work`, the close message to a merger whose launch is under
// way at the close, its session not holding the role's topic yet, is marked to wait for the
// merger's session, as a woken merger's is, and carries the merger's stop. The merger takes its
// pull request out of GitHub's merge queue on this message alone. So the message waits rather
// than end at a role no session holds, and no stop is posted meanwhile. Once the merger is ready
// and its session holds its topic, the merger is told the close, and only then is its stop
// posted. The wait is the message's row: a daemon restart while the merger launches keeps it, and
// a first publish its holder did not confirm is retried on the row's backoff.
func TestWorkHoldTellsALaunchingMergerTheCloseBeforeItsStop(t *testing.T) {
	for _, miss := range []string{"", "a restart while the merger launches", "a first publish its holder did not confirm"} {
		name := miss
		if name == "" {
			name = "the merger's session comes up"
		}
		t.Run(name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldWork))
			merger := r.token(claim.RoleMerger)
			topic := notify.RoleTopicPrefix + string(merger)
			r.create(claim.RoleMerger, supervise.Claim{State: supervise.StateQueued})
			r.handle(claim.RoleMerger, supervise.RequestSpawn{Claim: merger})
			r.publisher.setAbsent(topic)
			if miss == "a first publish its holder did not confirm" {
				r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: topic}
			}
			r.close(4)
			r.runOnce()
			r.clock.advance(65 * time.Second)
			r.runOnce()
			// A role message's row names its role by the claim token.
			if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
				t.Fatalf("the close message to the launching merger left %d rows, want it waiting for the merger's session", got)
			}
			if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
				t.Fatalf("the outbox holds the stops %+v of the merger before it was told the close, want none", stops)
			}

			if miss == "a restart while the merger launches" {
				r.restart()
			}
			readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
			r.publisher.setAbsent()
			r.clock.advance(65 * time.Second)
			told := r.clock.Now()
			r.runOnce()
			r.wantMergerToldThenStopped(told)
		})
	}
}

// Under `work`, a merger's close message whose holder the listener
// reports unresponsive waits for that holder, with its stop not posted, where a worker's close
// that is not a merger's ends and posts its stop (TestWorkHoldStopsAWorkerWhoseHolderIsUnresponsive).
// The merger takes its pull request out of the merge queue on this message alone. This holds for
// the released merger the close wakes, and for a merger live at the close, whose message the
// close marks in the same way (record.RoleMessage.Wake). Once the holder answers, the merger is
// told the close, and only then is its stop posted. A daemon restart while the holder is
// unresponsive keeps the row, and a first publish its holder did not confirm is retried on the
// row's backoff: neither ends the wait.
func TestWorkHoldWaitsForAnUnresponsiveMergerBeforeItsStop(t *testing.T) {
	for _, merger := range []string{"a woken released merger", "a live merger"} {
		for _, miss := range []string{"", "a restart while its holder is unresponsive", "a first publish its holder did not confirm"} {
			name := merger
			if miss != "" {
				name += "/" + miss
			}
			t.Run(name, func(t *testing.T) {
				var r *workHoldRig
				if merger == "a woken released merger" {
					r = mergingWithAReleasedMerger(t, config.HoldWork)
				} else {
					r = newWorkHoldRig(t, legionTree(phase.Merging, config.HoldWork))
					r.live(claim.RoleMerger)
				}
				token := r.token(claim.RoleMerger)
				topic := notify.RoleTopicPrefix + string(token)
				r.close(4)
				if merger == "a woken released merger" {
					// The message's first attempt wakes the merger, and its agent becomes ready.
					r.runOnce()
					r.clock.advance(65 * time.Second)
					readyClaimAs(t, r.sup, token, claim.RoleMerger, "ses-merger")
				}
				if miss == "a first publish its holder did not confirm" {
					r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: topic}
					r.runOnce()
					r.clock.advance(65 * time.Second)
				}
				r.runner.notices = &unresponsiveHolder{holderPublisher: r.publisher, topic: topic}
				waits := func(when string) {
					t.Helper()
					r.runOnce()
					// A role message's row names its role by the claim token.
					if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(token)); got != 1 {
						t.Fatalf("%s the close message to the merger left %d rows, want it waiting for the merger's holder", when, got)
					}
					if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
						t.Fatalf("%s the outbox holds the stops %+v of the merger, want none before it is told the close", when, stops)
					}
					if got := r.published(claim.RoleMerger); len(got) != 0 {
						t.Fatalf("%s the merger was told %q, want nothing yet", when, got)
					}
				}
				waits("while its holder is unresponsive")
				if miss == "a restart while its holder is unresponsive" {
					r.restart()
					r.clock.advance(65 * time.Second)
					waits("after the restart")
				}

				r.runner.notices = r.publisher
				r.clock.advance(65 * time.Second)
				told := r.clock.Now()
				r.runOnce()
				r.wantMergerToldThenStopped(told)
			})
		}
	}
}

// Under `work`, a merger whose start read the gate open before
// the close launches the merger after it, its pull request still in GitHub's merge queue. The
// close reads no claim of the merger, live or released, and a start of it under way
// (StartUnderWay). It writes no stop of that merger at once, which would stop the merger at its
// ready before it reads the close. Its close message carries the stop instead, and waits for the
// merger's session, as a woken merger's message does. The start launches the merger with its
// task, the merger is told the close once its session holds its topic, and only then is its stop
// posted. A daemon restart before the merger's ready keeps the message's row, and a first publish
// its holder did not confirm is retried on the row's backoff.
func TestWorkHoldTellsAMergerWhoseStartReadTheGateBeforeTheCloseBeforeItsStop(t *testing.T) {
	for _, miss := range []string{"", "a restart before its ready", "a first publish its holder did not confirm"} {
		name := miss
		if name == "" {
			name = "the merger's session comes up"
		}
		t.Run(name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldWork))
			merger := r.token(claim.RoleMerger)
			topic := notify.RoleTopicPrefix + string(merger)
			r.conn(claim.RoleMerger)
			r.publisher.setAbsent(topic)
			closed := r.closeWhileProvisioning(4)
			r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging,
				Task: "Merge the pull request."})
			r.runOnce()
			if !closed() {
				t.Fatal("the merger's start provisioned no workspace, so the close was never made")
			}
			if got := r.launches(claim.RoleMerger); got != 1 {
				t.Fatalf("the start launched the merger %d times, want once: it read the gate open before the close", got)
			}
			if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
				t.Fatalf("the close left the stops %+v of the merger, want none: its close message carries its stop", stops)
			}
			if miss == "a first publish its holder did not confirm" {
				r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: topic}
			}
			r.runOnce()
			// A role message's row names its role by the claim token.
			if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
				t.Fatalf("the close message to the merger left %d rows before its session held its topic, want it waiting", got)
			}
			if got := r.published(claim.RoleMerger); len(got) != 0 {
				t.Fatalf("the merger was told %q before its session held its topic, want nothing yet", got)
			}

			if miss == "a restart before its ready" {
				r.restart()
			}
			r.clock.advance(65 * time.Second)
			readyClaimAs(t, r.sup, merger, claim.RoleMerger, "ses-merger")
			r.machine(claim.RoleMerger).Wait()
			if prompts := r.conn(claim.RoleMerger).Prompts(); len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, "Merge the pull request.") {
				t.Fatalf("at its ready the merger was prompted %+v, want its start's task: the start read the gate open", prompts)
			}
			r.publisher.setAbsent()
			told := r.clock.Now()
			r.runOnce()
			r.wantMergerToldThenStopped(told)
		})
	}
}

// openedBeforeGatesRead is the runner's record store with act run once, armed, right before the
// next read of a tree's design gates: the opening commits after the attempt opened its
// transaction and before that transaction's first read. Every other call goes straight to the
// store.
type openedBeforeGatesRead struct {
	record.Store
	armed *atomic.Bool
	act   func()
}

func (s openedBeforeGatesRead) GatesOf(ctx context.Context, tx pgx.Tx, issue string) (map[record.DesignStep]record.DesignGate, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.act()
	}
	return s.Store.GatesOf(ctx, tx, issue)
}

// A close message reads its tree's standing gate and the tree's record of its work hold in one
// REPEATABLE READ transaction (closeOutrun), so both reads come from one snapshot. The snapshot
// starts at the transaction's first read, the read of the gate. Here the opening commits after
// the close message's attempt opened its transaction and right before that read. The snapshot
// holds the opening, so the gate reads open: the close is not published, the implementer is told
// the opening alone, and no stop is posted. A daemon restart or a failed attempt cannot change the
// outcome: the next attempt reads the opening committed, and the open gate ends the row
// (TestGatesHoldPublishesNoCloseMessageOnceTheGateOpensAgain).
func TestWorkHoldPublishesNoCloseWhoseHoldEndedAfterItReadTheGate(t *testing.T) {
	const unpublished = "outbox role message finished unpublished: the design gate opened again before its close message was published"
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	r.working(claim.RoleImplementer)
	implementer := r.token(claim.RoleImplementer)
	r.close(4)
	armed := &atomic.Bool{}
	armed.Store(true)
	r.runner.records = openedBeforeGatesRead{Store: r.records, armed: armed, act: func() { r.approve(4, 12) }}
	r.runOnce()
	if armed.Load() {
		t.Fatal("the close message's attempt read no design gate, so the opening was never made")
	}
	r.drain()

	if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.openedTo(claim.RoleImplementer, 4) {
		t.Fatalf("the implementer was told %q, want the opening alone: the hold ended before the close was published", got)
	}
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the outbox still holds %d role messages, want the close finished", got)
	}
	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("the outbox holds the stops %+v of the implementer, want none", stops)
	}
	if lines := loggedMessages(t, logs.String(), unpublished); len(lines) != 1 || lines[0]["role"] != string(implementer) {
		t.Fatalf("the outbox logged %v, want one line naming the implementer", lines)
	}
	if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateWorking || c.WorkHeld() {
		t.Fatalf("the implementer is %s, held %v; want it at work and not stopped", c.State, c.WorkHeld())
	}
}

// Each gated step numbers the versions of its own document, so a close message's stop names the
// step and the version of the close that sent it (record.HoldStop), and a close message is
// outrun where the hold that stands names another step or another version (closeOutrun). The
// deployment gates the Verification document and the spec. The close of version 4 of the spec
// reaches the working implementer unconfirmed, and its row waits on its backoff. The spec's
// approval opens the gate, and the open message reaches the implementer. Version 2 of the
// Verification document closes the gate again, and its close message reaches the implementer
// with its own stop. The spec's row then meets the standing hold of the criteria step at version
// 2, a version below its own 4: it is not published, and it posts no stop.
func TestWorkHoldPublishesNoCloseOfAnotherDocumentThatANewerCloseOutran(t *testing.T) {
	const outran = "outbox role message finished unpublished: a newer close of the design gate stands"
	const criteria = "artifact-208-criteria"
	tree := legionTree(phase.Implementing, config.HoldWork)
	tree.steps = []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}
	r := newWorkHoldRig(t, tree)
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	approved := 1
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: r.tree.key, Step: record.DesignStepCriteria, ArtifactID: criteria,
			LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 2}, EverOpened: true}); err != nil {
			return err
		}
		// The holders took the approved Verification document too, so the spec's opening owes
		// them no news of it (holdTree).
		for _, audience := range []record.DesignAudience{record.AudienceHolder, record.AudienceArchitect} {
			if err := r.records.RaiseTold(r.ctx, tx, r.tree.key, audience,
				[]record.NewsItem{{Step: record.DesignStepCriteria, To: approved}}, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
	r.working(claim.RoleImplementer)
	implementer := r.token(claim.RoleImplementer)
	r.runner.notices = &notConfirmedOnce{holderPublisher: r.publisher, topic: notify.RoleTopicPrefix + string(implementer)}
	r.close(4)
	r.runOnce()
	r.approve(4, 12)
	r.runOnce()
	r.apply("criteria-version-2", intake.DispatchArtifact{Key: r.tree.key, ArtifactID: criteria, Kind: intake.DispatchArtifactVersion, Version: 2})
	r.runOnce()
	r.clock.advance(65 * time.Second)
	r.runOnce()

	criteriaClose := "The design gate of LEGION-208 closed: version 2 of its criteria document (" + criteria + ") is with its reviewer, " +
		"so the design you are working to may change. " + holdSentence + " End your turn now."
	if got := r.published(claim.RoleImplementer); len(got) != 2 || got[0] != r.openedTo(claim.RoleImplementer, 4) || got[1] != criteriaClose {
		t.Fatalf("the implementer was told %q, want the opening and then the close of the criteria step alone", got)
	}
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the outbox still holds %d role messages, want the spec's close finished", got)
	}
	if lines := loggedMessages(t, logs.String(), outran); len(lines) != 1 || lines[0]["role"] != string(implementer) ||
		lines[0]["step"] != string(record.DesignStepSpec) || lines[0]["version"] != float64(4) ||
		lines[0]["standingStep"] != string(record.DesignStepCriteria) || lines[0]["standing"] != float64(2) {
		t.Fatalf("the outbox logged %v, want one line naming the implementer, the spec at version 4 and the criteria at version 2", lines)
	}
	for _, stop := range r.holdStops(claim.RoleImplementer) {
		if stop.HoldVersion != 2 {
			t.Fatalf("the outbox holds the stop %+v, want only the stop of the criteria's close", stop)
		}
	}
}

// A restart into `merge` while the work close message to a live merger is still queued, the
// gate still closed. The boot ends the tree's work hold, since `merge` holds no running work, and
// the gate stays closed. The merger takes its pull request out of GitHub's merge queue on the
// close message alone, so a hold that ended with the gate still closed does not outrun the
// message: the merger is told the close, and then the boot's sentence that the deployment no
// longer holds the tree's work. Under `merge` the message's stop is not made, and one line names
// the merger and the version that closed the gate.
func TestWorkHoldTellsAMergerTheCloseAfterARestartIntoMerge(t *testing.T) {
	const unmade = "outbox: the deployment's hold is no longer `work`, which holds no running work; the work hold's stop is not made"
	r := newWorkHoldRig(t, legionTree(phase.Merging, config.HoldWork))
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	r.live(claim.RoleMerger)
	merger := r.token(claim.RoleMerger)
	r.close(4)
	// A role message's row names its role by the claim token.
	if got := r.rows(record.OutboxKindRoleMessage, "", claim.Role(merger)); got != 1 {
		t.Fatalf("the close left %d messages to the merger, want its one close message queued", got)
	}

	r.restartBeforeTheBoot()
	r.deploy(config.HoldMerge)
	r.apply("boot", intake.DaemonBooted{})
	r.drain()

	bootOpened := "The deployment no longer holds the work of " + r.tree.key + "'s tree."
	if got := r.published(claim.RoleMerger); len(got) != 2 || got[0] != r.workerClose(4) || got[1] != bootOpened {
		t.Fatalf("the merger was told %q, want the work close and then that the deployment no longer holds the tree's work", got)
	}
	if got := r.rows(record.OutboxKindRoleMessage, "", ""); got != 0 {
		t.Fatalf("the outbox still holds %d role messages, want each published", got)
	}
	if stops := r.holdStops(claim.RoleMerger); len(stops) != 0 {
		t.Fatalf("the outbox holds the stops %+v of the merger, want none: `merge` holds no running work", stops)
	}
	if lines := loggedMessages(t, logs.String(), unmade); len(lines) != 1 || lines[0]["role"] != string(claim.RoleMerger) || lines[0]["version"] != float64(4) {
		t.Fatalf("the outbox logged %v, want one line naming the merger and version 4", lines)
	}
	if c := r.machine(claim.RoleMerger).Claim(); c.State == supervise.StateSuspended || c.WorkHeld() || c.SuspensionHeld {
		t.Fatalf("the merger is %s by %q, held %v, a stop held %v; want it running and not stopped", c.State, c.SuspendedBy, c.WorkHeld(), c.SuspensionHeld)
	}
}

// The work hold's stop that a close message's publish queues (postHoldStop) is a supervise row the
// outbox writes in a transaction of its own, outside any fact, so that transaction opens the commit
// scope itself (outbox.committing), as every writer of a row must (projectRecords.wakeOnCommit).
// Without the scope the records wake the supervise lane at the write, inside the transaction: the
// lane looks before the stop's row is there, finds nothing, and the stop waits for the outbox's
// sweep. The tester of INFRA-4528 saw the whole daemon log "outbox row written outside a commit
// scope" from postHoldStop at every close of its drive.
func TestWorkHoldStopRowWakesItsLaneOnlyOnceItCommits(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	logs := &testlog.Buffer{}
	var mu sync.Mutex
	var seen []int
	r.runner.records = projectRecords{Store: r.records, project: r.tree.project, log: slog.New(slog.NewJSONHandler(logs, nil)),
		wake: func(_ string, kind record.OutboxKind) {
			if kind != record.OutboxKindSupervise {
				return
			}
			visible := len(r.holdStops(claim.RoleImplementer))
			mu.Lock()
			seen = append(seen, visible)
			mu.Unlock()
		}}
	r.working(claim.RoleImplementer)
	r.close(4)
	r.runOnce()

	if stops := r.holdStops(claim.RoleImplementer); len(stops) != 1 {
		t.Fatalf("the implementer's stops are %+v, want the one the close message's publish queued", stops)
	}
	if lines := loggedLines(t, logs.String(), "outbox row written outside a commit scope; its lane is woken at once"); len(lines) != 0 {
		t.Errorf("the hold's stop was written outside a commit scope: %v", lines)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no wake of the supervise lane followed the stop's write")
	}
	for _, visible := range seen {
		if visible != 1 {
			t.Fatalf("the supervise lane was woken with %v stops visible, want each wake to find the committed stop", seen)
		}
	}
}

// An operator's act overrides the work hold, and the daemon drops no instruction without saying
// so: the opening keeps an operator's task queued behind the hold's stop, the claim takes it first,
// and the opening's task follows it (supervise.Machine.EndHold), end to end through the outbox. The
// operator gives the implementer a task while its turn runs, the close's stop is held for that
// turn, and the turn's end stops the claim with the operator's task kept. The opening's start then
// starts the claim with that task, and the start's own delivery waits behind it on its row's
// backoff (supervise's queue, ErrDeliveryPending). The implementer is prompted with the operator's
// task alone, and once that task's turn is over, with one task that names the version that closed
// the gate and carries today's open message. A daemon restart between the two deliveries leaves
// the opening's task owed, and the start's next attempt delivers it once.
func TestWorkHoldOpeningGivesTheOperatorsTaskFirstAndItsOwnTaskAfter(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "no restart"
		if restart {
			name = "a daemon restart between the two deliveries"
		}
		t.Run(name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			token := r.token(claim.RoleImplementer)
			r.working(claim.RoleImplementer)
			r.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: token, Task: heldOperatorTask})
			r.close(4)
			r.runOnce()
			r.runOnce()
			endTurn(r.ctx, t, r.machine(claim.RoleImplementer), token)
			r.wantHeld(claim.RoleImplementer, 1, "after the close")
			if kept := r.machine(claim.RoleImplementer).Claim().Pending; kept == nil || kept.Task != heldOperatorTask {
				t.Fatalf("the stopped implementer keeps %+v, want the operator's task", kept)
			}

			r.approve(4, 12)
			r.drain()
			if got := r.startsSince(claim.RoleImplementer, 0); got != 1 {
				t.Fatalf("after the opening the outbox holds %d starts of the implementer, want the opening's start waiting behind the operator's task", got)
			}
			readyClaimAs(t, r.sup, token, claim.RoleImplementer, "ses-implementer")
			machine := r.machine(claim.RoleImplementer)
			machine.Wait()
			prompts := r.conn(claim.RoleImplementer).Prompts()
			if len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, heldOperatorTask) || strings.Contains(prompts[0].Message, r.openedMessage(4)) {
				t.Fatalf("at its ready the implementer was prompted %+v, want the operator's task alone", prompts)
			}
			taken := machine.Claim().Pending
			if taken == nil || taken.Task != heldOperatorTask {
				t.Fatalf("the implementer holds %+v once prompted, want the operator's task", taken)
			}
			r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: taken.ID})
			endTurn(r.ctx, t, machine, token)
			if restart {
				r.restart()
			}

			r.wantPrompts("after the operator's turn", heldOperatorTask, r.opening(4, 4)+"\n\n"+r.resumeTask())
			if got := r.startsSince(claim.RoleImplementer, 0); got != 0 {
				t.Fatalf("the outbox still holds %d starts of the implementer, want the opening's start finished", got)
			}
		})
	}
}

// openingOwed is whether the told row of the root's phase holder owes the gate's opening
// (record.DesignTold.OpeningOwed), false where no such row stands.
func (r *workHoldRig) openingOwed() bool {
	r.t.Helper()
	var owed bool
	err := r.pool.QueryRow(r.ctx, `select opening_owed from design_news_told where issue = $1 and audience = $2`,
		r.tree.key, string(record.AudienceHolder)).Scan(&owed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.t.Fatalf("read the holder's told row of %s: %v", r.tree.key, err)
	}
	return owed
}

// heldOperatorTask is the task an operator gives the implementer in the rigs of heldWithAnOperatorsTask.
const heldOperatorTask = "Read the daemon log of the last hour and report what failed."

// noPhaseOpeningWork is the work the opening's start gives a claim it stopped that holds no phase
// (workflow's openHeldClaims), written out here.
const noPhaseOpeningWork = "The issue's other roles may ask you questions."

// heldWithAnOperatorsTask is tree, a root in testing under `work`, whose implementer holds no phase
// and kept an operator's task when the close stopped it: the operator gave the task while the
// implementer's turn ran, and the close's stop, held for that turn, was made at the turn's end.
// With testerStopped, the tester, the holder of the phase, is released before the close, so the
// close stops it too.
func heldWithAnOperatorsTask(t *testing.T, tree holdTree, testerStopped bool) *workHoldRig {
	t.Helper()
	r := newWorkHoldRig(t, tree)
	if testerStopped {
		r.released(claim.RoleTester)
		r.inTx(func(tx pgx.Tx) error {
			return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleTester, Claim: r.token(claim.RoleTester), Entry: 1})
		})
	}
	token := r.token(claim.RoleImplementer)
	r.working(claim.RoleImplementer)
	r.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: token, Task: heldOperatorTask})
	r.close(4)
	r.runOnce()
	r.runOnce()
	endTurn(r.ctx, t, r.machine(claim.RoleImplementer), token)
	r.drain()
	r.wantHeld(claim.RoleImplementer, 1, "after the close")
	if kept := r.machine(claim.RoleImplementer).Claim().Pending; kept == nil || kept.Task != heldOperatorTask {
		t.Fatalf("the stopped implementer keeps %+v, want the operator's task", kept)
	}
	return r
}

// openedWithItsStartOnBackoff is a person's approval of version 4, which opens the gate, with the
// opening's start of the implementer, which carries no phase work, put on its backoff, and one
// pass of the outbox after it.
func (r *workHoldRig) openedWithItsStartOnBackoff() {
	r.t.Helper()
	r.approve(4, 12)
	if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = $2 where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $3`, r.tree.key, r.clock.Now().Add(10*time.Minute),
		string(claim.RoleImplementer)); err != nil {
		r.t.Fatalf("put the implementer's opening start on its backoff: %v", err)
	}
	r.runOnce()
}

// movedBack applies a tester's move of the root back to implementing, and answers the task of the
// start of the implementer's phase that the move queued.
func (r *workHoldRig) movedBack() string {
	r.t.Helper()
	r.apply("back", intake.BackwardMove{Issue: r.tree.key, Requester: claim.RoleTester, To: phase.Implementing,
		Reason: "the empty list is not covered"})
	var work string
	for _, start := range startRequestsOf(r.t, r.pool, r.tree.key, "start") {
		if start.Role == claim.RoleImplementer && start.Opening == "" {
			work = start.Task
		}
	}
	if work == "" {
		r.t.Fatal("the move back queued no start of the implementer's phase")
	}
	return work
}

// takeTheTask has role's agent take the task its claim holds: the turn that task's prompt starts,
// and that turn's end.
func (r *workHoldRig) takeTheTask(role claim.Role) {
	r.t.Helper()
	machine := r.machine(role)
	pending := machine.Claim().Pending
	if pending == nil {
		r.t.Fatalf("the %s holds no task to take", role)
	}
	r.handle(role, supervise.StreamTurnStart{Claim: r.token(role), DeliveryID: pending.ID})
	endTurn(r.ctx, r.t, machine, r.token(role))
}

// wantPrompts is the implementer prompted with len(suffixes) tasks after a drain, each ending with
// its suffix in order, and a second drain prompting nothing more.
func (r *workHoldRig) wantPrompts(when string, suffixes ...string) {
	r.t.Helper()
	for _, pass := range []string{"a drain", "a second drain"} {
		r.drain()
		r.machine(claim.RoleImplementer).Wait()
		prompts := r.conn(claim.RoleImplementer).Prompts()
		ok := len(prompts) == len(suffixes)
		for i := 0; ok && i < len(suffixes); i++ {
			ok = strings.HasSuffix(prompts[i].Message, suffixes[i])
		}
		if !ok {
			r.t.Fatalf("%s, after %s, the implementer was prompted %+v, want %d tasks ending\n%q", when, pass, prompts, len(suffixes), suffixes)
		}
	}
}

// An operator's act overrides the work hold, and the daemon drops no instruction without saying
// so: a claim the hold stopped that kept an operator's task gets that task first, then the
// opening's task, then the task of a phase start written after the opening
// (supervise.Machine.EndHold, openingGoesFirst). The tester holds the phase at the opening, and
// the close stopped it, so the opening settles the issue's holder row as told: the opening's start
// of the implementer, which carries no phase work, is the one carrier of the opening's sentences.
// A move back then gives the implementer its phase. That phase start waits while the claim is
// stopped, and again after the opening's start ended the hold, while that start's task waits
// behind the operator's task; it never records itself first, which would finish the opening's
// start. The implementer is prompted with the operator's task, then with the task that names
// version 4 and carries today's open message, then with its phase work, and nothing more. A
// daemon restart while the phase start waits changes nothing.
func TestWorkHoldOpeningStartGoesBeforeAPhaseStartBehindAnOperatorsTask(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "no restart"
		if restart {
			name = "a daemon restart while the phase start waits"
		}
		t.Run(name, func(t *testing.T) {
			r := heldWithAnOperatorsTask(t, legionTree(phase.Testing, config.HoldWork), true)
			r.openedWithItsStartOnBackoff()
			if r.openingOwed() {
				t.Fatal("after the opening the holder's told row owes the opening, want it settled by the tester's own start")
			}
			work := r.movedBack()
			r.runOnce()
			if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() {
				t.Fatalf("the phase start ended the hold (the implementer is %s by %q), want it waiting for the opening's start", c.State, c.SuspendedBy)
			}
			r.clock.advance(11 * time.Minute)
			r.drain()
			if c := r.machine(claim.RoleImplementer).Claim(); c.WorkHeld() || c.Pending == nil || c.Pending.Task != heldOperatorTask {
				t.Fatalf("the implementer is %s by %q holding %+v, want the hold ended by the opening's start and the operator's task kept", c.State, c.SuspendedBy, c.Pending)
			}
			if got := r.startsSince(claim.RoleImplementer, 0); got != 2 {
				t.Fatalf("the outbox holds %d starts of the implementer, want the opening's start and the phase start waiting", got)
			}
			readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
			opening := r.opening(4, 4) + "\n\n" + noPhaseOpeningWork
			r.wantPrompts("at its ready", heldOperatorTask)
			r.takeTheTask(claim.RoleImplementer)
			if restart {
				r.restart()
			}
			r.wantPrompts("after the operator's turn", heldOperatorTask, opening)
			r.takeTheTask(claim.RoleImplementer)
			r.wantPrompts("after the opening's turn", heldOperatorTask, opening, work)
			if got := r.startsSince(claim.RoleImplementer, 0); got != 0 {
				t.Fatalf("the outbox still holds %d starts of the implementer, want none", got)
			}
		})
	}
}

// The same order where the issue's holder row owes the opening: the tester, the holder of the
// phase, has no claim and is owed the news of version 4 (withNews), so the opening leaves its told
// row owing the opening. The opening's start of the implementer, which holds the phase by then,
// settles that row as told and gives the opening's sentences, with the news of version 4 after its
// work, once. The phase start's task after it carries neither.
func TestWorkHoldOpeningStartGoesBeforeAPhaseStartBehindAnOperatorsTaskWithTheNews(t *testing.T) {
	r := heldWithAnOperatorsTask(t, legionTree(phase.Testing, config.HoldWork).withNews(), false)
	r.openedWithItsStartOnBackoff()
	if !r.openingOwed() {
		t.Fatal("after the opening the holder's told row owes no opening, want it owed to the holder owed news")
	}
	work := r.movedBack()
	r.runOnce()
	r.clock.advance(11 * time.Minute)
	r.drain()
	readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
	r.wantPrompts("at its ready", heldOperatorTask)
	r.takeTheTask(claim.RoleImplementer)
	opening := r.opening(4, 4) + "\n\n" + noPhaseOpeningWork + "\n\n" + r.heldNews(phase.Implementing, 3, 4)
	r.wantPrompts("after the operator's turn", heldOperatorTask, opening)
	if r.openingOwed() {
		t.Fatal("the holder's told row still owes the opening once the opening's start gave its sentences")
	}
	r.takeTheTask(claim.RoleImplementer)
	r.wantPrompts("after the opening's turn", heldOperatorTask, opening, work)
	if prompts := r.conn(claim.RoleImplementer).Prompts(); strings.Contains(prompts[2].Message, r.openedMessage(4)) ||
		strings.Contains(prompts[2].Message, "The approved design of") {
		t.Fatalf("the phase work's prompt is %q, want no second opening and no second news", prompts[2].Message)
	}
}

// The opening's start of the implementer runs before the move back, so its first attempt ends the
// hold while the implementer holds no phase: it settles no holder row (settleHeldOpening), and the
// tester's told row still owes the opening, as the tester has no claim and is owed the news of
// version 4 (withNews). The move back then makes the implementer the holder, and the opening's
// start's retry settles that row as told before it reads its news (startNewsOf, retriesItsStart).
// So the one task gives today's open message once, in the opening's sentences, with the news of
// version 4 after its work and no second gate sentence in front of that news.
func TestWorkHoldOpeningStartsRetryTellsTheOpeningOnceToAHolderOfANewPhase(t *testing.T) {
	r := heldWithAnOperatorsTask(t, legionTree(phase.Testing, config.HoldWork).withNews(), false)
	r.approve(4, 12)
	r.runOnce()
	if c := r.machine(claim.RoleImplementer).Claim(); c.WorkHeld() || c.LastStartRow == 0 {
		t.Fatalf("the implementer is %s by %q at start row %d, want the hold ended by the opening's start", c.State, c.SuspendedBy, c.LastStartRow)
	}
	if !r.openingOwed() {
		t.Fatal("after the opening's first attempt the holder's told row owes no opening, want it still owed: the implementer held no phase")
	}
	work := r.movedBack()
	readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
	r.wantPrompts("at its ready", heldOperatorTask)
	r.takeTheTask(claim.RoleImplementer)
	opening := r.opening(4, 4) + "\n\n" + noPhaseOpeningWork + "\n\n" + r.heldNews(phase.Implementing, 3, 4)
	r.wantPrompts("after the operator's turn", heldOperatorTask, opening)
	if told := r.conn(claim.RoleImplementer).Prompts()[1].Message; strings.Count(told, r.openedMessage(4)) != 1 {
		t.Fatalf("the opening's task tells the gate's open message %d times, want once:\n%s", strings.Count(told, r.openedMessage(4)), told)
	}
	if r.openingOwed() {
		t.Fatal("the holder's told row still owes the opening once the opening's task told it")
	}
	r.takeTheTask(claim.RoleImplementer)
	r.wantPrompts("after the opening's turn", heldOperatorTask, opening, work)
}

// Where no start of the opening of the claim is queued — its stop was made after the opening read
// the claim and the fact that starts it has not been applied yet, or a newer stop or run undid that
// start; here its row is deleted — a phase start that carries no opening's sentences ends the hold
// over the operator's task itself. It settles no opening as told (startNewsOf,
// supervise.KeptOperatorTask), so the holder's told row, which owes the opening, still owes it, and
// the phase start's task, which follows the operator's, carries the gate's open message in its news.
// The implementer is not told that the hold stopped its turn.
func TestWorkHoldPhaseStartWithNoOpeningStartQueuedCarriesTheOwedOpening(t *testing.T) {
	r := heldWithAnOperatorsTask(t, legionTree(phase.Testing, config.HoldWork).withNews(), false)
	r.openedWithItsStartOnBackoff()
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = $2 and coalesce(payload->>'opening', '') <> ''`, r.tree.key, string(claim.RoleImplementer)); err != nil {
		t.Fatalf("delete the implementer's opening start: %v", err)
	}
	if !r.openingOwed() {
		t.Fatal("after the opening the holder's told row owes no opening, want it owed to the holder owed news")
	}
	work := r.movedBack()
	r.runOnce()
	r.clock.advance(11 * time.Minute)
	r.drain()
	if c := r.machine(claim.RoleImplementer).Claim(); c.WorkHeld() || c.Pending == nil || c.Pending.Task != heldOperatorTask {
		t.Fatalf("the implementer is %s by %q holding %+v, want the hold ended and the operator's task kept", c.State, c.SuspendedBy, c.Pending)
	}
	if !r.openingOwed() {
		t.Fatal("the phase start that ended the hold settled the opening as told, want it still owed: its task carries no opening's sentences")
	}
	readyClaimAs(t, r.sup, r.token(claim.RoleImplementer), claim.RoleImplementer, "ses-implementer")
	r.wantPrompts("at its ready", heldOperatorTask)
	r.takeTheTask(claim.RoleImplementer)
	r.drain()
	r.machine(claim.RoleImplementer).Wait()
	prompts := r.conn(claim.RoleImplementer).Prompts()
	if len(prompts) != 2 || !strings.Contains(prompts[1].Message, work) || !strings.Contains(prompts[1].Message, r.openedMessage(4)) {
		t.Fatalf("after the operator's turn the implementer was prompted %+v, want the phase work with the gate's open message", prompts)
	}
	if !r.openingOwed() {
		t.Fatal("the holder's told row owes no opening before the turn of the task that tells it")
	}
	r.takeTheTask(claim.RoleImplementer)
	if r.openingOwed() {
		t.Fatal("the holder's told row still owes the opening after the turn of the task that told it")
	}
}
