package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// opens), or nil for a registration with none. A registration of a step that already stands on
// this same basis at this version (stored, the step's row of this document, nil for none),
// standing open or pending on the same request, is the same registration made again, and keeps
// the first one's time: the moment the step opened, or opens once the person answers, does not
// move when the architect registers it again (R2). A step the deployment does not gate asks
// nobody and opens at its registration, as every registration did under `gates.design: off`, so
// it takes no basis either. The value is a copy: the fact is the API's.
func (e *Engine) registeredBasis(fact intake.GateRegistered, ungated bool, stored *record.DesignGate, version int) *record.GateBasis {
	if fact.Basis == nil || ungated {
		return nil
	}
	basis := *fact.Basis
	basis.RegisteredAt = e.now()
	if stored != nil && stored.LatestVersion == version && standsOn(*stored, basis) {
		basis.RegisteredAt = stored.Basis.RegisteredAt
	}
	return &basis
}

// standsOn says whether gate already stands on basis as it would register: open on the same
// document of the same ancestor at the same version, or pending on that version through the same
// request.
func standsOn(gate record.DesignGate, basis record.GateBasis) bool {
	switch {
	case !sameBasis(gate.Basis, &basis):
		return false
	case basis.Pending == nil:
		return gate.OnBasis()
	}
	return gate.PendingOnBasis() && gate.Basis.Pending.Ask == basis.Pending.Ask
}

// acceptedBasis refuses fact, a registration on a basis, when the daemon's own store shows its
// ancestor accepted now (record.BasisAcceptance), as register_gate refuses an accepted ancestor
// (BASIS_ACCEPTED, R1's first condition), and answers that refusal, or a result that refuses
// nothing. register_gate read the ancestor outside any transaction, and an ancestor's sign-off,
// or its close_root, can commit between that read and this one. Each applies as a fact, under the
// one lock every fact takes (intake.SerializeFacts), so the two apply in one order: a sign-off or
// close applied first is read here, the close by the close it marked on the ancestor's record
// (record.PendingClose) before Dispatch's event of it comes back, and refuses the registration;
// one applied after reads the row the registration wrote among the roots that rest on the
// ancestor (record.RestingRoots) and is refused itself (SIGNOFF_RESTING_ROOT_IN_WORKFLOW). The
// refusal writes nothing and leaves the fact unspent (intake.Result.Unspent): its cause can clear,
// as a move of the ancestor back out of its retro clears it, and a call made again is then judged
// afresh rather than answered as one already applied. A step the deployment does not gate takes
// no basis (registeredBasis), so it refuses nothing, and neither does a registration on a root out
// of the workflow, whose basis gateRegistered drops before it asks.
func (e *Engine) acceptedBasis(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	if fact.Basis == nil || !e.gatesStep(fact.Step.OrSpec()) {
		return intake.Result{}, nil
	}
	ancestor := fact.Basis.Issue
	why, _, err := record.BasisAcceptance(ctx, e.store, tx, ancestor)
	if err != nil || why == "" {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a registration on a basis whose ancestor was accepted since register_gate read it records nothing", "issue", fact.Issue,
		"step", fact.Step.OrSpec(), "ancestor", ancestor, "accepted", why)
	result := refused("BASIS_ACCEPTED", fmt.Sprintf("%s, so %s has been accepted since this registration read it; %s", why, ancestor, record.BasisNotYetAccepted))
	result.Unspent = true
	return result, nil
}

// ancestorLeft is the act by which ancestor left R1's first condition, as the end of a pending
// step's wait or of a basis standing open names it, or "" where it has not: why, what the daemon's
// own store shows (record.BasisAcceptance), comes first, then the ancestor's Dispatch status, ""
// for none read, and whether it carries the legion label (labelled). The pending-basis seed's read
// of Dispatch gives the status and the label (judgeBasisRead), an event of the ancestor's issue
// gives its own to say whether it can be a cue at all (basisCueEvent) and to name the end of the
// bases standing open on it (basisIssue), and the daemon's own move of the ancestor gives why
// alone (movedAccepted).
func ancestorLeft(ancestor, why, status string, labelled bool) string {
	switch {
	case why != "":
		return fmt.Sprintf("%s, so %s is accepted.", why, ancestor)
	case record.AcceptedStatus(status):
		return fmt.Sprintf("Dispatch shows %s %s, so %s is accepted.", ancestor, status, ancestor)
	case !labelled:
		return fmt.Sprintf("%s no longer carries the %s label.", ancestor, dispatch.LegionLabel)
	}
	return ""
}

// movedAccepted is the act by which the daemon's own move of key into to, retro or merging,
// accepted it (record.Accepted), as the end of a step's wait on key's approval request names it
// (ancestorLeft): "The daemon moved LEGION-100 into retro, so LEGION-100 is accepted." The pass of
// a step into either phase (transition) and the release of a parent held before its retro
// (StartHeldRetro) end such waits with it.
func movedAccepted(key string, to phase.Phase) string {
	return ancestorLeft(key, fmt.Sprintf("The daemon moved %s into %s", key, to), "", true)
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
	return e.WriteIssue(ctx, tx, *root)
}

// sameBasis says whether a and b rest a step on one ancestor's approval: the same document of the
// same ancestor at the same version, or no basis at all.
func sameBasis(a, b *record.GateBasis) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Issue == b.Issue && a.ArtifactID == b.ArtifactID && a.Version == b.Version
}

// ancestorOpenedStep is the opening of each text that tells a step an ancestor's approval opened, a
// format of the step and the root: the design-approved notice's reason (openedNotice) and the
// resting-root notice of the opening (tellRegisteredBasis).
const ancestorOpenedStep = "An ancestor's approval opened the %s step of %s."

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
		Reason: fmt.Sprintf(ancestorOpenedStep+" A person approved version %d of the %s of %s. "+
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
// A root out of the workflow takes no basis (gateRegistered), and its leave told each ancestor's
// architect that it left, so the end of such a wait on it is told to nobody, as at the leave
// (endPendingUntold), and only logged.
func (e *Engine) tellRegisteredBasis(ctx context.Context, tx pgx.Tx, root record.Issue, before *record.DesignGate, gate record.DesignGate, stood bool) error {
	step := gate.Step.OrSpec()
	if before != nil && before.PendingOnBasis() && (gate.Basis == nil || gate.Basis.Issue != before.Basis.Issue) {
		ended := *before.Basis
		if record.Stopped(root) {
			e.logEndedUntold(ctx, root.Key, step, ended)
		} else {
			what := fmt.Sprintf("%s registered its %s step again with no basis on %s. The step no longer waits on approval request %s of version %d of the %s of %s.",
				root.Key, step, ended.Issue, ended.Pending.Ask, ended.Version, step.Document(), ended.Issue)
			if err := e.queueRestingRoot(ctx, tx, root, step, ended, restingEnded(ended.Pending.Ask), what); err != nil {
				return err
			}
		}
	}
	switch {
	case gate.PendingOnBasis():
		basis := *gate.Basis
		if err := e.enqueue(ctx, tx, root.Key, record.PendingBasisSeedOf(gate, root.Generation)); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a design gate step waits on an ancestor's approval request", "issue", root.Key, "step", step,
			"version", gate.LatestVersion, "ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask)
		what := fmt.Sprintf("%s registered its %s step on version %d of %s. The step stays closed until a person answers approval request %s of that version. %s rests on %s while the step waits.",
			root.Key, step, basis.Version, documentOf(basis.Issue, step), basis.Pending.Ask, root.Key, basis.Issue)
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
		what := fmt.Sprintf(ancestorOpenedStep+" A person approved version %d of %s. %s rests on this approval.",
			step, root.Key, basis.Version, documentOf(basis.Issue, step), root.Key)
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
		line("%s", servesSentence(root, basis.Issue, nil))
	} else {
		line("%s:", servesOpening(root, basis.Issue))
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

// endsPendingWait says whether event is the end of a pending wait (restingEnded).
func (e restingRootEvent) endsPendingWait() bool { return strings.HasPrefix(string(e), "ended:") }

// restingClosed is a resting root's close as done, and restingLeft its leave of the workflow any
// other way: a move to backlog, icebox or triage, or admission's leave of a root it found with
// neither a slot nor a place in the line (tellRestingLeft). Either ends the wait of the ancestor's
// sign-off and close for that root (record.RestingRoots).
const (
	restingClosed restingRootEvent = "closed"
	restingLeft   restingRootEvent = "left"
)

// restingBasisEnded is the end of a basis standing open on the ancestor's approval once the
// ancestor is done or accepted while the root is in Legion's work (endOpenOn): the root rests on
// the ancestor no more.
const restingBasisEnded restingRootEvent = "basis-ended"

// queueRestingRoot tells the architect that owns the ancestor basis names that root's step rests
// on, or waits on, that ancestor's approval: a resting-root notice keyed by the ancestor's issue,
// so it reaches that architect as any notice keyed by an issue does (the outbox's
// owningArchitect). It names the ancestor's document for the step as its reference and the
// version the step rests or waits on, and its reason is what (what happened, naming the root and
// its step) and the ancestor's requirements the root's checks serve (basis.Serves). It is the one
// way the notice is queued: a registration (tellRegisteredBasis), the end of a pending wait, and a
// resting root's close or its leaving the workflow each call it with the event it tells.
//
// Where no architect of the ancestor is in Legion's work on its Dispatch chain (ancestorReached)
// it queues nothing and records nothing as told: no architect of the ancestor would hear it, and
// the ancestor's start task at its admission names the roots that rest on it. Otherwise each
// event is told once (restingRootEvent), marked in the notice's own transaction. The end of a
// pending wait is the one event told wherever the daemon records the ancestor on a known chain
// (ancestorRecorded), whether or not it is still in Legion's work: the pending-basis seed that
// ends a wait on a closed ancestor runs after that ancestor's own leave, and the end is the last
// the ancestor's architect hears of the root. It goes by the owner walk as any notice keyed by an
// issue does, so it reaches an architect that still holds its role, and finishes undelivered
// where none does: nothing wakes or starts an architect that has ended for it.
func (e *Engine) queueRestingRoot(ctx context.Context, tx pgx.Tx, root record.Issue, step record.DesignStep, basis record.GateBasis, event restingRootEvent, what string) error {
	reached, err := e.restingRootHeard(ctx, tx, basis.Issue, event)
	if err != nil {
		return err
	}
	if !reached {
		e.logOnCommit(ctx, "workflow: no architect of the ancestor is in Legion's work on its Dispatch chain, so no resting-root notice is queued", "issue", root.Key,
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
	return servesOpening(root, ancestor) + ": " + strings.Join(serves, ", ") + "."
}

// servesOpening is the opening of each text that names the ancestor's requirements the checks of
// root serve: servesSentence's, and the basis-opened message's line above the list of them
// (basisOpenedMessage).
func servesOpening(root, ancestor string) string {
	return fmt.Sprintf("The checks of %s serve these requirements of %s", root, ancestor)
}

// ancestorReached says whether a notice keyed by key reaches an architect: the daemon records key,
// and its Dispatch chain has a gate root at or above it in Legion's work (record.Chains.GateRoot),
// where the owner walk of a notice on key ends (R18 of dispatch://INFRA-3320). An unknown chain
// has none, and the notice is not queued.
func (e *Engine) ancestorReached(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	ancestor, err := e.store.Issue(ctx, tx, key)
	if err != nil || ancestor == nil {
		return false, err
	}
	chains, err := record.ReadChainsOf(ctx, tx, e.store, ancestor.Project, []string{key})
	if err != nil {
		return false, err
	}
	return chains.GateRoot(key) != "", nil
}

// restingRootHeard says whether a resting-root notice of event, keyed by the ancestor, is queued:
// ancestorRecorded for the end of a pending wait, ancestorReached for every other event.
func (e *Engine) restingRootHeard(ctx context.Context, tx pgx.Tx, ancestor string, event restingRootEvent) (bool, error) {
	if event.endsPendingWait() {
		return e.ancestorRecorded(ctx, tx, ancestor)
	}
	return e.ancestorReached(ctx, tx, ancestor)
}

// ancestorRecorded says whether the daemon records key and its Dispatch chain is known: the owner
// walk of a notice keyed by it then names an architect, which the outbox delivers to or, where
// that architect has ended, finishes the notice without.
func (e *Engine) ancestorRecorded(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	ancestor, err := e.store.Issue(ctx, tx, key)
	if err != nil || ancestor == nil {
		return false, err
	}
	chains, err := record.ReadChainsOf(ctx, tx, e.store, ancestor.Project, []string{key})
	if err != nil {
		return false, err
	}
	return chains.Chain(key).Known(), nil
}

// leaveBases is what root's leave of the workflow for status (leave) does to its steps that rest
// or wait on an ancestor's approval (R8): the architect of each such ancestor is told that root
// left (tellRestingLeft), and then each step that waits, closed, on an ancestor's open approval
// request ends that wait with nobody told (endPendingUntold). The leave is the last that
// architect hears of root in this generation. A wait left standing would open the step on the
// person's approval of the ancestor's version, by the pending-basis seed's read of it, which its
// live event queues and the boot queues for every step still waiting: the ancestor's architect
// would hear that root rests on it again, root's issue would get the message of the basis, and
// root's architect design-approved, though a re-admission keeps nothing of the basis (R3). Any
// other act that ends a wait would tell both architects of a step nobody runs, but for the check
// each end makes of the root (openPendingBasis, tellPendingEnded), which ends a wait a release
// that ended none at the leave left standing on such a root with nobody told. A row of a step the
// deployment does not gate holds nothing and takes no event, as basisArtifact rules, so it is left
// as it is. The root's steps are read once for both.
func (e *Engine) leaveBases(ctx context.Context, tx pgx.Tx, root record.Issue, status string) error {
	gates, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil || len(gates) == 0 {
		return err
	}
	if err := e.tellRestingLeft(ctx, tx, root, gates, status); err != nil {
		return err
	}
	for _, step := range record.DesignSteps {
		gate, ok := gates[step]
		if !ok || !e.gatesStep(step) || !gate.PendingOnBasis() {
			continue
		}
		if _, err := e.endPendingUntold(ctx, tx, root, gate); err != nil {
			return err
		}
	}
	return nil
}

// tellRestingLeft tells the architect of each ancestor that root rests on (R8) that root has left
// the workflow for status (leaveBases): closed as done (restingClosed), or out of the workflow any
// other way (restingLeft). gates is root's steps as the leave read them. The ancestor's sign-off
// and close wait for a resting root while it is in the workflow (record.RestingRoots), so this
// tells the end of that wait. A step rests on each ancestor a basis opened it on (RestsOn) and on
// the ancestor its basis names now, standing or pending (record.DesignGate.RestsOnIssue), so one
// notice goes to each such ancestor, once per root, generation, ancestor and event
// (queueRestingRoot), naming each step that rests there and the requirements the root's checks
// serve, each once. The document and the version the notice names are one step's: the ancestor's
// document for the first step whose basis names the ancestor now, and the version that basis
// names. A step whose basis a new version of the root's own document ended, or a later
// registration moved to another ancestor, keeps only the rest, so where no step's basis names the
// ancestor the notice names the ancestor's document for the first step that rests there, no
// version and no requirement. Each ancestor is named to the fact's settle too, which starts the
// retro of one held before it that nothing holds any longer (restEnded).
func (e *Engine) tellRestingLeft(ctx context.Context, tx pgx.Tx, root record.Issue, gates map[record.DesignStep]record.DesignGate, status string) error {
	type rest struct {
		steps []record.DesignStep
		// step is the step whose document of the ancestor the notice names, and basis.Version
		// is the version that step's basis names, if any.
		step  record.DesignStep
		basis record.GateBasis
	}
	rests := map[string]*rest{}
	var ancestors []string
	for _, step := range record.DesignSteps {
		gate, ok := gates[step]
		if !ok {
			continue
		}
		named := slices.Clone(gate.RestsOn)
		if gate.Basis != nil {
			named = append(named, gate.Basis.Issue)
		}
		for _, ancestor := range named {
			if !gate.RestsOnIssue(ancestor) {
				continue
			}
			at, ok := rests[ancestor]
			if !ok {
				at = &rest{step: step, basis: record.GateBasis{Issue: ancestor}}
				rests[ancestor] = at
				ancestors = append(ancestors, ancestor)
			}
			if slices.Contains(at.steps, step) {
				continue
			}
			at.steps = append(at.steps, step)
			if gate.Basis != nil && gate.Basis.Issue == ancestor {
				if at.basis.Version == 0 {
					at.step, at.basis.Version = step, gate.Basis.Version
				}
				for _, served := range gate.Basis.Serves {
					if !slices.Contains(at.basis.Serves, served) {
						at.basis.Serves = append(at.basis.Serves, served)
					}
				}
			}
		}
	}
	for _, ancestor := range ancestors {
		e.restEnded(ctx, ancestor)
		at := rests[ancestor]
		event, what := restingLeft, fmt.Sprintf("%s left the workflow: its status is %s.", root.Key, status)
		if status == "done" {
			event, what = restingClosed, fmt.Sprintf("%s is done.", root.Key)
		}
		verb := "rests"
		if len(at.steps) > 1 {
			verb = "rest"
		}
		what += fmt.Sprintf(" Its %s %s on the approval of %s. The sign-off and the close of %s, and its retro where its task has no acceptance step, no longer wait for %s.",
			stepsPhrase(at.steps), verb, ancestor, ancestor, root.Key)
		if err := e.queueRestingRoot(ctx, tx, root, at.step, at.basis, event, what); err != nil {
			return err
		}
	}
	return nil
}

// restingRootsTask is what the architect of ancestor is told of the roots that rest on its
// approval and are still in the workflow (record.RestingRoots), "" for none: each one, and that
// the daemon refuses the ancestor's sign-off and close, and holds its retro where its task has no
// acceptance step (workflow's retroHeld), while any of them is in the workflow, since the checks
// of the issues under the ancestor prove its requirements together (R8). Its one reader is
// ArchitectStartOf (ArchitectStart.Resting).
func restingRootsTask(ctx context.Context, tx pgx.Tx, store record.Store, ancestor string) (string, error) {
	roots, err := record.RestingRoots(ctx, tx, store, ancestor)
	if err != nil || len(roots) == 0 {
		return "", err
	}
	return fmt.Sprintf("These roots rest on the approval of %s and are still in the workflow: %s. "+
		"The daemon refuses the sign-off and the close of %s, and holds its retro where its task has no acceptance step, while any of them is in the workflow (%s). "+
		"Each root's own architect, or a person, moves it.", ancestor, restingNames(roots), ancestor, signOffRestingRoot), nil
}

// restingClause is the clause of a catch-up's reason that names roots, the roots that rest on
// ancestor's approval and are still in the workflow, to the architect of ancestor's tree
// (QueueCatchUp).
func restingClause(ancestor string, roots []record.RestingRoot) string {
	return fmt.Sprintf("; resting on the approval of %s and still in the workflow: %s (the daemon refuses the sign-off and the close of %s, and holds its retro where its task has no acceptance step, while any of them is; each root's own architect, or a person, moves it)",
		ancestor, restingNames(roots), ancestor)
}

// restingNames names each of roots with its steps that rest and the phase it stands in, as the
// refusals, the start task and the catch-up name them.
func restingNames(roots []record.RestingRoot) string {
	names := make([]string, len(roots))
	for i, root := range roots {
		names[i] = fmt.Sprintf("%s (its %s, in %s)", root.Key, stepsPhrase(root.Steps), root.Phase)
	}
	return strings.Join(names, ", ")
}

// stepsPhrase names steps as a sentence does: "spec step", "criteria and spec steps".
func stepsPhrase(steps []record.DesignStep) string {
	names := record.StepNames(steps)
	if len(names) == 1 {
		return names[0] + " step"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " steps"
}

// What a step on an ancestor's approval takes after its registration (R1, R3). A step that waits,
// closed, on the ancestor's open approval request opens or ends that wait only on Dispatch's state
// as Dispatch shows it now, which the step's pending-basis seed reads and the workflow judges by
// one verdict (pendingBasisRead, judgeBasisRead), never on an event: each event that can change
// that state (pendingBasisCue) queues the seed, and nothing else. The person's approval of the
// version, as the latest review of the latest version, opens the step, as a registration on that
// approval would; a changes request on that version, a newer version of the ancestor's document,
// the request closed with no approval of that version, and the ancestor leaving R1's first
// condition end the wait, and so do a newer version or review of the root's own document
// (applyArtifact) and the daemon's own move of the ancestor into retro or merging (endPendingOn).
// A step that stands open on an ancestor's approval changes on nothing done to the ancestor's
// document, and passes on one act alone: a person's changes request on the version it rests on,
// made after that approval.

// basisEndedAnswer opens the detail of a root's wait on a person that the end of its step's wait
// on an ancestor's request answered (tellPendingEnded). The wait is answered as a changes request
// answers it (record.AnsweredByChanges), since the contract's answeredBy enum, which refuses a
// value it does not list, has no value of its own for it; but no person asked for changes on the
// root's own document, so the detail is what ended the wait instead, naming the act, the
// ancestor's document and its version, after this prefix, by which a resume tells it from a
// person's changes request. The prefix is stored in the wait's answered detail and parsed back
// from it at the resume (resumeTask) and at a leave before the resume (basisEndedWait, read by
// untellUnresumedChanges), either of which can come after a deploy, so a release that changes its
// words must go on reading the words a release before it stored as well, as gateOffAnswer says of
// every such mark.
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
		return fmt.Sprintf("%s requested changes on version %d of %s.", cmp.Or(actor.Name(), "A person"), version, document)
	}
	return fmt.Sprintf("%s requested changes on version %d of %s: %q.", cmp.Or(actor.Name(), "A person"), version, document, reason)
}

// newerVersionAct is a version of document newer than asked, the version a pending step's request
// asks a person to approve, as the act that ends the step's wait.
func newerVersionAct(newer int, document string, asked int) string {
	return fmt.Sprintf("Version %d of %s is newer than version %d, which the request asks a person to approve.", newer, document, asked)
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
			cmp.Or(fact.Actor.Name(), "A person"), fact.Version, own, step, root, ended)
	case intake.DispatchArtifactChangesRequested:
		return changesAct(fact.Actor, fact.Version, own+", the root's own document", fact.Reason) + " " + ended + " " + stepClosed(root, step)
	}
	return fmt.Sprintf("Version %d of %s, the root's own document, is newer than version %d, which the step was registered at. %s %s",
		fact.Version, own, gate.LatestVersion, ended, stepClosed(root, step))
}

// basisArtifact applies an event of an ancestor's document to the steps of other roots that name
// that document as their basis (Store.GatesForBasisArtifact), whatever tree the ancestor is in or
// none, which is why dispatchArtifact runs it before and apart from the ancestor's own gates. To a
// step that waits on the ancestor's request the event is a cue (pendingBasisCue), and it queues
// that step's pending-basis seed, whose read of Dispatch opens or ends the wait (readBasisLater):
// the event itself opens and ends nothing. A step that stands open on the ancestor's approval
// changes on nothing done to that document (R3): it passes on a person's changes request on the
// version it rests on, made after that approval (tellBasisChanges), and stays open. A row of a
// step the deployment does not gate holds nothing and takes no event, as artifactIgnored rules for
// a root's own document (eachWaitingRoot). Every other step takes nothing from the event, and its
// root is not read: most events on a basis document change nothing for a step that stands open on
// it.
func (e *Engine) basisArtifact(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) error {
	gates, err := e.store.GatesForBasisArtifact(ctx, tx, fact.ArtifactID)
	if err != nil {
		return err
	}
	changes := fact.Kind == intake.DispatchArtifactChangesRequested
	gates = slices.DeleteFunc(gates, func(gate record.DesignGate) bool {
		return !pendingBasisCue(gate, fact) && !BasisChangesRequest(gate, changes, fact.Version, fact.Actor, fact.Mark)
	})
	return e.eachWaitingRoot(ctx, tx, gates, func(root record.Issue, gate record.DesignGate) error {
		if pendingBasisCue(gate, fact) {
			return e.readBasisLater(ctx, tx, root, gate)
		}
		return e.tellBasisChanges(ctx, tx, root, gate, intake.BasisReview{Mark: fact.Mark, Actor: fact.Actor, Reason: fact.Reason})
	})
}

// BasisChangesRequest says whether a review of an ancestor's document is the one act on it that
// gate, a step standing open on that document, passes on (R3): a changes request (changes) by a
// person on the version the basis names, which Dispatch recorded after the approval the step
// opened on. Both marks are places in the ancestor's own event log, so they compare. The live
// event (basisArtifact), the gate seed's read (basisChangesRead) and the gate seed's choice of the
// reviews it reads (the outbox's seedGateBasis) all ask it. A step a read opened on the person's
// approval (judgeBasisRead) took the place of that approval, the latest review then, so no changes
// request older than it passes.
func BasisChangesRequest(gate record.DesignGate, changes bool, version int, actor dispatch.Actor, mark record.EventMark) bool {
	return gate.OnBasis() && changes && version == gate.Basis.Version && actor.Person() && mark.After(gate.Basis.Approval)
}

// pendingBasisCue says whether fact, an event of an ancestor's document, of its approval request
// or of its issue, is a cue for gate: a step that waits, closed, on that request
// (record.DesignGate.PendingOnBasis), for which such an event queues the step's pending-basis seed
// (readBasisLater), whose read of Dispatch alone opens or ends the wait (pendingBasisRead,
// judgeBasisRead). It takes only an event that can be a cue for some step (basisCueEvent), which
// its callers settle first. Of those events, the cues are an event of the basis document at the
// basis version or a newer one, other than the basis version's own version event; a close of the
// request other than a person's answer, whose review is an event of the document; and an event of
// the ancestor's issue that shows it leaving R1's first condition. It is the one place an event is
// judged a cue for a particular step.
func pendingBasisCue(gate record.DesignGate, fact intake.Fact) bool {
	if !gate.PendingOnBasis() {
		return false
	}
	basis := *gate.Basis
	switch fact := fact.(type) {
	case intake.DispatchArtifact:
		return fact.ArtifactID == basis.ArtifactID &&
			(fact.Version > basis.Version || fact.Version == basis.Version && fact.Kind != intake.DispatchArtifactVersion)
	case intake.DispatchAskClosed:
		return fact.Ask == basis.Pending.Ask
	case intake.DispatchIssue:
		return fact.Key == basis.Issue
	}
	return false
}

// basisCueEvent says whether fact can be a cue for any step at all (pendingBasisCue), before any
// step is read: an event of a document, a close of an approval request other than an answer (a
// question is no request a step waits on; an ask whose kind an older Dispatch does not say is
// read), or an event of an issue that shows it leaving R1's first condition. Most issue events
// show no leave, so the engine reads no step for them.
func basisCueEvent(fact intake.Fact) bool {
	switch fact := fact.(type) {
	case intake.DispatchArtifact:
		return true
	case intake.DispatchAskClosed:
		return fact.State != "answered" && fact.Kind != record.AskQuestion
	case intake.DispatchIssue:
		return ancestorLeft(fact.Key, "", fact.Status, fact.HandedOver) != ""
	}
	return false
}

// basisVerdictKind is what a basisVerdict does to a step's wait on an ancestor's request: nothing
// (basisUnchanged), open the step (basisOpens), or end the wait (basisEnds).
type basisVerdictKind int

const (
	basisUnchanged basisVerdictKind = iota
	basisOpens
	basisEnds
)

// basisVerdict is what Dispatch's state now does to a step waiting, closed, on an ancestor's
// request for a person's approval of a version (judgeBasisRead), as kind says: open the step on
// the person's approval, at approval in the ancestor's event log and by approver; end the wait,
// text being the act that ended it and actor who made it ("" for nobody); or change nothing, text
// being why.
type basisVerdict struct {
	kind     basisVerdictKind
	text     string
	actor    string
	approval record.EventMark
	approver string
}

// judgeBasisRead is the one verdict on read, the pending-basis seed's read of Dispatch
// (pendingBasisRead), for gate, a step that waits, closed, on the ancestor's request for a
// person's approval of the basis version (dispatch://INFRA-3320, R1). Dispatch's state now decides
// it, and never the event that queued the seed. accepted is what the daemon's own store shows of
// the ancestor's acceptance, read only where the verdict asks it.
//
//   - A version of the document newer than the basis version ends the wait.
//   - With the basis version the latest, its latest review decides. A person's approval of it opens
//     the step at that review's place in the ancestor's event log, on an ancestor that still meets
//     R1's first condition as the store and the read show it, and ends the wait on one that has
//     left it. An approval by an actor Dispatch names and who is no person ends the wait. A changes
//     request on it ends the wait, whoever made it. An approval that names nobody, which only a
//     Dispatch that records no reviewer gives, decides nothing: the ancestor's leaving R1's first
//     condition ends the wait, and nothing else changes anything.
//   - With no such review, the request no longer open, closed or replaced by another
//     (RequestClosed), ends the wait. The seed read the request's close from Dispatch by its id
//     (intake.BasisRead.Closed), and the end names who closed it (the actor the close names, or
//     someone Dispatch does not name) and the state Dispatch shows it in, and takes that actor as
//     its own. Where the ancestor has left R1's first condition, the end names that leave first
//     and then the close. A read in this case that carries no close is no read the seed makes,
//     and is an error rather than an end that names nobody.
//   - With the request still open, the ancestor's leaving R1's first condition ends the wait, and
//     nothing else changes anything.
//
// So an event delivered late or replayed changes nothing that Dispatch no longer shows: an approval
// a later changes request superseded opens nothing, a changes request older than the request ends
// nothing, and the close of a request the issue's reopen opened again ends nothing.
func judgeBasisRead(gate record.DesignGate, read intake.BasisRead, accepted func() (string, error)) (basisVerdict, error) {
	basis := *gate.Basis
	document := documentOf(basis.Issue, gate.Step.OrSpec())
	approval := read.Approval
	left := func() (string, error) {
		why, err := accepted()
		return ancestorLeft(basis.Issue, why, read.Ancestor.Status, read.Ancestor.HandedOver), err
	}
	ends := func(act, actor string) basisVerdict { return basisVerdict{kind: basisEnds, text: act, actor: actor} }
	approved := approval.ApprovedLatest() == basis.Version
	approvedByPerson := approved && approval.By.Person()
	changes, _ := approval.ChangesRequestedLatest()
	switch {
	case approval.LatestVersion > basis.Version:
		return ends(newerVersionAct(approval.LatestVersion, document, basis.Version), ""), nil
	case approved && approval.By != (dispatch.Actor{}) && !approval.By.Person():
		return ends(fmt.Sprintf("%s approved version %d of %s, and %s is no person.", approval.By.Name(), basis.Version, document, approval.By.Name()),
			approval.By.Name()), nil
	case changes == basis.Version:
		reason := ""
		if approval.Reason != nil {
			reason = *approval.Reason
		}
		return ends(changesAct(approval.By, basis.Version, document, reason), approval.By.Name()), nil
	}

	closed := RequestClosed(approval, basis.Version, basis.Pending.Ask)
	if closed && read.Closed == nil {
		return basisVerdict{}, fmt.Errorf("the read of approval request %s of version %d of %s shows it closed and carries no read of its close",
			basis.Pending.Ask, basis.Version, document)
	}
	act, err := left()
	if err != nil {
		return basisVerdict{}, err
	}
	switch {
	case closed:
		closer := read.Closed.Actor.Name()
		sentence := fmt.Sprintf("%s closed approval request %s of version %d of %s, as %s, with no approval of that version.",
			cmp.Or(closer, "Someone Dispatch does not name"), basis.Pending.Ask, basis.Version, document, read.Closed.State)
		if act != "" {
			sentence = act + " " + sentence
		}
		return ends(sentence, closer), nil
	case act != "":
		return ends(act, ""), nil
	case approvedByPerson:
		return basisVerdict{kind: basisOpens, approval: record.EventMark{Seq: approval.Seq, At: approval.At}, approver: approval.By.Name()}, nil
	case approved:
		return basisVerdict{text: "an approval of the ancestor's version that names nobody opens and ends no step waiting on it"}, nil
	}
	return basisVerdict{text: "Dispatch shows the ancestor's request still open on its version, so the step waiting on it neither opens nor ends"}, nil
}

// RequestClosed says whether approval, Dispatch's approval of an ancestor's document, shows ask,
// the request for a person's approval of version that a step waits on, no longer open with no
// approval of that version: version is the latest, its latest review neither approves it nor asks
// for changes on it, and the document holds no open request or another one. It is judgeBasisRead's
// closed case, and the pending-basis seed asks it of its own read to decide whether to read the
// request's close from Dispatch as well (the outbox's readBasis), so the seed reads the close in
// that case alone and the verdict finds the close on every read that asks for it.
func RequestClosed(approval dispatch.Approval, version int, ask string) bool {
	changes, _ := approval.ChangesRequestedLatest()
	return approval.LatestVersion <= version && approval.ApprovedLatest() != version && changes != version &&
		(approval.State != "awaiting" || approval.AskID != ask)
}

// readBasisLater queues the pending-basis seed of gate, a step of root that waits, closed, on an
// ancestor's request, for an event that is a cue for it (pendingBasisCue). The seed reads Dispatch,
// and that read alone opens or ends the wait (pendingBasisRead), so no event opens or ends it, and
// an event delivered late or replayed is judged by what Dispatch shows when the seed runs.
func (e *Engine) readBasisLater(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate) error {
	basis := *gate.Basis
	e.logOnCommit(ctx, "workflow: an event is a cue for a step waiting on an ancestor's request, so the step's pending-basis seed reads Dispatch",
		"issue", root.Key, "step", gate.Step.OrSpec(), "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask)
	return e.enqueue(ctx, tx, root.Key, record.PendingBasisSeedOf(gate, root.Generation))
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
//
// A root out of the workflow (record.Stopped) opens nothing: its leave ended the waits of its
// steps (leaveBases), and a registration after the leave takes no basis (gateRegistered). A step
// that still waits there, written before a leave ended such waits, ends its wait as the leave
// would have, with nobody told (endPendingUntold), and is returned as it ended.
func (e *Engine) openPendingBasis(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, approval record.EventMark, approver string) (record.DesignGate, error) {
	opened := record.OpenPendingBasis(gate, approval, approver)
	if !opened.OnBasis() {
		return opened, nil
	}
	if record.Stopped(root) {
		return e.endPendingUntold(ctx, tx, root, gate)
	}
	step := opened.Step.OrSpec()
	if err := e.putGatePublished(ctx, tx, root.Key, opened, opened.LatestVersion); err != nil {
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
		chains, err := record.ReadChainsOf(ctx, tx, e.store, current.Project, []string{current.Key})
		if err != nil {
			return opened, err
		}
		_, err = e.answerOpenGates(ctx, tx, chains, []record.Issue{*current}, nil)
		return opened, err
	}
	return opened, e.notice(ctx, tx, root.Key, openedNotice(root.Key, opened, ""))
}

// pendingEnd is what ended a step's wait on an ancestor's request, as tellPendingEnded tells the
// end: the pending-basis seed's read of Dispatch (endedByRead, pendingBasisRead), the daemon's own
// move of the ancestor into retro or merging (endedByMove, endPendingOn), or an act on the root's
// own document (endedByOwnDocument, applyArtifact). Only the last is told apart, to a root waiting
// on a person; the ancestor's architect hears every end alike (queueRestingRoot).
type pendingEnd int

const (
	endedByRead pendingEnd = iota
	endedByMove
	endedByOwnDocument
)

// endPendingBasis ends gate's wait on the ancestor's request without opening it, act being what
// ended it, actor who made it ("" for nobody) and by how it ended (endedByRead or endedByMove):
// the step stays closed and registered on the root's own document with no basis
// (record.EndPendingBasis), and the end is told (tellPendingEnded).
func (e *Engine) endPendingBasis(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate, act, actor string, by pendingEnd) error {
	step := gate.Step.OrSpec()
	basis := *gate.Basis
	ended := record.EndPendingBasis(gate)
	if err := e.putGatePublished(ctx, tx, root.Key, ended, ended.LatestVersion); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a step's wait on an ancestor's approval request ended", "issue", root.Key, "step", step,
		"ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask, "act", act)
	return e.tellPendingEnded(ctx, tx, root.Key, step, basis, act+" "+waitEnded(root.Key, step, basis)+" "+stepClosed(root.Key, step), actor, by)
}

// endPendingUntold ends gate's wait, a step of root that waits on an ancestor's request, with
// nobody told, as root's leave of the workflow ends it (leaveBases): the step stays closed and
// registered on the root's own document with no basis (record.EndPendingBasis), as any end leaves
// it. The leave told the ancestor's architect that root left, and root's tree runs nothing, so
// neither architect has a step to hear of. The end moves no document of the step and leaves it
// closed as it stood, so it publishes nothing and putGateTold tells nothing. It returns the step
// as it ended.
func (e *Engine) endPendingUntold(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate) (record.DesignGate, error) {
	ended := record.EndPendingBasis(gate)
	if err := e.putGateTold(ctx, tx, ended, ended.LatestVersion); err != nil {
		return ended, err
	}
	e.logEndedUntold(ctx, root.Key, gate.Step.OrSpec(), *gate.Basis)
	return ended, nil
}

// logEndedUntold logs the end of root key's step's wait on basis's request with nobody told, the
// end of such a wait on a root out of the workflow (endPendingUntold, tellPendingEnded, and
// tellRegisteredBasis for a registration with no basis on that root).
func (e *Engine) logEndedUntold(ctx context.Context, key string, step record.DesignStep, basis record.GateBasis) {
	e.logOnCommit(ctx, "workflow: a step's wait on an ancestor's approval request ended with its root out of the workflow, and nobody is told", "issue", key,
		"step", step, "ancestor", basis.Issue, "basis", basis.ArtifactID, "basis_version", basis.Version, "ask", basis.Pending.Ask)
}

// tellPendingEnded tells the end of root key's step's wait on basis's request: what says what
// ended it, naming the act, the ancestor's document and its version, and by how it ended. The
// ancestor's architect is told as a resting-root notice, once per request, however the wait
// ended, wherever the daemon records the ancestor on a known chain (queueRestingRoot,
// ancestorRecorded). A root waiting on a person keeps that wait beside its step's, so the end
// answers it, as a changes request does, with no notice beside it: the answer's detail says what
// ended the step's wait (basisEndedAnswer). An act on the root's own document (endedByOwnDocument)
// that meets a wait already answered is named by the resume as well: what is joined to the
// answer's detail (basisEndedAfter), and the resume names it after the act that answered the
// wait. Every other end reaches the root's architect as a basis-ended notice that names the act,
// the ancestor's document and the version, and who made the act (actor, "" for nobody). The step
// no longer rests on the ancestor through that wait, so the ancestor is named to the fact's
// settle, which starts the retro of one held before it that nothing holds any longer (restEnded).
//
// The end of the wait of a root out of the workflow (record.Stopped) is told to nobody, as the end
// the root's leave makes (endPendingUntold): the leave ended every such wait, so one still
// standing there was left by a release that ended none at a leave (a rollback, then this release
// again), and the root's tree runs nothing for either architect to hear of.
func (e *Engine) tellPendingEnded(ctx context.Context, tx pgx.Tx, key string, step record.DesignStep, basis record.GateBasis, what, actor string, by pendingEnd) error {
	root, err := e.store.Issue(ctx, tx, key)
	if err != nil || root == nil {
		return err
	}
	if record.Stopped(*root) {
		e.logEndedUntold(ctx, key, step, basis)
		return nil
	}
	e.restEnded(ctx, basis.Issue)
	if err := e.queueRestingRoot(ctx, tx, *root, step, basis, restingEnded(basis.Pending.Ask), what); err != nil {
		return err
	}
	switch {
	case root.AwaitingPerson() && root.Awaiting.AnsweredAt == nil:
		e.logOnCommit(ctx, "workflow: the end of a step's wait on an ancestor's request answers the root's wait on a person", "issue", key,
			"step", step, "ask", basis.Pending.Ask)
		return e.answerWait(ctx, tx, *root, record.AnsweredByChanges, basisEndedAnswer+what)
	case root.AwaitingPerson() && by == endedByOwnDocument:
		e.logOnCommit(ctx, "workflow: a root waiting on a person is told by its resume of the act on its own document that ended its step's wait", "issue", key,
			"step", step, "ask", basis.Pending.Ask)
		root.Awaiting.AnsweredDetail += basisEndedAfter + what
		return e.WriteIssue(ctx, tx, *root)
	}
	return e.notice(ctx, tx, key, record.Notice{Kind: "basis-ended", Role: claim.RoleArchitect, Version: basis.Version, Actor: actor,
		Ref: stepRef(basis.Issue, step), Reason: what})
}

// tellBasisChanges passes on review, a person's changes request on the version of the ancestor's
// document that gate, a step of root, stands open on, made after the approval the step opened on
// (R3): basis-changes-requested to the root's architect, naming the ancestor's document, the
// version, the person and their reason, and, where an architect of the ancestor is in Legion's
// work on its Dispatch chain (ancestorReached), the same keyed by the ancestor, so it reaches the
// architect that owns the ancestor (the outbox's owningArchitect), naming the root and the
// requirements it serves. The step
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
		cmp.Or(review.Actor.Name(), "A person"), basis.Version, documentOf(basis.Issue, step), step, root.Key)
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
	reached, err := e.ancestorReached(ctx, tx, basis.Issue)
	if err != nil || !reached {
		if err == nil {
			e.logOnCommit(ctx, "workflow: no architect of the ancestor is in Legion's work on its Dispatch chain, so it is not told the changes request", "issue", root.Key,
				"ancestor", basis.Issue)
		}
		return err
	}
	told.Reason = asked + " The step of " + root.Key + " stays open. " + reason + " " + servesSentence(root.Key, basis.Issue, basis.Serves)
	return e.notice(ctx, tx, basis.Issue, told)
}

// readBasisOnCue queues the pending-basis seed of every step of another root that fact, the close
// of an approval request or an event of an issue, is a cue for (pendingBasisCue, readBasisLater),
// whatever tree the ancestor is in or none: the seed's read of Dispatch opens or ends the wait,
// and fact itself opens and ends nothing. A close reaches the steps that wait on that request as
// an ancestor's (Store.GatesByPendingAsk, since the root-only ask mirror records no ancestor's
// ask). A person's answer of an approval request is no cue: it is a review, which Dispatch writes
// in the answer's transaction and whose artifact.* event, appended after the ask's, is the cue
// (basisArtifact). An event of an issue that shows it leaving R1's first condition (Dispatch shows
// it done or retro, or it no longer carries the legion label) reaches the steps that wait on a
// request of that issue (Store.GatesRestingOn). Most events that reach here are of issues nothing
// waits on, and most issue events show no leave, so the steps are read only for an event that can
// be a cue (basisCueEvent). A row of a step the deployment does not gate holds nothing and takes
// no event (eachWaitingRoot).
func (e *Engine) readBasisOnCue(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	if !basisCueEvent(fact) {
		return nil
	}
	var gates []record.DesignGate
	var err error
	switch fact := fact.(type) {
	case intake.DispatchAskClosed:
		gates, err = e.store.GatesByPendingAsk(ctx, tx, fact.Ask)
	case intake.DispatchIssue:
		gates, err = e.store.GatesRestingOn(ctx, tx, fact.Key)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	gates = slices.DeleteFunc(gates, func(gate record.DesignGate) bool { return !pendingBasisCue(gate, fact) })
	return e.eachWaitingRoot(ctx, tx, gates, func(root record.Issue, gate record.DesignGate) error {
		return e.readBasisLater(ctx, tx, root, gate)
	})
}

// basisIssue applies fact, an event of an issue, to the steps of other roots on that issue's
// approval as an ancestor's, whatever tree the ancestor is in or none. To each step that waits,
// closed, on a request of the issue the event is a cue as every cue is, and it queues the step's
// pending-basis seed (readBasisOnCue): the seed's read of Dispatch opens or ends the wait, and the
// event opens and ends none. Where the event shows the issue done or retro, it also ends every
// basis standing open on its approval of a root in Legion's work (endOpenOn, R2 of
// dispatch://INFRA-3320), unless the daemon has applied an event of the issue newer than it
// (ancestorEventSuperseded). That end reads the steps that rest on the issue
// (Store.GatesRestingOn) a second time, apart from the cue's read: only a done or retro event
// reads them again, and only for an issue something rests on, which is rare, does that read find
// any and the event get checked against a newer one.
func (e *Engine) basisIssue(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) error {
	if err := e.readBasisOnCue(ctx, tx, fact); err != nil || !record.AcceptedStatus(fact.Status) {
		return err
	}
	gates, err := e.store.GatesRestingOn(ctx, tx, fact.Key)
	if err != nil || len(gates) == 0 {
		return err
	}
	superseded, err := e.ancestorEventSuperseded(ctx, tx, fact)
	if err != nil || superseded {
		return err
	}
	return e.endOpenOn(ctx, tx, gates, fact.Key, ancestorLeft(fact.Key, "", fact.Status, fact.HandedOver))
}

// ancestorEventSuperseded says whether the daemon has applied an event of fact's issue newer than
// fact, which then says nothing of the issue now: its record's last sequence, or its copy of
// Dispatch's issue, which every applied event of the issue writes first, holds a later one. An
// event with no sequence, which no event of the issue orders, is never superseded.
func (e *Engine) ancestorEventSuperseded(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) (bool, error) {
	if fact.Seq == 0 {
		return false, nil
	}
	recorded, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil {
		return false, err
	}
	if recorded != nil && fact.Seq <= recorded.LastDispatchSeq {
		return true, nil
	}
	mirrored, err := e.store.DispatchIssuesOf(ctx, tx, []string{fact.Key})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(mirrored, func(row record.DispatchIssueRow) bool { return row.Key == fact.Key && row.Seq > fact.Seq }), nil
}

// endPendingOn ends every step of another root that waits, closed, on a request of ancestor
// (pendingOf, endPendingBasis), and every basis standing open on ancestor's approval of a root in
// Legion's work (endOpenOn), act being what ended them: the ancestor is accepted.
func (e *Engine) endPendingOn(ctx context.Context, tx pgx.Tx, ancestor, act string) error {
	gates, err := e.store.GatesRestingOn(ctx, tx, ancestor)
	if err != nil {
		return err
	}
	if err := e.eachWaitingRoot(ctx, tx, pendingOf(gates, ancestor), func(root record.Issue, gate record.DesignGate) error {
		return e.endPendingBasis(ctx, tx, root, gate, act, "", endedByMove)
	}); err != nil {
		return err
	}
	return e.endOpenOn(ctx, tx, gates, ancestor, act)
}

// endOpenOn ends each of gates that stands open on ancestor's approval (record.DesignGate.OnBasis,
// the basis naming ancestor) of a root in Legion's work, once ancestor is done or accepted: the
// step is left unregistered (Store.DeleteGate), its publication follows, the root's architect is
// told the basis ended by act and that it registers the step again, and the ancestor's architect
// that the root rests on it no more (queueRestingRoot). A root out of Legion's work keeps its
// step, which a new run's registration replaces. A row of a step the deployment does not gate
// holds nothing, as basisArtifact rules.
func (e *Engine) endOpenOn(ctx context.Context, tx pgx.Tx, gates []record.DesignGate, ancestor, act string) error {
	for _, gate := range gates {
		if !e.gatesStep(gate.Step) || !gate.OnBasis() || gate.Basis.Issue != ancestor {
			continue
		}
		root, err := e.store.Issue(ctx, tx, gate.Issue)
		if err != nil {
			return err
		}
		if root == nil || !record.InLegionsWork(*root) {
			continue
		}
		step, basis := gate.Step.OrSpec(), *gate.Basis
		before, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		if err := e.store.DeleteGate(ctx, tx, root.Key, step); err != nil {
			return err
		}
		if err := record.PublishChangedGates(ctx, e.store, tx, e.cfg.Steps, root.Key, before, e.now()); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a step standing open on an ancestor's approval ended with the ancestor's acceptance", "issue", root.Key,
			"step", step, "ancestor", ancestor, "basis", basis.ArtifactID, "basis_version", basis.Version, "act", act)
		what := fmt.Sprintf("%s The %s step of %s stood open on the approval of version %d of %s, so its basis ends, and the step is unregistered: register it again.",
			act, step, root.Key, basis.Version, documentOf(basis.Issue, step))
		if err := e.queueRestingRoot(ctx, tx, *root, step, basis, restingBasisEnded, what); err != nil {
			return err
		}
		if err := e.notice(ctx, tx, root.Key, record.Notice{Kind: "basis-ended", Role: claim.RoleArchitect, Version: basis.Version,
			Ref: stepRef(basis.Issue, step), Reason: what}); err != nil {
			return err
		}
	}
	return nil
}

// pendingOf is the steps of gates that wait, closed, on a request of ancestor: the rows
// Store.GatesRestingOn reads whose basis names ancestor now, the steps that rest on it alone left
// out.
func pendingOf(gates []record.DesignGate, ancestor string) []record.DesignGate {
	var pending []record.DesignGate
	for _, gate := range gates {
		if gate.PendingOnBasis() && gate.Basis.Issue == ancestor {
			pending = append(pending, gate)
		}
	}
	return pending
}

// eachWaitingRoot calls do for each of gates, steps on an ancestor's approval (waiting on its
// request, or, for basisArtifact, standing open on it), with the root it is a step of, in order,
// and stops at do's first error. A row of a step the deployment does not gate holds nothing and
// takes no event, as artifactIgnored rules for a root's own document, and a step of a root the
// daemon no longer records has no root to act on, so neither is done.
func (e *Engine) eachWaitingRoot(ctx context.Context, tx pgx.Tx, gates []record.DesignGate, do func(root record.Issue, gate record.DesignGate) error) error {
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
		if err := do(*root, gate); err != nil {
			return err
		}
	}
	return nil
}

// pendingBasisRead applies the pending-basis seed's read of Dispatch for a step waiting on an
// ancestor's request (intake.PendingBasisRead), the one place that opens or ends such a wait on
// what Dispatch shows: by the one verdict (judgeBasisRead), it opens the step on the person's
// approval the read shows (openPendingBasis), whose place in the ancestor's event log the open step
// keeps as its approval, so a changes request older than it passes nothing on
// (BasisChangesRequest); it ends the wait (endPendingBasis), and the end reaches the ancestor's
// architect wherever the daemon records the ancestor on a known chain, in Legion's work or not
// (tellPendingEnded); or it logs why it changes nothing. The daemon's own store is read for the
// ancestor's acceptance only where the verdict asks it. The fact names the registration it reads
// for, at the precision the store keeps (record.PendingBasisSeedOf normalized it once), so it
// compares with the stored step's registration as it is. A read of a root of another generation,
// or of a step that no longer waits on that request at that version under that same registration,
// changes nothing: a read applied first opened or ended it, an act of the root's own document or
// the daemon's own move of the ancestor ended it, or a registration since replaced it.
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
		gate.Basis.Version != fact.BasisVersion || gate.Basis.Pending.Ask != fact.Ask ||
		!gate.Basis.RegisteredAt.Equal(fact.RegisteredAt) {
		e.logOnCommit(ctx, "workflow: the pending-basis seed read a wait that has opened or ended since, and changes nothing", "issue", root.Key,
			"step", step, "basis", fact.BasisArtifactID, "basis_version", fact.BasisVersion, "ask", fact.Ask)
		return intake.Result{}, nil
	}
	basis := *gate.Basis
	verdict, err := judgeBasisRead(gate, fact.Read, func() (string, error) {
		why, _, err := record.BasisAcceptance(ctx, e.store, tx, basis.Issue)
		return why, err
	})
	if err != nil {
		return intake.Result{}, err
	}
	switch verdict.kind {
	case basisOpens:
		_, err = e.openPendingBasis(ctx, tx, *root, gate, verdict.approval, verdict.approver)
	case basisEnds:
		err = e.endPendingBasis(ctx, tx, *root, gate, verdict.text, verdict.actor, endedByRead)
	default:
		e.logOnCommit(ctx, "workflow: "+verdict.text, "issue", root.Key, "step", step, "basis", basis.ArtifactID, "basis_version", basis.Version,
			"ask", basis.Pending.Ask, "state", fact.Read.Approval.State, "latest_version", fact.Read.Approval.LatestVersion,
			"ancestor_status", fact.Read.Ancestor.Status)
	}
	return intake.Result{}, err
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
		!BasisChangesRequest(gate, true, fact.BasisVersion, fact.Changes.Actor, fact.Changes.Mark) {
		e.logOnCommit(ctx, "workflow: the gate seed read a changes request on an ancestor's version no open step of this root rests on", "issue", root.Key,
			"step", step, "basis", fact.BasisArtifactID, "basis_version", fact.BasisVersion)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.tellBasisChanges(ctx, tx, *root, gate, fact.Changes)
}
