package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Decision is a person's review of a design document as a sender holds it when it asks whether
// the review is still the person's word (SupersedingApproval): what the person did (Kind), on which
// step's document (Step), the document itself when the sender knows it (Artifact, "" when it does
// not), the version reviewed when the sender knows it (Version, 0 when it does not: a kept
// decision's row records none), and where Dispatch recorded the review (Mark, empty where the
// sender holds no mark of it). A sender that holds only the review's Dispatch reference asks
// through RefSuperseded, which resolves the step from the reference once.
type Decision struct {
	Kind     record.DecisionKind
	Step     record.DesignStep
	Artifact string
	Version  int
	Mark     record.EventMark
}

// Approval is a person's approval a gate records (record.DesignGate.ApprovedVersion): the one
// that superseded a review, as SupersedingApproval answers it, by the version approved, which is what
// a sender names of it.
type Approval struct {
	Version int
}

// SupersedingApproval is the one place that decides whether a person's recorded review of a design
// document is still their word, so a task or a notice may name it. Every sender asks it, of the
// gate the issue holds for d.Step as the record holds it when it asks (gate, nil where the issue
// holds none) and of whether the deployment gates that step (gated): the changes notice
// (tellChanges; the registration's and the artifact event's reading of the gate before they apply
// the review; and the daemon's readNoticeTree at the notice's delivery), the resume of a root a
// person answered (resumeTask), the decisions and the approval kept for an architect's start or
// catch-up (KeptDecisionsTask, again at each send and each delivery) and the controller's relay of
// a decision nobody in its tree hears (the daemon's parkDecision, and again at a relayed changes
// request's publish, its controllerNotice); the clear of a generation asks it too, before it
// deletes the gates of the issues it clears, of each decision kept on them and each changes
// request the outbox holds on them (Engine.ClearGeneration). It holds the guards that choose
// whether the one rule (classify.ReviewSuperseded) is asked of a review, listed below, so a new
// step kind or a change to how an approval of the same version is read is made here once. Which
// reviews a sender asks about is the sender's: the daemon's readNoticeTree and controllerNotice,
// and the clear, ask again of a queued notice only when it is a changes request, since an
// approval's own notice is the person's later word.
//
// It answers the approval that superseded the review, which a sender may name, and nil when the
// review stands. The review stands at each of these guards, in the order they are asked, because
// nothing the gate records can then be the person's later word:
//
//   - a decision that is no changes request or approval (an answer on an ask, a carried gate's
//     opening);
//   - no gate of the issue for d.Step's document, or a gate of another step (a step and a gate
//     naming none are the spec step's, record.DesignStep.OrSpec);
//   - a step the deployment does not gate (its approval is its registration's and no person's);
//   - a gate with no approval;
//   - a gate that now holds another document than the one reviewed, where the sender knows it
//     (d.Artifact);
//   - an approval that is the gate's own approval: the same version, and a mark that can be the
//     same event (record.EventMark.Same), which is no later word than itself.
//
// Past them the gate is asked whether its approval followed the review (classify.ReviewSuperseded):
// at d.Version, or, where the sender knows no version, at the version the gate holds approved — a
// review of a lower one is answered by that approval, and one of that version is placed by its
// mark — and at d.Mark. Both marks are Dispatch's, so the two are ordered by Dispatch's sequences
// when both carry one and otherwise by its times (record.EventMark.After), never by the daemon's
// clock against Dispatch's.
//
// One other function asks classify.ReviewSuperseded itself: requestStands
// (awaiting_reconcile.go). It is no sender of a task or notice about a person's recorded decision.
// It asks whether an architect's approval request of a document still waits on the person, a
// different question that the same classifier answers, so it does not go through this predicate.
func SupersedingApproval(gate *record.DesignGate, gated bool, d Decision) (superseded *Approval) {
	if d.Kind != record.DecisionDesignChangesRequested && d.Kind != record.DecisionDesignApproved {
		return nil
	}
	if gate == nil || gate.Step.OrSpec() != d.Step.OrSpec() || !gated || gate.ApprovedVersion == nil ||
		d.Artifact != "" && gate.ArtifactID != d.Artifact {
		return nil
	}
	version := d.Version
	if version == 0 {
		version = *gate.ApprovedVersion
	}
	if d.Kind == record.DecisionDesignApproved && version == *gate.ApprovedVersion && d.Mark.Same(gate.ApprovedMark) {
		return nil
	}
	if !classify.ReviewSuperseded(*gate, version, d.Mark) {
		return nil
	}
	return &Approval{Version: *gate.ApprovedVersion}
}

// RefSuperseded is SupersedingApproval for a sender that holds only the review's Dispatch reference,
// ref on issue (record.ParkedDecision.Ref, record.Notice.Ref): the step whose document ref names
// (refStep) is resolved once here and set as d.Step, and SupersedingApproval is asked of that step's
// gate in gates, the gates of issue, and of whether steps, the steps the deployment gates, holds
// it. A reference that names no step's document (an ask's or a comment's) is asked of no gate,
// and stands: nil.
func RefSuperseded(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep, issue, ref string, d Decision) *Approval {
	step, named := refStep(issue, ref)
	if !named {
		return nil
	}
	d.Step = step
	return SupersedingApproval(gateOf(gates, step), slices.Contains(steps, step), d)
}

// NoticeSuperseded is RefSuperseded of the decision of kind a queued notice carries on issue, a
// design decision's notice or the parked-decision relay of one read as the Notice it is: its
// reference, its version, Dispatch's mark of the review (record.Notice.Review) and the document
// reviewed (record.Notice.Artifact). Every reader of a queued notice asks through it, the daemon's
// delivery and publish (its decisionSuperseded) and the clear of a generation
// (Engine.ClearGeneration), so each asks the same question of the same notice.
func NoticeSuperseded(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep,
	issue string, notice record.Notice, kind record.DecisionKind) *Approval {
	return RefSuperseded(gates, steps, issue, notice.Ref, Decision{Kind: kind,
		Artifact: notice.Artifact, Version: notice.Version, Mark: notice.Review})
}

// gateOf is the gate gates holds for step, nil where it holds none: the gate SupersedingApproval asks
// of a decision on step's document.
func gateOf(gates map[record.DesignStep]record.DesignGate, step record.DesignStep) *record.DesignGate {
	gate, ok := gates[step]
	if !ok {
		return nil
	}
	return &gate
}
