package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// refreshCountSource is a source over cfg's project reading st, with the one rule the claims do
// not carry: a claim of role reviewer is a role whose launch installs no plugin, which is what a
// Claude Code role under Kubernetes is to the daemon's own source (noPluginInstall). Nothing
// else of the source matters to the count, and the rest is what stranded_test.go's bare source
// holds.
func refreshCountSource(t *testing.T, cfg config.Config, st *store.Store) *source {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	return &source{
		store: st, records: record.NewStore(), project: cfg.Project, projectToken: project,
		runtime: "tmux", admissionCap: cfg.AdmissionCap, startedAt: time.Now(),
		deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
		reports:    func() map[claim.Token]capabilities.Report { return nil },
		noPluginInstall: func(_ context.Context, _ pgx.Tx, c supervise.Claim) (bool, error) {
			return c.Role == claim.RoleReviewer, nil
		},
	}
}

// refreshCountState is the state refreshCountSource answers through a snapshot of its own.
func refreshCountState(t *testing.T, cfg config.Config, st *store.Store, view *source) api.State {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the state snapshot: %v", err)
	}
	defer tx.Rollback(ctx)
	state, _, err := view.State(ctx, tx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return state
}

// `GET /legion/v1/state` says how many claims the stale-plugin lane still has to relaunch onto
// this daemon's plugin, and the set it counts is the one the lane's own pass walks
// (refreshStalePlugins, claimsToRefresh): a claim with a recorded session, whose process holds a
// capability, whose recorded contract is not this daemon's, in no pod update round, and whose
// launch installs a plugin.
//
// Each claim the count leaves out is here for a reason of its own, because the lane leaves it
// out for a reason of its own: a claim already at this contract has nothing to refresh; a
// suspended one has no process to replace, and the one its next launch starts speaks this
// contract by itself; a claim in a pod round is passed over by the pass, which leaves it to the
// replacement that round is carrying; a claim whose launch installs no plugin can never be
// refreshed by a relaunch at all; and a claim with no recorded session has no process the lane
// could read a contract from.
//
// A claim in a plugin round is counted, because the pass posts it: the boot's own de-round pass
// has taken such a claim out of that round a moment before, and one machine's inbox is FIFO, so
// the relaunch is the lane's work and a count that left it out would hide that work at the one
// boot it exists to be read over (endPluginRounds, staleForTheLane).
//
// The count is read a second time with every claim brought onto this contract, because 0 is the
// answer an operator reads a drained wave by and a count that can only rise says nothing.
func TestTheStateCountsTheClaimsTheStalePluginLaneHasLeftToRefresh(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	stale := api.DaemonAPIVersion() - 1

	type seeded struct {
		issue   string
		role    claim.Role
		state   supervise.ClaimState
		shape   func(*supervise.Claim)
		counted bool
	}
	seeds := []seeded{
		{issue: cfg.Project + "-1", role: claim.RoleImplementer, state: supervise.StateIdle, counted: true},
		{issue: cfg.Project + "-2", role: claim.RoleImplementer, state: supervise.StateReady, counted: true},
		{issue: cfg.Project + "-3", role: claim.RoleImplementer, state: supervise.StateWorking, counted: true},
		{issue: cfg.Project + "-4", role: claim.RoleImplementer, state: supervise.StateRegistered, counted: true},
		{issue: cfg.Project + "-5", role: claim.RoleImplementer, state: supervise.StateIdle,
			shape: func(c *supervise.Claim) { c.PluginContract = api.DaemonAPIVersion() }},
		{issue: cfg.Project + "-6", role: claim.RoleImplementer, state: supervise.StateSuspended},
		{issue: cfg.Project + "-7", role: claim.RoleImplementer, state: supervise.StateIdle, counted: true,
			shape: func(c *supervise.Claim) {
				c.UpdateRound = supervise.PluginRoundPrefix + strconv.Itoa(api.DaemonAPIVersion())
			}},
		{issue: cfg.Project + "-8", role: claim.RoleImplementer, state: supervise.StateIdle,
			shape: func(c *supervise.Claim) { c.UpdateRound = supervise.PodRound("6f1c0c2a-1d1e-4a44-9d2c-0b7c1f3a9e55") }},
		{issue: cfg.Project + "-9", role: claim.RoleReviewer, state: supervise.StateIdle},
		{issue: cfg.Project + "-10", role: claim.RoleImplementer, state: supervise.StateIdle,
			shape: func(c *supervise.Claim) { c.Session = "" }},
	}

	run := previousRun{}
	want := 0
	for _, seed := range seeds {
		c := previousClaim(t, cfg, seed.issue, seed.issue, seed.role, seed.state)
		c.PluginContract = stale
		if seed.shape != nil {
			seed.shape(&c)
		}
		run.issues = append(run.issues, runningRoot(seed.issue, phase.Implementing))
		run.claims = append(run.claims, c)
		if seed.counted {
			want++
		}
	}
	if want == 0 || want == len(seeds) {
		t.Fatalf("the seeds count %d of %d claims; this test proves nothing", want, len(seeds))
	}
	putPreviousRun(t, cfg, run)

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	view := refreshCountSource(t, cfg, st)

	if got := refreshCountState(t, cfg, st, view).ClaimsToRefresh; got != want {
		t.Fatalf("claimsToRefresh = %d, want %d: the claims the lane's own pass would post", got, want)
	}

	// Every claim relaunched onto this daemon's plugin: the lane has nothing left, and the count
	// says so rather than going quiet.
	for _, c := range run.claims {
		c.PluginContract = api.DaemonAPIVersion()
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("bring %s onto this daemon's contract: %v", c.Token, err)
		}
	}
	if got := refreshCountState(t, cfg, st, view).ClaimsToRefresh; got != 0 {
		t.Fatalf("claimsToRefresh = %d once every claim records this contract, want 0", got)
	}
}

// Under Kubernetes the state's count asks, of a stale claim of a role that is none of Legion's,
// whether its launch installs a plugin, and that reads the plan of the claim's issue. The count
// runs inside the state's snapshot under a context that marks the connection the snapshot holds,
// as every API route's does, so the pool refuses that caller a connection of its own
// (store.ErrNestedAcquire). The deployment's prober runs on Claude Code. The plan of the first
// issue defines no prober, so its stale claim is one no install moves and the count leaves it
// out. The plan of the second defines a prober of its own, which runs on Oh My Pi, so an install
// moves its stale claim and the count names it. Only a read of the plan through the snapshot
// itself answers both (stateNoPluginInstall): a read refused or failed answers neither, so the
// count of 1 is what a read that left the snapshot cannot give.
func TestTheStateCountReadsAClaimsPlanThroughItsOwnSnapshot(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runtime.Name = "kubernetes"
	cfg.Roles = []config.Role{{Name: "prober", Type: "testing", GitHubApp: config.RoleAppReview}}
	cfg.WorkerBackends = map[claim.Role]runtime.Backend{"prober": runtime.BackendClaude}
	deployed, planned := cfg.Project+"-1", cfg.Project+"-2"
	var claims []supervise.Claim
	for _, issue := range []string{deployed, planned} {
		c := previousClaim(t, cfg, issue, issue, "prober", supervise.StateIdle)
		c.PluginContract = api.DaemonAPIVersion() - 1
		claims = append(claims, c)
	}
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(deployed, phase.Implementing), runningRoot(planned, phase.Implementing)},
		claims: claims,
	})

	ctx := store.WithTransactionTracking(context.Background())
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	putPlanRole(t, st, planned, record.PlanRole{Role: "prober", Type: "testing", Model: "prober-model", Prompt: "# Prober\n\nProbe the change."})
	view := refreshCountSource(t, cfg, st)
	view.noPluginInstall = stateNoPluginInstall(cfg)
	var state api.State
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, _, err = view.State(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.ClaimsToRefresh != 1 {
		t.Fatalf("claimsToRefresh = %d, want 1: the prober of %s is the deployment's on Claude Code, which no install moves, and the prober of %s is its plan's on Oh My Pi, which an install moves",
			state.ClaimsToRefresh, deployed, planned)
	}
}

// putPlanRole records version 1 of the plan of issue, at its generation 1, as defining role and
// no task: what a plan read of the role finds (record.Store.PlanRoleOf).
func putPlanRole(t *testing.T, st *store.Store, issue string, role record.PlanRole) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	role.Issue, role.Generation, role.PlanVersion = issue, 1, 1
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: issue, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, issue, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		if err := records.PutPlan(ctx, tx, v, nil, nil); err != nil {
			return err
		}
		return records.PutPlanRoles(ctx, tx, issue, 1, 1, []record.PlanRole{role})
	}); err != nil {
		t.Fatalf("record the plan of %s defining %s: %v", issue, role.Role, err)
	}
}

// A read of a claim's plan that the server fails inside the state's snapshot ends the state's read
// with that read's own error. The server's error has already ended the snapshot's transaction, so
// a count that took the failure for an answer would go on to the next read of the snapshot, which
// fails with "current transaction is aborted" and names nothing of the cause. The plan's table is
// renamed away in a database of the test's own, so the read fails at the server as a store fault
// does, through the daemon's own reader (stateNoPluginInstall).
//
// The control is the rule before this, run by hand: a failed read answered that no install moves
// the claim. The state's read then ends on the project's pause, with the aborted transaction's
// error (25P02) in place of the plan read's (42P01).
func TestAFailedPlanReadEndsTheStatesReadWithItsOwnError(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.Runtime.Name = "kubernetes"
	cfg.Roles = []config.Role{{Name: "prober", Type: "testing", GitHubApp: config.RoleAppReview}}
	cfg.WorkerBackends = map[claim.Role]runtime.Backend{"prober": runtime.BackendClaude}
	issue := cfg.Project + "-1"
	c := previousClaim(t, cfg, issue, issue, "prober", supervise.StateIdle)
	c.PluginContract = api.DaemonAPIVersion() - 1
	putPreviousRun(t, cfg, previousRun{issues: []record.Issue{runningRoot(issue, phase.Implementing)}, claims: []supervise.Claim{c}})

	ctx := store.WithTransactionTracking(context.Background())
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Pool().Exec(context.Background(), `alter table plan_roles rename to plan_roles_away`); err != nil {
		t.Fatalf("rename the plan's roles away: %v", err)
	}
	view := refreshCountSource(t, cfg, st)
	view.noPluginInstall = stateNoPluginInstall(cfg)
	err = store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := view.State(ctx, tx)
		return err
	})
	// 42P01 is undefined_table, the server's own error for the renamed table.
	var failed *pgconn.PgError
	if !errors.As(err, &failed) || failed.Code != "42P01" {
		t.Fatalf("State = %v, want the plan read's own error, undefined_table (42P01)", err)
	}
	if want := "the role prober of the plan of " + issue; !strings.Contains(err.Error(), want) {
		t.Errorf("State = %v, want it to name the read (%q)", err, want)
	}
}

// A daemon that knows no plugin source to relaunch a stale process onto (PluginRelaunchable
// false, LEGION-164's degraded Kubernetes mode) counts those claims all the same: the lane
// posts nothing for them and writes one Error line each at boot, and the count is what that gap
// costs, standing at the claims the lane cannot act on beside those lines. A count zeroed there
// would read as a lane with nothing left, which is the one thing such a daemon is not
// (dispatch://INFRA-2712 R4). Driven through the supervision option, as
// TestAKubernetesDaemonWithNoPluginSourceRelaunchesNothing is: a real Kubernetes runtime needs a
// cluster this pod has none of.
func TestTheStateCountsWhatADegradedDaemonCannotRefresh(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	no := false
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.pluginRelaunchable = &no
	logs := &syncBuffer{}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateReady, Session: "ses_impl", SessionFile: "/sessions/impl.jsonl",
		PluginContract: 1, Locator: pluginTestLocator(token),
	})

	d := startDaemonLogging(t, cfg, o, jsonLogger(logs))

	testwait.Eventually(t, "the line naming the claim this daemon cannot refresh", func() bool {
		return len(loggedLines(t, logs.String(), supervise.NoPluginSourceMessage)) == 1
	})
	state := bootState(t, d)
	var count int
	if err := json.Unmarshal(state[api.ClaimsToRefreshField], &count); err != nil {
		t.Fatalf("decode %s %s: %v", api.ClaimsToRefreshField, state[api.ClaimsToRefreshField], err)
	}
	if count != 1 {
		t.Fatalf("%s = %d on a daemon that can relaunch onto no plugin, want the 1 claim it cannot act on", api.ClaimsToRefreshField, count)
	}
	if c := d.claim(token); c.Generation != 1 || c.State != string(supervise.StateReady) {
		t.Fatalf("the stale claim = %+v, want untouched: the count is of claims the lane has left, not of relaunches made", c)
	}
}
