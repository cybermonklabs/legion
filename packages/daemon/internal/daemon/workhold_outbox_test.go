package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
// approved, the phase the root stands in, and the deployment's hold.
type holdTree struct {
	key, project, document string
	approved               int
	at                     phase.Phase
	hold                   config.Hold
}

// legionTree is LEGION-208 standing in at under hold, its spec (artifact-208) approved at version 3.
func legionTree(at phase.Phase, hold config.Hold) holdTree {
	return holdTree{key: "LEGION-208", project: "LEGION", document: "artifact-208", approved: 3, at: at, hold: hold}
}

// carriedTree is F3's root: ACME-2694 implementing, its spec (F3's document) approved at version
// 24, which Dispatch carried onto version 46 after an exact undo.
func carriedTree() holdTree {
	return holdTree{key: "ACME-2694", project: "ACME", document: "aec8fb48-fd46-458a-99a3-a1b199a46b38", approved: 24,
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
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: tree.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: tree.project, Tree: tree.key, Title: "a held root", Phase: tree.at, Generation: 1, Status: workflow.PhaseStatus(tree.at),
			Rank: "U", LastDispatchSeq: 5}); err != nil {
			return err
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: tree.key, Step: record.DesignStepSpec, ArtifactID: tree.document,
			LatestVersion: tree.approved, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true}); err != nil {
			return err
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
	podRecords, err := trees.PodRecordsOf(tree.project)
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	clock := &heldClock{at: time.Now().UTC().Truncate(time.Second)}
	// The engine reads the rig's clock, as the runner does, so the clock a restart puts in its
	// place (restart) is the one every later fact reads.
	var r *workHoldRig
	engine := workflow.New(records, workflow.Config{Project: tree.project, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Hold: tree.hold, Pods: podRecords, Linger: time.Hour, Clock: func() time.Time { return r.clock.Now() },
		Hooks: []workflow.Hook{{Name: "back-to-implementing", On: workflow.HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleImplementer},
			Prompt: "Read the tester's report before you write anything."}}}, quietLogger())
	admission := admit.New(records, engine, 2, tree.project, quietLogger())
	r = &workHoldRig{t: t, ctx: ctx, tree: tree, tokenProject: tokenProject, pool: pool, records: records, trees: trees,
		engine: engine, admission: admission, publisher: &holderPublisher{}, conns: map[claim.Role]*fake.Conn{}}
	r.supervise(clock)
	r.runner = &outbox{
		dispatchProject: tree.project, pool: pool, records: records, supervisor: r.sup, trees: trees, tokens: outboxTokens{},
		project: tokenProject, stateDir: t.TempDir(), notices: r.publisher, dispatch: &outboxDispatch{},
		handlers: []intake.Handler{engine, admission}, gatedSteps: config.DesignGateRootIssues.Steps(), hold: tree.hold,
		now: func() time.Time { return r.clock.Now() }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + tree.key}, nil
		},
	}
	return r
}

// supervise gives the rig a supervisor over the rig's store, on clock, as a daemon's boot builds
// one, with each agent connection the rig made registered again.
func (r *workHoldRig) supervise(clock *heldClock) {
	sup, rt := newOutboxSupervisor(r.t, r.tokenProject, r.t.TempDir())
	sup.deps.Store, sup.deps.Clock = r.trees, clock
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
	r.apply(fmt.Sprintf("approved-%d", version), intake.DispatchArtifact{Key: r.tree.key, ArtifactID: r.tree.document,
		Kind: intake.DispatchArtifactApproved, Version: version, Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: seq}})
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

// inTx runs write in one transaction of the rig's pool.
func (r *workHoldRig) inTx(write func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, write); err != nil {
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
	r.wantStarted(claim.RoleImplementer, 1, opening+"\n\n"+workflow.ResumePhaseTask(r.issue()))
	r.wantStarted(claim.RoleTester, 0, opening+"\n\n"+"The issue's other roles may ask you questions.")
}

// H14, check a3d61f8e: a released phase worker that is not a merger is not woken at the close; the
// hold takes it over its release at once, its bundle and its pending task kept. A CI settlement's
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
	if err := r.trees.PutWorkspaceBundle(r.ctx, r.tree.key, 1, runtime.WorkspaceBundle{Bundle: []byte("unpushed work"), Head: "abc123",
		PodUID: "pod-of-the-release", At: r.clock.Now()}); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}

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
	if _, kept, err := r.trees.WorkspaceBundleSize(r.ctx, r.tree.key); err != nil || !kept {
		t.Fatalf("the release's bundle is kept %v, %v; want it kept through the hold", kept, err)
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
// dropped, the turn going on; a released worker's stop not yet run takes nothing over. A stop
// that the turn's end made after the opening committed is started again with its opening task.
func TestWorkHoldMakesNoStopOnceTheGateOpensAgain(t *testing.T) {
	t.Run("before the message reached the worker", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		r.working(claim.RoleImplementer)
		r.released(claim.RoleTester)
		r.close(4)
		r.approve(4, 12)
		r.drain()
		if got := r.published(claim.RoleImplementer); len(got) != 2 || got[0] != r.workerClose(4) || got[1] != r.openedMessage(4) {
			t.Fatalf("the implementer was told %q, want the close and then the opening", got)
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
	t.Run("with the stop made after the opening committed", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		r.working(claim.RoleImplementer)
		r.close(4)
		r.runOnce()
		r.runOnce()
		r.approve(4, 12)
		// The turn ends between the opening's commit and the stop's next attempt: the claim stands
		// stopped by the hold, and the opening, which read it live, started nothing of it.
		endTurn(r.ctx, t, r.machine(claim.RoleImplementer), r.token(claim.RoleImplementer))
		if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByHold {
			t.Fatalf("the implementer after its turn is %s by %q, want it stopped by the hold", c.State, c.SuspendedBy)
		}
		r.drain()
		want := holdSentence + " The work hold stopped your turn. The deployment no longer holds the work of " + r.tree.key + "'s tree." +
			" Check what your turn already did: a tool call may not have finished.\n\n" + workflow.ResumePhaseTask(r.issue())
		r.wantStarted(claim.RoleImplementer, 1, want)
	})
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
	r.apply("boot", intake.DaemonBooted{})
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
			r.wantStarted(claim.RoleImplementer, 1, sentences+"\n\n"+workflow.ResumePhaseTask(r.issue()))
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
