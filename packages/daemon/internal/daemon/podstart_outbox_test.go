package daemon

// One session load at a time in an issue pod (dispatch://INFRA-5297), as the daemon's own paths
// meet it: a claim whose start waits at its issue pod's gate while another role of the issue loads
// is stopped by the outbox's suspend, release, issue_close and tree_close rows and by a pause's
// stop, each of which reads the claim before it stops it, and by a stop row queued behind the
// waiting start on the issue's lifecycle lane. A suspension, a release or a deletion of the pod
// never waits: each acts within podStopBound of real time while the other role still loads, and
// the waiting role's process is never asked of the runtime. supervise's own
// TestAPodStartsItsRolesOneLoadAtATime holds the gate itself.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// podStopBound is how much real time a stop of a claim waiting at its pod's gate may take, from
// the row's run to its act: a second, where a wait for the other role's load would hold it for as
// long as that load runs, which on a clock whose timers never fire (stillClock) is for ever.
const podStopBound = time.Second

// cutShortLine is the line a launch writes when something takes its start out of the pod's gate
// before it ran (supervise's podPark).
const cutShortLine = "supervise: launch cut short while it waited for its issue pod's start"

// podGated has sup's machines start their processes through one pod start gate, as a Kubernetes
// daemon's do (supervise.Deps.PodStarts, podStarts), on sup's clock, whose timers never fire
// (stillClock): a load the gate holds ends neither at its settle nor at its bound while the test
// runs. It answers the JSON log the machines write, each line naming its claim. It is set before
// any machine is made, since a machine takes the deps it is made with.
func podGated(sup *supervisor) *testlog.Buffer {
	logs := &testlog.Buffer{}
	sup.deps.PodStarts = supervise.NewPodStarts(sup.deps.Clock)
	sup.deps.Log = slog.New(slog.NewJSONHandler(logs, nil))
	return logs
}

// podPair is LEGION-208's implementer, whose process the gate let start and whose session load
// holds the pod for as long as the test runs, and its tester, suspended by the daemon over a
// session of its own, whose launch a test makes wait at the gate behind that load.
type podPair struct {
	loading, waiting           *supervise.Machine
	loadingToken, waitingToken claim.Token
}

// loadingPair makes the pair's two claims in sup and starts the implementer's process.
func loadingPair(t *testing.T, sup *supervisor) podPair {
	t.Helper()
	ctx := context.Background()
	p := podPair{loadingToken: mustClaimToken(t, pauseIssue, claim.RoleImplementer), waitingToken: mustClaimToken(t, pauseIssue, claim.RoleTester)}
	var err error
	p.loading, _, err = sup.Create(ctx, supervise.Claim{Token: p.loadingToken, Project: "legion", Tree: pauseIssue, Issue: pauseIssue,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the implementer: %v", err)
	}
	if err := p.loading.Handle(ctx, supervise.RequestSpawn{Claim: p.loadingToken}); err != nil {
		t.Fatalf("start the implementer: %v", err)
	}
	if c := p.loading.View().Claim; c.State != supervise.StateLaunching || c.Locator == nil {
		t.Fatalf("the implementer is %s recording %+v, want its process started and loading", c.State, c.Locator)
	}
	p.waiting, _, err = sup.Create(ctx, supervise.Claim{Token: p.waitingToken, Project: "legion", Tree: pauseIssue, Issue: pauseIssue,
		Role: claim.RoleTester, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
		Session: "ses-tester", SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the tester: %v", err)
	}
	return p
}

// parkedBehindALoad is loadingPair with the tester's resume by rule asked of its machine on a
// goroutine of its own, as the outbox's start asks it: the tester's launch then waits at the
// gate behind the implementer's load, its claim written launching and its process asked of no
// runtime.
func parkedBehindALoad(t *testing.T, sup *supervisor, rt *fake.Runtime) podPair {
	t.Helper()
	p := loadingPair(t, sup)
	go func() {
		_ = p.waiting.Handle(context.Background(), supervise.RequestRuleResume{Claim: p.waitingToken, Reason: "its issue's phase starts"})
	}()
	testwait.Eventually(t, "the tester's launch to wait at its pod's gate", func() bool {
		return p.waiting.View().Claim.State == supervise.StateLaunching
	})
	if launched := launchesOf(rt, p.waitingToken); len(launched) != 0 {
		t.Fatalf("the tester was launched %d times, want its launch waiting behind the implementer's load", len(launched))
	}
	return p
}

// wantStopped is the tester after its stop: in state, suspended by by where state is suspended,
// its process never asked of the runtime and its start taken out of the gate, which its launch
// writes once; and the implementer still loading, on the process the gate let start.
func (p podPair) wantStopped(t *testing.T, rt *fake.Runtime, logs *testlog.Buffer, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	if c := p.waiting.View().Claim; c.State != state || state == supervise.StateSuspended && c.SuspendedBy != by {
		t.Fatalf("the tester is %s by %s after its stop, want %s by %s", c.State, c.SuspendedBy, state, by)
	}
	if launched := launchesOf(rt, p.waitingToken); len(launched) != 0 {
		t.Fatalf("the tester was launched %d times, want none: its process is never asked of the runtime", len(launched))
	}
	cut := 0
	for _, line := range loggedMessages(t, logs.String(), cutShortLine) {
		if line["claim"] == string(p.waitingToken) {
			cut++
		}
	}
	if cut != 1 {
		t.Fatalf("the tester's lines that its launch was cut short at the gate = %d, want 1; log:\n%s", cut, logs.String())
	}
	if c := p.loading.View().Claim; c.State != supervise.StateLaunching || c.Locator == nil {
		t.Fatalf("the implementer is %s recording %+v after the tester's stop, want it still loading", c.State, c.Locator)
	}
	if launched := launchesOf(rt, p.loadingToken); len(launched) != 1 {
		t.Fatalf("the implementer was launched %d times, want the one launch whose load holds the pod", len(launched))
	}
}

// actsPromptly is the answer on done, which must come within podStopBound of real time.
func actsPromptly(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	timer := time.NewTimer(podStopBound)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Fatalf("%s did not act within %s while another role of its pod loads: it waits on that load", what, podStopBound)
	}
	return nil
}

// runsPromptly is row run through runner's executor (outbox.execute) on a goroutine of its own,
// answered within podStopBound (actsPromptly).
func runsPromptly(t *testing.T, runner *outbox, what string, row record.OutboxRow) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- runner.execute(context.Background(), row) }()
	return actsPromptly(t, what, done)
}

// podStopRig is LEGION-208, recorded as issue, on a database of its own with its admission slot
// held, the outbox runner its stop rows run through, the runtime, and the log of its machines,
// whose starts go through one pod start gate (podGated). tree is whether its tree's lifecycle is
// opened, as admission opens it: a tree close of a tree with none reserves no cleanup and is
// decided on the claim alone (reservedTreeClose), which is the close that reads the claim.
type podStopRig struct {
	pool   *store.Pool
	st     *store.Store
	sup    *supervisor
	runner *outbox
	rt     *fake.Runtime
	logs   *testlog.Buffer
}

func newPodStopRig(t *testing.T, issue record.Issue, tree bool) *podStopRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, issue)
	holdSlot(t, pool, issue.Key)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	logs := podGated(sup)
	var roots []string
	if tree {
		roots = append(roots, issue.Tree)
	}
	st := outboxTreeStore(t, pool, roots...)
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{},
		project: "legion", stateDir: t.TempDir(), notices: &outboxPublisher{}, now: time.Now, log: quietLogger()}
	return &podStopRig{pool: pool, st: st, sup: sup, runner: runner, rt: rt, logs: logs}
}

// podIssueAt is LEGION-208 in implementing at generation 1, the implementer the open phase's
// worker.
func podIssueAt() record.Issue {
	return record.Issue{Key: pauseIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: pauseIssue, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
}

// TestAPodStartsItsRolesOneLoadAtATime is the daemon's half of the check of the same name
// (dispatch://INFRA-5297 c-one-load-at-a-time): every stop the daemon makes of a claim waiting at
// its pod's gate acts within a second while another role of the pod loads, through the executor
// that makes it.
func TestAPodStartsItsRolesOneLoadAtATime(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lingers is whether the issue's tree lingers at generation 1, which a tree close is for.
		lingers bool
		// row is the stop row of the tester, made on the rig.
		row   func(t *testing.T, r *podStopRig) record.OutboxRow
		state supervise.ClaimState
		by    supervise.SuspendSource
	}{
		{name: "the outbox's suspend row", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon,
			row: func(t *testing.T, _ *podStopRig) record.OutboxRow {
				return mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "suspend", Tree: pauseIssue, Role: claim.RoleTester,
					Generation: 1, Reason: "the tree was parked"}, time.Now())
			}},
		{name: "the outbox's release row", state: supervise.StateSuspended, by: supervise.SuspendedByReleased,
			row: func(t *testing.T, r *podStopRig) record.OutboxRow {
				// The idle release of the pod, marked with its rows as the idle tick marks it
				// (idleReleaseRows): the tester's role stop is its boundary.
				marked, err := r.st.ReleaseIdleIssue(context.Background(), "legion", pauseIssue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
					return idleReleaseRows(view, workflow.IdleDecision{Release: true, Roles: []claim.Role{claim.RoleTester}}, time.Now())
				})
				if err != nil || !marked {
					t.Fatalf("mark the release of %s = %t, %v", pauseIssue, marked, err)
				}
				row := record.OutboxRow{Kind: record.OutboxKindSupervise}
				if err := r.pool.QueryRow(context.Background(), `select id, issue, payload from outbox where kind = 'supervise'
					and payload->>'op' = 'release'`).Scan(&row.ID, &row.Issue, &row.Payload); err != nil {
					t.Fatalf("read the release's role stop: %v", err)
				}
				return row
			}},
		{name: "the outbox's issue_close row", state: supervise.StateRetired,
			row: func(t *testing.T, _ *podStopRig) record.OutboxRow {
				return mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "issue_close", Tree: pauseIssue, Role: claim.RoleTester,
					Generation: 1}, time.Now())
			}},
		{name: "the outbox's tree_close row", lingers: true, state: supervise.StateRetired,
			row: func(t *testing.T, _ *podStopRig) record.OutboxRow {
				return mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "tree_close", Tree: pauseIssue, Role: claim.RoleTester,
					Generation: 1, Linger: 1}, time.Now())
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := podIssueAt()
			if tc.lingers {
				until := time.Now().Add(24 * time.Hour)
				issue.Phase, issue.Status, issue.LingerUntil = phase.Done, "done", &until
			}
			r := newPodStopRig(t, issue, !tc.lingers)
			pair := parkedBehindALoad(t, r.sup, r.rt)

			if err := runsPromptly(t, r.runner, tc.name, tc.row(t, r)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			pair.wantStopped(t, r.rt, r.logs, tc.state, tc.by)
		})
	}

	t.Run("a pause's stop", func(t *testing.T) {
		p := newPauseRig(t)
		sup, rt := p.supervisor()
		sup.deps.PauseStopped = p.pauses.stopped
		p.pauses.supervisor = sup
		logs := podGated(sup)
		// The tester's launch waits at the gate before the pause: a launch asked while the project
		// is paused is held by the pause's own gate instead (supervise's pauseGate).
		pair := parkedBehindALoad(t, sup, rt)
		held := p.pauseLegion()
		runner := &outbox{pool: p.pool, records: record.NewStore(), supervisor: sup, trees: p.st, project: "legion",
			dispatchProject: "LEGION", log: quietLogger(), now: time.Now}

		if err := runsPromptly(t, runner, "the pause's stop", mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "pause_stop",
			Tree: pauseIssue, Role: claim.RoleTester, Generation: 1, Pause: held.ID, Reason: "project LEGION is paused"}, time.Now())); err != nil {
			t.Fatalf("the pause's stop: %v", err)
		}
		pair.wantStopped(t, rt, logs, supervise.StateSuspended, supervise.SuspendedByReleased)
		if got := p.stops(held.ID, claim.RoleTester); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
			t.Fatalf("the tester's stops = %+v, want one stopped and owed the launch it waited for", got)
		}
	})

	t.Run("a stop row behind a start row whose launch waits, on the issue's lifecycle lane", func(t *testing.T) {
		r := newPodStopRig(t, podIssueAt(), true)
		pair := loadingPair(t, r.sup)
		// The tester's start, then its stop: two rows of the issue's lifecycle lane, which runs them
		// one at a time in the order they were queued.
		for _, row := range []record.OutboxRow{
			mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "start", Tree: pauseIssue, Role: claim.RoleTester, Generation: 1}, time.Now()),
			mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "suspend", Tree: pauseIssue, Role: claim.RoleTester, Generation: 1,
				Reason: "the tree was parked"}, time.Now()),
		} {
			enqueueOutbox(t, r.pool, r.runner.records, row)
		}
		drained := make(chan bool, 1)
		go func() {
			done, _ := r.runner.drain(context.Background(), pauseIssue, record.LaneKinds(record.LaneLifecycle))
			drained <- done
		}()

		// The tester starts suspended by the daemon at generation 1, which a stop leaves it in as
		// well: what says the start row launched it is the launch's generation, which the try
		// wrote before it waited at the gate (supervise's launchTries).
		launched := func() bool { return pair.waiting.View().Claim.Generation > 1 }
		testwait.Eventually(t, "the start row to launch the tester", launched)
		testwait.WithinPolling(t, podStopBound, testwait.Poll, "the suspend row behind the start row to act within a second", func() bool {
			return pair.waiting.View().Claim.State == supervise.StateSuspended
		})
		if done := testwait.Receive(t, "the lane's drain", drained); !done {
			t.Fatal("the lane did not drain its rows")
		}
		if left := unfinishedSuperviseRows(t, r.pool); left != 0 {
			t.Fatalf("unfinished supervise rows = %d, want the start and the stop both finished", left)
		}
		pair.wantStopped(t, r.rt, r.logs, supervise.StateSuspended, supervise.SuspendedByDaemon)
	})
}
