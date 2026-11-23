package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// registeredRule is the rule a registration of step writes its gate under (R24): the rule the
// row of that step already holds in this generation, whatever document it names, since the first
// registration of a generation fixes it and a re-admission's clear of the generation is what lets
// the next one fix it again; and for the first registration, the rule of a change request where
// the registration read the issue's change request and Dispatch answered that the issue's
// repository is a spec source, and the rule of a person's approval otherwise — every other step, a
// Dispatch older than the route, and a repository that is no spec source.
func registeredRule(gates map[record.DesignStep]record.DesignGate, step record.DesignStep, read *intake.ChangeRequestRead) record.GateRule {
	if held, found := gates[step]; found {
		return held.Rule
	}
	if step == record.DesignStepSpec && read != nil && read.SpecSource {
		return record.GateRuleChangeRequest
	}
	return record.GateRuleApproval
}

// readChanges is gate as read, a read of its change request at the gate's current version
// (LatestVersion), leaves it, under the rule of a change request (R23, R25); gate as it is under
// any other rule, for a nil read and for a read of another version. The read holds the version of
// the issue's Verification document it took its checks from, the second document of the request,
// so a read older than the Verification version the gate holds changes nothing. Otherwise the gate
// takes the read's Verification version, and:
//
//   - whether a check of The Spec that the Verification document changes and no person approved
//     holds the gate (ChecksUnapproved), as the read says it, where the read is of a Verification
//     version newer than the gate's or the version is new to the gate (fresh: the version a
//     registration, a reset or a version event moves it to); a read of the same Verification
//     version and spec version only takes the hold away, when it finds the change approved: such a
//     read may have been made before the person's approval the gate already applied, and the hold
//     comes back only with a newer Verification version;
//   - its exemption: a read that does not exempt the version takes back an exemption of it, and a
//     read that exempts it opens it with no person where opens says the fact may (a registration, a
//     version event of the gate's own document, a reset). An approval and a version of another
//     document never open a gate: a person's changes request takes an exemption back, and a read
//     that changes nothing must not undo it. The exemption is never an approval in a person's name:
//     ApprovedVersion is left as it stands.
func readChanges(gate record.DesignGate, read *intake.ChangeRequestRead, fresh, opens bool) record.DesignGate {
	if gate.Rule != record.GateRuleChangeRequest || read == nil || read.Version != gate.LatestVersion || read.VerificationVersion < gate.VerificationVersion {
		return gate
	}
	switch {
	case fresh || read.VerificationVersion > gate.VerificationVersion:
		gate.ChecksUnapproved = read.ChecksUnapproved
	case !read.ChecksUnapproved:
		gate.ChecksUnapproved = false
	}
	gate.VerificationVersion = read.VerificationVersion
	switch exempt := gate.LatestVersion; {
	case read.ExemptsVersion(exempt) && opens:
		gate.ExemptVersion = &exempt
	case !read.ExemptsVersion(exempt) && gate.ExemptVersion != nil && *gate.ExemptVersion == exempt:
		gate.ExemptVersion = nil
	}
	return gate
}

// openedNotice is the design-approved notice of gate's opening at version, the one notice every
// opening the architect is told of sends: a person's approval names that person (reviewer), and
// an opening by exemption alone (openedByExemption) names nobody and gives the reason no person
// approved it, since version proposes nothing (R23). ref is the step's document (stepRef).
func openedNotice(gate record.DesignGate, version int, reviewer, ref string) record.Notice {
	if openedByExemption(gate) {
		return record.Notice{Kind: "design-approved", Version: version, Ref: ref, Reason: exemptionReason(version)}
	}
	return record.Notice{Kind: "design-approved", Version: version, Actor: reviewer, Ref: ref}
}

// openedByExemption is whether gate stands open with no person: open by exemption at its latest
// version (record.GateExempt), and approved by no person at that version. An approval at the
// latest version is a person's opening, whatever the exemption beside it.
func openedByExemption(gate record.DesignGate) bool {
	return record.GateExempt(gate) && !record.GateApproved(gate)
}

// exemptionReason is the reason a design-approved notice of an opening by exemption gives.
func exemptionReason(version int) string {
	return fmt.Sprintf("no proposal: version %d of the change request proposes no statement, component or interface and changes no approved check, so no person approves it", version)
}

// exemptSpecGate is whether the spec gate of tree, its root's row of the spec step, stands open by
// exemption (record.GateExempt): the gate whose tree lands nothing until the ending condition of
// its change request holds at the head that would land (D10).
func (e *Engine) exemptSpecGate(ctx context.Context, tx pgx.Tx, tree string) (bool, error) {
	gates, err := e.store.GatesOf(ctx, tx, tree)
	if err != nil {
		return false, err
	}
	return record.SpecGateExempt(gates), nil
}

// queueEndingReads queues the read of the ending condition for every member of tree in merging
// whose pull request the record holds a head of, when tree's spec gate stands open by exemption
// (record.SpecEndingRead): a registration, a version, a reset or an approval of the root's spec
// document, or a list of outcomes recorded on an issue of the tree, can turn the condition either
// way for each of them. A tree whose gate is open some other way, or closed, queues nothing: the
// gate itself holds or releases its merger then.
func (e *Engine) queueEndingReads(ctx context.Context, tx pgx.Tx, tree string, cause record.SpecEndingCause) error {
	exempt, err := e.exemptSpecGate(ctx, tx, tree)
	if err != nil || !exempt {
		return err
	}
	members, err := e.store.TreeIssues(ctx, tx, tree)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Phase != phase.Merging {
			continue
		}
		pr, err := e.store.PullRequest(ctx, tx, member.Key)
		if err != nil {
			return err
		}
		if pr == nil || pr.HeadSHA == "" {
			continue
		}
		if err := e.enqueue(ctx, tx, member.Key, record.SpecEndingRead{Head: pr.HeadSHA, Cause: cause}); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the ending condition of a change request is read again for a merging member", "issue", member.Key,
			"tree", tree, "head", pr.HeadSHA, "cause", cause)
	}
	return nil
}

// endingCause is the cause a fact that leaves the root's spec gate open by exemption queues its
// reads under (queueEndingReads): SpecEndingCauseExempted where the gate did not stand open by
// exemption before the fact (wasExempt), since a merger that runs then was started without the
// ending step and is told it by that read, and cause, the fact's own, otherwise.
func endingCause(wasExempt bool, cause record.SpecEndingCause) record.SpecEndingCause {
	if wasExempt {
		return cause
	}
	return record.SpecEndingCauseExempted
}

// headMovedInMerging queues the read of the ending condition at issue's new head, for an issue in
// merging whose tree's spec gate stands open by exemption: the reading the daemon holds is of the
// head before, and a head with no final list of its own holds the merge (D10).
func (e *Engine) headMovedInMerging(ctx context.Context, tx pgx.Tx, issue record.Issue, head string) error {
	if issue.Phase != phase.Merging || head == "" {
		return nil
	}
	exempt, err := e.exemptSpecGate(ctx, tx, issue.Tree)
	if err != nil || !exempt {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merging member's head moved; the ending condition of its tree's change request is read again", "issue", issue.Key,
		"tree", issue.Tree, "head", head)
	return e.enqueue(ctx, tx, issue.Key, record.SpecEndingRead{Head: head, Cause: record.SpecEndingCauseHead})
}

// verificationRead applies the change request read that an event of another document of an issue
// carries — a version or an approval of its Verification document, the request's second document,
// from which it reads its checks (R25) — to the issue's spec gate under the rule of a change
// request, on a step the deployment gates (readChanges). The daemon's prepare stage reads it at the
// gate's current version for any document of the issue the gate does not name, since an event
// names no document's role. A read that moves nothing writes nothing.
//
// Such a read never opens the gate with no person: it can take an exemption back, when it finds a
// check of The Spec changed, and it can hold a gate a person's approval opened while a changed
// check no person approved stands, or take that hold away, when the approved spec holds the change
// as written. The write goes through putGateTold, which tells the tree when it moves the standing
// gate, naming the Verification document for a closing the read made (gateMoveOfChecks). A gate
// that stays open by exemption reads the ending condition again for every member in merging, as a
// version of the spec does (D10).
func (e *Engine) verificationRead(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) error {
	if fact.ChangeRequest == nil {
		return nil
	}
	gates, err := e.store.GatesOf(ctx, tx, fact.Key)
	if err != nil {
		return err
	}
	gate := record.SpecGate(gates)
	if gate == nil || gate.ArtifactID == fact.ArtifactID || gate.Rule != record.GateRuleChangeRequest || !e.gatesStep(gate.Step) {
		return nil
	}
	updated := readChanges(*gate, fact.ChangeRequest, false, false)
	if updated.VerificationVersion == gate.VerificationVersion && updated.ChecksUnapproved == gate.ChecksUnapproved &&
		record.GateExempt(updated) == record.GateExempt(*gate) {
		return nil
	}
	e.logOnCommit(ctx, "workflow: a change request read at another document's event moved the spec gate", "issue", fact.Key,
		"artifact", fact.ArtifactID, "event", fact.Kind, "verification_version", updated.VerificationVersion, "checks_unapproved", updated.ChecksUnapproved,
		"exempt", record.GateExempt(updated), "open", record.GateOpen(updated))
	if err := e.putGateTold(ctx, tx, updated, updated.LatestVersion); err != nil {
		return err
	}
	if err := record.PublishChangedGates(ctx, e.store, tx, fact.Key, gates, e.now()); err != nil {
		return err
	}
	if record.GateExempt(updated) && updated.VerificationVersion > gate.VerificationVersion {
		return e.queueEndingReads(ctx, tx, fact.Key, record.SpecEndingCauseVersion)
	}
	return nil
}

// specOutcomesRecorded applies Dispatch's spec.outcomes_recorded on an issue: a list of outcomes
// is part of what the ending condition of its tree's change request judges, so every member of the
// tree in merging is read again (queueEndingReads). An issue the record does not hold belongs to no
// tree the daemon runs, and queues nothing.
func (e *Engine) specOutcomesRecorded(ctx context.Context, tx pgx.Tx, fact intake.SpecOutcomesRecorded) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a list of outcomes was recorded on an issue of a tree", "issue", issue.Key, "tree", issue.Tree,
		"record", fact.Record, "list", fact.List, "head", fact.Head)
	return intake.Result{}, e.queueEndingReads(ctx, tx, issue.Tree, record.SpecEndingCauseOutcomes)
}

// specEndingRead applies one read of the ending condition of a merging member's change request
// at its pull request's head (D10): it records the reading (record.SpecEnding), tells the
// member's running merger when the reading turns, and tells the tree root's architect each reason
// it has not been told at that head.
//
// The read is held to the record as it stands now, since a read is made outside the fact's lock:
// a member that left merging, a pull request whose head moved past the read, or a tree whose spec
// gate stands open by exemption no more, takes nothing from it. The head that moved has a read of
// its own queued (headMovedInMerging), and a gate that closed holds the merger itself. A read
// queued because the gate turned open by exemption (record.SpecEndingCauseExempted) is the one
// that tells a merger already running the ending step, and the head's own read may find the
// reading unchanged and tell it nothing, so such a read whose head moved is queued again at the
// pull request's current head, under the same cause.
//
// The merger is told how the condition stands for that head — the closed-gate message of a
// reading that waits, the opened-gate message of one that is ready, each ending with the ending
// step — when the reading turns (it waits after one that was ready, or is ready after one that
// waited), when it is the first reading the record holds of the issue, and when it was queued
// because the gate turned open by exemption (record.SpecEndingCauseExempted). The last two reach a
// merger that may run with no word of the step: one started before the gate stood open by
// exemption, whose task names no step, whatever reading an earlier time the gate stood exempt
// left. A reading the same as the one before tells it nothing, and so does the start fence's own
// reading (intake.SpecEndingRead.Start): that merger is not running yet, and the task its start
// gives names the step (the outbox's startTask). The merger is told nothing either while another
// step the deployment gates is closed (standingGateOpen): that step's closed gate holds the merge,
// and an opened message of the ending would say the merge is released. The reading is recorded
// all the same, and the opening of that step gives the merger the ending step (tellGateMoved). The
// message goes to the merger's own role topic (record.RoleMessage), which a merger that holds no
// role is not told: its start is held by the same reading (the outbox's merger fence), and a
// released merger's woken deliveries wait for it (deliverHeld).
//
// Each reason is told once per head, in a spec-ending-waits notice on the tree's root, so it reaches
// the root's architect, the owner of the change request; the record keeps the codes told at the
// head it read, and a new head tells each reason again.
func (e *Engine) specEndingRead(ctx context.Context, tx pgx.Tx, fact intake.SpecEndingRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	gates, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	exempt := record.SpecGateExempt(gates)
	var stale string
	headMoved := false
	switch {
	case issue.Phase != phase.Merging:
		stale = fmt.Sprintf("the issue stands in %s, not merging", issue.Phase)
	case pr == nil || pr.HeadSHA != fact.Head:
		stale, headMoved = "the pull request's head moved past the read", true
	case !exempt:
		stale = "the tree's spec gate stands open by exemption no more"
	}
	if stale != "" {
		e.logOnCommit(ctx, "workflow: a read of the ending condition takes nothing: "+stale, "issue", issue.Key, "head", fact.Head, "state", fact.State)
		if headMoved && exempt && fact.Cause == record.SpecEndingCauseExempted && pr != nil && pr.HeadSHA != "" {
			e.logOnCommit(ctx, "workflow: a read of the turn to exemption whose head moved is queued again at the current head", "issue", issue.Key,
				"tree", issue.Tree, "head", pr.HeadSHA, "read_head", fact.Head)
			return intake.Result{}, e.enqueue(ctx, tx, issue.Key, record.SpecEndingRead{Head: pr.HeadSHA, Cause: record.SpecEndingCauseExempted})
		}
		return intake.Result{}, nil
	}
	before, err := e.store.SpecEnding(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	var told []string
	if before != nil && before.Head == fact.Head {
		told = slices.Clone(before.Told)
	}
	waits := fact.State != record.SpecEndingReady
	for _, reason := range fact.Reasons {
		if slices.Contains(told, reason.Code) {
			continue
		}
		told = append(told, reason.Code)
		if err := e.notice(ctx, tx, issue.Tree, record.Notice{Kind: record.SpecEndingWaitsNotice, Role: claim.RoleArchitect,
			Reason: reason.Code, Summary: endingWaitsSummary(issue.Tree, issue.Key, fact.Head, reason.Code, reason.Detail)}); err != nil {
			return intake.Result{}, err
		}
	}
	reading := record.SpecEnding{Issue: issue.Key, Head: fact.Head, State: record.SpecEndingReady, Told: told, ReadAt: e.now()}
	if waits {
		reading.State = record.SpecEndingWaiting
	}
	if err := e.store.PutSpecEnding(ctx, tx, reading); err != nil {
		return intake.Result{}, err
	}
	wasWaiting := before != nil && before.Waits()
	stands := e.standingGateOpen(gates)
	tell := !fact.Start && stands && (before == nil || waits != wasWaiting || fact.Cause == record.SpecEndingCauseExempted)
	e.logOnCommit(ctx, "workflow: the ending condition of a change request was read", "issue", issue.Key, "tree", issue.Tree,
		"head", fact.Head, "state", reading.State, "was_waiting", wasWaiting, "cause", fact.Cause, "start", fact.Start, "told", told,
		"gates_stand_open", stands, "tells_merger", tell)
	if !tell {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.tellMergerEnding(ctx, tx, *issue, fact, waits)
}

// tellMergerEnding tells issue's merger, on its own role topic, that the design gate of its tree
// closed or opened for the head fact read: the closed-gate message of a reading that waits, which
// names each reason, and the opened-gate message of one that is ready. Both end with the step the
// merger takes before each merge and each enqueue (mergerEndingStep). A merger whose token cannot
// be built is logged and not told, as tellGateMoved logs one.
func (e *Engine) tellMergerEnding(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.SpecEndingRead, closed bool) error {
	told, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleMerger, row: issue.Key}},
		e.logUnnamable(ctx, "workflow: the ending condition turned and the merger could not be named, so it is not told", "tree", issue.Tree, "head", fact.Head))
	if err != nil {
		return err
	}
	var body string
	if closed {
		var reasons []string
		for _, reason := range fact.Reasons {
			reasons = append(reasons, reason.Code+": "+reason.Detail)
		}
		body = fmt.Sprintf("The design gate of %s closed: the ending condition of its change request does not hold for head %s (%s). "+
			"Merge nothing. Enqueue nothing. Take a queued pull request out of the merge queue. %s",
			issue.Tree, fact.Head, strings.Join(reasons, "; "), mergerEndingStep(issue.Tree))
	} else {
		body = fmt.Sprintf("The design gate of %s opened: the ending condition of its change request holds for head %s. %s",
			issue.Tree, fact.Head, mergerEndingStep(issue.Tree))
	}
	for _, to := range told {
		if err := e.enqueue(ctx, tx, to.issue, record.RoleMessage{Role: string(to.token), Body: body}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the merger is told the ending condition turned", "issue", issue.Key, "tree", issue.Tree, "head", fact.Head,
		"closed", closed, "told", len(told))
	return nil
}

// mergerEndingStep is the step a merger of a tree whose spec gate opened with no person takes before
// each merge and each enqueue, as its start task and every message of the ending condition say it.
func mergerEndingStep(tree string) string {
	return fmt.Sprintf("Before each merge and each enqueue, run `dispatch spec-ending --issue %s --head <head>`, "+
		"and merge or enqueue only when its first line reads `ready`.", tree)
}

// MergerExemptionParagraph is the paragraph a merger's start task ends with when its tree's spec
// gate stands open by exemption as the start runs (D10; the outbox's startTask adds it): the gate
// opened with no person, so the merger reads the ending condition itself before each merge and
// each enqueue.
func MergerExemptionParagraph(tree string) string {
	return fmt.Sprintf("The design gate of %s opened with no person's approval: its change request proposes nothing. %s", tree, mergerEndingStep(tree))
}

// endingWaitsSummary is what a spec-ending-waits notice says: the member whose merge waits, the
// head, the reason's code and Dispatch's detail of it, and what the owner of the change request
// does about it.
func endingWaitsSummary(tree, member, head, code, detail string) string {
	return fmt.Sprintf("The change request of %s may not land at head %s of %s's pull request. Reason %s: %s %s",
		tree, head, member, code, strings.TrimSpace(detail), endingReasonStep(tree, head, code))
}

// endingReasonStep is what the owner does about one reason the ending condition does not hold: the
// owner writes or corrects the conclusion block, has the reviewer record its final list at the
// head, or opens an approval request of the whole spec (R26), with its Verification document for a
// changed check (R25). A code this daemon does not know sends the owner to Dispatch's own answer.
func endingReasonStep(tree, head, code string) string {
	switch code {
	case "no_conclusion", "incomplete":
		return fmt.Sprintf("Write or correct the conclusion block in the spec of %s.", tree)
	case "no_final_list_for_head", "final_list_of_another_head":
		return fmt.Sprintf("Have the reviewer record its final list at head %s.", head)
	case "uncovered_without_approval", "proposes_without_approval":
		return fmt.Sprintf("Open an approval request of the whole spec of %s.", tree)
	case "changes_checks_without_approval":
		return fmt.Sprintf("Open an approval request of the whole spec of %s, with its Verification document.", tree)
	}
	return fmt.Sprintf("Run `dispatch spec-ending --issue %s --head %s` and act on its reasons.", tree, head)
}

// specRequestStale tells the owner of a change request that Dispatch refused a person's approval
// of it as stale (D8): one notice of kind spec-request-stale, on the tree root of the issue the
// request is on, so it reaches the root's architect, who writes the request again against the
// approved spec and opens a new approval request. The fact is applied once per event, so one event
// is one notice however often it is delivered. Where the daemon holds no record of the issue, so no
// tree and no owner, the controller is told in the notice's place (record.ControllerNotice), and
// names an owner; a notice whose architect will not hold its role again goes to the controller from
// the outbox (the daemon's staleRequestToController).
func (e *Engine) specRequestStale(ctx context.Context, tx pgx.Tx, fact intake.SpecRequestStale) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil {
		return intake.Result{}, err
	}
	notice := record.Notice{Kind: record.SpecRequestStaleNotice, Role: claim.RoleArchitect, Reason: fact.Reason, Summary: staleRequestSummary(fact)}
	if issue == nil {
		e.logOnCommit(ctx, "workflow: a stale change request of an issue the daemon does not record goes to the controller", "issue", fact.Key)
		return intake.Result{}, e.enqueue(ctx, tx, fact.Key, StaleRequestForController(fact.Key, notice))
	}
	e.logOnCommit(ctx, "workflow: the owner of a stale change request is told", "issue", fact.Key, "tree", issue.Tree, "nodes", len(fact.Statements))
	return intake.Result{}, e.notice(ctx, tx, issue.Tree, notice)
}

// staleRequestSummary is what a spec-request-stale notice says: the issue, each stale node with the
// version the request read and the approved spec's current one, Dispatch's reason, and the step.
func staleRequestSummary(fact intake.SpecRequestStale) string {
	nodes := make([]string, 0, len(fact.Statements))
	for _, node := range fact.Statements {
		switch {
		case node.Removed:
			nodes = append(nodes, fmt.Sprintf("%s (base %d, removed from the approved spec)", node.ID, node.Base))
		default:
			nodes = append(nodes, fmt.Sprintf("%s (base %d, current %d)", node.ID, node.Base, node.Current))
		}
	}
	reason := strings.TrimSpace(fact.Reason)
	if reason != "" && !strings.HasSuffix(reason, ".") {
		reason += "."
	}
	return fmt.Sprintf("Dispatch refused the approval of the change request of %s: the approved spec changed %s after the request was written. %s %s",
		fact.Key, strings.Join(nodes, ", "), reason, staleRequestOwnerStep)
}

// staleRequestOwnerStep is the step a spec-request-stale notice gives its owner, the last
// sentences of its Summary.
const staleRequestOwnerStep = "Write the request again against the approved spec. Then open a new approval request."

// StaleRequestForController is the controller's copy of a spec-request-stale notice of issue, for
// a request nobody of its tree can be told of (D8): the issue is unrecorded, or its architect will
// not hold its role again. The copy says what happened as the owner's does, and gives the
// controller's own step in place of the owner's: name an owner, who writes the request again and
// opens a new approval request. Notices carry their action in their own text (the child-not-
// recorded and session-store-fault notices do), so the controller needs no other source for it.
// The copy carries no re-send count of the row it copies.
func StaleRequestForController(issue string, notice record.Notice) record.ControllerNotice {
	notice.Resends, notice.ResendOf = 0, 0
	notice.Summary = strings.TrimSuffix(notice.Summary, staleRequestOwnerStep) + fmt.Sprintf(
		"Name an owner for %s. The owner writes the request again against the approved spec and opens a new approval request.", issue)
	return record.ControllerNotice(notice)
}
