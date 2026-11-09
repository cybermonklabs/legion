package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

type issueProvisionTokens struct{}

func (issueProvisionTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "fixture-provision-token", nil
}

// A close whose role stop is still pending acts on nothing, and the same close, resumed after the
// daemon and its runtime restart, suspends the issue's Sandbox and deletes it: neither the runtime
// nor the outbox keeps the pending effect in memory, and the database owns it. The stopped role's
// session is the store's and outlives the Sandbox, which owned no part of it. The tree stays live
// until its linger closes, and its cleanup then finds the issue's Sandbox already gone.
func TestIssueCloseDeletesTheSandboxAfterARestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue.Tree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	name := "legion-legion-legion-208"
	api := newIssueSandboxAPI(issueSandbox(name, "sandbox-uid", issue.Tree, issue.Key))
	server := httptest.NewServer(api)
	t.Cleanup(func() { cancel(); server.Close() })
	newSupervisorRuntime := func() (*supervisor, context.CancelFunc) {
		t.Helper()
		runtimeCtx, stopRuntime := context.WithCancel(ctx)
		t.Cleanup(stopRuntime)
		reopened, err := store.Open(ctx, pool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reopened.Close)
		rt, err := sandbox.New(runtimeCtx, sandboxRESTConfig(server.URL), sandbox.Options{
			Namespace: "legion", Project: "legion", Store: reopened, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
			StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
			SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
			Tools:        sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout:  time.Second, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: time.Second, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, quietLogger()),
			ControllerCredential: appauth.ControllerCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, quietLogger()),
			GitHubDirs:           legionDirs, Conns: fake.NewConns(), Log: quietLogger(),
			RepositoryCache: defaultRepositoryCache(t), Mapped: noRepositories,
		})
		if err != nil {
			t.Fatal(err)
		}
		sup := newSupervisor(runtimeCtx, nil, "legion", t.TempDir(), quietLogger())
		sup.deps.Runtime = rt
		t.Cleanup(sup.stop)
		return sup, stopRuntime
	}
	sup, stopRuntime := newSupervisorRuntime()
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	// newRunner is the outbox over the given supervisor: the restart below builds a second one
	// over the restarted runtime, since the outbox carries locks a copy must not.
	newRunner := func(sup *supervisor) *outbox {
		return &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
			dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "done"}}, notices: &outboxPublisher{},
			handlers: []intake.Handler{engine}, log: quietLogger(), now: func() time.Time { return clock },
		}
	}
	runner := newRunner(sup)
	token := claim.Token("legion-legion-legion-208-planner")
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, TreeEpoch: 1, Issue: issue.Key, Role: claim.RolePlanner,
		State: supervise.StateLaunching, Generation: 1, Session: "saved-planner", SessionFile: "/legion/sessions/planner.jsonl"}
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "close", intake.DispatchIssue{Key: issue.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: issue.Title, Rank: "U"}, engine); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "Running" {
		t.Fatalf("close during launch changed Sandbox to %s", got)
	}
	var pendingStop bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'suspend' and payload->>'role' = 'planner')`, issue.Key).Scan(&pendingStop); err != nil || !pendingStop {
		t.Fatalf("close lost the stored launching role's stop before its machine appeared: pending=%t, err=%v", pendingStop, err)
	}
	// Restart the runtime and outbox while the close still waits. Neither has retained watch
	// membership or an in-memory suspension request; the same database owns the pending effect.
	stopRuntime()
	restarted, _ := newSupervisorRuntime()
	runner = newRunner(restarted)
	// The stored role's orderly stop completes independently of the runtime watch population.
	stored.State = supervise.StateSuspended
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "deleted" {
		t.Fatalf("Sandbox after issue close = %s, want it suspended and then deleted", got)
	}
	if options, deleted := api.deleted(name); !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	claims, err := st.Claims(ctx)
	if err != nil || len(claims) != 1 || claims[0].Session != "saved-planner" || claims[0].SessionFile != "/legion/sessions/planner.jsonl" {
		t.Fatalf("close lost retained session: %+v, %v", claims, err)
	}
	if live, err := st.TreeLive(ctx, "legion", issue.Tree); err != nil || !live {
		t.Fatalf("close started tree cleanup: live=%t, err=%v", live, err)
	}
	stored.State = supervise.StateRetired
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "deleted" {
		t.Fatalf("Sandbox after linger cleanup = %s, want it still gone", got)
	}
	if live, err := st.TreeLive(ctx, "legion", issue.Tree); err != nil || live {
		t.Fatalf("tree after its linger cleanup is live = %t, %v; want its cleanup confirmed", live, err)
	}
}

// issueLaunchSpecs builds a launch the Sandbox runtime accepts for any workflow claim: a role prompt
// on disk and the issue's repository; the machine fills in the claim, its generation, its boot token
// and the session it resumes.
type issueLaunchSpecs struct{ prompt string }

func (s issueLaunchSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{
		Env:           map[string]string{"JJ_USER": "legion-implement[bot]"},
		Prompt:        runtime.PromptParts{RolePromptPaths: []string{s.prompt}},
		Repository:    ghrepo.MustParse("acme/widgets"),
		DefaultBranch: "main",
	}, nil
}

// A child closed as done gives its Sandbox back at once while its parent keeps running: the
// close's issue_close rows retire the child's claims with their sessions dropped, and its
// issue_suspend row foreground-deletes the child's Sandbox — taking what it owns with it, with no
// PVC request from the restricted daemon; the root's Sandbox stays Running. A child parked in
// backlog keeps its claims suspended with their sessions, and its Sandbox goes the same way,
// since the pod it ran in, and the disk with it, go either way. The done child set back to todo
// launches fresh because its resume path was cleared, while its session id stays as reaping
// evidence until the volume is gone.
func TestAChildClosedDoneDeletesItsSandboxWhileItsParentRuns(t *testing.T) {
	// Two children under the running root: the one closed as done, whose planner is stopped with
	// its session kept, and the one parked in backlog, whose tester is. The root's own implementer
	// works the phase the root stands in, so admission's pass starts no worker of the root's while
	// the children depart.
	rig := newFenceRig(t, fenceSetup{rootImplementer: true, children: []fenceChild{
		{status: "testing", at: phase.Testing, role: claim.RolePlanner, uid: "sandbox-uid-done"},
		{status: "in_progress", at: phase.Planning, role: claim.RoleTester, uid: "sandbox-uid-parked"},
	}})
	ctx, pool, api, st, logs := rig.ctx, rig.pool, rig.api, rig.st, rig.logs
	engine, admission := rig.engine, rig.admission
	root, done, parked := rig.root, rig.children[0], rig.children[1]
	rootSandbox, doneSandbox, parkedSandbox := rig.rootSandbox, done.sandbox, parked.sandbox
	architect, planner, tester := rig.architect, done.suspended, parked.suspended
	run, storedClaim, effectsLeft := rig.run, rig.storedClaim, rig.effectsLeft

	// (a) The done child: its claims closed, its Sandbox and its pod's disk gone, its parent untouched.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-done", intake.DispatchIssue{Key: done.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the done child's close")
	if got := api.mode(doneSandbox); got != "deleted" {
		t.Fatalf("done child's Sandbox after its close = %s, want deleted", got)
	}
	options, deleted := api.deleted(doneSandbox)
	if !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("done child's Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "sandbox-uid-done" {
		t.Fatalf("done child's Sandbox delete preconditions = %+v, want fenced to uid sandbox-uid-done", options.Preconditions)
	}
	if api.disk(doneSandbox) {
		t.Fatal("the done child's pod's disk survived its Sandbox's foreground delete")
	}
	if got := api.mode(rootSandbox); got != "Running" || !api.disk(rootSandbox) || api.uid(rootSandbox) != "sandbox-uid-root" {
		t.Fatalf("root Sandbox after the child's close = %s (disk %t, uid %s), want the same Sandbox Running with its pod and that pod's disk", got, api.disk(rootSandbox), api.uid(rootSandbox))
	}
	if _, touched := api.deleted(rootSandbox); touched || api.pvcRequests != 0 {
		t.Fatalf("the child's close deleted the root's Sandbox (%t) or sent %d PVC requests, want neither", touched, api.pvcRequests)
	}
	if got := storedClaim(planner.Token); got.State != supervise.StateRetired || got.Session != planner.Session || got.SessionFile != "" || got.Locator != nil {
		t.Fatalf("done child's planner after the close = %+v, want retired with session evidence, no resume path, and no locator", got)
	}
	if got := storedClaim(architect.Token); got.State != supervise.StateWorking || got.Session != "saved-architect" {
		t.Fatalf("root architect after the child's close = %+v, want it working with its session", got)
	}
	if left := effectsLeft(done.Key); left != 0 {
		t.Fatalf("%d supervise/issue_suspend rows of the done child remain, want every one finished", left)
	}
	if live, err := st.TreeLive(ctx, "legion", root.Key); err != nil || !live {
		t.Fatalf("tree live after the child's close = %t, %v; want the tree still live", live, err)
	}
	if !strings.Contains(logs.String(), "deleted the closed issue's Sandbox") {
		t.Fatal("the close logged no line naming the issue's Sandbox")
	}

	// (c) The parked child: its claims suspended with their sessions, and its Sandbox deleted as
	// the done child's was, since its pod and the disk under it go either way.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: parked.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: parked.Title, Parent: root.Key, Rank: "W"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the parked child's move to backlog")
	if got := api.mode(parkedSandbox); got != "deleted" {
		t.Fatalf("parked child's Sandbox = %s, want it suspended and then deleted", got)
	}
	if options, deleted := api.deleted(parkedSandbox); !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("parked child's Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	if got := storedClaim(tester.Token); got.State != supervise.StateSuspended || got.Session != "saved-tester" || got.SessionFile != "/legion/sessions/tester.jsonl" {
		t.Fatalf("parked child's tester = %+v, want it suspended with its session kept", got)
	}
	if left := effectsLeft(parked.Key); left != 0 {
		t.Fatalf("%d supervise/issue_suspend rows of the parked child remain, want every one finished", left)
	}

	// (b) The done child set back to todo re-enters under its live tree and its planner starts
	// again: a fresh spawn into a new Sandbox whose workspace-init expects no volume. The fixture
	// runs no controller, so the launch ends at its wait for a pod; what it asked of the API is
	// what the assertion reads.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-todo", intake.DispatchIssue{Key: done.Key, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the re-entered child's branch and status")
	run("the re-entered child's planner start")
	if !slices.Contains(api.created, doneSandbox) {
		t.Fatalf("the re-entered child's start created %v, want a new Sandbox %s", api.created, doneSandbox)
	}
	if uid := api.uid(doneSandbox); uid == "" || uid == "sandbox-uid-done" {
		t.Fatalf("re-entered child's Sandbox uid = %q, want one other than the Sandbox its close deleted", uid)
	}
	env := api.initEnv(doneSandbox)
	if !slices.Contains(env, "PATH") {
		t.Fatalf("the re-entered child's workspace-init environment %v names no PATH; the Running patch wrote no pod template", env)
	}
	if got := storedClaim(planner.Token); got.SessionFile != "" {
		t.Fatalf("re-entered child's planner = %+v, want a fresh launch with no session to resume", got)
	}
}

// fenceChild is one child a reader asks the rig for: an issue at status standing at phase at,
// carrying a Sandbox of its own under uid, and one stopped role, the claim the rig seeds,
// restores and is read against. The rig names the issue itself, in the order the setup gives
// them: LEGION-209, LEGION-210, and so on.
type fenceChild struct {
	status string
	at     phase.Phase
	role   claim.Role
	uid    string
}

// fenceSetup is the arrangement a reader asks the rig to stand up.
type fenceSetup struct {
	children []fenceChild
	// rootImplementer gives the root the implementer of the phase it stands in, holding the
	// delivery it was given: an issue that holds an admission slot has a worker with the task of
	// its phase, and admission's pass starts one again for a slot whose worker has none (admit's
	// resumeSlottedWorkers). A reader that must see no launch of the root's own asks for it.
	rootImplementer bool
}

// oneFenceChild is the setup the readers of the fence itself take: the single child at status,
// standing at phase at, whose one stopped role is role. Its Sandbox carries the uid those readers
// name in their assertions.
func oneFenceChild(status string, at phase.Phase, role claim.Role) fenceSetup {
	return fenceSetup{children: []fenceChild{{status: status, at: at, role: role, uid: "sandbox-uid-child"}}}
}

// fenceIssue is one child as the rig seeded it: the issue the records hold, the Sandbox the API
// serves for it, and its one stopped role, holding the session a departure leaves it.
type fenceIssue struct {
	record.Issue
	sandbox   string
	suspended supervise.Claim
}

// fenceRig is the fixture a reader of the start fence and of a departure's effects stands on: a
// running root holding its admission slot, the setup's children under it, each with a Sandbox and
// one stopped role of its own, and the outbox over the real Sandbox runtime. Every reader takes a
// rig of its own, because a departure gives its issue's Sandbox back and a Sandbox is given back
// once.
type fenceRig struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	st        *store.Store
	api       *issueSandboxAPI
	engine    *workflow.Engine
	admission *admit.Admission
	// logs is everything the fixture logged, at debug, for a reader that reads a line of it.
	logs *syncBuffer
	root record.Issue
	// rootSandbox is the root's own Sandbox, the one no child's departure may touch.
	rootSandbox string
	// architect is the root's working role, which keeps the tree live: a lingering tree has a
	// start fence of its own, and the fence a rig reads must be the issue's own standing.
	architect    supervise.Claim
	children     []fenceIssue
	base         time.Time
	run          func(what string)
	storedClaim  func(token claim.Token) supervise.Claim
	effectsLeft  func(issue string) int
	enqueueStart func(issue string, role claim.Role, due time.Time) int64
}

// newFenceRig builds the rig the setup asks for.
func newFenceRig(t *testing.T, setup fenceSetup) *fenceRig {
	t.Helper()
	if len(setup.children) == 0 {
		t.Fatal("a fence rig stands on at least one child")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	parent := root.Key
	rootSandbox := "legion-legion-" + strings.ToLower(root.Key)
	sandboxes := []map[string]any{issueSandbox(rootSandbox, "sandbox-uid-root", root.Key, root.Key)}
	children := make([]fenceIssue, 0, len(setup.children))
	putOutboxIssue(t, pool, records, root)
	for i, spec := range setup.children {
		key := fmt.Sprintf("LEGION-%d", 209+i)
		child := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: spec.at, Generation: 1, Status: spec.status, Rank: string(rune('V' + i)), LastDispatchSeq: 1}
		name := "legion-legion-" + strings.ToLower(key)
		sandboxes = append(sandboxes, issueSandbox(name, spec.uid, root.Key, key))
		children = append(children, fenceIssue{Issue: child, sandbox: name})
		putOutboxIssue(t, pool, records, child)
	}
	approved := 1
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	api := newIssueSandboxAPI(sandboxes...)
	server := httptest.NewServer(api)
	t.Cleanup(func() { cancel(); server.Close() })
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	t.Cleanup(stopRuntime)
	rt, err := sandbox.New(runtimeCtx, sandboxRESTConfig(server.URL), sandbox.Options{
		Namespace: "legion", Project: "legion", Store: st, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
		SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
		Tools:        sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
		BootTimeout:  300 * time.Millisecond, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: 100 * time.Millisecond, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
		Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, logger),
		ControllerCredential: appauth.ControllerCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, logger),
		GitHubDirs:           legionDirs, Conns: fake.NewConns(), Log: logger,
		RepositoryCache: defaultRepositoryCache(t), Mapped: noRepositories,
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the role.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(runtimeCtx, st, "legion", t.TempDir(), logger)
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: issueLaunchSpecs{prompt: prompt}, Clock: stillClock{}, Log: logger,
		Limits:         supervise.Limits{LaunchFailures: 1, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
	}
	t.Cleanup(sup.stop)
	// The root's architect works on, with its session, so the tree runs and never lingers. Each
	// child's role stands stopped with its session kept, as a phase's end or a park leaves it.
	architect := supervise.Claim{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: root.Key, Role: claim.RoleArchitect,
		State: supervise.StateWorking, Generation: 1, Session: "saved-architect", SessionFile: "/legion/sessions/architect.jsonl"}
	seeded := []supervise.Claim{architect}
	if setup.rootImplementer {
		seeded = append(seeded, supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: root.Key, Role: claim.RoleImplementer,
			State: supervise.StateWorking, Generation: 1, Session: "saved-implementer", SessionFile: "/legion/sessions/implementer.jsonl",
			Pending: &supervise.Delivery{ID: "outbox:1", Task: "Continue root. Issue: LEGION-208. Phase: implementing.", Generation: 1,
				Phase: phase.Implementing, QueuedAt: time.Now(), DeliveredAt: time.Now(), ConfirmedAt: time.Now()}})
	}
	stopped := make([]supervise.Claim, 0, len(setup.children))
	for i, spec := range setup.children {
		child := children[i].Issue
		stopped = append(stopped, supervise.Claim{Token: mustClaimToken(t, child.Key, spec.role), Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: child.Key, Role: spec.role,
			State: supervise.StateSuspended, Generation: 1, Session: "saved-" + string(spec.role), SessionFile: "/legion/sessions/" + string(spec.role) + ".jsonl"})
		children[i].suspended = stopped[i]
	}
	for _, c := range append(seeded, stopped...) {
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sup.restore(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	clock := base
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, logger)
	admission := admit.New(records, engine, 2, "LEGION", logger)
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st, tokens: outboxTokens{},
		stateDir: t.TempDir(), githubAPI: newBranchGitHub(t, nil, branchExists).url,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: children[0].Key, Status: "todo"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine, admission}, log: logger, now: func() time.Time { return clock },
	}
	rig := &fenceRig{ctx: ctx, pool: pool, st: st, api: api, engine: engine, admission: admission, logs: logs,
		root: root, rootSandbox: rootSandbox, architect: architect, children: children, base: base}
	rig.run = func(what string) {
		t.Helper()
		// The clock may not simply advance: the rows the facts write carry next_at from the real
		// clock — the engine here has no Clock of its own and admission always reads time.Now —
		// and a row is due only when the runner's clock is at or past it, so a rig clock left
		// behind the real one leaves those rows undue and the drain acts on nothing.
		clock = clock.Add(2 * time.Second)
		if now := time.Now(); clock.Before(now) {
			clock = now
		}
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	rig.storedClaim = func(token claim.Token) supervise.Claim {
		t.Helper()
		claims, err := st.Claims(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claims {
			if c.Token == token {
				return c
			}
		}
		t.Fatalf("no stored claim %s", token)
		return supervise.Claim{}
	}
	rig.effectsLeft = func(issue string) int {
		t.Helper()
		var left int
		if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind in ('supervise', 'issue_suspend')`, issue).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left
	}
	// enqueueStart queues issue's start of role and answers its row id. It names no phase, as a
	// relaunch and a production check's look do, so the phase fence never answers for the standing
	// fence a rig's reader reads; due is the row's next_at, which is what puts it behind a
	// departure's own rows in the drain (record.Store.DueRows orders by next_at, then id).
	rig.enqueueStart = func(issue string, role claim.Role, due time.Time) int64 {
		t.Helper()
		row := mustOutboxRow(t, issue, record.SuperviseRequest{Op: "start", Tree: root.Key, Role: role, Generation: 1}, due)
		var id int64
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			id, err = records.EnqueueID(ctx, tx, row)
			return err
		}); err != nil {
			t.Fatalf("enqueue the %s's start: %v", role, err)
		}
		return id
	}
	return rig
}

// A start that outlives its issue's departure from the workflow starts nothing. The workflow
// writes a role's start while its issue is still in the workflow, and the row can reach the runner
// after the close or the park that took that issue out: it serves the same generation, it names
// the same tree, and a start that names no phase has no phase to be held to, so none of the
// start's own fences stands between it and the claim. The departure has already given the issue's
// Sandbox back, so a start acted on here builds the pod again behind an issue the board shows
// closed or parked, and brings back the claim that departure had just put down. Which of the two
// rows was written first decides nothing: a stop is fenced by the newest start run against its
// claim and not by the row ids, so a start queued before the departure and one queued after it
// both reach the runner behind it, and both must be declined. The Sandbox goes exactly once
// either way: the runtime reads it before it deletes and returns at once when it is already gone
// ((*Runtime).SuspendIssue), so no second delete is ever issued, and a drain that ends with no
// effect of the issue left has no suspension that could issue one. The fence is the status rule
// itself (record.OutOfWorkflow) rather than a list of its own, so every status that takes an issue
// out of the workflow declines the start while a status the workflow runs in still starts it.
func TestAStartThatOutlivesItsIssuesDepartureStartsNothing(t *testing.T) {
	// departed reads what a declined start leaves behind: nothing launched — the fixture seeds the
	// child's Sandbox, so a Sandbox created here is a relaunch and nothing else — no start recorded
	// against the claim, a claim with no process and no locator, and the issue's Sandbox given back
	// with its pod and the disk under it, fenced to the Sandbox the departure read. The delete
	// happened once: nothing is left of the issue to issue a second one.
	departed := func(t *testing.T, rig *fenceRig, child fenceIssue, start int64) {
		t.Helper()
		if slices.Contains(rig.api.created, child.sandbox) {
			t.Fatalf("the start of the departed issue created %v, want no Sandbox %s built again", rig.api.created, child.sandbox)
		}
		got := rig.storedClaim(child.suspended.Token)
		if got.LastStartRow != 0 {
			t.Fatalf("the departed issue's planner records start row %d, want start row %d declined before it was recorded", got.LastStartRow, start)
		}
		if !got.RunsNothing() || got.Locator != nil {
			t.Fatalf("the departed issue's planner = %s with locator %+v, want a claim that runs nothing and holds no process", got.State, got.Locator)
		}
		options, deleted := rig.api.deleted(child.sandbox)
		if mode := rig.api.mode(child.sandbox); mode != "deleted" || !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatalf("the departed issue's Sandbox = %s, delete %+v (requested %t), want it deleted with foreground propagation", mode, options, deleted)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "sandbox-uid-child" {
			t.Fatalf("the departed issue's Sandbox delete preconditions = %+v, want fenced to uid sandbox-uid-child", options.Preconditions)
		}
		if left := rig.effectsLeft(child.Key); left != 0 {
			t.Fatalf("%d supervise/issue_suspend rows of the departed issue remain, want the start finished and no suspension left to delete its Sandbox a second time", left)
		}
	}

	// The start queued before the close carries the lower id and the later next_at, so the drain
	// hands it to the runner after the close's own rows: the claim is retired and the Sandbox
	// given back by the time it runs, and it must start nothing on top of that.
	t.Run("a start queued before the close runs after it", func(t *testing.T) {
		rig := newFenceRig(t, oneFenceChild("testing", phase.Testing, claim.RolePlanner))
		child := rig.children[0]
		start := rig.enqueueStart(child.Key, claim.RolePlanner, rig.base.Add(time.Second))
		if _, err := intake.ApplyFact(rig.ctx, rig.pool, "dispatch", "child-done", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: child.Title, Parent: "LEGION-208", Rank: "V"}, rig.engine, rig.admission); err != nil {
			t.Fatal(err)
		}
		rig.run("the child's close, with a start queued before it")
		departed(t, rig, child, start)
		if got := rig.storedClaim(child.suspended.Token); got.State != supervise.StateRetired {
			t.Fatalf("the closed issue's planner = %s, want it left retired as its close retired it", got.State)
		}
	})

	// The same start written after the close carries the higher id, which is the order a stop's
	// own fence (workflow.StopActs) would read as a newer run: it is not one, and the start is
	// declined as the one before it was.
	t.Run("a start queued after the close waits behind it", func(t *testing.T) {
		rig := newFenceRig(t, oneFenceChild("testing", phase.Testing, claim.RolePlanner))
		child := rig.children[0]
		if _, err := intake.ApplyFact(rig.ctx, rig.pool, "dispatch", "child-done", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: child.Title, Parent: "LEGION-208", Rank: "V"}, rig.engine, rig.admission); err != nil {
			t.Fatal(err)
		}
		start := rig.enqueueStart(child.Key, claim.RolePlanner, rig.base.Add(time.Second))
		rig.run("the child's close, with a start queued after it")
		departed(t, rig, child, start)
		if got := rig.storedClaim(child.suspended.Token); got.State != supervise.StateRetired {
			t.Fatalf("the closed issue's planner = %s, want it left retired as its close retired it", got.State)
		}
	})

	// A park is the same departure by another op: the child moved to backlog has stopped rather
	// than finished, so its roles keep their sessions and the departure suspends them where a
	// close retires them. Its Sandbox goes either way, and so does the start.
	t.Run("a start queued before the park runs after it", func(t *testing.T) {
		rig := newFenceRig(t, oneFenceChild("in_progress", phase.Planning, claim.RolePlanner))
		child := rig.children[0]
		start := rig.enqueueStart(child.Key, claim.RolePlanner, rig.base.Add(time.Second))
		if _, err := intake.ApplyFact(rig.ctx, rig.pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: child.Title, Parent: "LEGION-208", Rank: "V"}, rig.engine, rig.admission); err != nil {
			t.Fatal(err)
		}
		rig.run("the child's park, with a start queued before it")
		departed(t, rig, child, start)
		if got := rig.storedClaim(child.suspended.Token); got.State != supervise.StateSuspended || got.Session != "saved-planner" || got.SessionFile != "/legion/sessions/planner.jsonl" {
			t.Fatalf("the parked issue's planner = %+v, want it left suspended with its session kept", got)
		}
	})

	// The fence is the standing rule and not a list of its own. Every status record.OutOfWorkflow
	// names declines the start, whether or not a departure ran in this drain, and the control
	// below starts the same row at a status the workflow runs in: without it a fence written for
	// done and backlog alone would read the same here.
	for _, status := range []string{"triage", "icebox", "backlog", "done"} {
		t.Run("a start of an issue at "+status, func(t *testing.T) {
			if !record.OutOfWorkflow(status) {
				t.Fatalf("%s no longer takes an issue out of the workflow; this table reads the rule the fence is held to", status)
			}
			rig := newFenceRig(t, oneFenceChild(status, phase.Planning, claim.RolePlanner))
			child := rig.children[0]
			start := rig.enqueueStart(child.Key, claim.RolePlanner, rig.base)
			rig.run("the start of the issue at " + status)
			if slices.Contains(rig.api.created, child.sandbox) {
				t.Fatalf("the start of the issue at %s created %v, want no Sandbox built behind it", status, rig.api.created)
			}
			got := rig.storedClaim(child.suspended.Token)
			if got.LastStartRow != 0 || got.State != supervise.StateSuspended || got.Session != "saved-planner" {
				t.Fatalf("the planner of the issue at %s = %s with last start %d and session %q, want start row %d declined and the claim untouched",
					status, got.State, got.LastStartRow, got.Session, start)
			}
			if left := rig.effectsLeft(child.Key); left != 0 {
				t.Fatalf("the start of the issue at %s left %d rows, want it declined and finished", status, left)
			}
		})
	}
	t.Run("a start of an issue the workflow still runs", func(t *testing.T) {
		if record.OutOfWorkflow("testing") {
			t.Fatal("testing now takes an issue out of the workflow; this control reads a status the workflow runs in")
		}
		rig := newFenceRig(t, oneFenceChild("testing", phase.Testing, claim.RolePlanner))
		child := rig.children[0]
		start := rig.enqueueStart(child.Key, claim.RolePlanner, rig.base)
		rig.run("the start of the issue at testing")
		if got := rig.storedClaim(child.suspended.Token); got.LastStartRow != start {
			t.Fatalf("the planner of the issue at testing records start row %d, want start row %d to have reached it", got.LastStartRow, start)
		}
	})
}

// An issue that comes back is not stranded by the start fence. record.OutOfWorkflow, the rule the
// fence is held to, names the parked statuses as well as done, so the start of a parked issue is
// declined with the rest (TestAStartThatOutlivesItsIssuesDepartureStartsNothing above). What that
// must never cost is the way back. The way back is the way in: an issue a person brings back is
// started by the run that brings it and never by the row the fence declined, and there are two
// doors it comes in through. A parked child moved to todo re-enters under its live tree
// (Engine.ReenterChild), which clears the run the park left and hands the waiting line its first
// phase to start again. A tree whose linger expired and whose root a person sets back to todo is
// re-admitted, and its promotion starts every member where it stood (admit's
// startMidPhaseMembers). Each door is read in the lighter rig that can express it: the first over
// the real Sandbox runtime, because what it must show is the pod built again behind the issue that
// came back, and the second over the stand-in runtime, because what it must show is which rows the
// re-admission wrote and which claim each brought back.
func TestAnIssueThatComesBackIsNotStrandedByTheStartFence(t *testing.T) {
	// Door one. The rig is newFenceRig's, the one
	// TestAStartThatOutlivesItsIssuesDepartureStartsNothing reads the fence in, cut to the single
	// park this door needs: a running root holding its admission slot, the tree's one child under
	// it standing mid-run in implementing with its implementer stopped and its session kept, a
	// Sandbox apiece, and the outbox over the real Sandbox runtime.
	t.Run("a parked child moved back to todo re-enters and starts its first phase", func(t *testing.T) {
		rig := newFenceRig(t, oneFenceChild("in_progress", phase.Implementing, claim.RoleImplementer))
		child := rig.children[0]

		// The implementer's start, written while the child still stood in the workflow and reaching
		// the runner behind the park that took it out: its next_at puts it behind the park's own
		// rows in the drain (record.Store.DueRows).
		startID := rig.enqueueStart(child.Key, claim.RoleImplementer, rig.base.Add(time.Second))
		if _, err := intake.ApplyFact(rig.ctx, rig.pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: child.Title, Parent: rig.root.Key, Rank: "V"}, rig.engine, rig.admission); err != nil {
			t.Fatal(err)
		}
		rig.run("the child's park, with its implementer's start queued behind it")
		if slices.Contains(rig.api.created, child.sandbox) {
			t.Fatalf("the park's drain created %v, want the start of the parked child declined and no pod built behind it", rig.api.created)
		}
		if got := rig.storedClaim(child.suspended.Token); got.LastStartRow != 0 || got.State != supervise.StateSuspended || got.SessionFile != child.suspended.SessionFile {
			t.Fatalf("the parked child's implementer = %s with last start %d and session file %q, want start row %d declined and the claim left suspended with its session",
				got.State, got.LastStartRow, got.SessionFile, startID)
		}
		if left := rig.effectsLeft(child.Key); left != 0 {
			t.Fatalf("%d supervise/issue_suspend rows of the parked child remain, want the declined start finished with the park's own", left)
		}

		// The person moves the parked child back to todo. Engine.ReenterChild clears the run the
		// park left and enters the child again under its live tree at the next generation, standing
		// at admitted; the waiting line promotes it in the same pass, and what it starts is the
		// child's first phase (record.LineFirstPhase), not the phase the park interrupted.
		if _, err := intake.ApplyFact(rig.ctx, rig.pool, "dispatch", "child-todo", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: child.Title, Parent: rig.root.Key, Rank: "V"}, rig.engine, rig.admission); err != nil {
			t.Fatal(err)
		}
		back := readIssue(t, rig.pool, child.Key)
		if back == nil {
			t.Fatalf("the re-entered child %s is gone from the records", child.Key)
		}
		if back.Generation != child.Generation+1 || back.Phase != phase.Planning || back.Tree != rig.root.Key {
			t.Fatalf("the re-entered child = %s at generation %d in tree %s, want its first phase at generation %d under the tree it was parked in",
				back.Phase, back.Generation, back.Tree, child.Generation+1)
		}
		// Nothing of the old run is wanted back: the one start the re-entry queued is the
		// planner's, of the new generation and the new run's first phase, carrying that phase's
		// own task and the assignment it minted, which is no resume of anything
		// (workflow.StartOrOwe).
		queued := startRequestsOf(t, rig.pool, child.Key)
		if len(queued) != 1 {
			t.Fatalf("the re-entry queued %d starts, want the one of the planner's first phase: %+v", len(queued), queued)
		}
		if row := queued[0]; row.Role != claim.RolePlanner || row.Generation != back.Generation || row.Phase != phase.Planning || row.ResumeTask {
			t.Fatalf("the re-entry queued %+v, want a start of the planner at %s generation %d carrying no resume of the old run", row, phase.Planning, back.Generation)
		}
		if row := queued[0]; row.Task == "" || row.Assignment == "" {
			t.Fatalf("the re-entry's start = %+v, want it carrying the phase's own task and the assignment it minted", row)
		}

		// The fixture runs no controller, so the planner's launch ends at its wait for a pod and
		// its rows wait on their backoff for the next one: what it asked of the API, and what it
		// recorded against the claim, is what the assertions below read.
		rig.run("the re-entered child's branch and status")
		rig.run("the re-entered child's planner start")
		if !slices.Contains(rig.api.created, child.sandbox) {
			t.Fatalf("the re-entered child's start created %v, want the Sandbox %s built again: the way back is the way in", rig.api.created, child.sandbox)
		}
		if uid := rig.api.uid(child.sandbox); uid == "" || uid == "sandbox-uid-child" {
			t.Fatalf("the re-entered child's Sandbox uid = %q, want one other than the Sandbox its park deleted", uid)
		}
		if got := rig.storedClaim("legion-legion-legion-209-planner"); got.LastStartRow == 0 || got.SessionFile != "" {
			t.Fatalf("the re-entered child's planner = %+v, want a fresh launch of the new run's first phase", got)
		}
		// No role of the old run is resumed: the implementer whose start the fence declined is left
		// exactly as the park left it, and the run that came back never asked for it.
		if got := rig.storedClaim(child.suspended.Token); got.LastStartRow != 0 || got.State != supervise.StateSuspended || got.Session != child.suspended.Session {
			t.Fatalf("the re-entered child's implementer = %+v, want the old run's role left suspended with its session and no start recorded against it", got)
		}
	})

	// Door two. The whole tree stopped, so what must come back is a member standing in a worker
	// phase, and what this door reads is which rows the re-admission wrote for it and which claim
	// each one brought back — nothing of a pod or a disk — so it takes the lighter rig
	// TestAClosedTreeSetBackToTodoRelaunchesItsArchitect builds, the stand-in runtime over a
	// supervisor of its own, with a member added beneath that test's root.
	t.Run("a re-admitted tree starts its mid-phase member again", func(t *testing.T) {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		clock := time.Now()
		root := record.Issue{Key: "LEGION-308", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-308", Title: "root", Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
		parent := root.Key
		member := record.Issue{Key: "LEGION-309", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "member", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "V", LastDispatchSeq: 3}
		for _, issue := range []record.Issue{root, member} {
			putOutboxIssue(t, pool, records, issue)
		}
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
			Linger: time.Hour, Clock: func() time.Time { return clock }}, quietLogger())
		admission := admit.New(records, engine, 2, "LEGION", quietLogger())
		runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup,
			trees: outboxTreeStore(t, pool, root.Key), tokens: outboxTokens{}, stateDir: t.TempDir(),
			githubAPI: newBranchGitHub(t, nil, branchExists).url, notices: &outboxPublisher{},
			dispatch: &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "todo"}},
			handlers: []intake.Handler{engine, admission}, log: quietLogger(), now: func() time.Time { return clock },
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + member.Key}, nil
			},
			remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
		}
		run := func(what string) {
			t.Helper()
			// The engine here takes this clock, but admission does not: admit.New always reads
			// time.Now (admit.go:107) and writes its rows with it, so the promotion's rows carry
			// the real clock whatever the engine's is. A row is due only when the runner's clock
			// is at or past its next_at, so this clock may not simply advance: left behind the
			// real one on a slow runner, it leaves those rows undue and the drain acts on nothing.
			clock = clock.Add(2 * time.Second)
			if now := time.Now(); clock.Before(now) {
				clock = now
			}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		// The tree as it ran: its architect, the member's implementer working the phase the member
		// stands in, and the member's planner, a role of an earlier phase left live with its
		// session so that a person can still talk to it. Each is spawned and registered on the
		// supervisor itself rather than through a start row of its own, so that every start the
		// outbox runs in this door is one the way back wrote, and the arrangement stands whatever
		// the fence does with a start.
		started := func(issue string, role claim.Role, generation uint64, session string) claim.Token {
			t.Helper()
			tok := mustClaimToken(t, issue, role)
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: tok, Project: "legion", Tree: root.Tree, Issue: issue,
				Role: role, State: supervise.StateQueued, Generation: generation}, "")
			if err != nil {
				t.Fatalf("create the %s of %s: %v", role, issue, err)
			}
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: tok}); err != nil {
				t.Fatalf("spawn the %s of %s: %v", role, issue, err)
			}
			machine.Wait()
			if err := machine.Handle(ctx, supervise.RequestRegister{Claim: tok, Generation: machine.Claim().Generation,
				Session: "ses-" + string(role), SessionFile: session}); err != nil {
				t.Fatalf("register the %s of %s: %v", role, issue, err)
			}
			return tok
		}
		started(root.Key, claim.RoleArchitect, root.Generation, "/tmp/architect.jsonl")
		worker := started(member.Key, claim.RoleImplementer, member.Generation, "/tmp/implementer.jsonl")
		earlier := started(member.Key, claim.RolePlanner, member.Generation, "/tmp/planner.jsonl")

		// A person parks the root, which begins the tree's linger and suspends every member's
		// claims where they stand (workflow's beginLinger). A root parked rather than finished
		// hands no member to Legion when the linger ends (workflow's unfinishedHandOvers), so the
		// member is still this tree's when the tree comes back.
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: root.Rank}, engine, admission); err != nil {
			t.Fatal(err)
		}
		run("the root's park")
		// The linger runs out: every role of every member is retired, keeping its session, and the
		// tree's workspaces go.
		if _, err := intake.ApplyFact(ctx, pool, "outbox", "linger:LEGION-308:3", intake.LingerExpired{Issue: root.Key, Generation: root.Generation}, engine, admission); err != nil {
			t.Fatal(err)
		}
		run("the linger's close of the tree")
		for _, tok := range []claim.Token{worker, earlier} {
			machine, found := sup.Machine(tok)
			if !found {
				t.Fatalf("no machine for %s after the linger closed the tree", tok)
			}
			machine.Wait()
			if got := machine.Claim(); got.State != supervise.StateRetired || got.SessionFile == "" {
				t.Fatalf("%s after the linger closed the tree = %s with session file %q, want it retired with its session kept", tok, got.State, got.SessionFile)
			}
		}

		// The person sets the closed root back to todo. It is a queued root again
		// (record.QueuedRoot), and its promotion opens the tree and starts its members.
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-todo", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 7, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: true}, engine, admission); err != nil {
			t.Fatal(err)
		}
		carried := readIssue(t, pool, member.Key)
		if carried == nil {
			t.Fatalf("the member %s is gone from the records after the re-admission", member.Key)
		}
		if carried.Phase != phase.Implementing || carried.Generation != member.Generation {
			t.Fatalf("the member after the re-admission = %s at generation %d, want it still standing where the linger left it", carried.Phase, carried.Generation)
		}
		// The rows admit.startMidPhaseMembers wrote for the member, which is the only pass that
		// wrote a start of it here: the phase's own role, the implementer, through
		// workflow.StartOrOwe, which writes one row carrying the phase's resume task
		// (workflow.ResumeTask), marked ResumeTask and held unclaimable until the member holds a
		// slot — and every other role one start of its own with no task at all, since a role with
		// nothing to do comes back and is told nothing.
		resume := workflow.ResumePhaseTask(*carried)
		taskless := map[claim.Role]bool{}
		var owed *record.SuperviseRequest
		for _, row := range startRequestsOf(t, pool, member.Key) {
			if row.Generation != carried.Generation || row.Phase != carried.Phase || row.Tree != root.Tree {
				t.Fatalf("the re-admission queued %+v for the member, want every start to serve its own run and the phase it stands in", row)
			}
			if row.Role != claim.RoleImplementer {
				taskless[row.Role] = row.Task == "" && !row.NeedsSlot && !row.ResumeTask
				continue
			}
			if owed != nil {
				t.Fatalf("the re-admission queued a second start of the member's implementer, %+v: its start is one row that carries the task", row)
			}
			owed = &row
		}
		for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			if !taskless[role] {
				t.Fatalf("the re-admission wrote no task-less start of the member's %s, want every role of the member brought back", role)
			}
		}
		if owed == nil || owed.Task != resume || !owed.ResumeTask || !owed.NeedsSlot {
			t.Fatalf("the member's start row = %+v, want the one StartOrOwe writes: the phase's resume task %q, held for the member's slot", owed, resume)
		}

		run("the re-admitted tree's branches and relaunches")
		run("the relaunches left over the branches")
		// The member's task row waits for a slot of its own, which the next admission pass grants;
		// the controller's tick is that pass here.
		if _, err := intake.ApplyFact(ctx, pool, "controller", "tick-after-the-readmission", intake.ControllerTick{}, engine, admission); err != nil {
			t.Fatal(err)
		}
		run("the member's resume task, once its slot came")
		for _, tc := range []struct {
			token   claim.Token
			session string
		}{{worker, "/tmp/implementer.jsonl"}, {earlier, "/tmp/planner.jsonl"}} {
			machine, _ := sup.Machine(tc.token)
			machine.Wait()
			resumes := resumesOf(rt, tc.token)
			if len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != tc.session {
				t.Fatalf("%s came back on %+v, want its kept session %s relaunched exactly once", tc.token, resumes, tc.session)
			}
		}
		machine, _ := sup.Machine(worker)
		machine.Wait()
		if got := machine.Claim(); got.Pending == nil || got.Pending.Task != resume || got.Pending.Phase != carried.Phase {
			t.Fatalf("the member's implementer holds %+v, want the phase's resume task carried by the row that waited for the slot", got.Pending)
		}
		if got, _ := sup.Machine(earlier); got.Claim().Pending != nil {
			t.Fatalf("the member's planner holds %+v, want a role with nothing to do brought back and told nothing", got.Claim().Pending)
		}
		var left int
		if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'`, member.Key).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("%d supervise rows of the re-admitted member remain, want every start of the new run run", left)
		}
	})
}

// startRequestsOf is every start the outbox holds for issue, oldest first, decoded as the pass
// that queued it wrote it. The rows a drain has run are gone, so a reader must take them before
// it drains. The name is this file's own: internal/daemon's stopped_worker_test.go has a
// startRowsOf of its own, which answers the same rows as rendered strings.
func startRequestsOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []record.SuperviseRequest
	for rows.Next() {
		var payload json.RawMessage
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a start row of %s: %v", issue, err)
		}
		var request record.SuperviseRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode a start row of %s: %v", issue, err)
		}
		starts = append(starts, request)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	return starts
}

// A production check's look starts its role behind a root that does not linger. The issue is
// done, so the standing status fence (record.OutOfWorkflow) would decline any other start of it,
// and its tree's linger has run out, so the lingering-tree guard never reaches the row: neither
// of the two paths that let a check start through a closed issue before this one is open here.
// What is left is the look's own exemption — the fence sits inside payload.CheckLook == 0, so a
// look is the one start an issue out of the workflow still runs — and this arrangement is the
// one that fence alone governs. The role the check records comes back in the stand-in runtime,
// the claim records the row as its newest start, and the issue is still done when the look is
// running: a look runs behind the close rather than reopening the issue.
func TestAProductionCheckLookStillStartsItsRole(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	// The linger the close gave the tree has run out while the check stays open: the check
	// outlives the tree it was opened under (two weeks against an hour), so this is the state a
	// look spends nearly all its life in.
	root.LingerUntil = nil
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

	row := checkStart(t, root, 1)
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("check start under a root that does not linger = %v, want the implementer started", err)
	}
	if got := machine.Claim(); got.LastStartRow != row.ID || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("implementer after the look's start = %s with last start %d and %d resumes, want it resumed by row %d",
			got.State, got.LastStartRow, len(rt.CallsOf("Resume")), row.ID)
	}
	after := readIssue(t, pool, root.Key)
	if after == nil {
		t.Fatalf("%s is gone from the records after the look's start", root.Key)
	}
	if after.Status != "done" || after.Lingers() {
		t.Fatalf("%s after the look's start = %q lingering until %v, want it left done under a tree that does not linger",
			root.Key, after.Status, after.LingerUntil)
	}
}
