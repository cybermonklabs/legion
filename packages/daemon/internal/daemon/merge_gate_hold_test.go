package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The merger's half of k29's case (R19 of dispatch://INFRA-3320): an issue waits for the design
// gate of each issue above it in Legion's work that holds its own gate, and for each gate step it
// holds itself, "and its merger waits for the same gates". The admission half is internal/admit's
// test of the same name; that package cannot reach the outbox, since this one imports it, so the
// merger's case runs here under the same name, and k29's run, which names the test across
// ./internal/..., runs both.
//
//   - An issue a release before kept at work in another issue's tree, whose parent a person then
//     took off, is a root of its own with no row of its gate. Under a deployment that gates the
//     spec step, its merger's start waits, and the wait names the spec step and says that its
//     architect registers that document and requests the person's approval. Its architect, which
//     the start launched under the old root, is relaunched in its session under the issue's own key
//     (supervise.GateRootMoved), and the catch-up that follows says the same to it. Once the step is
//     registered and approved, the same start row runs the merger (classify.MergeHold).
//   - An issue whose gate opened and that ran, under a deployment that has since started to gate a
//     second step it holds no row of: the merge does not wait on that step, as it did not before,
//     while the next start of the issue still does, as it did before (record.LineFacts.ShutGates).
//   - A root a hand-over of the release before carried into its run without a gate, in merging
//     with no row of its gate: its own record says so (record.CarriedIn, record.Issue.CarriedGate),
//     and its merge is held by the step a person is asked about, as before, so it merges.
//
// Negative controls, run when this was written: with the merger's hold reading
// classify.StandingGate alone, the first case's merger start ran at once, with no step registered;
// with the hand-over's exemption taken out of classify.MergeHold, the third case's merger start
// waited for the spec step.
func TestAnIssueWaitsForEveryGateAboveIt(t *testing.T) {
	t.Parallel()
	gated := config.DesignGateRootIssues.Steps()

	t.Run("an issue made a root at work with no gate of its own", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newMergeGateRig(t, gated)
		const old, key = "LEGION-500", "LEGION-533"
		r.putRoot(record.Issue{Key: old, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
		putOutboxGate(t, r.pool, r.records, record.DesignGate{Issue: old, Step: record.DesignStepSpec, ArtifactID: "artifact-500",
			LatestVersion: 2, ApprovedVersion: new(2)})
		issue := mergeGateIssue(key)
		issue.Phase, issue.Status, issue.TakenBack, issue.Parent = phase.Merging, "retro", true, new(old)
		putChainIssue(t, r.pool, r.records, issue)
		r.openTree(key)

		// Its own architect, started while it stood under the old root, is launched under that root,
		// and it works a turn there.
		architect := mustClaimToken(t, key, claim.RoleArchitect)
		machine := r.startArchitect(key, architect)
		if launched := machine.Claim().LaunchedTree(); launched != old {
			t.Fatalf("the architect of %s was launched under %q, want the gate root above it, %s", key, launched, old)
		}
		handleAll(t, machine, supervise.StreamTurnStart{Claim: architect}, supervise.StreamTurnEnd{Claim: architect})

		// The person takes the parent off: the issue holds its own gate, with no row of it.
		putCopyRow(t, r.pool, key, "", "retro")
		if _, err := r.pool.Exec(ctx, "delete from outbox"); err != nil {
			t.Fatalf("deliver the queued notices: %v", err)
		}
		merger := mustClaimToken(t, key, claim.RoleMerger)
		row := mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleMerger,
			Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())
		err := r.runner.execute(ctx, row)
		if !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the merger's start of %s with no row of its gate = %v, want it held (wait.ErrWaiting)", key, err)
		}
		for _, says := range []string{"the spec step of the design gate of " + key,
			"the architect of " + key + " registers the document of that step with register_gate and requests the person's approval"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the merger's wait reads %q, want it to say %q", err, says)
			}
		}
		if spawns := r.spawnsOf(merger); spawns != 0 {
			t.Fatalf("the runtime spawned %d merger sessions while no row of the gate stood, want none", spawns)
		}

		// Its architect is relaunched in its session under its own key, and the catch-up its
		// ready queues opens with the sentence of what the relaunch ended, then states the tree and
		// names the step whose document the architect registers.
		handleAll(t, machine, supervise.GateRootMoved{Claim: architect, Tree: key})
		relaunched := machine.Claim()
		if relaunched.LaunchedTree() != key || !relaunched.EndedWork.Tells() {
			t.Fatalf("the relaunched architect of %s is launched under %q with the record %+v, want its own key and an untold record of the relaunch",
				key, relaunched.LaunchedTree(), relaunched.EndedWork)
		}
		sentence := supervise.RelaunchSentence(relaunched.EndedWork)
		bootClaim(t, machine, architect)
		want := sentence + "\n\nthe tree of " + key + " at generation 1 as the daemon records it now; start or resume it as your role says" +
			"; no document is registered for the spec step of " + key + ": register the document of that step with register_gate and request the person's approval"
		if reasons := catchUpReasons(t, r.pool); len(reasons) != 1 || reasons[0] != want {
			t.Fatalf("the outbox holds catch-ups %q, want the relaunch's one, %q", reasons, want)
		}
		if !strings.Contains(sentence, "its issue now stands under another gate root") {
			t.Errorf("the relaunch's sentence %q does not say why the process was started again", sentence)
		}

		// Its architect registers the spec, and the person approves it.
		putOutboxGate(t, r.pool, r.records, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "artifact-533",
			LatestVersion: 1, ApprovedVersion: new(1)})
		putOutboxTold(t, r.pool, r.records, key, map[record.DesignStep]int{record.DesignStepSpec: 1})
		if err := r.runner.execute(ctx, row); err != nil {
			t.Fatalf("the merger's start of %s once its spec is registered and approved = %v, want it run", key, err)
		}
		if spawns := r.spawnsOf(merger); spawns != 1 {
			t.Fatalf("the runtime spawned %d merger sessions once the spec was approved, want one", spawns)
		}
	})

	t.Run("an issue whose gate opened and ran, then a step the deployment added", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		// The deployment gated the spec step when the gate opened; it gates decisions as well now.
		added := append(slices.Clone(gated), record.DesignStepDecisions)
		r := newMergeGateRig(t, added)
		const key = "LEGION-600"
		issue := r.putRoot(record.Issue{Key: key, Phase: phase.Merging, Status: "retro", TakenBack: true})
		putOutboxGate(t, r.pool, r.records, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "artifact-600",
			LatestVersion: 2, ApprovedVersion: new(2)})
		putOutboxTold(t, r.pool, r.records, key, map[record.DesignStep]int{record.DesignStepSpec: 2})

		// Its next start waits on the added step, as before.
		shut, err := store.ReadOnly(ctx, r.pool, "read the line facts of "+key, func(ctx context.Context, tx pgx.Tx) ([]string, error) {
			facts, err := record.ReadLineFacts(ctx, tx, r.records, "LEGION", []record.Issue{issue}, added)
			return facts.ShutGates(key), err
		})
		if err != nil || !slices.Equal(shut, []string{key}) {
			t.Fatalf("the gates that hold the next start of %s = %v (%v), want its own: the step the deployment added holds its starts", key, shut, err)
		}

		// Its merge does not.
		merger := mustClaimToken(t, key, claim.RoleMerger)
		if err := r.runner.execute(ctx, mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleMerger,
			Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())); err != nil {
			t.Fatalf("the merger's start of %s, whose gate opened before the deployment gated decisions = %v, want it run", key, err)
		}
		if spawns := r.spawnsOf(merger); spawns != 1 {
			t.Fatalf("the runtime spawned %d merger sessions, want one: a step added after the gate opened holds no merge", spawns)
		}
	})

	t.Run("a root the release before handed over without a gate", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newMergeGateRig(t, gated)
		const key = "LEGION-700"
		// The first boot's conversion copied the hand-over onto the root's own record: its run came
		// in at the generation it stands at, and the gate of the tree it left did not come with it.
		issue := mergeGateIssue(key)
		issue.Phase, issue.Status, issue.TakenBack, issue.HoldsGate, issue.HandedOver = phase.Merging, "retro", true, true, true
		issue.CarriedGeneration, issue.CarriedGate = issue.Generation, false
		putChainIssue(t, r.pool, r.records, issue)
		r.openTree(key)

		merger := mustClaimToken(t, key, claim.RoleMerger)
		if err := r.runner.execute(ctx, mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleMerger,
			Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())); err != nil {
			t.Fatalf("the merger's start of %s, handed over without a gate and holding no row of one = %v, want it run", key, err)
		}
		if spawns := r.spawnsOf(merger); spawns != 1 {
			t.Fatalf("the runtime spawned %d merger sessions, want one: a root handed over without a gate keeps the merge hold it had", spawns)
		}
	})
}

// mergeGateRig is the outbox of a deployment that gates steps, over a real Postgres, its
// supervision and its workflow: a start row the runner executes launches its claim on the faked
// runtime, and a claim's ready is applied to the workflow (workflowRuntime.terminal), whose
// catch-up is queued in the outbox.
type mergeGateRig struct {
	t       *testing.T
	pool    *store.Pool
	st      *store.Store
	records record.Store
	sup     *supervisor
	rt      *fake.Runtime
	runner  *outbox
}

func newMergeGateRig(t *testing.T, steps []record.DesignStep) *mergeGateRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.ClaimActivity = st.ClaimActivity
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), handlers: []intake.Handler{engine},
		gatedSteps: steps, now: time.Now,
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + request.Issue}, nil
		},
	}
	return &mergeGateRig{t: t, pool: pool, st: st, records: records, sup: sup, rt: rt, runner: runner}
}

// mergeGateIssue is key's record as the daemon holds an issue at work, at its first run.
func mergeGateIssue(key string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: key, Title: key, Generation: 1, Rank: "U"}
}

// putRoot records a root of its own at the phase and status shaped gives it, holding its own gate,
// its copy row naming no parent, and opens its tree; it returns the record.
func (r *mergeGateRig) putRoot(shaped record.Issue) record.Issue {
	r.t.Helper()
	issue := mergeGateIssue(shaped.Key)
	issue.Phase, issue.Status, issue.TakenBack, issue.HoldsGate = shaped.Phase, shaped.Status, shaped.TakenBack, true
	putChainIssue(r.t, r.pool, r.records, issue)
	r.openTree(issue.Key)
	return issue
}

// openTree opens key's tree lifecycle and its issue's, as admission leaves an issue at work.
func (r *mergeGateRig) openTree(key string) {
	r.t.Helper()
	if _, err := r.st.OpenTreeLifecycle(context.Background(), "legion", key, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open the tree of %s: %v", key, err)
	}
	openIssue(r.t, r.st, "legion", key, treelifecycle.AuthorityWorkflow)
}

// startArchitect runs the start row of key's architect and boots its process to ready.
func (r *mergeGateRig) startArchitect(key string, token claim.Token) *supervise.Machine {
	r.t.Helper()
	r.sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if err := r.runner.execute(context.Background(), mustOutboxRow(r.t, key, record.SuperviseRequest{Op: "start", Tree: key,
		Role: claim.RoleArchitect, Generation: 1}, time.Now())); err != nil {
		r.t.Fatalf("start the architect of %s: %v", key, err)
	}
	machine, found := r.sup.Machine(token)
	if !found {
		r.t.Fatalf("the start left no claim for the architect of %s", key)
	}
	bootClaim(r.t, machine, token)
	return machine
}

// spawnsOf is how many sessions the runtime spawned for token.
func (r *mergeGateRig) spawnsOf(token claim.Token) int {
	count := 0
	for _, call := range r.rt.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			count++
		}
	}
	return count
}

// bootClaim drives the claim's launched process through its shim's hello, its agent's
// registration and its ready, at the generation the claim is launching.
func bootClaim(t *testing.T, machine *supervise.Machine, token claim.Token) {
	t.Helper()
	generation := machine.Claim().Generation
	handleAll(t, machine, supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-" + string(token), SessionFile: "/tmp/" + string(token) + ".jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-" + string(token)})
}
