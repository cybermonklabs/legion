package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Decision is a person's review of a design document as a sender holds it when it asks whether
// the review is still the person's word (DecisionStands): what the person did (Kind), on which
// issue (Issue) and on which step's document (Ref, the reference stepRef gives it), the document
// itself when the sender knows it (Artifact, "" when it does not), the version reviewed when the
// sender knows it (Version, 0 when it does not: a kept decision's row records none), and where
// Dispatch recorded the review (Mark, empty where the sender holds no mark of it).
type Decision struct {
	Kind     record.DecisionKind
	Issue    string
	Ref      string
	Artifact string
	Version  int
	Mark     record.EventMark
}

// DecisionStands is the one place that decides whether a person's recorded review of a design
// document is still their word, so a task or a notice may name it. Every sender asks it, from the
// gates of d.Issue as the record holds them when it asks (gates, by step), with the steps the
// deployment gates (steps): the changes notice (tellChanges, and the registration's and the
// artifact event's reading of the gate before they apply the review), the resume of a root a
// person answered (resumeTask), the decisions and the approval kept for an architect's start or
// catch-up (KeptDecisionsTask, again at each send and each delivery) and the controller's relay of
// a decision nobody in its tree hears (the daemon's parkDecision). It holds every guard that
// chooses whether the one rule is asked (classify.ReviewSuperseded), so a new step kind or a
// change to how an approval of the same version is read is made here once.
//
// It reads the gate of the step whose document d.Ref names. A review stands when nothing the gate
// records can be the person's later word: a decision that is no changes request or approval (an
// answer on an ask, a carried gate's opening), a document no gate of the issue holds, a gate that
// now holds another document than the one reviewed, a step the deployment does not gate (its
// approval is its registration's and no person's), and a gate with no approval. Otherwise the
// gate is asked whether its approval followed the review: at d.Version, or, where the sender
// knows no version, at the version the gate holds approved — a review of a lower one is answered
// by that approval, and one of that version is placed by its mark — and at d.Mark. Both marks are
// Dispatch's, so the two are ordered by Dispatch's sequences when both carry one and otherwise by
// its times (record.EventMark.After), never by the daemon's clock against Dispatch's. An approval
// the gate's approval is (the same version, and a mark that can be the same event:
// record.EventMark.Same) is no later word than itself, so it stands.
func DecisionStands(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep, d Decision) bool {
	if d.Kind != record.DecisionDesignChangesRequested && d.Kind != record.DecisionDesignApproved {
		return true
	}
	for step, gate := range gates {
		if stepRef(d.Issue, step) != d.Ref {
			continue
		}
		if !slices.Contains(steps, step) || gate.ApprovedVersion == nil || d.Artifact != "" && gate.ArtifactID != d.Artifact {
			return true
		}
		version := d.Version
		if version == 0 {
			version = *gate.ApprovedVersion
		}
		if d.Kind == record.DecisionDesignApproved && version == *gate.ApprovedVersion && d.Mark.Same(gate.ApprovedMark) {
			return true
		}
		return !classify.ReviewSuperseded(gate, version, d.Mark)
	}
	return true
}
