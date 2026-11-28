package daemon

import (
	"context"
	"errors"
	"sync"
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
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// storeSupervisedClaims writes the claims row of each claim of tokens as its machine holds it, for
// a supervisor whose own store keeps its claims in memory (outboxClaimStore): the daemon's store
// holds a row for every claim it supervises, and a walk over every claim of an issue reads those
// rows (record.Store.IssueClaims), writing a row that names a claim only for a claim it finds
// there, and a row by the role alone for each role whose worker claim is none of those
// (workflow's claimTargets).
func storeSupervisedClaims(t *testing.T, pool *store.Pool, sup *supervisor, tokens ...claim.Token) {
	t.Helper()
	for _, token := range tokens {
		machine, found := sup.Machine(token)
		if !found {
			t.Fatalf("no claim %s to store", token)
		}
		c := machine.Claim()
		if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, 0, 0, 0) on conflict (token) do nothing`,
			string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), int64(c.Generation), c.Session, c.SessionFile, string(c.State)); err != nil {
			t.Fatalf("store the claim %s: %v", token, err)
		}
	}
}

// storeRoleClaims stores, for issue of tree, an idle claim of each role of Legion, as the daemon's
// store holds the claim of each role it ran there: what a walk over every claim of the issue reads
// (record.Store.IssueClaims), which writes no row for a claim the issue does not hold.
func storeRoleClaims(t *testing.T, pool *store.Pool, tree, issue string) {
	t.Helper()
	for _, role := range claim.Roles {
		token, err := claim.NewToken("legion", issue, role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $3, $4, 1, '', '', 'idle', 0, 0, 0, 0) on conflict (token) do nothing`,
			string(token), tree, issue, string(role)); err != nil {
			t.Fatalf("store the %s's claim of %s: %v", role, issue, err)
		}
	}
}

// Key a's agent works the tester's step in the claim it started as implementer, so the tester's
// phases row names `…-implementer`, and the fresh tester an earlier version of the plan started,
// `…-tester`, still holds its own claim. When the tree's linger expires, its close writes a row for
// each stored claim of the issue, each naming it: both claims retire, the tree's cleanup, which
// waits for every stored claim of the tree, confirms its reservation so the tree's next epoch can
// open, and the workspace removal acts. Close rows by role reached the claim of the worker on the
// tester's step twice and left `…-tester` running, so the cleanup and the removal waited for ever.
func TestALingersCloseRetiresAnEarlierVersionsClaimAndTheTreeIsCleanedUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, root)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	rt := fake.NewRuntime()
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	var removed sync.Mutex
	removals := 0
	// The drain's clock runs an hour ahead, so a row the cleanup's wait sent back is due again.
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", now: func() time.Time { return time.Now().Add(time.Hour) },
		pool: pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error {
			removed.Lock()
			defer removed.Unlock()
			removals++
			return nil
		},
	}
	keyed, fresh := mustClaimToken(t, root.Key, claim.RoleImplementer), mustClaimToken(t, root.Key, claim.RoleTester)
	for i, start := range []record.SuperviseRequest{
		{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Task: "Implement it.", Generation: 1},
		{Op: "start", Tree: root.Tree, Role: claim.RoleTester, Task: "Test it.", Generation: 1},
	} {
		row := mustOutboxRow(t, root.Key, start, time.Now())
		row.ID = int64(40 + i)
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("start the %s: %v", start.Role, err)
		}
	}
	for _, token := range []claim.Token{keyed, fresh} {
		if _, found := sup.Machine(token); !found {
			t.Fatalf("no claim %s after its start", token)
		}
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: root.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 1})
	}); err != nil {
		t.Fatalf("write the tester's phases row: %v", err)
	}

	until := time.Now().Add(-time.Minute)
	root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
	putOutboxIssue(t, pool, records, root)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	if _, err := intake.ApplyFact(ctx, pool, "linger", "linger-208-1", intake.LingerExpired{Issue: root.Key, Generation: 1}, engine); err != nil {
		t.Fatalf("expire the tree's linger: %v", err)
	}
	for range 12 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	}

	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Tree == root.Key && c.State != supervise.StateRetired {
			t.Errorf("claim %s of the closed tree is %s, want retired", c.Token, c.State)
		}
	}
	root.Generation, root.Phase, root.Status, root.LingerUntil = 2, phase.Planning, "in_progress", nil
	putOutboxIssue(t, pool, records, root)
	if opened, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil || opened.Epoch != 2 {
		t.Errorf("the tree's next epoch after its close = %+v, %v; want epoch 2 open: the cleanup confirmed once every claim retired (reserved wait: %t)",
			opened, err, errors.Is(err, treelifecycle.ErrCleanupReserved))
	}
	removed.Lock()
	defer removed.Unlock()
	if removals != 1 {
		t.Errorf("workspace removals = %d, want the linger's one once every claim retired", removals)
	}
}
