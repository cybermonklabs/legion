package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
		deployment: func() capabilities.Deployment { return capabilities.Deployment{} },
		reports:    func() map[claim.Token]capabilities.Report { return nil },
		noPluginInstall: func(c supervise.Claim) bool {
			return c.Role == claim.RoleReviewer
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
// capability, whose recorded contract is not this daemon's, in no update round, and whose launch
// installs a plugin.
//
// Each claim the count leaves out is here for a reason of its own, because the lane leaves it
// out for a reason of its own: a claim already at this contract has nothing to refresh; a
// suspended one has no process to replace, and the one its next launch starts speaks this
// contract by itself; a claim in a plugin round and one in a pod round are passed over by the
// pass; a claim whose launch installs no plugin can never be refreshed by a relaunch at all; and
// a claim with no recorded session has no process the lane could read a contract from.
//
// The count is read a second time with every claim brought onto this contract, because 0 is the
// answer an operator reads a drained wave by and a count that can only rise says nothing.
func TestTheStateCountsTheClaimsTheStalePluginLaneHasLeftToRefresh(t *testing.T) {
	cfg := testConfig(t)
	stale := api.DaemonAPIVersion - 1

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
			shape: func(c *supervise.Claim) { c.PluginContract = api.DaemonAPIVersion }},
		{issue: cfg.Project + "-6", role: claim.RoleImplementer, state: supervise.StateSuspended},
		{issue: cfg.Project + "-7", role: claim.RoleImplementer, state: supervise.StateIdle,
			shape: func(c *supervise.Claim) { c.UpdateRound = supervise.PluginRound(api.DaemonAPIVersion) }},
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
		c.PluginContract = api.DaemonAPIVersion
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("bring %s onto this daemon's contract: %v", c.Token, err)
		}
	}
	if got := refreshCountState(t, cfg, st, view).ClaimsToRefresh; got != 0 {
		t.Fatalf("claimsToRefresh = %d once every claim records this contract, want 0", got)
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
