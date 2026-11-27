package workflow

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// failedClaim is issue's role failing as the daemon applies it (daemon's applyTerminal): the
// agent never came up, launch after launch, until the launch budget ran out, with the counters
// that failure left and the limits of the shipped daemon beside the cause.
func failedClaim(issue string, role claim.Role) intake.ClaimFailed {
	return intake.ClaimFailed{Issue: issue, Role: role, Cause: supervise.CauseNeverRegistered,
		Budgets: supervise.Budgets{LaunchFailures: 3},
		Limits:  supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2}}
}

// checkOpened is when every check rig's merge lands, and checkBound the rig's production check
// bound, short enough to drive in a test.
var checkOpened = time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)

const checkBound = 48 * time.Hour

// checkClock is the engine's clock in the check tests, moved by hand where a time matters.
type checkClock struct{ now time.Time }

func (c *checkClock) Now() time.Time { return c.now }

// checkRig is a tree whose issue's production check the test drives: the pool, the engine on a
// clock the test moves, the journal it writes, and apply, which applies a fact and fails the test
// on an error.
type checkRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	clock  *checkClock
	logged *bytes.Buffer
	apply  func(id string, fact intake.Fact) intake.Result
}

func newCheckRig(t *testing.T) checkRig {
	t.Helper()
	pool := migratedPool(t)
	clock := &checkClock{now: checkOpened}
	logged := &bytes.Buffer{}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: checkBound, ProductionCheckRole: claim.RoleTester, Clock: clock.Now},
		slog.New(slog.NewTextHandler(logged, nil)))
	return checkRig{pool: pool, engine: engine, clock: clock, logged: logged, apply: applyFacts(t, pool, engine)}
}

// childCheckRig is a running root (LEGION-208, implementing) whose child LEGION-209 merged
// sjawhar/legion#43 at merge-43: the child is done with its check open at look 1 in the
// tester's live session.
func childCheckRig(t *testing.T) checkRig {
	t.Helper()
	rig := newCheckRig(t)
	seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	return rig
}

// rootCheckRig is a root LEGION-208 whose sjawhar/legion#42 merged at merge while it awaited it,
// and which a person then closed on Dispatch, with a child LEGION-209 left implementing: the tree
// lingers an hour and the root's check is open at look 1. The merge alone no longer produces that
// state — a merged root's close waits for the rest of its tree (closeMerged) — and a person's
// close of a root still lingers the tree whatever its children are doing, which is the path these
// tests are about.
func rootCheckRig(t *testing.T) checkRig {
	t.Helper()
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	rig.apply("root-closed", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
	return rig
}

// A report on the root's check while its tree lingers is recorded: linger holds the members of a
// closed tree, and the check after the close is the one thing a member still does there.
func TestACheckReportWhileTheRootLingersIsRecorded(t *testing.T) {
	t.Parallel()
	rig := rootCheckRig(t)
	if result := rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Claim: "tester", Look: 1, Verdict: "pass", Summary: "driven in production"}); result.Refusal != nil {
		t.Fatalf("the report = %+v, want it recorded", result.Refusal)
	}
	if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || check.Open() || check.Outcome != record.CheckRecorded || check.LookOpen {
		t.Fatalf("the root's check = %+v, want it closed as recorded", check)
	}
}

// A pending report ends the look with nothing to show and takes the pod: the tester's claim
// is suspended, session kept, and the issue suspended (issue_suspend), whose Sandbox goes with its
// pod and disk, both rows written after the look's
// start row so the suspension's fences find no later start. The check stays open at look 1 with
// its look closed.
func TestAPendingReportSuspendsTheTesterAfterTheLooksStart(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	starts := issueOutboxOf(t, rig.pool, "LEGION-209", "supervise")
	start := starts[len(starts)-1]
	if decodeSupervise(t, start).CheckLook != 1 {
		t.Fatalf("the newest supervise row before the report = %s, want the check's look 1 start", start.payload)
	}
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "not deployed yet"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check == nil || !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after pending = %+v, want it open at look 1 with the look closed", check)
	}
	suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
	if len(suspends) != 1 || suspends[0].Role != claim.RoleTester || suspends[0].Generation != 7 {
		t.Fatalf("suspend rows = %+v, want the tester's alone", suspends)
	}
	suspensions := issueSuspensions(t, rig.pool, "LEGION-209")
	if len(suspensions) != 1 || suspensions[0].Generation != 7 || suspensions[0].TreeGeneration != 7 {
		t.Fatalf("issue suspensions = %+v, want one row of the look's run", suspensions)
	}
	for _, kind := range []string{"supervise", "issue_suspend"} {
		rows := issueOutboxOf(t, rig.pool, "LEGION-209", kind)
		if rows[len(rows)-1].id <= start.id {
			t.Fatalf("the newest %s row %d is not newer than the look's start %d", kind, rows[len(rows)-1].id, start.id)
		}
	}
}

// A pass closes a child's check as recorded and ends the tester's part in it: its claim is
// closed (issue_close) and the child's Sandbox deleted with the pod it ran in (an IssueSuspend),
// since its five other claims retired at the close. A fail closes it as a defect the same way.
func TestAPassOrFailOfAChildsCheckClosesItAndDeletesItsSandbox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		verdict string
		outcome record.CheckOutcome
	}{{"pass", record.CheckRecorded}, {"fail", record.CheckDefect}} {
		t.Run(tc.verdict, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("report", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: tc.verdict, Summary: "seen"})
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || check.Open() || check.Outcome != tc.outcome || check.LookOpen || !check.ClosedAt.Equal(checkOpened) {
				t.Fatalf("the check after %s = %+v, want it closed as %s", tc.verdict, check, tc.outcome)
			}
			closed := superviseRequests(t, rig.pool, "issue_close")
			for _, role := range claim.Roles {
				if closed["LEGION-209/"+string(role)] != 1 {
					t.Fatalf("issue_close rows = %v, want every role's once, the tester's by the check's close", closed)
				}
			}
			if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 || got[0].Generation != 7 || got[0].TreeGeneration != 7 {
				t.Fatalf("issue suspensions = %+v, want one row of the check's run", got)
			}
			assertOutboxCount(t, rig.pool, "linger_close", 0)
		})
	}
}

// A pass of a root's check closes the tester's claim and writes the suspension that deletes
// its Sandbox, as the linger's own row did: the tree's close then finds nothing of it left.
func TestAPassOfARootsCheckClosesItsTesterAndSuspendsTheIssue(t *testing.T) {
	t.Parallel()
	rig := rootCheckRig(t)
	rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass", Summary: "seen"})
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-208/tester"] != 1 || len(got) != 1 {
		t.Fatalf("issue_close rows = %v, want the tester's alone", got)
	}
	got := issueSuspensions(t, rig.pool, "LEGION-208")
	if len(got) != 2 {
		t.Fatalf("the root's issue suspensions = %+v, want the linger's and the check's", got)
	}
	// The root lingers until an hour from now, so the tree's close is not re-armed by the check.
	assertOutboxCount(t, rig.pool, "linger_close", 1)
}

// A report is refused, and changes nothing, on an issue with no open check (none, or one closed),
// on a look that is not the check's current one (an earlier look, the check having moved on), and
// with a verdict other than pass, pending or fail. A report of the current look is taken however
// that look ended (TestALateReportOfTheCurrentLookIsRecorded).
func TestACheckReportIsRefusedWhereNoLookIsOpenToIt(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	if got := refusalCode(rig.apply("no-check", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass"})); got != "CHECK_NOT_OPEN" {
		t.Fatalf("a report on an issue with no check = %q, want CHECK_NOT_OPEN", got)
	}
	if got := refusalCode(rig.apply("bad-verdict", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "maybe"})); got != "CHECK_VERDICT_REQUIRED" {
		t.Fatalf("a report with verdict maybe = %q, want CHECK_VERDICT_REQUIRED", got)
	}
	if got := refusalCode(rig.apply("wrong-look", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})); got != "CHECK_LOOK_NOT_OPEN" {
		t.Fatalf("a report naming look 2 while the check is at look 1 = %q, want CHECK_LOOK_NOT_OPEN", got)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || !check.LookOpen || check.Looks != 1 {
		t.Fatalf("the check after the refused reports = %+v, want it untouched at look 1 open", check)
	}
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "nothing in production yet"})
	// A second report of look 1 is not refused here: the look it names is still the check's own,
	// and a report of the current look counts however that look ended (dispatch://INFRA-714 R12,
	// TestALateReportOfTheCurrentLookIsRecorded). What this case holds is the look that is not the
	// check's, above and below.
	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})
	if got := refusalCode(rig.apply("closed", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})); got != "CHECK_NOT_OPEN" {
		t.Fatalf("a report on a closed check = %q, want CHECK_NOT_OPEN", got)
	}
}

// A look nobody reports ends at its bound as a pending report would: the tester is suspended
// and the Sandbox with it. A row naming a look already ended, or an earlier look, changes nothing.
func TestCheckLookEndedEndsASilentLook(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	rig.apply("stale-look", intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "look bound"})
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen {
		t.Fatalf("the check after a row of a look it has not reached = %+v, want look 1 still open", check)
	}
	rig.clock.now = checkOpened.Add(CheckLookBound)
	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after its look's bound = %+v, want it open at look 1 with the look ended", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 || len(got) != 1 {
		t.Fatalf("suspend rows = %v, want the tester's", got)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 {
		t.Fatalf("issue suspensions = %+v, want one row", got)
	}
	rig.apply("look-bound-again", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound"})
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 {
		t.Fatalf("suspend rows after a second row of the ended look = %v, want the one", got)
	}
}

// A fact of a check a later merge of the same issue replaced — the first check, opened at the
// child's generation 6, outlived the child's re-entry open at its look 1, and the rerun's merge at
// generation 7 closed it as not run and opened the second, which counts its looks from one again —
// changes nothing of the second: a look bound naming the first check's OpenedAt leaves the
// second's look 1 open, where the same fact naming the second check ends the look.
func TestAFactOfAReplacedCheckChangesNothingOfItsSuccessor(t *testing.T) {
	t.Parallel()
	rig := newCheckRig(t)
	seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	firstOpened := checkOpened.Add(-24 * time.Hour)
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
			Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41", Role: claim.RoleTester,
			OpenedAt: firstOpened, BoundAt: firstOpened.Add(checkBound), Looks: 1, LookOpen: true, LookWakeAt: &firstOpened,
			LastWakeKind: "merge", LastWakeDetail: "merge-41", LastWakeAt: &firstOpened,
			LookID: "LOOKIDOFTHEREPLACEDCHECKAA", LookIDLook: 1, LookIDOpenedAt: firstOpened, OpenedGeneration: 6,
		})
	})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || !check.OpenedAt.Equal(checkOpened) || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check after the rerun's merge = %+v, want the second check, opened at the merge, at look 1 open", check)
	}

	rig.clock.now = checkOpened.Add(CheckLookBound)
	rig.apply("stale-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: firstOpened})
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen || check.Looks != 1 {
		t.Fatalf("the second check after the first check's look bound = %+v, want its look 1 still open", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); len(got) != 0 {
		t.Fatalf("suspend rows after the first check's look bound = %v, want none", got)
	}
	rig.apply("bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.LookOpen || check.Looks != 1 || !check.Open() {
		t.Fatalf("the second check after its own look bound = %+v, want it open at look 1 with the look ended", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 {
		t.Fatalf("suspend rows after the second check's look bound = %v, want the tester's", got)
	}
}

// lookIDRule is the rule each refusal of a report that a later merge or a new look overtook names,
// in the route's words for it (api's lookIDRule).
const lookIDRule = "A report gives check_opened_at and look_id from the task of its look."

// A report of a check that a later merge replaced — the route read the first check, and the
// rerun's merge opened the second before the report's fact applied — names the first check's
// OpenedAt, and is refused CHECK_NOT_OPEN: the second check, which counts its looks from one
// again, is not changed in any field, no claim is closed and nothing is suspended. The refusal
// names the second check's current look and the rule, and holds no look id. The same report
// naming the second check is recorded on it.
func TestAReportOfAReplacedCheckIsNotRecordedOnItsSuccessor(t *testing.T) {
	t.Parallel()
	rig := newCheckRig(t)
	seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	firstOpened := checkOpened.Add(-24 * time.Hour)
	firstClosed := firstOpened.Add(2 * time.Hour)
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
			Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41",
			OpenedAt: firstOpened, BoundAt: firstOpened.Add(checkBound), Looks: 1, LookWakeAt: &firstOpened,
			LastWakeKind: "merge", LastWakeDetail: "merge-41", LastWakeAt: &firstOpened, ClosedAt: &firstClosed, Outcome: record.CheckNotRun,
		})
	})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	successor := productionCheck(t, rig.pool, "LEGION-209")
	if successor == nil || !successor.Open() || !successor.OpenedAt.Equal(checkOpened) || successor.Looks != 1 || !successor.LookOpen || successor.CurrentLookID() == "" {
		t.Fatalf("the check after the rerun's merge = %+v, want the second check, opened at the merge, at look 1 open with its look id", successor)
	}
	closesBefore := superviseRequests(t, rig.pool, "issue_close")["LEGION-209/tester"]

	result := rig.apply("stale-report", intake.CheckReport{Issue: "LEGION-209", Claim: "tester", Look: 1, OpenedAt: firstOpened,
		Verdict: "pass", Summary: "driven in production"})
	if got := refusalCode(result); got != "CHECK_NOT_OPEN" {
		t.Fatalf("the report of the replaced check = %q, want CHECK_NOT_OPEN", got)
	}
	for _, want := range []string{"look 1", lookIDRule} {
		if !strings.Contains(result.Refusal.Message, want) {
			t.Fatalf("the refusal is %q, want it to say %q", result.Refusal.Message, want)
		}
	}
	if strings.Contains(result.Refusal.Message, successor.LookID) {
		t.Fatalf("the refusal is %q, which holds the second check's look id", result.Refusal.Message)
	}
	if after := productionCheck(t, rig.pool, "LEGION-209"); !reflect.DeepEqual(after, successor) {
		t.Fatalf("the second check after the first check's report = %+v, want it unchanged: %+v", after, successor)
	}
	if got := superviseRequests(t, rig.pool, "issue_close")["LEGION-209/tester"]; got != closesBefore {
		t.Fatalf("the tester's issue_close rows after the first check's report = %d, want %d: no report closed the second check", got, closesBefore)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); len(got) != 0 {
		t.Fatalf("suspend rows after the first check's report = %v, want none", got)
	}

	if result := rig.apply("report", intake.CheckReport{Issue: "LEGION-209", Claim: "tester", Look: 1, OpenedAt: checkOpened,
		Verdict: "pass", Summary: "driven in production"}); result.Refusal != nil {
		t.Fatalf("the second check's own report = %+v, want it recorded", result.Refusal)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.Open() || check.Outcome != record.CheckRecorded || !check.OpenedAt.Equal(checkOpened) {
		t.Fatalf("the second check after its own report = %+v, want it closed as recorded", check)
	}
}

// A report that a new look overtook — the route read look 1 as the current look, and look 1's
// pending named a wake that started look 2 before the report's fact applied — is refused
// CHECK_LOOK_NOT_OPEN and changes nothing: look 2 stays open. The refusal names look 2 as the
// current look and the rule that a report gives check_opened_at and look_id from the task of its
// look, and holds neither look's id.
func TestAReportThatANewLookOvertookNamesTheCurrentLookAndTheRule(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	first := productionCheck(t, rig.pool, "LEGION-209")
	wake := checkOpened.Add(time.Hour)
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Verdict: "pending", Summary: "the deploy has not carried the merge", WakeAt: &wake})
	rig.clock.now = wake
	rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the deploy has not carried the merge"})
	second := productionCheck(t, rig.pool, "LEGION-209")
	if second == nil || second.Looks != 2 || !second.LookOpen || second.CurrentLookID() == "" {
		t.Fatalf("the check after the wake = %+v, want look 2 open with its look id", second)
	}

	result := rig.apply("overtaken", intake.CheckReport{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Verdict: "pass", Summary: "driven in production"})
	if got := refusalCode(result); got != "CHECK_LOOK_NOT_OPEN" {
		t.Fatalf("the report of look 1 after look 2 started = %q, want CHECK_LOOK_NOT_OPEN", got)
	}
	for _, want := range []string{"current look of the check is look 2", lookIDRule} {
		if !strings.Contains(result.Refusal.Message, want) {
			t.Fatalf("the refusal is %q, want it to say %q", result.Refusal.Message, want)
		}
	}
	for _, id := range []string{first.LookID, second.LookID} {
		if strings.Contains(result.Refusal.Message, id) {
			t.Fatalf("the refusal is %q, which holds a look id", result.Refusal.Message)
		}
	}
	if after := productionCheck(t, rig.pool, "LEGION-209"); !reflect.DeepEqual(after, second) {
		t.Fatalf("the check after the overtaken report = %+v, want it unchanged: %+v", after, second)
	}
}

// The claim of the role a check records failing inside a look ends the look as a pending report
// would and tells the architect immediately. It holds nothing: a done issue has no phase to hold,
// and the next wake starts the claim again. Another role's claim failing on the done issue
// changes nothing, as before: the implementer's, which worked the issue's phases but not its
// check.
func TestTheTestersFailedClaimEndsItsLook(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	rig.apply("implementer-failed", failedClaim("LEGION-209", claim.RoleImplementer))
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen {
		t.Fatalf("the check after the implementer's claim failed = %+v, want look 1 still open", check)
	}
	rig.apply("tester-failed", failedClaim("LEGION-209", claim.RoleTester))
	check := productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after the tester's claim failed = %+v, want it open at look 1 with the look ended", check)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 {
		t.Fatalf("issue suspensions = %+v, want one row", got)
	}
	var childPhase phase.Phase
	if err := rig.pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&childPhase); err != nil || childPhase != phase.Done {
		t.Fatalf("the child's phase = %s, %v; want done, not held", childPhase, err)
	}
	notices := noticeKinds(t, rig.pool, "LEGION-209")
	if containsNotice(notices, "held") || !containsNotice(notices, "worker-died") {
		t.Fatalf("notices %v, want one worker-died and no held notice for a check look", notices)
	}
}

// A check recorded with the implementer — the shape every check open before the upgrade has, when
// the implementer worked each of them — stays the implementer's for as long as it is open, under
// an engine whose own checks are the tester's: the look its bound starts is handed to the
// implementer, and that implementer's report of the look is taken and retires its claim.
func TestACheckRecordingTheImplementerStartsTheImplementerAndTakesItsReport(t *testing.T) {
	t.Parallel()
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Done, Generation: 7, Status: "done", Rank: "V", LastDispatchSeq: 2})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", MergeSHA: "merge-43", Failing: []string{}})
	opened := checkOpened
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
			Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
			OpenedAt: opened, BoundAt: opened.Add(checkBound), Role: claim.RoleImplementer, Looks: 1,
			LastWakeKind: wakeMerge, LastWakeDetail: "merge-43", LastWakeAt: &opened,
		})
	})

	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].CheckLook != 2 || !starts[0].CheckOpenedAt.Equal(checkOpened) {
		t.Fatalf("start rows = %+v, want look 2 handed to the implementer the check records", starts)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.Role != claim.RoleImplementer || !check.LookOpen || check.Looks != 2 {
		t.Fatalf("the check at its bound = %+v, want the implementer's, at look 2 open", check)
	}

	rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass", Summary: "seen in production"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Open() || check.Outcome != record.CheckRecorded {
		t.Fatalf("the check after the implementer's pass = %+v, want it closed as recorded", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/implementer"] != 1 || len(got) != 1 {
		t.Fatalf("issue_close rows = %v, want the implementer's alone: the check's own role is the implementer", got)
	}
}

// A check keeps the role it opened with when the project's `production_check` changes under it:
// the setting decides the checks a close opens, never one already open, which is why the role is
// a column of the record. A check opened under `tester` is still the tester's for the rest of its
// looks once the daemon is rebuilt with the setting off, where that same daemon opens no check of
// its own.
func TestACheckOpenedUnderTheTesterKeepsItWhenTheSettingGoesOff(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	off := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: checkBound, Clock: rig.clock.Now}, nil)
	apply := applyFacts(t, rig.pool, off)

	apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "not deployed yet"})
	suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
	if len(suspends) != 1 || suspends[0].Role != claim.RoleTester {
		t.Fatalf("suspend rows = %+v, want the tester's alone: the check is the tester's", suspends)
	}
	rig.clock.now = checkOpened.Add(checkBound)
	apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Role != claim.RoleTester || check.Looks != 2 || !check.LookOpen || !check.Last {
		t.Fatalf("the check at its bound = %+v, want the tester's last look 2 open", check)
	}
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 2 || starts[1].Role != claim.RoleTester {
		t.Fatalf("start rows = %+v, want the bound's look handed to the tester the check records", starts)
	}
}

// A root reopened inside its linger runs again from the reopen: admit's readmit stands it in
// `admitted` with the reopen's status, no role's phase, and its production check untouched. Nothing
// of the check moves the tree while it runs again: the implementer's failed claim takes the live
// phase's path rather than ending the look (the look's own bound ends it, and that end starts no
// look and suspends nothing, since the implementer and pod are the run's), and the bound closes
// the check as not run without an issue_close of the implementer the run reuses.
func TestAReopenedRootsCheckStartsNoLookWhileItRunsAgain(t *testing.T) {
	t.Parallel()
	rig := rootCheckRig(t)
	if _, err := rig.pool.Exec(context.Background(), "update issues set phase = 'admitted', status = 'todo', linger_until = null where key = 'LEGION-208'"); err != nil {
		t.Fatal(err)
	}
	before := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend"))
	suspensions := len(issueSuspensions(t, rig.pool, "LEGION-208"))

	rig.apply("implementer-failed", failedClaim("LEGION-208", claim.RoleImplementer))
	if check := productionCheck(t, rig.pool, "LEGION-208"); !check.Open() || !check.LookOpen {
		t.Fatalf("the check after the implementer's claim failed while the root runs again = %+v, want its look still open", check)
	}
	if !containsNotice(noticeKinds(t, rig.pool, "LEGION-208"), "worker-died") {
		t.Fatalf("notices %v after the implementer's claim failed, want worker-died: the failure is the run's", noticeKinds(t, rig.pool, "LEGION-208"))
	}

	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-208", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	check := productionCheck(t, rig.pool, "LEGION-208")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after its look's bound = %+v, want open at look 1 with the look ended", check)
	}
	if got := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend")); got != before {
		t.Fatalf("suspend rows = %d, want %d: the look's end suspends nothing of a run", got, before)
	}
	if got := len(issueSuspensions(t, rig.pool, "LEGION-208")); got != suspensions {
		t.Fatalf("issue suspensions = %d, want %d: the pod is the run's", got, suspensions)
	}

	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-208"})
	check = productionCheck(t, rig.pool, "LEGION-208")
	if check.Open() || check.Outcome != record.CheckNotRun {
		t.Fatalf("the check at its bound while the root runs again = %+v, want closed as not run", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-208/implementer"] != 0 {
		t.Fatalf("issue_close rows = %v, want none for the implementer the run reuses", got)
	}
}

// A done child a person parks and sets done again leaves twice more, each a close of the same
// merge: the check it has stands, recorded or open, and no second check is opened over it, no
// second bound row queued, no look started. A rerun's merge, of another pull request, still
// replaces it (openCheck; TestAReplacedOpenCheckClosesNotRunNamingTheMergeThatReplacedIt).
func TestADoneChildSetDoneAgainKeepsItsChecks(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	rig.apply("report", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pass", Summary: "seen"})
	recorded := productionCheck(t, rig.pool, "LEGION-209")
	bounds := outboxKindCount(t, rig.pool, "LEGION-209", record.OutboxKindCheckBound)

	rig.apply("parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 3, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
	// Admission, which runs after the engine on each event, records the observation the engine
	// kept its hands off (the parked status, the sequence); the rig's admission stub records
	// nothing, so the row is moved by hand to what the daemon would hold at the next event.
	if _, err := rig.pool.Exec(context.Background(), "update issues set status = 'backlog', dispatch_status = 'backlog', last_dispatch_seq = 3 where key = 'LEGION-209'"); err != nil {
		t.Fatal(err)
	}
	rig.apply("done-again", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"})
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/planner"] != 2 {
		t.Fatalf("issue_close rows = %v, want the second done to have left again (two of the planner's)", got)
	}
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Open() || check.Outcome != recorded.Outcome || !check.OpenedAt.Equal(recorded.OpenedAt) || check.Looks != recorded.Looks {
		t.Fatalf("the check after the child was set done again = %+v, want the recorded one kept: %+v", check, recorded)
	}
	if got := outboxKindCount(t, rig.pool, "LEGION-209", record.OutboxKindCheckBound); got != bounds {
		t.Fatalf("check_bound rows = %d, want %d: no second check was opened", got, bounds)
	}
	if starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start"); len(starts) != 1 {
		t.Fatalf("start rows = %+v, want look 1's alone", starts)
	}
}

// The check's bound starts its last look, told that it is the last and the check's own bound,
// counted from the issue's close; a pending report of the last look closes the check as not run,
// and so does its silence. A look open at the bound defers the bound by the look bound, and a bound
// row that comes due before the check's bound — an earlier check's of the same issue — is deferred
// to it.
func TestTheBoundStartsTheLastLookWhoseNothingClosesTheCheckAsNotRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  intake.Fact
	}{
		{name: "pending", end: intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pending"}},
		{name: "silence", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "look bound"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("early-bound", intake.CheckBound{Issue: "LEGION-209"})
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 2 || !bounds[1].nextAt.Equal(checkOpened.Add(checkBound)) {
				t.Fatalf("check_bound rows after an early bound = %+v, want a second deferred to the check's bound", bounds)
			}
			rig.clock.now = checkOpened.Add(checkBound)
			rig.apply("bound-in-look", intake.CheckBound{Issue: "LEGION-209"})
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 1 || !check.LookOpen || check.Last {
				t.Fatalf("the check after a bound during look 1 = %+v, want look 1 still open and not last", check)
			}
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 3 || !bounds[2].nextAt.Equal(rig.clock.now.Add(CheckLookBound)) {
				t.Fatalf("check_bound rows after a bound during a look = %+v, want a third deferred two hours", bounds)
			}
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
			rig.clock.now = rig.clock.now.Add(CheckLookBound)
			rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 2 || !check.LookOpen || !check.Last || check.LastWakeKind != "bound" || check.LastWakeDetail != "2 days since the issue closed" {
				t.Fatalf("the check at its bound = %+v, want its last look 2 open, woken by the bound", check)
			}
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if len(starts) != 2 || starts[1].CheckLook != 2 || !strings.Contains(starts[1].Task, "This is the last look: no look follows.") ||
				!strings.Contains(starts[1].Task, "woken by the bound: 2 days since the issue closed.") || !strings.Contains(starts[1].Task, "did not run") {
				t.Fatalf("start rows = %+v, want the last look's task saying so", starts)
			}
			rig.apply("end", tc.end)
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen {
				t.Fatalf("the check after its last look's %s = %+v, want it closed as not run", tc.name, check)
			}
			if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/tester"] != 1 {
				t.Fatalf("issue_close rows = %v, want the tester's once at the close", got)
			}
			if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 2 {
				t.Fatalf("issue suspensions = %+v, want look 1's row and the close's", got)
			}
			rig.apply("bound-after-close", intake.CheckBound{Issue: "LEGION-209"})
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 3 {
				t.Fatalf("check_bound rows after the close = %d, want no more", len(bounds))
			}
		})
	}
}

// The bound's look is told the check's own bound (BoundAt less OpenedAt), never the project's
// bound of the daemon that reaches it: a check opened under a 2-day bound whose daemon is then
// rebuilt with 3 days starts its last look at the bound it opened with, told 2 days since the
// issue closed. Every other rig reaches the bound under the bound it opened with, where the two
// read the same.
func TestTheBoundsLookIsToldTheBoundTheCheckOpenedWith(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "not deployed yet"})
	cfg := rig.engine.cfg
	cfg.ProductionCheckBound = 3 * 24 * time.Hour
	rebuilt := New(record.NewStore(), cfg, rig.engine.log)
	rig.clock.now = checkOpened.Add(checkBound)
	applyFacts(t, rig.pool, rebuilt)("bound", intake.CheckBound{Issue: "LEGION-209"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Looks != 2 || !check.LookOpen || !check.Last || check.LastWakeKind != "bound" || check.LastWakeDetail != "2 days since the issue closed" {
		t.Fatalf("the check at its bound under a rebuilt 3-day bound = %+v, want its last look 2 open, woken by its own 2-day bound", check)
	}
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 2 || !strings.Contains(starts[1].Task, "woken by the bound: 2 days since the issue closed.") {
		t.Fatalf("start rows = %+v, want the last look's task naming the check's own 2-day bound", starts)
	}
}

// The tree's close at the end of its linger waits for an open check: the expiry returns a wait
// naming the moment the check is bound to have ended its last look, rolls back unapplied, and the
// tree stays as it was. The check's close re-arms the linger's close, due now, and the expiry then
// applies.
func TestTheTreesCloseWaitsForAnOpenCheckAndRunsOnceItCloses(t *testing.T) {
	t.Parallel()
	rig := rootCheckRig(t)
	rig.clock.now = checkOpened.Add(time.Hour)
	expiry := intake.LingerExpired{Issue: "LEGION-208", Generation: 7}
	_, err := intake.ApplyFact(context.Background(), rig.pool, "outbox", "linger:LEGION-208:7", expiry, rig.engine, admissionStub{})
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the linger's expiry with an open check = %v, want a wait", err)
	}
	if at, ok := wait.UntilAt(err); !ok || !at.Equal(checkOpened.Add(checkBound+CheckLookBound)) {
		t.Fatalf("the wait names %s (%t), want the check's bound plus the look bound", at, ok)
	}
	if !strings.Contains(err.Error(), "member LEGION-208's production check is open") {
		t.Fatalf("the wait says %q, want it to name the open check", err)
	}
	if got := superviseRequests(t, rig.pool, "tree_close"); len(got) != 0 {
		t.Fatalf("tree_close rows = %v, want none while the check is open", got)
	}
	assertOutboxCount(t, rig.pool, "linger_close", 1)

	rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass"})
	closes := issueOutboxOf(t, rig.pool, "LEGION-208", "linger_close")
	if len(closes) != 2 || !closes[1].nextAt.Equal(rig.clock.now) {
		t.Fatalf("linger_close rows after the check's close = %+v, want a second due now", closes)
	}
	if _, err := intake.ApplyFact(context.Background(), rig.pool, "outbox", "linger:LEGION-208:7", expiry, rig.engine, admissionStub{}); err != nil {
		t.Fatalf("the linger's expiry once the check closed: %v", err)
	}
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	if got := superviseRequests(t, rig.pool, "tree_close"); !sameCounts(got, everyClaim) {
		t.Fatalf("tree_close rows = %v, want each tree claim once: %v", got, everyClaim)
	}
}

// A done child set back to todo keeps its open check through its new run (ReenterChild): the same
// check, open at the same look, with the same look id, so a report its role makes from that
// look's task still counts. Only the task of the look ends: the re-entry queues one retire_look
// row, of that look and check, unreported. The tester's claim is neither closed nor suspended, and
// neither is the issue's Sandbox: they are the new run's. No look starts while the child runs: the
// look's bound ends look 1 and suspends nobody, and the wake it names, due while the child still
// runs, starts no look and is queued again.
func TestReenterChildKeepsItsOpenCheckAndItsLook(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	before := productionCheck(t, rig.pool, "LEGION-209")
	retires := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
	looks := len(lookStarts(t, rig.pool, "LEGION-209"))
	reenterChild(t, rig)
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check == nil || !check.Open() || !check.OpenedAt.Equal(before.OpenedAt) || check.Looks != 1 || !check.LookOpen ||
		check.CurrentLookID() == "" || check.CurrentLookID() != before.CurrentLookID() {
		t.Fatalf("the re-entered child's check = %+v, want the check opened at %s open at look 1, its look open with the same look id", check, before.OpenedAt)
	}
	var generation uint64
	var childPhase phase.Phase
	if err := rig.pool.QueryRow(context.Background(), "select generation, phase from issues where key = 'LEGION-209'").Scan(&generation, &childPhase); err != nil || generation != 8 || childPhase != phase.Admitted {
		t.Fatalf("the re-entered child = generation %d in %s, %v; want generation 8 admitted", generation, childPhase, err)
	}
	if got := superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look")[retires:]; len(got) != 1 || got[0].Role != claim.RoleTester ||
		got[0].CheckLook != 1 || !got[0].CheckOpenedAt.Equal(before.OpenedAt) || got[0].Reported {
		t.Fatalf("retire_look rows of the re-entry = %+v, want one, unreported, of the tester's look 1 of the check opened at %s", got, before.OpenedAt)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/tester"] != 0 {
		t.Fatalf("issue_close rows = %v, want none for the tester the rerun reuses", got)
	}

	rig.clock.now = checkOpened.Add(CheckLookBound)
	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	check = productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.Looks != 1 || check.LookOpen || check.WakeAt == nil {
		t.Fatalf("the check after its look's bound while the child runs = %+v, want open at look 1, the look ended and a wake named", check)
	}
	wake := *check.WakeAt
	rig.clock.now = wake
	rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, WakeAt: wake, Reason: "the retry of a look nobody reported: look bound"})
	if check = productionCheck(t, rig.pool, "LEGION-209"); !check.Open() || check.Looks != 1 || check.LookOpen || check.WakeAt == nil || !check.WakeAt.After(wake) {
		t.Fatalf("the check at its wake while the child runs = %+v, want look 1 still ended and the wake deferred past %s", check, wake)
	}
	if got := len(lookStarts(t, rig.pool, "LEGION-209")); got != looks {
		t.Fatalf("look start rows = %d, want %d: no look starts while the child runs", got, looks)
	}
	if suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend"); len(suspends) != 0 {
		t.Fatalf("suspend rows = %+v, want none while the child is back in the workflow", suspends)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions = %+v, want none: the pod is the new run's", got)
	}
}

// A rerun's merge replaces the check that outlived its child's re-entry (openCheck): the earlier
// check, still open at look 1, closes first as not run, and the line of its close names the merge
// that replaced it (replacedBy) and comes before the line of the new check's open. The close ends
// the earlier look's task (one retire_look row of that look and check, unreported) and is a close
// of the check alone: the merge's own close queues the same claim closes, suspensions and status
// writes as it does for a twin whose earlier check is gone, so the replaced check's close queues
// no issue_close and no suspension of its own. The new check opens at the child's new generation.
func TestAReplacedOpenCheckClosesNotRunNamingTheMergeThatReplacedIt(t *testing.T) {
	t.Parallel()
	rerun := checkOpened.Add(24 * time.Hour)
	merge := func(t *testing.T, earlier bool) (checkRig, []string, int) {
		t.Helper()
		rig := childCheckRig(t)
		reenterChild(t, rig)
		if !earlier {
			if _, err := rig.pool.Exec(context.Background(), "delete from production_checks where issue = 'LEGION-209'"); err != nil {
				t.Fatal(err)
			}
		}
		after, retires := lastOutboxID(t, rig.pool), len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
		mergeRerun(t, rig, rerun)
		return rig, closeAndSuspendRows(t, rig.pool, after), retires
	}
	rig, rows, retires := merge(t, true)
	_, twin, _ := merge(t, false)
	if !slices.Equal(rows, twin) {
		t.Fatalf("rows the rerun's merge queued = %q, want those of a twin with no earlier check %q: the replaced check's close queues none of its own", rows, twin)
	}
	if got := superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look")[retires:]; len(got) != 1 || got[0].Role != claim.RoleTester ||
		got[0].CheckLook != 1 || !got[0].CheckOpenedAt.Equal(checkOpened) || got[0].Reported {
		t.Fatalf("retire_look rows of the merge = %+v, want one, unreported, of look 1 of the earlier check", got)
	}
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check == nil || !check.Open() || check.MergeSHA != "merge-44" || check.Number != 44 || !check.OpenedAt.Equal(rerun) ||
		check.OpenedGeneration != 8 || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check after the rerun's merge = %+v, want the check of merge-44 opened at %s at generation 8, at look 1", check, rerun)
	}
	closed, opened := -1, -1
	lines := strings.Split(rig.logged.String(), "\n")
	for i, line := range lines {
		switch {
		case strings.Contains(line, `msg="workflow: production check closed"`) && strings.Contains(line, "replacedBy="):
			closed = i
		case strings.Contains(line, `msg="workflow: production check opened"`) && strings.Contains(line, "mergeCommit=merge-44"):
			opened = i
		}
	}
	if closed < 0 || opened < 0 || closed > opened {
		t.Fatalf("the journal =\n%s\nwant the replaced check's close (line %d) before the new check's open (line %d)", rig.logged.String(), closed, opened)
	}
	for _, key := range []string{"issue=LEGION-209", "mergeCommit=merge-43", "outcome=not_run", "looks=1", "replacedBy=merge-44"} {
		if !strings.Contains(lines[closed], key) {
			t.Fatalf("the replaced check's close = %q, want %s", lines[closed], key)
		}
	}
}

// ranAgainLeave is one leave of an issue whose open production check was opened at an earlier
// generation than the issue's: setup builds that state, leave moves the issue out of the workflow,
// and issue is its key.
type ranAgainLeave struct {
	name  string
	issue string
	setup func(t *testing.T) checkRig
	leave func(t *testing.T, rig checkRig)
}

// ranAgainLeaves is every way an issue that ran again leaves the workflow with no merge of its new
// run: a re-entered child parked in backlog, icebox or triage, or set done with no merged pull
// request; a re-admitted root parked in backlog, set done with no merge, closed by close_root, and
// moved to done by a transition on no merge of its new run; and a child whose check an older binary
// opened, with no generation, which re-enters after the boot gave the check the child's generation
// (olderBinarysChildCheckRig) and is parked. The table's one transition to done is the merge's
// (TableOf), so the transition is a transition on the merge its check already checks.
func ranAgainLeaves() []ranAgainLeave {
	var leaves []ranAgainLeave
	for _, status := range []string{"backlog", "icebox", "triage", "done"} {
		leaves = append(leaves, ranAgainLeave{
			name: "a re-entered child to " + status, issue: "LEGION-209",
			setup: func(t *testing.T) checkRig {
				rig := childCheckRig(t)
				reenterChild(t, rig)
				return rig
			},
			leave: func(t *testing.T, rig checkRig) {
				rig.apply("leave", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: status, Title: "child", Parent: "LEGION-208", Rank: "V"})
			},
		})
	}
	for _, status := range []string{"backlog", "done"} {
		leaves = append(leaves, ranAgainLeave{
			name: "a re-admitted root to " + status, issue: "LEGION-208", setup: readmittedRootRig,
			leave: func(t *testing.T, rig checkRig) {
				rig.apply("leave", intake.DispatchIssue{Key: "LEGION-208", Seq: 3, Type: "issue.updated", Status: status, Title: "root", Rank: "U"})
			},
		})
	}
	return append(leaves,
		ranAgainLeave{
			name: "a re-entered child whose check an older binary opened, given its generation at the boot, to backlog", issue: "LEGION-209",
			setup: func(t *testing.T) checkRig {
				rig := olderBinarysChildCheckRig(t)
				reenterChild(t, rig)
				return rig
			},
			leave: func(t *testing.T, rig checkRig) {
				rig.apply("leave", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
			},
		},
		ranAgainLeave{
			name: "a re-admitted root closed by close_root", issue: "LEGION-208", setup: readmittedRootRig,
			leave: func(t *testing.T, rig checkRig) {
				if result := rig.apply("close-root", intake.CloseRoot{Issue: "LEGION-208", Reason: "moot"}); result.Refusal != nil {
					t.Fatalf("close_root of the re-admitted root = %+v, want it accepted", result.Refusal)
				}
			},
		},
		ranAgainLeave{
			name: "a re-admitted root moved to done by a transition on no merge of its new run", issue: "LEGION-208",
			setup: func(t *testing.T) checkRig {
				rig := readmittedRootRig(t)
				if _, err := rig.pool.Exec(context.Background(), "update issues set phase = 'merging', status = 'retro' where key = 'LEGION-208'"); err != nil {
					t.Fatal(err)
				}
				seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
				return rig
			},
			leave: func(t *testing.T, rig checkRig) {
				rig.apply("merged-again", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
				if got := issueAt(t, rig.pool, "LEGION-208"); got != phase.Done {
					t.Fatalf("the root after the merge = %s, want done by the merge's transition", got)
				}
			},
		},
	)
}

// An issue that ran again since its open check opened (its generation above the check's
// OpenedGeneration) and leaves the workflow with no merge of its new run closes that check as not
// run, at the leave, through each path a leave takes (ranAgainLeaves): the same check, its look
// ended, one retire_look row of its look, and a close line with no replacedBy. The controls close
// nothing: a done child a person parks and sets done again, also under a root reopened inside its
// linger, whose re-admission deleted the child's merged pull request and left its generation as it
// was, and also with a check an older binary opened, parked after the boot gave that check the
// child's generation (olderBinarysChildCheckRig); and a first merge, which opens the check at the
// issue's generation.
func TestAnIssueThatRanAgainAndLeftWithNoMergeClosesItsCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range ranAgainLeaves() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := tc.setup(t)
			before := productionCheck(t, rig.pool, tc.issue)
			if before == nil || !before.Open() || before.OpenedGeneration == 0 {
				t.Fatalf("the check before the leave = %+v, want one open with the generation it opened at", before)
			}
			retires := len(superviseRowsOf(t, rig.pool, tc.issue, "retire_look"))
			mark := rig.logged.Len()
			tc.leave(t, rig)
			check := productionCheck(t, rig.pool, tc.issue)
			if check == nil || check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen || !check.OpenedAt.Equal(before.OpenedAt) {
				t.Fatalf("the check after the leave = %+v, want the check opened at %s closed as not run", check, before.OpenedAt)
			}
			if got := superviseRowsOf(t, rig.pool, tc.issue, "retire_look")[retires:]; len(got) != 1 || got[0].CheckLook != before.Looks ||
				!got[0].CheckOpenedAt.Equal(before.OpenedAt) || got[0].Reported {
				t.Fatalf("retire_look rows of the leave = %+v, want one, unreported, of look %d of the check", got, before.Looks)
			}
			journal := rig.logged.String()[mark:]
			closes := checkCloseLines(journal, tc.issue)
			if len(closes) != 1 || !strings.Contains(closes[0], "outcome=not_run") || strings.Contains(closes[0], "replacedBy=") ||
				!strings.Contains(journal, `msg="workflow: production check of a run that left with no merge closes as not run" issue=`+tc.issue) {
				t.Fatalf("the journal of the leave =\n%s\nwant one close of %s's check as not run, naming no merge that replaced it", journal, tc.issue)
			}
		})
	}

	park := func(t *testing.T, rig checkRig) {
		t.Helper()
		rig.apply("parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 3, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
		// Admission, which runs after the engine on each event, records the parked status; the
		// rig's admission stub records nothing, so the row is moved by hand to what the daemon
		// would hold at the next event.
		if _, err := rig.pool.Exec(context.Background(), "update issues set status = 'backlog', dispatch_status = 'backlog', last_dispatch_seq = 3 where key = 'LEGION-209'"); err != nil {
			t.Fatal(err)
		}
		rig.apply("done-again", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"})
		if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/planner"] < 1 {
			t.Fatalf("issue_close rows = %v, want the second done to have left again", got)
		}
	}
	for _, tc := range []ranAgainLeave{
		{name: "a done child parked and set done again", setup: childCheckRig, leave: park},
		{name: "a done child whose check an older binary opened, parked after the boot", setup: olderBinarysChildCheckRig, leave: park},
		{
			name: "a done child of a root reopened inside its linger, parked and set done again",
			setup: func(t *testing.T) checkRig {
				rig := childCheckRig(t)
				rig.apply("root-closed", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
				// admit's readmit, as it leaves the tree: the root at its next generation in
				// admitted, its linger ended, and the merged pull requests of the whole tree deleted
				// with the generation (record.Store.ClearTreeGeneration); the done child keeps its own.
				if _, err := rig.pool.Exec(context.Background(), `update issues set generation = 8, phase = 'admitted', status = 'todo',
					dispatch_status = 'todo', linger_until = null, last_dispatch_seq = 3 where key = 'LEGION-208'`); err != nil {
					t.Fatal(err)
				}
				if _, err := rig.pool.Exec(context.Background(), "delete from pull_requests where issue in (select key from issues where tree = 'LEGION-208')"); err != nil {
					t.Fatal(err)
				}
				return rig
			},
			leave: park,
		},
		{
			name: "a first merge",
			setup: func(t *testing.T) checkRig {
				rig := newCheckRig(t)
				seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
				return rig
			},
			leave: func(t *testing.T, rig checkRig) {
				rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := tc.setup(t)
			before := productionCheck(t, rig.pool, "LEGION-209")
			retires := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look"))
			mark := rig.logged.Len()
			tc.leave(t, rig)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || !check.Open() || check.OpenedGeneration != 7 || before != nil && (!check.OpenedAt.Equal(before.OpenedAt) || check.Looks != before.Looks) {
				t.Fatalf("the check after the leave = %+v, want the check of generation 7 open as it was (%+v)", check, before)
			}
			if got := len(superviseRowsOf(t, rig.pool, "LEGION-209", "retire_look")); got != retires {
				t.Fatalf("retire_look rows = %d, want %d: the leave ended no look", got, retires)
			}
			if closes := checkCloseLines(rig.logged.String()[mark:], "LEGION-209"); len(closes) != 0 {
				t.Fatalf("check closes the leave logged = %q, want none", closes)
			}
		})
	}
}

// The close of a check at a leave (ranAgainLeaves) is a close of the check alone: the claim closes,
// suspensions and Dispatch status writes the leave queues are those the same leave queues for the
// same issue with no check. The close's own retire_look row is neither, and is left out.
func TestTheCloseOfACheckAtALeaveQueuesNothingOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range ranAgainLeaves() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leave := func(t *testing.T, check bool) []string {
				t.Helper()
				rig := tc.setup(t)
				if !check {
					if _, err := rig.pool.Exec(context.Background(), "delete from production_checks where issue = $1", tc.issue); err != nil {
						t.Fatal(err)
					}
				}
				after := lastOutboxID(t, rig.pool)
				tc.leave(t, rig)
				if got := productionCheck(t, rig.pool, tc.issue); check && (got == nil || got.Open()) {
					t.Fatalf("the check after the leave = %+v, want it closed", got)
				}
				return closeAndSuspendRows(t, rig.pool, after)
			}
			rows, twin := leave(t, true), leave(t, false)
			if len(rows) == 0 || !slices.Equal(rows, twin) {
				t.Fatalf("rows the leave queued = %q, want those it queues for the issue with no check %q", rows, twin)
			}
		})
	}
}

// The close of a lingering tree waits only on a check a tree waited on before a check outlived a
// child's re-entry (checksTheLingerWaitsOn). A child that re-entered and then merged holds the
// check of its new merge, which the close waits on; one that re-entered and then left closed its
// check at the leave, and the close waits on nothing; one still running when its root's close
// lingers the tree holds a check that the release before closed at the re-entry, and the close
// waits on nothing and leaves that check open. The control is a done child that never ran again,
// whose check the close waits on.
//
// What becomes of the running child's open check after the close is the close's own rule
// (unfinishedHandOvers). A root that stayed done hands the child over to a tree of its own: the
// check stays open under that tree, no look starts while the child runs, and the check ends at the
// child's merge (the replaced check closes not run, naming the merge), at its leave with no merge,
// or at its bound, one row each. A root a person parked hands over nothing of a child not at its
// merge, and closes the child's claims with the rest of the tree: no look starts, and the check
// closes not run at its bound, with no Dispatch status write, no claim close and no suspension of
// the child.
func TestALingeringTreeWaitsOnNoCheckThatARunClosed(t *testing.T) {
	t.Parallel()
	rerun := checkOpened.Add(24 * time.Hour)
	handedOver := func(end func(t *testing.T, rig checkRig)) func(t *testing.T, rig checkRig) {
		return func(t *testing.T, rig checkRig) {
			t.Helper()
			handOverReadmits(t, rig)
			if child, check := issueRecord(t, rig.pool, "LEGION-209"), productionCheck(t, rig.pool, "LEGION-209"); child == nil || child.Tree != "LEGION-209" ||
				check == nil || !check.Open() || !check.OpenedAt.Equal(checkOpened) || check.Looks != 1 {
				t.Fatalf("the handed-over child = %+v with its check %+v, want it a root of its own tree with its check of merge-43 open at look 1", child, check)
			}
			lookWaitsWhileTheChildRuns(t, rig)
			end(t, rig)
		}
	}
	for _, tc := range []struct {
		name    string
		reenter bool
		run     func(t *testing.T, rig checkRig)
		// moved is the status admission records for the person's move of the root, which the close
		// of the linger reads (unfinishedHandOvers); "" leaves the record as the engine wrote it.
		moved string
		// waits is the merge commit whose check the close waits on, "" for none.
		waits string
		// open says the child's check stays open through the close.
		open bool
		// handed says the close hands the child over to a tree of its own.
		handed bool
		// then is what follows the close.
		then func(t *testing.T, rig checkRig)
	}{
		{name: "a child that re-entered and then merged", reenter: true, waits: "merge-44", open: true,
			run: func(t *testing.T, rig checkRig) { mergeRerun(t, rig, rerun) }},
		{name: "a child that re-entered and then left", reenter: true, run: func(t *testing.T, rig checkRig) {
			rig.apply("parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
		}},
		{name: "a child that re-entered and still runs", reenter: true, open: true, run: func(*testing.T, checkRig) {}},
		{name: "a done child that never ran again", waits: "merge-43", open: true, run: func(*testing.T, checkRig) {}},
		{name: "a running child its done root hands over, whose check ends at its merge", reenter: true, moved: "done", open: true, handed: true,
			run: func(*testing.T, checkRig) {}, then: handedOver(func(t *testing.T, rig checkRig) {
				mark := rig.logged.Len()
				mergeRerun(t, rig, rerun)
				check := productionCheck(t, rig.pool, "LEGION-209")
				closes := checkCloseLines(rig.logged.String()[mark:], "LEGION-209")
				if check == nil || !check.Open() || check.MergeSHA != "merge-44" || check.OpenedGeneration != 9 || len(closes) != 1 ||
					!strings.Contains(closes[0], "mergeCommit=merge-43") || !strings.Contains(closes[0], "outcome=not_run") || !strings.Contains(closes[0], "replacedBy=merge-44") {
					t.Fatalf("the check after the handed child's merge = %+v with closes %q, want merge-43's closed not run, replaced by the open check of merge-44 at generation 9", check, closes)
				}
			})},
		{name: "a running child its done root hands over, whose check ends at its leave with no merge", reenter: true, moved: "done", open: true, handed: true,
			run: func(*testing.T, checkRig) {}, then: handedOver(func(t *testing.T, rig checkRig) {
				mark := rig.logged.Len()
				rig.apply("handed-parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 6, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V", HandedOver: true})
				checkClosedNotRun(t, rig)
				if !strings.Contains(rig.logged.String()[mark:], `msg="workflow: production check of a run that left with no merge closes as not run" issue=LEGION-209`) {
					t.Fatalf("the journal of the handed child's leave =\n%s\nwant the close of its check as a run that left with no merge", rig.logged.String()[mark:])
				}
			})},
		{name: "a running child its done root hands over, whose check ends at its bound", reenter: true, moved: "done", open: true, handed: true,
			run: func(*testing.T, checkRig) {}, then: handedOver(func(t *testing.T, rig checkRig) {
				checkBoundWhileTheChildRuns(t, rig)
			})},
		{name: "a running child its parked root's close hands nothing, whose check closes at its bound", reenter: true, moved: "backlog", open: true,
			run: func(*testing.T, checkRig) {}, then: func(t *testing.T, rig checkRig) {
				after := lastOutboxID(t, rig.pool)
				lookWaitsWhileTheChildRuns(t, rig)
				checkBoundWhileTheChildRuns(t, rig)
				for _, row := range closeAndSuspendRows(t, rig.pool, after) {
					if strings.HasPrefix(row, "LEGION-209 ") {
						t.Fatalf("rows of the child after the close = %q, want no Dispatch status write, no claim close and no suspension of it", row)
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := childCheckRig(t)
			if tc.reenter {
				reenterChild(t, rig)
			}
			tc.run(t, rig)
			if tc.moved == "backlog" {
				rig.apply("root-parked", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"})
			} else {
				rig.apply("root-closed", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
			}
			if tc.moved != "" {
				recordTheStatusAMoveShows(t, rig.pool, "LEGION-208", tc.moved)
			}
			rig.clock.now = rig.clock.now.Add(2 * time.Hour)
			_, err := intake.ApplyFact(context.Background(), rig.pool, "outbox", "linger:LEGION-208:7", intake.LingerExpired{Issue: "LEGION-208", Generation: 7}, rig.engine, admissionStub{})
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || check.Open() != tc.open {
				t.Fatalf("the child's check after the linger's expiry = %+v, want open %t", check, tc.open)
			}
			if tc.waits == "" {
				if err != nil {
					t.Fatalf("the linger's expiry = %v, want the tree closed", err)
				}
				closed := superviseRequests(t, rig.pool, "tree_close")
				for _, role := range claim.Roles {
					if closed["LEGION-209/"+string(role)] != 1 {
						t.Fatalf("tree_close rows = %v, want each of the child's claims closed once", closed)
					}
				}
				if got := handoverFences(t, rig.pool); slices.Equal(got, []string{"LEGION-209"}) != tc.handed || len(got) > 1 {
					t.Fatalf("hand-over fences = %v, want the child handed over %t", got, tc.handed)
				}
				if tc.then != nil {
					tc.then(t, rig)
				}
				return
			}
			if at, ok := wait.UntilAt(err); !errors.Is(err, wait.ErrWaiting) || !ok || check.MergeSHA != tc.waits || !at.Equal(check.BoundAt.Add(CheckLookBound)) ||
				!strings.Contains(err.Error(), "member LEGION-209's production check is open") {
				t.Fatalf("the linger's expiry = %v (until %s), want a wait on the check of %s to %s", err, at, tc.waits, check.BoundAt.Add(CheckLookBound))
			}
			if got := superviseRequests(t, rig.pool, "tree_close"); len(got) != 0 {
				t.Fatalf("tree_close rows = %v, want none while the close waits", got)
			}
		})
	}
}

// handOverReadmits is admit's readmit of the running child LEGION-209 that its done root's
// linger close handed to Legion, as the hand-over's own todo applies it (admit's
// consumeHandover): a root of its own tree at its next generation, 9, in admitted with status
// todo, handed over from LEGION-208, its hand-over fence gone and its tree running. Its check
// keeps the tree it opened in.
func handOverReadmits(t *testing.T, rig checkRig) {
	t.Helper()
	child := issueRecord(t, rig.pool, "LEGION-209")
	child.Tree, child.Generation, child.Phase = "LEGION-209", child.Generation+1, phase.Admitted
	child.Status, child.DispatchStatus, child.HandedOver, child.HandedOverFrom = "todo", "todo", true, "LEGION-208"
	child.LingerUntil, child.LastDispatchSeq = nil, 5
	seedIssue(t, rig.pool, *child)
	if _, err := rig.pool.Exec(context.Background(), "delete from handovers where issue = 'LEGION-209'"); err != nil {
		t.Fatal(err)
	}
	seedSlot(t, rig.pool, record.Slot{Issue: "LEGION-209", Index: 1})
}

// lookWaitsWhileTheChildRuns ends look 1 of LEGION-209's check at its bound and brings the wake
// that names due while the child runs: the look ends and names its wake, and the wake starts no
// look and is queued again.
func lookWaitsWhileTheChildRuns(t *testing.T, rig checkRig) {
	t.Helper()
	looks := len(lookStarts(t, rig.pool, "LEGION-209"))
	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check == nil || !check.Open() || check.Looks != 1 || check.LookOpen || check.WakeAt == nil {
		t.Fatalf("the check after its look's bound while the child runs = %+v, want open at look 1, the look ended and a wake named", check)
	}
	wake := *check.WakeAt
	rig.clock.now = wake
	rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, WakeAt: wake, Reason: "the retry of a look nobody reported: look bound"})
	if check = productionCheck(t, rig.pool, "LEGION-209"); !check.Open() || check.Looks != 1 || check.LookOpen || check.WakeAt == nil || !check.WakeAt.After(wake) {
		t.Fatalf("the check at its wake while the child runs = %+v, want look 1 still ended and the wake deferred past %s", check, wake)
	}
	if got := len(lookStarts(t, rig.pool, "LEGION-209")); got != looks {
		t.Fatalf("look start rows = %d, want %d: no look starts while the child runs", got, looks)
	}
}

// checkBoundWhileTheChildRuns brings LEGION-209's check to its bound while the child runs: it
// closes as not run, and its last look never starts.
func checkBoundWhileTheChildRuns(t *testing.T, rig checkRig) {
	t.Helper()
	looks := len(lookStarts(t, rig.pool, "LEGION-209"))
	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	checkClosedNotRun(t, rig)
	if got := len(lookStarts(t, rig.pool, "LEGION-209")); got != looks {
		t.Fatalf("look start rows = %d, want %d: the bound starts no last look while the child runs", got, looks)
	}
}

// checkClosedNotRun fails unless LEGION-209's check of merge-43 is closed as not run.
func checkClosedNotRun(t *testing.T, rig checkRig) {
	t.Helper()
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen ||
		!check.OpenedAt.Equal(checkOpened) || check.MergeSHA != "merge-43" {
		t.Fatalf("the child's check = %+v, want the check of merge-43 closed as not run", check)
	}
}

// reenterChild sets childCheckRig's done child LEGION-209 back to todo under its live root, which
// holds its slot: the child's new run is its generation 8, at admitted (ReenterChild).
func reenterChild(t *testing.T, rig checkRig) {
	t.Helper()
	seedSlot(t, rig.pool, record.Slot{Issue: "LEGION-208", Index: 0})
	rig.apply("todo", intake.DispatchIssue{Key: "LEGION-209", Seq: 3, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V"})
}

// mergeRerun is the new run of the re-entered child LEGION-209 reaching merging on
// sjawhar/legion#44 and GitHub merging it at merge-44 at at.
func mergeRerun(t *testing.T, rig checkRig, at time.Time) {
	t.Helper()
	rig.clock.now = at
	if _, err := rig.pool.Exec(context.Background(), "update issues set phase = 'merging', status = 'retro' where key = 'LEGION-209'"); err != nil {
		t.Fatal(err)
	}
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 44, Branch: "legion/LEGION-209", HeadSHA: "head-44", Failing: []string{}})
	rig.apply("rerun-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge-44"})
	if got := issueAt(t, rig.pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("the rerun after its merge = %s, want done", got)
	}
}

// readmittedRootRig is a root LEGION-208 with no issue under it, closed at generation 7 by its own
// merge of sjawhar/legion#42 at merge, which opened its check at look 1, and then reopened inside
// its linger as admit's readmit leaves it: at generation 8 in admitted with status todo and no
// linger, its merged pull request deleted with its generation (record.Store.ClearTreeGeneration),
// and its check open.
func readmittedRootRig(t *testing.T) checkRig {
	t.Helper()
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "U", LastDispatchSeq: 1})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "root-merger", HandoffCommit: "head"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || !check.Open() || check.OpenedGeneration != 7 {
		t.Fatalf("the root's check after its merge = %+v, want it open at generation 7", check)
	}
	if _, err := rig.pool.Exec(context.Background(), `update issues set generation = 8, phase = 'admitted', status = 'todo',
		dispatch_status = 'todo', linger_until = null, last_dispatch_seq = 2 where key = 'LEGION-208'`); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.pool.Exec(context.Background(), "delete from pull_requests where issue = 'LEGION-208'"); err != nil {
		t.Fatal(err)
	}
	return rig
}

// olderBinarysChildCheckRig is childCheckRig's done child LEGION-209 with its open check as a
// binary before dispatch://INFRA-3518 leaves it, with no generation and no look id, after this
// release's boot (Engine.RepairOpenChecks): the boot gives the check the child's generation, 7,
// and its open look 1 an id and a new task.
func olderBinarysChildCheckRig(t *testing.T) checkRig {
	t.Helper()
	rig := childCheckRig(t)
	if _, err := rig.pool.Exec(context.Background(), `update production_checks set opened_generation = null, look_id = null, look_id_look = null,
		look_id_opened_at = null where issue = 'LEGION-209'`); err != nil {
		t.Fatal(err)
	}
	rebooted(t, rig.pool)
	if looks, generations := repairOpenChecks(t, rig.pool, rig.engine); looks != 1 || generations != 1 {
		t.Fatalf("the boot gave %d looks an id and %d checks a generation, want the child's look and its check", looks, generations)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || check.OpenedGeneration != 7 || check.CurrentLookID() == "" {
		t.Fatalf("the child's check after the boot = %+v, want it open at generation 7 with an id of its look", check)
	}
	return rig
}

// lookStarts is issue's supervise start rows of a look of its production check, oldest first.
func lookStarts(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var looks []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, issue, "start") {
		if start.CheckLook > 0 {
			looks = append(looks, start)
		}
	}
	return looks
}

// lastOutboxID is the id of the newest outbox row, 0 for none.
func lastOutboxID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), "select coalesce(max(id), 0) from outbox").Scan(&id); err != nil {
		t.Fatalf("read the newest outbox row: %v", err)
	}
	return id
}

// closeAndSuspendRows is every row queued after the row after that closes or suspends a claim,
// suspends an issue's Sandbox or writes a Dispatch status: a supervise row of op issue_close or
// suspend, an issue_suspend row and a dispatch_status row, each as its issue, its kind and its
// payload, in queue order.
func closeAndSuspendRows(t *testing.T, pool *pgxpool.Pool, after int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue || ' ' || kind || ' ' || payload::text from outbox where id > $1
		and (kind in ('issue_suspend', 'dispatch_status') or (kind = 'supervise' and payload->>'op' in ('issue_close', 'suspend')))
		order by id`, after)
	if err != nil {
		t.Fatalf("read the close and suspend rows: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect the close and suspend rows: %v", err)
	}
	return got
}

// checkCloseLines is each line of journal that logs the close of issue's production check.
func checkCloseLines(journal, issue string) []string {
	var closes []string
	for _, line := range strings.Split(journal, "\n") {
		if strings.Contains(line, `msg="workflow: production check closed" issue=`+issue+" ") {
			closes = append(closes, line)
		}
	}
	return closes
}

// The boot's sweep closes a migrated issue that awaits a merge GitHub already reported, as the
// merge would have: a root with no running child, and an issue held from merging, whose
// hold the close clears, each with a check opened on no merge commit yet. A root with a child still
// in the workflow stays, and closes when that child leaves; an issue whose pull request is open
// stays as it is. The boot writes its "closes" line only where it closed, and no notice at all:
// the architect was told of the wait when the close entered it. Another project's merged issue is
// never read here, since the daemon's records hold no record of another project: that case is the
// daemon package's (TestTheBootSweepLeavesAMergedIssueOfAnotherProjectWhereItStands).
func TestTheBootSweepClosesMigratedMergedIssuesAndLeavesARootWithARunningChild(t *testing.T) {
	t.Parallel()
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-301", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-301", Project: "LEGION", Title: "alone", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "A"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-301", Repo: "sjawhar/legion", Number: 1, Branch: "legion/LEGION-301", HeadSHA: "h1", Failing: []string{}})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-302", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-302", Project: "LEGION", Title: "parent", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-302", Repo: "sjawhar/legion", Number: 2, Branch: "legion/LEGION-302", HeadSHA: "h2", MergeSHA: "m2", Failing: []string{}})
	parent := "LEGION-302"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-303", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-302", Project: "LEGION", Title: "running child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "C", LastDispatchSeq: 1})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-304", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-304", Project: "LEGION", Title: "held", Phase: phase.Held, Hold: &record.Hold{From: phase.Merging}, Generation: 1, Status: "retro", Rank: "D"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-304", Repo: "sjawhar/legion", Number: 4, Branch: "legion/LEGION-304", HeadSHA: "h4", Failing: []string{}})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-305", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-305", Project: "LEGION", Title: "open", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "E"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-305", Repo: "sjawhar/legion", Number: 5, Branch: "legion/LEGION-305", HeadSHA: "h5", Failing: []string{}})

	rig.apply("boot", intake.DaemonBooted{Boot: 1})
	phases := map[string]phase.Phase{}
	lingering := map[string]bool{}
	rows, err := rig.pool.Query(context.Background(), "select key, phase, linger_until is not null from issues")
	if err != nil {
		t.Fatalf("read issues: %v", err)
	}
	for rows.Next() {
		var key string
		var p phase.Phase
		var lingers bool
		if err := rows.Scan(&key, &p, &lingers); err != nil {
			t.Fatalf("scan issue: %v", err)
		}
		phases[key], lingering[key] = p, lingers
	}
	rows.Close()
	for key, want := range map[string]phase.Phase{"LEGION-301": phase.Done, "LEGION-302": phase.Merging, "LEGION-303": phase.Implementing, "LEGION-304": phase.Done, "LEGION-305": phase.Merging} {
		if phases[key] != want || lingering[key] != (want == phase.Done) {
			t.Fatalf("%s after the boot = %s lingering %t, want %s", key, phases[key], lingering[key], want)
		}
	}
	for _, key := range []string{"LEGION-301", "LEGION-304"} {
		if check := productionCheck(t, rig.pool, key); check == nil || !check.Open() || check.Looks != 1 || check.MergeSHA != "" {
			t.Fatalf("%s's check after the boot = %+v, want it open at look 1 with no merge commit yet", key, check)
		}
		if starts := superviseRowsOf(t, rig.pool, key, "start"); len(starts) != 1 || starts[0].CheckLook != 1 || !strings.Contains(starts[0].Task, "merge commit unknown, read it from the pull request on GitHub") {
			t.Fatalf("%s's starts = %+v, want its check's look 1 telling the agent to read the merge commit from GitHub", key, starts)
		}
	}
	var held *string
	if err := rig.pool.QueryRow(context.Background(), "select held_from from issues where key = 'LEGION-304'").Scan(&held); err != nil || held != nil {
		t.Fatalf("the held issue's hold after its close = %v, %v; want cleared", held, err)
	}
	for _, key := range []string{"LEGION-302", "LEGION-305"} {
		if check := productionCheck(t, rig.pool, key); check != nil {
			t.Fatalf("%s's check after the boot = %+v, want none", key, check)
		}
	}
	// The boot writes its "closes" line only where it closed, and tells no architect anything: a
	// restart must not repeat the notice the entry into the wait wrote, and a root can now wait for
	// days across many boots.
	for _, key := range []string{"LEGION-301", "LEGION-304"} {
		if !strings.Contains(rig.logged.String(), `upgrade" issue=`+key) {
			t.Fatalf("the boot's journal holds no close of %s:\n%s", key, rig.logged.String())
		}
	}
	if strings.Contains(rig.logged.String(), `upgrade" issue=LEGION-302`) {
		t.Fatalf("the boot logged a close of the root it left waiting:\n%s", rig.logged.String())
	}
	if got := noticeKinds(t, rig.pool, "LEGION-302"); len(got) != 0 {
		t.Fatalf("the waiting root's notices after the boot = %v, want none", got)
	}

	rig.apply("child-done", intake.DispatchIssue{Key: "LEGION-303", Seq: 2, Type: "issue.closed", Status: "done", Title: "running child", Parent: "LEGION-302", Rank: "C"})
	var rootPhase phase.Phase
	var rootLingers bool
	if err := rig.pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-302'").Scan(&rootPhase, &rootLingers); err != nil || rootPhase != phase.Done || !rootLingers {
		t.Fatalf("the parent once its last child left = %s lingering %t, %v; want done and lingering", rootPhase, rootLingers, err)
	}
	if check := productionCheck(t, rig.pool, "LEGION-302"); check == nil || !check.Open() || check.MergeSHA != "m2" {
		t.Fatalf("the parent's check = %+v, want it open on m2", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-303/implementer"] != 1 || got["LEGION-302/implementer"] != 0 {
		t.Fatalf("issue_close rows = %v, want the child's every claim closed and the parent's none", got)
	}
}

// The check task is held to the retired phrases the composed prompts are held to
// (internal/prompts' TestNoComposedPromptTeachesAnE2ELineOrAProofArray): what a look proves is a
// result recorded against the issue's after_merge check, so each of the three verdicts asks for
// `dispatch result` at the merge commit and none asks for the retired Production: line. The task
// is prose for a pane, so it carries the phrases without their backticks.
func TestTheCheckTaskAsksForARecordedResultAndNoRetiredLine(t *testing.T) {
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43", OpenedAt: checkOpened, Looks: 1}
	for _, last := range []bool{false, true} {
		task := checkTask(issue, check, checkTaskLookID, wakeMerge, "", last)
		for _, retired := range []string{"Production: line", "pending deploy of", "proof array"} {
			if strings.Contains(task, retired) {
				t.Errorf("the check task (last %t) still says %q", last, retired)
			}
		}
		for _, want := range []string{"merge commit merge-43", "dispatch result"} {
			if !strings.Contains(task, want) {
				t.Errorf("the check task (last %t) omits %q", last, want)
			}
		}
	}
}

// Each look of a check gets a look id of its own when it starts: crypto/rand's Text, 26 base32
// characters. The record holds the id with the look and the check instance it was made for, so
// the id is the check's CurrentLookID while its look is current, and the look's start row carries
// it, as the task on that row gives it. The next look of the same check gets another id. Neither
// id is a form of the check's OpenedAt, which every state and read_record body shows (R3 of
// dispatch://INFRA-3518): not RFC 3339, not the check's ID (its microseconds) and not its Unix
// seconds. The engine's journal carries neither id.
func TestEachLookGetsALookIDOfItsOwn(t *testing.T) {
	t.Parallel()
	rig := childCheckRig(t)
	first := productionCheck(t, rig.pool, "LEGION-209")
	wake := checkOpened.Add(time.Hour)
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy has not carried the merge", WakeAt: &wake})
	rig.clock.now = wake
	rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the deploy has not carried the merge"})
	second := productionCheck(t, rig.pool, "LEGION-209")
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if first == nil || second == nil || len(starts) != 2 {
		t.Fatalf("the check = %+v then %+v with start rows %+v, want two looks, each with its start row", first, second, starts)
	}
	base32 := regexp.MustCompile(`^[A-Z2-7]{26}$`)
	opened := first.OpenedAt
	forms := []string{opened.UTC().Format(time.RFC3339Nano), opened.Format(time.RFC3339Nano), first.ID(), strconv.FormatInt(opened.Unix(), 10)}
	for i, look := range []struct {
		check *record.ProductionCheck
		start record.SuperviseRequest
	}{{first, starts[0]}, {second, starts[1]}} {
		n, id := i+1, look.check.LookID
		if look.check.Looks != n || !base32.MatchString(id) || look.check.LookIDLook != n || !look.check.LookIDOpenedAt.Equal(look.check.OpenedAt) ||
			look.check.CurrentLookID() != id {
			t.Fatalf("the check at look %d = look %d with look id %q made for look %d of the check opened %s (current %q), want 26 base32 characters made for look %d of the check opened %s",
				n, look.check.Looks, id, look.check.LookIDLook, look.check.LookIDOpenedAt, look.check.CurrentLookID(), n, look.check.OpenedAt)
		}
		if look.start.CheckLook != n || look.start.LookID != id || !strings.Contains(look.start.Task, "look id `"+id+"`") {
			t.Fatalf("look %d's start row = look %d with look id %q, want the look id the check holds for it on the row and in its task", n, look.start.CheckLook, look.start.LookID)
		}
		for _, form := range forms {
			if strings.Contains(id, form) {
				t.Fatalf("look %d's look id %q holds %q, a form of the check's opening time", n, id, form)
			}
		}
		if strings.Contains(rig.logged.String(), id) {
			t.Fatalf("the engine's journal carries look %d's look id", n)
		}
	}
	if first.LookID == second.LookID || second.CurrentLookID() == first.LookID {
		t.Fatalf("look 2's look id is look 1's, want an id of its own")
	}
}

// outboxKindCount is how many outbox rows of kind issue has, settled or not.
func outboxKindCount(t *testing.T, pool *pgxpool.Pool, issue string, kind record.OutboxKind) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1 and kind = $2", issue, string(kind)).Scan(&count); err != nil {
		t.Fatalf("count outbox rows of kind %s for %s: %v", kind, issue, err)
	}
	return count
}
