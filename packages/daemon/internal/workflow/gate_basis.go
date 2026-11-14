package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// same basis, so its record on the root's issue was posted then.
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
