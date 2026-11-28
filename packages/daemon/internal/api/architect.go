package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// EmptyResponse is every accepted mutating route whose caller needs no result beyond success.
type EmptyResponse struct{}

// GateRegisterRequest records the document version an architect asked a human to approve, for one
// step of the tree's design gate. Step is the step the document is for, which a deployment that
// gates one step alone may leave out; the deployment's list says which steps it takes, with
// criteria beside a list that names spec and not criteria (registeredStep), and each step binds
// to a document of its own (gateRegister).
//
// BasisArtifactID, BasisVersion and Reason name a basis: the same step's document of an ancestor on
// the issue's Dispatch child_of chain, at a version a person approved or an open approval request
// asks a person to approve, and the architect's reason, which the daemon posts on the issue
// (dispatch://INFRA-3320, R1). The three come together or not at all (refuseBasisInput), and the
// daemon checks the basis against what Dispatch shows before it records it (registrationBasis).
type GateRegisterRequest struct {
	GrantID         string            `json:"grantId"`
	Issue           string            `json:"issue"`
	ArtifactID      string            `json:"artifactId"`
	Version         int               `json:"version"`
	Step            record.DesignStep `json:"step,omitempty"`
	BasisArtifactID string            `json:"basisArtifactId,omitempty"`
	BasisVersion    int               `json:"basisVersion,omitempty"`
	Reason          string            `json:"reason,omitempty"`
}

// GateRegisterResponse confirms the gate registration fact committed.
type GateRegisterResponse struct{}

// WaveReleaseRequest asks an architect to move selected children in its tree to todo.
type WaveReleaseRequest struct {
	GrantID string   `json:"grantId"`
	Issues  []string `json:"issues"`
}

// WaveReleaseResponse answers release_children. A release is accepted whatever the children's
// links say, so released is every issue the request named, each once, in the order the request
// first named it: no child of a wave is lost, and the architect is never asked to send the request
// again. started and held then say what the release did with each of them: started compares each
// child's record read before the request's writes with its record read after them
// (startedByRelease), and held is judged from the blockers Dispatch showed the request, against
// the mirror (blockers.UnfinishedOf) — the same list the child's own todo event carries — not from
// the child's record.
//
// started names the children whose first phase this request started, and held each child the
// request did not start because it waits on an unfinished issue, with the issues it waits on. A
// child the tree's closed design gate withholds is in released alone: the gate, not a blocker,
// holds it, its phase starts when the gate opens, and calling it started would be a lie. Both
// lists follow the request's own order.
//
// queued names each status write the release queued, in the wave's order (decision 5 of
// dispatch://INFRA-4730): each child it writes todo, which Dispatch takes when the child's row
// runs. A caller below the contract that names the writes (chainContract) is served the answer
// without it, as it has always read it (shapeAnswer, queuedHidden).
type WaveReleaseResponse struct {
	Released []string            `json:"released"`
	Started  []string            `json:"started"`
	Held     []HeldChild         `json:"held"`
	Queued   []QueuedStatusWrite `json:"queued"`
	// queuedHidden leaves `queued` off the wire, for a caller below chainContract.
	queuedHidden bool
}

// MarshalJSON keeps `released`, `started`, `held` and `queued` arrays on the wire, empty rather
// than `null`: the plugin's parse of this answer refuses a null where it reads an array. `queued`
// is left off for a caller that does not read it (queuedHidden).
func (wr WaveReleaseResponse) MarshalJSON() ([]byte, error) {
	type wire struct {
		Released []string             `json:"released"`
		Started  []string             `json:"started"`
		Held     []HeldChild          `json:"held"`
		Queued   *[]QueuedStatusWrite `json:"queued,omitempty"`
	}
	out := wire{Released: wr.Released, Started: wr.Started, Held: wr.Held}
	if out.Released == nil {
		out.Released = []string{}
	}
	if out.Started == nil {
		out.Started = []string{}
	}
	if out.Held == nil {
		out.Held = []HeldChild{}
	}
	if !wr.queuedHidden {
		queued := wr.Queued
		if queued == nil {
			queued = []QueuedStatusWrite{}
		}
		out.Queued = &queued
	}
	return json.Marshal(out)
}

// HeldChild is one released child the daemon did not start, and the issues it waits on that are
// not done — the keys of its Dispatch blocked-by links, including any the daemon has not yet
// observed as an issue of its own. It is record.BlockedIssue, the row the state's `blocked` and
// a tick controller notice carry: one issue and what it waits on, declared once below all three
// and validated by the one schema the plugin reads them all through. Its own MarshalJSON is what
// keeps `blockedBy` an array here too, empty rather than `null`.
type HeldChild = record.BlockedIssue

// PhaseBackwardRequest asks a phase worker to return its own issue to an earlier phase, or an
// architect to return an issue it owns from accepting. An architect may own more issues than its
// own, so it names the issue; a phase worker's grant names its own, so issue may be left out and,
// when given, must be the grant's.
type PhaseBackwardRequest struct {
	GrantID string      `json:"grantId"`
	To      phase.Phase `json:"to"`
	Reason  string      `json:"reason"`
	Issue   string      `json:"issue,omitempty"`
}

// PhaseBackwardResponse confirms the phase-backward fact committed.
type PhaseBackwardResponse struct{}

// PhaseRetryRequest records the retry-or-escalate choice of the architect that owns a held issue.
// Reason is why the architect made it, and both decisions carry it: a retry writes it into the
// task the restarted role is given and into the daemon's own log line for the release, and an
// escalation sends it to the controller in the held notice, beside the cause that hold already
// names (record.Notice.Escalation). A request naming none leaves the daemon's own words in the
// restarted role's task and the escalation's notice without the field, so it is optional.
type PhaseRetryRequest struct {
	GrantID  string                         `json:"grantId"`
	Issue    string                         `json:"issue"`
	Decision intake.RetryOrEscalateDecision `json:"decision"`
	Reason   string                         `json:"reason,omitempty"`
}

// PhaseRetryResponse confirms the retry-or-escalate fact committed.
type PhaseRetryResponse struct{}

// SignOffRequest records the owning architect's acceptance of an issue in accepting.
type SignOffRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

// SignOffResponse confirms the sign-off fact committed.
type SignOffResponse struct{}

// RootCloseRequest is a root's architect's close of it before its first phase starts, with the
// reason posted on the issue.
type RootCloseRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Reason  string `json:"reason"`
}

// RerunChildRequest names the issue under the architect's own that rerun_child runs again, and
// its parent argument: "include" moves each issue above it that is not in work to in_progress with
// it (Dispatch's include_parent), which a todo under a parent out of work needs (R24 of
// dispatch://INFRA-3320), and "" moves none.
type RerunChildRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Parent  string `json:"parent,omitempty"`
}

func (s *server) architectGrant(w http.ResponseWriter, r *http.Request, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, r, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || grant.Role != claim.RoleArchitect {
		s.writeFailure(w, r, http.StatusForbidden, "ARCHITECT_REQUIRED", "architect grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func (s *server) phaseWorkerGrant(w http.ResponseWriter, r *http.Request, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, r, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || !isPhaseWorker(grant.Role) {
		s.writeFailure(w, r, http.StatusForbidden, "PHASE_WORKER_REQUIRED", "phase worker grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func isPhaseWorker(role claim.Role) bool {
	switch role {
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger:
		return true
	default:
		return false
	}
}

// recordedIssue reads the issue's record, in a read-only transaction of its own.
func (s *server) recordedIssue(ctx context.Context, issue string) (*record.Issue, error) {
	if s.pool == nil {
		return nil, errors.New("record dependencies are unavailable")
	}
	var recorded *record.Issue
	err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recorded, err = s.records.Issue(ctx, tx, issue)
		return err
	})
	return recorded, err
}

// ownsOnChain authorizes an architect's call on issue, a recorded issue, by the ownership rule
// over the Dispatch chain as the daemon's copy holds it (intake.CallRefusal, R18 of
// dispatch://INFRA-3320): an architect's grant, the issue's record, and call's rule on the chains
// read in one read-only transaction over issue's own chain (record.ReadChainsOf: the rule refuses
// an unknown chain first, and asks the rest of the known one), whose refusal names the
// architect that may act. No stored tree is read. It answers the grant and the record. The workflow
// holds the call's fact to the same rule again where it commits, since a change of parent can land
// between the two (decision 5 of dispatch://INFRA-4730).
func (s *server) ownsOnChain(w http.ResponseWriter, r *http.Request, grantID, issue string, call intake.ArchitectCall) (credential.Grant, record.Issue, bool) {
	grant, ok := s.architectGrant(w, r, grantID)
	if !ok {
		return credential.Grant{}, record.Issue{}, false
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue's record and chain")
		return credential.Grant{}, record.Issue{}, false
	}
	var recorded *record.Issue
	var refusal *intake.Refusal
	if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if recorded, err = s.records.Issue(ctx, tx, issue); err != nil || recorded == nil {
			return err
		}
		chains, err := record.ReadChainsOf(ctx, tx, s.records, recorded.Project, []string{issue})
		if err != nil {
			return err
		}
		refusal, err = intake.CallRefusal(ctx, tx, s.records, recorded.Project, chains, grant.Claim, grant.Issue, issue, call)
		return err
	}); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue's record and chain")
		return credential.Grant{}, record.Issue{}, false
	}
	if recorded == nil {
		s.writeFailure(w, r, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return credential.Grant{}, record.Issue{}, false
	}
	if refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return credential.Grant{}, record.Issue{}, false
	}
	if s.callJudged != nil {
		s.callJudged(r.Context(), call.Op(), []string{issue})
	}
	return grant, *recorded, true
}

// requestFactID names one API request's fact. A grant serves every request its command makes, so
// the grant cannot name the request: each request is its own fact, applied by its one exchange.
func requestFactID(route string) string {
	return "api:" + route + ":" + rand.Text()
}

// applyFact applies one fact of a fact route and answers the empty object every fact route
// answers (AnswerEmptyObject), or the refusal the workflow committed.
func (s *server) applyFact(w http.ResponseWriter, r *http.Request, eventID string, fact intake.Fact, response any) {
	if s.factTaken(w, r, eventID, fact) {
		s.writeAnswer(w, r, AnswerEmptyObject, http.StatusOK, response)
	}
}

// factTaken applies one fact of a fact route and says whether the workflow took it, or writes the
// failure of the apply or the refusal the workflow committed and answers false. A route whose
// answer is not the empty object, register_plan's (AnswerPlanRegister), reads what it answers once
// the fact is taken and writes that itself.
func (s *server) factTaken(w http.ResponseWriter, r *http.Request, eventID string, fact intake.Fact) bool {
	if s.pool == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return false
	}
	result, err := s.apply(r.Context(), "api", eventID, fact)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return false
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return false
	}
	return true
}

func (s *server) gateRegister(w http.ResponseWriter, r *http.Request) {
	var req GateRegisterRequest
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
	if s.refuseBasisInput(w, r, req) {
		return
	}
	artifactID := strings.ToLower(req.ArtifactID)
	// The gate is taken from the architect of an issue that holds its own gate, on that issue, on
	// the Dispatch chain (intake.GateCall): its opening lets each issue under it start its task's
	// first step, so a child working under another issue's gate cannot stand its document in for
	// that gate holder's review.
	grant, recorded, ok := s.ownsOnChain(w, r, req.GrantID, req.Issue, intake.GateCall)
	if !ok {
		return
	}
	step, ok := s.registeredStep(w, r, req.Step)
	if !ok {
		return
	}
	// The issue is read before the approval, so every review that read does not show has a later
	// event sequence: a review the gate applies after this read places after it. The same read
	// names the issue's parent, which a basis and the view fact walk up from.
	root, ok := s.gateIssue(w, r, req.Issue)
	if !ok {
		return
	}
	since := root.LastSeq
	approval, ok := s.documentOfIssue(w, r, artifactID, req.Issue, "register_gate")
	if !ok {
		return
	}
	// Each step records its own document, so a registration of another of the issue's documents
	// would hold the step open for ever with a document no approval of it opens. A deployment
	// that gates no step asks nobody, so nothing turns on which document its one record names,
	// and it takes whatever the architect registers, as it always has under `gates.design: off`.
	if len(s.steps) > 0 && !stepDocument(step, approval) {
		s.writeFailure(w, r, http.StatusConflict, "GATE_STEP_DOCUMENT_MISMATCH", fmt.Sprintf(
			"%s is not the %s step's document of %s; that step records the issue's %s", artifactID, step, req.Issue, step.Document()))
		return
	}
	// The version registered is the caller's; only Dispatch's read says the document has it. A
	// version Dispatch does not show would record a gate no person's review can open.
	if req.Version > approval.LatestVersion {
		s.writeFailure(w, r, http.StatusConflict, "VERSION_NOT_IN_DISPATCH", fmt.Sprintf(
			"Dispatch shows %s at version %d, not %d; register the version you requested approval of", artifactID, approval.LatestVersion, req.Version))
		return
	}
	// What this read found of the person's review, and who gave it, travels with the registration,
	// so a spec a person approved or asked changes of before the architect registered is applied at
	// once: the gate opens, or closes with the architect told. The read's own start is its sequence
	// alone: no time this read shows is the read's own (the latest review's time is that review's),
	// and every act Dispatch records after the read carries a later sequence. The approval request
	// the read found open, and whose turn it waits on, travel with it too, for the registration to
	// mirror.
	changes, reason := approval.ChangesRequestedLatest()
	fact := intake.GateRegistered{Issue: req.Issue, Step: step, ArtifactID: artifactID, Version: req.Version, ApprovedVersion: approval.ApprovedLatest(),
		ChangesRequestedVersion: changes, ChangesReason: reason, ReviewedBy: approval.LatestReviewer(),
		DispatchVersion: approval.LatestVersion, Since: record.EventMark{Seq: since},
		RequestAsk: approval.AskID, RequestWaitsOnAgent: approval.AskID != "" && approval.WaitingOn == "agent",
		By: grant.Claim, Generation: recorded.Generation}
	review := record.EventMark{Seq: approval.Seq, At: approval.At}
	if changes != 0 {
		fact.ChangesRequestedMark = review
	}
	if fact.ApprovedVersion != 0 {
		fact.ApprovedMark = review
	}
	// A basis is checked against everything Dispatch shows of it and of the root, and recorded with
	// the registration, or the call is refused and records nothing. Every registration of the spec
	// step records the view fact (R4): on a basis, the ancestor it rests on, which the basis check
	// read the root as a view on; otherwise whatever viewFact reads.
	if req.basisNamed() {
		if fact.Basis, ok = s.registrationBasis(w, r, grant, req, step, root, artifactID, approval); !ok {
			return
		}
	}
	switch {
	case step != record.DesignStepSpec:
	case fact.Basis != nil:
		fact.ViewOf = fact.Basis.Issue
	default:
		if fact.ViewOf, ok = s.viewFact(w, r, root); !ok {
			return
		}
	}
	// A re-admitted root's architect registers its spec again, often at the same version: each
	// generation's registration is its own fact. The fact's id names the read as well as the
	// version: a second registration at one version carries what Dispatch showed when it ran, so a
	// retry of one read stays one fact while a new read — a review or a version since — is a new
	// fact the handlers see. The ancestor's document is read too, and moves on no event of the
	// root's, so the id names what was read of it: the basis document, its version and the request
	// a pending basis waits on, and the ancestor the view fact names. A registration with no basis
	// after one with a basis at the same version and sequence is a new fact, not a duplicate that
	// answers 200 and changes nothing. The record is read as well: the id names how many times the
	// step's basis has moved (record.DesignGate.BasisMoves, stepBasisMoves), so a call made
	// again over a step whose basis a registration has moved since, as a registration with no basis
	// over a step a pending basis left waiting, or one on that pending basis again after the wait
	// ended, is a new fact the handlers apply, while a call made again over a step whose basis its
	// first attempt left as it was stays one.
	var basisMoves int
	if basisMoves, ok = s.stepBasisMoves(w, r, req.Issue, step); !ok {
		return
	}
	id := fmt.Sprintf("gate:%s:%d:%s:%s:%d:%d", req.Issue, recorded.Generation, step, artifactID, req.Version, since)
	if fact.Basis != nil {
		id += fmt.Sprintf(":basis:%s:%d", fact.Basis.ArtifactID, fact.Basis.Version)
		if fact.Basis.Pending != nil {
			id += ":pending:" + fact.Basis.Pending.Ask
		}
	}
	if fact.ViewOf != "" {
		id += ":view-of:" + fact.ViewOf
	}
	if basisMoves > 0 {
		id += fmt.Sprintf(":basis-moves:%d", basisMoves)
	}
	if s.gateRead != nil {
		s.gateRead(r.Context(), req.Issue)
	}
	s.applyFact(w, r, id, fact, GateRegisterResponse{})
}

// stepBasisMoves is how many times the basis of step of issue has moved in its generation, as
// the record shows it now (record.DesignGate.BasisMoves), zero for a step with no row, which a
// register_gate fact's id names (gateRegister). A failed read refuses the call with
// RECORD_UNAVAILABLE and records nothing.
func (s *server) stepBasisMoves(w http.ResponseWriter, r *http.Request, issue string, step record.DesignStep) (int, bool) {
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the design gate of "+issue)
		return 0, false
	}
	var moves int
	if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		gates, err := s.records.GatesOf(ctx, tx, issue)
		moves = gates[step].BasisMoves
		return err
	}); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the design gate of "+issue)
		return 0, false
	}
	return moves, true
}

// registeredStep is the design-gate step a registration records, from the step the call named and
// the steps the deployment gates.
//
// A deployment that gates no step records the step spec and the registration opens it at once,
// whatever step the call named: no person approves anything there, so the step the call names
// decides nothing. A deployment that gates one step takes a call that names none as that step,
// which is every call a one-step deployment's architect makes. A deployment of several steps
// needs the call to say which one it registers, since the document alone would leave the daemon
// guessing.
//
// A deployment that gates spec and not criteria also takes criteria, for the root's Verification
// document (record.TakesStep): that step is not one every root must register, and a root that
// registers it is held on it as on the spec (record.TreeSteps), so one approval of both documents
// opens the tree. Any other step the deployment does not gate is refused.
func (s *server) registeredStep(w http.ResponseWriter, r *http.Request, named record.DesignStep) (record.DesignStep, bool) {
	switch {
	case len(s.steps) == 0:
		return record.DesignStepSpec, true
	case named == "" && len(s.steps) == 1:
		return s.steps[0], true
	case named == "":
		s.writeFailure(w, r, http.StatusBadRequest, "GATE_STEP_REQUIRED", fmt.Sprintf(
			"this deployment gates the design-gate steps %s; name the step this document is for", stepNames(s.steps)))
		return "", false
	case !record.TakesStep(s.steps, named):
		s.writeFailure(w, r, http.StatusConflict, "GATE_STEP_NOT_CONFIGURED", fmt.Sprintf(
			"this deployment does not gate the design-gate step %s; it gates %s", named, stepNames(s.steps)))
		return "", false
	}
	return named, true
}

// stepNames is steps as a refusal names them, in the order the deployment gates them.
func stepNames(steps []record.DesignStep) string {
	return strings.Join(record.StepNames(steps), ", ")
}

// stepDocument says whether document is the one the step records of its issue (stepRecords).
func stepDocument(step record.DesignStep, document dispatch.Approval) bool {
	return stepRecords(step, document.Primary, document.Role, document.Slug)
}

// stepRecords says whether a document that is its issue's primary document or not (primary),
// plays role beside it and is filed under slug is the one step records of its issue: the issue's
// primary document for spec, its Verification document for criteria, and its decisions document
// for decisions. It is the one statement of that binding, which a registration's document
// (stepDocument) and the parts a view is read by (documentPart) both read.
//
// A decisions document stored before Dispatch knew the role carries none, so its slug answers for
// it: that is what lets Dispatch and the daemon of this release deploy in either order.
func stepRecords(step record.DesignStep, primary bool, role, slug string) bool {
	switch step {
	case record.DesignStepSpec:
		return primary
	case record.DesignStepCriteria:
		return role == dispatch.ArtifactRoleVerification
	case record.DesignStepDecisions:
		return role == dispatch.ArtifactRoleDecisions || slug == dispatch.DecisionsSlug
	}
	return false
}

// gateIssue reads the issue a registration is for as Dispatch shows it: its latest event sequence,
// its parent and its documents. A failed read records nothing, so the architect's register_gate
// retries.
func (s *server) gateIssue(w http.ResponseWriter, r *http.Request, issue string) (dispatch.Issue, bool) {
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return dispatch.Issue{}, false
	}
	current, err := s.dispatch.GetIssue(r.Context(), issue)
	if err != nil {
		s.refuseFailedRead(w, r, &readFailure{what: issue, err: err})
		return dispatch.Issue{}, false
	}
	return current, true
}

// documentOfIssue refuses a document the issue does not carry, as the shipped daemon does: an
// already-approved document of any other issue would otherwise open this gate, or give this issue
// another issue's plan. A failed read records nothing, so the architect's operation, which the
// refusal names, retries. It returns the approval it read.
func (s *server) documentOfIssue(w http.ResponseWriter, r *http.Request, artifactID, issue, operation string) (dispatch.Approval, bool) {
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return dispatch.Approval{}, false
	}
	approval, err := s.dispatch.Approval(r.Context(), artifactID)
	switch {
	case dispatch.Missing(err), err == nil && approval.IssueKey != issue:
		s.writeFailure(w, r, http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE", fmt.Sprintf("%s is not a document of %s", artifactID, issue))
		return dispatch.Approval{}, false
	case err != nil:
		s.refuseDispatchRead(w, r, artifactID, operation)
		return dispatch.Approval{}, false
	}
	return approval, true
}

// maxChildReleaseAttempts and childReleaseBackoff bound release_children's retries of a todo child
// whose parent is not yet recorded (scheduleChildReleaseRetry).
const (
	maxChildReleaseAttempts = 3
	childReleaseBackoff     = time.Second
)

// enterTodoChild answers a released child Dispatch already shows todo that the daemon records
// nothing of once the release's call has applied (workflow's statusCall, which records such a
// child under its parent at work from Dispatch's view of it, as its todo event would record it: a
// child set todo before its parent was at work, by a person or another session, has no event to
// come, since Dispatch makes none for a status the issue already has). A child the call recorded,
// or that another release recorded already, clears its retry. prepared is why the release could
// not ready Dispatch's view of the child (Options.PrepareRepository), nil where it could, which
// the release answers now, after the call wrote the rest of the wave.
//
// The workflow records a child only under a recorded parent at work. A parent the same wave writes
// is counted at work by its own write in the call (R16 of dispatch://INFRA-3320) and recorded
// before its child, since the call takes the wave in its order (releaseOrder); one outside the
// wave may still be unrecorded. A child the call did not record is answered CHILD_NOT_RECORDED,
// retryable: each attempt is its own call, so the retry records the child once the parent is
// recorded. Its durable retry record bounds attempts with exponential backoff; after the limit, it
// queues one notice to the architect that released the child (scheduleChildReleaseRetry) instead
// of asking an architect to retry forever.
//
// A child the call did not record while it, or an unrecorded issue of its parent chain up to the
// first recorded one, resolves to a repository left out because GitHub did not answer its read of
// the repository — its GitHub App installation read, or its default-branch read past GitHub's rate
// limit — (Options.RepositoryUnread) ends nothing, and is answered by what the daemon will do once
// GitHub answers and it recovers that repository's issues from Dispatch's listing. When every
// unrecorded issue of that chain is todo (Dispatch shows it, or this wave writes it: todoNow), on
// any repository, the chain is recorded under the recorded one, parents first, by the recovery or
// by those issues' own events (one on a served repository whose event the consumer has not
// applied yet, or one on another unread repository that recovers later): the child stands
// released, neither started nor held, with nothing to retry, and the recovery tells the architect
// that owns the child's parent if it did not record it after all. Otherwise the chain stops at a
// parent that is not todo (one in backlog, say), which nothing will record: the child stands
// released too, and the architect that released it is told at once that it waits for that
// parent's release. Either way the wave goes on: the answer is the unread repository (the child
// pending), and ok is false only when the answer was written (a read failed).
// CHILD_NOT_RECORDED's few quick retries would run out in seconds of an outage and say the parent
// never reached the daemon, and an error would leave the rest of the wave unreleased.
func (s *server) enterTodoChild(w http.ResponseWriter, r *http.Request, grant credential.Grant, child dispatch.Issue, prepared error, read issueReader, todoNow func(string) bool) (string, bool) {
	recorded, err := s.recordedIssue(r.Context(), child.Key)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", child.Key))
		return "", false
	}
	if recorded != nil {
		return "", s.clearChildReleaseRetry(w, r, child.Key)
	}
	var later *intake.RetryLater
	switch {
	case errors.As(prepared, &later):
		s.writeFailure(w, r, http.StatusConflict, "CHILD_RELEASE_BACKOFF", fmt.Sprintf("%s: %v; retry after %s", child.Key, prepared, later.After))
		return "", false
	case prepared != nil:
		s.writeFailure(w, r, http.StatusBadGateway, "REPOSITORY_READ_FAILED", fmt.Sprintf("could not read the repository of %s: %v; retry release_children", child.Key, prepared))
		return "", false
	}
	observed := intake.ObservedIssue(child)
	if s.unread != nil {
		repo, ok, handled := s.enterOnUnreadRepository(w, r, grant, child, observed.ResolvedRepo, read, todoNow)
		if handled {
			return repo, ok
		}
	}
	return "", s.scheduleChildReleaseRetry(w, r, grant, child, observed.Parent)
}

// enterOnUnreadRepository is enterTodoChild's answer for a todo child its apply did not record,
// on resolved, when it or its parent chain is on an unread repository (handled); a child neither
// is on is not its to answer. It walks the chain up from the child's parent to the first recorded
// issue, reading each from Dispatch (read), and answers as enterTodoChild says: the unread
// repository, and whether the wave goes on (false once a read failed and its answer is written).
func (s *server) enterOnUnreadRepository(w http.ResponseWriter, r *http.Request, grant credential.Grant, child dispatch.Issue, resolved string, read issueReader, todoNow func(string) bool) (repo string, ok, handled bool) {
	if s.unread(resolved) {
		repo = resolved
	}
	// chain is every unrecorded issue from the child's parent up to the first recorded one.
	var chain []dispatch.Issue
	seen := map[string]bool{child.Key: true}
	for key := intake.ObservedIssue(child).Parent; key != "" && !seen[key]; {
		seen[key] = true
		above, err := s.recordedIssue(r.Context(), key)
		if err != nil {
			s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", key))
			return "", false, true
		}
		if above != nil {
			break
		}
		issue, read := read(w, r, key)
		if !read {
			return "", false, true
		}
		chain = append(chain, issue)
		if repo == "" && issue.ResolvedRepo != "" && s.unread(issue.ResolvedRepo) {
			repo = issue.ResolvedRepo
		}
		key = intake.ObservedIssue(issue).Parent
	}
	if repo == "" {
		return "", false, false
	}
	parent := intake.ObservedIssue(child).Parent
	var stop *dispatch.Issue
	for i := range chain {
		if chain[i].Status != "todo" && !todoNow(chain[i].Key) {
			stop = &chain[i]
			break
		}
	}
	// Every unrecorded issue up to the recorded one is todo: the recovery, or those issues' own
	// events on a served repository (a consumer behind, or another repository that recovers
	// later), record the chain under the recorded one, parents first. Nothing is told now: a child
	// still unrecorded when the repository's recovery has run is named then (tellReleaseWaits).
	if stop == nil {
		if !s.clearChildReleaseRetry(w, r, child.Key) {
			return "", false, true
		}
		ok := s.tellReleaseWait(w, r, grant, child, parent, repo, "")
		if ok {
			s.log.Info("api: release_children's todo child waits for GitHub to answer its repository's installation or default-branch read; the daemon records it then",
				"issue", child.Key, "architect_issue", grant.Issue, "repo", repo)
		}
		return repo, ok, true
	}
	blocker := *stop
	if !s.clearChildReleaseRetry(w, r, child.Key) {
		return "", false, true
	}
	return repo, s.tellReleaseWait(w, r, grant, child, parent, repo, fmt.Sprintf(
		"%s is todo but not recorded: the daemon records a child only under a recorded parent, and %s, on its parent chain, is %s and not recorded, so the recovery of %s (left out for now because GitHub did not answer its App installation read, or its default-branch read past GitHub's rate limit) will not record %s either. Release %s first, then release %s again.",
		child.Key, blocker.Key, blocker.Status, repo, child.Key, blocker.Key, child.Key)), true
}

// tellReleaseWait leaves child, released in grant's tree under parent, to the recovery of the
// unread repo (Options.AwaitRecovery), which tells an architect if it is still unrecorded then,
// and, when reason is given, tells the architect that released it at once in a
// child-release-stalled notice. The notice answers that architect's own release, so it is queued
// on that architect's own issue (grant.Issue): the owner walk takes it to that architect while it
// holds its role, and on up the tree once it does not, where a notice queued on the tree root
// would reach the root's architect even for a sub-architect's release. It reports whether the
// wave goes on: false once an answer is written, the notice's write having failed or the retry
// scheduled.
//
// The daemon may refuse the wait: repo was unread when this request read it, and its recovery has
// stopped waiting on GitHub for it since (Options.AwaitRecovery answering false). A child left to
// that recovery would be read by no pass and named to no architect, while this route had answered
// it released, so it is answered as a child on no unread repository is, with the child-release
// retry (scheduleChildReleaseRetry), and the notice is not written: its reason says the
// repository is left out, which the daemon has just said it no longer is.
func (s *server) tellReleaseWait(w http.ResponseWriter, r *http.Request, grant credential.Grant, child dispatch.Issue, parent, repo, reason string) bool {
	if s.await != nil && !s.await(repo, grant.Issue, child.Key, parent) {
		s.log.Info("api: release_children's child was on a repository the daemon no longer holds unread, so its release is retried rather than left to a recovery",
			"issue", child.Key, "architect_issue", grant.Issue, "repo", repo)
		return s.scheduleChildReleaseRetry(w, r, grant, child, parent)
	}
	if reason == "" {
		return true
	}
	// The notice is its own transaction, so it owns the commit hook its write registers (the
	// daemon's projectRecords, store.OnCommit), as ApplyFact does for a fact's rows.
	if err := store.Transact(r.Context(), s.pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := record.NewFencedRow(ctx, tx, s.records, grant.Issue, record.Notice{Kind: "child-release-stalled", Role: claim.RoleArchitect, Reason: reason}, time.Now().UTC())
		if err != nil {
			return err
		}
		return s.records.Enqueue(ctx, tx, row)
	}); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "NOTICE_UNAVAILABLE", fmt.Sprintf("could not tell the architect that %s waits", child.Key))
		return false
	}
	return true
}

func (s *server) clearChildReleaseRetry(w http.ResponseWriter, r *http.Request, issue string) bool {
	if err := store.Transact(r.Context(), s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return s.records.DeleteReleaseRetry(ctx, tx, issue)
	}); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not clear the release retry of %s", issue))
		return false
	}
	return true
}

// scheduleChildReleaseRetry answers a todo child the release could not record because its parent
// has not reached the daemon: CHILD_NOT_RECORDED with the backoff before the next attempt, the
// backoff itself while one runs, and after maxChildReleaseAttempts CHILD_RELEASE_STALLED, with one
// child-release-stalled notice queued on the releasing architect's own issue (grant.Issue), as
// tellReleaseWait queues its own. The retry is the child's, and every architect that may release
// the child shares it, so only the call that runs out the attempts queues the notice, and only its
// answer names the architect it told. A later call is told the notice was queued when the attempts
// ran out, naming no issue: the retry does not record whose call that was.
func (s *server) scheduleChildReleaseRetry(w http.ResponseWriter, r *http.Request, grant credential.Grant, child dispatch.Issue, parent string) bool {
	now := time.Now().UTC()
	var retry record.ReleaseRetry
	var exhausted, told, backoff bool
	// The retry is its own transaction, so it owns the commit hook the stalled notice's write
	// registers (the daemon's projectRecords, store.OnCommit), as ApplyFact does for a fact's rows.
	if err := store.Transact(r.Context(), s.pool, func(ctx context.Context, tx pgx.Tx) error {
		current, err := s.records.ReleaseRetry(ctx, tx, child.Key)
		if err != nil {
			return err
		}
		switch {
		case current != nil && current.Sequence == child.LastSeq && current.Exhausted:
			retry, exhausted = *current, true
			return nil
		case current != nil && current.Sequence == child.LastSeq && now.Before(current.NextAt):
			retry, backoff = *current, true
			return nil
		case current != nil && current.Sequence == child.LastSeq:
			retry = *current
			retry.Attempts++
		default:
			retry = record.ReleaseRetry{Issue: child.Key, Sequence: child.LastSeq, Attempts: 1}
		}
		retry.NextAt = now.Add(childReleaseBackoff << (retry.Attempts - 1))
		retry.Exhausted = retry.Attempts >= maxChildReleaseAttempts
		if retry.Exhausted {
			row, err := record.NewFencedRow(ctx, tx, s.records, grant.Issue, record.Notice{Kind: "child-release-stalled", Role: claim.RoleArchitect,
				Reason: fmt.Sprintf("%s is todo but its parent %s has not reached the daemon after %d releases; inspect the tree before trying again", child.Key, parent, retry.Attempts)}, now)
			if err != nil {
				return err
			}
			if err := s.records.Enqueue(ctx, tx, row); err != nil {
				return err
			}
			exhausted, told = true, true
		}
		return s.records.PutReleaseRetry(ctx, tx, retry)
	}); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RETRY_UNAVAILABLE", fmt.Sprintf("could not record the release retry of %s", child.Key))
		return false
	}
	if exhausted {
		notified := "a child-release-stalled notice was queued when the attempts ran out"
		if told {
			notified = fmt.Sprintf("the architect of %s was notified", grant.Issue)
		}
		s.writeFailure(w, r, http.StatusConflict, "CHILD_RELEASE_STALLED", fmt.Sprintf("%s is todo but its parent %s has not reached the daemon after %d attempts; %s", child.Key, parent, retry.Attempts, notified))
		return false
	}
	if backoff {
		s.writeFailure(w, r, http.StatusConflict, "CHILD_RELEASE_BACKOFF", fmt.Sprintf("%s is waiting for %s's release; retry after %s", child.Key, parent, time.Until(retry.NextAt).Round(time.Millisecond)))
		return false
	}
	s.writeFailure(w, r, http.StatusConflict, "CHILD_NOT_RECORDED", fmt.Sprintf("%s is todo but not recorded: the daemon records a child only under a recorded parent (%s) of a live tree; retry after %s", child.Key, parent, childReleaseBackoff<<(retry.Attempts-1)))
	return false
}

// issueReader reads one issue for a request, answering its own failures.
type issueReader func(w http.ResponseWriter, r *http.Request, key string) (dispatch.Issue, bool)

// dispatchReads reads each issue from Dispatch once per request, for a route that brings the
// daemon's copy of Dispatch's issues up to date before it decides over the copy (the status
// calls: set_status, rerun_child and release_children). A wave's children share the issues above
// them, and the walk up each chain would read every one of them again for every child.
//
// An issue whose detail read fails, other than one Dispatch says does not exist, is read from the
// project's listing instead, once per request (listedIssues). Dispatch's detail read loads the
// issue's documents, asks, children and references beside it; in a deployment it failed for some
// children again and again while the listing, which the daemon's boot reads in full, still carried
// them, so every release_children naming one of them answered 502 and the child, already todo,
// was never recorded. A call needs only what the listing carries: the status, the parent chain,
// the blockers and the issue's sequence. An issue the listing does not carry either is a failed
// read (listingMiss).
type dispatchReads struct {
	s       *server
	seen    map[string]dispatch.Issue
	listed  map[string]dispatch.IssueSummary
	listErr error
}

func (s *server) dispatchReads() *dispatchReads {
	return &dispatchReads{s: s, seen: map[string]dispatch.Issue{}}
}

// listingMiss is a failed detail read of key that the project's listing does not answer either:
// read is the detail read's failure, and list the listing's, nil where the listing was read and
// does not carry key.
type listingMiss struct {
	key        string
	read, list error
}

func (m *listingMiss) Error() string {
	listing := "does not carry it"
	if m.list != nil {
		listing = fmt.Sprintf("failed too (%v)", m.list)
	}
	return fmt.Sprintf("Dispatch read of %s failed (%v), and the project listing %s", m.key, m.read, listing)
}

// get is Dispatch's issue key, read once for the request: its detail read, else the project's
// listing's summary of it, under the daemon's own project, which the listing is of. A detail read
// Dispatch answers with a missing issue fails as it is (dispatch.Missing).
func (d *dispatchReads) get(ctx context.Context, key string) (dispatch.Issue, error) {
	if issue, read := d.seen[key]; read {
		return issue, nil
	}
	issue, err := d.s.dispatch.GetIssue(ctx, key)
	switch {
	case dispatch.Missing(err):
		return dispatch.Issue{}, err
	case err != nil:
		if d.listed == nil && d.listErr == nil {
			d.listed, d.listErr = d.s.listedIssues(ctx)
		}
		summary, inListing := d.listed[key]
		if !inListing {
			return dispatch.Issue{}, &listingMiss{key: key, read: err, list: d.listErr}
		}
		d.s.log.Warn("api: Dispatch's detail read of an issue failed, so the call read it from the project listing", "issue", key, "error", err)
		issue = summary.Issue()
		issue.Project = d.s.dispatchProject
	}
	if issue.Key == "" {
		issue.Key = key
	}
	d.seen[key] = issue
	return issue, nil
}

// issue is get for an issue the request names, answering its failure: 404 ISSUE_NOT_FOUND for an
// issue Dispatch does not have, and 502 DISPATCH_FAILED, with both errors, for a read the listing
// did not answer either.
func (d *dispatchReads) issue(w http.ResponseWriter, r *http.Request, key string) (dispatch.Issue, bool) {
	issue, err := d.get(r.Context(), key)
	switch {
	case dispatch.Missing(err):
		d.s.writeFailure(w, r, http.StatusNotFound, "ISSUE_NOT_FOUND", fmt.Sprintf("%s is not a Dispatch issue", key))
		return dispatch.Issue{}, false
	case err != nil:
		d.s.writeFailure(w, r, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("%v; retry the call", err))
		return dispatch.Issue{}, false
	}
	return issue, true
}

// readings is Dispatch's reading of each of keys and of each issue above it on its Dispatch chain,
// up to the top or to record.MaxChainLinks links (intake.ReadingOf), each issue read once: what a
// status call brings the copy of Dispatch's issues up to date with before the workflow decides it
// over the copy (intake.StatusCall.Readings; decision 5 of dispatch://INFRA-4730). Every link is
// read, a row the copy holds included, since a stale row and a loop two late moves made are rows
// whose values are wrong. A parent of another project ends the walk as no parent does
// (record.InProjectParent), since it ends the chain (R19 of dispatch://INFRA-3320), so the call
// reads and writes nothing of another project. A link the read cannot reach ends that chain's walk
// there: the copy decides on what it holds, and an unknown chain refuses the call (CHAIN_UNKNOWN).
func (d *dispatchReads) readings(ctx context.Context, keys []string) []intake.IssueStatus {
	var readings []intake.IssueStatus
	taken := map[string]bool{}
	for _, key := range keys {
		next := key
		for links := 0; next != "" && !taken[next] && links <= record.MaxChainLinks; links++ {
			taken[next] = true
			issue, err := d.get(ctx, next)
			if err != nil {
				break
			}
			if reading, ok := intake.ReadingOf(next, issue); ok {
				readings = append(readings, reading)
			}
			parent := ""
			if issue.Parent != nil {
				parent = record.InProjectParent(next, *issue.Parent)
			}
			next = parent
		}
	}
	return readings
}

// listedIssues is Dispatch's listing of the daemon's project, by key.
func (s *server) listedIssues(ctx context.Context) (map[string]dispatch.IssueSummary, error) {
	summaries, err := s.dispatch.ListIssues(ctx, s.dispatchProject, nil)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]dispatch.IssueSummary, len(summaries))
	for _, summary := range summaries {
		listed[summary.Key] = summary
	}
	return listed, nil
}

// applyStatusCall applies a route's status call (intake.StatusCall) and answers its refusal. It
// reports the call's result, and true, once the call committed with no refusal, for the route to
// answer.
func (s *server) applyStatusCall(w http.ResponseWriter, r *http.Request, route string, call intake.StatusCall) (intake.Result, bool) {
	if s.pool == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return intake.Result{}, false
	}
	if s.callJudged != nil && call.Architect != "" {
		keys := make([]string, len(call.Issues))
		for i, issue := range call.Issues {
			keys[i] = issue.Key
		}
		s.callJudged(r.Context(), string(call.Op), keys)
	}
	result, err := s.apply(r.Context(), "api", requestFactID(route), call)
	switch {
	case err != nil:
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the call")
		return intake.Result{}, false
	case result.Refusal != nil:
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return intake.Result{}, false
	}
	return result, true
}

// waveRelease releases children under the architect's own issue into Legion's work (R19 of
// dispatch://INFRA-3320). It reads each child, and each issue above it on its Dispatch chain, from
// Dispatch, and applies one call that brings the copy of Dispatch's issues up to date with those
// reads and decides the whole wave over it, in one transaction (intake.StatusCall, the workflow's
// statusCall; decision 5 of dispatch://INFRA-4730): every child is checked before anything is
// written — on a known chain, under the caller's own issue, in triage, backlog or icebox (written
// todo) or todo already (written nothing), beneath no ancestor the daemon has already accepted,
// and under a parent in work or one the same wave writes before it (PARENT_NOT_RELEASED) — so a
// wave that names one child the call refuses releases none. Each todo is then queued as a status
// row on the child's lane, in releaseOrder, a child's row waiting for the row of a parent the same
// wave writes, and counted on its record before Dispatch echoes it (R16): a child the daemon
// records returns to work, and one it does not record is recorded under its parent at work from
// Dispatch's view of it, readied as its event would be (Options.PrepareRepository). A child
// Dispatch shows todo already is recorded the same way, and one the call could not record is
// answered by enterTodoChild. The ownership rule is asked of the wave before any view is readied
// (callOwned), so a wave naming a child its caller does not take readies none and spends no GitHub
// read on it; the call still answers the refusal the workflow judges where it commits.
func (s *server) waveRelease(w http.ResponseWriter, r *http.Request) {
	var req WaveReleaseRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}) {
		return
	}
	grant, ok := s.architectGrant(w, r, req.GrantID)
	if !ok {
		return
	}
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return
	}
	if req.Issues == nil {
		req.Issues = []string{}
	}
	// A wave that names a child twice releases it once, at its first place: a second write would
	// make Dispatch append another todo event for the same move, the answer would name the child
	// twice, and releaseOrder would add up the child's depth once per naming, so it could sort the
	// child after a child of its own, which would then be applied before its parent is recorded.
	named := make(map[string]bool, len(req.Issues))
	req.Issues = slices.DeleteFunc(req.Issues, func(key string) bool {
		twice := named[key]
		named[key] = true
		return twice
	})
	for _, issue := range req.Issues {
		if !claim.IsIssueKey(issue) {
			s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issues must contain issue keys")
			return
		}
	}
	reads := s.dispatchReads()
	children := make([]releasedChild, 0, len(req.Issues))
	for _, issue := range req.Issues {
		view, ok := reads.issue(w, r, issue)
		if !ok {
			return
		}
		children = append(children, releasedChild{view: view, write: releaseWrites(view.Status)})
	}
	// The records are read before anything is written, so releaseOutcome counts a child the
	// release started as one this request started.
	if err := s.readReleasedBefore(r.Context(), children); err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the records of the children to release")
		return
	}
	ordered := releaseOrder(children)
	call := intake.StatusCall{Op: intake.ReleaseChildrenCall, Architect: grant.Claim, Readings: reads.readings(r.Context(), req.Issues)}
	for _, child := range ordered {
		call.Issues = append(call.Issues, intake.StatusCallIssue{Key: child.view.Key})
	}
	owned, err := s.callOwned(r.Context(), call, intake.ReleaseCall)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not judge the release over the copy of Dispatch's issues")
		return
	}
	for i := range ordered {
		child := &ordered[i]
		if !owned || child.before != nil {
			continue
		}
		call.Issues[i].View, child.prepared = s.preparedView(r.Context(), child.view)
		if child.prepared != nil && child.write {
			s.log.Warn("api: release_children could not ready a child it writes for its record; the write's answer records it",
				"issue", child.view.Key, "error", child.prepared)
		}
	}
	result, ok := s.applyStatusCall(w, r, "waves/release", call)
	if !ok {
		return
	}
	// A todo child the call left unrecorded because a repository it or its parent chain resolves
	// to is unread (Options.RepositoryUnread) ends nothing: enterTodoChild answers it as the
	// recovery will leave it and the wave goes on, and a child the call wrote under it is told
	// (tellWrittenUnderPending).
	byKey := make(map[string]releasedChild, len(children))
	for _, child := range children {
		byKey[child.view.Key] = child
	}
	todoNow := func(key string) bool {
		_, inWave := byKey[key]
		return inWave
	}
	pending := map[string]string{}
	for _, child := range ordered {
		if child.write {
			continue
		}
		repo, ok := s.enterTodoChild(w, r, grant, child.view, child.prepared, reads.issue, todoNow)
		if !ok {
			return
		}
		if repo != "" {
			pending[child.view.Key] = repo
		}
	}
	for _, child := range ordered {
		if child.write && !s.tellWrittenUnderPending(w, r, grant, child.view, byKey, pending) {
			return
		}
	}
	started, held, err := s.releaseOutcome(r.Context(), children)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read what the released children wait on")
		return
	}
	s.writeAnswer(w, r, AnswerWaveRelease, http.StatusOK, WaveReleaseResponse{Released: req.Issues, Started: started, Held: held, Queued: queuedOf(result)})
}

// tellWrittenUnderPending answers a child the release wrote todo, written, whose parent chain
// within the wave (byKey) reaches a todo child the release left to the recovery of an unread
// repository (pending: each such child's repository, enterTodoChild). The workflow records a child
// only under a recorded parent, so the call queued its todo and could not record it, and its
// todo's event will not record it either while that parent is unrecorded. It is left to the same
// recovery (tellReleaseWait), and the architect that released it is told at once to release it
// again once that parent is recorded, when release_children records a child Dispatch shows todo.
// A child the call recorded, or one under no such child, is not its to answer. It reports whether
// the wave goes on: false once an answer is written.
func (s *server) tellWrittenUnderPending(w http.ResponseWriter, r *http.Request, grant credential.Grant, written dispatch.Issue, byKey map[string]releasedChild, pending map[string]string) bool {
	var above, repo string
	seen := map[string]bool{written.Key: true}
	for parent := written.Parent; parent != nil && !seen[*parent]; {
		key := *parent
		seen[key] = true
		if unread, left := pending[key]; left {
			above, repo = key, unread
			break
		}
		child, inWave := byKey[key]
		if !inWave {
			break
		}
		parent = child.view.Parent
	}
	if above == "" {
		return true
	}
	recorded, err := s.recordedIssue(r.Context(), written.Key)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", written.Key))
		return false
	}
	if recorded != nil {
		return true
	}
	return s.tellReleaseWait(w, r, grant, written, intake.ObservedIssue(written).Parent, repo, fmt.Sprintf(
		"%s's todo is queued, but it is not recorded: the daemon records a child only under a recorded parent, and %s, above it in this release, is todo and left unrecorded until GitHub answers for %s. Release %s again once %s is recorded.",
		written.Key, above, repo, written.Key, above))
}

// releaseWrites says whether release_children writes todo on a child Dispatch shows at status:
// triage, backlog and icebox are written, todo is in work already, and the workflow refuses any
// other (ISSUE_ALREADY_RELEASED).
func releaseWrites(status string) bool {
	return status == "triage" || status == "backlog" || status == "icebox"
}

// preparedView is Dispatch's view of an issue the daemon does not record, readied as its event
// would be (Options.PrepareRepository): what the workflow records a released child from. A server
// given no preparation takes the view as it is.
func (s *server) preparedView(ctx context.Context, view dispatch.Issue) (*intake.DispatchIssue, error) {
	observed := intake.ObservedIssue(view)
	if s.prepare != nil {
		prepared, err := s.prepare(ctx, observed)
		if err != nil {
			return nil, err
		}
		observed = prepared
	}
	return &observed, nil
}

// errOnlyJudged ends callOwned's transaction, which writes a call's readings into the copy only to
// judge the call over them: the error rolls those writes back.
var errOnlyJudged = errors.New("the call was only judged")

// callOwned says whether the ownership rule refuses none of call's issues (intake.CallRefusal
// under rule), judged as the workflow judges each where the call's fact commits: over the copy of
// Dispatch's issues as the call's readings bring it up to date (blockers.Mirror), in a transaction
// of its own that is rolled back once the rule has read the chains, so nothing of it stays. A
// route asks it before it readies any issue's view (preparedView), since a call it refuses is
// refused where the fact commits whatever views it carries.
func (s *server) callOwned(ctx context.Context, call intake.StatusCall, rule intake.ArchitectCall) (bool, error) {
	if s.pool == nil {
		return false, errors.New("record dependencies are unavailable")
	}
	keys := make([]string, len(call.Issues))
	for i, issue := range call.Issues {
		keys[i] = issue.Key
	}
	owned := true
	err := store.Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := blockers.NewMirror(s.records, s.dispatchProject).Apply(ctx, tx, call); err != nil {
			return err
		}
		chains, err := record.ReadChainsOf(ctx, tx, s.records, s.dispatchProject, keys)
		if err != nil {
			return err
		}
		for _, key := range keys {
			// The rule reads the project only for its claim token, which s.project is.
			refusal, err := intake.CallRefusal(ctx, tx, s.records, s.project, chains, call.Architect, "", key, rule)
			if err != nil {
				return err
			}
			if refusal != nil {
				owned = false
				break
			}
		}
		return errOnlyJudged
	})
	if !errors.Is(err, errOnlyJudged) {
		return false, err
	}
	return owned, nil
}

// releasedChild is one child of a release as its answer judges it: Dispatch's view of it as the
// request read it, whether Dispatch shows it at a status the release writes todo over
// (releaseWrites) or todo already, its record as it stood before any of the request's writes
// (readReleasedBefore), and why its view could not be readied for its record
// (Options.PrepareRepository), nil where it could or where the daemon records it already.
type releasedChild struct {
	view     dispatch.Issue
	write    bool
	before   *record.Issue
	prepared error
}

// readReleasedBefore reads each child's record before the release writes anything, in one
// read-only transaction, so releaseOutcome can tell a first phase this request started from one
// the child was already in.
func (s *server) readReleasedBefore(ctx context.Context, children []releasedChild) error {
	if s.pool == nil {
		return errors.New("record dependencies are unavailable")
	}
	return store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		for i := range children {
			var err error
			if children[i].before, err = s.records.Issue(ctx, tx, children[i].view.Key); err != nil {
				return err
			}
		}
		return nil
	})
}

// releaseOrder is the wave in the request's order, except that each child comes after every child
// of the wave on its Dispatch parent chain: the workflow records a child only under a recorded
// parent, so a parent the wave releases must be recorded before its child is applied. A chain is
// followed through the wave's own children only, since a parent outside the wave is recorded or not
// whatever the order.
func releaseOrder(children []releasedChild) []releasedChild {
	byKey := make(map[string]releasedChild, len(children))
	for _, child := range children {
		byKey[child.view.Key] = child
	}
	depth := make(map[string]int, len(children))
	for _, child := range children {
		seen := map[string]bool{child.view.Key: true}
		for parent := child.view.Parent; parent != nil && !seen[*parent]; {
			above, inWave := byKey[*parent]
			if !inWave {
				break
			}
			seen[*parent] = true
			depth[child.view.Key]++
			parent = above.view.Parent
		}
	}
	ordered := slices.Clone(children)
	slices.SortStableFunc(ordered, func(a, b releasedChild) int { return cmp.Compare(depth[a.view.Key], depth[b.view.Key]) })
	return ordered
}

// releaseOutcome answers what this request did with each of its children, once its writes and
// observations are in, in the request's own order. It reads each child's record again and the
// mirror rows of the issues the children wait on with one query (blockers.Load), in one read-only
// transaction: a wave's children share the issues they wait on.
//
// A child is started when this request started its first phase (startedByRelease): the request
// records every child it releases, one it writes todo — out of backlog, or parked at done — by its
// own write (ApplyOwnStatusWrite), and one Dispatch already shows todo by Dispatch's view of it
// (enterTodoChild). Otherwise it is held when it waits on an issue that is not done, unless it is
// running a phase it was already in: what it waits on is judged from the blockers Dispatch showed
// the request (blockers.UnfinishedOf), which the workflow's enterChild held a recorded child at
// phase.Admitted on, and which still answers for a written child the workflow could not record yet
// (a parent outside the wave that the daemon does not record), whose write's event, when the
// consumer delivers it, enters it on that same wait.
//
// A child in neither list is one this request started nothing for and that waits on nothing — its
// tree's closed design gate holds it until the gate's opening starts it, or the workflow could not
// record it yet and its write's event is still to start it — or one already running a phase before
// the request, which holds nothing back.
func (s *server) releaseOutcome(ctx context.Context, children []releasedChild) ([]string, []HeldChild, error) {
	if s.pool == nil {
		return nil, nil, errors.New("record dependencies are unavailable")
	}
	var waitedOn []string
	for _, child := range children {
		waitedOn = append(waitedOn, child.view.BlockedBy...)
	}
	// The mirror's rows are keyed by the Dispatch project key the issues are filed under, never
	// by the claim project token: a read with the token finds nothing wherever the two differ,
	// which in a deployment is always (Options.Project, Options.DispatchProject).
	started, held := []string{}, []HeldChild{}
	err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := blockers.Load(ctx, tx, s.records, s.dispatchProject, waitedOn)
		if err != nil {
			return err
		}
		for _, child := range children {
			after, err := s.records.Issue(ctx, tx, child.view.Key)
			if err != nil {
				return err
			}
			switch {
			case startedByRelease(child.before, after):
				started = append(started, child.view.Key)
			case runsPhase(after):
			default:
				// The mirror's rows are keyed as the store keeps the keys (storetext.Strings).
				if unfinished := blockers.UnfinishedOf(rows, storetext.Strings(child.view.BlockedBy)); len(unfinished) > 0 {
					held = append(held, HeldChild{Issue: child.view.Key, BlockedBy: unfinished})
				}
			}
		}
		return nil
	})
	return started, held, err
}

// startedByRelease says whether the release started a child's first phase: its record after the
// release runs a phase of a run that, before it, had none — the child was unrecorded, still at
// phase.Admitted, or in an earlier run (a parked child at done) that the release's todo re-entered
// as a new generation. A record already running the same run, or a parked one the todo event has
// not reached yet, was started by no part of this request.
func startedByRelease(before, after *record.Issue) bool {
	if !runsPhase(after) {
		return false
	}
	return before == nil || before.Phase == phase.Admitted || before.Generation != after.Generation
}

// runsPhase says whether a recorded child is in a phase of a run: past phase.Admitted, where it
// waits to start, and short of phase.Done, where it stands out of the workflow until a todo
// re-enters it.
func runsPhase(issue *record.Issue) bool {
	return issue != nil && issue.Phase != phase.Admitted && issue.Phase != phase.Done
}

// phaseBackward applies a backward move: a phase worker's on its own issue (the grant's), or an
// architect's on an issue it acts on (ownsOnChain, as sign_off), named by the request, from
// accepting. The workflow decides whether the requester runs the phase (BACKWARD_REFUSED), so a
// move it applies ends the requester's own step — the worker's phase, or accepting, the owning
// architect's — which apply waits for the requester's stream before.
func (s *server) phaseBackward(w http.ResponseWriter, r *http.Request) {
	var req PhaseBackwardRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID},
		field{"to", string(req.To)},
		field{"reason", req.Reason},
	) {
		return
	}
	if !validPhase(req.To) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_PHASE", "to is not a phase")
		return
	}
	if req.Issue != "" && !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller && grant.Role == claim.RoleArchitect {
		if !s.requireFailureFields(w, r, field{"issue", req.Issue}) {
			return
		}
		architect, recorded, ok := s.ownsOnChain(w, r, req.GrantID, req.Issue, intake.BackwardCall)
		if !ok {
			return
		}
		entry, ok := s.openStep(w, r, req.Issue)
		if !ok {
			return
		}
		s.applyFact(w, r, requestFactID("phase/backward"),
			intake.BackwardMove{Issue: req.Issue, Requester: claim.RoleArchitect, To: req.To, Reason: req.Reason,
				By: architect.Claim, Generation: recorded.Generation, Entry: entry}, PhaseBackwardResponse{})
		return
	}
	grant, ok = s.phaseWorkerGrant(w, r, req.GrantID)
	if !ok {
		return
	}
	if req.Issue != "" && req.Issue != grant.Issue {
		s.writeFailure(w, r, http.StatusBadRequest, "ISSUE_NOT_GRANTS", fmt.Sprintf("a phase worker moves only its own issue %s, not %s", grant.Issue, req.Issue))
		return
	}
	s.applyFact(w, r, requestFactID("phase/backward"),
		intake.BackwardMove{Issue: grant.Issue, Requester: grant.Role, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})
}

func (s *server) phaseRetry(w http.ResponseWriter, r *http.Request) {
	var req PhaseRetryRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"decision", string(req.Decision)},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if req.Decision != intake.RetryDecision && req.Decision != intake.EscalateDecision {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_DECISION", "decision must be retry or escalate")
		return
	}
	grant, recorded, ok := s.ownsOnChain(w, r, req.GrantID, req.Issue, intake.RetryCall)
	if !ok {
		return
	}
	s.applyFact(w, r, requestFactID("phase/retry"),
		intake.RetryOrEscalate{Issue: req.Issue, Decision: req.Decision, Reason: req.Reason, By: grant.Claim, Generation: recorded.Generation}, PhaseRetryResponse{})
}

func (s *server) signOff(w http.ResponseWriter, r *http.Request) {
	var req SignOffRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	grant, recorded, ok := s.ownsOnChain(w, r, req.GrantID, req.Issue, intake.SignOffCall)
	if !ok {
		return
	}
	entry, ok := s.openStep(w, r, req.Issue)
	if !ok {
		return
	}
	s.applyFact(w, r, requestFactID("signoff"), intake.SignOff{Issue: req.Issue, By: grant.Claim, Generation: recorded.Generation, Entry: entry}, SignOffResponse{})
}

// closeRoot ends a root while it has started no phase — admitted, or waiting on a person
// (workflow's closeRoot): the root's own architect only (intake.CloseCall), with a reason Dispatch
// takes as a message.
func (s *server) closeRoot(w http.ResponseWriter, r *http.Request) {
	var req RootCloseRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"reason", req.Reason}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	fact := intake.CloseRoot{Issue: req.Issue, Reason: req.Reason}
	// The close posts the reason on the issue and writes the status after it (the outbox's status
	// row), and Dispatch refuses a body past the bounds it publishes, so a reason it would refuse
	// is refused here, before anything is recorded. Refused on the post instead, the reason would
	// be on no issue, and the daemon's record would read done while the Dispatch issue kept the
	// status it had. The text measured is the post the outbox makes, not the reason alone
	// (record.LongestPost), and the reader is the server's own, so the daemon refuses exactly
	// what Dispatch refuses (dispatchbody.Check). That one read answers the decision block too:
	// the post's marker is an HTML comment, which the document parser refuses, and the server
	// reads a refused body for the card its reader would draw all the same
	// (pmdoc.FindDecisionCard).
	if over := dispatchbody.Check(record.LongestPost(fact.Message())); over != nil {
		// What the reason is refused for decides what the architect does about it. A decision
		// block is refused however short the reason is, since a question in a message is one
		// nobody can answer, so the refusal names the block as the cause and where the question
		// belongs rather than asking for a shorter reason.
		if dispatchbody.IsDecisionBlock(over) {
			s.writeFailure(w, r, http.StatusBadRequest, "CLOSE_REASON_DECISION_BLOCK",
				fmt.Sprintf("the reason holds a decision block, and the close posts the reason on the issue as a message, where nobody can answer one: %s; the close was not applied, so close again with a reason that states the decision rather than asking for it", over))
			return
		}
		s.writeFailure(w, r, http.StatusBadRequest, "CLOSE_REASON_TOO_LONG",
			fmt.Sprintf("the reason makes a message Dispatch refuses: %s; shorten it", over))
		return
	}
	grant, recorded, ok := s.ownsOnChain(w, r, req.GrantID, req.Issue, intake.CloseCall)
	if !ok {
		return
	}
	fact.By, fact.Generation = grant.Claim, recorded.Generation
	s.applyFact(w, r, requestFactID("roots-close"), fact, EmptyResponse{})
}

// openStep is the run of the step issue stands in (record.Store.OpenStep), read in one read-only
// transaction: what a request about accepting, sign_off and request_backward_move, names on its
// fact, so the workflow refuses it where another run of accepting stands under the fact lock. A
// failed read is answered 500 RECORD_UNAVAILABLE.
func (s *server) openStep(w http.ResponseWriter, r *http.Request, issue string) (record.StepEntry, bool) {
	entry, err := func() (record.StepEntry, error) {
		if s.pool == nil || s.records == nil {
			return record.StepEntry{}, errors.New("record dependencies are unavailable")
		}
		var entry record.StepEntry
		err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			entry, err = s.records.OpenStep(ctx, tx, issue)
			return err
		})
		return entry, err
	}()
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the step the issue stands in")
		return record.StepEntry{}, false
	}
	return entry, true
}

// removedChildRoute answers an older plugin's call of a child route that this daemon no longer
// serves. The owning architect now takes a child out of the workflow by its own status write in
// Dispatch, and the daemon takes that write from its event (workflow's sessionStatusWrite). So the
// route reads no body, applies no fact and writes nothing to Dispatch. It answers 410
// ROUTE_REMOVED with a reason that says which status to write, followed by the rules of that
// write (record.EndChildRule). An older plugin's tool gives its agent the code and the reason of
// any 4xx as the result of the call. A 502 or a 503 would be retried, and STALE_PLUGIN_CONTRACT
// would end the plugin. The reason names the child by a placeholder, because the route reads no
// body.
func (s *server) removedChildRoute(w http.ResponseWriter, r *http.Request) {
	s.writeFailure(w, r, http.StatusGone, "ROUTE_REMOVED", "This route is removed. Write backlog, icebox or done on the child in Dispatch "+
		"(dispatch issue-update --issue <KEY> --status <status>). Add --reason when the status is done. "+
		"The daemon takes the move from the Dispatch event of that write. "+record.EndChildRule)
}

// rerunChild runs again an issue under the architect's own on the Dispatch chain once it is out of
// Legion's work, by its todo, the move a person would make (decision 8 of dispatch://INFRA-4730).
// It reads the child, and each issue above it on its chain, from Dispatch, and applies one call
// that brings the copy of Dispatch's issues up to date with those reads and decides over it, in
// one transaction (intake.StatusCall, the workflow's statusCall; decision 5): on a known chain,
// under the caller's own issue, recorded, out of Legion's work (CHILD_RUNNING says what to do
// first), beneath no ancestor the daemon has already accepted (CHILD_UNDER_ACCEPTED_PARENT), and
// under a parent in work, else with the parent argument, which moves each issue above it that is
// not in work to in_progress with it (PARENT_ARGUMENT_REQUIRED, R24 of dispatch://INFRA-3320). The
// todo is queued as a status row on the child's lane and counted on its record before Dispatch
// echoes it (R16): a child Dispatch shows in backlog or icebox, whoever wrote that status, opens a
// new run (R20), whose slot starts the first step of its plan, the newest plan its owner gave or
// the project's default steps, and any other returns to work as a person's todo would.
func (s *server) rerunChild(w http.ResponseWriter, r *http.Request) {
	var req RerunChildRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if req.Parent != "" && req.Parent != intake.IncludeArgument {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_PARENT", "parent must be include")
		return
	}
	grant, ok := s.architectGrant(w, r, req.GrantID)
	if !ok {
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
	if result, ok := s.applyStatusCall(w, r, "children/rerun", intake.StatusCall{Op: intake.RerunChildCall, Architect: grant.Claim,
		Issues:   []intake.StatusCallIssue{{Key: req.Issue, Status: "todo", Parent: req.Parent}},
		Readings: reads.readings(r.Context(), []string{req.Issue})}); ok {
		s.writeAnswer(w, r, AnswerStatusCall, http.StatusOK, StatusCallResponse{Queued: queuedOf(result)})
	}
}

func validPhase(value phase.Phase) bool {
	return slices.Contains(phase.All, value)
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}
