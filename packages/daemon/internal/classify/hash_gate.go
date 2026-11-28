package classify

import (
	"github.com/sjawhar/legion/daemon/internal/record"
)

// GateFact is one fact of a gated document as ApplyGateFact applies it to the document's row: a
// live Dispatch event (artifact.version, artifact.digest, artifact.approved,
// artifact.changes_requested) or a read of Dispatch's approval (register_gate, the gate seed), with
// the hash-gate fields it carries (R1 and R3 of dispatch://INFRA-6065). A field Dispatch did not
// send is nil or zero, and a fact that carries neither DigestOwed nor the digest kind is a fact of
// a Dispatch that gives no verdict.
type GateFact struct {
	// Kind is what the fact is: a review (DesignGateApproved, DesignGateChangesRequested), a
	// version (DesignGateVersion) or a digest (DesignGateDigest). A read is of the review it shows:
	// the approval it records, the changes request of its latest version, or neither, a version.
	Kind DesignGateEventKind
	// Version is a review's version, a version's number or the version a digest gives its verdict
	// for; for a read, the version of the review it shows (approval.version), which is the version
	// it records as approved, never its latest.
	Version int
	// Mark is where Dispatch recorded the fact: a review's own mark, which the approval it records
	// (ApprovedMark) or the told changes request takes, and a digest's event mark, which its
	// verdict takes. A version's mark orders nothing.
	Mark record.EventMark
	// Read says the fact is a read of Dispatch rather than a live event, and Latest is the read's
	// latest_version, 0 for a live event.
	Read   bool
	Latest int
	// DigestOwed is an artifact.version's or a read's digest_owed, nil where it sent none.
	DigestOwed *bool
	// Verdict is a digest's approval_current, or a read's verdict at Latest where DigestOwed is
	// false (dispatch.Approval.Stands); it is read nowhere else. VerdictMark is a read's mark of
	// that verdict, which is never before the review the read shows; a digest's verdict takes Mark.
	Verdict     *bool
	VerdictMark record.EventMark
	// Digest is the content_sha256 of the version the verdict is for, where the fact names it (a
	// digest's content_sha256, a read's current_sha256), and ApprovedSHA256 a digest's
	// approved_sha256. Both are for a reader only (record.GateVerdict.Digest,
	// record.DesignGate.ApprovedDigest): nothing is decided from them.
	Digest         string
	ApprovedSHA256 string
}

// GateFactResult is the row after one fact (ApplyGateFact), and what the fact asks of its caller.
type GateFactResult struct {
	Gate record.DesignGate
	// SeedWanted says the fact is a review of a version below the row's latest in hash mode
	// (SeedWanted, the function): it gave no verdict and changed nothing, since only a read of
	// Dispatch (the gate seed) can say whether that approval covers the latest version. The caller
	// drops the review as stale (workflow's artifactIgnored) and queues the seed.
	SeedWanted bool
	// Settled says the row owed its digest (record.DigestOwed) before the fact, or once the fact
	// raised its latest version (a digest above it, a read of a newer one), and owes none after it:
	// the effects the owed version deferred are due now, by today's rules, whatever the standing
	// the row settled at. A return to version mode settles a row that owed its digest too.
	Settled bool
}

// ApplyGateFact applies one fact of a gated document to its row, by R3 of dispatch://INFRA-6065
// and its plan's model, and returns the row after it with what the fact asks of the caller. It
// writes through no pointer of the row it is given.
//
// A row is in hash mode while it holds Dispatch's verdict (record.DesignGate.Verdict), and in
// version mode while it holds none. On a row in version mode a fact with no digest field is
// ApplyDesignGateEvent's, so a Dispatch that sends none keeps today's rules exactly. Otherwise:
//
//   - Entry into hash mode: an artifact.version or a read that carries digest_owed, true or false,
//     or a digest, on a row in version mode first gives the row a verdict at its latest version:
//     what the version rule (record.GateOpen) answers there over the root's own approval, with
//     any basis closed (record.CloseBasis), marked by the newest review the row holds
//     (LatestReview), with no mark where it holds none; the told standing is what the version
//     rule answers over the row as it stands (Departure D4). The fact then applies by the rules
//     below, except that a verdict it carries replaces that entry verdict only at or above the
//     row's latest version, with a mark the entry verdict's is not after, and under the
//     changes-request guard; any other entering verdict is stale, and a stale digest then changes
//     nothing but the entry.
//   - Return to version mode: an artifact.version above the latest version, or a read, that
//     carries no digest_owed, on a row in hash mode clears every verdict column (the verdict,
//     ApprovedDigest, ToldOpen) and applies by today's rule; an older artifact.version with none
//     changes nothing.
//   - A version raises the latest version, which then owes its digest: nothing else moves, so no
//     basis ends, the approval stays and the told standing keeps its value.
//   - A digest gives the verdict when it is for a version above the verdict's, or for the same one
//     with a later mark (record.EventMark.After); any other digest is stale and changes nothing.
//     A digest above the latest version first raises it, as that version's artifact.version. Its
//     approved_sha256 is the row's ApprovedDigest; its approved_version writes nothing, so
//     ApprovedVersion stays the approval the daemon applied.
//   - A true verdict, a digest's or a read's, at a version whose changes request the row records
//     with no approval of that version changes nothing (Departure D5): the fill Dispatch evaluated
//     before the person's request does not overrule it. A later approval, or a newer version's
//     digest, still gives the verdict.
//   - A review keeps today's rules (ApplyDesignGateEvent), and at or above the latest version it
//     also gives the verdict at its version, true for an approval and false for a changes request,
//     in the digest's order. A review of an older version gives no verdict and changes nothing: it
//     asks for the gate seed (SeedWanted).
//   - A read records the review it shows (approval.version, never its latest version) by today's
//     rules, unless it is older than the review the row applied (of a lower version, or
//     StaleReview), and raises the latest version to its own. Where digest_owed is false it gives
//     its verdict at its latest version, marked VerdictMark, in the digest's order and under the
//     guard; where it is true it gives none.
//   - The told standing (ToldOpen) is, after each fact, what the one write rule of it gives from
//     the standing told before the fact (record.ToldOpenAfter): GateOpen of the row where no digest
//     is owed, or where the step stands open on a basis at its latest version, and the value it had
//     before the fact otherwise.
//   - An approval the row holds of another version than before, or none, takes no ApprovedDigest
//     with it: that digest named the approval the row held.
//
// No review the row holds is marked after its verdict, nor the changes request a fact applies,
// which the workflow records as told (RecordChangesTold): where a review would be, the verdict
// takes that review's mark, and keeps its version and its answer. A person's review of the
// verdict's version gives the verdict itself (R3, "A review"), so a later fact of that version
// replaces the verdict only where it comes after the newest review the row records
// (verdictTaken), whatever mark the read that recorded the review gave its own verdict.
//
// A step on an ancestor's basis (record.DesignGate.Basis) tracks the root's own verdict in its
// columns from its entry verdict on, and GateOpen does not read that verdict for a step open on
// the basis, so it neither opens nor closes it. A version that owes its digest ends no basis; a
// fact that settles the row at a version above the one the basis was registered at ends it, as
// today's newer version does (record.CloseBasis, record.EndPendingBasis). A step pending on a
// basis whose own verdict stands at or above that version ends its wait open on it, as a person's
// approval of the root's own document ends the wait. Reviews keep today's basis rules.
func ApplyGateFact(gate record.DesignGate, fact GateFact) GateFactResult {
	carriesDigest := fact.DigestOwed != nil || fact.Kind == DesignGateDigest
	review := fact.review()
	switch {
	case gate.Verdict == nil && !carriesDigest:
		return GateFactResult{Gate: ApplyDesignGateEvent(gate, fact.Kind, fact.Version, fact.Mark)}
	case gate.Verdict != nil && !carriesDigest && !review:
		return versionModeAgain(gate, fact)
	case SeedWanted(gate, fact):
		return GateFactResult{Gate: gate, SeedWanted: true}
	}
	entering := gate.Verdict == nil
	if entering {
		gate = enteredHashMode(gate)
	}
	prior := gate
	switch {
	case fact.Read:
		gate = readApplied(gate, fact, entering)
	case fact.Kind == DesignGateDigest:
		gate = digestApplied(gate, fact, entering)
	case review:
		gate = reviewApplied(gate, fact)
	default:
		gate.LatestVersion = max(gate.LatestVersion, fact.Version)
	}
	raised := (fact.Read || fact.Kind == DesignGateDigest) && gate.LatestVersion > prior.LatestVersion
	settled := (record.DigestOwed(prior) || raised) && !record.DigestOwed(gate)
	if settled {
		gate = basisEnded(prior, gate)
	}
	gate = pendingAnswered(prior, gate)
	sameVersion := func(a, b record.PersonApproval) bool { return a.Version == b.Version }
	if !samePointer(prior.ApprovedByPerson(), gate.ApprovedByPerson(), sameVersion) {
		gate.ApprovedDigest = ""
	}
	var toldChanges record.EventMark
	if fact.Kind == DesignGateChangesRequested {
		toldChanges = fact.Mark
	}
	gate = verdictAfterReviews(gate, toldChanges)
	gate.ToldOpen = record.ToldOpenAfter(record.ToldStanding(prior), gate)
	return GateFactResult{Gate: gate, Settled: settled}
}

// SeedWanted says whether fact is a review of a version below the latest of gate, a row in hash
// mode: such a review gives no verdict and changes nothing, and asks for the gate seed instead
// (GateFactResult.SeedWanted), whose read of Dispatch says whether that approval covers the
// latest version. ApplyGateFact and the workflow's drop of a stale review (applyArtifact) both
// read it.
func SeedWanted(gate record.DesignGate, fact GateFact) bool {
	return gate.Verdict != nil && fact.review() && fact.Version < gate.LatestVersion
}

// review says whether fact is a live review, an approval or a changes request, rather than a read
// of the review Dispatch shows.
func (f GateFact) review() bool {
	return !f.Read && (f.Kind == DesignGateApproved || f.Kind == DesignGateChangesRequested)
}

// versionModeAgain applies a fact that carries no digest field, and is no review, to a row in
// hash mode: an artifact.version or a read of a Dispatch that gives no verdict for the document
// (one that has not converted it, or one rolled back). An artifact.version at or below the row's
// latest version is late or replayed and changes nothing, so no old event undoes a verdict. Any
// other clears every verdict column and applies by today's rule. A row that owed its digest has
// settled: a basis its owed version would have ended under today's rule ends now (basisEnded).
func versionModeAgain(gate record.DesignGate, fact GateFact) GateFactResult {
	if !fact.Read && fact.Version <= gate.LatestVersion {
		return GateFactResult{Gate: gate}
	}
	prior := gate
	owed := record.DigestOwed(gate)
	gate.Verdict, gate.ApprovedDigest, gate.ToldOpen = nil, "", nil
	gate = ApplyDesignGateEvent(gate, fact.Kind, fact.Version, fact.Mark)
	if owed {
		gate = basisEnded(prior, gate)
	}
	return GateFactResult{Gate: gate, Settled: owed}
}

// enteredHashMode is a row in version mode as it enters hash mode, before the fact that entered
// it applies: its verdict at its latest version is what the version rule answers there over the
// root's own approval (record.GateOpen of the row with its basis closed, record.CloseBasis),
// marked by the newest review the row holds (LatestReview), no mark where it holds none, and the
// standing told is what the version rule answers over the row as it stands (Departure D4). The two
// differ for a step open on an ancestor's approval alone: the basis opened it at the version the
// root registered, which is the standing the tree was told, but no person approved that version
// of the root's own document, so its verdict is the person's approval the step kept apart
// (KeptPerson), or none, at the latest version, which the row reads once the basis ends. A
// pending step's approval is already the person's own.
func enteredHashMode(gate record.DesignGate) record.DesignGate {
	own, told := record.GateOpen(record.CloseBasis(gate)), record.GateOpen(gate)
	mark, _, _ := LatestReview(gate)
	gate.Verdict = &record.GateVerdict{Version: gate.LatestVersion, Current: own, Mark: mark}
	gate.ToldOpen = &told
	return gate
}

// readApplied applies a read of Dispatch's approval to a row in hash mode: the review it shows, by
// today's rules, unless the row has applied a newer one (a review of a higher version, or
// StaleReview); its latest version; and its verdict at that version where no digest is owed.
// entering says the read is what put the row in hash mode (verdictTaken).
func readApplied(gate record.DesignGate, fact GateFact, entering bool) record.DesignGate {
	if fact.Kind == DesignGateApproved || fact.Kind == DesignGateChangesRequested {
		if _, reviewed, ok := LatestReview(gate); !(ok && fact.Version < reviewed) && !StaleReview(gate, fact.Version, fact.Mark) {
			gate = ApplyDesignGateEvent(gate, fact.Kind, fact.Version, fact.Mark)
		}
	}
	latest := max(fact.Latest, fact.Version)
	gate.LatestVersion = max(gate.LatestVersion, latest)
	if fact.DigestOwed != nil && !*fact.DigestOwed {
		verdict := record.GateVerdict{Version: latest, Current: fact.Verdict != nil && *fact.Verdict, Digest: fact.Digest, Mark: fact.VerdictMark}
		if verdictTaken(gate, verdict, entering, true) {
			gate.Verdict = &verdict
		}
	}
	return gate
}

// digestApplied applies an artifact.digest to a row in hash mode: a digest the row takes
// (verdictTaken) raises the latest version to its own and gives its verdict and the approved
// digest it names; any other changes nothing. entering says the digest is what put the row in
// hash mode.
func digestApplied(gate record.DesignGate, fact GateFact, entering bool) record.DesignGate {
	verdict := record.GateVerdict{Version: fact.Version, Current: fact.Verdict != nil && *fact.Verdict, Digest: fact.Digest, Mark: fact.Mark}
	if !verdictTaken(gate, verdict, entering, true) {
		return gate
	}
	gate.LatestVersion = max(gate.LatestVersion, fact.Version)
	gate.Verdict = &verdict
	if fact.ApprovedSHA256 != "" {
		gate.ApprovedDigest = fact.ApprovedSHA256
	}
	return gate
}

// reviewApplied applies a person's review at or above the latest version of a row in hash mode:
// today's rules, and the verdict at its version, true for an approval and false for a changes
// request, where the row takes it (verdictTaken). A person's review is no fill, so no guard holds
// it back.
func reviewApplied(gate record.DesignGate, fact GateFact) record.DesignGate {
	gate = ApplyDesignGateEvent(gate, fact.Kind, fact.Version, fact.Mark)
	verdict := record.GateVerdict{Version: fact.Version, Current: fact.Kind == DesignGateApproved, Mark: fact.Mark}
	if verdictTaken(gate, verdict, false, false) {
		gate.Verdict = &verdict
	}
	return gate
}

// verdictTaken says whether verdict replaces the one the row holds, by R3's order: a verdict for
// a newer version, or for the same one with a later mark (record.EventMark.After). The verdict a
// row in version mode was just given as it entered hash mode (entering) is replaced only by a
// verdict at or above its version whose mark that entry verdict's is not after; an entry verdict
// with no mark, from a row that held no review, comes after nothing (Departure D4). A guarded
// verdict, a digest's or a read's, is also not taken where it is true at a version whose changes
// request the row records with no approval of that version (Departure D5).
func verdictTaken(gate record.DesignGate, verdict record.GateVerdict, entering, guarded bool) bool {
	held := gate.Verdict
	switch {
	case guarded && verdict.Current && changesRequestStands(gate, verdict.Version):
		return false
	case entering:
		return verdict.Version >= held.Version && (held.Mark == (record.EventMark{}) || !held.Mark.After(verdict.Mark))
	}
	return verdict.Version > held.Version || verdict.Version == held.Version && verdict.Mark.After(held.Mark)
}

// changesRequestStands says whether the row records a changes request of version as told and no
// approval of that version, a person's decision that a fill of the version does not overrule.
func changesRequestStands(gate record.DesignGate, version int) bool {
	if gate.ToldChangesVersion == nil || *gate.ToldChangesVersion != version {
		return false
	}
	approval := gate.ApprovedByPerson()
	return approval == nil || approval.Version != version
}

// basisEnded ends the basis of a row a fact settled at a version of the root's own document above
// the one the basis was registered at (record.RegisteredRootVersion), as today's newer version
// ends it: a step open on the basis falls back to the person's approval it kept, and a pending
// wait ends closed. prior is the row before the fact, in hash mode. The boot's reset after an
// earlier release's boot ends such a basis by the same rule (record.ResetAfterEarlierRelease).
func basisEnded(prior, gate record.DesignGate) record.DesignGate {
	if gate.Basis == nil || prior.Basis == nil || gate.LatestVersion <= record.RegisteredRootVersion(prior) {
		return gate
	}
	return record.EndPendingBasis(record.CloseBasis(gate))
}

// pendingAnswered ends the wait of a step pending on a basis whose own verdict reads it open
// (record.GateOpen) at or above the version the basis was registered at: the root's own approval
// stands, as a person's approval of the root's own document ends the wait. prior is the row before
// the fact, in hash mode.
func pendingAnswered(prior, gate record.DesignGate) record.DesignGate {
	if gate.PendingOnBasis() && record.GateOpen(gate) && gate.Verdict.Version >= record.RegisteredRootVersion(prior) {
		return record.EndPendingBasis(gate)
	}
	return gate
}

// verdictAfterReviews gives the row's verdict the mark of each review the row holds, and of the
// changes request toldChanges marks, that is marked after it (record.EventMark.After), keeping
// its version and its answer, so a later fact of the verdict's version is ordered after the
// newest review the row records (ApplyGateFact). A review with no mark is passed over.
func verdictAfterReviews(gate record.DesignGate, toldChanges record.EventMark) record.DesignGate {
	for _, review := range [...]record.EventMark{
		reviewMark(gate.ApprovedVersion, gate.ApprovedMark), reviewMark(gate.ToldChangesVersion, gate.ToldChangesMark), toldChanges,
	} {
		if review != (record.EventMark{}) && review.After(gate.Verdict.Mark) {
			verdict := *gate.Verdict
			verdict.Mark = review
			gate.Verdict = &verdict
		}
	}
	return gate
}

// reviewMark is the mark of a review the row holds at version, zero where it holds none.
func reviewMark(version *int, mark record.EventMark) record.EventMark {
	if version == nil {
		return record.EventMark{}
	}
	return mark
}

// samePointer says whether a and b point at values sameValue reads as one, a nil pointer equal to
// nil alone.
func samePointer[T any](a, b *T, sameValue func(T, T) bool) bool {
	if a == nil || b == nil || a == b {
		return a == b
	}
	return sameValue(*a, *b)
}
