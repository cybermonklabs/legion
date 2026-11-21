package texts

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Urgency is the urgency that a notice or a role message goes with, in the envelope's urgency
// field.
type Urgency string

const (
	// Low is the urgency of a text whose reader can act on it at its next turn.
	Low Urgency = "low"
	// Med is the urgency of every other notice and role message.
	Med Urgency = "med"
)

// Variant holds the facts of one text that an urgency rule reads, beside the kind and the
// reader. A notice's variant comes from the notice's own row, so a copy of the row gets the
// urgency of the row it copies. The json names are the payload keys the facts come from, so
// json.Unmarshal of a notice's payload gives its variant.
type Variant struct {
	// Role is the worker role that the notice names.
	Role claim.Role `json:"role,omitempty"`
	// Phase is the phase that the text names: the phase that ended, or the issue's phase.
	Phase phase.Phase `json:"phase,omitempty"`
	// To is the phase that a phase-finished notice's move went to.
	To phase.Phase `json:"to,omitempty"`
	// Change is the variant word of a child-status notice (ChangeTodo and the others).
	Change string `json:"change,omitempty"`
	// Hold is the variant word of a held notice (HoldFailure and the others), and what holds the
	// held issue a checks-red notice is of.
	Hold string `json:"hold,omitempty"`
}

// UrgencyOf is the urgency of a text of kind to reader, with the variant v. A task has no
// urgency, so it gets the empty urgency. A notice or a role message gets what its entry's rule
// gives, and Med where its entry has no rule. A kind that is not on the list gets Med, the
// urgency of a text that nothing makes low.
func UrgencyOf(kind Kind, reader claim.Role, v Variant) Urgency {
	entry, ok := Lookup(kind)
	if ok && entry.Form == Task {
		return ""
	}
	if !ok || entry.Urgency == nil {
		return Med
	}
	return entry.Urgency(reader, v)
}
