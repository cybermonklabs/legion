package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// IssueStatusRequest is the controller's set_status: the status to write on Issue, todo, backlog or
// icebox, and the two arguments Dispatch's parent rule asks for (R24 of dispatch://INFRA-3320).
// Children goes with a backlog or icebox: "include" moves each open issue under Issue with it
// (Dispatch's include_children) and "unlink" takes each open child out from under it
// (unlink_children); a move that would leave a child in work under Issue needs one of them. Parent
// goes with a todo: "include" moves each issue above Issue that is not in work to in_progress with
// it (include_parent), which a todo under a parent out of work needs.
type IssueStatusRequest struct {
	GrantID  string `json:"grantId"`
	Issue    string `json:"issue"`
	Status   string `json:"status"`
	Children string `json:"children,omitempty"`
	Parent   string `json:"parent,omitempty"`
}

// StatusCallResponse is the answer of a status call that writes one issue's status, the
// controller's set_status and an architect's rerun_child, once the call's transaction has
// committed: each status write the call queued, in the order it queued them (decision 5 of
// dispatch://INFRA-4730), none where the call writes no status (a todo on an issue in Legion's
// work). Each write is counted on the record of an issue the daemon records before Dispatch echoes
// it, so an issue the controller parks stops before the answer, and Dispatch takes it when its row
// runs. A caller below the contract that names the writes (chainContract) is served the empty
// object it has always read (shapeAnswer).
type StatusCallResponse struct {
	Queued []QueuedStatusWrite `json:"queued"`
}

// MarshalJSON keeps `queued` an array on the wire, empty rather than `null`.
func (sr StatusCallResponse) MarshalJSON() ([]byte, error) {
	type wire StatusCallResponse
	out := wire(sr)
	if out.Queued == nil {
		out.Queued = []QueuedStatusWrite{}
	}
	return json.Marshal(out)
}

// QueuedStatusWrite is one status write a status call queued (intake.QueuedWrite): its issue, the
// status the write gives it, and the outbox row that writes it, which runs on the issue's own lane.
type QueuedStatusWrite struct {
	Issue  string `json:"issue"`
	Status string `json:"status"`
	Row    int64  `json:"row"`
}

// queuedOf is the writes result queued, as an answer names them.
func queuedOf(result intake.Result) []QueuedStatusWrite {
	queued := make([]QueuedStatusWrite, len(result.Queued))
	for i, write := range result.Queued {
		queued[i] = QueuedStatusWrite{Issue: write.Issue, Status: write.Status, Row: write.Row}
	}
	return queued
}

// issueStatus is the controller's set_status. It reads the issue, and each issue above it on its
// Dispatch chain, from Dispatch, and applies one call that brings the copy of Dispatch's issues up
// to date with those reads and decides over it in one transaction (intake.StatusCall, the
// workflow's statusCall; decision 5 of dispatch://INFRA-4730): on a known chain, with the children
// argument where a backlog or icebox would leave a child in work under the issue
// (CHILDREN_ARGUMENT_REQUIRED) and the parent argument where a todo would put it in work under a
// parent that is not (PARENT_ARGUMENT_REQUIRED). The write is queued as a status row on the issue's
// lane, which the controller is told of should Dispatch refuse it, and counted on the issue's
// record before Dispatch echoes it (R16). A todo on an issue in Legion's work writes no status: on a
// root whose architect claim failed it starts that claim again in its session, as on any issue in
// work it is the issue's own todo (R20). An issue the daemon does not record is recorded, where its
// todo makes it one the daemon takes, from Dispatch's answer to the write (the outbox, through
// ApplyOwnStatusWrite). An issue of another Dispatch project is refused before anything is read or
// queued (ISSUE_OF_ANOTHER_PROJECT): only that project's daemon writes its status, as the outbox
// and the consumer each keep to the daemon's own project (ownDispatchIssue).
func (s *server) issueStatus(w http.ResponseWriter, r *http.Request) {
	var req IssueStatusRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"status", req.Status}) {
		return
	}
	switch {
	case req.Status != "todo" && req.Status != "backlog" && req.Status != "icebox":
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_STATUS", "status must be todo, backlog, or icebox")
		return
	case req.Children != "" && req.Children != intake.IncludeArgument && req.Children != intake.UnlinkArgument:
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_CHILDREN", "children must be include or unlink")
		return
	case req.Children != "" && req.Status == "todo":
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_CHILDREN", "children goes with backlog or icebox, which move an issue out of work")
		return
	case req.Parent != "" && req.Parent != intake.IncludeArgument:
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_PARENT", "parent must be include")
		return
	case req.Parent != "" && req.Status != "todo":
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_PARENT", "parent goes with todo, which moves an issue into work")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "CONTROLLER_REQUIRED", "issue status requires a controller grant")
		return
	}
	if !s.ownDispatchIssue(req.Issue) {
		s.writeFailure(w, r, http.StatusUnprocessableEntity, "ISSUE_OF_ANOTHER_PROJECT", fmt.Sprintf(
			"%s is an issue of project %s, not of this daemon's project %s; only that project's daemon writes its status",
			req.Issue, claim.KeyProject(req.Issue), s.dispatchProject))
		return
	}
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return
	}
	reads := s.dispatchReads()
	if _, ok := reads.issue(w, r, req.Issue); !ok {
		return
	}
	if result, ok := s.applyStatusCall(w, r, "issues/status", intake.StatusCall{Op: intake.SetStatusCall,
		Issues:   []intake.StatusCallIssue{{Key: req.Issue, Status: req.Status, Children: req.Children, Parent: req.Parent}},
		Readings: reads.readings(r.Context(), []string{req.Issue})}); ok {
		s.writeAnswer(w, r, AnswerStatusCall, http.StatusOK, StatusCallResponse{Queued: queuedOf(result)})
	}
}
