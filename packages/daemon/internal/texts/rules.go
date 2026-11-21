package texts

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// This file holds the urgency rule of each notice and role-message kind that is not Med for
// every reader, and the variant words those rules read. Every other kind has no rule, and so is
// Med for every reader (UrgencyOf).

// The variant words of a child-status notice: the change of the child that it tells. The
// payload key is change (Variant.Change).
const (
	ChangeTodo            = "todo"
	ChangeStarted         = "started"
	ChangeJoined          = "joined"
	ChangeLeft            = "left"
	ChangeBeneathAccepted = "beneath-accepted"
	ChangeReparented      = "reparented"
)

// The variant words of a held notice: what held the issue. A checks-red notice of a held issue
// names what holds it with the same words. The payload key is hold (Variant.Hold).
const (
	// HoldFailure is the hold that a phase worker's failed claim puts the issue in.
	HoldFailure = "failure"
	// HoldEscalation is the hold that the architect escalated (retry_or_escalate).
	HoldEscalation = "escalation"
	// HoldCarried is the hold that admission tells a carried root's own architect of.
	HoldCarried = "carried"
)

// phaseFinishedUrgency is Low for the architect where the daemon has already started the next
// phase: the phase of an implementer, a tester or a merger, a reviewer's request for changes (a
// move to implementing), the design gate's opening (a move out of admitted) and the architect's
// own backward move out of accepting. A planner's phase and a reviewer's approval are Med, and
// so is a reviewer's phase that names no phase it moved to, as a row of an older daemon does.
func phaseFinishedUrgency(reader claim.Role, v Variant) Urgency {
	if reader != claim.RoleArchitect {
		return Med
	}
	switch {
	case v.Phase == phase.Admitted:
		return Low
	case v.Role == claim.RoleImplementer, v.Role == claim.RoleTester, v.Role == claim.RoleMerger:
		return Low
	case v.Role == claim.RoleReviewer && v.To == phase.Implementing:
		return Low
	case v.Role == claim.RoleArchitect && v.Phase == phase.Accepting:
		return Low
	}
	return Med
}

// heldUrgency is Low for the architect where the hold records the architect's own escalation,
// and Med for every other hold and for the controller.
func heldUrgency(reader claim.Role, v Variant) Urgency {
	if reader == claim.RoleArchitect && v.Hold == HoldEscalation {
		return Low
	}
	return Med
}

// childStatusUrgency is Low for the architect where a child moved to todo, joined the tree or
// starts planning, and Med for every other change.
func childStatusUrgency(reader claim.Role, v Variant) Urgency {
	if reader != claim.RoleArchitect {
		return Med
	}
	switch v.Change {
	case ChangeTodo, ChangeJoined, ChangeStarted:
		return Low
	}
	return Med
}

// lowToArchitect is Low for the architect and Med for every other reader: pr-merged.
func lowToArchitect(reader claim.Role, _ Variant) Urgency {
	if reader == claim.RoleArchitect {
		return Low
	}
	return Med
}

// checksRedUrgency is the checks-red notice's rule, read from the phase the issue stays in
// (Variant.Phase) and what holds the issue (Variant.Hold). For the architect it is Low while a
// worker holds that phase, since the daemon has already asked the implementer to merge the base
// forward and told that worker to hold, and Med wherever no worker holds it: where the architect
// holds it itself (accepting), where it must not sign off the conflicting head, where no role
// does, and where the issue is held, which stays in the phase its hold came from while no worker
// works it, as the CI settlement's rule reads a held issue (ciSettledUrgency). It is Med for every
// other reader.
func checksRedUrgency(reader claim.Role, v Variant) Urgency {
	if worker := phase.RoleFor(v.Phase); reader == claim.RoleArchitect && v.Hold == "" && worker != "" && worker != claim.RoleArchitect {
		return Low
	}
	return Med
}

// lowToController is Low for the controller and Med for every other reader: awaiting-person,
// which is no reason to label or admit an issue.
func lowToController(reader claim.Role, _ Variant) Urgency {
	if reader == claim.RoleController {
		return Low
	}
	return Med
}

// ciSettledUrgency is the CI settlement's rule, read from the issue's phase (Variant.Phase). For
// the architect it is Low while a worker holds the phase, and Med where none does (admitted,
// accepting, held, awaiting_person). For a phase role it is Med where that role works the phase
// and Low otherwise.
func ciSettledUrgency(reader claim.Role, v Variant) Urgency {
	worker := phase.RoleFor(v.Phase)
	if reader == claim.RoleArchitect {
		if worker != "" && worker != claim.RoleArchitect {
			return Low
		}
		return Med
	}
	if reader == worker {
		return Med
	}
	return Low
}
