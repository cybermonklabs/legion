package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// DefectReportRequest is report_defect's body (`POST /legion/v1/defects/report`, the
// defect-calls entry): the role's grant, the issue whose change holds the defect, the defect
// with its evidence, the head of the pull request the role saw it at, as 40 lowercase hex, and
// optionally the block id of the check it is about.
type DefectReportRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Defect  string `json:"defect"`
	Head    string `json:"head"`
	Check   string `json:"check,omitempty"`
}

// DefectReportResponse is report_defect's answer: the defect's id, `recorded` for a defect the
// call recorded, `same` for an open defect of the issue whose text is the report's, or `joined`
// for a reason of the issue the report's check names, and the generation of the change that will
// carry it (intake.DefectAnswer).
type DefectReportResponse struct {
	Defect     string `json:"defect"`
	State      string `json:"state"`
	NextChange uint64 `json:"nextChange"`
}

// DefectWithdrawRequest is withdraw_defect's body (`POST /legion/v1/defects/withdraw`): the owning
// architect's grant, the issue, the id report_defect answered and the reason, which close-wait.md
// names beside the withdrawn defect.
type DefectWithdrawRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Defect  string `json:"defect"`
	Reason  string `json:"reason"`
}

// DefectWithdrawResponse is withdraw_defect's answer: the defect's id and `withdrawn`.
type DefectWithdrawResponse struct {
	Defect string `json:"defect"`
	State  string `json:"state"`
}

// defectHeadForm is the form of a head report_defect takes: a full commit id, 40 lowercase hex.
var defectHeadForm = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reportDefect is report_defect: a role reports a defect of its issue's change once the task's
// last step passed, which the workflow records and moves no phase for (workflow's defectReport).
// The route refuses what it can judge from the body alone, before anything is recorded: a blank
// defect (DEFECT_TEXT_REQUIRED), one Dispatch would refuse in a message, by the rule a backward
// move's reason is held to (DEFECT_TEXT_TOO_LONG, DEFECT_TEXT_DECISION_BLOCK), and a head that is
// no full commit id (DEFECT_HEAD_REQUIRED). The controller reports nothing, and a phase worker
// reports only on its own issue (DEFECT_CALLER_NOT_ALLOWED). Who may report and when is judged
// under the fact lock (DEFECT_BEFORE_LAST_STEP, DEFECT_CALLER_NOT_ALLOWED, DEFECT_ISSUE_CLOSED,
// TREE_LINGERING).
func (s *server) reportDefect(w http.ResponseWriter, r *http.Request) {
	var req DefectReportRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if strings.TrimSpace(req.Defect) == "" {
		s.writeFailure(w, r, http.StatusBadRequest, "DEFECT_TEXT_REQUIRED",
			"defect is required: say what is wrong in the change, with its evidence; nothing was recorded")
		return
	}
	if over := dispatchbody.Check(record.LongestPost(req.Defect)); over != nil {
		if dispatchbody.IsDecisionBlock(over) {
			s.writeFailure(w, r, http.StatusBadRequest, "DEFECT_TEXT_DECISION_BLOCK", fmt.Sprintf(
				"the defect holds a decision block, which close-wait.md and the notices would carry where nobody can answer it: %s; nothing was recorded, so report it again stating what is wrong rather than asking", over))
			return
		}
		s.writeFailure(w, r, http.StatusBadRequest, "DEFECT_TEXT_TOO_LONG", fmt.Sprintf(
			"the defect makes a message Dispatch refuses: %s; nothing was recorded, so report it again shorter", over))
		return
	}
	if !defectHeadForm.MatchString(req.Head) {
		s.writeFailure(w, r, http.StatusBadRequest, "DEFECT_HEAD_REQUIRED",
			"head is required: the 40 lowercase hex of the pull request's head where you saw the defect; nothing was recorded")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller || grant.Role != claim.RoleArchitect && req.Issue != grant.Issue {
		s.writeFailure(w, r, http.StatusForbidden, "DEFECT_CALLER_NOT_ALLOWED", fmt.Sprintf(
			"a phase worker reports a defect of its own issue alone, and the controller reports none; %s is no issue of this grant, and nothing was recorded", req.Issue))
		return
	}
	fact := intake.DefectReport{Issue: req.Issue, Role: grant.Role, By: grant.Claim, Text: req.Defect, Head: req.Head, Check: strings.TrimSpace(req.Check)}
	answer, ok := s.applyDefectFact(w, r, "defects/report", fact)
	if !ok {
		return
	}
	s.writeAnswer(w, r, AnswerDefectReport, http.StatusOK, DefectReportResponse{
		Defect: strconv.FormatInt(answer.Defect, 10), State: string(answer.State), NextChange: answer.NextChange})
}

// withdrawDefect is withdraw_defect: the owning architect ends a reported defect with a reason
// (workflow's defectWithdrawal). The route refuses a blank reason (DEFECT_REASON_REQUIRED), an id
// that is no number (DEFECT_NOT_FOUND) and a grant that is no architect's
// (DEFECT_WITHDRAW_NOT_OWNER); the workflow judges the rest under the fact lock.
func (s *server) withdrawDefect(w http.ResponseWriter, r *http.Request) {
	var req DefectWithdrawRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) ||
		!s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"defect", req.Defect}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeFailure(w, r, http.StatusBadRequest, "DEFECT_REASON_REQUIRED",
			"reason is required: close-wait.md names the withdrawn defect with it; nothing was withdrawn")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller || grant.Role != claim.RoleArchitect {
		s.writeFailure(w, r, http.StatusForbidden, "DEFECT_WITHDRAW_NOT_OWNER", fmt.Sprintf(
			"only the architect that owns %s by the rule of sign_off withdraws a defect of it; nothing was withdrawn", req.Issue))
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(req.Defect), 10, 64)
	if err != nil || id <= 0 {
		s.writeRefusal(w, r, intake.DefectNotFound(req.Issue, req.Defect))
		return
	}
	answer, ok := s.applyDefectFact(w, r, "defects/withdraw", intake.DefectWithdrawal{Issue: req.Issue, Defect: id, Reason: req.Reason, By: grant.Claim})
	if !ok {
		return
	}
	s.writeAnswer(w, r, AnswerDefectWithdraw, http.StatusOK, DefectWithdrawResponse{Defect: strconv.FormatInt(answer.Defect, 10), State: string(answer.State)})
}

// applyDefectFact applies fact, a report or a withdrawal of a defect, and answers what the
// workflow answered of it, or writes the refusal it committed. A fact the workflow took and
// answered nothing of is a fault of the daemon's wiring, not of the call.
func (s *server) applyDefectFact(w http.ResponseWriter, r *http.Request, route string, fact intake.Fact) (intake.DefectAnswer, bool) {
	if s.pool == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return intake.DefectAnswer{}, false
	}
	result, err := s.apply(r.Context(), "api", requestFactID(route), fact)
	if err != nil {
		s.logRequestFailure(r, err, "api: apply a call on a defect", "route", route)
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return intake.DefectAnswer{}, false
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return intake.DefectAnswer{}, false
	}
	if result.Defect == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "the workflow answered nothing of this call; nothing is known to be recorded")
		return intake.DefectAnswer{}, false
	}
	return *result.Defect, true
}
