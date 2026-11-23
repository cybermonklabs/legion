package workflow

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// verificationDoc is the root's Verification document in these tests: a document of the root's
// issue that no gate is registered on, from which its change request reads its checks.
const verificationDoc = "the-roots-verification"

// checksRead is what Dispatch read of the root's change request at spec version spec, its checks
// taken from version verification of the Verification document: whether the version proposes
// nothing, and whether a check it changes is one no person approved.
func checksRead(spec, verification int, proposesNothing, unapproved bool) *intake.ChangeRequestRead {
	return &intake.ChangeRequestRead{Version: spec, VerificationVersion: verification, SpecSource: true, ProposesNothing: proposesNothing,
		ChecksUnapproved: unapproved}
}

// ofVerification is an event of the root's Verification document of kind at version, carrying
// read, the change request the prepare stage read for it at the gate's spec version.
func ofVerification(kind intake.DispatchArtifactKind, version int, seq int64, read *intake.ChangeRequestRead) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: gatedRoot, ArtifactID: verificationDoc, Kind: kind, Version: version, Mark: record.EventMark{Seq: seq},
		ChangeRequest: read}
}

// verificationClosed is the opening of the closed-gate message a version of the Verification
// document that changes an approved check sends (R25).
func verificationClosed(version string) string {
	return "The design gate of " + gatedRoot + " closed: version " + version + " of its Verification document changes an approved check, " +
		"and no person approved the change."
}

// A version of the Verification document is a version of the change request too (R25): its read
// is applied to the root's spec gate under the rule of a change request. One that changes nothing
// leaves the exempt gate open, tells nobody and reads the ending condition again; one that changes
// an approved check revokes the exemption and holds the gate, and the merger and the architect are
// told it closed, naming the Verification document. A read older than the Verification version the
// gate holds changes nothing, a later spec version read at an older Verification version opens
// nothing, and a later spec version read at the newest one opens by exemption, with the messages
// and the notice of today's opening.
func TestAVerificationVersionThatChangesAnApprovedCheckRevokesTheExemption(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)

	apply("first-verification", ofVerification(intake.DispatchArtifactVersion, 1, 21, checksRead(4, 1, true, false)))
	if gate := specGateOf(t, pool); !record.GateExempt(gate) || gate.VerificationVersion != 1 || gate.ChecksUnapproved {
		t.Fatalf("the gate after a Verification version that changes nothing = %+v, want it exempt at 4, read at Verification version 1, holding nothing", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages after a Verification version that changes nothing = %q, want none", moved)
	}
	if got, want := endingReads(t, pool, gatedRoot), []record.SpecEndingRead{{Head: head, Cause: record.SpecEndingCauseVersion}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ending reads after a Verification version that changes nothing = %+v, want %+v", got, want)
	}

	apply("second-verification", ofVerification(intake.DispatchArtifactVersion, 2, 22, checksRead(4, 2, false, true)))
	gate := specGateOf(t, pool)
	if record.GateOpen(gate) || gate.ExemptVersion != nil || gate.ApprovedVersion != nil || gate.VerificationVersion != 2 || !gate.ChecksUnapproved {
		t.Fatalf("the gate after a Verification version that changes an approved check = %+v; want it closed, its exemption revoked, "+
			"read at Verification version 2 and holding the changed check", gate)
	}
	moved := roleMessageBodies(t, pool, gatedRoot)
	if len(moved) != 2 || !strings.HasPrefix(moved[0], verificationClosed("2")) || !strings.HasPrefix(moved[1], verificationClosed("2")) {
		t.Fatalf("role messages after a Verification version that changes an approved check = %q; want the merger and the architect told %q",
			moved, verificationClosed("2"))
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 1 || !strings.HasPrefix(merger[0], verificationClosed("2")) {
		t.Fatalf("the merger's messages = %q, want the closed gate of the Verification document", merger)
	}
	if got := endingReads(t, pool, gatedRoot); len(got) != 1 {
		t.Fatalf("ending reads after the gate closed = %+v, want the one read before: a closed gate holds the merger itself", got)
	}

	// A read made before the Verification version the gate holds changes nothing.
	apply("first-verification-late", ofVerification(intake.DispatchArtifactApproved, 1, 23, checksRead(4, 1, true, false)))
	if late := specGateOf(t, pool); record.GateOpen(late) || late.VerificationVersion != 2 || !late.ChecksUnapproved {
		t.Fatalf("the gate after a read older than its Verification version = %+v, want it as it stood: closed at Verification version 2", late)
	}

	// A spec version read at an older Verification version opens nothing.
	apply("fifth-spec", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
		Mark: record.EventMark{Seq: 24}, ChangeRequest: checksRead(5, 1, true, false)})
	if stale := specGateOf(t, pool); record.GateOpen(stale) || stale.ExemptVersion != nil || stale.LatestVersion != 5 || stale.VerificationVersion != 2 {
		t.Fatalf("the gate after a spec version read at an older Verification version = %+v, want it closed at 5, still at Verification version 2", stale)
	}
	if told := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(told) != 0 {
		t.Fatalf("design-approved notices after a stale read = %+v, want none", told)
	}

	// A spec version read at the newest Verification version, which changes nothing, opens by
	// exemption as a version always has.
	apply("sixth-spec", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 6,
		Mark: record.EventMark{Seq: 26}, ChangeRequest: checksRead(6, 3, true, false)})
	opened := specGateOf(t, pool)
	if !record.GateExempt(opened) || opened.VerificationVersion != 3 || opened.ChecksUnapproved || opened.ApprovedVersion != nil {
		t.Fatalf("the gate after a spec version read at Verification version 3 that proposes nothing = %+v, want it exempt at 6, holding nothing", opened)
	}
	moved = roleMessageBodies(t, pool, gatedRoot)
	if len(moved) != 4 || !strings.HasPrefix(moved[2], "The design gate of "+gatedRoot+" opened with no person's approval: version 6") {
		t.Fatalf("role messages after the opening by exemption = %q, want the opening with no person told", moved)
	}
	told := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(told) != 1 || told[0].Actor != "" || told[0].Version != 6 || !strings.HasPrefix(told[0].Reason, "no proposal: version 6 of the change request") {
		t.Fatalf("design-approved notices after the opening by exemption = %+v, want one at 6 naming nobody", told)
	}
}

// approvedMergingRoot records the root in merging with its pull request at head and its spec gate
// opened by a person's approval of version 4 under the rule of a change request, read at version 1
// of its Verification document.
func approvedMergingRoot(t *testing.T, head string) (*pgxpool.Pool, func(id string, fact intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	mergingExemptRoot(t, pool, head)
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ApprovedVersion: &four, ApprovedMark: record.EventMark{Seq: 12}, VerificationVersion: 1, EverOpened: true})
	return pool, applyFacts(t, pool, specGateEngine())
}

// A person's approval of a change request covers the checks its Verification document held then
// (R25: "A changed approved check waits for the person"). A later Verification version that
// changes only an acceptance check, which names no statement, changes nothing; one that changes an
// approved check closes the gate the person's approval opened, though the approval is kept, and the
// merger and the architect are told it closed, naming the Verification document. The person's
// approval of a request that covers the change — the read that finds the approved spec holding the
// check as written — opens it again, and a read made before that approval, of the same
// Verification version, holds nothing again.
func TestAVerificationVersionThatChangesAnApprovedCheckClosesAPersonsApproval(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool, apply := approvedMergingRoot(t, head)

	apply("acceptance-check", ofVerification(intake.DispatchArtifactVersion, 2, 21, checksRead(4, 2, true, false)))
	if gate := specGateOf(t, pool); !record.GateApproved(gate) || !record.GateOpen(gate) || gate.VerificationVersion != 2 || gate.ChecksUnapproved {
		t.Fatalf("the gate after a Verification version that changes an acceptance check = %+v, want it open by the person's approval at Verification version 2", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages after an acceptance check changed = %q, want none", moved)
	}

	apply("approved-check", ofVerification(intake.DispatchArtifactVersion, 3, 22, checksRead(4, 3, false, true)))
	gate := specGateOf(t, pool)
	if record.GateOpen(gate) || record.GateApproved(gate) || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 4 || gate.VerificationVersion != 3 ||
		!gate.ChecksUnapproved {
		t.Fatalf("the gate after a Verification version that changes an approved check = %+v; want it closed, the approval of 4 kept and "+
			"standing on nothing, read at Verification version 3", gate)
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 1 || !strings.HasPrefix(merger[0], verificationClosed("3")) {
		t.Fatalf("the merger's messages = %q, want the closed gate of the Verification document", merger)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 2 || !strings.HasPrefix(moved[1], verificationClosed("3")) {
		t.Fatalf("role messages = %q, want the merger and the architect told the gate closed", moved)
	}

	// The person approves the spec with its Verification document: the approval writes the check
	// into the approved spec, so the read of the Verification document's approval holds nothing.
	apply("verification-approved", ofVerification(intake.DispatchArtifactApproved, 3, 23, checksRead(4, 3, false, false)))
	if reopened := specGateOf(t, pool); !record.GateApproved(reopened) || !record.GateOpen(reopened) || reopened.ChecksUnapproved {
		t.Fatalf("the gate after the person's approval that covers the change = %+v, want it open by that approval", reopened)
	}
	merger := mergerEndingMessages(t, pool)
	if len(merger) != 2 || !strings.HasPrefix(merger[1], "The design gate of "+gatedRoot+" opened: a person approved version 4") {
		t.Fatalf("the merger's messages = %q, want the person's opening after the closed gate", merger)
	}

	// A read of the same Verification version made before that approval holds nothing again.
	apply("approved-check-late", ofVerification(intake.DispatchArtifactVersion, 3, 24, checksRead(4, 3, false, true)))
	if late := specGateOf(t, pool); !record.GateOpen(late) || late.ChecksUnapproved {
		t.Fatalf("the gate after a late read of the same Verification version = %+v, want it open: only a newer Verification version holds a check", late)
	}
	if got := mergerEndingMessages(t, pool); !slices.Equal(got, merger) {
		t.Fatalf("the merger's messages after the late read = %q, want them as they were", got)
	}
}

// R24: a gate under the rule of a person's approval — a repository with no spec source, or an
// approval recorded before the rule of a change request — keeps its standing whatever a
// Verification version says: the read is no input of that gate.
func TestAVerificationVersionLeavesTheGateOfAPersonsApprovalAsItStands(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	four := 4
	approved := record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		ApprovedVersion: &four, ApprovedMark: record.EventMark{Seq: 12}, EverOpened: true}
	seedGate(t, pool, approved)

	apply("approved-check", ofVerification(intake.DispatchArtifactVersion, 2, 21, checksRead(4, 2, false, true)))
	gate := specGateOf(t, pool)
	if !record.GateApproved(gate) || gate.Rule != record.GateRuleApproval || gate.VerificationVersion != 0 || gate.ChecksUnapproved {
		t.Fatalf("the gate under the rule of a person's approval after a Verification version = %+v, want it as it stood: %+v", gate, approved)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages = %q, want none", moved)
	}
}

// A registration of the version the gate stands exempt at reads the Verification document again:
// a read that finds an approved check changed revokes the exemption and holds the gate, as the
// Verification version's own event does, and the merger is told the gate closed.
func TestARegistrationAtTheExemptVersionReadsTheVerificationDocumentAgain(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &four, VerificationVersion: 1, EverOpened: true})

	apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4,
		Since: record.EventMark{Seq: 30}, ChangeRequest: checksRead(4, 2, false, true)})
	gate := specGateOf(t, pool)
	if record.GateOpen(gate) || gate.ExemptVersion != nil || gate.VerificationVersion != 2 || !gate.ChecksUnapproved {
		t.Fatalf("the gate after a registration whose read finds an approved check changed = %+v; want it closed, its exemption revoked", gate)
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 1 || !strings.HasPrefix(merger[0], verificationClosed("2")) {
		t.Fatalf("the merger's messages = %q, want the closed gate of the Verification document", merger)
	}
}
