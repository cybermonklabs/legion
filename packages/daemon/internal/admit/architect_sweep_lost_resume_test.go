package admit

import (
	"strings"
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
		// names is what the task sent again must say: the ask or the review the person answered.
		names []string
	}{
		{name: "a question with no gate rows", waiting: record.Awaiting{Ask: "ask-q", Since: seqMark(10)},
			answer: answeredBy("ask-q", 11), names: []string{"ask-q"}},
		{name: "a question with the gate registered", waiting: record.Awaiting{Ask: "ask-q", Since: seqMark(10)},
			gate:   &record.DesignGate{Issue: root, ArtifactID: "artifact-root", LatestVersion: 2},
			answer: answeredBy("ask-q", 11), names: []string{"ask-q"}},
		{name: "a gate wait answered by a changes request", waiting: record.Awaiting{GateVersion: 4, Since: seqMark(10)},
			answer: changes, names: []string{"version 4", "name the rollback path"}},
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
			for _, want := range tc.names {
				if !strings.Contains(resumed.EntryTask, want) {
					t.Fatalf("the resume's own task %q does not name %q, so this case proves nothing", resumed.EntryTask, want)
				}
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
			for _, want := range tc.names {
				if !strings.Contains(sent, want) {
					t.Errorf("the task sent again for entry %d is %q; it does not name %q, which the resume's own task %q named: "+
						"the answer is the work this entry owes", resumed.Entry, sent, want, resumed.EntryTask)
				}
			}
			for _, untrue := range []string{workflow.ArchitectDecompositionLabel, "no message is coming"} {
				if strings.Contains(sent, untrue) {
					t.Errorf("the task sent again for entry %d is %q; it holds %q, which is false of a root whose architect has run "+
						"and whose person's answer stands", resumed.Entry, sent, untrue)
				}
			}
		})
	}
}
