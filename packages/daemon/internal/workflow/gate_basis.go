package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// What a registration on an ancestor's approval does beyond a person's approval of the root's own
// document (dispatch://INFRA-3320, R1, R2 and R8): the record of the basis on the root's issue, the
// design-approved notice that names it, the pending wait's seed, and the resting-root notice to the
// ancestor's architect. gateRegistered writes the row; this file says what the row tells.

// basisOpenedLine is the first line of the message posted on a root's issue for each step an
// ancestor's approval opens (R2), by which a reader counts such messages.
const basisOpenedLine = "Design gate opened on an ancestor's approval."

// registeredBasis is the basis fact rests the step on, stamped with the engine's own time of the
// registration (record.GateBasis.RegisteredAt, the waiting line's moment for a step a basis
// opens), or nil for a registration with none. A step the deployment does not gate asks nobody
// and opens at its registration, as every registration did under `gates.design: off`, so it takes
// no basis either. The value is a copy: the fact is the API's.
func (e *Engine) registeredBasis(fact intake.GateRegistered, ungated bool) *record.GateBasis {
	if fact.Basis == nil || ungated {
		return nil
	}
	basis := *fact.Basis
	basis.RegisteredAt = e.now()
	return &basis
}

// recordViewOf writes the view fact a registration of root's spec step carries (Issue.ViewOf, R4):
// the ancestor whose requirement blocks the root's documents read as a view on, "" for none. It is
// what that registration read of the root's documents, so the newest registration's fact stands;
// a registration of another step reads no view and leaves the fact as it is. root is updated in
// place, so the caller's copy stays the row.
func (e *Engine) recordViewOf(ctx context.Context, tx pgx.Tx, root *record.Issue, step record.DesignStep, viewOf string) error {
	if step != record.DesignStepSpec || root.ViewOf == viewOf {
		return nil
	}
	root.ViewOf = viewOf
	return e.store.PutIssue(ctx, tx, *root)
}

// sameBasis says whether a and b rest a step on one ancestor's approval: the same document of the
// same ancestor at the same version, or no basis at all.
func sameBasis(a, b *record.GateBasis) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Issue == b.Issue && a.ArtifactID == b.ArtifactID && a.Version == b.Version
}

// openedNotice is the design-approved notice of a registration that opened gate on root. For a
// step a person's approval of the root's own document opened, it names the version registered and
// reviewer, the person whose approval the read carried, as it always has. For a step an ancestor's
// approval opened it names that basis: the ancestor's document for the step as its reference, the
// version a person approved and that person, with a reason that names the root's version and says
// that no person approved it (R1, R2).
func openedNotice(root string, gate record.DesignGate, reviewer string) record.Notice {
	step := gate.Step.OrSpec()
	if !gate.OnBasis() {
		return record.Notice{Kind: "design-approved", Version: gate.LatestVersion, Actor: reviewer, Ref: stepRef(root, step)}
	}
	basis := gate.Basis
	return record.Notice{Kind: "design-approved", Version: basis.Version, Actor: basis.Approver, Ref: stepRef(basis.Issue, step),
		Reason: fmt.Sprintf("An ancestor's approval opened the %s step of %s. A person approved version %d of the %s of %s. "+
			"%s registered version %d of its own %s on that approval. No person approved that version.",
			step, root, basis.Version, step.Document(), basis.Issue, root, gate.LatestVersion, step.Document())}
}

// tellRegisteredBasis tells what a registration on a basis does beyond a person's approval of the
// root's own document, given the step's row before the registration (before, nil for none) and the
// gate the registration wrote. stood says the step already stood open at this version on this
// same basis, so its record on the root's issue was posted then. The opening of a step that waited
// on the ancestor's request, by the person's answer, tells the same as a registration on that
// approval would (openPendingBasis), with the pending row as before.
//
// An approved basis posts one message on the root's issue that records which approval opened the
// step (basisOpenedMessage), unless it stood, and tells the ancestor's architect of the root's first
// rest on it (queueRestingRoot, once per root, generation and ancestor). A pending basis queues the
// pending-basis seed, whose read of the ancestor's request opens or ends the wait when that
// request's event came between register_gate's read and this commit, or never came, and tells the
// ancestor's architect of the pending registration. A registration that leaves no basis on the
// ancestor a pending row of the step waited on ends that wait, and the ancestor's architect is told;
// the root's own architect made that registration, so no basis-ended notice tells it what it did.
func (e *Engine) tellRegisteredBasis(ctx context.Context, tx pgx.Tx, root record.Issue, before *record.DesignGate, gate record.DesignGate, stood bool) error {
	step := gate.Step.OrSpec()
	if before != nil && before.PendingOnBasis() && (gate.Basis == nil || gate.Basis.Issue != before.Basis.Issue) {
		ended := *before.Basis
		what := fmt.Sprintf("%s registered its %s step again with no basis on %s. The step no longer waits on approval request %s of version %d of the %s of %s.",
			root.Key, step, ended.Issue, ended.Pending.Ask, ended.Version, step.Document(), ended.Issue)
		if err := e.queueRestingRoot(ctx, tx, root, step, ended, restingEnded(ended.Pending.Ask), what); err != nil {
			return err
		}
	}
	switch {
	case gate.PendingOnBasis():
		basis := *gate.Basis
		if err := e.enqueue(ctx, tx, root.Key, record.PendingBasisSeed{Step: step, ArtifactID: gate.ArtifactID,
			BasisArtifactID: basis.ArtifactID, BasisVersion: basis.Version, Ask: basis.Pending.Ask, Generation: root.Generation}); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a design gate step waits on an ancestor's approval request", "issue", root.Key, "step", step,
			"version", gate.LatestVersion, "ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask)
		what := fmt.Sprintf("%s registered its %s step on version %d of the %s of %s (%s). The step stays closed until a person answers approval request %s of that version. %s rests on %s while the step waits.",
			root.Key, step, basis.Version, step.Document(), basis.Issue, stepRef(basis.Issue, step), basis.Pending.Ask, root.Key, basis.Issue)
		return e.queueRestingRoot(ctx, tx, root, step, basis, restingPending(basis.Pending.Ask), what)
	case gate.OnBasis():
		basis := *gate.Basis
		if !stood {
			if err := e.enqueue(ctx, tx, root.Key, record.MessagePost{Body: basisOpenedMessage(root.Key, gate)}); err != nil {
				return err
			}
		}
		e.logOnCommit(ctx, "workflow: a design gate step opened on an ancestor's approval", "issue", root.Key, "step", step,
			"version", gate.LatestVersion, "ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "recorded_before", stood)
		what := fmt.Sprintf("An ancestor's approval opened the %s step of %s. A person approved version %d of the %s of %s (%s). %s rests on this approval.",
			step, root.Key, basis.Version, step.Document(), basis.Issue, stepRef(basis.Issue, step), root.Key)
		return e.queueRestingRoot(ctx, tx, root, step, basis, restingOpened, what)
	}
	return nil
}

// basisOpenedMessage is the message posted on root's Dispatch issue for a step an ancestor's
// approval opened (R2): its first line (basisOpenedLine), then one fact to a sentence: the step;
// the root's document and the version registered, which no person approved; the ancestor, its
// document and the version a person approved; that person and Dispatch's time of the approval;
// the ancestor's requirements the root's checks serve; the architect session that registered it;
// and, quoted, the architect's reason. It asks nothing.
func basisOpenedMessage(root string, gate record.DesignGate) string {
	step := gate.Step.OrSpec()
	basis := gate.Basis
	var body strings.Builder
	line := func(format string, args ...any) {
		body.WriteString("- ")
		fmt.Fprintf(&body, format, args...)
		body.WriteString("\n")
	}
	body.WriteString(basisOpenedLine + "\n\n")
	line("The step is %s.", step)
	line("The %s of %s for this step is %s (%s).", step.Document(), root, stepRef(root, step), gate.ArtifactID)
	line("The architect of %s registered version %d of this document.", root, gate.LatestVersion)
	line("No person approved version %d of this document.", gate.LatestVersion)
	line("The ancestor is %s.", basis.Issue)
	line("The %s of %s for this step is %s (%s).", step.Document(), basis.Issue, stepRef(basis.Issue, step), basis.ArtifactID)
	line("A person approved version %d of this document.", basis.Version)
	line("The person is %s.", basis.Approver)
	if basis.Approval.At.IsZero() {
		line("Dispatch gave no time for this approval.")
	} else {
		line("Dispatch recorded this approval at %s.", basis.Approval.At.UTC().Format(time.RFC3339))
	}
	if len(basis.Serves) == 0 {
		line("The daemon records no requirement of %s that the checks of %s serve.", basis.Issue, root)
	} else {
		line("The checks of %s serve these requirements of %s:", root, basis.Issue)
		for _, served := range basis.Serves {
			body.WriteString("  - " + served + "\n")
		}
	}
	if basis.Session == "" {
		line("The daemon does not know the architect session that registered this step.")
	} else {
		line("The architect session %s registered this step.", basis.Session)
	}
	body.WriteString("\nThe reason of the architect:\n\n")
	for reason := range strings.SplitSeq(basis.Reason, "\n") {
		body.WriteString(strings.TrimRight("> "+reason, " ") + "\n")
	}
	return strings.TrimSuffix(body.String(), "\n")
}

// restingRootEvent names one event a resting-root notice tells (R8), and keys the record that it
// was told (queueRestingRoot): each is told once per root, generation and ancestor, and the events
// of a pending wait once per request as well, so a registration read again, a redelivered event
// and a seed that reads the same again tell the ancestor's architect nothing new.
type restingRootEvent string

// restingOpened is the root's first rest on the ancestor's approval in its generation: a basis on
// the ancestor opened the step, at a registration or at a person's answer to the request a pending
// step waited on.
const restingOpened restingRootEvent = "opened"

// restingPending is a registration whose step waits on the ancestor's open approval request ask.
func restingPending(ask string) restingRootEvent { return "pending:" + restingRootEvent(ask) }

// restingEnded is the end, with no opening, of a step's wait on the ancestor's request ask.
func restingEnded(ask string) restingRootEvent { return "ended:" + restingRootEvent(ask) }

// queueRestingRoot tells the architect that owns the ancestor basis names that root's step rests
// on, or waits on, that ancestor's approval: a resting-root notice keyed by the ancestor's issue,
// so it reaches that architect as any notice keyed by an issue does (the outbox's
// owningArchitect). It names the ancestor's document for the step as its reference and the
// version the step rests or waits on, and its reason is what (what happened, naming the root and
// its step) and the ancestor's requirements the root's checks serve (basis.Serves). It is the one
// way the notice is queued: a registration (tellRegisteredBasis), the end of a pending wait, and a
// resting root's close or its leaving the workflow each call it with the event it tells.
//
// Where the daemon records the ancestor in no live tree (ancestorInLiveTree) it queues nothing
// and records nothing as told: no architect of the ancestor would hear it, and the ancestor's start
// task at its admission names the roots that rest on it. Otherwise each event is told once
// (restingRootEvent), marked in the notice's own transaction.
func (e *Engine) queueRestingRoot(ctx context.Context, tx pgx.Tx, root record.Issue, step record.DesignStep, basis record.GateBasis, event restingRootEvent, what string) error {
	live, err := e.ancestorInLiveTree(ctx, tx, basis.Issue)
	if err != nil {
		return err
	}
	if !live {
		e.logOnCommit(ctx, "workflow: the daemon records the ancestor in no live tree, so no resting-root notice is queued", "issue", root.Key,
			"ancestor", basis.Issue, "event", string(event))
		return nil
	}
	fresh, err := e.store.MarkProcessed(ctx, tx, "resting-root-told", fmt.Sprintf("%s:%d:%s:%s", root.Key, root.Generation, basis.Issue, event))
	if err != nil || !fresh {
		if err == nil {
			e.logOnCommit(ctx, "workflow: the ancestor's architect was already told this of a resting root", "issue", root.Key,
				"ancestor", basis.Issue, "event", string(event))
		}
		return err
	}
	return e.notice(ctx, tx, basis.Issue, record.Notice{Kind: "resting-root", Role: claim.RoleArchitect, Version: basis.Version,
		Ref: stepRef(basis.Issue, step), Reason: what + " " + servesSentence(root.Key, basis.Issue, basis.Serves)})
}

// servesSentence names the ancestor's requirements the checks of root serve, as a basis recorded
// them.
func servesSentence(root, ancestor string, serves []string) string {
	if len(serves) == 0 {
		return fmt.Sprintf("The daemon records no requirement of %s that the checks of %s serve.", ancestor, root)
	}
	return fmt.Sprintf("The checks of %s serve these requirements of %s: %s.", root, ancestor, strings.Join(serves, ", "))
}

// ancestorInLiveTree says whether the daemon records key in a live tree: key is recorded, and the
// root of the tree its record names is live (record.TreeLive). It is where a notice keyed by key
// reaches an architect.
func (e *Engine) ancestorInLiveTree(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	ancestor, err := e.store.Issue(ctx, tx, key)
	if err != nil || ancestor == nil || ancestor.Tree == "" {
		return false, err
	}
	root := ancestor
	if !claim.IsTreeRoot(ancestor.Key, ancestor.Tree) {
		if root, err = e.store.Issue(ctx, tx, ancestor.Tree); err != nil || root == nil {
			return false, err
		}
	}
	return record.TreeLive(ctx, e.store, tx, *root, e.cfg.Steps)
}

// What a step on an ancestor's approval takes after its registration (R1, R3). A step that waits,
// closed, on the ancestor's open approval request ends that wait on any act R1 names: a changes
// request on the version it waits on, a newer version of the ancestor's document, a close of the
// request that carries no approval of that version, the ancestor leaving R1's first condition, and
// a newer version or review of the root's own document (applyArtifact); and the person's approval
// of the version opens it, as a registration on that approval would. A step that stands open on
// an ancestor's approval changes on nothing done to the ancestor's document, and passes on one act
// alone: a person's changes request on the version it rests on, made after that approval.

// basisEndedAnswer opens the detail of a root's wait on a person that the end of its step's wait
// on an ancestor's request answered (tellPendingEnded). The wait is answered as a changes request
// answers it (record.AnsweredByChanges), since the contract's answeredBy enum, which refuses a
// value it does not list, has no value of its own for it; but no person asked for changes on the
// root's own document, so the detail is what ended the wait instead, naming the act, the
// ancestor's document and its version, after this prefix, by which a resume tells it from a
// person's changes request.
const basisEndedAnswer = "the wait on an ancestor's approval ended: "

// basisEndedWait says whether waiting was answered by the end of a step's wait on an ancestor's
// request (basisEndedAnswer) rather than by a person's changes request on the root's own
// document.
func basisEndedWait(waiting record.Awaiting) bool {
	return waiting.AnsweredBy == record.AnsweredByChanges && strings.HasPrefix(waiting.AnsweredDetail, basisEndedAnswer)
}

// reviewPlace is where a review stands in its issue's Dispatch event log, as a told-once mark keys
// it (tellChanges, tellBasisChanges): Dispatch's time of the review, which every carrier has (the
// live event's created_at, the approval read's at and the review list's created_at are one
// transaction's now()), else its sequence, else none. The time comes first because a read that
// gives no sequence, as the review list gives none, must key a review as its live event does.
func reviewPlace(mark record.EventMark) string {
	switch {
	case !mark.At.IsZero():
		return fmt.Sprint(mark.At.UnixNano())
	case mark.Seq > 0:
		return fmt.Sprintf("s%d", mark.Seq)
	}
	return "none"
}

// actorOr names actor as Dispatch names it (`<kind>:<id>`), or fallback where Dispatch names
// nobody.
func actorOr(actor dispatch.Actor, fallback string) string {
	if name := actor.Name(); name != "" {
		return name
	}
	return fallback
}

// documentOf names issue's document for step as a sentence names it: the document, its issue and
// its reference.
func documentOf(issue string, step record.DesignStep) string {
	return fmt.Sprintf("the %s of %s (%s)", step.Document(), issue, stepRef(issue, step))
}

// waitEnded says that root's step no longer waits on basis's request, which every telling of a
// pending step's end says after the act that ended it.
func waitEnded(root string, step record.DesignStep, basis record.GateBasis) string {
	return fmt.Sprintf("The %s step of %s no longer waits on approval request %s of version %d of %s.",
		step, root, basis.Pending.Ask, basis.Version, documentOf(basis.Issue, step))
}

// stepClosed says how a step whose wait ended without opening stands: closed, and registered on
// the root's own document with no basis (record.EndPendingBasis).
func stepClosed(root string, step record.DesignStep) string {
	return fmt.Sprintf("The step stays closed, registered on %s with no basis.", documentOf(root, step))
}

// changesAct is a changes request on version of document by actor, with the reason the read gave,
// as the act that ends a pending step's wait.
func changesAct(actor dispatch.Actor, version int, document, reason string) string {
	if reason == "" {
		return fmt.Sprintf("%s requested changes on version %d of %s.", actorOr(actor, "A person"), version, document)
	}
	return fmt.Sprintf("%s requested changes on version %d of %s: %q.", actorOr(actor, "A person"), version, document, reason)
}

// ownDocumentEnded is what ended gate's wait on its ancestor's request when fact, an event of the
// root's own document at the version registered or a later one, ended it (applyArtifact): a
// person's approval, which opened the step on that approval; a person's changes request; or a
// newer version.
func ownDocumentEnded(root string, gate record.DesignGate, fact intake.DispatchArtifact) string {
	step := gate.Step.OrSpec()
	own := documentOf(root, step)
	ended := waitEnded(root, step, *gate.Basis)
	switch fact.Kind {
	case intake.DispatchArtifactApproved:
		return fmt.Sprintf("%s approved version %d of %s, the root's own document. That approval opened the %s step of %s and ended its wait on the ancestor's request. %s",
			actorOr(fact.Actor, "A person"), fact.Version, own, step, root, ended)
	case intake.DispatchArtifactChangesRequested:
		return changesAct(fact.Actor, fact.Version, own+", the root's own document", fact.Reason) + " " + ended + " " + stepClosed(root, step)
	}
	return fmt.Sprintf("Version %d of %s, the root's own document, is newer than version %d, which the step was registered at. %s %s",
		fact.Version, own, gate.LatestVersion, ended, stepClosed(root, step))
}

// basisArtifact applies an event of an ancestor's document to the steps of other roots that name
// that document as their basis (Store.GatesForBasisArtifact), whatever tree the ancestor is in or
// none, which is why dispatchArtifact runs it before and apart from the ancestor's own gates. A
// step that waits on the ancestor's request opens or ends on it (pendingBasisArtifact). A step
// that stands open on the ancestor's approval changes on nothing done to that document (R3): it
// passes on a person's changes request on the version it rests on, made after that approval
// (tellBasisChanges), and stays open. A row of a step the deployment does not gate holds nothing
// and takes no event, as artifactIgnored rules for a root's own document.
func (e *Engine) basisArtifact(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) error {
	gates, err := e.store.GatesForBasisArtifact(ctx, tx, fact.ArtifactID)
	if err != nil {
		return err
	}
	for _, gate := range gates {
		if !e.gatesStep(gate.Step) {
			continue
		}
		root, err := e.store.Issue(ctx, tx, gate.Issue)
		if err != nil {
			return err
		}
		if root == nil {
			continue
		}
		switch {
		case gate.PendingOnBasis():
			err = e.pendingBasisArtifact(ctx, tx, *root, gate, fact)
		case basisChangesRequest(gate, fact.Kind == intake.DispatchArtifactChangesRequested, fact.Version, fact.Actor, fact.Mark):
			err = e.tellBasisChanges(ctx, tx, *root, gate, intake.BasisReview{Mark: fact.Mark, Actor: fact.Actor, Reason: fact.Reason})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// basisChangesRequest says whether a review of an ancestor's document is the one act on it that
// gate, a step standing open on that document, passes on (R3): a changes request (changes) by a
// person on the version the basis names, which Dispatch recorded after the approval the step
// opened on. Both marks are places in the ancestor's own event log, so they compare.
func basisChangesRequest(gate record.DesignGate, changes bool, version int, actor dispatch.Actor, mark record.EventMark) bool {
	return gate.OnBasis() && changes && version == gate.Basis.Version && actor.Person() && mark.After(gate.Basis.Approval)
}

// pendingBasisArtifact applies fact, an event of the ancestor's document, to gate, a step of root
// that waits, closed, on the ancestor's request for a person's approval of the basis version. An
// event of an older version, and a version event of that version, change nothing. The person's
// approval of that version opens the step (openPendingBasis). A changes request on it ends the
// wait, and so does any event of a newer version, whoever wrote it, and an approval of that version
// by an actor Dispatch names and who is no person. An approval that names nobody, which only a
// read of a Dispatch that records no reviewer gives, decides nothing: the person's answer does.
func (e *Engine) pendingBasisArtifact(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, fact intake.DispatchArtifact) error {
	basis := *gate.Basis
	step := gate.Step.OrSpec()
	document := documentOf(basis.Issue, step)
	switch {
	case fact.Version < basis.Version, fact.Version == basis.Version && fact.Kind == intake.DispatchArtifactVersion:
		return nil
	case fact.Version > basis.Version:
		return e.endPendingBasis(ctx, tx, root, gate, fmt.Sprintf("Version %d of %s is newer than version %d, which the request asks a person to approve.",
			fact.Version, document, basis.Version), fact.Actor.Name())
	case fact.Kind == intake.DispatchArtifactChangesRequested:
		return e.endPendingBasis(ctx, tx, root, gate, changesAct(fact.Actor, basis.Version, document, fact.Reason), fact.Actor.Name())
	case fact.Actor.Person():
		_, err := e.openPendingBasis(ctx, tx, root, gate, fact.Mark, fact.Actor.Name())
		return err
	case fact.Actor == (dispatch.Actor{}):
		e.logOnCommit(ctx, "workflow: an approval of the ancestor's version that names nobody opens and ends no step waiting on it", "issue", root.Key,
			"step", step, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask)
		return nil
	}
	return e.endPendingBasis(ctx, tx, root, gate, fmt.Sprintf("%s approved version %d of %s, and %s is no person.",
		fact.Actor.Name(), basis.Version, document, fact.Actor.Name()), fact.Actor.Name())
}

// openPendingBasis opens gate, a step of root that waited on the ancestor's request, on the
// person's answer to it: their approval of the version, at approval in the ancestor's event log
// and by approver (record.OpenPendingBasis). It does what a registration on that approval would
// (gateRegistered): the tree starts as after a person's approval at the version registered, the
// gate seed is queued, the root's issue gets the message that records the basis and the
// ancestor's architect the root's first rest on it (tellRegisteredBasis), and the root's
// architect the design-approved notice that names the basis. A root waiting on a person keeps
// that wait beside its step's, so the opening answers it as an approval does, wherever the open
// steps answer it (answerOpenGates), with no notice beside it: its resume says how the step
// stands. It returns the step as it opened.
func (e *Engine) openPendingBasis(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, approval record.EventMark, approver string) (record.DesignGate, error) {
	opened := record.OpenPendingBasis(gate, approval, approver)
	if !opened.OnBasis() {
		return opened, nil
	}
	step := opened.Step.OrSpec()
	before, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return opened, err
	}
	if err := e.putGateTold(ctx, tx, opened, opened.LatestVersion); err != nil {
		return opened, err
	}
	if err := record.PublishChangedGates(ctx, e.store, tx, root.Key, before, e.now()); err != nil {
		return opened, err
	}
	e.logOnCommit(ctx, "workflow: a person's approval of the ancestor's version opened a step that waited on it", "issue", root.Key, "step", step,
		"version", opened.LatestVersion, "ancestor", opened.Basis.Issue, "basis", opened.Basis.ArtifactID, "basis_version", opened.Basis.Version,
		"ask", gate.Basis.Pending.Ask)
	if err := e.enqueue(ctx, tx, root.Key, record.GateSeed{ArtifactID: opened.ArtifactID, Step: step, Version: opened.LatestVersion, Generation: root.Generation}); err != nil {
		return opened, err
	}
	if err := e.tellRegisteredBasis(ctx, tx, root, &gate, opened, false); err != nil {
		return opened, err
	}
	current, err := e.store.Issue(ctx, tx, root.Key)
	if err != nil || current == nil {
		return opened, err
	}
	if current.AwaitingPerson() {
		e.logOnCommit(ctx, "workflow: a root waiting on a person is told its step's opening on the ancestor's approval by its resume", "issue", root.Key)
		_, err := e.answerOpenGates(ctx, tx, current.Project, []record.Issue{*current}, nil)
		return opened, err
	}
	return opened, e.notice(ctx, tx, root.Key, openedNotice(root.Key, opened, ""))
}

// endPendingBasis ends gate's wait on the ancestor's request without opening it, act being what
// ended it and actor who made it ("" for nobody): the step stays closed and registered on the
// root's own document with no basis (record.EndPendingBasis), and the end is told
// (tellPendingEnded).
func (e *Engine) endPendingBasis(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, act, actor string) error {
	step := gate.Step.OrSpec()
	basis := *gate.Basis
	ended := record.EndPendingBasis(gate)
	before, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	if err := e.putGateTold(ctx, tx, ended, ended.LatestVersion); err != nil {
		return err
	}
	if err := record.PublishChangedGates(ctx, e.store, tx, root.Key, before, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a step's wait on an ancestor's approval request ended", "issue", root.Key, "step", step,
		"ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask, "act", act)
	return e.tellPendingEnded(ctx, tx, root.Key, step, basis, act+" "+waitEnded(root.Key, step, basis)+" "+stepClosed(root.Key, step), actor, false)
}

// tellPendingEnded tells the end of root key's step's wait on basis's request: what says what
// ended it, naming the act, the ancestor's document and its version. The ancestor's architect is
// told as a resting-root notice (queueRestingRoot), once per request. A root waiting on a person
// keeps that wait beside its step's, so the end answers it, as a changes request does, with no
// notice beside it: the answer's detail says what ended the step's wait (basisEndedAnswer). An act
// on the root's own document (own) that meets a wait already answered is named by the resume as
// well: what is joined to the answer's detail (basisEndedAfter), and the resume names it after the
// act that answered the wait. Every other end reaches the root's architect as a basis-ended notice
// that names the act, the ancestor's document and the version, and who made the act (actor, "" for
// nobody).
func (e *Engine) tellPendingEnded(ctx context.Context, tx pgx.Tx, key string, step record.DesignStep, basis record.GateBasis, what, actor string, own bool) error {
	root, err := e.store.Issue(ctx, tx, key)
	if err != nil || root == nil {
		return err
	}
	if err := e.queueRestingRoot(ctx, tx, *root, step, basis, restingEnded(basis.Pending.Ask), what); err != nil {
		return err
	}
	switch {
	case root.AwaitingPerson() && root.Awaiting.AnsweredAt == nil:
		e.logOnCommit(ctx, "workflow: the end of a step's wait on an ancestor's request answers the root's wait on a person", "issue", key,
			"step", step, "ask", basis.Pending.Ask)
		return e.answerWait(ctx, tx, *root, record.AnsweredByChanges, basisEndedAnswer+what)
	case root.AwaitingPerson() && own:
		e.logOnCommit(ctx, "workflow: a root waiting on a person is told by its resume of the act on its own document that ended its step's wait", "issue", key,
			"step", step, "ask", basis.Pending.Ask)
		root.Awaiting.AnsweredDetail += basisEndedAfter + what
		return e.store.PutIssue(ctx, tx, *root)
	}
	return e.notice(ctx, tx, key, record.Notice{Kind: "basis-ended", Role: claim.RoleArchitect, Version: basis.Version, Actor: actor,
		Ref: stepRef(basis.Issue, step), Reason: what})
}

// tellBasisChanges passes on review, a person's changes request on the version of the ancestor's
// document that gate, a step of root, stands open on, made after the approval the step opened on
// (R3): basis-changes-requested to the root's architect, naming the ancestor's document, the
// version, the person and their reason, and, where the daemon records the ancestor in a live tree
// (ancestorInLiveTree), the same keyed by the ancestor, so it reaches the architect that owns the
// ancestor (the outbox's owningArchitect), naming the root and the requirements it serves. The step
// stays open. Each review is told once, whatever carries it (the live event, a seed's read) and in
// whatever order: the told-once mark is keyed by the review's place in the ancestor's event log
// (reviewPlace), as tellChanges marks a changes request on the root's own document, so a second
// changes request on the same version is told as well.
func (e *Engine) tellBasisChanges(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, review intake.BasisReview) error {
	basis := *gate.Basis
	step := gate.Step.OrSpec()
	fresh, err := e.store.MarkProcessed(ctx, tx, "basis-changes-told",
		fmt.Sprintf("%s:%d:%s:%s:%d:%s", root.Key, root.Generation, step, basis.ArtifactID, basis.Version, reviewPlace(review.Mark)))
	if err != nil || !fresh {
		if err == nil {
			e.logOnCommit(ctx, "workflow: a changes request on the ancestor's version a step stands open on was already told", "issue", root.Key,
				"step", step, "basis", basis.ArtifactID, "basis_version", basis.Version)
		}
		return err
	}
	asked := fmt.Sprintf("%s requested changes on version %d of %s, after the approval of that version that the %s step of %s stands open on.",
		actorOr(review.Actor, "A person"), basis.Version, documentOf(basis.Issue, step), step, root.Key)
	reason := "The daemon's read of Dispatch gave no reason; read the review on the document."
	if review.Reason != "" {
		reason = fmt.Sprintf("The person's reason: %q", review.Reason)
	}
	e.logOnCommit(ctx, "workflow: a person requested changes on the ancestor's version a step stands open on; the step stays open", "issue", root.Key,
		"step", step, "ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "review_seq", review.Mark.Seq)
	told := record.Notice{Kind: "basis-changes-requested", Role: claim.RoleArchitect, Version: basis.Version, Actor: review.Actor.Name(),
		Ref: stepRef(basis.Issue, step), Reason: asked + " The step stays open. " + reason}
	if err := e.notice(ctx, tx, root.Key, told); err != nil {
		return err
	}
	live, err := e.ancestorInLiveTree(ctx, tx, basis.Issue)
	if err != nil || !live {
		if err == nil {
			e.logOnCommit(ctx, "workflow: the daemon records the ancestor in no live tree, so its architect is not told the changes request", "issue", root.Key,
				"ancestor", basis.Issue)
		}
		return err
	}
	told.Reason = asked + " The step of " + root.Key + " stays open. " + reason + " " + servesSentence(root.Key, basis.Issue, basis.Serves)
	return e.notice(ctx, tx, basis.Issue, told)
}

// endPendingOnRequest ends every step that waits, closed, on fact's ask as an ancestor's approval
// request (Store.GatesByPendingAsk, since the root-only ask mirror records no ancestor's ask),
// when the close carries no approval of the version the request names, whoever closed it. A
// person's answer of an approval request is a review: Dispatch writes it in the answer's
// transaction and appends its artifact.* event after the ask's, so that event approves the
// version, which opens the step, or asks for changes on it, which ends the wait (basisArtifact),
// and the answer itself ends nothing. Every other close (a resolution by the person, the
// architect, another session or Dispatch's settlement) carries no approval.
func (e *Engine) endPendingOnRequest(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) error {
	if fact.State == "answered" {
		return nil
	}
	gates, err := e.store.GatesByPendingAsk(ctx, tx, fact.Ask)
	if err != nil {
		return err
	}
	for _, gate := range gates {
		if !gate.PendingOnBasis() || gate.Basis.Pending.Ask != fact.Ask {
			continue
		}
		root, err := e.store.Issue(ctx, tx, gate.Issue)
		if err != nil {
			return err
		}
		if root == nil {
			continue
		}
		act := fmt.Sprintf("%s closed approval request %s of version %d of %s, as %s, with no approval of that version.",
			actorOr(fact.Actor, "Someone Dispatch does not name"), fact.Ask, gate.Basis.Version, documentOf(gate.Basis.Issue, gate.Step.OrSpec()), fact.State)
		if err := e.endPendingBasis(ctx, tx, *root, gate, act, fact.Actor.Name()); err != nil {
			return err
		}
	}
	return nil
}

// endPendingOnAncestorIssue ends every step that waits, closed, on a request of fact's issue as an
// ancestor (pendingOn) when the event shows that issue leaving R1's first condition: Dispatch shows
// it done or retro, or it no longer carries the legion label. recorded is the issue's record, nil
// for none: the ancestor may be in no tree the daemon records. An event older than one the daemon
// has applied of the issue (its record's last sequence, or the mirror's, which every applied event
// of the issue writes first) says nothing of the issue now, and ends nothing. Most events that
// reach here are of issues nothing waits on, so the steps are read before anything else is.
func (e *Engine) endPendingOnAncestorIssue(ctx context.Context, tx pgx.Tx, recorded *record.Issue, fact intake.DispatchIssue) error {
	var act string
	switch {
	case fact.Status == "done", fact.Status == "retro":
		act = fmt.Sprintf("Dispatch shows %s %s, so %s is accepted.", fact.Key, fact.Status, fact.Key)
	case !fact.HandedOver:
		act = fmt.Sprintf("%s no longer carries the %s label.", fact.Key, dispatch.LegionLabel)
	default:
		return nil
	}
	pending, err := e.pendingOn(ctx, tx, fact.Key)
	if err != nil || len(pending) == 0 {
		return err
	}
	if fact.Seq > 0 {
		if recorded != nil && fact.Seq <= recorded.LastDispatchSeq {
			return nil
		}
		mirrored, err := e.store.DispatchIssuesOf(ctx, tx, e.cfg.Project, []string{fact.Key})
		if err != nil {
			return err
		}
		for _, row := range mirrored {
			if row.Key == fact.Key && row.Seq > fact.Seq {
				return nil
			}
		}
	}
	return e.endEachPending(ctx, tx, pending, act)
}

// endPendingOn ends every step of another root that waits, closed, on a request of ancestor
// (pendingOn), act being what ended it. A step standing open on ancestor's approval stays open
// (R3).
func (e *Engine) endPendingOn(ctx context.Context, tx pgx.Tx, ancestor, act string) error {
	pending, err := e.pendingOn(ctx, tx, ancestor)
	if err != nil {
		return err
	}
	return e.endEachPending(ctx, tx, pending, act)
}

// pendingOn is every step of another root, of every tree, that waits, closed, on a request of
// ancestor (Store.GatesRestingOn, with the steps that rest on it alone left out).
func (e *Engine) pendingOn(ctx context.Context, tx pgx.Tx, ancestor string) ([]record.DesignGate, error) {
	gates, err := e.store.GatesRestingOn(ctx, tx, ancestor)
	if err != nil {
		return nil, err
	}
	var pending []record.DesignGate
	for _, gate := range gates {
		if gate.PendingOnBasis() && gate.Basis.Issue == ancestor {
			pending = append(pending, gate)
		}
	}
	return pending, nil
}

// endEachPending ends the wait of each of gates (endPendingBasis), act being what ended it.
func (e *Engine) endEachPending(ctx context.Context, tx pgx.Tx, gates []record.DesignGate, act string) error {
	for _, gate := range gates {
		root, err := e.store.Issue(ctx, tx, gate.Issue)
		if err != nil {
			return err
		}
		if root == nil {
			continue
		}
		if err := e.endPendingBasis(ctx, tx, *root, gate, act, ""); err != nil {
			return err
		}
	}
	return nil
}

// pendingBasisRead applies the pending-basis seed's read of the ancestor's request a step waits
// on (intake.PendingBasisRead) as the events it read would have applied, in their order: the
// person's approval of the version opens the step (openPendingBasis), and a changes request on
// that version made after it is then passed on as on any step open on that approval
// (tellBasisChanges); with no such approval, a changes request on the version, a newer version of
// the document, or the request's close ends the wait (endPendingBasis). A request still waiting
// changes nothing. Neither does a read of a root of another generation, or of a step that no
// longer waits on that request at that version: an event applied first opened or ended it, or a
// registration since replaced it.
func (e *Engine) pendingBasisRead(ctx context.Context, tx pgx.Tx, fact intake.PendingBasisRead) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil || root.Generation != fact.Generation {
		return intake.Result{}, err
	}
	gates, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return intake.Result{}, err
	}
	step := fact.Step.OrSpec()
	gate, ok := gates[step]
	if !ok || !e.gatesStep(step) || !gate.PendingOnBasis() || gate.ArtifactID != fact.ArtifactID || gate.Basis.ArtifactID != fact.BasisArtifactID ||
		gate.Basis.Version != fact.BasisVersion || gate.Basis.Pending.Ask != fact.Ask {
		e.logOnCommit(ctx, "workflow: the pending-basis seed read a wait that has opened or ended since, and changes nothing", "issue", root.Key,
			"step", step, "basis", fact.BasisArtifactID, "basis_version", fact.BasisVersion, "ask", fact.Ask)
		return intake.Result{}, nil
	}
	document := documentOf(gate.Basis.Issue, step)
	switch {
	case fact.Approval != nil:
		opened, err := e.openPendingBasis(ctx, tx, *root, gate, fact.Approval.Mark, fact.Approval.Actor.Name())
		if err != nil || fact.Changes == nil || !basisChangesRequest(opened, true, fact.BasisVersion, fact.Changes.Actor, fact.Changes.Mark) {
			return intake.Result{}, err
		}
		return intake.Result{}, e.tellBasisChanges(ctx, tx, *root, opened, *fact.Changes)
	case fact.Changes != nil:
		return intake.Result{}, e.endPendingBasis(ctx, tx, *root, gate, changesAct(fact.Changes.Actor, fact.BasisVersion, document, fact.Changes.Reason),
			fact.Changes.Actor.Name())
	case fact.LatestVersion > fact.BasisVersion:
		return intake.Result{}, e.endPendingBasis(ctx, tx, *root, gate, fmt.Sprintf("Version %d of %s is newer than version %d, which the request asks a person to approve.",
			fact.LatestVersion, document, fact.BasisVersion), "")
	case !fact.RequestOpen:
		return intake.Result{}, e.endPendingBasis(ctx, tx, *root, gate, fmt.Sprintf("Dispatch shows approval request %s of version %d of %s closed with no approval of that version.",
			fact.Ask, fact.BasisVersion, document), "")
	}
	e.logOnCommit(ctx, "workflow: the pending-basis seed read the ancestor's request still waiting on the person", "issue", root.Key,
		"step", step, "basis", fact.BasisArtifactID, "basis_version", fact.BasisVersion, "ask", fact.Ask)
	return intake.Result{}, nil
}

// basisChangesRead applies the gate seed's read of a person's changes request on the ancestor's
// version a step stands open on (intake.BasisChangesRead): it is passed on as its live event
// would pass it on (tellBasisChanges, once per review), and only for a root of that generation
// whose step still stands open on that same basis and whose approval Dispatch recorded before the
// changes request.
func (e *Engine) basisChangesRead(ctx context.Context, tx pgx.Tx, fact intake.BasisChangesRead) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil || root.Generation != fact.Generation {
		return intake.Result{}, err
	}
	gates, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return intake.Result{}, err
	}
	step := fact.Step.OrSpec()
	gate, ok := gates[step]
	if !ok || !e.gatesStep(step) || gate.Basis == nil || gate.Basis.ArtifactID != fact.BasisArtifactID ||
		!basisChangesRequest(gate, true, fact.BasisVersion, fact.Changes.Actor, fact.Changes.Mark) {
		e.logOnCommit(ctx, "workflow: the gate seed read a changes request on an ancestor's version no open step of this root rests on", "issue", root.Key,
			"step", step, "basis", fact.BasisArtifactID, "basis_version", fact.BasisVersion)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.tellBasisChanges(ctx, tx, *root, gate, fact.Changes)
}
