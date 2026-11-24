package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A keyed agent works the tester's step in the claim it started on the implementer's step, and the
// tester's phases row names that claim. Each row the workflow writes for the tester then reaches
// that one agent: the start delivers to its claim rather than starting a second session of the
// key, and the step's stop suspends it. An update round's ask names the claim the sweep asked and
// is published where that claim records the round, also where that claim is not the one the
// record gives the role: a fresh tester an earlier version of the plan started still holds its
// claim in the pod. An ask the release before wrote names no claim, and reaches the claim the
// record gives the role. The asks run while key a's agent is ready, before the step's stop: a
// claim the daemon suspended is published no ask (supervise's UpdateAskSuspended).
func TestARowOfATesterStepReachesItsAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	own := mustClaimToken(t, issue.Key, claim.RoleTester)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range []record.PhaseRow{
			{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1},
			{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2},
		} {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write the phases rows of key a's two steps: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued, UpdateRound: "r1"}, "")
	if err != nil {
		t.Fatalf("create the claim of key a: %v", err)
	}
	publisher := &outboxPublisher{}
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: publisher,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	topic := notify.RoleTopicPrefix + string(keyed)

	start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
		Entry: 2, Task: "Test it.", Generation: 1}, time.Now())
	start.ID = 57
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("the tester's start: %v", err)
	}
	if _, found := sup.Machine(own); found {
		t.Fatalf("the tester's start made a claim %s, want it delivered to key a's claim %s", own, keyed)
	}
	if got := machine.Claim(); got.Pending == nil || got.Pending.ID != "outbox:57" {
		t.Fatalf("key a's claim after the tester's start = %s holding %+v, want the start's task outbox:57", got.State, got.Pending)
	}

	launch := machine.Claim().Generation
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: keyed, Generation: launch, Session: "ses-a", SessionFile: "/tmp/a.jsonl"}); err != nil {
		t.Fatalf("register key a's agent: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: keyed, Generation: launch, Session: "ses-a"}); err != nil {
		t.Fatalf("ready key a's agent: %v", err)
	}

	// A claim is asked once per round (the outbox marks it asked), so the ask the release before
	// wrote, which names no claim, runs first: it reaches the claim the record gives the role.
	before := record.OutboxRow{ID: 60, Issue: issue.Key, Kind: record.OutboxKindUpdateAsk, NextAt: time.Now(),
		Payload: json.RawMessage(`{"role":"tester","round":"r1","text":"Push your work."}`)}
	if err := runner.execute(ctx, before); err != nil {
		t.Fatalf("the update ask of the release before: %v", err)
	}
	if published := publisher.publishes(); len(published) != 1 || published[0].topic != topic || published[0].message != "Push your work." {
		t.Fatalf("the update ask of the release before was published as %+v, want it on %s", published, topic)
	}

	// A fresh tester of an earlier version of the plan still holds its own claim in the pod, and
	// the round's sweep asks it too: the ask names it, and reaches it rather than key a's agent.
	if _, _, err := sup.Create(ctx, supervise.Claim{Token: own, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleTester, State: supervise.StateQueued, UpdateRound: "r1"}, ""); err != nil {
		t.Fatalf("create the earlier version's tester claim: %v", err)
	}
	stale := mustOutboxRow(t, issue.Key, record.UpdateAsk{Role: claim.RoleTester, Claim: own, Round: "r1", Text: "Push yours too."}, time.Now())
	stale.ID = 61
	if err := runner.execute(ctx, stale); err != nil {
		t.Fatalf("the update ask of the earlier version's tester: %v", err)
	}
	if published := publisher.publishes(); len(published) != 2 || published[1].topic != notify.RoleTopicPrefix+string(own) || published[1].message != "Push yours too." {
		t.Fatalf("the update ask of the earlier version's tester was published as %+v, want it on %s", published, notify.RoleTopicPrefix+string(own))
	}

	// The step's stop suspends key a's agent, the claim the record gives the tester's step.
	stop := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "step_stop", Tree: issue.Tree, Role: claim.RoleTester, Generation: 1}, time.Now())
	stop.ID = 58
	if err := runner.execute(ctx, stop); err != nil {
		t.Fatalf("the tester's step stop: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("key a's claim after the tester's step stop = %s, want suspended", got)
	}
}

// Key a's agent works the implementer's step and is in a turn when the start of the tester's entry
// runs: the start asks the claim to suspend, keeping its session, and is a wait, so the turn's last
// push or report is not cut off and the claim keeps its role. Once the turn ends the suspension
// acts, and the start's next attempt gives the claim the tester's role and relaunches it on its
// session. A start of no entry (a wake) of the tester for the same claim starts it with the role it
// holds and changes no role.
func TestAReRolesStartWaitsForTheTurnsEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range []record.PhaseRow{
			{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1},
			{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2},
		} {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write the phases rows of key a's two steps: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the claim of key a: %v", err)
	}
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	// The supervisor of this rig keeps its claims in its own state, so the role is read from the
	// machine; the store's write of a re-role is held by supervise's own test.
	storedRole := func() claim.Role { return machine.Claim().Role }

	// A wake of the tester (a start of no entry) for key a's claim, which holds the implementer's
	// role, starts it with the role it holds; the agent comes up and takes a turn on the wake's task.
	wake := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Claim: keyed,
		Task: "Carry on.", Generation: 1}, time.Now())
	wake.ID = 70
	if err := runner.execute(ctx, wake); err != nil {
		t.Fatalf("the wake of the tester: %v", err)
	}
	if spawns := rt.CallsOf("Spawn"); storedRole() != claim.RoleImplementer || len(spawns) != 1 || spawns[0].Spec.Role != claim.RoleImplementer {
		t.Fatalf("after a wake of no entry the claim holds %s and was spawned as %+v, want the implementer it holds", storedRole(), spawns)
	}
	launch := machine.Claim().Generation
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: keyed, Generation: launch, Session: "ses-a", SessionFile: "/tmp/a.jsonl"}); err != nil {
		t.Fatalf("register key a's agent: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: keyed, Generation: launch, Session: "ses-a"}); err != nil {
		t.Fatalf("ready key a's agent: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: keyed, DeliveryID: "outbox:70"}); err != nil {
		t.Fatalf("key a's agent's turn: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateWorking {
		t.Fatalf("key a's agent is %s, want working: the case's premise", got)
	}
	suspends := len(rt.CallsOf("Suspend"))

	start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
		Entry: 2, Task: "Test it.", Generation: 1}, time.Now())
	start.ID = 71
	if err := runner.execute(ctx, start); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the tester's start during the implementer's turn = %v, want a wait", err)
	}
	if got := len(rt.CallsOf("Suspend")); got != suspends || machine.Claim().State != supervise.StateWorking {
		t.Fatalf("after the tester's start the claim is %s with %d suspends, want its turn running untouched", machine.Claim().State, got-suspends)
	}
	if got := storedRole(); got != claim.RoleImplementer {
		t.Fatalf("the claim's role during the turn = %s, want implementer", got)
	}

	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: keyed}); err != nil {
		t.Fatalf("end key a's agent's turn: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("key a's agent after its turn = %s, want suspended by the start's ask", got)
	}
	resumes := len(rt.CallsOf("Resume"))
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("the tester's start after the turn: %v", err)
	}
	if got := storedRole(); got != claim.RoleTester {
		t.Fatalf("the claim's stored role after the tester's start = %s, want tester", got)
	}
	calls := rt.CallsOf("Resume")
	if len(calls) != resumes+1 || calls[len(calls)-1].Spec.Role != claim.RoleTester || calls[len(calls)-1].Spec.ResumeSessionFile != "/tmp/a.jsonl" {
		t.Fatalf("the relaunches after the tester's start = %+v, want one as the tester resuming /tmp/a.jsonl", calls[resumes:])
	}
}

// LEGION-208 has left tree LEGION-207 for a tree of its own, and two claims of its tester's role
// still stand in the old tree: key a's agent, which the tester's phases row names on the step, and
// the tester's own claim of an earlier version of the plan. A suspend of the old tree
// (stopInLeftTree) stops the claim it names, and a suspend the release before wrote, which names
// none, stops the claim the record gives the role's step; the tester's own claim runs on either way.
// Read by the role's own token, the stop would reach the tester's own claim and leave key a's
// agent running in the old tree.
func TestASuspendOfALeftTreeStopsTheClaimItReaches(t *testing.T) {
	t.Parallel()
	const oldTree = "LEGION-207"
	for _, tc := range []struct {
		name string
		// names is whether the suspend row names key a's claim.
		names bool
	}{
		{name: "a row naming the claim", names: true},
		{name: "a row the release before wrote", names: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 2, Status: "testing"}
			putOutboxIssue(t, pool, records, record.Issue{Key: oldTree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: oldTree, Title: "Root", Phase: phase.Done, Generation: 1, Status: "done"})
			putOutboxIssue(t, pool, records, issue)
			keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
			}); err != nil {
				t.Fatalf("name key a's claim on the tester's step: %v", err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			launchedClaimAs(t, sup, oldTree, issue.Key, claim.RoleImplementer, "ses-a")
			own := launchedClaimAs(t, sup, oldTree, issue.Key, claim.RoleTester, "ses-tester")
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
				trees: outboxTreeStore(t, pool, oldTree, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				now: time.Now}

			request := record.SuperviseRequest{Op: "suspend", Tree: oldTree, Role: claim.RoleTester, Generation: 1,
				Reason: "the tree of LEGION-207 lingers"}
			if tc.names {
				request.Claim = keyed
			}
			if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, request, time.Now())); err != nil {
				t.Fatalf("the old tree's suspend of the tester: %v", err)
			}
			for token, want := range map[claim.Token]supervise.ClaimState{keyed: supervise.StateSuspended, own: supervise.StateReady} {
				machine, _ := sup.Machine(token)
				if got := machine.Claim().State; got != want {
					t.Fatalf("%s after the old tree's suspend of the tester = %s, want %s: the suspend reaches key a's agent alone", token, got, want)
				}
			}
		})
	}
}
