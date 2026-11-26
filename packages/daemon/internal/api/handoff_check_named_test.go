package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// completeCheck writes the wire member even before the Go request acquires it, so the first
// run exercises readBody's refusal rather than a compiler error.
func completeCheck(h *harness, req HandoffCompleteRequest, opened time.Time) *httptest.ResponseRecorder {
	return h.request(http.MethodPost, "/legion/v1/handoff/complete", struct {
		HandoffCompleteRequest
		CheckOpenedAt time.Time `json:"checkOpenedAt"`
	}{req, opened}, nil)
}

func TestAStepsCompletionWithNoCheckCompletesTheStep(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
	c := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeTask(t, h, c.token, "outbox:testing-1", "test it", 1, 1)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: c.grant(t), Summary: "tests pass",
		Commit: headSHA, HandoffID: c.writeHandoff("test", 1), HandoffVersion: 1}))
	lastCompletion(t, facts)
}

func TestALookOfADoneIssueIsReportedByNamingItsCheck(t *testing.T) {
	testNamedLook(t, phase.Done)
}

func TestALookOfAnIssueThatIsNotDoneIsReportedByNamingItsCheck(t *testing.T) {
	testNamedLook(t, phase.Implementing)
}

func testNamedLook(t *testing.T, at phase.Phase) {
	t.Helper()
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", at)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	c := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	completion(t, completeCheck(h, HandoffCompleteRequest{GrantID: c.grant(t), Summary: "still checking", Verdict: "pending", WakeAt: &checkWakeAt}, checkOpenedAt))
	got := lastCheckReport(t, facts)
	if got.Look != 2 || got.Verdict != "pending" || !got.WakeAt.Equal(checkWakeAt) {
		t.Fatalf("report = %+v, want look 2 pending until %s", got, checkWakeAt)
	}
}

func TestADoneIssuesLookReportThatNamesNoCheckIsRefused(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	c := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: c.grant(t), Summary: "production passes", Verdict: "pass"}), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
	if !strings.Contains(message, "checkOpenedAt") {
		t.Fatalf("refusal = %q, want the missing check identity", message)
	}
	assertNoNamedFact(t, h, facts)
}

func assertNoNamedFact(t *testing.T, h *harness, facts *factRecorder) {
	t.Helper()
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none", got)
	}
	if got := checkEvents(t, h, "LEGION-208"); got != 0 {
		t.Fatalf("check events = %d, want none", got)
	}
}

func TestOneClaimsStepAndLookAreEachTheCallThatSaysSo(t *testing.T) {
	for _, order := range []string{"step then look", "look then step", "step queued during look", "unconfirmed look"} {
		t.Run(order, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			c := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			takeTask(t, h, c.token, "outbox:testing-1", "test it", 1, 1)
			m, _ := h.supervisor.Machine(c.token)
			step := func() {
				completion(t, h.complete(HandoffCompleteRequest{GrantID: c.grant(t), Summary: "tests pass",
					Commit: headSHA, HandoffID: c.writeHandoff("test", 1), HandoffVersion: 1}))
				lastCompletion(t, facts)
			}
			look := func() {
				completion(t, completeCheck(h, HandoffCompleteRequest{GrantID: c.grant(t), Summary: "still checking", Verdict: "pending", WakeAt: &checkWakeAt}, checkOpenedAt))
				if got := lastCheckReport(t, facts); got.Look != 2 || got.Verdict != "pending" {
					t.Fatalf("report = %+v, want look 2 pending", got)
				}
			}
			switch order {
			case "step then look":
				step()
				c.worksLook(t, 2, checkOpenedAt)
				look()
			case "look then step":
				c.worksLook(t, 2, checkOpenedAt)
				look()
				for _, event := range []supervise.Event{supervise.StreamTurnEnd{Claim: c.token}, supervise.StreamTurnStart{Claim: c.token}} {
					if err := m.Handle(t.Context(), event); err != nil {
						t.Fatal(err)
					}
				}
				step()
				for _, fact := range facts.recorded() {
					if report, ok := fact.(intake.CheckReport); ok && report.Verdict == "pass" {
						t.Fatalf("step became a report: %+v", report)
					}
				}
			case "step queued during look":
				c.worksLook(t, 2, checkOpenedAt)
				if err := m.Handle(t.Context(), supervise.RequestDeliver{Claim: c.token, ID: "outbox:testing-2", Task: "test again", Generation: 1, Phase: phase.Testing, Entry: 2}); err != nil {
					t.Fatal(err)
				}
				if p := m.Claim().Pending; p == nil || p.Phase != phase.Testing {
					t.Fatalf("pending = %+v, want testing", p)
				}
				look()
			case "unconfirmed look":
				if err := m.Handle(t.Context(), supervise.StreamTurnEnd{Claim: c.token}); err != nil {
					t.Fatal(err)
				}
				id := "outbox:look:2"
				if err := m.Handle(t.Context(), supervise.RequestDeliver{Claim: c.token, ID: id, Task: "check production", Generation: 1, CheckLook: supervise.CheckLook{Look: 2, OpenedAt: checkOpenedAt}}); err != nil {
					t.Fatal(err)
				}
				if err := m.Handle(t.Context(), supervise.PromptAcked{Claim: c.token, Generation: m.Claim().Generation, DeliveryID: id}); err != nil {
					t.Fatal(err)
				}
				refused(t, completeCheck(h, HandoffCompleteRequest{GrantID: c.grant(t), Summary: "passes", Verdict: "pass"}, checkOpenedAt), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
				assertNoNamedFact(t, h, facts)
			}
		})
	}
}

func TestAReportNamingNoOpenCheckOfThatTimeIsRefused(t *testing.T) {
	for _, which := range []string{"replaced", "wrong time", "closed", "absent", "step without a check"} {
		t.Run(which, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			at := phase.Done
			if which == "step without a check" {
				at = phase.Testing
			}
			seedIssueAt(t, h, "LEGION-208", at)
			if which == "replaced" || which == "wrong time" || which == "closed" {
				seedCheck(t, h, "LEGION-208", claim.RoleTester, which == "closed", false)
			}
			c := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			opened := checkOpenedAt
			if which == "replaced" {
				opened = opened.Add(-24 * time.Hour)
			}
			if which == "wrong time" {
				opened = opened.Add(time.Second)
			}
			if which == "step without a check" {
				seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
				takeTask(t, h, c.token, "outbox:testing-1", "test it", 1, 1)
			} else {
				c.worksLook(t, 2, opened)
			}
			message := refused(t, completeCheck(h, HandoffCompleteRequest{GrantID: c.grant(t), Summary: "passes", Verdict: "pass"}, opened), http.StatusConflict, "CHECK_NOT_OPEN")
			if which == "replaced" || which == "wrong time" {
				for _, detail := range []string{"production check is open", "check_opened_at", "check.openedAt", "read_record"} {
					if !strings.Contains(message, detail) {
						t.Fatalf("refusal = %q, want the open check and its recovery source %q", message, detail)
					}
				}
			} else if strings.Contains(message, "production check is open") {
				t.Fatalf("refusal = %q, but no check is open", message)
			}
			assertNoNamedFact(t, h, facts)
		})
	}
}

func TestANamedCheckReportRefusesPhaseOnlyFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  HandoffCompleteRequest
	}{
		{"commit", HandoffCompleteRequest{Commit: headSHA}},
		{"handoff id", HandoffCompleteRequest{HandoffID: "test-phase-1"}},
		{"handoff version", HandoffCompleteRequest{HandoffVersion: 1}},
		{"all phase fields", HandoffCompleteRequest{Commit: headSHA, HandoffID: "test-phase-1", HandoffVersion: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			c := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
			tc.req.GrantID, tc.req.Summary, tc.req.Verdict = c.grant(t), "production passes", "pass"
			refused(t, completeCheck(h, tc.req, checkOpenedAt), http.StatusBadRequest, "CHECK_PHASE_FIELDS_UNEXPECTED")
			assertNoNamedFact(t, h, facts)
		})
	}
}

func TestAReportNamingAnotherRolesCheckIsRefused(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleImplementer, false, false)
	c := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	refused(t, completeCheck(h, HandoffCompleteRequest{GrantID: c.grant(t), Summary: "passes", Verdict: "pass"}, checkOpenedAt), http.StatusConflict, "CHECK_ROLE_MISMATCH")
	assertNoNamedFact(t, h, facts)
}

func TestACallWithNoCheckAndNoStepOfARunningIssueIsRefused(t *testing.T) {
	testLateNamedCompletion(t, false, true, "HANDOFF_NOT_CURRENT_PHASE")
}

func TestAStartedStepsLateCompletionBesideItsRolesLookIsRecorded(t *testing.T) {
	testLateNamedCompletion(t, true, true, "")
}

func TestAStartedStepsLateCompletionWithNoRecordIsRefused(t *testing.T) {
	testLateNamedCompletion(t, true, false, "HANDOFF_RECORD_REQUIRED")
}

func testLateNamedCompletion(t *testing.T, started, hasRecord bool, want string) {
	t.Helper()
	h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()})
	seedIssueAt(t, h, "LEGION-208", phase.Reviewing)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	c := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	if started {
		if err := store.Transact(t.Context(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: c.issue, Role: claim.RoleTester, Claim: c.token, Entry: 1, EntryTask: "test this change"})
		}); err != nil {
			t.Fatal(err)
		}
		takeTask(t, h, c.token, "outbox:testing-1", "test this change", 1, 1)
	}
	c.worksLook(t, 2, checkOpenedAt)
	before := namedPhaseRow(t, h, c.issue)
	check := recordedCheck(t, h, c.issue)
	var issueBefore record.Issue
	if err := store.Transact(t.Context(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		i, err := record.NewStore().Issue(ctx, tx, c.issue)
		if i != nil {
			issueBefore = *i
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	req := HandoffCompleteRequest{GrantID: c.grant(t), Summary: "tests pass", Commit: headSHA}
	if hasRecord {
		req.HandoffID, req.HandoffVersion = c.writeHandoff("test", 1), 1
	}
	response := h.complete(req)
	if want != "" {
		refused(t, response, http.StatusConflict, want)
	} else {
		completion(t, response)
	}
	after := namedPhaseRow(t, h, c.issue)
	if want != "" {
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("refusal changed tester row:\nbefore %+v\nafter %+v", before, after)
		}
	} else if !after.Completed() || after.HandoffRecord != req.HandoffID || after.HandoffVersion != 1 || after.HandoffCommit != headSHA {
		t.Fatalf("late completion = %+v, want the fresh testing handoff", after)
	}
	if got := recordedCheck(t, h, c.issue); !reflect.DeepEqual(got, check) {
		t.Fatalf("check changed: %+v -> %+v", check, got)
	}
	if err := store.Transact(t.Context(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		i, err := record.NewStore().Issue(ctx, tx, c.issue)
		if i == nil || i.Phase != phase.Reviewing || (want != "" && !reflect.DeepEqual(*i, issueBefore)) {
			t.Fatalf("issue changed: %+v -> %+v", issueBefore, i)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts.recorded() {
		if _, ok := fact.(intake.CheckReport); ok {
			t.Fatalf("phase call applied check report: %#v", fact)
		}
	}
}

func namedPhaseRow(t *testing.T, h *harness, issue string) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	if err := store.Transact(t.Context(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(ctx, tx, issue)
		for _, r := range rows {
			if r.Role == claim.RoleTester {
				row = r
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}
