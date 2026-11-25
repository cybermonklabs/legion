package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
		unresumable, claimedBy, err := Unresumable(ctx, e.store, tx, root)
		if err != nil {
			return nil, false, err
		}
		if claimedBy != "" {
			unresumable += " by " + claimedBy
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
			if err := e.enterAwaiting(ctx, tx, root, decision.Waiting, gates, decision.Why); err != nil {
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
			version := root.Awaiting.GateVersion
			if gate := gateOfDocument(gates, root.Awaiting.Artifact); gate != nil {
				version = gate.LatestVersion
			}
			e.logOnCommit(ctx, "workflow: the design gate of a root waiting on a person is open, which answers its wait", "issue", root.Key,
				"version", version, "ask", root.Awaiting.Ask)
			if err := e.answerWait(ctx, tx, root, record.AnsweredByApproval, fmt.Sprintf("version %d", version)); err != nil {
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
	return e.answerWait(ctx, tx, root, record.AnsweredByApproval, fmt.Sprintf("version %d", root.Awaiting.GateVersion)+gateOffAnswer)
}

// gateOffAnswer ends the detail of a gate wait the empty-list rule answered (leaveGateOff).
// That answer is recorded, and shown on the API, as AnsweredByApproval: the contract's answeredBy
// enum, which refuses a value it does not list, has no value of its own for it. But no person
// approved anything, so the detail tells the resume (resumeTask) never to say so, should the gate
// be on again by then.
const gateOffAnswer = ", answered with the design gate off"

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
// could not resume (Unresumable: Unresumable's word, with the claim's holder where it names one,
// "" when it could), its architect's run at the root's
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
// an approval request of requestOf, the one document a root can wait on now
// (gateRequestArtifact), is open on the person's turn. An approval request of any other document
// asks the person for nothing the root waits on.
func withHuman(asks []record.RootAsk, requestOf string) (questions []record.RootAsk, request bool) {
	for _, ask := range asks {
		switch {
		case ask.GateRequestOf(requestOf):
			request = request || ask.WithHuman()
		case ask.ByTree && ask.Question() && ask.WithHuman():
			questions = append(questions, ask)
		}
	}
	slices.SortFunc(questions, func(a, b record.RootAsk) int {
		return cmp.Or(cmp.Compare(a.WaitingOnSeq, b.WaitingOnSeq), cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Ask, b.Ask))
	})
	return questions, request
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

// gateArtifactOf is the document gate is registered on, "" for no gate.
func gateArtifactOf(gate *record.DesignGate) string {
	if gate == nil {
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
	standsOn, _ := classify.StandingGate(steps, gates)
	questions, request := withHuman(asks, standsOn.ArtifactID)
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
// read the person's answer, and the controller is told (AwaitingPersonNotice), the document of a
// gate wait named by the step of gates that records it. reason is the decision's, for the log.
func (e *Engine) enterAwaiting(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting,
	gates map[record.DesignStep]record.DesignGate, reason string) error {
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
	var step record.DesignStep
	if waiting.Artifact != "" {
		if gate := gateOfDocument(gates, waiting.Artifact); gate != nil {
			step = gate.Step
		}
	}
	notice, err := record.ControllerNoticeOf(AwaitingPersonNotice(root.Key, waiting, step))
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, root.Key, notice); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: root waits on a person and keeps its admission slot", "issue", root.Key, "tree", root.Tree,
		"reason", reason, "since_seq", waiting.Since.Seq)
	return nil
}

// AwaitingPersonNotice is the awaiting-person notice of root, which waits on a person and keeps
// its admission slot: the ask it waits on (a dispatch:// link), or the document of step whose
// version waits on a person's review (a dispatch:// link, stepRef) and that version.
func AwaitingPersonNotice(root string, waiting record.Awaiting, step record.DesignStep) texts.AwaitingPersonRecord {
	notice := texts.AwaitingPersonRecord{NoticeHead: texts.NoticeHead{Kind: texts.AwaitingPerson, Issue: root}}
	if waiting.GateVersion != 0 {
		notice.Document, notice.Version = stepRef(root, step), waiting.GateVersion
	} else {
		notice.Ask = itemRef(root, "ask", waiting.Ask)
	}
	return notice
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
// how the document of the step in question stands now (gateInQuestion), and what of the root is
// still with the person or waits on it. It is a handout (Store.StartHandout): what the architect
// handed back before it is answered or listed here, and only a hand-back after it starts the
// next wait. The members an open gate releases are not started here: each is an issue the cap
// counts, so each takes its own place in the waiting line and starts at its own slot
// (record.Waiting's LineFirstPhase), in the same admission pass that resumed this root.
//
// The resume hands the architect work it was not on, so it raises the architect's phase entry and
// writes this task as the work of that entry (EnterArchitect), and the start carries the number
// (record.SuperviseRequest.Entry) as a phase worker's start does. That is what makes a resume
// survivable: the entry and this task live on the record, so a start row that reaches no claim
// leaves the architect's claim below that entry, and the entry sweep sends it that entry's work.
// The raise marks the task as one to send as written (record.PhaseRow.EntryVerbatim), so that work
// is this text (SendArchitectStart): it names the answer and no kept decision, so nothing it says
// can have been told by another start. Both callers get it — the promotion of an answered root
// (admit's record.LineAnswered branch) and tellInPlace, which gives a live architect this task
// where it stands.
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
	gates, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	step, gate := gateInQuestion(e.cfg.Steps, gates)
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	if err := e.store.StartHandout(ctx, tx, root.Key); err != nil {
		return err
	}
	task := resumeTask(root.Key, waiting, step, gate, gateDocuments(gates), asks[root.Key], e.armed())
	entry, err := e.EnterArchitect(ctx, tx, root, task, true)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, root.Key, record.SuperviseRequest{
		Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation, Task: task, Entry: entry,
	}); err != nil {
		return err
	}
	return e.store.MarkIssueMoved(ctx, tx, root.Key, e.now())
}

// gateInQuestion is the step a resume speaks of and the row that step holds (resumeTask): the
// step the person was asked about (classify.StandingGate), which is the one the resume answers
// for; else the first required step that is not open, which nobody has registered and whose
// document the architect has still to register; and when every required step is open the first
// required step, whose version the resume says the work runs at. The required steps are the
// ones the tree is gated on (classify.TreeSteps), as in the two rules it reads. The row is nil
// for the unregistered step, as a root nobody has registered a gate for holds none.
func gateInQuestion(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) (record.DesignStep, *record.DesignGate) {
	if gate, standing := classify.StandingGate(steps, gates); standing {
		return gate.Step, &gate
	}
	step, _, closed := classify.FirstClosedStep(steps, gates)
	if !closed {
		step = classify.RequiredSteps(classify.TreeSteps(steps, gates))[0]
	}
	gate, ok := gates[step]
	if !ok {
		return step, nil
	}
	return step, &gate
}

// RootResume is the task an answered root's architect resumes with, as the record of its kind.
//
// The act that answered its wait (Awaiting.AnsweredBy, texts.ActApproved and the others) and that
// act's own detail (Awaiting.AnsweredDetail, recorded when the wait was answered, so a later act of
// the same kind does not rename it): the ask it was on as a dispatch:// link, or the document of
// step as one (stepRef) with the version the act was about; the closer of an ask nobody will
// answer (AnsweredByClosed); and the person who acted and when, with their words in a block of
// their own, as the act's Dispatch event carried them and the ask's mirror row keeps them
// (record.RootAsk.PersonAnswerWords and PersonWords): an answer with the options it chose, a reply,
// the reason of a changes request (read from the act's detail, with the person and the time the
// gate records the request with, changesWords), a resolution's reason, and a
// decline's note beside the sentences Dispatch wrote for the asking agent. The words are cut to
// texts.OneAct with the count of the rest, or with the mark that more follows where the event did
// not carry all of them (texts.Carry); where the resume carries the words of two acts, the act's
// and a later changes request's reason, each is cut to texts.EachOfSeveral (eachOfSeveral).
// gateArtifacts are the documents the root's gates are registered on, whose approval requests are
// answered by a review rather than by a person's act on the ask (record.RootAsk.PersonActSeq),
// which says which act's words those are.
//
// How the document of step stands at the resume, read from that step's gate now, so an approval
// a newer version superseded, or a gate closed again since, is said as it is: the document (stepRef)
// wherever the step has a gate, whatever the act, as main named it in each sentence of the gate
// ("Your <doc> is not approved at version N", "the approval request for <doc> version N", "When the
// <doc> is ready"), its latest version, whether the gate is open and whether it is armed. A step
// with no gate row (gateInQuestion's nil, a question asked before any step was registered, or the
// step after the one whose approval answered the wait) names no gate state, as main's resume wrote
// no gate sentence for it (`case gate == nil`), but on an approval, which names whether the design
// gate is armed all the same: that is what says whether a person approved, as main said it from
// armed alone ("Your %s %s on %s was approved." or "The design gate on %s is off, so no approval
// of your %s %s is asked for."). An open gate keeps texts.DesignApprovedWake word for word, since
// the daemon sends no design-approved notice to a root waiting on a person (applyArtifact,
// gateRegistered), so this resume is that root's design-approved wake. A wait the gates.design:
// off rule answered (gateOffAnswer) says so (GateOffAtAnswer), so it is never read as a person's
// approval. A changes request the gate records as told after the wait began that did not answer
// it, which no notice told (tellChangesOnce), is named with its version, the document of step it
// was made on and its reason in a block of its own.
//
// What is still with the person (withHuman): the asks as dispatch:// links, and the version whose
// approval request is with them. What waits on the architect: the asks on its turn, a tree
// question or the closed gate's approval request, and whether that request's thread is open. The
// steps for each are the root architect's prompt's (roles/architect-root.md).
func RootResume(key string, waiting record.Awaiting, step record.DesignStep, gate *record.DesignGate, gateArtifacts []string,
	asks []record.RootAsk, armed bool,
) texts.RootResumeRecord {
	gateOpen := gate != nil && classify.DesignGateOpen(*gate)
	gateClosed := gate != nil && !gateOpen
	task := texts.RootResumeRecord{TaskHead: texts.TaskHead{Kind: texts.RootResume, Issue: key}}
	if gate != nil {
		task.Document, task.DocumentVersion = stepRef(key, step), gate.LatestVersion
		task.GateOpen, task.GateArmed = &gateOpen, &armed
	}
	switch waiting.AnsweredBy {
	case record.AnsweredByApproval:
		detail, gateOff := strings.CutSuffix(waiting.AnsweredDetail, gateOffAnswer)
		task.Act, task.Document, task.GateOffAtAnswer = texts.ActApproved, stepRef(key, step), gateOff
		task.Version, _ = detailVersion(detail)
		task.GateArmed = &armed
	case record.AnsweredByChanges:
		task.Act, task.Document = texts.ActChangesRequested, stepRef(key, step)
		version, reason := changesOfDetail(waiting.AnsweredDetail)
		task.Version = version
		words := changesWords(version, reason, gate, gateArtifactOf(gate), asks)
		task.By, task.At, words.By, words.At = words.By, words.At, "", time.Time{}
		task.Reason = &words
	case record.AnsweredByVersion:
		task.Act, task.Document = texts.ActVersion, stepRef(key, step)
		task.Version, _ = detailVersion(waiting.AnsweredDetail)
	case record.AnsweredByRetraction:
		task.Act, task.Ask = texts.ActRetracted, itemRef(key, "ask", waiting.Ask)
	case record.AnsweredByClosed:
		task.Act, task.Ask, task.ClosedBy = texts.ActClosed, itemRef(key, "ask", waiting.Ask), waiting.AnsweredDetail
	case record.AnsweredByAsk:
		task.Act, task.Ask = texts.ActAsk, itemRef(key, "ask", waiting.Ask)
		carryAct(&task, askRow(asks, waiting.Ask), gateArtifacts)
	case record.AnsweredByReply:
		task.Act, task.Ask = texts.ActReply, itemRef(key, "ask", waiting.AnsweredDetail)
		carryAct(&task, askRow(asks, waiting.AnsweredDetail), gateArtifacts)
	case record.AnsweredByTurn:
		task.Act, task.Ask = texts.ActTurn, itemRef(key, "ask", waiting.AnsweredDetail)
	case record.AnsweredByDecline:
		task.Act, task.Ask = texts.ActDeclined, itemRef(key, "ask", waiting.AnsweredDetail)
		carryDecline(&task, askRow(asks, waiting.AnsweredDetail))
	default:
		task.Act = cmp.Or(string(waiting.AnsweredBy), texts.ActActed)
	}
	if gate != nil && gate.ToldChangesVersion != nil && !(waiting.Since.Seq > 0 && gate.ToldChangesMark.Seq == 0) &&
		gate.ToldChangesMark.After(waiting.Since) {
		detail := changesDetail(*gate.ToldChangesVersion, gate.ToldChangesReason)
		if waiting.AnsweredBy != record.AnsweredByChanges || waiting.AnsweredDetail != detail {
			task.LaterChangesVersion = *gate.ToldChangesVersion
			later := changesWords(*gate.ToldChangesVersion, gate.ToldChangesReason, gate, gateArtifactOf(gate), asks)
			task.LaterReason = &later
		}
	}
	eachOfSeveral(task.Answer, task.Reply, task.Reason, task.Note, task.LaterReason)
	requestOf := ""
	if gate != nil {
		requestOf = gateRequestOf(*gate, armed)
	}
	questions, request := withHuman(asks, requestOf)
	if request {
		task.ApprovalRequest = gate.LatestVersion
	}
	for _, ask := range questions {
		task.WithPerson = append(task.WithPerson, itemRef(key, "ask", ask.Ask))
	}
	for _, ask := range asks {
		isRequest := ask.GateRequestOf(gateArtifactOf(gate))
		if isRequest && ask.Open {
			task.RequestOpen = true
		}
		if ask.Open && ask.WaitingOn == "agent" && (isRequest && gateClosed || ask.ByTree && ask.Question()) {
			task.WaitingOnYou = append(task.WaitingOnYou, itemRef(key, "ask", ask.Ask))
		}
	}
	return task
}

// resumeTask is RootResume's record as the root's architect reads it.
func resumeTask(key string, waiting record.Awaiting, step record.DesignStep, gate *record.DesignGate, gateArtifacts []string,
	asks []record.RootAsk, armed bool,
) string {
	return taskText(RootResume(key, waiting, step, gate, gateArtifacts, asks, armed))
}

// detailVersion reads the version an answer's detail names ("version <n>", the start of every
// detail a review or a version writes: changesDetail, answerAwaitedGate, answerRegisteredGate), and
// what follows it; 0 and the whole detail where it names none.
func detailVersion(detail string) (int, string) {
	rest, ok := strings.CutPrefix(detail, "version ")
	if !ok {
		return 0, detail
	}
	digits := rest
	if end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		digits = rest[:end]
	}
	version, err := strconv.Atoi(digits)
	if err != nil {
		return 0, detail
	}
	return version, rest[len(digits):]
}

// changesOfDetail reads a changes request's version and reason back from the detail
// changesDetail wrote, the reason as the person gave it: the detail quotes it, so it is unquoted.
// A detail that does not read so gives its version and what follows it as the reason.
func changesOfDetail(detail string) (int, string) {
	version, rest := detailVersion(detail)
	quoted, ok := strings.CutPrefix(rest, ": ")
	if !ok {
		return version, rest
	}
	reason, err := strconv.Unquote(quoted)
	if err != nil {
		return version, quoted
	}
	return version, reason
}

// changesWords is the reason of a changes request of version, cut to texts.OneAct (texts.Carry),
// by the person who asked for the changes and when. The gate records the request it tells with its
// reviewer and the time Dispatch gives the review (record.DesignGate.ToldChangesBy and
// ToldChangesMark, written from the review's own event or a registration's read of it), and those
// name it wherever the gate holds this version and this reason. A gate that names no reviewer — a
// row an older daemon wrote — leaves the person's answer of the approval request of the gate's
// document (artifact) whose words are that reason, which is how a person requests changes through
// the request (Dispatch's reviewFromAnswer trims the reason the review keeps), as its mirror row
// keeps them: that answer's words are the reason as its event carried them, so a body the event
// marked cut says that more follows.
func changesWords(version int, reason string, gate *record.DesignGate, artifact string, asks []record.RootAsk) texts.Words {
	words := texts.Carry(reason, texts.OneAct, false)
	if gate != nil && gate.ToldChangesVersion != nil && *gate.ToldChangesVersion == version && gate.ToldChangesReason == reason {
		words.By, words.At = gate.ToldChangesBy, utc(gate.ToldChangesMark.At)
	}
	if words.By != "" {
		return words
	}
	for _, ask := range asks {
		answer := ask.PersonAnswerWords
		if answer != nil && ask.GateRequestOf(artifact) && strings.TrimSpace(answer.Text) == reason {
			words = answer.Carry(texts.OneAct)
			words.By, words.At = answer.By, utc(answer.At)
			return words
		}
	}
	return words
}

// carryAct puts the person's act on row that answered the wait into task: the person, the time
// and the options an answer chose as fields, and the words in the block the act is: an answer of
// the ask (PersonAnswerWords), else the act PersonSeq records (PersonWords), which is the ask's
// close or a reply in its thread. A close by the person is a resolution, its words its reason,
// and a decline among them its note. Which of the two acts answered the wait is the one rule
// RootAsk.PersonActSeq gives, read against gateArtifacts. A row the record does not hold, or one an
// older daemon wrote, which kept no words, carries none.
func carryAct(task *texts.RootResumeRecord, row *record.RootAsk, gateArtifacts []string) {
	if row == nil {
		return
	}
	seq := row.PersonActSeq(gateArtifacts...)
	switch {
	case seq == 0:
	case seq != row.PersonSeq:
		task.Answer = carryWords(task, row.PersonAnswerWords)
	case seq == row.ClosedSeq && row.Decline != nil:
		carryDecline(task, row)
	case seq == row.ClosedSeq:
		task.Reason = carryWords(task, row.PersonWords)
	default:
		task.Reply = carryWords(task, row.PersonWords)
	}
}

// carryDecline puts a person's decline of the ask on row into task: the sentences Dispatch wrote
// for the asking agent and the person's note, both as the mirror row holds them
// (record.RootAsk.Decline), the daemon writing neither, so the architect reads the person's words
// and not the daemon's paraphrase of them. The note's person, time and cut are those of the
// decline's own act (PersonWords) where the row records the close as the person's act. A record
// holding no decline — one mirrored by a release before this one, or a row a later read has not
// reached — leaves the resume naming the ask alone.
func carryDecline(task *texts.RootResumeRecord, row *record.RootAsk) {
	if row == nil || row.Decline == nil {
		return
	}
	task.DispatchWords = row.Decline.Words
	words := row.PersonWords
	if words == nil || row.PersonSeq != row.ClosedSeq {
		words = &record.ActWords{Text: row.Decline.Note}
	}
	task.Note = carryWords(task, words)
}

// carryWords is words as task's block carries them, cut to texts.OneAct, with the person and the
// time they acted put on task's own fields, where they are named once; their options too. Nil
// where there are none, and where the act carried nothing at all.
func carryWords(task *texts.RootResumeRecord, words *record.ActWords) *texts.Words {
	if words == nil {
		return nil
	}
	task.By, task.At, task.Options = words.By, utc(words.At), words.Options
	if words.Text == "" && !words.More {
		return nil
	}
	carried := words.Carry(texts.OneAct)
	return &carried
}

// eachOfSeveral cuts each of blocks that carries words to texts.EachOfSeveral where more than one
// does: a resume that carries the words of several acts carries up to that much of each
// (dispatch://INFRA-3686/spec R1). A block with no words, the mark that more follows alone (a
// payload_omitted stand-in), carries no act's words and does not count, as keptBound counts the
// kept decisions. Each block was cut to texts.OneAct, which is a head of the act's words, so its
// head of texts.EachOfSeveral is the act's own, and the units the second cut leaves off add to the
// count of the rest. A block whose count is not known keeps its mark that more follows.
func eachOfSeveral(blocks ...*texts.Words) {
	worded := 0
	for _, words := range blocks {
		if words != nil && words.Text != "" {
			worded++
		}
	}
	if worded < 2 {
		return
	}
	for _, words := range blocks {
		if words == nil || words.Text == "" {
			continue
		}
		head, rest := texts.Bound(words.Text, texts.EachOfSeveral)
		words.Text = head
		if !words.More {
			words.Rest += rest
		}
	}
}

// utc is t in UTC, the zero time as it is.
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// askRow is the mirror row of ask among asks, nil where the record holds none.
func askRow(asks []record.RootAsk, ask string) *record.RootAsk {
	for i := range asks {
		if asks[i].Ask == ask {
			return &asks[i]
		}
	}
	return nil
}
