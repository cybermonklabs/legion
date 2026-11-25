package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// applyStates applies one check.state_changed event of issue at head of repo for each of states,
// the first at serial and each next one at the serial after, through intake.ApplyFact and engine,
// as the live consumer applies a fact (checkstatetest.Apply), never a write to the store.
func applyStates(t *testing.T, pool *pgxpool.Pool, engine *Engine, issue, repo, head string, serial int64, states ...intake.DispatchCheckState) {
	t.Helper()
	checkstatetest.Apply(t, pool, issue, repo, head, serial, states, engine, admissionStub{})
}

// testedAs applies, ahead of a tester's completion of issue's testing that names no serial, the
// state event that decides that completion's move (testedFact), through intake.ApplyFact and
// engine.
func testedAs(t *testing.T, pool *pgxpool.Pool, engine *Engine, issue, head string, passes bool) {
	t.Helper()
	fact, id := testedFact(t, pool, issue, head, passes)
	if result, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("apply the state event %s: %+v, %v", id, result.Refusal, err)
	}
}

// testedFact is the state event that decides a tester's completion of issue's testing, decoded,
// and its event id (checkstatetest.Tested): k1, the head's one check, yes from the tester where
// passes, and no for a fail where not, at head, on statesRepo where issue has no pull request. A
// test that applies its facts through a function of its own applies this one there too.
func testedFact(t *testing.T, pool *pgxpool.Pool, issue, head string, passes bool) (intake.DispatchCheckState, string) {
	t.Helper()
	return checkstatetest.Tested(t, pool, issue, statesRepo, head, passes)
}

// stateOf is check's state with reason, from roles.
func stateOf(check, state, reason string, roles ...string) intake.DispatchCheckState {
	return intake.DispatchCheckState{Check: check, State: state, Reason: reason, Roles: roles}
}

// passOf is check's state yes, from roles.
func passOf(check string, roles ...string) intake.DispatchCheckState {
	return stateOf(check, "yes", "", roles...)
}

// ofSet is state carrying checks as its head's set.
func ofSet(state intake.DispatchCheckState, checks ...string) intake.DispatchCheckState {
	state.Checks = checks
	return state
}

// inCI is state of a check CI proves, with its run's case.
func inCI(state intake.DispatchCheckState, ciCase string) intake.DispatchCheckState {
	state.CICase = ciCase
	return state
}

// statesRepo is the repository LEGION-208's pull request #42 is on.
const statesRepo = "sjawhar/legion"

// statesRig is LEGION-208 at a phase, with its pull request #42 open at headOne and the tester's
// claim on its row, and the engine every fact of the test applies through.
type statesRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
}

func newStatesRig(t *testing.T, at phase.Phase) *statesRig {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: statesRepo, DefaultBranch: "main", GitHubRepo: statesRepo, Tree: "LEGION-208", Project: "LEGION",
		Title: "root", Phase: at, Generation: 1, Status: PhaseStatus(at), Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: headOne, CheckedHead: headOne, Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	return &statesRig{t: t, pool: pool, engine: testEngine(config.DesignGateRootIssues, nil)}
}

// states applies states of LEGION-208 at head from serial on (applyStates).
func (r *statesRig) states(head string, serial int64, states ...intake.DispatchCheckState) {
	r.t.Helper()
	applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, head, serial, states...)
}

// complete is the tester's completion of testing at head, its test record carrying serial as the
// serial of the check states it was written after, and no verdict.
func (r *statesRig) complete(head string, serial int64) {
	r.t.Helper()
	result, err := intake.ApplyFact(context.Background(), r.pool, "api", fmt.Sprintf("test-%s-%d", head, serial), intake.HandoffComplete{
		Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "Every check passes.",
		Commit: head, HandoffRecord: "test-1", HandoffVersion: 1, StatesSerial: serial,
	}, r.engine, admissionStub{})
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply the tester's completion: %+v, %v", result.Refusal, err)
	}
}

// held is what the store holds of LEGION-208's checks at head.
func (r *statesRig) held(head string) record.HeadStates {
	r.t.Helper()
	var held record.HeadStates
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error {
		var err error
		held, err = record.NewStore().HeadStates(context.Background(), tx, "LEGION-208", statesRepo, head)
		return err
	})
	return held
}

// finished is every phase-finished notice of LEGION-208, in the order written, each as its
// stored payload.
func (r *statesRig) finished() []map[string]any {
	r.t.Helper()
	var notices []map[string]any
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", "notice") {
		var notice map[string]any
		if err := json.Unmarshal(row.payload, &notice); err != nil {
			r.t.Fatalf("decode notice %s: %v", row.payload, err)
		}
		if notice["kind"] == "phase-finished" {
			notices = append(notices, notice)
		}
	}
	return notices
}

// finishedNotices is every phase-finished notice of LEGION-208, in the order written, each as the
// architect receives it: its stored payload decoded as a notice.
func (r *statesRig) finishedNotices() []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", "notice") {
		var notice record.Notice
		if err := json.Unmarshal(row.payload, &notice); err != nil {
			r.t.Fatalf("decode notice %s: %v", row.payload, err)
		}
		if notice.Kind == "phase-finished" {
			notices = append(notices, notice)
		}
	}
	return notices
}

// failingOf is check not passing at the tested head, in state for reason, as the move judges it.
func failingOf(check, state, reason string) record.FailingCheck {
	return record.FailingCheck{Check: check, State: state, Reason: reason}
}

// starts is the number of worker starts LEGION-208's outbox holds.
func (r *statesRig) starts() int {
	r.t.Helper()
	var starts int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'`).Scan(&starts); err != nil {
		r.t.Fatalf("count the starts: %v", err)
	}
	return starts
}

// roleStarts is the number of starts of role the outbox holds.
func roleStarts(t *testing.T, pool *pgxpool.Pool, role claim.Role) int {
	t.Helper()
	var starts int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = $1`, string(role)).Scan(&starts); err != nil {
		t.Fatalf("count the starts of the %s: %v", role, err)
	}
	return starts
}

// waitsFor fails unless LEGION-208 is still in testing, with no worker started and one
// phase-finished notice of the tester, as the architect receives it: the tester's summary, the
// tested head head, what the move waits for (waitsFor) and the checks it waits for (checks), and
// no failing check. It reads no reason: the notice carries the wait as fields.
func (r *statesRig) waitsFor(head, waitsFor string, checks ...string) {
	r.t.Helper()
	assertPhase(r.t, r.pool, phase.Testing)
	if starts := r.starts(); starts != 0 {
		r.t.Fatalf("the waiting move started %d workers, want none", starts)
	}
	notices := r.finishedNotices()
	if len(notices) != 1 || notices[0].Role != claim.RoleTester || notices[0].Phase != phase.Testing || notices[0].Summary != "Every check passes." ||
		notices[0].Head != head || notices[0].WaitsFor != waitsFor || !slices.Equal(notices[0].Waiting, checks) || len(notices[0].Failing) != 0 {
		r.t.Fatalf("phase-finished notices = %+v, want one of the tester's testing, with its summary, the head %q, waits_for %q and waiting %v", notices, head, waitsFor, checks)
	}
}

// movedBackFailing fails unless LEGION-208 went back to implementing with one start of the
// implementer, and the tester's one phase-finished notice of the move back, the last notice
// written, carries the tested head head and failing, compared whole: each check that does not
// pass there, with its state and reason. The implementer's task must carry the reason that notice
// gives, read from the notice and not written here, since the move writes the two from one value;
// no word of either is compared with a literal.
func (r *statesRig) movedBackFailing(head string, failing ...record.FailingCheck) {
	r.t.Helper()
	assertPhase(r.t, r.pool, phase.Implementing)
	if starts := roleStarts(r.t, r.pool, claim.RoleImplementer); starts != 1 {
		r.t.Fatalf("the move back started the implementer %d times, want once", starts)
	}
	notices := r.finishedNotices()
	var back []record.Notice
	for _, notice := range notices {
		if notice.WaitsFor == "" {
			back = append(back, notice)
		}
	}
	if len(back) != 1 || notices[len(notices)-1].WaitsFor != "" {
		r.t.Fatalf("phase-finished notices = %+v, want the move back's one notice, written last", notices)
	}
	last := back[0]
	if last.Role != claim.RoleTester || last.Phase != phase.Testing || last.Head != head || !slices.Equal(last.Failing, failing) || len(last.Waiting) != 0 {
		r.t.Fatalf("the tester's phase-finished notice of the move back = %+v, want the head %q and failing %+v", last, head, failing)
	}
	if task := startTask(r.t, r.pool, claim.RoleImplementer); last.Reason == "" || !strings.Contains(task, last.Reason) {
		r.t.Fatalf("the implementer's task %q does not carry the reason %q the notice of the move back gives", task, last.Reason)
	}
}
