package workflow

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The third input of a change request: the approved spec of the issue's repository, which another
// issue's approval or an import writes while the issue's own documents stand (R22, R25). Dispatch
// names it on each read (approved_spec_mark) and tells of each write (spec.approved_written); the
// gate keeps the mark of the read it stands on, so a read of an older approved spec changes
// nothing, and the daemon reads every open or held gate of that repository again.

// specRead is what Dispatch read of the root's change request at spec version spec, its checks
// from version verification of the Verification document, against the approved spec mark: whether
// it proposes nothing, and whether a changed check or a proposal is one no person approved.
func specRead(spec, verification int, mark int64, proposesNothing, checks, proposals bool) *intake.ChangeRequestRead {
	return &intake.ChangeRequestRead{Version: spec, VerificationVersion: verification, ApprovedSpecMark: mark, SpecSource: true,
		ProposesNothing: proposesNothing, ChecksUnapproved: checks, ProposesUnapproved: proposals}
}

// approvedSpecClosed is the opening of the closed-gate message a read of a newer approved spec
// sends when the request no longer holds.
const approvedSpecClosed = "The design gate of " + gatedRoot + " closed: the approved spec of sjawhar/legion changed under its change request"

// gateReads is the cause of every read of a spec gate's change request the outbox holds for issue,
// in order.
func gateReads(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'cause' from outbox where kind = 'spec_gate_read' and issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the spec gate reads of %s: %v", issue, err)
	}
	defer rows.Close()
	causes := []string{}
	for rows.Next() {
		var cause string
		if err := rows.Scan(&cause); err != nil {
			t.Fatalf("read a spec gate read of %s: %v", issue, err)
		}
		causes = append(causes, cause)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the spec gate reads of %s: %v", issue, err)
	}
	return causes
}

// exemptAtMark seeds the merging root's spec gate open by exemption at version 4, read at
// Verification version 1 against the approved spec at mark.
func exemptAtMark(t *testing.T, pool *pgxpool.Pool, mark int64) {
	t.Helper()
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &four, VerificationVersion: 1, ApprovedSpecMark: mark, EverOpened: true})
}

// An approval elsewhere of the root's repository queues one read of the root's open spec gate,
// and none of a gate that stands closed for want of a person, nor of a gate of another
// repository, nor of a gate under the rule of a person's approval: only a gate that stands open,
// or that a hold closed, can move on a read of a newer approved spec. The repository is matched
// whatever the case Dispatch spells it in.
func TestAnApprovalElsewhereQueuesAReadOfEachGateItsRepositoryHolds(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	seed := func(key, repo string, gate record.DesignGate) {
		t.Helper()
		seedIssue(t, pool, record.Issue{Key: key, Repo: repo, DefaultBranch: "main", GitHubRepo: repo, Tree: key, Project: "LEGION",
			Title: "another root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "V", HandedOver: true, LastDispatchSeq: 1})
		gate.Issue, gate.Step, gate.ArtifactID, gate.LatestVersion = key, record.DesignStepSpec, "spec-"+key, 2
		seedGate(t, pool, gate)
	}
	two := 2
	seed("LEGION-209", "sjawhar/legion", record.DesignGate{Rule: record.GateRuleChangeRequest})
	seed("LEGION-210", "acme/tools", record.DesignGate{Rule: record.GateRuleChangeRequest, ExemptVersion: &two})
	seed("LEGION-211", "sjawhar/legion", record.DesignGate{ApprovedVersion: &two})

	apply("approved-elsewhere", intake.ApprovedSpecWritten{Key: "LEGION-300", Seq: 5, Repo: "sjawhar/legion", Act: "approval"})
	if got := gateReads(t, pool, gatedRoot); !slices.Equal(got, []string{string(record.SpecGateCauseApprovedSpec)}) {
		t.Fatalf("the reads of the open gate of %s after an approval elsewhere = %v, want one of cause %s", gatedRoot, got, record.SpecGateCauseApprovedSpec)
	}
	for _, key := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		if got := gateReads(t, pool, key); len(got) != 0 {
			t.Fatalf("the reads of the gate of %s after an approval elsewhere = %v, want none", key, got)
		}
	}
	apply("imported-elsewhere", intake.ApprovedSpecWritten{Key: "LEGION-301", Seq: 6, Repo: "SJawhar/Legion", Act: "import"})
	if got := gateReads(t, pool, gatedRoot); len(got) != 2 {
		t.Fatalf("the reads of the open gate after a second write spelled in another case = %v, want two", got)
	}
}

// A read of a newer approved spec is applied to the exempt gate: one that changes nothing moves
// the gate's mark, tells nobody and reads the ending condition again, since the approved spec is an
// input of the conclusion's judgment too; one that finds a changed check no person approved takes
// the exemption back and holds the gate, and the merger and the architect are told it closed,
// naming the approved spec. A read of an older approved spec delivered after it — another gate
// read, or a registration whose read raced the approval — changes nothing. A later read that finds
// the check held as written takes the hold away and opens nothing with no person.
func TestAReadOfANewerApprovedSpecClosesTheExemptGateItsRequestNoLongerHolds(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	exemptAtMark(t, pool, 10)

	apply("unrelated", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 11, true, false, false)})
	if gate := specGateOf(t, pool); !record.GateExempt(gate) || gate.ApprovedSpecMark != 11 {
		t.Fatalf("the gate after a read of a newer approved spec that changes nothing = %+v, want it exempt, read at mark 11", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages after a read that changes nothing = %q, want none", moved)
	}
	if got, want := endingReads(t, pool, gatedRoot), []record.SpecEndingRead{{Head: head, Cause: record.SpecEndingCauseApprovedSpec}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ending reads after a read of a newer approved spec = %+v, want %+v", got, want)
	}

	apply("changed-check", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 12, false, true, false)})
	gate := specGateOf(t, pool)
	if record.GateOpen(gate) || gate.ExemptVersion != nil || !gate.ChecksUnapproved || gate.ApprovedSpecMark != 12 || gate.VerificationVersion != 1 {
		t.Fatalf("the gate after a read of a newer approved spec that changes the check the request holds = %+v; want it closed, "+
			"its exemption revoked, held by the changed check, read at mark 12", gate)
	}
	moved := roleMessageBodies(t, pool, gatedRoot)
	if len(moved) != 2 || !strings.HasPrefix(moved[0], approvedSpecClosed) || !strings.HasPrefix(moved[1], approvedSpecClosed) {
		t.Fatalf("role messages after the approved spec moved under the request = %q; want the merger and the architect told %q", moved, approvedSpecClosed)
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 1 || !strings.HasPrefix(merger[0], approvedSpecClosed) {
		t.Fatalf("the merger's messages = %q, want the closed gate of the approved spec", merger)
	}

	// Two reads of the older approved spec, delivered after the newer one, change nothing.
	apply("older-gate-read", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 11, true, false, false)})
	apply("raced-registration", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4,
		Since: record.EventMark{Seq: 30}, ChangeRequest: specRead(4, 1, 11, true, false, false)})
	if late := specGateOf(t, pool); record.GateOpen(late) || late.ExemptVersion != nil || !late.ChecksUnapproved || late.ApprovedSpecMark != 12 {
		t.Fatalf("the gate after reads of an older approved spec = %+v, want it as the newer read left it: closed, held, at mark 12", late)
	}
	if got := roleMessageBodies(t, pool, gatedRoot); !slices.Equal(got, moved) {
		t.Fatalf("role messages after reads of an older approved spec = %q, want them as they were", got)
	}

	// A newer approved spec that holds the check as the request writes it takes the hold away; the
	// request still changes a check, so nothing opens with no person.
	apply("held-as-written", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 13, false, false, false)})
	if released := specGateOf(t, pool); record.GateOpen(released) || released.ChecksUnapproved || released.ApprovedSpecMark != 13 {
		t.Fatalf("the gate after a read that finds the check held as written = %+v, want the hold taken away and the gate still closed", released)
	}
	if got := roleMessageBodies(t, pool, gatedRoot); !slices.Equal(got, moved) {
		t.Fatalf("role messages after the hold was taken away = %q, want no opening told", got)
	}
}

// A person's approval of a change request covers the nodes it proposed as they were. A read of a
// newer approved spec that no longer holds a proposal as the request writes it — another approval
// changed that statement — holds the gate the person's approval opened, though the approval is
// kept, and the merger is told it closed, naming the approved spec. A read of a still newer
// approved spec that holds it as written again takes the hold away, and the person's approval
// opens the gate again.
func TestAReadOfANewerApprovedSpecHoldsTheGateAPersonsApprovalOpened(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool, apply := approvedMergingRoot(t, head)

	apply("changed-statement", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 12, false, false, true)})
	gate := specGateOf(t, pool)
	if record.GateOpen(gate) || record.GateApproved(gate) || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 4 || !gate.ProposesUnapproved ||
		gate.ApprovedSpecMark != 12 {
		t.Fatalf("the gate after a read that finds a proposal the approved spec no longer holds = %+v; want it closed, the approval of 4 kept "+
			"and standing on nothing, held by the proposal, at mark 12", gate)
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 1 || !strings.HasPrefix(merger[0], approvedSpecClosed) {
		t.Fatalf("the merger's messages = %q, want the closed gate of the approved spec", merger)
	}

	apply("held-again", intake.SpecGateRead{Issue: gatedRoot, Read: specRead(4, 1, 13, false, false, false)})
	if reopened := specGateOf(t, pool); !record.GateApproved(reopened) || !record.GateOpen(reopened) || reopened.ProposesUnapproved {
		t.Fatalf("the gate after a read that finds the proposal held as written again = %+v, want it open by the person's approval", reopened)
	}
	if merger := mergerEndingMessages(t, pool); len(merger) != 2 || !strings.HasPrefix(merger[1], "The design gate of "+gatedRoot+" opened: a person approved version 4") {
		t.Fatalf("the merger's messages = %q, want the person's opening after the closed gate", merger)
	}
}

// A read newer in one input and older in another — a version of the spec, or of the Verification
// document, new to the gate, read against an older approved spec than the read the gate stood on —
// is the newest read of that version, so it is applied; but the approved spec it saw is older, so
// the gate's change request is read again (cause older_read), and that read decides.
func TestAReadOfANewVersionAgainstAnOlderApprovedSpecIsReadAgain(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	exemptAtMark(t, pool, 12)

	apply("fifth-spec", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
		Mark: record.EventMark{Seq: 24}, ChangeRequest: specRead(5, 1, 10, true, false, false)})
	if gate := specGateOf(t, pool); gate.LatestVersion != 5 || !record.GateExempt(gate) || gate.ApprovedSpecMark != 10 {
		t.Fatalf("the gate after a new version read against an older approved spec = %+v, want it exempt at 5, read at mark 10", gate)
	}
	if got := gateReads(t, pool, gatedRoot); !slices.Equal(got, []string{string(record.SpecGateCauseOlderRead)}) {
		t.Fatalf("the reads queued after a new version read against an older approved spec = %v, want one of cause %s", got, record.SpecGateCauseOlderRead)
	}

	apply("second-verification", ofVerification(intake.DispatchArtifactVersion, 2, 25, specRead(5, 2, 9, true, false, false)))
	if gate := specGateOf(t, pool); gate.VerificationVersion != 2 || gate.ApprovedSpecMark != 9 {
		t.Fatalf("the gate after a newer Verification version read against an older approved spec = %+v, want Verification version 2 at mark 9", gate)
	}
	if got := gateReads(t, pool, gatedRoot); len(got) != 2 {
		t.Fatalf("the reads queued = %v, want a second of cause %s", got, record.SpecGateCauseOlderRead)
	}
	// A read that moves no mark back queues nothing.
	apply("same-mark", ofVerification(intake.DispatchArtifactVersion, 3, 26, specRead(5, 3, 9, true, false, false)))
	if got := gateReads(t, pool, gatedRoot); len(got) != 2 {
		t.Fatalf("the reads queued after a read that moved no mark back = %v, want the two before", got)
	}
}

// R24 for the spec source itself: removing a repository's spec source (Dispatch then answers
// spec_source false and approved_spec_mark 0, and appends no event) is a change of the rule, and a
// gate that started under the rule of a change request keeps it and stands on the read it holds. A
// read of the same versions with no spec source compares against no approved spec, which is older
// than the gate's, so it changes nothing, whether a version of another document or a registration
// carries it. A new version of the spec is read fresh, and a read with no spec source exempts
// nothing, so the gate closes at that version and waits for a person, as today's gate does.
func TestARemovedSpecSourceLeavesTheGateOnTheReadItStandsOn(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, head)
	exemptAtMark(t, pool, 10)
	noSource := func(version int) *intake.ChangeRequestRead {
		return &intake.ChangeRequestRead{Version: version, VerificationVersion: 1, ProposesNothing: true}
	}

	apply("verification-after-removal", ofVerification(intake.DispatchArtifactVersion, 1, 21, noSource(4)))
	apply("registration-after-removal", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4,
		Since: record.EventMark{Seq: 30}, ChangeRequest: noSource(4)})
	if gate := specGateOf(t, pool); !record.GateExempt(gate) || gate.ApprovedSpecMark != 10 || gate.Rule != record.GateRuleChangeRequest {
		t.Fatalf("the gate after reads with no spec source at its own versions = %+v, want it exempt as it stood, at mark 10, under the rule of a change request", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages after reads with no spec source at the gate's versions = %q, want none", moved)
	}

	apply("fifth-spec-after-removal", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
		Mark: record.EventMark{Seq: 31}, ChangeRequest: noSource(5)})
	if gate := specGateOf(t, pool); record.GateOpen(gate) || gate.LatestVersion != 5 || (gate.ExemptVersion != nil && *gate.ExemptVersion == 5) ||
		gate.Rule != record.GateRuleChangeRequest {
		t.Fatalf("the gate after a new version read with no spec source = %+v, want it closed at 5 for a person, under the rule it started with", gate)
	}
}
