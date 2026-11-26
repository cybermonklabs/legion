package daemon

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// readyClaimAs drives token's launched claim through its shim's hello, its agent's registration
// and its ready, at the claim's current generation, on session, whose file is named for it: the
// daemon holds a session's id and its file's path and name to the first claim of the project that
// recorded them (supervise's holdRecorded), so a test with two claims of one role names the
// second's session itself, and so its file.
func readyClaimAs(t *testing.T, sup *supervisor, token claim.Token, role claim.Role, session string) {
	t.Helper()
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	generation := machine.Claim().Generation
	file := "/tmp/" + strings.TrimPrefix(session, "ses-") + ".jsonl"
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: file},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
}

// launchedClaim creates role's claim on issue of tree, launches it and makes its agent ready.
func launchedClaim(t *testing.T, sup *supervisor, tree, issue string, role claim.Role) claim.Token {
	t.Helper()
	return launchedClaimAs(t, sup, tree, issue, role, "ses-"+string(role))
}

// launchedClaimAs is launchedClaim with its agent registering session (readyClaimAs).
func launchedClaimAs(t *testing.T, sup *supervisor, tree, issue string, role claim.Role, session string) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, role)
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if err := machine.Handle(context.Background(), supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn %s: %v", token, err)
	}
	readyClaimAs(t, sup, token, role, session)
	return token
}

// resumesOf is the runtime's Resume calls for token's claim.
func resumesOf(rt *fake.Runtime, token claim.Token) []fake.Call {
	var of []fake.Call
	for _, call := range rt.CallsOf("Resume") {
		if call.Spec.Claim == token || call.Locator.Claim == token {
			of = append(of, call)
		}
	}
	return of
}

// adoptionsOf is the App git name of every adoption of the working copy rt was asked to make for
// token's claim, in order. An adoption runs in the claim's own process, so the locator it is made
// over names that claim.
func adoptionsOf(rt *fake.Runtime, token claim.Token) []string {
	var made []string
	for _, call := range rt.CallsOf("AdoptWorkingCopy") {
		if call.Locator.Claim == token {
			made = append(made, call.Identity.Name)
		}
	}
	return made
}

// resumeTaskOf is the resume task engine sends the worker of key's phase, as the record holds
// the issue now: workflow's Engine.ResumeTask, the one place every resume is composed, read in a
// transaction it rolls back.
func resumeTaskOf(t testing.TB, pool *store.Pool, engine *workflow.Engine, key string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the read of the resume task of %s: %v", key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := record.NewStore().Issue(ctx, tx, key)
	if err != nil || issue == nil {
		t.Fatalf("read the record of %s: %+v, %v", key, issue, err)
	}
	task, err := engine.ResumeTask(ctx, tx, *issue)
	if err != nil {
		t.Fatalf("compose the resume task of %s: %v", key, err)
	}
	return task
}

// The boot's start of a daemon-suspended phase owner resumes it by rule. If that owner can never
// launch, the resume spends its launch budget and the claim fails; the start row then finishes
// rather than retrying the failed claim with fresh budgets (RequestRetry), so the owner is launched
// exactly the budget's times however many ticks follow, and stays failed for the boot report.
func TestABootRuleResumeThatSpendsTheLaunchBudgetFinishes(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	tester := launchedClaim(t, sup, issue.Tree, issue.Key, claim.RoleTester)
	machine, _ := sup.Machine(tester)
	if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: tester, Reason: "LEGION-208 is backlog", By: supervise.SuspendedByDaemon}); err != nil {
		t.Fatalf("the daemon's suspend: %v", err)
	}
	unschedulable := errors.New("the pod was never scheduled")
	for range 20 {
		rt.ScriptResume(fake.SpawnResult{Err: unschedulable})
	}
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	// The row the boot's resume by rule writes, carrying the task the engine composes for it.
	engine := workflow.New(records, workflow.Config{Project: issue.Project}, quietLogger())
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Task: resumeTaskOf(t, pool, engine, issue.Key), ResumeTask: true, RuleResume: true}, clock))

	for tick := range 6 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		machine.Wait()
		clock = clock.Add(2 * time.Minute)
	}
	limit := sup.deps.Limits.LaunchFailures
	if got := machine.Claim().State; got != supervise.StateFailed || len(resumesOf(rt, tester)) != limit || outboxRows(t, pool) != 0 {
		t.Fatalf("after six ticks the tester is %s after %d launches with %d rows left; want failed after its budget of %d launches and the row finished",
			got, len(resumesOf(rt, tester)), outboxRows(t, pool), limit)
	}
}

// operatorSuspend is the operator's suspend route's request (api's suspendEvent) handled by token's
// machine.
func operatorSuspend(t *testing.T, sup *supervisor, token claim.Token) {
	t.Helper()
	machine, _ := sup.Machine(token)
	if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "the operator suspended it", By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the operator's suspend of %s: %v", token, err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("%s is %s after the operator's suspend, want suspended", token, got)
	}
}

// An operator's suspend holds until the operator resumes the claim: a phase moving on to the role
// does not resume it. The start waits, logged once at Info naming who suspended the claim (the line
// Stage 4b's child-reentered control reads), and once the operator resumes the claim, its task is
// given.
func TestAStartWaitsForTheOperatorToResumeAClaimItSuspended(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	tester := launchedClaim(t, sup, issue.Tree, issue.Key, claim.RoleTester)
	operatorSuspend(t, sup, tester)
	clock := time.Now()
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewJSONHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Task: "Test it."}, clock))
	ctx := context.Background()
	machine, _ := sup.Machine(tester)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the start: %v", err)
	}
	machine.Wait()
	if resumes := resumesOf(rt, tester); len(resumes) != 0 || machine.Claim().State != supervise.StateSuspended || outboxRows(t, pool) != 1 {
		t.Fatalf("after the start the tester is %s with %d resumes and %d rows, want it suspended, never resumed, the start waiting",
			machine.Claim().State, len(resumes), outboxRows(t, pool))
	}
	if waits := strings.Count(logged.String(), `"msg":"outbox row waits"`); waits != 1 ||
		!strings.Contains(logged.String(), `"level":"INFO","msg":"outbox row waits"`) ||
		!strings.Contains(logged.String(), "suspended by operator; its agent waits for the operator's resume: the suspension holds until the operator resumes the agent") {
		t.Fatalf("the waiting start logged %q, want one Info line naming the operator's suspension", logged.String())
	}

	if err := machine.Handle(ctx, supervise.RequestResume{Claim: tester, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the operator's resume: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the start after the operator's resume: %v", err)
	}
	if pending := machine.Claim().Pending; outboxRows(t, pool) != 0 || pending == nil || pending.Task != "Test it." {
		t.Fatalf("after the operator's resume: %d rows, pending %+v; want the start run and its task held for the tester", outboxRows(t, pool), pending)
	}
}

// suspendedBefore0038 creates role's claim on issue as migration 0038 leaves a claim that was
// suspended before it: suspended, its session kept, its source unknown.
func suspendedBefore0038(t *testing.T, sup *supervisor, tree, issue string, role claim.Role) claim.Token {
	t.Helper()
	token := claim.ControllerToken("legion")
	if role != claim.RoleController {
		token = mustClaimToken(t, issue, role)
	}
	if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role,
		Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByUnknown,
		Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl"}, ""); err != nil {
		t.Fatalf("create the pre-0038 suspended %s: %v", role, err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	return token
}

// A claim suspended before migration 0038 recorded who suspended it is resumed by every trigger
// main resumed it on, here its phase's start, as on re-admission or a child's re-entry. Most such
// rows are the daemon's own (a lingering tree, a parked child, a re-entry's interrupted worker), so
// holding them for the operator would leave the trees main ran again parked.
func TestAStartResumesAClaimSuspendedBeforeItsSourceWasRecorded(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	tester := suspendedBefore0038(t, sup, issue.Tree, issue.Key, claim.RoleTester)
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Task: "Test it."}, clock))

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the start: %v", err)
	}
	machine, _ := sup.Machine(tester)
	machine.Wait()
	if got := machine.Claim(); len(resumesOf(rt, tester)) != 1 || got.State != supervise.StateLaunching || outboxRows(t, pool) != 0 ||
		got.Pending == nil || got.Pending.Task != "Test it." {
		t.Fatalf("after the start the tester is %s with %d resumes, %d rows and pending %+v; want its session relaunched once, the start run and its task held",
			got.State, len(resumesOf(rt, tester)), outboxRows(t, pool), got.Pending)
	}
}

// The daemon keeps its own controller running by rule. A controller suspended before migration
// 0038 recorded who suspended it is relaunched, as main relaunched it; one the operator suspended
// stays suspended until the operator resumes it.
func TestTheControllerKeeperResumesEveryControllerButOneTheOperatorSuspended(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		by      supervise.SuspendSource
		resumed bool
	}{
		{name: "suspended before 0038", by: supervise.SuspendedByUnknown, resumed: true},
		{name: "the operator's", by: supervise.SuspendedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			token := suspendedBefore0038(t, sup, "", "", claim.RoleController)
			machine, _ := sup.Machine(token)
			if tc.by == supervise.SuspendedByOperator {
				if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "the operator suspended it", By: supervise.SuspendedByOperator}); err != nil {
					t.Fatalf("the operator's suspend: %v", err)
				}
			}
			keeper := newControllerKeeper(context.Background(), sup, "legion", time.Minute, 0, quietLogger())
			if err := keeper.keep(); err != nil {
				t.Fatalf("keep the controller: %v", err)
			}
			machine.Wait()
			if resumed := len(resumesOf(rt, token)) == 1 && machine.Claim().State == supervise.StateLaunching; resumed != tc.resumed {
				t.Fatalf("the keeper left the controller %s after %d resumes, want resumed %t", machine.Claim().State, len(resumesOf(rt, token)), tc.resumed)
			}
		})
	}
}

// seedWorkflowTree records root as an admitted tree of project in the store at dsn — the issue and
// its row of the copy, as a reading of Dispatch leaves it (so the root stands on a known Dispatch
// chain, record.Chains), the admission slot its phase keeps (record.KeepsSlot: none in accepting,
// retro or merging), its tree's lifecycle and the root's own issue lifecycle, as admission leaves
// a root it promoted — and each of claims, bound to its issue's lifecycle as the daemon binds a
// claim it admits, that issue's lifecycle opened as admission opens it for each issue it starts,
// and its pending delivery written too when it carries one (AdmitClaim alone writes only the
// claim row). Each tree takes the next free slot, so a test may seed more than one running tree.
func seedWorkflowTree(t *testing.T, dsn, project string, root record.Issue, claims ...supervise.Claim) {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	seedWorkflowTreeIn(t, st, project, root, claims...)
}

// seedWorkflowTreeIn is seedWorkflowTree through st, a store the test already holds open.
func seedWorkflowTreeIn(t *testing.T, st *store.Store, project string, root record.Issue, claims ...supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		var parent string
		if root.Parent != nil {
			parent = *root.Parent
		}
		seedCopyRow(t, tx, root.Key, parent, cmp.Or(root.DispatchStatus, root.Status), 1)
		if !record.KeepsSlot(root) {
			return nil
		}
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		// Above every slot the store holds, so a second tree seeded here takes a free one whatever
		// the first left.
		index := 0
		for _, slot := range taken {
			if slot.Index >= index {
				index = slot.Index + 1
			}
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: index, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("record the tree of %s: %v", root.Key, err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, project, root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", root.Key, err)
	}
	openIssue(t, st, project, root.Key, treelifecycle.AuthorityWorkflow)
	for _, c := range claims {
		openIssue(t, st, c.Project, c.Issue, treelifecycle.AuthorityWorkflow)
		if _, err := st.AdmitClaim(ctx, c); err != nil {
			t.Fatalf("put %s: %v", c.Token, err)
		}
		if c.Pending != nil {
			if err := st.PutDelivery(ctx, c.Token, *c.Pending); err != nil {
				t.Fatalf("put the pending delivery of %s: %v", c.Token, err)
			}
		}
	}
}

// A daemon that boots with a suspended claim owning its issue's current phase resumes it: nothing
// else will, since the phase's start already ran before the restart. The resumed worker relaunches
// its own session and is told to carry its phase on.
func TestABootResumesASuspendedClaimThatOwnsItsIssuesPhase(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.PostgresDSN = pool.Config().ConnString()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: cfg.Project, Tree: root, Title: "Workflow",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer, Generation: 1,
			State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_implementer", SessionFile: filepath.Join(cfg.StateDir, "implementer.jsonl")})
	writePrompt(t, cfg, implementer)
	hostWorkspace(t, cfg.StateDir, "acme/widgets", root)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}

	startDaemon(t, cfg, o)

	testwait.Eventually(t, "the booted daemon to resume the implementer that owns its issue's phase", func() bool {
		return len(resumesOf(rt, implementer)) == 1
	})
	if resume := resumesOf(rt, implementer)[0]; resume.Spec.ResumeSessionFile != filepath.Join(cfg.StateDir, "implementer.jsonl") {
		t.Fatalf("resumed %+v, want the implementer's own session", resume.Spec)
	}
}
