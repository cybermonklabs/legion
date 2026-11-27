package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// defectHead is the head of the pull request a role saw a defect at: 40 lowercase hex.
const defectHead = "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90"

// defectText is the defect these tests report.
const defectText = "The retry never stops: internal/retry/loop.go:42 loops after the budget ends."

// reportDefectAs posts report_defect with grant, naming issue, text and head, and check where it
// is not empty.
func reportDefectAs(h *harness, grant, issue, text, head, check string) *httptest.ResponseRecorder {
	body := map[string]any{"grantId": grant, "issue": issue, "defect": text, "head": head}
	if check != "" {
		body["check"] = check
	}
	return h.request(http.MethodPost, "/legion/v1/defects/report", body, nil)
}

// withdrawDefectAs posts withdraw_defect with grant, naming issue, the defect's id and reason.
func withdrawDefectAs(h *harness, grant, issue, defect, reason string) *httptest.ResponseRecorder {
	return h.request(http.MethodPost, "/legion/v1/defects/withdraw",
		map[string]any{"grantId": grant, "issue": issue, "defect": defect, "reason": reason}, nil)
}

// defectAnswer is what report_defect and withdraw_defect answer, read field by field.
type defectAnswer struct {
	Defect     string  `json:"defect"`
	State      string  `json:"state"`
	NextChange *uint64 `json:"nextChange"`
}

// answeredDefect holds recorder to a 200 and reads its answer.
func answeredDefect(t *testing.T, recorder *httptest.ResponseRecorder) defectAnswer {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer defectAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body, err)
	}
	return answer
}

// setIssueColumns runs `update issues set <set> where key = $1` with args after the key: the
// record written as a test needs it, past the workflow, as acceptPhase does.
func setIssueColumns(t *testing.T, h *harness, key, set string, args ...any) {
	t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(), "update issues set "+set+" where key = $1", append([]any{key}, args...)...); err != nil {
		t.Fatalf("set %s of %s: %v", set, key, err)
	}
}

// issuePhaseOf is where key's record stands now.
func issuePhaseOf(t *testing.T, h *harness, key string) phase.Phase {
	t.Helper()
	var at phase.Phase
	if err := h.store.Pool().QueryRow(context.Background(), "select phase from issues where key = $1", key).Scan(&at); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return at
}

// defectRow is one row of defects, as these tests read it.
type defectRow struct {
	id         int64
	generation int64
	kind       string
	text       string
	role       string
	head       string
	check      string
	withdrawn  bool
	reason     string
}

// defectRows is every defect of issue, oldest first.
func defectRows(t *testing.T, h *harness, issue string) []defectRow {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), `select id, generation, kind, text, role, head, check_block,
		withdrawn_at is not null, coalesce(withdraw_reason, '') from defects where issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the defects of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []defectRow
	for rows.Next() {
		var row defectRow
		if err := rows.Scan(&row.id, &row.generation, &row.kind, &row.text, &row.role, &row.head, &row.check, &row.withdrawn, &row.reason); err != nil {
			t.Fatalf("scan a defect of %s: %v", issue, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the defects of %s: %v", issue, err)
	}
	return got
}

// seedDefect records defect on its issue through the record layer and answers its id: a reason
// the daemon read (a check's, a production check's) or a defect another call reported.
func seedDefect(t *testing.T, h *harness, defect record.Defect) int64 {
	t.Helper()
	var id int64
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = record.NewStore().PutDefect(ctx, tx, defect)
		return err
	}); err != nil {
		t.Fatalf("seed a defect of %s: %v", defect.Issue, err)
	}
	return id
}

// noticesOfKindOn counts the notices of kind queued on issue.
func noticesOfKindOn(t *testing.T, h *harness, issue, kind string) int {
	t.Helper()
	var count int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = $2", issue, kind).Scan(&count); err != nil {
		t.Fatalf("count the %s notices of %s: %v", kind, issue, err)
	}
	return count
}

// storedWaitOn reads the stored wait of issue (issues.close_wait), the zero wait where none is stored.
func storedWaitOn(t *testing.T, h *harness, issue string) record.StoredWait {
	t.Helper()
	var encoded []byte
	if err := h.store.Pool().QueryRow(context.Background(), "select close_wait from issues where key = $1", issue).Scan(&encoded); err != nil {
		t.Fatalf("read the stored wait of %s: %v", issue, err)
	}
	var wait record.StoredWait
	if len(encoded) == 0 {
		return wait
	}
	if err := json.Unmarshal(encoded, &wait); err != nil {
		t.Fatalf("decode the stored wait of %s: %v", issue, err)
	}
	return wait
}

// report_defect answers each case of the shared contract (dispatch://INFRA-5580/artifact/plan-md,
// "Shared contract with the texts piece"): before the last step of the task it is refused, and
// after it the call records the defect and moves no phase. Two reports of one text are one defect,
// and a report naming a check that is already a reason joins it. Every refusal records nothing.
func TestReportDefectAnswersEachCase(t *testing.T) {
	t.Parallel()

	t.Run("the implementer in retro records the defect and moves no phase", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		acceptPhase(t, h, statusRoot, phase.Retro)
		implementer := newLiveClaim(t, h, statusRoot, claim.RoleImplementer)
		answer := answeredDefect(t, reportDefectAs(h, implementer.grant(t), statusRoot, defectText, defectHead, ""))
		if answer.State != "recorded" || answer.Defect == "" || answer.NextChange == nil || *answer.NextChange != 2 {
			t.Fatalf("answer = %+v, want state recorded, an id and nextChange 2", answer)
		}
		if at := issuePhaseOf(t, h, statusRoot); at != phase.Retro {
			t.Fatalf("%s after the report stands in %s, want retro: the call moves no phase", statusRoot, at)
		}
		rows := defectRows(t, h, statusRoot)
		if len(rows) != 1 || strconv.FormatInt(rows[0].id, 10) != answer.Defect || rows[0].kind != "defect" || rows[0].text != defectText ||
			rows[0].role != "implementer" || rows[0].head != defectHead || rows[0].generation != 1 {
			t.Fatalf("defects = %+v, want the one defect %s of generation 1 the implementer reported at %s", rows, answer.Defect, defectHead)
		}
		if got := outboxRows(t, h, statusRoot)["close_wait_publish"]; got != 1 {
			t.Fatalf("close_wait_publish rows = %d, want 1: close-wait.md names the defect", got)
		}
		if got := noticesOfKindOn(t, h, statusRoot, "defect-reported"); got != 1 {
			t.Fatalf("defect-reported notices = %d, want 1 to the owning architect", got)
		}
		wait := storedWaitOn(t, h, statusRoot)
		if len(wait.Defects) != 1 || strconv.FormatInt(wait.Defects[0].ID, 10) != answer.Defect || wait.Defects[0].Role != claim.RoleImplementer || wait.Defects[0].Head != defectHead {
			t.Fatalf("the stored wait's defects = %+v, want defect %s with its role and head", wait.Defects, answer.Defect)
		}

		again := answeredDefect(t, reportDefectAs(h, implementer.grant(t), statusRoot, "  The retry never stops:\n internal/retry/loop.go:42   loops after the budget ends.  ", defectHead, ""))
		if again.State != "same" || again.Defect != answer.Defect {
			t.Fatalf("the second report of the text = %+v, want state same naming %s", again, answer.Defect)
		}
		if rows := defectRows(t, h, statusRoot); len(rows) != 1 {
			t.Fatalf("defects after the second report = %+v, want still one", rows)
		}
	})

	t.Run("the merger in merging records the defect", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		acceptPhase(t, h, statusRoot, phase.Merging)
		merger := newLiveClaim(t, h, statusRoot, claim.RoleMerger)
		if answer := answeredDefect(t, reportDefectAs(h, merger.grant(t), statusRoot, defectText, defectHead, "")); answer.State != "recorded" {
			t.Fatalf("answer = %+v, want recorded", answer)
		}
		if at := issuePhaseOf(t, h, statusRoot); at != phase.Merging {
			t.Fatalf("%s after the report stands in %s, want merging", statusRoot, at)
		}
	})

	t.Run("the merger in verifying before its completion lands records the defect", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		setIssueColumns(t, h, statusRoot, "phase = 'verifying', status = 'testing', merger_completion_owed = true")
		merger := newLiveClaim(t, h, statusRoot, claim.RoleMerger)
		if answer := answeredDefect(t, reportDefectAs(h, merger.grant(t), statusRoot, defectText, defectHead, "")); answer.State != "recorded" {
			t.Fatalf("answer = %+v, want recorded", answer)
		}
		if at := issuePhaseOf(t, h, statusRoot); at != phase.Verifying {
			t.Fatalf("%s after the report stands in %s, want verifying", statusRoot, at)
		}
		if wait := storedWaitOn(t, h, statusRoot); len(wait.Defects) != 1 {
			t.Fatalf("the stored wait's defects = %+v, want the one reported", wait.Defects)
		}
	})

	t.Run("the merger in verifying after its completion landed is refused", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		setIssueColumns(t, h, statusRoot, "phase = 'verifying', status = 'testing', merger_completion_owed = false")
		merger := newLiveClaim(t, h, statusRoot, claim.RoleMerger)
		refused(t, reportDefectAs(h, merger.grant(t), statusRoot, defectText, defectHead, ""), http.StatusForbidden, "DEFECT_CALLER_NOT_ALLOWED")
		if rows := defectRows(t, h, statusRoot); len(rows) != 0 {
			t.Fatalf("defects = %+v, want none", rows)
		}
	})

	t.Run("the owning architect's report naming a check that is a reason joins it", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		setIssueColumns(t, h, statusRoot, "phase = 'verifying', status = 'testing'")
		reason := seedDefect(t, h, record.Defect{Issue: statusRoot, Generation: 1, Kind: record.DefectKindCheck, Text: "check c-done failed",
			Check: "c-done", Result: "result-7", ReportedAt: time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)})
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		answer := answeredDefect(t, reportDefectAs(h, architect.grant(t), statusRoot, defectText, defectHead, "c-done"))
		if answer.State != "joined" || answer.Defect != strconv.FormatInt(reason, 10) {
			t.Fatalf("answer = %+v, want state joined naming the reason %d", answer, reason)
		}
		if rows := defectRows(t, h, statusRoot); len(rows) != 1 {
			t.Fatalf("defects = %+v, want the one reason", rows)
		}
	})

	t.Run("the owning architect of a parent held before its retro records the defect", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		setIssueColumns(t, h, statusRoot, "phase = 'held', held_from = 'retro', hold_reason = $2", string(record.HoldForChildren))
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		if answer := answeredDefect(t, reportDefectAs(h, architect.grant(t), statusRoot, defectText, defectHead, "")); answer.State != "recorded" {
			t.Fatalf("answer = %+v, want recorded", answer)
		}
		if got := noticesOfKindOn(t, h, statusRoot, "defect-reported"); got != 0 {
			t.Fatalf("defect-reported notices = %d, want none: the architect reported it itself", got)
		}
	})

	for _, tc := range []struct {
		name   string
		at     phase.Phase
		role   claim.Role
		status int
		code   string
	}{
		{"the owning architect in accepting", phase.Accepting, claim.RoleArchitect, http.StatusConflict, "DEFECT_BEFORE_LAST_STEP"},
		{"the implementer in implementing", phase.Implementing, claim.RoleImplementer, http.StatusConflict, "DEFECT_BEFORE_LAST_STEP"},
		{"the tester in retro", phase.Retro, claim.RoleTester, http.StatusForbidden, "DEFECT_CALLER_NOT_ALLOWED"},
		{"the implementer in merging", phase.Merging, claim.RoleImplementer, http.StatusForbidden, "DEFECT_CALLER_NOT_ALLOWED"},
	} {
		t.Run(tc.name+" is refused "+tc.code, func(t *testing.T) {
			t.Parallel()
			h, _, _ := statusHarness(t)
			acceptPhase(t, h, statusRoot, tc.at)
			caller := newLiveClaim(t, h, statusRoot, tc.role)
			refused(t, reportDefectAs(h, caller.grant(t), statusRoot, defectText, defectHead, ""), tc.status, tc.code)
			if rows := defectRows(t, h, statusRoot); len(rows) != 0 {
				t.Fatalf("defects = %+v, want none", rows)
			}
			if at := issuePhaseOf(t, h, statusRoot); at != tc.at {
				t.Fatalf("%s after the refusal stands in %s, want %s", statusRoot, at, tc.at)
			}
		})
	}

	for _, tc := range []struct {
		name string
		text string
		head string
		code string
	}{
		{"a blank defect", " \n\t ", defectHead, "DEFECT_TEXT_REQUIRED"},
		{"a defect of more blocks than Dispatch holds a body to", strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n"), defectHead, "DEFECT_TEXT_TOO_LONG"},
		{"a defect holding a decision block", "The retry never stops.\n\n" + theDecisionBlock + "\n", defectHead, "DEFECT_TEXT_DECISION_BLOCK"},
		{"no head", defectText, "", "DEFECT_HEAD_REQUIRED"},
		{"a short head", defectText, "4d2f6a0", "DEFECT_HEAD_REQUIRED"},
		{"an uppercase head", defectText, strings.ToUpper(defectHead), "DEFECT_HEAD_REQUIRED"},
	} {
		t.Run(tc.name+" is refused "+tc.code, func(t *testing.T) {
			t.Parallel()
			h, _, _ := statusHarness(t)
			acceptPhase(t, h, statusRoot, phase.Retro)
			implementer := newLiveClaim(t, h, statusRoot, claim.RoleImplementer)
			refused(t, reportDefectAs(h, implementer.grant(t), statusRoot, tc.text, tc.head, ""), http.StatusBadRequest, tc.code)
			if rows := defectRows(t, h, statusRoot); len(rows) != 0 {
				t.Fatalf("defects = %+v, want none", rows)
			}
		})
	}

	t.Run("a closed issue is refused DEFECT_ISSUE_CLOSED", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		grant := architect.grant(t)
		setIssueColumns(t, h, statusRoot, "phase = 'done', status = 'done'")
		refused(t, reportDefectAs(h, grant, statusRoot, defectText, defectHead, ""), http.StatusConflict, "DEFECT_ISSUE_CLOSED")
		if rows := defectRows(t, h, statusRoot); len(rows) != 0 {
			t.Fatalf("defects = %+v, want none", rows)
		}
	})

	t.Run("an issue of a lingering tree is refused TREE_LINGERING", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		acceptPhase(t, h, statusChild, phase.Retro)
		implementer := newLiveClaimIn(t, h, statusRoot, statusChild, claim.RoleImplementer)
		grant := implementer.grant(t)
		setIssueColumns(t, h, statusRoot, "linger_until = $2", time.Now().Add(time.Hour))
		refused(t, reportDefectAs(h, grant, statusChild, defectText, defectHead, ""), http.StatusConflict, "TREE_LINGERING")
		if rows := defectRows(t, h, statusChild); len(rows) != 0 {
			t.Fatalf("defects = %+v, want none", rows)
		}
	})
}

// withdraw_defect answers each case of the shared contract: the owning architect withdraws a
// reported defect with a reason, which close-wait.md's next version names; anyone else, an id the
// issue does not hold, a check's or a production check's reason and a defect a merged change
// carried are refused, and every refusal changes nothing. 409 DEFECT_REQUEST_OF_A_PERSON is not
// here: it refuses a request of a person, a kind of reason Task 13 adds, which no code writes yet.
func TestWithdrawDefectAnswersEachCase(t *testing.T) {
	t.Parallel()

	// reported is the harness with the root in retro and one defect its implementer reported, that
	// defect's id, and the implementer's claim.
	reported := func(t *testing.T) (*harness, string, liveClaim) {
		t.Helper()
		h, _, _ := statusHarness(t)
		acceptPhase(t, h, statusRoot, phase.Retro)
		implementer := newLiveClaim(t, h, statusRoot, claim.RoleImplementer)
		answer := answeredDefect(t, reportDefectAs(h, implementer.grant(t), statusRoot, defectText, defectHead, ""))
		return h, answer.Defect, implementer
	}

	t.Run("the owning architect withdraws a reported defect with its reason", func(t *testing.T) {
		t.Parallel()
		h, id, _ := reported(t)
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		answer := answeredDefect(t, withdrawDefectAs(h, architect.grant(t), statusRoot, id, "same as 3"))
		if answer.State != "withdrawn" || answer.Defect != id {
			t.Fatalf("answer = %+v, want defect %s withdrawn", answer, id)
		}
		rows := defectRows(t, h, statusRoot)
		if len(rows) != 1 || !rows[0].withdrawn || rows[0].reason != "same as 3" {
			t.Fatalf("defects = %+v, want the one defect withdrawn with its reason", rows)
		}
		wait := storedWaitOn(t, h, statusRoot)
		if len(wait.Defects) != 0 || len(wait.Withdrawn) != 1 || strconv.FormatInt(wait.Withdrawn[0].ID, 10) != id || wait.Withdrawn[0].Reason != "same as 3" {
			t.Fatalf("the stored wait = %+v, want no defect and the withdrawal of %s with its reason", wait, id)
		}
		if at := issuePhaseOf(t, h, statusRoot); at != phase.Retro {
			t.Fatalf("%s after the withdrawal stands in %s, want retro", statusRoot, at)
		}
	})

	t.Run("a phase worker is refused DEFECT_WITHDRAW_NOT_OWNER", func(t *testing.T) {
		t.Parallel()
		h, id, implementer := reported(t)
		refused(t, withdrawDefectAs(h, implementer.grant(t), statusRoot, id, "fixed"), http.StatusForbidden, "DEFECT_WITHDRAW_NOT_OWNER")
		if rows := defectRows(t, h, statusRoot); len(rows) != 1 || rows[0].withdrawn {
			t.Fatalf("defects = %+v, want the defect still open", rows)
		}
	})

	for _, id := range []string{"999999", "not-an-id"} {
		t.Run("the id "+id+" is refused DEFECT_NOT_FOUND", func(t *testing.T) {
			t.Parallel()
			h, _, _ := reported(t)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			refused(t, withdrawDefectAs(h, architect.grant(t), statusRoot, id, "fixed"), http.StatusNotFound, "DEFECT_NOT_FOUND")
		})
	}

	for _, kind := range []record.DefectKind{record.DefectKindCheck, record.DefectKindProduction} {
		t.Run("a "+string(kind)+" reason is refused DEFECT_IS_A_CHECK", func(t *testing.T) {
			t.Parallel()
			h, _, _ := statusHarness(t)
			setIssueColumns(t, h, statusRoot, "phase = 'verifying', status = 'testing'")
			// A check's reason names its deciding result, as the record requires of it.
			reason := record.Defect{Issue: statusRoot, Generation: 1, Kind: kind, Text: "it failed", Check: "c-done",
				ReportedAt: time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)}
			if kind == record.DefectKindCheck {
				reason.Result = "result-7"
			}
			id := seedDefect(t, h, reason)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			refused(t, withdrawDefectAs(h, architect.grant(t), statusRoot, strconv.FormatInt(id, 10), "wrong check"), http.StatusConflict, "DEFECT_IS_A_CHECK")
			if rows := defectRows(t, h, statusRoot); len(rows) != 1 || rows[0].withdrawn {
				t.Fatalf("defects = %+v, want the reason still open", rows)
			}
		})
	}

	t.Run("a defect a merged change carried is refused DEFECT_IN_MERGED_CHANGE", func(t *testing.T) {
		t.Parallel()
		h, _, _ := statusHarness(t)
		setIssueColumns(t, h, statusRoot, "phase = 'verifying', status = 'testing'")
		id := seedDefect(t, h, record.Defect{Issue: statusRoot, Generation: 1, Kind: record.DefectKindDefect, Text: defectText,
			Role: claim.RoleMerger, Head: defectHead, ReportedAt: time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)})
		if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			records := record.NewStore()
			if err := records.CarryDefects(ctx, tx, statusRoot, 2, []int64{id}); err != nil {
				return err
			}
			return records.MergeDefects(ctx, tx, statusRoot, 2, time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC))
		}); err != nil {
			t.Fatalf("merge the change that carried defect %d: %v", id, err)
		}
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		refused(t, withdrawDefectAs(h, architect.grant(t), statusRoot, strconv.FormatInt(id, 10), "fixed"), http.StatusConflict, "DEFECT_IN_MERGED_CHANGE")
	})

	t.Run("a blank reason is refused DEFECT_REASON_REQUIRED", func(t *testing.T) {
		t.Parallel()
		h, id, _ := reported(t)
		architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
		refused(t, withdrawDefectAs(h, architect.grant(t), statusRoot, id, "  \n"), http.StatusBadRequest, "DEFECT_REASON_REQUIRED")
		if rows := defectRows(t, h, statusRoot); len(rows) != 1 || rows[0].withdrawn {
			t.Fatalf("defects = %+v, want the defect still open", rows)
		}
	})
}

// The goldens of the two calls' answers are real answers of their routes: the implementer's
// report in retro, the first defect of a fresh record, and the owning architect's withdrawal of
// it (`defect-report.json` and `defect-withdraw.json`, which the contracts package parses).
func TestTheDefectCallsAnswerTheirGoldens(t *testing.T) {
	h, _, _ := statusHarness(t)
	acceptPhase(t, h, statusRoot, phase.Retro)
	implementer := newLiveClaim(t, h, statusRoot, claim.RoleImplementer)
	report := reportDefectAs(h, implementer.grant(t), statusRoot, defectText, defectHead, "")
	if report.Code != http.StatusOK {
		t.Fatalf("report_defect = %d, want 200; body %s", report.Code, report.Body)
	}
	var reported DefectReportResponse
	decodeInto(t, report, &reported)
	golden(t, "defect-report.json", reported)

	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	withdrawal := withdrawDefectAs(h, architect.grant(t), statusRoot, reported.Defect, "same as an earlier defect")
	if withdrawal.Code != http.StatusOK {
		t.Fatalf("withdraw_defect = %d, want 200; body %s", withdrawal.Code, withdrawal.Body)
	}
	var withdrawn DefectWithdrawResponse
	decodeInto(t, withdrawal, &withdrawn)
	golden(t, "defect-withdraw.json", withdrawn)
}
