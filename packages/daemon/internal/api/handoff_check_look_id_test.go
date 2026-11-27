package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The route takes a report's look from the report's look id (R1 and R2 of dispatch://INFRA-3518):
// the look the id names is the check's current one, whatever the claim's turn is on, and a report
// whose id names no current look records nothing, from any turn. These tests drive the completion
// route over a real Postgres with the workflow deciding the report, so a recorded report is one
// the engine took and wrote on the check.

// lookIDHarness is the completion route with the workflow of a project whose production check is
// the tester's, deciding every fact the route applies, and the recorder of those facts.
func lookIDHarness(t *testing.T) (*harness, *factRecorder) {
	t.Helper()
	now := checkOpenedAt.Add(3 * time.Hour)
	h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, ProductionCheckRole: claim.RoleTester,
		ProductionCheckBound: 336 * time.Hour, Clock: func() time.Time { return now }})
	return h, facts
}

// reseedCheck writes key's production check again as change leaves the record's copy of it: a
// state that seedCheck's look 2 does not hold, as the workflow or an older daemon left it.
func reseedCheck(t *testing.T, h *harness, key string, change func(*record.ProductionCheck)) {
	t.Helper()
	check := recordedCheck(t, h, key)
	change(&check)
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(ctx, tx, check)
	}); err != nil {
		t.Fatalf("write the production check of %s again: %v", key, err)
	}
}

// readyClaimIn is the live claim of role on issue of tree, registered and ready, which the daemon
// handed no task: a claim that serves nothing and holds nothing.
func readyClaimIn(t *testing.T, h *harness, tree, issue string, role claim.Role) liveClaim {
	t.Helper()
	token, boot := h.launchIn(tree, issue, role)
	session := "ses_" + string(role) + "_" + issue
	registration := h.registered(boot, session)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: machine.Claim().Session}); err != nil {
		t.Fatalf("ready %s: %v", token, err)
	}
	return liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: tree, issue: issue}
}

// claimOf is the claim c's machine holds now.
func claimOf(t *testing.T, c liveClaim) supervise.Claim {
	t.Helper()
	machine, ok := c.h.supervisor.Machine(c.token)
	if !ok {
		t.Fatalf("no machine for %s", c.token)
	}
	return machine.Claim()
}

// A report that gives the current look's id is recorded on the current look, with the check it
// names, whatever the claim's turn is on and whatever it holds: a turn names no prompt, so no
// turn decides which look a report is of. That holds however the look ended and whether or not the
// issue runs again. Each row ends with the engine's record of the report: the check closed as
// recorded on look 2, its one check event processed.
func TestAReportGivingTheCurrentLooksIDIsRecordedWhateverTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// key is the issue whose check the tester reports on.
		key string
		// arrange records the issue and its check (seedCheck's, at look 2 with checkLookID), and
		// answers the tester's claim in the state the row is about.
		arrange func(t *testing.T, h *harness) liveClaim
	}{
		{name: "a claim that served its testing run", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			takeTask(t, h, tester.token, "outbox:testing-1", "Continue Workflow. Issue: LEGION-208. Phase: testing.", 1, 1)
			if c := claimOf(t, tester); c.ServingRun() != 1 || c.ServingCheck() != (supervise.CheckLook{}) {
				t.Fatalf("the tester serves run %d and look %+v, want its testing run and no look", c.ServingRun(), c.ServingCheck())
			}
			return tester
		}},
		{name: "a claim that served nothing", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := readyClaimIn(t, h, "LEGION-208", "LEGION-208", claim.RoleTester)
			if c := claimOf(t, tester); c.Pending != nil || c.ServingRun() != 0 || c.ServingCheck() != (supervise.CheckLook{}) {
				t.Fatalf("the tester holds %+v and serves run %d and look %+v, want nothing", c.Pending, c.ServingRun(), c.ServingCheck())
			}
			return tester
		}},
		{name: "a confirmed look task", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
			if p := claimOf(t, tester).Pending; p == nil || p.ConfirmedAt.IsZero() || p.LookID != checkLookID {
				t.Fatalf("the tester holds %+v, want look 2's task confirmed", p)
			}
			return tester
		}},
		{name: "an unconfirmed look task", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			machine, _ := h.supervisor.Machine(tester.token)
			ctx := context.Background()
			if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
				t.Fatalf("end the tester's turn: %v", err)
			}
			id := "outbox:look:2"
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, ID: id, Task: "Production check of LEGION-208.", Generation: 1,
				CheckLook: supervise.CheckLook{Look: 2, OpenedAt: checkOpenedAt}, LookID: checkLookID}); err != nil {
				t.Fatalf("deliver look 2 to the tester: %v", err)
			}
			if err := machine.Handle(ctx, supervise.PromptAcked{Claim: tester.token, Generation: machine.Claim().Generation, DeliveryID: id}); err != nil {
				t.Fatalf("acknowledge look 2's prompt: %v", err)
			}
			if c := claimOf(t, tester); c.Pending == nil || c.Pending.DeliveredAt.IsZero() || !c.Pending.ConfirmedAt.IsZero() || c.ServingCheck() != (supervise.CheckLook{}) {
				t.Fatalf("the tester holds %+v and serves look %+v, want look 2's task read, unconfirmed, and no look served", c.Pending, c.ServingCheck())
			}
			return tester
		}},
		{name: "no pending task", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
			machine, _ := h.supervisor.Machine(tester.token)
			if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
				t.Fatalf("end look 2's turn: %v", err)
			}
			if p := claimOf(t, tester).Pending; p != nil {
				t.Fatalf("the tester holds %+v, want no task", p)
			}
			return tester
		}},
		{name: "a look ended by its bound", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			reseedCheck(t, h, "LEGION-208", func(check *record.ProductionCheck) {
				wake := check.LookWakeAt.Add(workflow.CheckLookBound + workflow.CheckLookBound)
				check.LookOpen, check.WakeAt = false, &wake
			})
			return newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
		}},
		{name: "a root re-admitted", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
				Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 2, Status: "in_progress"})
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			return newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
		}},
		{name: "a child re-entered", key: "LEGION-209", arrange: func(t *testing.T, h *harness) liveClaim {
			// The child is back in the workflow at a later run with its check still open: the
			// record a re-entry that keeps the check leaves, seeded here as it stands.
			h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
				Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			parent := "LEGION-208"
			h.recordIssue(record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
				Parent: &parent, Project: testProject, Title: "LEGION-209", Phase: phase.Implementing, Generation: 2, Status: "in_progress"})
			seedCheck(t, h, "LEGION-209", claim.RoleTester, false, false)
			reseedCheck(t, h, "LEGION-209", func(check *record.ProductionCheck) { check.Tree = "LEGION-208" })
			// The tester's turn is on a task of the child's new run.
			return newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleTester)
		}},
		{name: "the claim holds the testing task of a new run", key: "LEGION-208", arrange: func(t *testing.T, h *harness) liveClaim {
			h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
				Project: testProject, Title: "LEGION-208", Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
			takeTask(t, h, tester.token, "outbox:testing-2", "Continue Workflow. Issue: LEGION-208. Phase: testing.", 1, 2)
			if c := claimOf(t, tester); c.Pending == nil || c.Pending.Generation != 2 || c.Pending.ConfirmedAt.IsZero() || c.ServingRun() != 2 {
				t.Fatalf("the tester holds %+v serving run %d, want the new run's testing task confirmed", c.Pending, c.ServingRun())
			}
			return tester
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := lookIDHarness(t)
			tester := tc.arrange(t, h)

			completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "driven in production", Verdict: "pass",
				CheckOpenedAt: &checkOpenedAt, LookID: checkLookID}))
			want := intake.CheckReport{Issue: tc.key, Claim: tester.token, Look: 2, OpenedAt: checkOpenedAt, Verdict: "pass", Summary: "driven in production"}
			if got := lastCheckReport(t, facts); !sameCheckReport(got, want) {
				t.Fatalf("the report = %+v, want %+v: look 2, the look its id names", got, want)
			}
			if check := recordedCheck(t, h, tc.key); check.Open() || check.Outcome != record.CheckRecorded || check.Looks != 2 {
				t.Fatalf("the check after the report = %+v, want it closed as recorded on look 2", check)
			}
			if got := checkEvents(t, h, tc.key); got != 1 {
				t.Fatalf("processed check events = %d, want the report's one", got)
			}
		})
	}
}

// A report that does not give the current look's id records nothing: no id, a made-up id, the id
// of a look that a later look replaced, the id of a check that a later merge replaced (whether the
// report names that check or the one that replaced it), and any id while the current look has no
// id of its own yet. Each answer is refused before any fact: the check is unchanged and no report
// reached the workflow. Each answer names the current look, look 2, and the rule that a report
// gives check_opened_at and look_id from the task of its look, holds no look id, and is not
// CHECK_LOOK_NOT_SERVED.
func TestAReportWithoutTheCurrentLooksIDRecordsNothing(t *testing.T) {
	const (
		madeUpLookID   = "MADEUPLOOKIDOFNOLOOKABCDEF"
		earlierLookID  = "EARLIERLOOKONEIDABCDEFGHIJ"
		replacedLookID = "REPLACEDCHECKLOOKTWOABCDEF"
	)
	replacedOpenedAt := checkOpenedAt.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name string
		// change writes the record the row is about over seedCheck's look 2 with checkLookID.
		change func(*record.ProductionCheck)
		// openedAt is the check the report names, and lookID the id it gives.
		openedAt time.Time
		lookID   string
		status   int
		code     string
	}{
		{name: "no id", openedAt: checkOpenedAt, status: http.StatusBadRequest, code: "CHECK_LOOK_ID_REQUIRED"},
		{name: "a made-up id of 26 characters", openedAt: checkOpenedAt, lookID: madeUpLookID, status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN"},
		// Look 1 had earlierLookID, and look 2's start gave look 2 checkLookID.
		{name: "the id of a look that a later look replaced", openedAt: checkOpenedAt, lookID: earlierLookID, status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN"},
		// The check opened a day earlier had replacedLookID on its look 2, and the check that
		// replaced it is the open one.
		{name: "the id of a check that a later merge replaced, naming the check that replaced it", openedAt: checkOpenedAt, lookID: replacedLookID,
			status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN"},
		{name: "the id of a check that a later merge replaced, naming that check", openedAt: replacedOpenedAt, lookID: replacedLookID,
			status: http.StatusConflict, code: "CHECK_NOT_OPEN"},
		{name: "a current look with no id yet", openedAt: checkOpenedAt, lookID: madeUpLookID, status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN",
			change: func(check *record.ProductionCheck) {
				check.LookID, check.LookIDLook, check.LookIDOpenedAt = "", 0, time.Time{}
			}},
		{name: "a current look with no id yet, and the id an older daemon left for the look before", openedAt: checkOpenedAt, lookID: earlierLookID,
			status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN",
			change: func(check *record.ProductionCheck) { check.LookID, check.LookIDLook = earlierLookID, 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := lookIDHarness(t)
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			if tc.change != nil {
				reseedCheck(t, h, "LEGION-208", tc.change)
			}
			tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
			before := recordedCheck(t, h, "LEGION-208")

			opened := tc.openedAt
			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "driven in production", Verdict: "pass",
				CheckOpenedAt: &opened, LookID: tc.lookID}), tc.status, tc.code)
			for _, want := range []string{"look 2", lookIDRule} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal is %q, want it to say %q: the current look and the rule", message, want)
				}
			}
			if strings.Contains(message, "works no look") {
				t.Fatalf("the refusal is %q, want no \"works no look\": the route reads no turn", message)
			}
			for _, id := range []string{checkLookID, madeUpLookID, earlierLookID, replacedLookID} {
				if strings.Contains(message, id) {
					t.Fatalf("the refusal is %q, which holds the look id %q", message, id)
				}
			}
			if after := recordedCheck(t, h, "LEGION-208"); !reflect.DeepEqual(after, before) {
				t.Fatalf("the check after the refusal = %+v, want it unchanged: %+v", after, before)
			}
			for _, fact := range facts.recorded() {
				if report, ok := fact.(intake.CheckReport); ok {
					t.Fatalf("the refused report reached the workflow as %+v", report)
				}
			}
			if got := checkEvents(t, h, "LEGION-208"); got != 0 {
				t.Fatalf("processed check events = %d, want none", got)
			}
		})
	}
}

// A tester whose testing turn runs behind a merged close reads check.openedAt from the record, as
// read_record serves it (CheckView.OpenedAt), and sends its testing verdict as a report on the
// check: with checkOpenedAt and no look id, and with a look id it made up. Neither is a pass of
// the production check: each is refused, nothing is recorded, and the check stays open at look 1
// with its look open. Control: the report that gives the look id the look's task gives is
// recorded.
func TestANamedReportFromATestingTurnWithNoLookIDIsNoProductionPass(t *testing.T) {
	h, facts, tester, _ := mergedCloseBehindATestingTurn(t)
	opened := recordedCheck(t, h, "LEGION-209").OpenedAt
	for _, tc := range []struct {
		lookID string
		status int
		code   string
	}{
		{status: http.StatusBadRequest, code: "CHECK_LOOK_ID_REQUIRED"},
		{lookID: "MADEUPLOOKIDOFNOLOOKABCDEF", status: http.StatusConflict, code: "CHECK_LOOK_NOT_OPEN"},
	} {
		message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
			CheckOpenedAt: &opened, LookID: tc.lookID}), tc.status, tc.code)
		if !strings.Contains(message, "look 1") || !strings.Contains(message, lookIDRule) {
			t.Fatalf("the refusal is %q, want it to name look 1 and the rule", message)
		}
		if strings.Contains(message, "works no look") || strings.Contains(message, recordedCheck(t, h, "LEGION-209").LookID) {
			t.Fatalf("the refusal is %q, want no \"works no look\" and no look id", message)
		}
		theCheckIsUntouched(t, h, facts)
	}

	tester.worksLook(t, 1, opened)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "driven in production", Verdict: "pass",
		CheckOpenedAt: &opened, LookID: recordedCheck(t, h, "LEGION-209").LookID}))
	if check := recordedCheck(t, h, "LEGION-209"); check.Open() || check.Outcome != record.CheckRecorded {
		t.Fatalf("the check after the report with its look id = %+v, want it closed as recorded", check)
	}
}

// A look id is a check report's alone: a phase completion that gives one and no checkOpenedAt is
// refused CHECK_LOOK_ID_UNEXPECTED, before any fact, on an issue that runs and on a done issue,
// whose completion with no check names would otherwise be CHECK_LOOK_NOT_SERVED. The look id it
// gives is the current look's, so the refusal holds even for the one id a report may give, and
// the answer does not echo it. Control: the same phase completion without the look id is taken.
func TestAPhaseCompletionCarryingALookIDIsRefused(t *testing.T) {
	for _, at := range []phase.Phase{phase.Testing, phase.Done} {
		t.Run(string(at), func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", at)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			before := recordedCheck(t, h, "LEGION-208")

			req := HandoffCompleteRequest{Summary: "tests pass", Verdict: "pass"}
			if at == phase.Testing {
				req.HandoffID, req.HandoffVersion = tester.writeHandoff("test", 1), 1
			}
			withID := req
			withID.GrantID, withID.LookID = tester.grant(t), checkLookID
			message := refused(t, h.complete(withID), http.StatusBadRequest, "CHECK_LOOK_ID_UNEXPECTED")
			for _, want := range []string{"lookId", "checkOpenedAt", "Nothing was recorded", lookIDRule} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal is %q, want it to say %q", message, want)
				}
			}
			if strings.Contains(message, checkLookID) {
				t.Fatalf("the refusal is %q, which holds the look id", message)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none: the refusal is written before any fact is applied", got)
			}
			if after := recordedCheck(t, h, "LEGION-208"); !reflect.DeepEqual(after, before) {
				t.Fatalf("the check after the refusal = %+v, want it unchanged: %+v", after, before)
			}
			if got := checkEvents(t, h, "LEGION-208"); got != 0 {
				t.Fatalf("processed check events = %d, want none", got)
			}

			if at == phase.Testing {
				req.GrantID = tester.grant(t)
				completion(t, h.complete(req))
				if got := facts.recorded(); len(got) != 1 {
					t.Fatalf("facts after the completion without the look id = %#v, want the one completion", got)
				}
				lastCompletion(t, facts)
			}
		})
	}
}
