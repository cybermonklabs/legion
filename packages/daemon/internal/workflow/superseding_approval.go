package workflow

import (
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Decision is a person's review of a design document as a sender holds it when it asks whether
// the review is still the person's word (SupersedingApproval): what the person did (Kind), on which
// step's document (Step), the document itself when the sender knows it (Artifact, "" when it does
// not), the version reviewed when the sender knows it (Version, 0 when it does not: a decision an
// earlier release kept, or a wait it answered), and where Dispatch recorded the review (Mark,
// empty where the sender holds no mark of it). A sender that holds only the review's Dispatch
// reference asks through RefSuperseded, which resolves the step from the reference once.
//
// Basis says the approval decided on may be the one an ancestor's approval opened the step on, at
// the version the root registered (record.OpenOnBasis): the news of an approved version names
// whichever approval the gate held when the news was made (StepApprovalStands). Every other
// decision is a person's own, which the gate holds as record.DesignGate.ApprovedByPerson, so no
// sender reads a basis as a person's word in its place (dispatch://INFRA-3320, R2 and R3).
type Decision struct {
	Kind     record.DecisionKind
	Step     record.DesignStep
	Artifact string
	Version  int
	Mark     record.EventMark
	Basis    bool
}

// Approval is a person's approval a gate records (record.DesignGate.ApprovedByPerson), by the
// version approved, which is what a sender names of it.
type Approval struct {
	Version int
}

// Supersession is what SupersedingApproval answers for a person's decision that is no longer
// their word. By is the person's approval the gate records in its place, which a sender may name,
// never the version a basis opened the step at. It is nil where the gate records no person's
// approval, so none replaced the decision: an approval that a later changes request cleared
// (classify.ApplyDesignGateEvent), one of a document the gate no longer holds, its step holding
// another document with none approved, or one a basis now stands beside with no approval of the
// person's kept (record.DesignGate.KeptPerson). For an approval of a document the gate no longer
// holds, a By that is set is the approval of that other document. A sender names a version only
// where By is set. Only a person's approval replaces a changes request, so By is always set for
// one.
type Supersession struct {
	By *Approval
}

// SupersedingApproval is the one place that decides whether a person's recorded decision on a
// design document, a changes request or an approval, is still their word, so a task or a notice
// may name it. Every sender of a task or a notice about such a decision asks it, of the gate the
// issue holds for d.Step as the record holds it when it asks (gate, nil where the issue holds
// none) and of whether a person approves that step (gated): the changes notice (tellChanges; the
// registration's and the artifact event's reading of the gate before they apply the review; and
// the daemon's readNoticeTree at the notice's delivery), the resume of a root a person answered
// (resumeTask), the decisions and the approval kept for an architect's start or catch-up
// (KeptDecisionsTask, again at each send and each delivery; the kept approval through
// StepApprovalStands), the controller's relay of a decision nobody in its tree hears (the daemon's
// parkDecision, and again at a relayed changes request's publish, its controllerNotice; and the
// relay of a person's approval whose handover a later change of the root's status refused,
// Engine.refuseHandover) and the news of an approved version at each send of the task that carries
// it (StepApprovalStands, which the daemon's itemStands asks); the clear of a generation asks it
// too, before it deletes the gates of the issues it clears, of each decision kept on them and
// each changes request the outbox holds on them (Engine.ClearGeneration). It holds the guards
// that choose which of the two rules is asked of a decision, listed below, and the rule for an
// approval itself, so a new step kind or a change to how a decision is read is made here once.
// Which decisions a sender asks about is the sender's: of the queued notices,
// record.ChangesRequest is the one rule for which rows a reader asks again.
//
// It answers nil while the decision stands, and otherwise what took its place (Supersession).
// A decision of either kind stands at each of these guards, in the order they are asked, because
// nothing the gate records can then be the person's later word:
//
//   - a decision that is no changes request or approval (an answer on an ask, a carried gate's
//     opening);
//   - no gate of the issue for d.Step's document: the daemon cleared the gate (a re-admission's
//     clear deletes the root's gates, and the approval it kept is asked after that), so nothing
//     in the record can be the person's later word;
//   - a gate of another step (a step and a gate naming none are the spec step's,
//     record.DesignStep.OrSpec);
//   - a step no person approves (its approval is its registration's: record.TakesStep).
//
// Past them an approval stands while the gate still holds it, which this function compares itself,
// so no other function can ask it: the gate's row holds the document approved (d.Artifact, or the
// document the gate holds where the sender knows none), and the person's approval it records
// (record.DesignGate.ApprovedByPerson) is of the version approved (d.Version, or that approval's
// version where the sender knows none). A decision that may name a basis (d.Basis) is compared
// with the version the gate holds approved, a basis's included, while a basis stands on the step.
// A new version that closed the gate since leaves a person's approval standing: the gate keeps that
// approval as the version a person approved below the latest, as it keeps it beside a basis that
// opened the step since (record.DesignGate.KeptPerson). Its mark is not asked: a person who
// approves the same version again after a changes request gives the same word again, under a new
// mark. An approval the gate no longer holds is superseded: a later changes request cleared it, or
// the gate now holds another version approved or another document.
//
// Past them a changes request stands where the gate holds no person's approval, or now holds
// another document than the one reviewed, where the sender knows it (d.Artifact): a basis is no
// answer a person gave to it. Otherwise the gate is asked whether the person's approval followed
// the review (classify.ReviewSuperseded): at d.Version, or, where the sender knows no version, at
// the version that approval holds — a review of a lower one is answered by that approval, and one
// of that version is placed by its mark — and at d.Mark. Both marks are Dispatch's, so the two are
// ordered by Dispatch's sequences when both carry one and otherwise by its times
// (record.EventMark.After), never by the daemon's clock against Dispatch's. The approval a
// supersession names (Supersession.By) is always a person's.
//
// One other function asks classify.ReviewSuperseded itself: requestStands
// (awaiting_reconcile.go). It is no sender of a task or notice about a person's recorded decision.
// It asks whether an architect's approval request of a document still waits on the person, a
// different question that the same classifier answers, so it does not go through this predicate.
func SupersedingApproval(gate *record.DesignGate, gated bool, d Decision) *Supersession {
	if d.Kind != record.DecisionDesignChangesRequested && d.Kind != record.DecisionDesignApproved {
		return nil
	}
	if gate == nil || gate.Step.OrSpec() != d.Step.OrSpec() || !gated {
		return nil
	}
	held := personApproval(*gate)
	if d.Kind == record.DecisionDesignApproved {
		if d.Basis && gate.OnBasis() {
			held = &Approval{Version: *gate.ApprovedVersion}
		}
		artifact, version := d.Artifact, d.Version
		if artifact == "" {
			artifact = gate.ArtifactID
		}
		if version == 0 && held != nil {
			version = held.Version
		}
		if gate.ArtifactID == artifact && held != nil && held.Version == version {
			return nil
		}
		return &Supersession{By: personApproval(*gate)}
	}
	if held == nil || d.Artifact != "" && gate.ArtifactID != d.Artifact {
		return nil
	}
	version := d.Version
	if version == 0 {
		version = held.Version
	}
	if !classify.ReviewSuperseded(*gate, version, d.Mark) {
		return nil
	}
	return &Supersession{By: held}
}

// personApproval is the person's approval gate records (record.DesignGate.ApprovedByPerson), nil
// where it records none: the version a basis opened the step on is no person's approval, so no
// sender names it as one.
func personApproval(gate record.DesignGate) *Approval {
	approved := gate.ApprovedByPerson()
	if approved == nil {
		return nil
	}
	return &Approval{Version: approved.Version}
}

// StepApprovalStands is the predicate's answer for an approval of version to of the document
// artifact on step, asked of step's gate in gates: whether it is still the word that stands on
// that version. It is the one ask of the two senders that hold such an approval by its step, not
// by a Dispatch reference: the news of an approved version, of each item a send of the task that
// carries it tells, from the gates of the item's tree (the daemon's itemStands), and the approval
// a re-admission kept, from the gates of the root it was kept for, at each start or catch-up that
// may name it (KeptDecisionsTask; artifact is "" for a pair kept before record.KeptApproval named
// its document, and such a pair is asked of whichever document the gate holds). Each is asked as
// an approval of step, and as gated: the gate recorded the approval a news item tells, so a person
// approves that step, and KeptDecisionsTask asks only of a step a person approves
// (record.TakesStep). basis says the approval may be the one an ancestor's approval opened the
// step on (Decision.Basis): a news item's may, since the news tells a version a basis opened as it
// tells one a person approved (the workflow's basisWords), and the kept approval is a person's
// (admit's keepStaleApproval). It reads no fact of the tree beside that word: whether the tree
// still holds a gate row for step to tell an audience of is the news's to read, beside what the
// audience's told row holds.
func StepApprovalStands(gates map[record.DesignStep]record.DesignGate, step record.DesignStep,
	artifact string, to int, basis bool) bool {
	d := Decision{Kind: record.DecisionDesignApproved, Step: step, Artifact: artifact, Version: to, Basis: basis}
	return SupersedingApproval(gateOf(gates, step), true, d) == nil
}

// RefSuperseded is SupersedingApproval for a sender that holds only the review's Dispatch reference,
// ref on issue (record.ParkedDecision.Ref, record.Notice.Ref): the step whose document ref names
// (refStep) is resolved once here and set as d.Step, and SupersedingApproval is asked of that step's
// gate in gates, the gates of issue, and of whether a person approves that step under steps, the
// steps the deployment gates (record.TakesStep: a step the deployment gates, or criteria a root
// registered beside a deployment that gates spec and not criteria, which the one approval of both
// documents opens, as the engine's gatesStep reads it). A reference that names no step's document
// (an ask's or a comment's) is asked of no gate, and stands: nil.
func RefSuperseded(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep, issue, ref string, d Decision) *Supersession {
	step, named := refStep(issue, ref)
	if !named {
		return nil
	}
	d.Step = step
	return SupersedingApproval(gateOf(gates, step), record.TakesStep(steps, step), d)
}

// NoticeSuperseded is RefSuperseded of the decision of kind a queued notice carries on issue, a
// design decision's notice or the parked-decision relay of one read as the Notice it is: its
// reference, its version, Dispatch's mark of the review (record.Notice.Review) and the document
// reviewed (record.Notice.Artifact). Readers ask through it at the daemon's relay (parkDecision),
// delivery and publish (stillOwed) and at the clear of a generation (Engine.ClearGeneration),
// so each asks the same question of the same notice.
func NoticeSuperseded(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep,
	issue string, notice record.Notice, kind record.DecisionKind) *Supersession {
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
