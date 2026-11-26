package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// CompleteStepRequest is the owner's report of a step that is its own: a step whose key is owner,
// or the retro of a task whose retro is the owner's. Step is the step's place in the task, from 1,
// and 0 for the retro. The other members are what a worker's completion of that place reports
// (HandoffCompleteRequest), under the same names and rules: the verdict and the commit of the
// place's role (stepReportRefusal), the handoff record of a phase that ends with one, returnTo with
// a fail, and the rating of the plan on the retro. PlanRating, PlanReason and ReturnTo are pointers
// for the reason they are on the completion: a report naming 0 is told from one naming none.
type CompleteStepRequest struct {
	GrantID        string  `json:"grantId"`
	Issue          string  `json:"issue"`
	Step           int     `json:"step"`
	Summary        string  `json:"summary"`
	Verdict        string  `json:"verdict,omitempty"`
	Commit         string  `json:"commit,omitempty"`
	HandoffID      string  `json:"handoffId,omitempty"`
	HandoffVersion int     `json:"handoffVersion,omitempty"`
	ReturnTo       *int    `json:"returnTo,omitempty"`
	PlanRating     *int    `json:"planRating,omitempty"`
	PlanReason     *string `json:"planReason,omitempty"`
}

// CompleteStepResponse confirms the fact committed.
type CompleteStepResponse struct{}

// completion is the request as the completion route's rules read it, so the owner's report is held
// to the rules of a worker's completion by the same functions (returnToRefusal, planRatingOf,
// handoffRecord).
func (req CompleteStepRequest) completion() HandoffCompleteRequest {
	return HandoffCompleteRequest{GrantID: req.GrantID, Summary: req.Summary, Verdict: req.Verdict, Commit: req.Commit,
		HandoffID: req.HandoffID, HandoffVersion: req.HandoffVersion, PlanRating: req.PlanRating, PlanReason: req.PlanReason, ReturnTo: req.ReturnTo}
}

// ownerPlace is where the owner's report of one place finds its issue, read in one read-only
// transaction: the issue, its plan's running task and steps (no plan rows where stored is false),
// the role of the place reported (record.PlaceWorker: the step's own role at a step, the role of the
// retro's run at the retro, the implementer for the owner's, and none for a step the task does not
// hold) and that role's phases row, and the issue's pull request record, nil while none is recorded.
type ownerPlace struct {
	issue  record.Issue
	stored bool
	task   record.PlanTask
	steps  []record.PlanStep
	role   claim.Role
	row    record.PhaseRow
	pr     *record.PullRequest
}

// readOwnerPlace reads where the owner's report of the place named finds key (ownerPlace), nil for
// an issue the daemon does not record.
func (s *server) readOwnerPlace(ctx context.Context, key string, named record.TaskPlace) (*ownerPlace, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := s.records.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, err
	}
	at := &ownerPlace{issue: *issue}
	stored, err := s.records.Plan(ctx, tx, key, issue.Generation)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if at.task, err = stored.Task(); err != nil {
			return nil, err
		}
		at.stored, at.steps = true, stored.Steps[at.task.Task]
	}
	worker, _ := record.PlaceWorker(at.task.Task, named, at.steps)
	at.role = worker.Role
	rows, err := s.records.Phases(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	at.row = record.PhaseRowOf(rows, at.role)
	if at.pr, err = s.records.PullRequest(ctx, tx, key); err != nil {
		return nil, err
	}
	return at, tx.Commit(ctx)
}

// completeStep is the owner's report of a step of its own, `POST /legion/v1/steps/complete`: a step
// whose key is owner, or the retro of a task whose retro is the owner's (complete_step). It takes a
// report from the owner's own claim alone, the issue's own architect (record.OwnerClaim): every
// other architect, one above the issue in its tree that sign_off takes for it among them, is
// refused NOT_THE_OWNER. It takes a report of the step that runs now, whose agent is the owner, and
// refuses any other OWNER_STEP_NOT_RUNNING (record.OwnerStepNotRunning), as the workflow does again
// under the fact's lock (workflow's ownerStepRefusal). The report is held to the rules of a
// worker's completion of the same place: what it carries (stepReportRefusal), its returnTo, the
// form of its rating, its handoff record, read under the owner's session, and, for the
// implementer's step, the open pull request the implementer's completion reads
// (implementationPullRequest). The owner's acceptance is applied as today's acts of it, so it has
// one engine path (ownersAcceptance). Every other report is the completion of the place's role
// under the owner's claim (intake.HandoffComplete with Step), at the entry of that role's phases
// row. Each refusal is written before the fact is applied, so nothing is recorded; a report the
// workflow takes queues the read of the issue's pull request, as a worker's completion does
// (Options.HandoffCompleted).
func (s *server) completeStep(w http.ResponseWriter, r *http.Request) {
	var req CompleteStepRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"summary", req.Summary}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if req.Step < 0 {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_STEP", fmt.Sprintf(
			"step is %d, and it names a step of the task by its place, 1 for its first, or 0 for the retro; nothing was recorded", req.Step))
		return
	}
	grant, ok := s.architectGrant(w, r, req.GrantID)
	if !ok {
		return
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	named := record.ReportedPlace(req.Step)
	at, err := s.readOwnerPlace(r.Context(), req.Issue, named)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	if at == nil {
		s.writeFailure(w, r, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	issue := at.issue
	owner, err := record.OwnerClaim(issue)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not name the owner of %s", issue.Key))
		return
	}
	if grant.Claim != owner {
		s.writeFailure(w, r, http.StatusForbidden, "NOT_THE_OWNER", fmt.Sprintf(
			"the steps of the key owner of %s are its own architect's, %s, and this grant is of %s. This report changed nothing.", issue.Key, owner, grant.Claim))
		return
	}
	if !record.OwnerStepRuns(issue, at.task.Task, at.stored, at.task.State, req.Step, at.task.Place, at.steps) {
		s.writeFailure(w, r, http.StatusConflict, "OWNER_STEP_NOT_RUNNING",
			record.OwnerStepNotRunning(issue, at.task.Task, at.stored, at.task.State, req.Step, at.task.Place, at.steps))
		return
	}
	completion := req.completion()
	if refusal := stepReportRefusal(at.role, req.Step == 0, true, req.Verdict, req.Commit, issue.Key); refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	what := fmt.Sprintf("this report of %s of %s", record.PlaceWords(named), issue.Key)
	if refusal := returnToRefusal(completion, req.Verdict == "fail", what); refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	rating, refusal := planRatingOf(completion)
	if refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	machine, running := s.supervisor.Machine(owner)
	if !running {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_NO_CLAIM", "the daemon supervises no claim for this grant")
		return
	}
	c := machine.Claim()
	if word, writer, ok := phase.Handoff(issue.Phase); ok && writer == at.role {
		if !s.handoffRecord(w, r, c.Session, &issue, issue.Phase, word, completion) {
			return
		}
	} else if req.HandoffID != "" || req.HandoffVersion != 0 {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_RECORD_MISMATCH", fmt.Sprintf(
			"%s of %s ends with no handoff record, and this report names one: report without handoffId and handoffVersion; nothing was recorded", record.PlaceWords(named), issue.Key))
		return
	}
	if at.role == claim.RoleAcceptance {
		if rating != nil {
			refusal := intake.PlanRatingUnexpected(what)
			s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
			return
		}
		s.ownersAcceptance(w, r, req, owner, at)
		return
	}
	eventID := requestFactID("steps/complete")
	if at.role == claim.RoleImplementer && issue.Phase == phase.Implementing && !s.implementationPullRequest(w, r, &issue, at.pr, eventID) {
		return
	}
	returnTo := 0
	if req.ReturnTo != nil {
		returnTo = *req.ReturnTo
	}
	result, err := s.apply(r.Context(), "api", eventID, intake.HandoffComplete{Generation: issue.Generation, Issue: issue.Key, Role: at.role, Claim: owner,
		Summary: req.Summary, Verdict: req.Verdict, Commit: req.Commit, HandoffRecord: req.HandoffID, HandoffVersion: req.HandoffVersion,
		PlanRating: rating, MustRate: c.PluginContract >= PlanRatingContract, ReturnTo: returnTo, Entry: at.row.Entry, Step: req.Step})
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return
	}
	if s.handoffCompleted != nil {
		s.handoffCompleted(issue.Key)
	}
	s.writeAnswer(w, r, AnswerEmptyObject, http.StatusOK, CompleteStepResponse{})
}

// ownersAcceptance applies the owner's report of its acceptance as today's acts of it: pass as
// sign_off (intake.SignOff), and fail as request_backward_move (intake.BackwardMove) with the
// report's summary as its reason, to the step its returnTo names, or, where it names none, to the
// task's first step of the type implementation before the acceptance, as a failed step's work
// returns (record.ReturnTarget). A fail that names no step, with no such step before it (an
// acceptance that is its task's first step among them), returns the work nowhere: it is the fail
// of the step itself, applied as the completion of the acceptance's role under the owner's claim
// (intake.HandoffComplete with the verdict fail), which stops the task and tells its owner, as the
// fail of the fresh agent of acceptance and of every other step with nothing before it to take the
// work does.
func (s *server) ownersAcceptance(w http.ResponseWriter, r *http.Request, req CompleteStepRequest, by claim.Token, at *ownerPlace) {
	entry, ok := s.openStep(w, r, req.Issue)
	if !ok {
		return
	}
	if req.Verdict == "pass" {
		s.applyFact(w, r, requestFactID("steps/complete"), intake.SignOff{Issue: req.Issue, By: by, Generation: at.issue.Generation, Entry: entry}, CompleteStepResponse{})
		return
	}
	back := 0
	if req.ReturnTo != nil {
		if back = *req.ReturnTo; back >= req.Step {
			s.writeFailure(w, r, http.StatusBadRequest, "INVALID_RETURN_TO", fmt.Sprintf(
				"returnTo is %d, and step %d of task %s of %s is not before step %d, the acceptance that failed: send it again naming an earlier step, or without returnTo; nothing was recorded",
				back, back, at.task.Task, req.Issue, req.Step))
			return
		}
	} else {
		back, _ = record.ReturnTarget(at.steps, req.Step)
	}
	if back < 1 {
		s.applyFact(w, r, requestFactID("steps/complete"), intake.HandoffComplete{Generation: at.issue.Generation, Issue: req.Issue, Role: at.role,
			Claim: by, Summary: req.Summary, Verdict: req.Verdict, Commit: req.Commit, Entry: at.row.Entry, Step: req.Step}, CompleteStepResponse{})
		return
	}
	s.applyFact(w, r, requestFactID("steps/complete"), intake.BackwardMove{Issue: req.Issue, Requester: claim.RoleArchitect,
		To: plan.RunsAs(at.steps[back-1].Role), Reason: req.Summary, ReturnTo: back, By: by, Generation: at.issue.Generation, Entry: entry}, CompleteStepResponse{})
}
