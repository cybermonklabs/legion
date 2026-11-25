package workflow

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// architectRootPromptLine is the one line of the root architect's go/architect-root.md that starts
// with prefix, read from disk as texts_steps_test.go reads the prompts.
func architectRootPromptLine(t *testing.T, prefix string) string {
	t.Helper()
	body, err := os.ReadFile("../prompts/go/architect-root.md")
	if err != nil {
		t.Fatalf("read the root architect's prompt: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("go/architect-root.md has no line starting %q", prefix)
	return ""
}

// Main's writeGateStanding (workflow/architect_start.go at d46ec4e0) told a root whose design gate
// stands part way to "claim it in Dispatch, carry that gate on from where it stands and take the
// tree up from there." dispatch://INFRA-2614's R3 (approved v11, done, held by main) has "a root
// that holds design gate rows is told where its gate stands and to take the tree up from there",
// and INFRA-3686's R8 keeps it: "a fact or a step that it has the text give stays as a field". So
// the gate-standing task must still give that step: as the first act the prompt glosses as taking
// the tree up (take_up_tree), or in the gloss of the act it does carry (carry_gate).
func TestAGateStandingTaskTakesTheTreeUpFromThere(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 2, Phase: phase.Admitted}
	start := ArchitectStart{Root: root, Gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec},
		Rows: map[record.DesignStep]record.DesignGate{
			record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1}}}}
	facts, served := ArchitectStartFacts(start)
	if !served || facts.Shape != texts.ShapeGateStanding {
		t.Fatalf("shape = %q (served %t), want %s", facts.Shape, served, texts.ShapeGateStanding)
	}
	gloss := architectRootPromptLine(t, "- `carry_gate`:")
	if !slices.Contains(facts.FirstAct, FirstActTakeUpTree) && !strings.Contains(gloss, "take the tree up") {
		t.Fatalf("the gate-standing task gives no step to take the tree up from where its gate stands:\n%s\n"+
			"first_act = %v; the prompt's gloss of its act is %q", ArchitectStartTask(start), facts.FirstAct, gloss)
	}
}

// Main's writeCarriedPhase (workflow/architect_start.go at d46ec4e0) told a carried root that it
// "stands in %s" and to "resume the tree from that phase": the phase it stands in now, which its
// comment says is the one that came with it or one of that run it has moved to since, "so every
// clause here holds of either" (the branch's carriedPhase keeps "so every fact here holds of
// either"). INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where
// the prompt does not give it". The prompt's gloss of resume_phase must therefore name the phase
// the root stands in (the task's `phase`), not the phase the hand-over carried it in with, which
// is false of a root that moved on.
func TestTheResumePhaseGlossNamesThePhaseTheRootStandsIn(t *testing.T) {
	moved := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 2, Phase: phase.Testing}
	start := ArchitectStart{Root: moved, Carried: true, GateCarried: true,
		Gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}}}
	facts, served := ArchitectStartFacts(start)
	if !served || facts.Shape != texts.ShapeCarriedPhase || !slices.Contains(facts.FirstAct, FirstActResumePhase) {
		t.Fatalf("facts = %+v (served %t), want a carried-phase task with resume_phase", facts, served)
	}
	gloss := architectRootPromptLine(t, "- `resume_phase`:")
	if strings.Contains(gloss, "carried the root in with") || !(strings.Contains(gloss, "`phase`") || strings.Contains(gloss, "stands in")) {
		t.Fatalf("a root carried in at implementing that moved on to testing gets\n%s\nand the prompt glosses its act as %q: "+
			"that names the phase the hand-over carried it in with, not the phase it stands in", ArchitectStartTask(start), gloss)
	}
}
