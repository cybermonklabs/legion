package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/dispatchblocks"
	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A register_gate call may name, beside the root's own document, a basis: the same step's document
// of an ancestor on the root's Dispatch child_of chain, at a version, with the architect's reason
// (dispatch://INFRA-3320, R1). The step then opens on the ancestor's approval, or waits, closed,
// on the ancestor's open approval request, and asks nobody for an approval of the root's own
// document. Everything here reads Dispatch and the record, and decides; nothing here writes. A
// refusal records nothing and names what Dispatch shows, so the architect can act on it.

// reasonRunes is the longest reason a registration on a basis carries, in characters (R1's seventh
// condition). The daemon posts the reason on the root's issue, in the message that names the
// approval the step opened on (R2).
const reasonRunes = 1000

// requirementBlock and checkBlock are the two typed blocks a view is read by, as Dispatch's block
// schema names them (pmdoc/schema/blocks.json), and servesAttribute the check's attribute that
// names the requirement blocks it proves.
const (
	requirementBlock = "requirement"
	checkBlock       = "check"
	servesAttribute  = "serves"
)

// basisNamed says whether the call names a basis by any of its three fields.
func (req GateRegisterRequest) basisNamed() bool {
	return req.BasisArtifactID != "" || req.BasisVersion != 0 || req.Reason != ""
}

// refuseBasisInput refuses a basis the call names malformed, before anything is read: the three
// fields come together or not at all, the document is a UUID, the version positive, and the reason
// 1 to reasonRunes characters with no decision block in it. A decision block is read as Dispatch
// reads one (dispatchbody.Check), since the reason is posted on the root's issue as a message,
// where nobody can answer one. A reason inside reasonRunes passes Dispatch's bounds on a body's
// blocks and their length, so the decision block is the one refusal Check can answer for it.
func refuseBasisInput(w http.ResponseWriter, req GateRegisterRequest) bool {
	if !req.basisNamed() {
		return false
	}
	var missing []string
	if req.BasisArtifactID == "" {
		missing = append(missing, "basisArtifactId")
	}
	if req.BasisVersion == 0 {
		missing = append(missing, "basisVersion")
	}
	if req.Reason == "" {
		missing = append(missing, "reason")
	}
	switch characters := utf8.RuneCountInString(req.Reason); {
	case len(missing) > 0:
		writeFailure(w, http.StatusBadRequest, "BASIS_INCOMPLETE", fmt.Sprintf(
			"basisArtifactId, basisVersion and reason come together or not at all, and this call leaves out %s; name all three to register on an ancestor's approval, or none to register the issue's own document",
			strings.Join(missing, " and ")))
		return true
	case !validUUID(req.BasisArtifactID):
		writeFailure(w, http.StatusBadRequest, "INVALID_ARTIFACT_ID", "basisArtifactId must be a UUID")
		return true
	case req.BasisVersion < 1:
		writeFailure(w, http.StatusBadRequest, "INVALID_VERSION", "basisVersion must be positive")
		return true
	case characters > reasonRunes:
		writeFailure(w, http.StatusBadRequest, "REASON_INVALID", fmt.Sprintf(
			"the reason is %d characters, and a registration on a basis carries a reason of 1 to %d characters; shorten it", characters, reasonRunes))
		return true
	}
	if over := dispatchbody.Check(req.Reason); dispatchbody.IsDecisionBlock(over) {
		writeFailure(w, http.StatusBadRequest, "REASON_HOLDS_DECISION_BLOCK", fmt.Sprintf(
			"the reason holds a decision block, and the daemon posts the reason on the issue as a message, where nobody can answer one: %s; nothing was recorded, so register again with a reason that states the decision rather than asking for it",
			over))
		return true
	}
	return false
}

// readFailure is a Dispatch read that failed, and what it was reading, as the refusal names it.
type readFailure struct {
	what string
	err  error
}

func (f *readFailure) Error() string { return "read " + f.what + ": " + f.err.Error() }
func (f *readFailure) Unwrap() error { return f.err }

// refuseFailedRead answers a failed Dispatch read as register_gate answers every one: 502
// DISPATCH_FAILED, nothing recorded, so the architect's register_gate retries.
func refuseFailedRead(w http.ResponseWriter, err error) {
	what := "Dispatch"
	var failure *readFailure
	if errors.As(err, &failure) {
		what = failure.what
	}
	writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("Dispatch read of %s failed; retry register_gate", what))
}

// gateAncestors walks root's Dispatch child_of chain upward from its parent, nearest first,
// reading each issue whole, and calls visit on each until visit answers stop or the chain ends. A
// chain that comes back to an issue it already read ends there, so a cycle Dispatch should never
// hold cannot hold the call.
func (s *server) gateAncestors(ctx context.Context, root dispatch.Issue, visit func(above dispatch.Issue) (stop bool, err error)) error {
	seen := map[string]bool{root.Key: true}
	for parent := root.Parent; parent != nil && *parent != "" && !seen[*parent]; {
		key := *parent
		seen[key] = true
		above, err := s.dispatch.GetIssue(ctx, key)
		if err != nil {
			return &readFailure{what: key, err: err}
		}
		if stop, err := visit(above); stop || err != nil {
			return err
		}
		parent = above.Parent
	}
	return nil
}

// stepDocumentOf is issue's document for step, as stepDocument binds one: its primary document for
// spec, its Verification document for criteria and its decisions document for decisions; "" for an
// issue that holds none.
func stepDocumentOf(step record.DesignStep, issue dispatch.Issue) string {
	switch step {
	case record.DesignStepCriteria:
		return issue.VerificationArtifactID
	case record.DesignStepDecisions:
		return issue.DecisionsArtifactID
	}
	return issue.PrimaryArtifactID
}

// reviewed says whether Dispatch shows a review of the document or an open request for a person's
// approval of it: every approval state but draft. The nearest ancestor so shown for a step is the
// one a root's step may rest on (R1), so an approval far up the chain never passes over a nearer
// design a person reviewed or is asked to review.
func reviewed(approval dispatch.Approval) bool {
	return approval.State != "draft"
}

// approvalShown is what Dispatch shows of a document's approval, as a refusal names it.
func approvalShown(approval dispatch.Approval) string {
	version := 0
	if approval.Version != nil {
		version = *approval.Version
	}
	by := actorNamed(approval.By)
	switch approval.State {
	case "approved":
		return fmt.Sprintf("an approval of its latest version %d by %s", version, by)
	case "stale":
		return fmt.Sprintf("an approval of version %d by %s, older than its latest version %d, and no open approval request", version, by, approval.LatestVersion)
	case "changes_requested":
		return fmt.Sprintf("a changes request on version %d by %s", version, by)
	case "awaiting":
		turn := "the person"
		if approval.WaitingOn != "human" {
			turn = "the " + cmp.Or(approval.WaitingOn, "agent")
		}
		return fmt.Sprintf("an open approval request (ask %s) at its latest version %d, waiting on %s", cmp.Or(approval.AskID, "with no id"), approval.LatestVersion, turn)
	case "draft":
		return fmt.Sprintf("no review and no open approval request at its latest version %d", approval.LatestVersion)
	}
	return fmt.Sprintf("the approval state %q at its latest version %d", approval.State, approval.LatestVersion)
}

// registrationBasis checks the basis a register_gate call names, after the call's architect, root
// and step are checked and the root's issue and own step document are read (root, own), and
// answers the basis the registration records. It answers no basis, and true, when a person's
// approval of the root's own document at the version the call names opens the step as today: that
// registration records none (R1). Each refusal writes its own code, records nothing, and names what
// Dispatch shows.
//
// The reads come in a fixed order: the root's own document's reviews; the basis document; the
// root's child_of chain up to the basis's issue, then the step's document of each ancestor below
// it, nearest first; the ancestor's record; for a pending basis the basis document's reviews; the
// ancestor's primary document at the version its requirements are read at; and the root's own
// documents, which viewOf reads.
func (s *server) registrationBasis(w http.ResponseWriter, r *http.Request, grant credential.Grant, req GateRegisterRequest, step record.DesignStep,
	root dispatch.Issue, document string, own dispatch.Approval) (*record.GateBasis, bool) {
	ctx := r.Context()
	refuse := func(code, format string, args ...any) (*record.GateBasis, bool) {
		writeFailure(w, http.StatusConflict, code, fmt.Sprintf(format, args...))
		return nil, false
	}
	failed := func(err error) (*record.GateBasis, bool) {
		refuseFailedRead(w, err)
		return nil, false
	}

	// The root's own document: its latest version, a person's approval of it, and its newest review.
	if req.Version < own.LatestVersion {
		return refuse("ROOT_VERSION_NOT_LATEST", "Dispatch shows %s's %s (%s) at version %d, and the call names version %d; a registration on a basis names the latest version of the issue's own document",
			root.Key, step.Document(), document, own.LatestVersion, req.Version)
	}
	if own.ApprovedLatest() != 0 && own.By.Person() {
		return nil, true
	}
	reviews, err := s.dispatch.Reviews(ctx, document)
	if err != nil {
		return failed(&readFailure{what: "the reviews of " + document, err: err})
	}
	if len(reviews) > 0 && reviews[0].State == "changes_requested" {
		return refuse("ROOT_CHANGES_REQUESTED", "the newest review of %s's %s (%s) is a changes request on version %d by %s, whatever its approval shows now; a root follows that request, so request a person's approval of the issue's own document rather than registering on a basis",
			root.Key, step.Document(), document, reviews[0].Version, actorNamed(reviews[0].Actor))
	}

	// The basis document, and where it stands on the root's chain.
	basisID := strings.ToLower(req.BasisArtifactID)
	shown, err := s.dispatch.Approval(ctx, basisID)
	switch {
	case dispatch.Missing(err):
		return refuse("BASIS_NOT_ON_CHAIN", "Dispatch has no document %s, so it belongs to no issue on %s's child_of chain", basisID, root.Key)
	case err != nil:
		return failed(&readFailure{what: basisID, err: err})
	}
	var chain []dispatch.Issue
	var ancestor *dispatch.Issue
	if shown.IssueKey != "" && shown.IssueKey != root.Key {
		if err := s.gateAncestors(ctx, root, func(above dispatch.Issue) (bool, error) {
			if above.Key == shown.IssueKey {
				ancestor = &above
				return true, nil
			}
			chain = append(chain, above)
			return false, nil
		}); err != nil {
			return failed(err)
		}
	}
	switch {
	case shown.IssueKey == "":
		return refuse("BASIS_NOT_ON_CHAIN", "Dispatch shows %s on no issue, as a project's document; a basis is the same step's document of an issue above %s on its child_of chain",
			basisID, root.Key)
	case shown.IssueKey == root.Key:
		return refuse("BASIS_NOT_ON_CHAIN", "Dispatch shows %s on %s itself; a basis is the same step's document of an issue above %s on its child_of chain",
			basisID, root.Key, root.Key)
	case ancestor == nil:
		return refuse("BASIS_NOT_ON_CHAIN", "Dispatch shows %s on %s, which is not on %s's child_of chain (%s); a basis is the same step's document of an issue above the root",
			basisID, shown.IssueKey, root.Key, chainNames(root, chain))
	}
	if !stepDocument(step, shown) {
		return refuse("GATE_STEP_DOCUMENT_MISMATCH", "%s is not the %s step's document of %s; that step records the issue's %s, and its basis is the ancestor's %s",
			basisID, step, ancestor.Key, step.Document(), step.Document())
	}
	for _, nearer := range chain {
		id := stepDocumentOf(step, nearer)
		if id == "" {
			continue
		}
		approval, err := s.dispatch.Approval(ctx, id)
		if err != nil {
			return failed(&readFailure{what: id, err: err})
		}
		if reviewed(approval) {
			return refuse("BASIS_NOT_NEAREST", "%s stands between %s and %s on the child_of chain, and Dispatch shows on its %s (%s) %s; the basis is the nearest ancestor whose %s has a review or an open approval request, so register on %s's document or request a person's approval of the issue's own",
				nearer.Key, root.Key, ancestor.Key, step.Document(), id, approvalShown(approval), step.Document(), nearer.Key)
		}
	}

	// The ancestor: in Legion and not accepted (R1's first condition).
	if !dispatch.CarriesLegionLabel(ancestor.Labels) {
		return refuse("BASIS_NOT_IN_LEGION", "%s carries no %q label in Dispatch (its labels: %s); a basis is an issue Legion works on",
			ancestor.Key, dispatch.LegionLabel, labelNames(ancestor.Labels))
	}
	if ancestor.Status == "retro" || ancestor.Status == "done" {
		return refuse("BASIS_ACCEPTED", "Dispatch shows %s %s, so it has been accepted; a basis is an ancestor not yet accepted, so request a person's approval of the issue's own document",
			ancestor.Key, ancestor.Status)
	}
	recorded, err := s.recordedIssue(ctx, ancestor.Key)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the record of "+ancestor.Key)
		return nil, false
	}
	if recorded != nil && record.Accepted(*recorded) {
		return refuse("BASIS_ACCEPTED", "the daemon records %s in %s, so it has accepted %s's reviewed head; a basis is an ancestor not yet accepted, so request a person's approval of the issue's own document",
			ancestor.Key, record.StandsAt(*recorded), ancestor.Key)
	}

	// The ancestor's approval: a person's of its latest version, or its open request of that version
	// waiting on the person (R1's second and third conditions, and the pending basis).
	session := ""
	if machine, holds := s.claimHolds(grant); holds {
		session = machine.Claim().Session
	}
	basis := &record.GateBasis{Issue: ancestor.Key, ArtifactID: basisID, Version: req.BasisVersion, Reason: req.Reason, Session: session}
	switch {
	case shown.ApprovedLatest() != 0:
		if req.BasisVersion != shown.LatestVersion {
			return refuse("BASIS_VERSION_MISMATCH", "Dispatch shows %s's %s (%s) approved at its latest version %d, and the call names version %d; name version %d",
				ancestor.Key, step.Document(), basisID, shown.LatestVersion, req.BasisVersion, shown.LatestVersion)
		}
		if !shown.By.Person() {
			return refuse("BASIS_NOT_BY_PERSON", "the approval of %s's %s (%s) at version %d was given by %s, not by a person; a basis is a person's approval, so request a person's approval of the issue's own document",
				ancestor.Key, step.Document(), basisID, shown.LatestVersion, actorNamed(shown.By))
		}
		basis.Approval, basis.Approver = record.EventMark{Seq: shown.Seq, At: shown.At}, shown.By.Name()
	case shown.State == "awaiting":
		if req.BasisVersion != shown.LatestVersion {
			return refuse("BASIS_VERSION_MISMATCH", "Dispatch shows %s's %s (%s) at its latest version %d with an open approval request, and the call names version %d; a pending basis names version %d",
				ancestor.Key, step.Document(), basisID, shown.LatestVersion, req.BasisVersion, shown.LatestVersion)
		}
		if shown.AskID == "" || shown.WaitingOn != "human" {
			return refuse("BASIS_REQUEST_NOT_OPEN", "Dispatch shows %s's %s (%s) with %s; a pending basis waits on a request that waits on the person",
				ancestor.Key, step.Document(), basisID, approvalShown(shown))
		}
		basisReviews, err := s.dispatch.Reviews(ctx, basisID)
		if err != nil {
			return failed(&readFailure{what: "the reviews of " + basisID, err: err})
		}
		for _, review := range basisReviews {
			if review.Version == shown.LatestVersion {
				return refuse("BASIS_REQUEST_NOT_OPEN", "version %d of %s's %s (%s) already holds a review (%s by %s), so the request open on it no longer waits for the person's answer to that version",
					shown.LatestVersion, ancestor.Key, step.Document(), basisID, review.State, actorNamed(review.Actor))
			}
		}
		basis.Pending = &record.PendingBasis{Ask: shown.AskID}
	default:
		return refuse("BASIS_NOT_APPROVED", "Dispatch shows on %s's %s (%s) %s; a basis is a person's approval of its latest version, or an open approval request of that version waiting on the person",
			ancestor.Key, step.Document(), basisID, approvalShown(shown))
	}

	// The ancestor's requirement blocks: at the basis version for the spec step, whose basis is the
	// ancestor's primary document, and at that document's latest version for the other steps.
	primary, version := basisID, req.BasisVersion
	if step != record.DesignStepSpec {
		primary = ancestor.PrimaryArtifactID
		if primary == "" {
			return refuse("BASIS_NO_REQUIREMENTS", "Dispatch shows no primary document of %s, so it marks no requirement block and no root is a view on it; request a person's approval of the issue's own %s",
				ancestor.Key, step.Document())
		}
		latest, err := s.dispatch.Approval(ctx, primary)
		if err != nil {
			return failed(&readFailure{what: primary, err: err})
		}
		version = latest.LatestVersion
	}
	requirements, err := s.requirementsOf(ctx, primary, version)
	if err != nil {
		return failed(err)
	}
	if len(requirements) == 0 {
		return refuse("BASIS_NO_REQUIREMENTS", "%s's primary document (%s) at version %d marks no requirement block, so no root is a view on it; request a person's approval of the issue's own %s",
			ancestor.Key, primary, version, step.Document())
	}

	// The root is a view on those requirements (R1's fourth condition).
	serves, refusal, err := s.viewOf(ctx, root.Key, viewBasis{issue: ancestor.Key, version: version, requirements: requirements})
	if err != nil {
		return failed(err)
	}
	if refusal != nil {
		return refuse(refusal.code, "%s", refusal.message)
	}
	basis.Serves = serves
	return basis, true
}

// viewFact is the ancestor the root's documents read as a view on, the view fact a registration of
// the spec step records with no basis (R4): the nearest ancestor on the root's child_of chain whose
// primary document has a review or an open approval request, where that document marks
// requirement blocks at its latest version and viewOf reads the root as a view on them; "" for none.
// A failed read refuses the registration with DISPATCH_FAILED and records nothing, so no
// registration records a view fact it could not read.
func (s *server) viewFact(w http.ResponseWriter, r *http.Request, root dispatch.Issue) (string, bool) {
	ctx := r.Context()
	var nearest dispatch.Issue
	var shown dispatch.Approval
	if err := s.gateAncestors(ctx, root, func(above dispatch.Issue) (bool, error) {
		if above.PrimaryArtifactID == "" {
			return false, nil
		}
		approval, err := s.dispatch.Approval(ctx, above.PrimaryArtifactID)
		if err != nil {
			return false, &readFailure{what: above.PrimaryArtifactID, err: err}
		}
		if !reviewed(approval) {
			return false, nil
		}
		nearest, shown = above, approval
		return true, nil
	}); err != nil {
		refuseFailedRead(w, err)
		return "", false
	}
	if nearest.Key == "" {
		return "", true
	}
	requirements, err := s.requirementsOf(ctx, nearest.PrimaryArtifactID, shown.LatestVersion)
	if err != nil {
		refuseFailedRead(w, err)
		return "", false
	}
	if len(requirements) == 0 {
		return "", true
	}
	_, refusal, err := s.viewOf(ctx, root.Key, viewBasis{issue: nearest.Key, version: shown.LatestVersion, requirements: requirements})
	if err != nil {
		refuseFailedRead(w, err)
		return "", false
	}
	if refusal != nil {
		return "", true
	}
	return nearest.Key, true
}

// requirementsOf is the ids of the requirement blocks one version of a document marks, in document
// order. A block whose markdown names no id is no block a reference can name, so it is left out.
func (s *server) requirementsOf(ctx context.Context, document string, version int) ([]string, error) {
	markdown, err := s.dispatch.DocumentVersion(ctx, document, version)
	if err != nil {
		return nil, &readFailure{what: fmt.Sprintf("version %d of %s", version, document), err: err}
	}
	var requirements []string
	for _, block := range dispatchblocks.Read(markdown) {
		if block.Type == requirementBlock && block.ID != "" {
			requirements = append(requirements, block.ID)
		}
	}
	return requirements, nil
}

// viewBasis is the ancestor's primary document a view is read against: its issue, the version read
// and the ids of the requirement blocks that version marks.
type viewBasis struct {
	issue        string
	version      int
	requirements []string
}

// viewRefusal is why a root's documents do not read as a view on an ancestor (R1's fourth
// condition): the code a registration on a basis is refused with, and a message naming what
// Dispatch shows.
type viewRefusal struct{ code, message string }

// viewDocument is one document of the root as viewOf reads it: what it is on the issue and the
// typed blocks of its latest version.
type viewDocument struct {
	artifact dispatch.Artifact
	blocks   []dispatchblocks.Block
}

// viewOf says whether the root's documents read as a view on basis, the ancestor's primary document
// at a version, and answers the references to the ancestor's requirements the root's checks serve,
// each once, pinned to that version (`dispatch://<KEY>/spec#b-<id>@v<N>`). It is the one judge of a
// view: the basis check and the view fact both ask it (R1's fourth condition, R4).
//
// It reads every document of the root Dispatch lists, each at its latest version, and skips a file
// or an image, whose version is its bytes. The root is a view when its primary, Verification and
// decisions documents hold no requirement block; its Verification document stands at a version
// and holds at least one check; no other document holds a check, since a result is recorded only
// against a check of the Verification document while coverage counts a check in any; and every
// check serves at least one block, each a requirement block of basis, compared by issue, document
// and decoded block id with the reference's own version pin left aside. A refusal answers the
// first rule broken, in that order; a failed read answers its error.
func (s *server) viewOf(ctx context.Context, root string, basis viewBasis) ([]string, *viewRefusal, error) {
	artifacts, err := s.dispatch.Artifacts(ctx, root)
	if err != nil {
		return nil, nil, &readFailure{what: "the documents of " + root, err: err}
	}
	var documents []viewDocument
	for _, artifact := range artifacts {
		if !artifact.Document() || artifact.LatestVersion < 1 {
			continue
		}
		markdown, err := s.dispatch.DocumentVersion(ctx, artifact.ID, artifact.LatestVersion)
		if err != nil {
			return nil, nil, &readFailure{what: fmt.Sprintf("version %d of %s", artifact.LatestVersion, artifact.ID), err: err}
		}
		documents = append(documents, viewDocument{artifact: artifact, blocks: dispatchblocks.Read(markdown)})
	}
	refuse := func(code, format string, args ...any) ([]string, *viewRefusal, error) {
		return nil, &viewRefusal{code: code, message: fmt.Sprintf(format, args...)}, nil
	}

	verification := -1
	for index, document := range documents {
		part := documentPart(document.artifact)
		if part == dispatch.ArtifactRoleVerification && verification < 0 {
			verification = index
		}
		if part == "" {
			continue
		}
		for _, block := range document.blocks {
			if block.Type == requirementBlock {
				return refuse("VIEW_HOLDS_REQUIREMENT", "%s holds requirement block %s; a view on %s holds no requirement of its own, so serve %s's requirements from the Verification document's checks and send a requirement %s does not state to the architect that owns %s",
					documentNamed(root, document.artifact), blockNamed(block), basis.issue, basis.issue, basis.issue, basis.issue)
			}
		}
	}
	if verification < 0 {
		return refuse("VIEW_NO_VERIFICATION", "Dispatch lists no Verification document of %s at a version; a view's checks, each serving a requirement block of %s's primary document, stand in its Verification document",
			root, basis.issue)
	}
	var checks []dispatchblocks.Block
	for _, block := range documents[verification].blocks {
		if block.Type == checkBlock {
			checks = append(checks, block)
		}
	}
	if len(checks) == 0 {
		return refuse("VIEW_NO_CHECK", "%s holds no check block; a view holds at least one check, each serving a requirement block of %s's primary document",
			documentNamed(root, documents[verification].artifact), basis.issue)
	}
	for index, document := range documents {
		if index == verification {
			continue
		}
		for _, block := range document.blocks {
			if block.Type == checkBlock {
				return refuse("VIEW_CHECK_OUTSIDE_VERIFICATION", "%s holds check block %s; a result is recorded only against a check of the Verification document, so a view keeps every check there",
					documentNamed(root, document.artifact), blockNamed(block))
			}
		}
	}
	var serves []string
	for _, check := range checks {
		served := 0
		for _, ref := range check.Refs {
			if ref.Attribute != servesAttribute {
				continue
			}
			served++
			if ref.Issue != basis.issue || !ref.Primary || !slices.Contains(basis.requirements, ref.Block) {
				return refuse("VIEW_SERVES_OUTSIDE_BASIS", "check %s of %s serves %s, which is no requirement block of %s's primary document at version %d (it marks %s); each block a view's check serves is one of those",
					blockNamed(check), documentNamed(root, documents[verification].artifact), ref.Text, basis.issue, basis.version, strings.Join(basis.requirements, ", "))
			}
			if pinned := pinnedTo(ref, basis.version); !slices.Contains(serves, pinned) {
				serves = append(serves, pinned)
			}
		}
		if served == 0 {
			return refuse("VIEW_CHECK_SERVES_NOTHING", "check %s of %s serves no block; each check of a view serves a requirement block of %s's primary document at version %d",
				blockNamed(check), documentNamed(root, documents[verification].artifact), basis.issue, basis.version)
		}
	}
	return serves, nil, nil
}

// documentPart is the part a document of the root plays that viewOf reads apart: its primary
// document ("primary"), its Verification document or its decisions document (the roles Dispatch
// names them by, the decisions document also by its slug, as stepDocument reads it), and "" for any
// other document.
func documentPart(artifact dispatch.Artifact) string {
	switch {
	case artifact.Primary:
		return "primary"
	case artifact.Role == dispatch.ArtifactRoleVerification:
		return dispatch.ArtifactRoleVerification
	case artifact.Role == dispatch.ArtifactRoleDecisions || artifact.Slug == dispatch.DecisionsSlug:
		return dispatch.ArtifactRoleDecisions
	}
	return ""
}

// documentNamed is one document of issue as a refusal names it: what it is, its slug and the
// version read.
func documentNamed(issue string, artifact dispatch.Artifact) string {
	part := "document"
	switch documentPart(artifact) {
	case "primary":
		part = "primary document"
	case dispatch.ArtifactRoleVerification:
		part = "Verification document"
	case dispatch.ArtifactRoleDecisions:
		part = "decisions document"
	}
	return fmt.Sprintf("%s's %s %s at version %d", issue, part, artifact.Slug, artifact.LatestVersion)
}

// blockNamed is a typed block as a refusal names it: its id, or what it is when its markdown names
// none.
func blockNamed(block dispatchblocks.Block) string {
	if block.ID == "" {
		return "(a " + block.Type + " block with no id)"
	}
	return block.ID
}

// pinnedTo is ref, a reference to a requirement block of the ancestor's primary document, pinned to
// version: the reference as written, in Dispatch's canonical form, with its own pin replaced.
func pinnedTo(ref dispatchblocks.Ref, version int) string {
	text := ref.Text
	if ref.Pin > 0 {
		text = strings.TrimSuffix(text, "@v"+strconv.Itoa(ref.Pin))
	}
	return text + "@v" + strconv.Itoa(version)
}

// chainNames is root's child_of chain as a refusal names it, from root upward.
func chainNames(root dispatch.Issue, chain []dispatch.Issue) string {
	names := []string{root.Key}
	for _, above := range chain {
		names = append(names, above.Key)
	}
	if len(names) == 1 {
		return root.Key + " has no parent"
	}
	return strings.Join(names, " under ")
}

// labelNames is labels as a refusal names them.
func labelNames(labels []string) string {
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ", ")
}

// actorNamed is actor as a refusal names it, `<kind>:<id>`, or what Dispatch's silence means.
func actorNamed(actor dispatch.Actor) string {
	if name := actor.Name(); name != "" {
		return name
	}
	return "nobody Dispatch names"
}
