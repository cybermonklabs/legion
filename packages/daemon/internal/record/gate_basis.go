package record

import "time"

// The rules of a step that an ancestor's approval opens, or that waits on an ancestor's open
// approval request (dispatch://INFRA-3320, R1 to R3), written once here, where both the store's
// reader and internal/classify reach them. Each transition takes the gate as the caller holds it
// and returns the gate after the move, as classify.ApplyDesignGateEvent does, and none writes
// through a pointer of the gate it is given, so a caller that compares the two reads what moved.

// OnBasis says whether a basis stands open on the step: an ancestor's approval opened it, and no
// new version of the issue's own document, review of it at the version registered or later, or
// re-registration has ended it since.
func (g DesignGate) OnBasis() bool {
	return g.Basis != nil && g.Basis.Pending == nil
}

// PendingOnBasis says whether the step waits, closed, on an ancestor's open approval request.
func (g DesignGate) PendingOnBasis() bool {
	return g.Basis != nil && g.Basis.Pending != nil
}

// ApprovedByPerson is the person's approval of the issue's own document the step records, nil for
// none: KeptPerson while a basis stands, otherwise the approval ApprovedVersion and ApprovedMark
// record. A basis sets ApprovedVersion to a version no person approved, so every reader that names
// a person's approval, or compares a review with the newest one the step holds
// (classify.LatestReview), reads this rather than ApprovedVersion. The value is a copy.
func (g DesignGate) ApprovedByPerson() *PersonApproval {
	if g.OnBasis() {
		if g.KeptPerson == nil {
			return nil
		}
		kept := *g.KeptPerson
		return &kept
	}
	if g.ApprovedVersion == nil {
		return nil
	}
	return &PersonApproval{Version: *g.ApprovedVersion, Mark: g.ApprovedMark}
}

// RestsOnIssue says whether the step rests on ancestor in this generation (R8): a basis on
// ancestor opened it at some point (RestsOn), or its basis names ancestor now, standing or
// pending. A pending wait that ended without opening leaves no rest of its own.
func (g DesignGate) RestsOnIssue(ancestor string) bool {
	return ancestor != "" && (g.RestsOn == ancestor || g.Basis != nil && g.Basis.Issue == ancestor)
}

// openedAt is the moment the step opened, as the waiting line orders it (TreeGate.OpenedAt): the
// daemon's own time of the registration for a step a basis opened, never zero, and where Dispatch
// recorded the person's approval otherwise.
func (g DesignGate) openedAt() time.Time {
	if g.OnBasis() {
		return g.Basis.RegisteredAt
	}
	return g.ApprovedMark.At
}

// OpenOnBasis opens the step on basis, an ancestor's approval, at the version of the issue's own
// document the gate holds as its latest, which is the version the root registered: ApprovedVersion
// becomes that version with no ApprovedMark, so GateOpen, TreeGate.Open, EverOpened and the
// merger's start read the step open as after a person's approval. A person's approval of an
// earlier version the step held moves into KeptPerson; a basis that already stood keeps the
// approval it kept. The first ancestor a basis opens the step on is the one it rests on for the
// generation (RestsOn). A step a person's approval holds open at its latest version stays on
// that approval, which no basis outranks (R3), and is returned as it is.
func OpenOnBasis(gate DesignGate, basis GateBasis) DesignGate {
	if !gate.OnBasis() && GateOpen(gate) {
		return gate
	}
	if !gate.OnBasis() {
		gate.KeptPerson = gate.ApprovedByPerson()
	}
	basis.Pending = nil
	gate.Basis = &basis
	registered := gate.LatestVersion
	gate.ApprovedVersion, gate.ApprovedMark = &registered, EventMark{}
	if gate.RestsOn == "" {
		gate.RestsOn = basis.Issue
	}
	return gate
}

// PendOnBasis records that the step waits, closed, on ask, the ancestor's open request for a
// person's approval of basis's version: basis carries no approval yet, so its Approval and
// Approver are cleared. A basis that stood is closed first (CloseBasis), so the step falls back to
// the person's own approval it kept. The step rests on the ancestor through the pending basis
// alone (RestsOnIssue), so a wait that ends without opening leaves no rest behind. A step a
// person's approval holds open at its latest version waits on nothing, as OpenOnBasis leaves it.
func PendOnBasis(gate DesignGate, basis GateBasis, ask string) DesignGate {
	gate = CloseBasis(gate)
	if GateOpen(gate) {
		return gate
	}
	basis.Approval, basis.Approver = EventMark{}, ""
	basis.Pending = &PendingBasis{Ask: ask}
	gate.Basis = &basis
	return gate
}

// OpenPendingBasis opens a step that waits on an ancestor's request on the person's approval
// that answered it, at approval in the ancestor's event log and by approver, as an approved
// basis registered then would (OpenOnBasis): the registration's own time stays the opening's
// moment. A step that waits on nothing is returned as it is.
func OpenPendingBasis(gate DesignGate, approval EventMark, approver string) DesignGate {
	if !gate.PendingOnBasis() {
		return gate
	}
	basis := *gate.Basis
	basis.Approval, basis.Approver = approval, approver
	return OpenOnBasis(gate, basis)
}

// CloseBasis ends the basis a step stands open on: ApprovedVersion and ApprovedMark fall back to
// the person's own approval the step kept (KeptPerson), or to none, and never to the version the
// basis opened. RestsOn stays. A step no basis stands on is returned as it is.
func CloseBasis(gate DesignGate) DesignGate {
	if !gate.OnBasis() {
		return gate
	}
	gate.ApprovedVersion, gate.ApprovedMark = nil, EventMark{}
	if kept := gate.KeptPerson; kept != nil {
		version := kept.Version
		gate.ApprovedVersion, gate.ApprovedMark = &version, kept.Mark
	}
	gate.Basis, gate.KeptPerson = nil, nil
	return gate
}

// EndPendingBasis ends the wait of a step pending on an ancestor's request without opening it: the
// step stays closed and registered on the issue's own document with no basis, and stops resting on
// that ancestor unless a basis on it opened the step earlier (RestsOn). A step that waits on
// nothing is returned as it is.
func EndPendingBasis(gate DesignGate) DesignGate {
	if gate.PendingOnBasis() {
		gate.Basis = nil
	}
	return gate
}

// basisAsWritten is gate as this release reads a row whose basis a release before the basis
// moved: such a release names none of the basis columns, so a write of its own leaves them on a
// row whose approval it changed. A standing basis is only ever written at the step's latest
// version with no mark of a person's (OpenOnBasis), so a row read otherwise is the earlier
// release's move, and is read as this release's rule would have made it: a changes request (no
// approval) or a person's approval (a mark) ended the basis and the approval it kept; a newer
// version (an approval below the latest) closed it, falling back to the kept approval rather than
// to the version the basis opened. A pending row a person's approval opened at its latest version
// has ended its wait. The next write of the row clears what is left (Store.PutGate).
func basisAsWritten(gate DesignGate) DesignGate {
	switch {
	case gate.PendingOnBasis() && GateOpen(gate):
		gate.Basis = nil
	case !gate.OnBasis():
	case gate.ApprovedVersion == nil, gate.ApprovedMark != (EventMark{}):
		gate.Basis, gate.KeptPerson = nil, nil
	case *gate.ApprovedVersion != gate.LatestVersion:
		gate = CloseBasis(gate)
	}
	return gate
}
