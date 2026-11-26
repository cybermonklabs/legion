package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ReconcileAwaiting moves each admitted root of project that now waits only on a person into
// awaiting_person, or, when a person already acted after its wait would begin, gives its
// architect the resume task where it stands. It also answers each root waiting on a person whose
// design gate is open (answerOpenGates). It reads only the record, so the same record gives the
// same decision, and admission calls it on every pass (Admission.promoteHolds): a guard that
// clears on any path — a member leaving, a claim released, the label put back, the architect's
// start taken — is seen at the next fact. issues is the caller's snapshot of project's records
// (admission's LiveIssues, which names the project), and held names each root the caller still
// holds for the Dispatch consumer to catch up (nil holds none): a root whose listing is ahead of
// the record decides nothing until the consumer has reached it, so a question already answered on
// Dispatch does not start a wait here first. It returns the roots that entered awaiting_person,
// which keep the slots they hold, and whether it wrote any issue of the snapshot, which the caller
// then reads again.
//
// It reads the slot rows once for the pass, because a root waits on a person only while the
// daemon has started it, which is what its slot row says (record.CanWaitOnPerson,
// decideAwait's rule 0); the wait itself keeps that row (record.KeepsSlot). For an admitted
// root the work it hands over is the architect writing the spec, before
// the design gate first opens. That is the spec's own condition, and the boot's catch-up reads
// the roots this pass can move (the daemon's catchesUp) by that same predicate.
func (e *Engine) ReconcileAwaiting(ctx context.Context, tx pgx.Tx, project string, issues []record.Issue, held func(string) bool) ([]string, bool, error) {
	changed, err := e.answerOpenGates(ctx, tx, issues, held)
	if err != nil {
		return nil, false, err
	}
	var roots []record.Issue
	for _, issue := range issues {
		if held != nil && held(issue.Key) {
			continue
		}
		if claim.IsTreeRoot(issue.Key, issue.Tree) &&
			issue.Phase == phase.Admitted && issue.DeclaredSeq != 0 {
			roots = append(roots, issue)
		}
	}
	if len(roots) == 0 {
		return nil, changed, nil
	}
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, false, err
	}
	// A root whose key yields no claim token has no architect claim to read (record.RoleRun's
	// zero value), so it owes its start and keeps its slot.
	keys := make([]string, len(roots))
	tokens := make([]claim.Token, len(roots))
	var runKeys []record.RoleRunKey
	for i, root := range roots {
		keys[i] = root.Key
		architect, err := claim.NewToken(token, root.Key, claim.RoleArchitect)
		if err != nil {
			continue
		}
		tokens[i] = architect
		runKeys = append(runKeys, record.RoleRunKey{Token: architect, Issue: root.Key, Role: claim.RoleArchitect, Generation: root.Generation})
	}
	asks, err := e.store.RootAsks(ctx, tx, keys)
	if err != nil {
		return nil, false, err
	}
	runs, err := e.store.RoleRuns(ctx, tx, runKeys)
	if err != nil {
		return nil, false, err
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	var entered []string
	for i, root := range roots {
		gates, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return nil, false, err
		}
		unresumable, err := Unresumable(ctx, e.store, tx, root)
		if err != nil {
			return nil, false, err
		}
		_, holds := slotted[root.Key]
		view := awaitView{Root: root, Slotted: holds, Unresumable: unresumable, Run: runs[tokens[i]], Asks: asks[root.Key],
			Steps: e.cfg.Steps, Gates: gates}
		for _, member := range issues {
			if member.Tree == root.Key && member.Key != root.Key {
				view.Members = append(view.Members, member)
			}
		}
		decision := decideAwait(view)
		switch decision.Act {
		case actEnter:
			if err := e.enterAwaiting(ctx, tx, root, decision.Waiting, decision.Why); err != nil {
				return nil, false, err
			}
			entered = append(entered, root.Key)
			changed = true
		case actTellInPlace:
			if err := e.tellInPlace(ctx, tx, root, decision); err != nil {
				return nil, false, err
			}
			changed = true
		case actTakeIn:
			if err := e.store.ClearDeclared(ctx, tx, root.Key); err != nil {
				return nil, false, err
			}
			e.logOnCommit(ctx, "workflow: what a root handed back is no longer with the person, so it starts no wait", "issue", root.Key,
				"declared_seq", root.DeclaredSeq)
			changed = true
		default:
			store.OnCommit(ctx, func() {
				e.log.Debug("workflow: a root that handed work to a person starts no wait on that person", "issue", root.Key, "why", decision.Why)
			})
		}
	}
	return entered, changed, nil
}

// answerOpenGates answers each unanswered root of issues waiting on a person whose design gate
// is open, whatever opened it and whenever: a person's approval Dispatch recorded before the wait
// began but applied after it, a registration's read of an approval, or a registration of a step
// the deployment does not gate. The gate it reads is the one the wait's own document names, since
// that is the step the person was asked about; a wait on a question names no document, and every
// required step being open answers it, as the one gate's opening did. The gate's opening starts
// no phase of the tree while the root waits, since the line offers a slot to nothing of a
// waiting root's tree, and the person has nothing left to decide that holds the tree, so the
// root resumes on the next pass, keeping the slot it holds, told how its spec stands, and its
// tree advances then. A wait
// on a gate under a deployment that gates no step (the operator emptied the list while the root
// waited on it) whose gate is not open is decided in the same pass, since no approval request
// counts then (gateRequestArtifact): the wait moves to a question of the tree still with the
// person, and is otherwise answered, so the architect resumes and registers its spec, which
// opens the gate. It reports whether it answered or moved any root's wait.
func (e *Engine) answerOpenGates(ctx context.Context, tx pgx.Tx, issues []record.Issue, held func(string) bool) (bool, error) {
	answered := false
	for _, root := range issues {
		if (held != nil && held(root.Key)) || !claim.IsTreeRoot(root.Key, root.Tree) ||
			!root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
			continue
		}
		gates, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return answered, err
		}
		if waitGateOpen(e.cfg.Steps, gates, root.Awaiting.Artifact) {
			version, reviewed := root.Awaiting.GateVersion, 0
			var step record.DesignStep
			gate := gateOfDocument(gates, root.Awaiting.Artifact)
			if gate != nil {
				version, step = gate.LatestVersion, gate.Step
			} else {
				// A wait on a question names no document: the step its resume speaks of says
				// whether an ancestor's approval opened it (approvalDetail).
				gate = gateOf(gates, stepInQuestion(e.cfg.Steps, gates))
			}
			// A step an ancestor's approval opened records no version reviewed: no person approved
			// the root's version, and the resume asks no person's word of it (answeredDecision).
			if gate == nil || !gate.OnBasis() {
				reviewed = version
			}
			e.logOnCommit(ctx, "workflow: the design gate of a root waiting on a person is open, which answers its wait", "issue", root.Key,
				"version", version, "ask", root.Awaiting.Ask)
			if err := e.answerWait(ctx, tx, root, waitAnswer{by: record.AnsweredByApproval, detail: approvalDetail(version, gate),
				document: record.AnsweredDocument{Artifact: root.Awaiting.Artifact, Step: step, Version: reviewed}}); err != nil {
				return answered, err
			}
			answered = true
			continue
		}
		if root.Awaiting.GateVersion != 0 && !e.armed() {
			if err := e.leaveGateOff(ctx, tx, root, gates); err != nil {
				return answered, err
			}
			answered = true
		}
	}
	return answered, nil
}

// leaveGateOff decides a wait on root's design gate, which is not open, once the deployment gates
// no step: the wait moves to the oldest question of the tree still with the person, keeping where
// it began, and is otherwise answered (AnsweredByApproval, at the version it waited on, its
// detail marked gateOffAnswer), so the architect resumes told that the gate opens, with no
// approval, when it registers its spec, or, should the gate be on again by then, that no person
// approved it (resumeTask).
func (e *Engine) leaveGateOff(ctx context.Context, tx pgx.Tx, root record.Issue, gates map[record.DesignStep]record.DesignGate) error {
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	// The empty list is passed as the configured one, so no approval request is still with the
	// person and the wait can only move to a question (gateRequestArtifact).
	if next, ok := waitSubject(asks[root.Key], nil, gates, root.Awaiting.Since); ok {
		e.logOnCommit(ctx, "workflow: the deployment gates no step, so a root's wait on its gate moves to what is still with the person",
			"issue", root.Key, "version", root.Awaiting.GateVersion, "next_ask", next.Ask)
		root.Awaiting.Ask, root.Awaiting.GateVersion, root.Awaiting.Artifact = next.Ask, next.GateVersion, next.Artifact
		return e.WriteIssue(ctx, tx, root)
	}
	e.logOnCommit(ctx, "workflow: the deployment gates no step, which answers a root's wait on its gate", "issue", root.Key,
		"version", root.Awaiting.GateVersion)
	var step record.DesignStep
	if gate := gateOfDocument(gates, root.Awaiting.Artifact); gate != nil {
		step = gate.Step
	}
	return e.answerWait(ctx, tx, root, waitAnswer{by: record.AnsweredByApproval,
		detail:   fmt.Sprintf("version %d", root.Awaiting.GateVersion) + gateOffAnswer,
		document: record.AnsweredDocument{Artifact: root.Awaiting.Artifact, Step: step}})
}

// gateOffAnswer ends the detail of a gate wait the empty-list rule answered (leaveGateOff).
// That answer is recorded, and shown on the API, as AnsweredByApproval: the contract's answeredBy
// enum, which refuses a value it does not list, has no value of its own for it. But no person
// approved anything, so the detail tells the resume (resumeTask) never to say so, should the gate
// be on again by then. This mark, basisOpenedAnswer, basisEndedAfter and basisEndedAnswer are
// stored in the wait's answered detail (record.Awaiting.AnsweredDetail) and parsed back from it
// at the resume (resumeTask), which can come after a deploy, so a release that changes the words
// of any of them must go on reading the words a release before it stored as well.
const gateOffAnswer = ", answered with the design gate off"

// basisOpenedAnswer marks the detail of a wait answered as an approval where an ancestor's
// approval opened the step (dispatch://INFRA-3320, R1 and R2), as gateOffAnswer marks the gate-off
// answer. That answer is recorded, and shown on the API, as AnsweredByApproval: the contract's
// answeredBy enum has no value of its own for it. No person approved the root's version, so the
// detail names the ancestor's approval after this mark (approvalDetail), and the resume
// (resumeTask) names that approval and never a person's approval of the root's document, also
// where the step has closed again since.
const basisOpenedAnswer = ", opened on an ancestor's approval: "

// approvalDetail is the detail of a wait that an open step answered as an approval at version:
// the version and, where gate stands open on an ancestor's approval, the root's version registered
// and that approval (basisOpenedAnswer, basisApproval). Every answer of a wait by an open step
// writes it (answerOpenGates, answerRegisteredGate).
func approvalDetail(version int, gate *record.DesignGate) string {
	if gate == nil || !gate.OnBasis() {
		return fmt.Sprintf("version %d", version)
	}
	return fmt.Sprintf("version %d", gate.LatestVersion) + basisOpenedAnswer + basisApproval(*gate)
}

// basisEndedAfter joins, to the detail of a wait already answered, what then ended its step's
// wait on an ancestor's request (tellPendingEnded): an act on the root's own document can answer
// the wait and end that step's wait at once, as a person's approval of the root's own document
// does, and the resume names both (dispatch://INFRA-3320, R1). A newline joins them, since the
// detail of no act holds one: a review's reason is quoted (changesDetail).
const basisEndedAfter = "\n" + basisEndedAnswer

// answeredAct is the detail of the act that answered a wait, with what ended a step's wait on an
// ancestor's request after it (basisEndedAfter) left out. Every reader that compares or prints the
// act's detail reads this.
func answeredAct(detail string) string {
	act, _, _ := strings.Cut(detail, basisEndedAfter)
	return act
}

// basisEndsAfter is what ended a step's wait on an ancestor's request after the act that answered
// the wait, one entry for each end, oldest first; none for a detail that holds none
// (basisEndedAfter).
func basisEndsAfter(detail string) []string {
	return strings.Split(detail, basisEndedAfter)[1:]
}

// act is what ReconcileAwaiting does with one root.
type act int

const (
	// actStay leaves the root as it is.
	actStay act = iota
	// actEnter moves the root into awaiting_person.
	actEnter
	// actTellInPlace resumes the root's architect where it stands, its slot kept.
	actTellInPlace
	// actTakeIn clears what the root handed back (Store.ClearDeclared): none of it is still with
	// the person, and the running architect has the act that took it away, so no later change of
	// the record may start a wait from that hand-back.
	actTakeIn
)

// awaitView is what decideAwait reads of one root: its record, whether it holds an admission slot
// (Slotted, read from the slot rows by ReconcileAwaiting), the other members of its tree, why it
// could not resume (Unresumable, "" when it could), its architect's run at the root's
// generation, its asks' mirror rows, the design-gate steps the deployment gates (Steps) and the
// gates the root holds by step (Gates). A gate's document names its approval requests — every
// approval row of that document — and their answer is a review rather than a person's act on an
// ask (RootAsk.PersonActSeq).
type awaitView struct {
	Root        record.Issue
	Slotted     bool
	Members     []record.Issue
	Unresumable string
	Run         record.RoleRun
	Asks        []record.RootAsk
	Steps       []record.DesignStep
	Gates       map[record.DesignStep]record.DesignGate
}

// awaitDecision is decideAwait's answer: the act, why, the wait the root enters or is told of
// (Waiting), and for actTellInPlace the act that already answered it (Answer, Detail).
type awaitDecision struct {
	Act     act
	Why     string
	Waiting record.Awaiting
	Answer  record.Answer
	Detail  string
}

// decideAwait is the rule for whether a root waits on a person, checked cheapest first:
//
//  0. a root not at admitted, lingering, out of the workflow, or one that holds no admission
//     slot — one the daemon has not started, or one that gave its slot back to wait on another
//     issue (record.CanWaitOnPerson) — stays;
//  1. one whose architect handed nothing back since it was last given work (DeclaredSeq) stays;
//  2. one with nothing still with the person (withHuman) takes in what it handed back
//     (actTakeIn): that was answered or closed first, and the running architect has that act;
//  3. one whose tree has a member in a phase stays, since its tree is at work and the root does
//     not wait only on a person;
//  4. one that could not resume (Unresumable) stays, since released, nothing could resume it;
//  5. one whose architect still owes the work it was given — a start queued, a claim still
//     starting, a task not yet taken (owed) — stays: the wait begins only after that task is taken;
//  6. otherwise the wait begins at the newest hand-back (DeclaredSeq), on the design gate when its
//     approval request is with the person, else on the oldest question that is. When a person's
//     act Dispatch recorded after that start is already in the record (answeredSince), the
//     architect is told of it where it stands (actTellInPlace); otherwise the root enters
//     awaiting_person (actEnter).
//
// Rule 0 is the spec's own condition (dispatch://INFRA-980, "Which issue needs a slot"): a root
// enters awaiting_person only while it holds a slot, which now says the daemon has started it
// and it stands in a phase that keeps a slot (record.KeepsSlot); a root at admitted whose gate has
// never opened keeps that slot through a wait on another issue (blockers.waitsOf). It is read as
// the root's own slot row
// (awaitView.Slotted). A root still waiting in line for
// its first slot holds none, so it starts no wait however its architect's hand-back stands —
// a re-admitted root the old architect declared on again before the linger's suspend ran is
// kept out by this rule, not by rule 1. The boot's catch-up of the roots this pass can move
// calls the same predicate (the daemon's catchesUp).
//
// The predicate reads the design gate as well, because a root whose gate has opened waits for
// its children by the children-first rule: the row says the daemon started the root and nothing
// about work it has now, so the row alone would let such a root wait on a person. Entering the
// wait there would stop nothing and free nothing; its own
// architect stays live to answer the person either way; and it would take every first-phase
// member of the tree out of the line (record.Waiting), stopping the children it waits for.
func decideAwait(v awaitView) awaitDecision {
	root := v.Root
	switch {
	// Rule 0 is record.CanWaitOnPerson alone: it tests the phase, the linger and the workflow,
	// and through AtWork that the issue is a tree root whose design gate has never opened, so
	// every standing a second case here could reject it rejects already. Only what to say of a
	// rejection is decided here, and whether the daemon started the root is what divides the
	// two: a root it has never started, and one it started whose own standing no longer lets it
	// wait. Two facts say it started the root: the slot row, and the mark of a row a pass took
	// back for a wait on another issue (record.Issue.ReleasedForWaitAt, blockers.waitsOf), since
	// such a root holds no row and was started all the same. blockers.startedIssues counts a
	// third, an owed start (record.LineFacts.Owed), and rule 0 does not read it: this rule
	// divides by what the record holds of the root's present standing, its row or the mark of the
	// row it gave back, and an owed start is a start its worker still waits to be given, not a
	// standing. A root at admitted that owes one and holds neither reads the never-started reason;
	// the decision is actStay either way, so only the reason could differ.
	case !record.CanWaitOnPerson(root, record.TreeGate{Steps: v.Steps, Rows: v.Gates}, v.Slotted):
		if !v.Slotted && root.ReleasedForWaitAt == nil {
			return awaitDecision{Why: "the daemon has not started it, so it holds no admission slot"}
		}
		return awaitDecision{Why: "it is not an admitted root of a tree in the workflow whose spec its architect is still writing"}
	case root.DeclaredSeq == 0:
		return awaitDecision{Why: "its architect has handed nothing to a person since it was last given work"}
	}
	since := record.EventMark{Seq: root.DeclaredSeq, At: root.DeclaredAt}
	waiting, ok := waitSubject(v.Asks, v.Steps, v.Gates, since)
	if !ok {
		return awaitDecision{Act: actTakeIn, Why: "nothing it handed back is still with the person"}
	}
	for _, member := range v.Members {
		if record.InWorkflow(member) && member.Phase != phase.Admitted {
			return awaitDecision{Why: fmt.Sprintf("%s of its tree is in %s", member.Key, member.Phase)}
		}
	}
	if v.Unresumable != "" {
		return awaitDecision{Why: "it could not resume: " + v.Unresumable}
	}
	if why, ok := owed(v.Run); ok {
		return awaitDecision{Why: "its architect " + why}
	}
	if answer, detail, ok := answeredSince(waiting, v.Asks, gateDocuments(v.Gates)); ok {
		return awaitDecision{Act: actTellInPlace, Why: "a person already acted after its hand-back", Waiting: waiting, Answer: answer, Detail: detail}
	}
	why := fmt.Sprintf("question %s is awaiting an answer", waiting.Ask)
	if waiting.GateVersion != 0 {
		why = fmt.Sprintf("spec version %d is awaiting a person's review", waiting.GateVersion)
	}
	return awaitDecision{Act: actEnter, Why: why, Waiting: waiting}
}

// withHuman is what of a root is still with the person: its tree's open questions that wait on
// the person, the one on the person's turn longest first (the turn's own place, WaitingOnSeq; an
// open ask no event gave a turn is the person's since it opened, so it sorts first), and whether
// an approval request of gate's document, the one document a root can wait on now
// (gateRequestArtifact; nil for none), stands open on the person's turn. An approval request of
// any other document asks the person for nothing the root waits on, and one the gate's approval
// answered, whichever request that approval landed on, asks them for nothing either
// (requestStands).
func withHuman(asks []record.RootAsk, gate *record.DesignGate) (questions []record.RootAsk, request bool) {
	for _, ask := range asks {
		switch {
		case gate != nil && ask.GateRequestOf(gate.ArtifactID):
			request = request || ask.WithHuman() && requestStands(*gate, ask)
		case ask.ByTree && ask.Question() && ask.WithHuman():
			questions = append(questions, ask)
		}
	}
	slices.SortFunc(questions, func(a, b record.RootAsk) int {
		return cmp.Or(cmp.Compare(a.WaitingOnSeq, b.WaitingOnSeq), cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Ask, b.Ask))
	})
	return questions, request
}

// requestStands says whether ask, an approval request of gate's document, still asks the person
// anything: the mirror's row of it is asked whether the gate's approval followed it
// (classify.ReviewSuperseded), at the approved version and at the row's newest event of its own
// standing — its newest open or close (Seq) and its newest turn (WaitingOnSeq). An approval the
// person gave on a later request of the same version answers it, which is
// dispatch://INFRA-2964's measured case: two requests for one version, the approval on the second,
// and the first left open on the mirror. A hand-back or a version that moved the request after the
// approval is newer than it, so that request stands.
func requestStands(gate record.DesignGate, ask record.RootAsk) bool {
	if gate.ApprovedVersion == nil {
		return true
	}
	return !classify.ReviewSuperseded(gate, *gate.ApprovedVersion, record.EventMark{Seq: max(ask.Seq, ask.WaitingOnSeq)})
}

// gateRequestArtifact is the document whose approval requests a root can wait on: the document of
// the step a person is being asked about (classify.StandingGate). A deployment that gates no step
// waits on no approval, and so does a root no closed required step holds a row for, whose document
// the record does not know: both answer "" (RootAsk.GateRequestOf matches no row against ""). A
// root waits on one document at a time, and the step after it is asked for once that one is open.
func gateRequestArtifact(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) string {
	gate, standing := classify.StandingGate(steps, gates)
	if !standing {
		return ""
	}
	return gate.ArtifactID
}

// gateRequestOf is gateRequestArtifact for one gate the caller already holds: its document while
// the deployment gates its step (gated) and the gate is closed, "" otherwise.
func gateRequestOf(gate record.DesignGate, gated bool) string {
	if !gated || classify.DesignGateOpen(gate) {
		return ""
	}
	return gate.ArtifactID
}

// gateDocuments is the document of each gate the root holds, in no order: the approval requests
// of any of them are answered by a review of their own, not by a person's act on an ask
// (RootAsk.PersonActSeq).
func gateDocuments(gates map[record.DesignStep]record.DesignGate) []string {
	documents := make([]string, 0, len(gates))
	for _, gate := range gates {
		documents = append(documents, gate.ArtifactID)
	}
	return documents
}

// waitGateOpen says whether the gate a root's wait names is open: the row of document, the
// document the wait records, when it names one; otherwise, for a wait on a question, every
// required step being open (classify.DesignGatesOpen), which is what the one gate's opening
// answered before a deployment could name a list.
func waitGateOpen(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate, document string) bool {
	if document == "" {
		return classify.DesignGatesOpen(steps, gates)
	}
	gate := gateOfDocument(gates, document)
	return gate != nil && classify.DesignGateOpen(*gate)
}

// waitSubject is the wait a root starts from since on what is still with the person: the gate a
// person is being asked about (classify.StandingGate, the step gateRequestArtifact answers for) at
// its latest version, on its own document, when its request is; otherwise the oldest question of
// its tree; false when nothing is. It asks which step the person has, not which step holds the
// work: a step nobody has registered holds the work and raises no wait of its own, and it does not
// take the wait away from a registered step after it either.
func waitSubject(asks []record.RootAsk, steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate, since record.EventMark) (record.Awaiting, bool) {
	standsOn, standing := classify.StandingGate(steps, gates)
	var requestOf *record.DesignGate
	if standing {
		requestOf = &standsOn
	}
	questions, request := withHuman(asks, requestOf)
	switch {
	case request:
		return record.Awaiting{GateVersion: standsOn.LatestVersion, Artifact: standsOn.ArtifactID, Since: since}, true
	case len(questions) > 0:
		return record.Awaiting{Ask: questions[0].Ask, Since: since}, true
	}
	return record.Awaiting{}, false
}

// owed says whether the architect has not yet taken the work it was last given, and how: a start
// still queued for it, no claim yet, a claim still starting, or a task it has not confirmed.
//
// The daemon's own delivery of a relaunch's record is no work it was given (supervise.HoldsTask):
// it names no person's decision, only the sentence of what the relaunch ended, which the
// architect reads whether or not the root waits — entering the wait leaves the architect running.
// So it holds neither the wait nor the telling of a person's act, which would otherwise wait
// behind that sentence for as long as the agent's own turns kept it unread
// (dispatch://INFRA-3276 R3). Nor is news of the approved design alone, queued as a start
// (startsWork) or held as a task (supervise.HoldsWork): it hands the architect no work of the
// root's, and the start can wait on its backoff for as long as the architect's turn runs.
func owed(run record.RoleRun) (string, bool) {
	if queuedStart(run) {
		return "has a start still queued", true
	}
	held := run.Claim
	switch {
	case held == nil:
		return "has no claim yet", true
	case slices.Contains([]supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
		supervise.StateShimConnected, supervise.StateRegistered}, held.State):
		return fmt.Sprintf("is still starting (%s)", held.State), true
	case supervise.HoldsWork(held.Pending) && held.Pending.ConfirmedAt.IsZero():
		return "has not taken its task yet", true
	}
	return "", false
}

// answeredSince names a person's act the record already holds that Dispatch recorded after waiting
// began: their act on the subject question, or on any other ask of the root the mirror recorded one
// on (record.RootAsk.PersonActSeq against the documents the root's gates are registered on, read
// from the acts the ask handlers record under one rule, personAct). A review of one of those
// documents is not read here: the architect is already live, and the daemon's own
// design-changes-requested notice is the work that reaches it (a handout of its own).
//
// A decline is named only where the act that answered is the decline itself: a decline closes the
// ask, so the row's ClosedSeq is that close's own sequence (askChange.apply writes ClosedBy and
// ClosedSeq from it), while a later act of the person's — a reply in the thread of the ask they
// declined — raises PersonSeq above it and answers as the reply it is. The row is all this path
// has to read, because a decline reaches it when it falls between the architect's hand-back and
// the pass that would put the root in the wait, and there is no event in hand; the live path has
// the close itself and reads the decline from that (personAnswered), since the row's decline
// never comes off it.
func answeredSince(waiting record.Awaiting, asks []record.RootAsk, gateArtifacts []string) (record.Answer, string, bool) {
	since := waiting.Since.Seq
	for _, ask := range asks {
		if seq := ask.PersonActSeq(gateArtifacts...); ask.Ask == waiting.Ask && seq > since {
			return answerOfAct(ask, seq, record.AnsweredByAsk), ask.Ask, true
		}
	}
	for _, ask := range asks {
		if seq := ask.PersonActSeq(gateArtifacts...); seq > since {
			return answerOfAct(ask, seq, record.AnsweredByReply), ask.Ask, true
		}
	}
	return "", "", false
}

// answerOfAct is how the person's act at seq on ask answered the wait: their decline of it where
// that act is the ask's own close and the mirror records a decline, else otherwise, which is
// whether the ask is the one the wait names.
func answerOfAct(ask record.RootAsk, seq int64, otherwise record.Answer) record.Answer {
	if ask.Decline != nil && seq == ask.ClosedSeq {
		return record.AnsweredByDecline
	}
	return otherwise
}

// enterAwaiting moves root into awaiting_person on waiting, which keeps the slot the root holds:
// a root in awaiting_person keeps its row through that wait (record.KeepsSlot), even where it
// waits on another issue as well (blockers.waitsOf leaves it out). The architect stays live to
// read the person's answer, and the controller is told.
func (e *Engine) enterAwaiting(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting, reason string) error {
	// The root's task keeps its place (moveTask): the wait comes before its first step, so the task
	// has not started, and awaiting_person, like admitted, is a phase of the architect before it.
	root.Phase, root.Awaiting = phase.AwaitingPerson, &waiting
	if err := e.WriteIssue(ctx, tx, root); err != nil {
		return err
	}
	// The root moves from admitted into awaiting_person here, which is a phase change and so a
	// movement (record.Store.MarkIssueMoved, the rule in dispatchIssue). The wait is reached
	// from a root standing at admitted (ReconcileAwaiting's own read), so this write never
	// re-asserts the phase the record carries, and no fact of its own brings it: the pass
	// decides it from the record on any fact at all, so nothing else would mark it. Unmarked,
	// a root that waits on a person carries the stamp of whatever last moved it before the
	// wait, and the stall rule would read the wait itself as stillness of that age.
	if err := e.store.MarkIssueMoved(ctx, tx, root.Key, e.now()); err != nil {
		return err
	}
	waitReason := fmt.Sprintf("%s waits on a person (%s): it keeps its admission slot until they answer", root.Key, reason)
	if err := e.enqueue(ctx, tx, root.Key, record.ControllerNotice{Kind: record.AwaitingPersonNotice, Reason: waitReason}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: root waits on a person and keeps its admission slot", "issue", root.Key, "tree", root.Tree,
		"reason", reason, "since_seq", waiting.Since.Seq)
	return nil
}

// tellInPlace gives the architect of a root it would have released the resume task naming the act
// that already answered its hand-back. It moves nothing: the root stays in its phase, and its
// slot row is as admission left it.
func (e *Engine) tellInPlace(ctx context.Context, tx pgx.Tx, root record.Issue, decision awaitDecision) error {
	waiting := decision.Waiting
	at := e.now()
	waiting.AnsweredAt, waiting.AnsweredBy, waiting.AnsweredDetail = &at, decision.Answer, decision.Detail
	e.logOnCommit(ctx, "workflow: a person already acted on what the root handed back; its architect is told where it stands", "issue", root.Key,
		"answered_by", decision.Answer, "detail", decision.Detail, "since_seq", waiting.Since.Seq)
	return e.ResumeAwaitedRoot(ctx, tx, root, waiting)
}

// ResumeAwaitedRoot gives the architect's live claim a task telling it what answered its wait,
// how the document of the step in question stands now (stepInQuestion), and what of the root is
// still with the person or waits on it. It is a handout (Store.StartHandout): what the architect
// handed back before it is answered or listed here, and only a hand-back after it starts the
// next wait. The members an open gate releases are not started here: each is an issue the cap
// counts, so each takes its own place in the waiting line and starts at its own slot
// (record.Waiting's LineFirstPhase), in the same admission pass that resumed this root.
//
// The start carries the wait it answers (record.SuperviseRequest.Awaiting) and no text: the task
// is rendered where it is sent (ResumeAwaitedRootTask, through the daemon's rootTask), from the
// gate and the asks as the record holds them then, because a person can review the document again
// between this write and that send, and every send of a held task is that moment again.
//
// The resume hands the architect work it was not on, so it raises the architect's phase entry and
// writes the task as the work of that entry (EnterArchitect), rendered now by the same rule
// (ResumeAwaitedRootTask), and the start carries the number (record.SuperviseRequest.Entry) as a
// phase worker's start does. That is what makes a resume survivable: the entry and its work live
// on the record, so a start row that reaches no claim leaves the architect's claim below that
// entry, and the entry sweep sends it that entry's work. The raise keeps on the entry the wait it
// answers (record.PhaseRow.EntryAwaiting), which is what this start is built from, and the sweep's
// send builds the same start from it (SendArchitectStart), so that send renders the task at its
// send as this start's does. The text the entry holds is the record as it stands at this raise,
// and is never sent again by this release: a person who approves the reviewed version after this
// raise has answered the changes request it names. It is rendered and written all the same for its
// one reader, a rollback to the release before this change's entry, 20261125181104: the rollback
// block in docs/kubernetes.md (contract entry 20261125181104) marks each entry that holds a wait,
// and that release sends a marked entry's task as written (record.PhaseRow.EntryTask), so the
// render goes only with that block. The root's own record cannot stand in for the kept wait,
// since it holds the wait no longer once the root resumes (the promotion clears it, and
// tellInPlace never writes one). Both callers get it — the promotion of an answered root (admit's
// record.LineAnswered branch) and tellInPlace, which gives a live architect this task where it
// stands.
//
// The resume starts the root's architect, which is a movement of the issue
// (record.Store.MarkIssueMoved, the rule in dispatchIssue). It is marked here rather than at
// either caller, so that both of them carry it: the promotion of a root a person answered
// (admit's promoteHolds) and the root whose person had already acted, which keeps its phase and
// is told where it stands (tellInPlace). Nothing else writes the mark for either of them. The
// act that answered the wait was recorded on Dispatch and is read from the record rather than
// carried by a fact of this issue; the start below is queued through Engine.enqueue, which waits
// for no slot and so never reaches StartOrOwe, where a phase worker's start marks one; and the
// architect's claim stands idle through the resume, which the quiet measure counts as no work
// (record's restingStates). Unmarked, the root would keep the moved_at of its last real
// movement, which is where it stood when it handed the question to the person, while it stands
// at admitted holding a slot: the next pass would read that stamp, find no wait of the daemon's
// own and tell the controller that a tree the daemon has just resumed has stopped. The resume
// and that rule run in the one admission pass (admit's ReconcileAwaiting and tellStalledTrees),
// so the pass that resumes the root is itself a pass that could tell it stopped.
func (e *Engine) ResumeAwaitedRoot(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting) error {
	if err := e.store.StartHandout(ctx, tx, root.Key); err != nil {
		return err
	}
	// The text is for the release a rollback returns to, which sends it as written; this release
	// renders the entry's task again at each send (record.PhaseRow.EntryTask says why it stays).
	task, err := ResumeAwaitedRootTask(ctx, tx, e.store, root.Key, waiting, e.cfg.Steps)
	if err != nil {
		return err
	}
	entry, err := e.EnterArchitect(ctx, tx, root, task, &waiting)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, root.Key, record.SuperviseRequest{
		Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation, Awaiting: &waiting, Entry: entry,
	}); err != nil {
		return err
	}
	return e.store.MarkIssueMoved(ctx, tx, root.Key, e.now())
}

// ResumeAwaitedRootTask is the resume task of root key's architect for the wait waiting it
// answers, as tx reads the record now: the root's gates, from which the step in question is taken
// (stepInQuestion) under steps, the design-gate steps the deployment gates (none is the gate off),
// and the root's ask mirror rows. The daemon's rootTask renders it at each send of the task
// ResumeAwaitedRoot queued, and ResumeAwaitedRoot renders it once for the entry it raises; the
// reads are here so that every caller reads the rows the same way.
func ResumeAwaitedRootTask(ctx context.Context, tx pgx.Tx, store record.Store, key string, waiting record.Awaiting, steps []record.DesignStep) (string, error) {
	gates, err := store.GatesOf(ctx, tx, key)
	if err != nil {
		return "", err
	}
	asks, err := store.RootAsks(ctx, tx, []string{key})
	if err != nil {
		return "", err
	}
	return resumeTask(key, waiting, stepInQuestion(steps, gates), gates, asks[key], len(steps) > 0), nil
}

// stepInQuestion is the step a resume speaks of (resumeTask): the step the person was asked about
// (classify.StandingGate), which is the one the resume answers for; else the first required step
// that is not open, which nobody has registered and whose document the architect has still to
// register; and when every required step is open the first required step, whose version the
// resume says the work runs at. The required steps are the ones the tree's work waits on
// (record.TreeGate.Required), as in the two rules it reads. It need not be the step whose document
// the review that answered the wait was of, which resumeTask reads from the wait itself.
func stepInQuestion(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) record.DesignStep {
	if gate, standing := classify.StandingGate(steps, gates); standing {
		return gate.Step
	}
	if step, _, closed := classify.FirstClosedStep(steps, gates); closed {
		return step
	}
	return record.TreeGate{Steps: steps, Rows: gates}.Required()[0]
}

// DesignApprovedWake is the phrase a resume task carries when the root's design gate is open at
// the resume. The daemon sends no design-approved notice to a root waiting on a person
// (applyArtifact, gateRegistered), so this resume is that root's design-approved wake: the
// architect skill, its references and the root architect's prompt name this phrase, and a test
// holds them to it.
const DesignApprovedWake = "this resume is your design-approved wake"

// resumeTask is the task an answered root's architect resumes with: what answered its wait
// (Awaiting.AnsweredBy) and that act's own detail (Awaiting.AnsweredDetail, recorded when the wait
// was answered, so a later act of the same kind does not rename it); how the document of the step
// in question stands at the resume, read from that step's gate now, so a gate closed again since
// is said as it is, and an open gate is said as the design-approved wake (DesignApprovedWake) — a
// person's review that answered is asked whether it is still their word (SupersedingApproval), so
// an approval that a later approval replaced, that a later changes request cleared, or whose step
// holds another document now is said to stand no longer (writeApprovalReplaced), one that stands
// below a newer version is said to stand, a changes request a later approval superseded asks for
// no revision, and one of a document its step no longer holds is said to be of a document since
// replaced (documentReplaced); a changes request the gate records as told after the
// wait began that did not answer it, which no notice told (tellChangesOnce); what is still with the
// person (withHuman) and what waits on the architect (a tree question, or the closed gate's
// approval request, on the agent's turn). Handing back is advice only while the gate is not open,
// since an open gate moves the root to planning, where a hand-back starts no wait; the request's
// own thread is named only while the request is open. armed says whether the design gate is armed:
// with it off, no approval is asked for, register_gate is never the advice, and an open gate is no
// person's approval. A wait the gates.design: off rule answered (gateOffAnswer) is never said to
// be approved, nor asked about as a person's approval: under an armed gate it is said as answered
// while the gate was off.
//
// A step on an ancestor's approval (dispatch://INFRA-3320, R1 and R2) is never said to be approved
// by a person. A wait an ancestor's approval answered (basisOpenedAnswer) names that approval and
// says that no person approved the root's version, and a step that stands open on such an approval
// at the resume is said so, as the design-approved wake. A step that still waits on an ancestor's
// open approval request names that request, and the task asks for no approval request of the
// root's own. A wait that the end of such a wait answered (basisEndedAnswer) names what ended it:
// the act, the ancestor's document and its version, and not a person's changes request on the
// root's document. An end that came after the act that answered the wait (basisEndedAfter) is
// named after that act.
//
// Every sentence about the gate names step's own document (record.DesignStep.Document), which is
// the document gate holds and the one whose version it prints: under a deployment that gates the
// decisions or the criteria step first, the step in question is not the spec, and a task saying
// "your spec" would send the architect to the wrong document. The READY refusal names a step's
// document the same way (Engine.handoff). The sentence about the act that answered — a person's
// review, or a newer version — names the document that act was of, which need not be step's
// (answeredStep below). gates are the root's rows by step, and step the step in question
// (stepInQuestion).
func resumeTask(key string, waiting record.Awaiting, step record.DesignStep, gates map[record.DesignStep]record.DesignGate,
	asks []record.RootAsk, armed bool) string {
	gate := gateOf(gates, step)
	document := step.Document()
	gateOpen := gate != nil && classify.DesignGateOpen(*gate)
	gateClosed := gate != nil && !gateOpen
	onBasis := gateOpen && gate.OnBasis()
	pending := gate != nil && gate.PendingOnBasis()
	answer := answeredAct(waiting.AnsweredDetail)
	version, gateOff := strings.CutSuffix(answer, gateOffAnswer)
	gateOff = gateOff && waiting.AnsweredBy == record.AnsweredByApproval
	version, openedBy, openedOnBasis := strings.Cut(version, basisOpenedAnswer)
	openedOnBasis = openedOnBasis && waiting.AnsweredBy == record.AnsweredByApproval
	basisEnded, endedByBasis := strings.CutPrefix(answer, basisEndedAnswer)
	endedByBasis = endedByBasis && waiting.AnsweredBy == record.AnsweredByChanges
	// The act that answered the wait is named by the document it was of, and a person's review
	// among them is asked of the gate of that document's step (answeredStep).
	act, actGate := answeredStep(waiting, step, gates)
	actDocument := act.Document()
	// superseded is the predicate's answer for the person's review that answered the wait, an
	// approval or a changes request, nil while it stands and for every other answer
	// (SupersedingApproval, answeredDecision). It is asked once, of the gate of the step whose
	// document the review was of alone (answeredStep), so an approval of a document registered on
	// the step since answers nothing of a changes request; armed says a person approves that step
	// (record.TakesStep), since the review answered a wait on that step's document or on a question
	// of a tree gated on it. An opening on an ancestor's approval and the end of a step's wait on an
	// ancestor's request are no person's review of the root's document, so neither is asked as one.
	// approved says a person's approval answered the wait.
	superseded := SupersedingApproval(actGate, armed, answeredDecision(act, waiting, actGate, gateOff || openedOnBasis || endedByBasis))
	approved := waiting.AnsweredBy == record.AnsweredByApproval && armed && !gateOff && !openedOnBasis
	var task strings.Builder
	switch waiting.AnsweredBy {
	case record.AnsweredByApproval:
		switch {
		case armed && gateOff:
			fmt.Fprintf(&task, "The design gate on %s was off when your wait was answered, so no person approved your %s %s then; the design gate is on again.",
				key, actDocument, version)
		case armed && openedOnBasis:
			fmt.Fprintf(&task, "An ancestor's approval opened the %s step of %s at %s: %s. No person approved your %s at that version.",
				act, key, version, openedBy, actDocument)
		case armed:
			fmt.Fprintf(&task, "Your %s %s on %s was approved.", actDocument, version, key)
			writeApprovalReplaced(&task, superseded, waiting, actGate, actDocument)
		case gateOpen:
			fmt.Fprintf(&task, "Your %s %s on %s is registered, and the design gate is off, so it needs no approval.", document, version, key)
		default:
			fmt.Fprintf(&task, "The design gate on %s is off, so no approval of your %s %s is asked for.", key, document, version)
		}
	case record.AnsweredByChanges:
		if endedByBasis {
			fmt.Fprintf(&task, "The wait of your %s step on an ancestor's approval request ended: %s", step, basisEnded)
			break
		}
		// A review the gate's own approval followed is not named: the person's word is that
		// approval, and the sentences after this one say how the document stands. Only a person's
		// approval replaces a changes request, so the sentence names its version
		// (Supersession.By). An ancestor's approval that opened the step since asks for no
		// revision either, and is no person's word in the review's place.
		other, replaced := documentReplaced(waiting, actGate)
		switch {
		case superseded != nil:
			fmt.Fprintf(&task, "A person's review answered your wait on %s, and the %s was "+
				"approved at version %d after it, so the review asks for no revision now.",
				key, actDocument, superseded.By.Version)
		case armed && !replaced && actGate != nil && actGate.OnBasis():
			fmt.Fprintf(&task, "A person requested changes to your %s on %s at %s. An ancestor's approval opened the %s step after that review, "+
				"so it asks for no revision now; read it.", actDocument, key, answer, act)
		default:
			fmt.Fprintf(&task, "A person requested changes to your %s on %s at %s. Read the review, and revise the %s as they ask.",
				actDocument, key, answer, actDocument)
			if replaced {
				fmt.Fprintf(&task, " The review was of a document since replaced: another document, %s, is now registered as your %s.",
					other, actDocument)
			}
		}
	case record.AnsweredByVersion:
		fmt.Fprintf(&task, "A newer version of your %s on %s, %s, handed the approval request back to you. Read it and the request's thread.", actDocument, key, answer)
	case record.AnsweredByRetraction:
		fmt.Fprintf(&task, "Your ask %s on %s was retracted when its decision block left the spec, so no person will answer it. "+
			"Read the spec as it stands; if you still need the decision, ask again.", waiting.Ask, key)
	case record.AnsweredByClosed:
		fmt.Fprintf(&task, "Your ask %s on %s was closed by %s, so no person will answer it. Read its thread; if you still need the decision, ask again.",
			waiting.Ask, key, answer)
	case record.AnsweredByAsk:
		fmt.Fprintf(&task, "A person answered your ask %s on %s, or replied in its thread; read it.", waiting.Ask, key)
	case record.AnsweredByReply:
		fmt.Fprintf(&task, "A person answered or replied in the thread of ask %s on %s; read that thread.", answer, key)
	case record.AnsweredByTurn:
		fmt.Fprintf(&task, "A session's reply handed your question %s on %s to your turn, so no person is asked anything there; read its thread.",
			answer, key)
	case record.AnsweredByDecline:
		// The sentences and the note are Dispatch's record of the decline, printed as the mirror
		// row holds them (record.RootAsk.Decline): the daemon writes neither, so the architect
		// reads the person's words and not the daemon's paraphrase of them. The note is quoted,
		// as a review's reason is (changesDetail), so the architect reads where the person's own
		// words begin and end. A decline the person wrote no note with ends after the sentences.
		fmt.Fprintf(&task, "A person declined to answer your ask %s on %s.", answer, key)
		if decline := recordedDecline(asks, answer); decline != nil {
			if decline.Words != "" {
				fmt.Fprintf(&task, " %s", decline.Words)
			}
			if decline.Note != "" {
				fmt.Fprintf(&task, " Their note: %q", decline.Note)
			}
		}
	default:
		fmt.Fprintf(&task, "A person acted on %s; read the issue's asks and its spec.", key)
	}
	for _, ended := range basisEndsAfter(waiting.AnsweredDetail) {
		fmt.Fprintf(&task, " The wait of your %s step on an ancestor's approval request also ended: %s", step, ended)
	}
	switch {
	case gate == nil:
	case gateOpen && !armed:
		fmt.Fprintf(&task, " The design gate is open at version %d: %s, so act on it as on design-approved.",
			gate.LatestVersion, DesignApprovedWake)
	case onBasis && openedOnBasis && openedBy == basisApproval(*gate):
		fmt.Fprintf(&task, " The step stands open on that approval at version %d, and the design gate is open: %s, so act on it as on design-approved.",
			gate.LatestVersion, DesignApprovedWake)
	case onBasis:
		fmt.Fprintf(&task, " Your %s step stands open at version %d on an ancestor's approval: %s. No person approved that version of your %s. "+
			"The design gate is open: %s, so act on it as on design-approved.", step, gate.LatestVersion, basisApproval(*gate), document, DesignApprovedWake)
	case gateOpen:
		fmt.Fprintf(&task, " Your %s is approved at version %d and the design gate is open: %s, so act on it as on design-approved.",
			document, gate.LatestVersion, DesignApprovedWake)
	case !armed:
		fmt.Fprintf(&task, " The design gate is closed at version %d, as no registration of that version has opened it: when the %s is ready, "+
			"register it at its current version, which opens the gate with no approval.", gate.LatestVersion, document)
	case pending:
		fmt.Fprintf(&task, " Your %s step is closed at version %d. It waits on approval request %s of version %d of %s. "+
			"A person's approval of that version opens the step. Do not request approval of your own %s.",
			step, gate.LatestVersion, gate.Basis.Pending.Ask, gate.Basis.Version, documentOf(gate.Basis.Issue, step), document)
	case approved && superseded == nil && act == step && gate.ApprovedVersion != nil:
		// The approval that answered still stands, and the gate is closed: a newer version of the
		// document, which no person has approved, closed it. An approval of another step's
		// document says nothing of this gate.
		fmt.Fprintf(&task, " The approval of version %d stands, and the %s is now at version %d, which is not approved.",
			*gate.ApprovedVersion, document, gate.LatestVersion)
	case openedOnBasis:
		fmt.Fprintf(&task, " That opening no longer stands: the %s is at version %d, and the step is closed.", document, gate.LatestVersion)
	default:
		fmt.Fprintf(&task, " Your %s is not approved at version %d.", document, gate.LatestVersion)
	}
	if gate != nil && gate.ToldChangesVersion != nil && !(waiting.Since.Seq > 0 && gate.ToldChangesMark.Seq == 0) &&
		gate.ToldChangesMark.After(waiting.Since) && SupersedingApproval(gate, armed, Decision{Kind: record.DecisionDesignChangesRequested,
		Step: step, Version: *gate.ToldChangesVersion, Mark: gate.ToldChangesMark}) == nil {
		detail := changesDetail(*gate.ToldChangesVersion, gate.ToldChangesReason)
		if waiting.AnsweredBy != record.AnsweredByChanges || act != step || answer != detail {
			fmt.Fprintf(&task, " A person then requested changes to your %s at %s. Read the review, and revise the %s as they ask.", document, detail, document)
		}
	}
	var requestOf *record.DesignGate
	if gate != nil && gateRequestOf(*gate, armed) != "" {
		requestOf = gate
	}
	questions, request := withHuman(asks, requestOf)
	var still, yours []string
	if request {
		still = append(still, fmt.Sprintf("the approval request for %s version %d", document, gate.LatestVersion))
	}
	for _, ask := range questions {
		still = append(still, "ask "+ask.Ask)
	}
	requestOpen := false
	for _, ask := range asks {
		// A request the gate's approval answered, on whichever request it landed, asks nothing of
		// anyone (requestStands), so it is neither the request's open thread nor one waiting on you.
		isRequest := gate != nil && ask.GateRequestOf(gate.ArtifactID) && requestStands(*gate, ask)
		if isRequest && ask.Open {
			requestOpen = true
		}
		if ask.Open && ask.WaitingOn == "agent" && (isRequest && gateClosed || ask.ByTree && ask.Question()) {
			yours = append(yours, "ask "+ask.Ask)
		}
	}
	if len(yours) > 0 {
		fmt.Fprintf(&task, " Waiting on you: %s; read each thread and reply.", strings.Join(yours, ", "))
	}
	switch {
	case len(still) > 0 && !gateOpen:
		fmt.Fprintf(&task, " Still with the person: %s. Hand each one back once you have done what the answer asks — a reply in its thread that "+
			"leaves the turn with the person", strings.Join(still, ", "))
		if armed && gateClosed && !pending {
			task.WriteString(", or request approval again and register that version with register_gate")
		}
		task.WriteString(" — or close it if you no longer need it. Handing one back starts your next wait; then end your turn.")
	case len(still) > 0:
		fmt.Fprintf(&task, " Still with the person: %s. Continue.", strings.Join(still, ", "))
	case pending && armed:
		task.WriteString(" The daemon tells you when the step opens or its wait ends. Continue.")
	case gateClosed && armed && endedByBasis:
		fmt.Fprintf(&task, " Read that document before you register the %s step again. You can register the step on a later version of that document, "+
			"with register_gate's basis. Or you can request approval of your own %s and register that version with register_gate. "+
			"A request you hand to the person starts your next wait. Then end your turn.", step, document)
	case gateClosed && armed:
		fmt.Fprintf(&task, " When the %s is ready, hand the request back to the person: request approval again and register that version with register_gate", document)
		if requestOpen {
			task.WriteString(", or answer in the request's thread")
		}
		task.WriteString(". Handing it back starts your next wait; then end your turn.")
	default:
		task.WriteString(" Continue.")
	}
	return task.String()
}

// answeredDecision is the act that answered waiting as the resume asks SupersedingApproval about
// it, once for every answer: its kind is the person's review the answer names (AnsweredBy), an
// approval or a changes request, and none for every other answer and for an answer that is no
// person's review of the root's document (noReview): an approval the gates.design: off rule gave
// (gateOffAnswer), an opening on an ancestor's approval (basisOpenedAnswer) and the end of a step's
// wait on an ancestor's request (basisEndedAnswer), so the predicate answers nil for each. The
// review is of step's document, the document it was of (answeredDocument; "" for
// an answer an earlier release wrote of a wait on a question, which is asked of the document the
// gate holds), so, for a changes request, an approval of a document registered on the step since
// answers nothing of it, as every other sender asks, and an approval of a document the step no
// longer holds no longer stands; and at the version the answer recorded beside its detail
// (record.AnsweredDocument.Version; 0 for an answer a daemon of an earlier release wrote, which is
// then asked at the version the gate holds approved, as a sender that knows no version asks).
//
// An approval is asked at no mark: a person who approves the same version again gives the same
// word again. A changes request is asked at a mark of its own. The wait records no mark of the
// review itself, and the gate's told record of it goes once an approval follows, so it is asked at
// the wait's start: the review answered the wait, so Dispatch recorded it after that start, and an
// approval of its version the gate still holds after that start came after the review, since the
// review applied to the gate clears the approval it held then (classify.ApplyDesignGateEvent). A
// wait whose start has no mark — one recorded before waits were placed — is asked at the gate's
// approval itself, for the same reason.
func answeredDecision(step record.DesignStep, waiting record.Awaiting, gate *record.DesignGate, noReview bool) Decision {
	decision := Decision{Step: step, Artifact: answeredDocument(waiting)}
	if waiting.AnsweredDocument != nil {
		decision.Version = waiting.AnsweredDocument.Version
	}
	if noReview {
		return decision
	}
	switch waiting.AnsweredBy {
	case record.AnsweredByApproval:
		decision.Kind = record.DecisionDesignApproved
	case record.AnsweredByChanges:
		decision.Kind, decision.Mark = record.DecisionDesignChangesRequested, waiting.Since
		if decision.Mark == (record.EventMark{}) && gate != nil {
			decision.Mark = gate.ApprovedMark
		}
	}
	return decision
}

// writeApprovalReplaced writes to task what took the place of the approval that answered waiting,
// where the predicate holds that approval no longer the person's word (replaced, SupersedingApproval
// asked of actGate; nil while it stands, which writes nothing). A changes request is named only
// where actGate still holds the document approved: there the gate's approval in its place
// (Supersession.By) replaced it, and with none, a later changes request cleared it
// (classify.ApplyDesignGateEvent). Where the step holds another document now (documentReplaced),
// the architect registered that one since, which starts a fresh row (gateRegistered): no review
// took the approval away, and an approval actGate holds is of that other document, so the sentence
// names the document and no review. actDocument is the step's document, as the sentence before
// names it.
func writeApprovalReplaced(task *strings.Builder, replaced *Supersession, waiting record.Awaiting, actGate *record.DesignGate,
	actDocument string) {
	if replaced == nil {
		return
	}
	if other, ok := documentReplaced(waiting, actGate); ok {
		fmt.Fprintf(task, " That approval no longer stands: another document, %s, is now registered as your %s.", other, actDocument)
		return
	}
	if replaced.By != nil {
		fmt.Fprintf(task, " That approval no longer stands: a later approval, of version %d, replaced it.", replaced.By.Version)
		return
	}
	task.WriteString(" That approval no longer stands: a later changes request cleared it.")
}

// documentReplaced is the document actGate holds in place of the one the review that answered
// waiting was of (answeredDocument), and true; false where actGate holds that document or is nil,
// and where the answer names no document (an answer an earlier release wrote of a wait on a
// question, which is asked of the document the gate holds). actGate is the row of the step the
// answer recorded (answeredStep), so a document other than the one reviewed there is one the
// architect registered on that step since, which started a fresh row (gateRegistered).
func documentReplaced(waiting record.Awaiting, actGate *record.DesignGate) (string, bool) {
	document := answeredDocument(waiting)
	if actGate == nil || document == "" || actGate.ArtifactID == document {
		return "", false
	}
	return actGate.ArtifactID, true
}

// answeredStep is the step whose document the act that answered waiting was of — a person's
// review, or a newer version — and the row gates hold for it now. Under a deployment that gates
// more than one step that need not be step, the step in question (stepInQuestion): an approval
// that opens the last closed step leaves the resume speaking of the first required step, a
// registration of another step's document moves a gate wait to that document while the step in
// question stays closed, a review of either document answers a wait on a question, and a review
// of the other document after the answer moves the step in question. It is the step the answer
// recorded (record.AnsweredDocument.Step), first: the row that held the document then is the
// only one that knew it, since a registration of another document on that step afterwards leaves
// no row holding it, and the step still says where the person's review was given. An answer an
// earlier release wrote records no step, and is read as of the step of the row that holds its
// document (answeredDocument), and where it names none (a wait on a question) or no row holds it,
// as of step.
func answeredStep(waiting record.Awaiting, step record.DesignStep, gates map[record.DesignStep]record.DesignGate) (record.DesignStep, *record.DesignGate) {
	if answered := waiting.AnsweredDocument; answered != nil && answered.Step != "" {
		return answered.Step, gateOf(gates, answered.Step)
	}
	if document := answeredDocument(waiting); document != "" {
		if gate := gateOfDocument(gates, document); gate != nil {
			return gate.Step, gate
		}
	}
	return step, gateOf(gates, step)
}

// answeredDocument is the document the act that answered waiting was of: the one the answer
// recorded (record.AnsweredDocument.Artifact), and for an answer an earlier release wrote, which
// records none, the one the wait records (record.Awaiting.Artifact), which a review or a newer
// version that answers a wait on a document is always of (answerAwaitedGate, answerOpenGates; a
// registration of another document moves the wait to it first, answerRegisteredGate). It is ""
// for such an answer of a wait on a question.
func answeredDocument(waiting record.Awaiting) string {
	if answered := waiting.AnsweredDocument; answered != nil && answered.Artifact != "" {
		return answered.Artifact
	}
	return waiting.Artifact
}

// recordedDecline is the decline the mirror holds for ask, nil where no person declined it: it
// carries the sentences Dispatch wrote for the asking agent and the person's own note, which the
// resume prints. A record holding no such row — a decline mirrored by a release before this one,
// or a row a later read has not reached — leaves the resume naming the ask alone.
func recordedDecline(asks []record.RootAsk, ask string) *record.AskDecline {
	for i := range asks {
		if asks[i].Ask == ask && asks[i].Decline != nil {
			return asks[i].Decline
		}
	}
	return nil
}
