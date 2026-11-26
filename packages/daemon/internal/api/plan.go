package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// PlanRegisterRequest is register_plan: the owner of an issue, an architect of the issue's tree,
// or the sub-owner of one of its tasks gives Legion the plan in one version of a document of the
// issue. A sub-owner gives a version of the plan of its own issue alone, which changes only the
// parts of its task (planOwner; the workflow's planGiven).
type PlanRegisterRequest struct {
	GrantID    string `json:"grantId"`
	Issue      string `json:"issue"`
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
}

// PlanRegisterResponse is the plan register_plan took: the version given, the tasks it added and
// each of its tasks.
type PlanRegisterResponse struct {
	Plan RegisteredPlan `json:"plan"`
}

// RegisteredPlan is one version of a plan as register_plan read it. Added is the ids of the tasks
// the version adds over the version in force, in the plan's order, as the workflow's apply of the
// plan answered them (intake.Result.Added): every task of a generation's first version, none for a
// version that changes or keeps each task it gives, [] and never null; a task it removes is in no
// list of the answer. A registration of a version the store holds already is answered what its
// apply answered, which the store recorded on the version (intake.ApplyFact). A caller below
// tasksContract, which added the field, is served none (shaping.go's withoutNeedsOrAdded).
type RegisteredPlan struct {
	Version int              `json:"version"`
	Added   *[]string        `json:"added,omitempty"`
	Tasks   []RegisteredTask `json:"tasks"`
}

// RegisteredTask is one task of a plan register_plan took: its id, the steps its block names, each
// as the plan writes it (`role` or `role:key`), and the tasks it needs, each by its id as
// register_plan resolved it (plan.FromBlocks). A task whose block names no steps has no step
// here, never null: it runs the project's default steps, which the workflow reads when the task's
// work starts, so the answer names none rather than guessing what the project will run then. A
// task that needs none has [] here, never null; the answer to a caller below tasksContract, which
// added the field, carries none (shaping.go's withoutNeedsOrAdded).
type RegisteredTask struct {
	ID    string    `json:"id"`
	Steps []string  `json:"steps"`
	Needs *[]string `json:"needs,omitempty"`
}

// PlanRefusal is the answer to a plan whose content Legion does not take, 409 PLAN_REFUSED: the
// code and the sentence every refusal carries (Failure), and one reason for each cause
// (intake.PlanRefused). A plan of an issue whose experiment's group takes no plan from its owner
// is no such answer: it is the workflow's 409 PLAN_NOT_TAKEN (intake.PlanNotTaken), a Failure
// with no reasons. `reasons` arrived with register_plan at OwnerPlanContract, and a caller below
// it is served the Failure alone (shaping.go's refusal row), as a PauseFailure is below
// pauseContract.
type PlanRefusal struct {
	Failure
	Reasons []PlanReason `json:"reasons"`
}

// PlanReason is one cause of a refused plan: the task it names, "" for a cause of the whole plan.
type PlanReason struct {
	Task   string `json:"task"`
	Reason string `json:"reason"`
}

// registerPlanRoute is register_plan as a refusal names it, the operation the caller retries.
const registerPlanRoute = "register_plan"

// OwnerPlanContract is the daemon contract that added the owner's plan (version.go's 219): the
// routes register_plan and stop_task with register_plan's answer (answers.go's
// AnswerPlanRegister) and the `reasons` of its refusal (PlanRefusal), a failed step's returnTo on
// handoff_complete, the hold reasons task-stopped and children of an issue's holdReason, and the
// words of those two holds in `admission.held` (children_before_retro and task_stopped). The
// declared table records the four values at it (answers.go's holdReasons and heldReasons), and
// the value strips hold each back from a caller below it (shaping.go's valueStrips).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract these keys and values arrived at stays where it is. It moves
// only with this change's own number, and it is a number of SupportedContracts, which names the
// constant rather than its number for that reason.
const OwnerPlanContract = 219

// writeRefusal writes a committed refusal: a refused plan with its reasons beside the code
// (PlanRefusal), and every other refusal as its code and its sentence (writeFailure). Both are
// the refusal answer, written through the one writer every answer goes through.
func (s *server) writeRefusal(w http.ResponseWriter, r *http.Request, refusal *intake.Refusal) {
	if refusal.Reasons == nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	if noter, ok := w.(refusalNoter); ok {
		noter.noteRefusal(refusal.Code)
	}
	reasons := make([]PlanReason, len(refusal.Reasons))
	for i, reason := range refusal.Reasons {
		reasons[i] = PlanReason{Task: reason.Task, Reason: reason.Reason}
	}
	s.writeAnswer(w, r, AnswerRefusal, refusal.Status, PlanRefusal{
		Failure: Failure{Code: refusal.Code, Error: redact.URLCredentials(refusal.Message)},
		Reasons: reasons,
	})
}

// registerPlan is register_plan. It authorizes the owner (planOwner), reads the document's approval
// for the version Dispatch has and the issue carrying it (documentOfIssue), reads the plan's blocks
// at the version given, each need as the task it names (planAt, through plan.FromBlocks), and
// refuses with one reason for each cause of a plan this release does not carry out (plan.Supported)
// or no release can (plan.Validate); each of those answers records nothing. A plan both take is
// recorded as intake.PlanGiven, its needs as task ids, whose rules of a later version are the
// workflow's. A new version's apply answers its added tasks; where the workflow takes the version
// already in force, the route reads that version's recorded additions after it applies.
//
// The fact's id is the issue, the generation the plan is given at, the document and the version.
// No id decides whether a registration is one already taken (intake.ApplyFact applies every
// PlanGiven): the workflow decides it against the store. A version the store holds is answered as
// taken, the same as its first registration, and one that the issue's first step passed over
// applies when it is given again, whoever gives it. planOwner authorizes an architect of the
// issue's tree or the sub-owner of a task for the sub-owner's own issue; the workflow judges the
// giver again under the fact lock.
func (s *server) registerPlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRegisterRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"artifactId", req.ArtifactID},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if !validUUID(req.ArtifactID) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ARTIFACT_ID", "artifactId must be a UUID")
		return
	}
	if req.Version < 1 {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_VERSION", "version must be positive")
		return
	}
	artifactID := strings.ToLower(req.ArtifactID)
	grant, generation, ok := s.planOwner(w, r, req.GrantID, req.Issue)
	if !ok {
		return
	}
	approval, ok := s.documentOfIssue(w, r, artifactID, req.Issue, registerPlanRoute)
	if !ok {
		return
	}
	// The version is the caller's; only Dispatch's read says the document has it.
	if req.Version > approval.LatestVersion {
		s.writeFailure(w, r, http.StatusConflict, "VERSION_NOT_IN_DISPATCH", fmt.Sprintf(
			"Dispatch shows %s at version %d, not %d; give register_plan a version that Dispatch has", artifactID, approval.LatestVersion, req.Version))
		return
	}
	given, ok := s.planAt(w, r, artifactID, dispatch.DocumentRef(req.Issue, approval.Slug, approval.Primary), req.Version, req.Issue)
	if !ok {
		return
	}
	// FromBlocks read each need as the task it names, so a need that names no task is Validate's,
	// and the plan recorded and answered holds task ids, which every reader of a need reads. Both
	// checks run, so a plan with two causes gets two reasons: what this release does not carry out
	// yet, then what no release can. Each judges the plan against the deployment the daemon's
	// settings define (Options.Deployment), so a role of `roles` is a role a step names. Validate
	// checks a plan its owner wrote, before the fact lock, so it runs on the request's context and
	// stops when the request ends, recording nothing.
	invalid, err := plan.Validate(r.Context(), given, s.deployment)
	if err != nil {
		s.logRequestFailure(r, err, "api: the check of a plan stopped", "issue", req.Issue, "artifact", artifactID, "version", req.Version)
		s.refuse(w, r, http.StatusServiceUnavailable, "register_plan stopped before it finished checking the plan, because its request ended; nothing was recorded")
		return
	}
	refusals := append(plan.Supported(given, s.deployment), invalid...)
	if len(refusals) > 0 {
		s.writeRefusal(w, r, intake.PlanRefused(refusals))
		return
	}
	fact := intake.PlanGiven{Issue: req.Issue, Generation: generation, Plan: given, GivenBy: grant.Claim}
	if s.pool == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	result, err := s.apply(r.Context(), "api", fmt.Sprintf("plan:%s:%d:%s:%d", req.Issue, generation, artifactID, req.Version), fact)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return
	}
	if result.Added == nil {
		var held bool
		if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result.Added, held, err = s.records.PlanVersionHeld(ctx, tx, fact.Issue, fact.Generation, fact.Plan.ArtifactID, fact.Plan.Version)
			return err
		}); err != nil {
			s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not read the plan registration")
			return
		} else if !held {
			result.Added = nil
		}
	}
	s.writeAnswer(w, r, AnswerPlanRegister, http.StatusOK, PlanRegisterResponse{Plan: registeredPlan(given, result.Added)})
}

// planOwner authorizes register_plan for an architect of the issue's tree, or for the sub-owner of
// one of its tasks (subOwnerOfIssue), and answers the generation the plan is given at. A recorded
// issue is its tree's architects', the root's or a sub-architect's (architectOfRecord, by
// record.ArchitectOfTree, the rule every architect operation holds it to), and its plan is given at
// its generation. One the daemon has not recorded — a child before release_children records it — is
// the tree's when Dispatch's parent chain reaches the tree (treeChain) and it has not entered the
// workflow, triage, backlog, icebox or todo, and its plan is given at generation 0, where it waits
// for the issue's first step. These reads come before the fact lock, so each refusal here is an
// early one: the workflow judges the giver and the generation again on the record its fact reads
// (workflow's planGiven). The grant picks the rule (subOwnerGrant), and each rule redeems it and
// answers its own refusals: the architect's refuses every grant but an architect's
// ARCHITECT_REQUIRED.
func (s *server) planOwner(w http.ResponseWriter, r *http.Request, grantID, issue string) (credential.Grant, uint64, bool) {
	if s.subOwnerGrant(grantID) {
		return s.subOwnerOfIssue(w, r, grantID, issue)
	}
	grant, recorded, ok := s.architectOfRecord(w, r, grantID, issue)
	if !ok {
		return credential.Grant{}, 0, false
	}
	if recorded != nil {
		return grant, recorded.Generation, true
	}
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return credential.Grant{}, 0, false
	}
	read := s.cachedIssueReader(registerPlanRoute)
	unrecorded, found := read(w, r, issue)
	if !found {
		return credential.Grant{}, 0, false
	}
	if _, ok := s.treeChain(w, r, issue, unrecorded, grant.Tree, read); !ok {
		return credential.Grant{}, 0, false
	}
	switch unrecorded.Status {
	case "triage", "backlog", "icebox", "todo":
		return grant, 0, true
	}
	s.writeFailure(w, r, http.StatusConflict, "ISSUE_ALREADY_RELEASED", fmt.Sprintf(
		"%s is %s in Dispatch and the daemon holds no record of it; register_plan takes a plan for an issue the daemon has not recorded only while it is triage, backlog, icebox or todo",
		issue, unrecorded.Status))
	return credential.Grant{}, 0, false
}

// subOwnerGrant says whether grantID names a grant of role sub-owner (claim.RoleSubOwner) whose
// claim is a task's sub-owner's (record.SubOwnerOf). It calls credential.Grants.Redeem, which
// writes nothing, and answers no refusal: the rule it picks redeems the grant through s.redeem and
// answers an expired or revoked one itself.
func (s *server) subOwnerGrant(grantID string) bool {
	held, err := s.grants.Redeem(grantID)
	if err != nil || held.Controller || held.Role != claim.RoleSubOwner {
		return false
	}
	_, subOwner := record.SubOwnerOf(held.Claim)
	return subOwner
}

// subOwnerOfIssue authorizes register_plan for the grant of a task's sub-owner (subOwnerGrant)
// whose claim is on issue (credential.Grant.Issue, as phaseBackward holds a worker to its own
// issue). A sub-owner's grant for another issue is refused 403 SUB_OWNER_OUTSIDE_ISSUE. A sub-owner
// starts only once its task runs, so its issue is recorded (404 ISSUE_NOT_FOUND otherwise), and its
// plan is given at the issue's generation. Which task the agent is the sub-owner of is the
// workflow's to read, from the plan in force in the fact's own transaction (planGiven).
func (s *server) subOwnerOfIssue(w http.ResponseWriter, r *http.Request, grantID, issue string) (credential.Grant, uint64, bool) {
	grant, ok := s.redeem(w, r, grantID)
	if !ok {
		return credential.Grant{}, 0, false
	}
	if grant.Issue != issue {
		s.writeFailure(w, r, http.StatusForbidden, "SUB_OWNER_OUTSIDE_ISSUE", fmt.Sprintf(
			"a sub-owner of %s gives a version of the plan of %s alone.", grant.Issue, grant.Issue))
		return credential.Grant{}, 0, false
	}
	recorded, err := s.recordedIssue(r.Context(), issue)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return credential.Grant{}, 0, false
	}
	if s.notRecorded(w, r, recorded) {
		return credential.Grant{}, 0, false
	}
	return grant, recorded.Generation, true
}

// TaskStopRequest is stop_task: the owner of a recorded issue stops one task of the plan Legion
// runs for it, so that a later version of the plan may change that task.
type TaskStopRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Task    string `json:"task"`
}

// stopTask is stop_task: an architect of the tree of an issue the daemon has recorded
// (architectForIssue) stops one task of it, answered 200 with no body. The stop is the
// workflow's (intake.TaskStopped), at the generation the route read: the workflow judges the
// owner and the generation again under the fact lock, since an adoption or a new run can commit
// between this read and the fact, and it answers a task the plan does not hold (TaskNotFound), one
// that is done (TaskDone), and every task of an issue whose experiment's group takes no plan from
// its owner (intake.TaskNotStopped, PLAN_NOT_TAKEN). This route's own refusals are the early ones.
// Each request is its own fact (requestFactID): a task the owner stops, starts again with a later
// version and stops once more is stopped twice, and a retry of a stop already applied finds the
// task stopped, which changes nothing.
func (s *server) stopTask(w http.ResponseWriter, r *http.Request) {
	var req TaskStopRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"task", req.Task},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	grant, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	if !ok {
		return
	}
	s.applyFact(w, r, requestFactID("tasks/stop"), intake.TaskStopped{Issue: req.Issue, Task: req.Task, By: grant.Claim, Generation: recorded.Generation}, EmptyResponse{})
}

// planAt is the plan in version of the document artifactID, which documentOfIssue already held
// to issue: its blocks as Dispatch gives them for that version (dispatch.Client.PlanDocument),
// each task block's Parent read from its `parent` (plan.ParentOf) against reference, the document
// as a reference to one of its blocks names it (dispatch.DocumentRef), then read as the plan of
// issue (plan.FromBlocks). So a need that is a reference into another issue is no need of a task of
// this plan, and a `parent` puts a block under a block of this document alone. A block the plan's
// grammar cannot read, a `parent` that is not one reference among them, is a refused plan whose one
// reason names the block; a Dispatch that gives no blocks cannot show Legion a plan at all. Every
// refusal records nothing.
func (s *server) planAt(w http.ResponseWriter, r *http.Request, artifactID, reference string, version int, issue string) (plan.Plan, bool) {
	document, err := s.dispatch.PlanDocument(r.Context(), artifactID, version)
	switch {
	case errors.Is(err, dispatch.ErrNoBlocks):
		s.writeFailure(w, r, http.StatusBadGateway, "PLAN_UNREADABLE", fmt.Sprintf(
			"Dispatch gave version %d of %s without its blocks, so Legion cannot read a plan from it: that Dispatch predates the plan's blocks. Nothing was recorded; give the plan again once Dispatch is upgraded",
			version, artifactID))
		return plan.Plan{}, false
	case dispatch.Missing(err), err == nil && document.IssueKey != issue:
		s.writeFailure(w, r, http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE", fmt.Sprintf("%s is not a document of %s", artifactID, issue))
		return plan.Plan{}, false
	case err != nil:
		s.writeFailure(w, r, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("Dispatch read of %s at version %d failed; retry %s", artifactID, version, registerPlanRoute))
		return plan.Plan{}, false
	}
	blocks := make([]plan.Block, len(document.Blocks))
	for i, block := range document.Blocks {
		blocks[i] = plan.Block{ID: block.ID, Type: block.Type, Attributes: block.Attributes, Text: block.Text}
		if block.Type != plan.TaskBlockType {
			continue
		}
		parent, err := plan.ParentOf(block.Attributes["parent"], reference)
		if err != nil {
			s.writeRefusal(w, r, intake.PlanRefused([]plan.Refusal{{Reason: fmt.Sprintf("task block %s: %v.", block.ID, err)}}))
			return plan.Plan{}, false
		}
		blocks[i].Parent = parent
	}
	given, err := plan.FromBlocks(issue, artifactID, version, blocks)
	if err != nil {
		s.writeRefusal(w, r, intake.PlanRefused([]plan.Refusal{{Reason: err.Error() + "."}}))
		return plan.Plan{}, false
	}
	return given, true
}

// registeredPlan is given as register_plan answers it, with added, the tasks its version added
// ([] for nil).
func registeredPlan(given plan.Plan, added []string) RegisteredPlan {
	tasks := make([]RegisteredTask, len(given.Tasks))
	for i, task := range given.Tasks {
		needs := append([]string{}, task.Needs...)
		tasks[i] = RegisteredTask{ID: task.ID, Steps: plan.Words(task.Steps), Needs: &needs}
	}
	answered := append([]string{}, added...)
	return RegisteredPlan{Version: given.Version, Added: &answered, Tasks: tasks}
}
