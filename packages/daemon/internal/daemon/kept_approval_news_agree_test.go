package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A kept approval (the start's and the catch-up's, workflow.KeptDecisionsTask) and a news item
// (each send's, itemStands) both name a person's approval of a version of a document, and one
// predicate decides whether that approval is still the person's word (workflow.SupersedingApproval,
// dispatch://INFRA-3136/spec R2). Driven through the same gate states, the two agree on every
// state of the person's word: a changes request that cleared the approval, a later approved
// version, and a move of the step to another document each end it; the same version approved
// again under a new mark, and a new version that closed the gate, which keeps the approval below
// the latest, leave it standing.
//
// The last row is a fact of the tree, not a second rule of the person's word. A clear of the
// generation deleted the gate row: nothing recorded since can be the person's later word, so the
// kept approval is named and the news's ask of the predicate stands too, but the news drops the
// item, since a gate that is gone holds no approval to tell an audience of.
func TestAKeptApprovalAndANewsItemAgreeOnThePersonsWord(t *testing.T) {
	t.Parallel()
	const (
		tree     = "LEGION-208"
		document = "artifact-208"
		sentence = "is approved at version 2"
	)
	gate := record.DesignGate{Issue: tree, Step: record.DesignStepSpec, ArtifactID: document, LatestVersion: 2}
	approved := classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, 2, record.EventMark{Seq: 10})
	cleared := classify.ApplyDesignGateEvent(approved, classify.DesignGateChangesRequested, 2, record.EventMark{Seq: 11})
	closed := classify.ApplyDesignGateEvent(approved, classify.DesignGateVersion, 3, record.EventMark{})
	moved := approved
	moved.ArtifactID = "artifact-209"
	for _, tc := range []struct {
		name string
		gate *record.DesignGate
		// word is the predicate's answer for both: the approval of version 2 of document stands.
		word bool
		// told is whether the news tells the item: the word stands and the tree holds a gate row.
		told bool
	}{
		{name: "the gate still holds the approval", gate: &approved, word: true, told: true},
		{name: "a later changes request cleared it", gate: &cleared},
		{name: "a later approval of a new version replaced it",
			gate: new(classify.ApplyDesignGateEvent(closed, classify.DesignGateApproved, 3, record.EventMark{Seq: 12}))},
		{name: "the person approved the same version again under a new mark",
			gate: new(classify.ApplyDesignGateEvent(cleared, classify.DesignGateApproved, 2, record.EventMark{Seq: 12})),
			word: true, told: true},
		{name: "the step's gate moved to another document", gate: &moved},
		{name: "a new version closed the gate, which keeps the approval below the latest", gate: &closed, word: true, told: true},
		{name: "a fact of the tree: a clear of the generation left no gate row", word: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gates := map[record.DesignStep]record.DesignGate{}
			if tc.gate != nil {
				gates[record.DesignStepSpec] = *tc.gate
			}
			owed := record.KeptDecisions{KeptApproval: record.KeptApproval{Step: record.DesignStepSpec, Artifact: document,
				Approved: 2, Latest: 3}}
			task, kept := workflow.KeptDecisionsTask(workflow.KeptGates{tree: gates}, []record.DesignStep{record.DesignStepSpec},
				tree, owed)
			if named := strings.Contains(task, sentence); named != tc.word || (kept.Approved > 0) != tc.word {
				t.Fatalf("the kept approval's task %q names %q = %t and keeps approval %d, want named %t",
					task, sentence, named, kept.Approved, tc.word)
			}
			if word := workflow.StepApprovalStands(gates, record.DesignStepSpec, document, 2); word != tc.word {
				t.Fatalf("the news asks the predicate of the item and hears %t, want %t, as the kept approval hears it", word, tc.word)
			}
			item := record.DeliveryNewsItem{Issue: tree, Audience: record.AudienceArchitect, Step: record.DesignStepSpec,
				Artifact: document, From: new(1), To: 2}
			told := record.DesignTold{Issue: tree, Audience: record.AudienceArchitect,
				Versions: map[record.DesignStep]int{record.DesignStepSpec: 1}}
			if stands := itemStands(item, gates, told); stands != tc.told {
				t.Fatalf("itemStands = %t, want %t", stands, tc.told)
			}
		})
	}
}
