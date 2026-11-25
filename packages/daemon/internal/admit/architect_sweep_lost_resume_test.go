package admit

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The resume of a root a person answered raises its architect's entry and writes, as the work of
// that entry, the task that names the answer: which ask or which review, and where to read it
// (workflow.Engine.ResumeAwaitedRoot). When that resume's start row is lost, the sweep sends the
// entry again, and the work of the entry is the answer: an architect sent a task that does not
// name it has lost the work it owes (dispatch://INFRA-2614, R4, and the architect's reading at
// dispatch://INFRA-2614/comment/03dd9487-b44d-4508-bc0e-268fe951b17e).
//
// The task sent again says nothing false of the root either. Its architect has run in this
// generation, so the first act (a spec to write and children to file) is not what it owes (R3),
// and a person's answer stands on the root, so "no message is coming" is false of it.
//
// Three waits are answered here: a question with no design gate rows, a question on a root whose
// gate is registered and unanswered, and a gate wait answered by a person's changes request.
func TestTheArchitectSweepResendsWhatALostResumeCarried(t *testing.T) {
	const root = "LEGION-100"
	changes := intake.DispatchArtifact{Key: root, ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path", Mark: seqMark(11)}
	for _, tc := range []struct {
		name    string
		waiting record.Awaiting
		gate    *record.DesignGate
		answer  intake.Fact
		// lines are what the task sent again must hold: the act, with the ask or the review the
		// person answered.
		lines []string
	}{
		{name: "a question with no gate rows", waiting: record.Awaiting{Ask: "ask-q", Since: seqMark(10)},
			answer: answeredBy("ask-q", 11), lines: []string{"act: ask", "ask: " + askRef("ask-q")}},
		{name: "a question with the gate registered", waiting: record.Awaiting{Ask: "ask-q", Since: seqMark(10)},
			gate:   &record.DesignGate{Issue: root, ArtifactID: "artifact-root", LatestVersion: 2},
			answer: answeredBy("ask-q", 11), lines: []string{"act: ask", "ask: " + askRef("ask-q")}},
		{name: "a gate wait answered by a changes request", waiting: record.Awaiting{GateVersion: 4, Since: seqMark(10)},
			answer: changes, lines: []string{"act: changes_requested", "version: 4", "name the rollback path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			awaitingRoot(t, pool, tc.waiting)
			if tc.gate != nil {
				seedGate(t, pool, *tc.gate)
			}
			putArchitectRow(t, pool, root, seededEntry, handedBackAt)
			token := putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateIdle, "")
			markGivenEntry(t, pool, token, seededEntry)

			applyFact(t, pool, admission, "answer", tc.answer)
			if got := issue(t, pool, root); got.Phase != phase.Admitted || got.Awaiting != nil {
				t.Fatalf("%s after the person's answer stands in %s waiting on %+v, want it resumed at admitted", root, got.Phase, got.Awaiting)
			}
			resumed := architectRow(t, pool, root)
			if !hasLines(resumed.EntryTask, tc.lines...) {
				t.Fatalf("the resume's own task %q does not hold the lines %q, so this case proves nothing", resumed.EntryTask, tc.lines)
			}

			// The lane finishes the resume's start row without the task reaching the claim, and the
			// sweep is the one thing left that sends the raised entry's work.
			runStart(t, pool, root)
			applySweep(t, pool, admission, "resume-lost")
			starts := architectSweepStarts(t, pool, root)
			if len(starts) != 1 || starts[0].entry != resumed.Entry {
				t.Fatalf("the architect starts of %s after the sweep = %+v, want one at the entry %d the resume raised", root, starts, resumed.Entry)
			}
			sent := starts[0].task
			if !hasLines(sent, tc.lines...) {
				t.Errorf("the task sent again for entry %d is %q; it does not hold the lines %q, which the resume's own task %q held: "+
					"the answer is the work this entry owes", resumed.Entry, sent, tc.lines, resumed.EntryTask)
			}
			for _, untrue := range []string{workflow.ArchitectDecompositionLabel, "nothing_else_starts_it: true"} {
				if hasLines(sent, untrue) {
					t.Errorf("the task sent again for entry %d is %q; it holds %q, which is false of a root whose architect has run "+
						"and whose person's answer stands", resumed.Entry, sent, untrue)
				}
			}
		})
	}
}

// A person's approval that opens the design gate a root waits on resumes the root and, in the
// same pass, promotes it past admitted to its first phase (admit's record.LineAnswered branch, then
// the line's LineFirstPhase once the records are read again). The resume raised the architect's
// entry with its own text as the work, marked to be sent as written, and queued the architect's
// start carrying that entry (workflow.Engine.ResumeAwaitedRoot). When that start row is lost, the
// root stands in planning, where a phase role owns the tree's work, and the entry its architect's
// row names is still the resume's, one its claim was never given. The sweep's reach keeps such a
// root while that entry stands unsent (workflow.ArchitectAtWork), so the next pass writes the
// architect's start at that entry, carrying the resume's own text: a lost row loses none of what
// the person's answer owes the architect (dispatch://INFRA-2614, R4).
//
// The control is the same pass a second time, while the start it wrote is still queued: a start
// on its way is not written twice (workflow.ArchitectStartPending).
func TestTheArchitectSweepResendsALostResumeOfARootTheAnswerPromotedPastAdmitted(t *testing.T) {
	const root = "LEGION-100"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: seqMark(10)})
	putArchitectRow(t, pool, root, seededEntry, handedBackAt)
	token := putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateIdle, "")
	markGivenEntry(t, pool, token, seededEntry)

	applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: root, ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(11)})

	if got := issue(t, pool, root); got.Phase != phase.Planning || got.Awaiting != nil {
		t.Fatalf("%s after the approval stands in %s waiting on %+v, want it resumed and promoted to planning in the same pass: the case's premise",
			root, got.Phase, got.Awaiting)
	}
	resumed := architectRow(t, pool, root)
	if resumed.Entry != seededEntry+1 || !resumed.EntryVerbatim || !hasLines(resumed.EntryTask, "act: approved", "version: 4") {
		t.Fatalf("the architect row of %s after the resume = entry %d carrying %q, sent as written %t; want entry %d carrying the resume's "+
			"own task, which says the spec was approved, marked to be sent as written", root, resumed.Entry, resumed.EntryTask,
			resumed.EntryVerbatim, seededEntry+1)
	}
	if starts := architectSweepStarts(t, pool, root); len(starts) != 1 || starts[0].entry != resumed.Entry {
		t.Fatalf("the architect starts of %s after the resume = %+v, want the one of entry %d the resume queued", root, starts, resumed.Entry)
	}

	// The resume's start row is lost: nothing of it reaches the claim, which stands at the entry
	// the wait began on.
	if _, err := pool.Exec(context.Background(), `delete from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $2`, root, string(claim.RoleArchitect)); err != nil {
		t.Fatalf("delete the architect start of %s: %v", root, err)
	}
	if starts := architectSweepStarts(t, pool, root); len(starts) != 0 {
		t.Fatalf("the architect starts of %s after the delete = %+v, want none", root, starts)
	}

	applySweep(t, pool, admission, "resume-lost-past-admitted")

	starts := architectSweepStarts(t, pool, root)
	if len(starts) != 1 || starts[0].entry != resumed.Entry {
		t.Fatalf("the architect starts of %s after the sweep = %+v, want one at the entry %d the resume raised: the resume's work "+
			"is owed to the architect of a root in planning as much as at admitted", root, starts, resumed.Entry)
	}
	if starts[0].task != resumed.EntryTask {
		t.Errorf("the task sent again for entry %d is %q, want the resume's own %q: the answer is the work this entry owes",
			resumed.Entry, starts[0].task, resumed.EntryTask)
	}

	applySweep(t, pool, admission, "resume-lost-past-admitted-again")

	if again := architectSweepStarts(t, pool, root); len(again) != 1 || again[0] != starts[0] {
		t.Fatalf("the architect starts of %s after a second sweep = %+v, want only the %+v the first wrote: that row is still on its way",
			root, again, starts[0])
	}
}
