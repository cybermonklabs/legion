package workflow

// A merger the design gate holds is not woken by its backstop while the gate is closed
// (dispatch://INFRA-2040 R1, D13 of its plan). podBackstopDue reads the hold in the fact's own
// transaction before it fires the row, and while the hold stands it writes nothing and leaves the
// fact unspent; the wake tick's filter (HeldMergers) reads the same rule before any fact. These
// cases apply the facts through the engine over a real PostgreSQL, as wake_test.go's rig does.

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// heldMergerSpec is the spec document of the held merger's tree.
const heldMergerSpec = "artifact-208"

// newHeldMergerWakeRig is the wake rig's pod in merging: the root LEGION-208 at generation 7, its
// pull request at head1, its merger's claim released, and the root's spec at version 2 with
// version 1 approved, under a deployment that gates the spec. So the design gate holds the
// released merger.
func newHeldMergerWakeRig(t *testing.T) wakeRig {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: wakeIssueGeneration,
		Status: PhaseStatus(phase.Merging), Rank: "U", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}})
	seedReleasedClaim(t, pool, claim.RoleMerger, "session-merger")
	approved := 1
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: heldMergerSpec,
		LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
	return wakeRigOf(t, pool, config.DesignGateRootIssues.Steps())
}

// wakeRigOf is the wake rig's engine over pool, as a daemon booted on a deployment that gates
// steps builds it.
func wakeRigOf(t *testing.T, pool *pgxpool.Pool, steps []record.DesignStep) wakeRig {
	t.Helper()
	logged := &testlog.Buffer{}
	clock := &checkClock{now: wakeAt}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: steps, Pods: wakePods(t, pool), Linger: time.Hour,
		ProductionCheckRole: claim.RoleTester, Clock: clock.Now}, slog.New(slog.NewTextHandler(logged, nil)))
	return wakeRig{pool: pool, engine: engine, logged: logged, apply: applyFacts(t, pool, engine)}
}

// backstopUnfired is whether backstop id is still armed and not fired.
func backstopUnfired(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var unfired bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from pod_wake_backstops where id = $1 and not fired)`,
		id).Scan(&unfired); err != nil {
		t.Fatalf("read backstop %d: %v", id, err)
	}
	return unfired
}

// factSpent is whether the fact applyFacts applied under event is marked applied.
func factSpent(t *testing.T, pool *pgxpool.Pool, event string) bool {
	t.Helper()
	var spent bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from processed_events where source = 'api' and event_id = $1)`,
		event).Scan(&spent); err != nil {
		t.Fatalf("read the mark of %s: %v", event, err)
	}
	return spent
}

// A backstop that comes due for a held merger whose pod was released is not taken while the gate
// is closed (R1). The fact reads the hold in its own transaction, so two ticks that apply it — the
// second as a tick whose filter read the gate open before its closing committed would — each write
// no start and no resume, leave the row armed and unfired, and leave the event unspent, so the
// same id applies again at a later tick. A CI settlement of the merger's head then wakes the
// merger: the backstop left no open resume, so pod_resumes_one_open_per_role refuses nothing, and
// the wake cancels the backstop as every wake does (TakePodWake).
func TestAHeldMergersBackstopIsLeftDueAndACIWakeAfterItIsTaken(t *testing.T) {
	rig := newHeldMergerWakeRig(t)
	id := armBackstop(t, rig.pool, claim.RoleMerger, wakeAt.Add(-time.Minute))
	event := fmt.Sprintf("pod-backstop:%d", id)
	fact := intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleMerger}

	for _, tick := range []string{"the first tick", "a later tick"} {
		if result := rig.apply(event, fact); !result.Unspent || result.Duplicate || result.Refusal != nil {
			t.Fatalf("the held merger's backstop at %s = %+v, want it left unspent: the gate holds the merger", tick, result)
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
			t.Fatalf("supervise start rows after %s = %+v, want none: the held merger gets no backstop turn", tick, starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes after %s = %v, want none: a backstop left due records no resume", tick, resumes)
		}
		if !backstopUnfired(t, rig.pool, id) {
			t.Fatalf("the backstop after %s is fired or gone, want it armed and unfired", tick)
		}
		if factSpent(t, rig.pool, event) {
			t.Fatalf("the backstop's event %s is marked applied after %s, want it unspent so a later tick applies it again", event, tick)
		}
	}

	rig.apply("settled", settlement("head1"))
	oneWake(t, rig.pool, claim.RoleMerger, pods.WakeChecks, "head1")
	var left int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) from pod_wake_backstops where id = $1`, id).Scan(&left); err != nil {
		t.Fatalf("read backstop %d: %v", id, err)
	}
	if left != 0 {
		t.Fatalf("the merger's backstop is still armed after the CI wake, want the wake to have cancelled it")
	}
}

// After a boot that empties gates.design, which writes no gate row, the first tick that reads the
// gate open takes the backstop the hold left due (R1): the same event id the held tick left unspent
// applies, fires the row and wakes the merger with the backstop's task. It is the merger's one wake
// of that opening.
func TestAHeldMergersBackstopIsTakenOnceABootEmptiesTheGatedSteps(t *testing.T) {
	rig := newHeldMergerWakeRig(t)
	id := armBackstop(t, rig.pool, claim.RoleMerger, wakeAt.Add(-time.Minute))
	event := fmt.Sprintf("pod-backstop:%d", id)
	fact := intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleMerger}
	if result := rig.apply(event, fact); !result.Unspent {
		t.Fatalf("the held merger's backstop = %+v, want it left unspent while the gate holds the merger", result)
	}

	booted := wakeRigOf(t, rig.pool, nil)
	if result := booted.apply(event, fact); result.Unspent || result.Duplicate || result.Refusal != nil {
		t.Fatalf("the backstop after the boot = %+v, want it applied: no step is gated", result)
	}
	oneWake(t, rig.pool, claim.RoleMerger, pods.WakeBackstop, "backstop")
	if backstopUnfired(t, rig.pool, id) {
		t.Fatal("the backstop is still unfired after the boot's tick took it")
	}
}

// The wake tick's filter reads the fact's own rule (HeldMergers): it answers the issue whose merger
// the gate holds, with the closed gate's row, and nothing for a member of the same tree in another
// phase, for an issue the record does not hold, or for any issue under a deployment that gates no
// step.
func TestTheBackstopFilterAnswersTheIssuesWhoseMergerTheDesignGateHolds(t *testing.T) {
	rig := newHeldMergerWakeRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1,
		Status: PhaseStatus(phase.Implementing), Rank: "U", LastDispatchSeq: 1})
	read := func(engine *Engine) map[string]record.DesignGate {
		t.Helper()
		var held map[string]record.DesignGate
		if err := pgx.BeginTxFunc(context.Background(), rig.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			held, err = engine.HeldMergers(context.Background(), tx, []string{"LEGION-208", "LEGION-209", "LEGION-404"})
			return err
		}); err != nil {
			t.Fatalf("read the held mergers: %v", err)
		}
		return held
	}

	held := read(rig.engine)
	if gate, holds := held["LEGION-208"]; len(held) != 1 || !holds || gate.ArtifactID != heldMergerSpec || gate.LatestVersion != 2 {
		t.Fatalf("the held mergers = %+v, want LEGION-208's alone, held by version 2 of %s", held, heldMergerSpec)
	}
	if held := read(wakeRigOf(t, rig.pool, nil).engine); len(held) != 0 {
		t.Fatalf("the held mergers under a deployment that gates no step = %+v, want none", held)
	}
}
