package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The fix runs beside the review: each path that resumes or wakes the role of the issue's phase
// reaches the fix's implementer too while the fix runs (OpenRoles), and sends it the fix's own
// work, never the phase's.

// clearStarts takes every start the outbox holds for LEGION-208 out, as the outbox's lanes take a
// start out once it ran, so a later read finds only the starts the next fact writes.
func (r *fixRig) clearStarts() {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), `delete from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start'`, "LEGION-208"); err != nil {
		r.t.Fatalf("take the starts of LEGION-208 out: %v", err)
	}
}

// claimGiven sets the entry role's claim of LEGION-208 was last given.
func (r *fixRig) claimGiven(role claim.Role, entry int64) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), `update claims set given_entry = $2 where token = $1`,
		string(wakeToken(r.t, role)), entry); err != nil {
		r.t.Fatalf("give the %s's claim entry %d: %v", role, entry, err)
	}
}

// bootSuspended records LEGION-208's reviewer and implementer as claims the daemon's own rule
// suspended, each level with the entry its phases row names where level and behind it otherwise,
// takes out the starts the outbox holds, boots the daemon, and answers the starts the boot wrote.
func (r *fixRig) bootSuspended(level bool) []record.SuperviseRequest {
	r.t.Helper()
	for _, role := range []claim.Role{claim.RoleReviewer, claim.RoleImplementer} {
		seedRuleSuspendedOwner(r.t, r.pool, wakeProjectToken(r.t), "LEGION-208", role)
		if level {
			r.claimGiven(role, roleRowOf(r.t, r.pool, r.engine, "LEGION-208", role).Entry)
		}
	}
	r.clearStarts()
	r.apply("boot", intake.DaemonBooted{Boot: 1})
	return superviseRowsOf(r.t, r.pool, "LEGION-208", "start")
}

// startsByRole is starts by the role each one starts, failing the test where two start one role.
func startsByRole(t *testing.T, starts []record.SuperviseRequest) map[claim.Role]record.SuperviseRequest {
	t.Helper()
	byRole := map[claim.Role]record.SuperviseRequest{}
	for _, start := range starts {
		if _, twice := byRole[start.Role]; twice {
			t.Fatalf("the starts = %+v, want one of the %s", starts, start.Role)
		}
		byRole[start.Role] = start
	}
	return byRole
}

// assertReviewersStart fails unless start is a start of the reviewer's own step in reviewing.
func assertReviewersStart(t *testing.T, start record.SuperviseRequest) {
	t.Helper()
	if start.Role != claim.RoleReviewer || start.Phase != phase.Reviewing || start.Fix != 0 || start.FixFrom != "" || start.Task == "" {
		t.Fatalf("the reviewer's start = %+v, want its own step's in reviewing, of no fix", start)
	}
}

// assertFixResumed fails unless start resumes fix 1: the implementer's start in implementing,
// carrying the fix's number and the head its working copy is made at, waiting for no slot, with
// the fix's stored task (the implementer's row's) at entry.
func (r *fixRig) assertFixResumed(start record.SuperviseRequest, entry int64) {
	r.t.Helper()
	row := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleImplementer)
	if start.Role != claim.RoleImplementer || start.Phase != phase.Implementing || start.Fix != 1 || start.FixFrom != fixHead ||
		start.NeedsSlot || start.Entry != entry || start.Generation != 1 {
		r.t.Fatalf("the implementer's start = %+v, want fix 1's in implementing from %s at entry %d, waiting for no slot", start, fixHead, entry)
	}
	if row.EntryTask == "" || start.Task != row.EntryTask {
		r.t.Fatalf("the implementer's start carries task %q, want the fix's stored task %q", start.Task, row.EntryTask)
	}
}

// The boot resumes the fix beside the review: the reviewer is started on its own step, and the
// implementer on the fix's stored task, as a start of the fix, which waits for no slot. A claim
// behind the entry its row names is sent that entry; one level with it is sent the stored task as
// its resume. The control, kept: once the fix ended, the same boot resumes the reviewer alone.
func TestABootResumesTheFixBesideTheReview(t *testing.T) {
	t.Run("a claim behind its entry is sent the fix's entry", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		entry := r.latestFix().Entry
		starts := startsByRole(t, r.bootSuspended(false))
		if len(starts) != 2 {
			t.Fatalf("the boot's starts = %+v, want the reviewer's and the fix's", starts)
		}
		assertReviewersStart(t, starts[claim.RoleReviewer])
		r.assertFixResumed(starts[claim.RoleImplementer], entry)
	})

	t.Run("a claim level with its entry is sent the fix's stored task as its resume", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		starts := startsByRole(t, r.bootSuspended(true))
		if len(starts) != 2 {
			t.Fatalf("the boot's starts = %+v, want the reviewer's and the fix's", starts)
		}
		assertReviewersStart(t, starts[claim.RoleReviewer])
		fix := starts[claim.RoleImplementer]
		r.assertFixResumed(fix, 0)
		if !fix.ResumeTask {
			t.Fatalf("the fix's resume = %+v, want it marked a resume task", fix)
		}
	})

	t.Run("the control: once the fix ended, the reviewer alone", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		if result := r.apply("fix-done", fixCompletion(fixHead)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.RunningFix != 0 || issue.Phase != phase.Reviewing {
			t.Fatalf("LEGION-208 after the fix's end = %s running fix %d, want reviewing with none", issue.Phase, issue.RunningFix)
		}
		starts := r.bootSuspended(false)
		if len(starts) != 1 {
			t.Fatalf("the boot's starts = %+v, want the reviewer's alone", starts)
		}
		assertReviewersStart(t, starts[0])
	})
}

// A fix that still reads its failed job has raised no entry, so the boot resumes the reviewer
// alone: no path resumes such a fix.
func TestABootWhileTheFixReadsItsJobResumesTheReviewerAlone(t *testing.T) {
	r := reviewing(t)
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
	starts := r.bootSuspended(false)
	if len(starts) != 1 {
		t.Fatalf("the boot's starts = %+v, want the reviewer's alone", starts)
	}
	assertReviewersStart(t, starts[0])
}

// releasedBesideTheFix records role's claim of LEGION-208 as a release leaves it: suspended as
// `released`, serving generation 1.
func (r *fixRig) releasedBesideTheFix(role claim.Role) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, 'LEGION-208', 'LEGION-208', $3, 1, $4, '', 'suspended', 'released', 1, 0, 0, 0, 0)`,
		string(wakeToken(r.t, role)), wakeProjectToken(r.t), string(role), "session-"+string(role)); err != nil {
		r.t.Fatalf("seed the released %s: %v", role, err)
	}
}

// settledWakes is the role of each start a CI settlement's wake wrote for LEGION-208, oldest
// first, failing the test where one names no settlement.
func (r *fixRig) settledWakes() []string {
	r.t.Helper()
	var roles []string
	for _, start := range superviseRowsOf(r.t, r.pool, "LEGION-208", "start") {
		if start.WakeKind != pods.WakeChecks {
			continue
		}
		if start.Task == "" || start.Phase != "" {
			r.t.Fatalf("the wake = %+v, want a start of no phase carrying its task", start)
		}
		roles = append(roles, string(start.Role))
	}
	return roles
}

// A CI settlement of the head on a task that takes a fix wakes nobody itself while the fix runs
// beside the review: its reports name each current reader, the fix the implementer runs and the
// reviewer's step, and those rows wake their released readers when the outbox delivers them. The
// control, kept: on a task that takes no fix the same settlement wakes the worker of the issue's
// phase, the reviewer, as main wakes it.
func TestACISettlementWakesTheFixAndTheReviewer(t *testing.T) {
	settle := func(r *fixRig) {
		r.engine.cfg.Pods = wakePods(t, r.pool)
		r.releasedBesideTheFix(claim.RoleReviewer)
		r.releasedBesideTheFix(claim.RoleImplementer)
		r.apply("settled", intake.PullRequestChecks{Repo: statesRepo, Number: 42, HeadSHA: fixHead, Generation: 1,
			Snapshot: fixHead + "-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: []string{"build"}})
	}

	t.Run("the reports name each reader and the settlement wakes nobody", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		settle(r)
		assertStrings(t, "the settlement's wakes", r.settledWakes(), nil)
		if resumes := wakeResumes(t, r.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none: the reports wake their readers", resumes)
		}
		assertStrings(t, "the reports", r.news(record.CINewsRuns), []string{"architect next=fix-runs",
			"implementer " + r.fixReader() + " next=fix-runs", "reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing) + " next=fix-runs"})
	})

	t.Run("the control: on a task that takes no fix the settlement wakes the reviewer", func(t *testing.T) {
		r := reviewing(t)
		r.releaseBefore()
		settle(r)
		assertStrings(t, "the settlement's wakes", r.settledWakes(), []string{string(claim.RoleReviewer)})
		if resumes := wakeResumes(t, r.pool); len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleReviewer), pods.WakeChecks} {
			t.Fatalf("pod resumes = %v, want the reviewer's settlement wake", resumes)
		}
	})
}

// silentFixStep is the moment the silence tests' implementer last stepped: two hours before the
// fix rig's clock, past the bound.
var silentFixStep = stepClock.Add(-2 * time.Hour)

// silentImplementer records LEGION-208's implementer as the silence repair finds a worker nothing
// will prompt: up and between turns in the issue's tree, holding no task, level with the entry its
// row names, its agent's last step two hours before the clock; and takes out the starts the
// outbox holds, as its lanes ran them.
func (r *fixRig) silentImplementer() {
	r.t.Helper()
	entry := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleImplementer).Entry
	if _, err := r.pool.Exec(r.t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, update_round, step_at, activity_background, given_entry, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'ready', '', $3, false, $4, 0, 0, 0, 0)`,
		string(wakeToken(r.t, claim.RoleImplementer)), wakeProjectToken(r.t), silentFixStep, entry); err != nil {
		r.t.Fatalf("seed the silent implementer: %v", err)
	}
	r.clearStarts()
}

// silence applies the sweep's fact of the implementer's silence.
func (r *fixRig) silence() {
	r.t.Helper()
	fact := intake.WorkerSilent{Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, LastStep: silentFixStep, Bound: silenceBound}
	r.apply(fmt.Sprintf("worker-silent:%s:%s", wakeToken(r.t, claim.RoleImplementer), silentFixStep.Format(time.RFC3339Nano)), fact)
}

// The silence tick reminds the fix's implementer: a live implementer nothing will prompt while
// the fix runs beside the review is sent the fix's stored task again as a start of the fix, and
// its architect is told. The control, kept: once the fix ended, the implementer works no open
// phase, and its silence is passed over.
func TestASilenceTickRemindsTheFix(t *testing.T) {
	t.Run("the fix's implementer is reminded", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.silentImplementer()
		r.silence()
		starts := r.fixStarts()
		if len(starts) != 1 {
			t.Fatalf("the starts of a fix after the silence = %+v, want one", starts)
		}
		r.assertFixResumed(starts[0], 0)
		notices := noticesOfKind(t, r.pool, "LEGION-208", workerSilentNotice)
		if len(notices) != 1 || notices[0].Role != claim.RoleImplementer {
			t.Fatalf("the %s notices = %+v, want the one of the implementer", workerSilentNotice, notices)
		}
	})

	t.Run("the control: once the fix ended, nothing", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		if result := r.apply("fix-done", fixCompletion(fixHead)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		r.silentImplementer()
		r.silence()
		if starts := superviseRowsOf(t, r.pool, "LEGION-208", "start"); len(starts) != 0 {
			t.Fatalf("the starts after the silence = %+v, want none", starts)
		}
		if notices := noticesOfKind(t, r.pool, "LEGION-208", workerSilentNotice); len(notices) != 0 {
			t.Fatalf("the %s notices = %+v, want none", workerSilentNotice, notices)
		}
	})
}
