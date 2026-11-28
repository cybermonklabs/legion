package record

import (
	"slices"
	"time"
)

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
// ancestor opened it at some point (RestsOn, which names each ancestor a basis opened it on), or
// its basis names ancestor now, standing or pending. A pending wait that ended without opening
// leaves no rest of its own.
func (g DesignGate) RestsOnIssue(ancestor string) bool {
	return ancestor != "" && (slices.Contains(g.RestsOn, ancestor) || g.Basis != nil && g.Basis.Issue == ancestor)
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
// document its registration stood at (GateBasis.RootVersion): the version the root registered, the
// gate's latest, unless basis names one already, as a pending basis a person's approval of the
// request opens (OpenPendingBasis) names the version its pending registration stood at.
// ApprovedVersion becomes that version with no ApprovedMark, so GateOpen, TreeGate.Open, EverOpened
// and the merger's start read the step open as after a person's approval, and the root version
// of a step open on a basis is its ApprovedVersion in either release (RegisteredRootVersion). A
// person's approval of an earlier version the step held moves into KeptPerson; a basis that
// already stood keeps the approval it kept. The step rests on basis's ancestor for the generation
// from then on, beside each ancestor an earlier basis opened
// it on (RestsOn). A step a person's own approval holds (heldByPerson) stays on that approval,
// which no basis outranks (R3), and is returned as it is, so a version of the issue's own document
// that owes its digest does not move a standing approval into KeptPerson. That guard reads no told
// standing (ToldStanding): a row whose basis ended, or turned pending, can keep the standing that
// basis told (ToldOpenAfter), which says nothing of a person's approval, so a basis registered or
// opened over that row is set. The caller sets the told standing after the move (ToldOpenAfter).
func OpenOnBasis(gate DesignGate, basis GateBasis) DesignGate {
	if heldByPerson(gate) {
		return gate
	}
	if !gate.OnBasis() {
		gate.KeptPerson = gate.ApprovedByPerson()
	}
	basis.Pending = nil
	if basis.RootVersion == 0 {
		basis.RootVersion = gate.LatestVersion
	}
	gate.Basis = &basis
	registered := basis.RootVersion
	gate.ApprovedVersion, gate.ApprovedMark = &registered, EventMark{}
	if !slices.Contains(gate.RestsOn, basis.Issue) {
		// Clipped, so the append never writes into the backing array of the caller's gate.
		gate.RestsOn = append(slices.Clip(gate.RestsOn), basis.Issue)
	}
	return gate
}

// PendOnBasis records that the step waits, closed, on ask, the ancestor's open request for a
// person's approval of basis's version: basis carries no approval yet, so its Approval and
// Approver are cleared, and it records the gate's latest version as the one its registration
// stood at (GateBasis.RootVersion). A basis that stood is closed first (CloseBasis), so the step
// falls back to the person's own approval it kept. The step rests on the ancestor through the
// pending basis alone (RestsOnIssue), so a wait that ends without opening leaves no rest behind. A
// step a person's own approval holds (heldByPerson) waits on nothing, as OpenOnBasis leaves it, so
// a version that owes its digest leaves that approval standing; a step a basis stood open on
// waits on nothing where, that basis closed, the version rule or the verdict at the latest
// version reads it open (GateOpen).
func PendOnBasis(gate DesignGate, basis GateBasis, ask string) DesignGate {
	if heldByPerson(gate) {
		return gate
	}
	gate = CloseBasis(gate)
	if GateOpen(gate) {
		return gate
	}
	basis.Approval, basis.Approver = EventMark{}, ""
	basis.Pending = &PendingBasis{Ask: ask}
	basis.RootVersion = gate.LatestVersion
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

// RegisteredRootVersion is the version of the root's own document the basis of gate was
// registered at. A step open on the basis reads it from its ApprovedVersion, which OpenOnBasis
// sets to that version in this release and in the release before, and which a version that owes
// its digest leaves where it is (Departure D2 of dispatch://INFRA-6065). A step pending on the
// basis reads GateBasis.RootVersion. A pending basis written before the migration that added that
// column records none: on a row in version mode it is the latest version, which no version of its
// own document moved without ending the basis, and on a row in hash mode it is read at its
// verdict's version, which is where a version-mode row entered hash mode. classify.ApplyGateFact
// reads it of a row in hash mode with a basis, and the workflow of any row, in its telling of an
// end by the root's own document and its opening of a pending step.
func RegisteredRootVersion(gate DesignGate) int {
	switch {
	case gate.OnBasis() && gate.ApprovedVersion != nil:
		return *gate.ApprovedVersion
	case gate.Basis != nil && gate.Basis.RootVersion > 0:
		return gate.Basis.RootVersion
	case gate.Verdict == nil:
		return gate.LatestVersion
	}
	return gate.Verdict.Version
}

// heldByPerson says whether a person's own approval of the issue's own document holds the step,
// which no basis outranks (R3): OpenOnBasis and PendOnBasis leave such a step as it is. A step a
// basis stands open on is held by that basis, not by a person. Any other step is held, in version
// mode, where GateOpen reads it open, an approval of the latest version; in hash mode, where
// Dispatch's verdict on the root's own document is that the approval stands (GateVerdict.Current),
// which the row keeps under a basis too. Where no digest is owed that is the verdict at the latest
// version, so an approval of an older version that Dispatch reports current holds the step and a
// changes request does not; inside a fill window (DigestOwed) it is the standing of the person's
// approval at the verdict's version, so a version that owes its digest leaves a standing approval
// holding it. It reads no told standing (ToldOpen): the standing a basis told stays on the row
// after that basis ends or turns pending, and says nothing of a person's approval.
func heldByPerson(gate DesignGate) bool {
	switch {
	case gate.OnBasis():
		return false
	case gate.Verdict == nil:
		return GateOpen(gate)
	}
	return gate.Verdict.Current
}

// basisAsWritten is gate as this release reads a row whose basis a release before the basis
// moved: such a release names none of the basis columns, so a write of its own leaves them on a
// row whose approval it changed. A standing basis is only ever written at the step's latest
// version with no mark of a person's (OpenOnBasis), so a row read otherwise is the earlier
// release's move, and is read as this release's rule would have made it: a changes request (no
// approval) or a person's approval (a mark) ended the basis and the approval it kept; a newer
// version (an approval below the latest) closed it, falling back to the kept approval rather than
// to the version the basis opened. That last reading holds only where no digest is owed
// (DigestOwed): a version of the issue's own document that owes its digest raises the latest
// version under this release too, and leaves the basis for the digest to settle. A pending row a
// person's approval opened at its latest version, or whose own verdict reads it open, has ended
// its wait. The next write of the row clears what is left (Store.PutGate).
func basisAsWritten(gate DesignGate) DesignGate {
	switch {
	case gate.PendingOnBasis() && (approvedAtLatest(gate) || GateOpen(gate)):
		gate.Basis = nil
	case !gate.OnBasis():
	case gate.ApprovedVersion == nil, gate.ApprovedMark != (EventMark{}):
		gate.Basis, gate.KeptPerson = nil, nil
	case *gate.ApprovedVersion != gate.LatestVersion && !DigestOwed(gate):
		gate = CloseBasis(gate)
	}
	return gate
}
