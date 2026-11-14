package workflow

// The silence handlers and the sweep's own filter leave alone a phase worker the record gives a
// reason of the daemon's own to take no turn (dispatch://INFRA-2040 R1 and R3): a merger the closed
// design gate holds, and a worker whose wait stands. Each case stands on the wait rig of
// wait_ends_test.go, whose facts run through the engine, admission and the wait reconciler as the
// daemon runs them, so a gate's closing and opening and a wait's end are the facts that make them.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// quietStep is the moment every quiet claim below last stepped, two hours before the rig's now
// (waitEndsAt) and so past silenceBound; quietReport is when a quiet claim's session reported
// background work of its own, five hours before it and so past backgroundBound.
var (
	quietStep   = waitEndsAt.Add(-2 * time.Hour)
	quietReport = waitEndsAt.Add(-5 * time.Hour)
)

// claimed records the claim of issue's role as the rig's waiting does, live in session
// ses-<role> with its phases row on entry 2, and declares no wait. It answers the claim's token.
func (r *waitRig) claimed(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token := waitToken(r.t, issue, role)
	if _, err := r.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
		session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $3, $4, 1, $5, '', 'working', 0, 0, 0, 0)`,
		string(token), waitRoot, issue, string(role), "ses-"+string(role)); err != nil {
		r.t.Fatalf("seed the claim of %s's %s: %v", issue, role, err)
	}
	seedPhase(r.t, r.pool, record.PhaseRow{Issue: issue, Role: role, Claim: token, Entry: 2})
	return token
}

// goesQuiet puts token's claim up between turns in ready, holding no task, its agent's last step
// at quietStep: every term of a silence but the one each case is about.
func (r *waitRig) goesQuiet(token claim.Token) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = 'ready', step_at = $2 where token = $1`,
		string(token), quietStep); err != nil {
		r.t.Fatalf("put the claim %s between turns: %v", token, err)
	}
}

// reportsBackground records token's session reporting, at quietReport, background work of its
// own running, or none when running is false.
func (r *waitRig) reportsBackground(token claim.Token, running bool) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set activity_at = $2, activity_background = $3,
		activity_kinds = 'async-job' where token = $1`, string(token), quietReport, running); err != nil {
		r.t.Fatalf("record the activity report of %s: %v", token, err)
	}
}

// silenceOf is the worker-silent fact the sweep mints for issue's role, and its id.
func silenceOf(issue string, role claim.Role) (intake.WorkerSilent, string) {
	return intake.WorkerSilent{Issue: issue, Role: role, Generation: 1, LastStep: quietStep, Bound: silenceBound},
		fmt.Sprintf("worker-silent:%s:%s:%s", issue, role, quietStep.Format(time.RFC3339Nano))
}

// backgroundOf is the worker-background-stalled fact the sweep mints for issue's role, and its id.
func backgroundOf(issue string, role claim.Role) (intake.WorkerBackgroundStalled, string) {
	return intake.WorkerBackgroundStalled{Issue: issue, Role: role, Generation: 1, ReportedAt: quietReport,
			Kinds: []string{"async-job"}, Bound: backgroundBound},
		fmt.Sprintf("worker-background:%s:%s:%s", issue, role, quietReport.Format(time.RFC3339Nano))
}

// starts is how many supervise rows of issue the outbox holds.
func (r *waitRig) starts(issue string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and issue = $1`,
		issue).Scan(&n); err != nil {
		r.t.Fatalf("count the supervise rows of %s: %v", issue, err)
	}
	return n
}

// applied is whether the supervise source's event id is marked applied.
func (r *waitRig) applied(id string) bool {
	r.t.Helper()
	applied, err := intake.Applied(context.Background(), r.pool, "supervise", []string{id})
	if err != nil {
		r.t.Fatalf("read whether %s is applied: %v", id, err)
	}
	return applied[id]
}

// assertLeftAlone asserts the silence fact id of issue's role, applied as result, wrote nothing —
// no supervise row beyond the before the case counted and no notice of kind — and was left
// unspent, so the same id applies again once the reason for the quiet is over.
func (r *waitRig) assertLeftAlone(issue string, kind record.NoticeKind, before int, id string, result intake.Result) {
	r.t.Helper()
	if got := r.starts(issue); got != before {
		r.t.Fatalf("the supervise rows of %s = %d after %s, want the %d before it: the daemon sends this worker nothing", issue, got, id, before)
	}
	if notices := noticesOfKind(r.t, r.pool, issue, kind); len(notices) != 0 {
		r.t.Fatalf("the %s notices of %s = %+v after %s, want none: its architect is told nothing", kind, issue, notices, id)
	}
	if !result.Unspent || r.applied(id) {
		r.t.Fatalf("%s left the result %+v and applied %t, want it unspent and unapplied: the hold or the wait ends while the id stands still", id, result, r.applied(id))
	}
}

// assertRepaired asserts the silence fact id of issue's role sent the worker its phase's task
// again and told its architect: one supervise row more than before, and one worker-silent notice.
func (r *waitRig) assertRepaired(issue string, before int, id string) {
	r.t.Helper()
	if got := r.starts(issue); got != before+1 {
		r.t.Fatalf("the supervise rows of %s = %d after %s, want one more than the %d before it: the start that sends the worker its task again", issue, got, id, before)
	}
	if notices := noticesOfKind(r.t, r.pool, issue, workerSilentNotice); len(notices) != 1 {
		r.t.Fatalf("the %s notices of %s = %d after %s, want the one its architect is owed", workerSilentNotice, issue, len(notices), id)
	}
	if !r.applied(id) {
		r.t.Fatalf("%s is not applied after the repair it made", id)
	}
}

// A merger the closed design gate holds is told nothing and sent nothing by the silence sweep's
// handlers (R1), however long it is quiet, and each fact is left unspent. The tree's gate is open
// while its child's merger works merging; the spec's version 4 closes it; the merger goes quiet
// past `worker_silence_minutes`, and later reports background work of its own past
// `worker_background_hours`. Neither fact writes a start row or a notice. Once a person approves
// version 4 the gate opens, and the same facts act on the merger as on any open phase: the
// background report is told to its architect, and, the report cleared, the silence sends the
// merger its phase's task again and tells its architect.
func TestASilenceOfAMergerTheDesignGateHoldsWritesNothingUntilTheGateOpens(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Planning)
	r.issue(waitChild, phase.Merging)
	merger := r.claimed(waitChild, claim.RoleMerger)
	r.apply("dispatch", "spec-4", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 4})
	if gate := gateState(t, r.pool, waitRoot); gate.LatestVersion != 4 || *gate.ApprovedVersion != 3 {
		t.Fatalf("the spec's gate = %+v after its version 4, want it closed at 4 with 3 approved", gate)
	}
	r.goesQuiet(merger)
	before := r.starts(waitChild)

	silence, silenceID := silenceOf(waitChild, claim.RoleMerger)
	r.assertLeftAlone(waitChild, workerSilentNotice, before, silenceID, r.apply("supervise", silenceID, silence))

	r.reportsBackground(merger, true)
	background, backgroundID := backgroundOf(waitChild, claim.RoleMerger)
	r.assertLeftAlone(waitChild, workerBackgroundNotice, before, backgroundID, r.apply("supervise", backgroundID, background))

	r.apply("dispatch", "approved-4", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactApproved, Version: 4})
	if gate := gateState(t, r.pool, waitRoot); *gate.ApprovedVersion != 4 {
		t.Fatalf("the spec's gate = %+v after the approval of version 4, want it open", gate)
	}

	r.apply("supervise", backgroundID, background)
	if notices := noticesOfKind(t, r.pool, waitChild, workerBackgroundNotice); len(notices) != 1 {
		t.Fatalf("the %s notices of %s = %d once the gate opened, want the one its architect is owed", workerBackgroundNotice, waitChild, len(notices))
	}
	r.reportsBackground(merger, false)
	r.apply("supervise", silenceID, silence)
	r.assertRepaired(waitChild, before, silenceID)
}

// A gated step that has no gate row holds no merger (R1, D6): the outbox's fence skips such a
// step, although the tree's gate (record.TreeGate.Open) reads it closed. The deployment gates the
// spec and the decisions; the spec's row is approved and the decisions have no row, so the tree's
// gate is closed and its only unapproved step has no row. Its merger in merging, quiet past the
// bound, is a worker the sweep repairs as on any open phase.
func TestAGatedStepWithNoRowHoldsNoMergerAndItsSilenceIsRepaired(t *testing.T) {
	r := newWaitRig(t, []record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions})
	r.tree(phase.Planning)
	r.issue(waitChild, phase.Merging)
	merger := r.claimed(waitChild, claim.RoleMerger)
	r.goesQuiet(merger)
	seedRecord(t, r.pool, func(tx pgx.Tx) error {
		gates, err := record.NewStore().GatesOf(context.Background(), tx, waitRoot)
		if err != nil {
			return err
		}
		gate := record.TreeGate{Steps: r.engine.cfg.Steps, Rows: gates}
		if gate.Open() {
			t.Fatalf("the tree's gate %+v reads open, want it closed on the decisions, which have no row", gate)
		}
		if _, held := HeldByDesignGate(record.Issue{Key: waitChild, Phase: phase.Merging}, claim.RoleMerger, gate); held {
			t.Fatalf("the tree's gate %+v holds the merger, want a step with no row to hold none", gate)
		}
		return nil
	})
	before := r.starts(waitChild)

	silence, id := silenceOf(waitChild, claim.RoleMerger)
	r.apply("supervise", id, silence)

	r.assertRepaired(waitChild, before, id)
}

// A worker whose wait stands is told nothing and sent nothing by the silence sweep's handlers (R3),
// and each fact is left unspent. The tester declares a wait, goes quiet past the bound, and later
// reports background work past its bound: neither fact writes a start row or a notice. News then
// reaches its session, which ends the wait, and the same facts act on the tester as before it
// declared: the background report is told, and, the report cleared, the silence is repaired.
func TestASilenceOfAWorkerWhoseWaitStandsWritesNothingUntilTheWaitEnds(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Testing)
	wait := r.waiting(waitRoot, claim.RoleTester)
	r.goesQuiet(wait.Token)
	r.assertStanding(r.engine, wait)
	before := r.starts(waitRoot)

	silence, silenceID := silenceOf(waitRoot, claim.RoleTester)
	r.assertLeftAlone(waitRoot, workerSilentNotice, before, silenceID, r.apply("supervise", silenceID, silence))

	r.reportsBackground(wait.Token, true)
	background, backgroundID := backgroundOf(waitRoot, claim.RoleTester)
	r.assertLeftAlone(waitRoot, workerBackgroundNotice, before, backgroundID, r.apply("supervise", backgroundID, background))
	r.assertStanding(r.engine, wait)

	if result := r.apply("api", "wait-news:"+wait.ID, intake.WaitNews{Token: wait.Token, Issue: waitRoot,
		Role: claim.RoleTester, WaitID: wait.ID}); !result.WaitEnded {
		t.Fatalf("the news report = %+v, want it to end wait %s", result, wait.ID)
	}

	r.apply("supervise", backgroundID, background)
	if notices := noticesOfKind(t, r.pool, waitRoot, workerBackgroundNotice); len(notices) != 1 {
		t.Fatalf("the %s notices of %s = %d once the wait ended, want the one its architect is owed", workerBackgroundNotice, waitRoot, len(notices))
	}
	r.reportsBackground(wait.Token, false)
	r.apply("supervise", silenceID, silence)
	r.assertRepaired(waitRoot, before, silenceID)
}

// The sweep's own filter (Engine.QuietByDesign) answers, of the claims it is given, the mergers the
// closed design gate holds and the workers whose wait stands, and no other, in one read over many
// claims. The tree's gate closes over its merger in merging; beside it a tester waits, a second
// tester's wait was declared in a session its claim no longer runs (a wait that does not stand,
// though no fact has ended it yet), an implementer declared nothing, and a key names no record.
// Once a person approves the spec the gate opens and the merger is no longer answered. Under a
// deployment that also gates the decisions, for which the root holds no row, the tree's gate
// reads closed and still holds no merger; the spec's version 5 then closes the spec's own row, and
// the merger is held again.
func TestTheSilenceSweepsFilterHoldsBackHeldMergersAndStandingWaitsAlone(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Planning)
	r.issue(waitChild, phase.Merging)
	r.issue(waitSibling, phase.Testing)
	r.issue("LEGION-211", phase.Testing)
	r.issue("LEGION-212", phase.Implementing)
	merger := r.claimed(waitChild, claim.RoleMerger)
	waiting := r.waiting(waitSibling, claim.RoleTester)
	replaced := r.waiting("LEGION-211", claim.RoleTester)
	implementer := r.claimed("LEGION-212", claim.RoleImplementer)
	r.apply("dispatch", "spec-4", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 4})
	if _, err := r.pool.Exec(context.Background(), `update claims set session = 'ses-fresh' where token = $1`,
		string(replaced.Token)); err != nil {
		t.Fatalf("give the second tester a fresh session: %v", err)
	}
	keys := []record.RoleRunKey{
		{Token: merger, Issue: waitChild, Role: claim.RoleMerger, Generation: 1},
		{Token: waiting.Token, Issue: waitSibling, Role: claim.RoleTester, Generation: 1},
		{Token: replaced.Token, Issue: "LEGION-211", Role: claim.RoleTester, Generation: 1},
		{Token: implementer, Issue: "LEGION-212", Role: claim.RoleImplementer, Generation: 1},
		{Token: waitToken(t, "LEGION-299", claim.RoleMerger), Issue: "LEGION-299", Role: claim.RoleMerger, Generation: 1},
	}
	quietUnder := func(engine *Engine) map[claim.Token]struct{} {
		t.Helper()
		var quiet map[claim.Token]struct{}
		seedRecord(t, r.pool, func(tx pgx.Tx) error {
			var err error
			quiet, err = engine.QuietByDesign(context.Background(), tx, keys)
			return err
		})
		return quiet
	}
	wantQuiet := func(engine *Engine, why string, tokens ...claim.Token) {
		t.Helper()
		quiet := quietUnder(engine)
		if len(quiet) != len(tokens) {
			t.Fatalf("the filter answers %v, want %v: %s", quiet, tokens, why)
		}
		for _, token := range tokens {
			if _, found := quiet[token]; !found {
				t.Fatalf("the filter answers %v, want %v: %s", quiet, tokens, why)
			}
		}
	}

	wantQuiet(r.engine, "the merger the closed spec holds and the tester whose wait stands", merger, waiting.Token)

	r.apply("dispatch", "approved-4", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactApproved, Version: 4})
	wantQuiet(r.engine, "the gate opened, so the tester whose wait stands alone", waiting.Token)

	decisions := r.engineGating([]record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions})
	wantQuiet(decisions, "the decisions have no row, which holds no merger, and the spec is approved", waiting.Token)
	r.apply("dispatch", "spec-5", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 5})
	wantQuiet(decisions, "the spec's version 5 is unapproved and has its row", merger, waiting.Token)
}
