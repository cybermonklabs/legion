package prompts

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergerGateTexts are the design gate's texts a merger reads, as the daemon writes them: the
// closing message, which is also the note of every task the closed gate holds; the note of a task
// sent while the gate stands open; and the opening message (workflow.MergerGateNote and
// workflow.MergerGateMessage, the texts tellGateMoved sends a merger). The tree, the step, the
// document and the version are written as the merger parts quote them: <tree>, <step>, <id> and
// <n>.
func mergerGateTexts(t *testing.T) (closed, open, opened string) {
	t.Helper()
	issue := record.Issue{Key: "<tree>", Tree: "<tree>", Phase: phase.Merging}
	gate := func(approved int) record.TreeGate {
		return record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}, Rows: map[record.DesignStep]record.DesignGate{
			record.DesignStepSpec: {Issue: "<tree>", Step: record.DesignStepSpec, ArtifactID: "<id>", LatestVersion: 7, ApprovedVersion: &approved}}}
	}
	quoted := strings.NewReplacer("version 7 ", "version <n> ", "its spec document", "its <step> document")
	closed = quoted.Replace(workflow.MergerGateNote(issue, claim.RoleMerger, gate(6)).Text)
	open = workflow.MergerGateNote(issue, claim.RoleMerger, gate(7)).Text
	opened = quoted.Replace(workflow.MergerGateMessage(issue, claim.RoleMerger, gate(7)).Text)
	// A daemon text this test could not turn into the quoted form would check nothing below.
	const named = "version <n> of its <step> document (<id>)"
	if !strings.Contains(closed, named) || !strings.Contains(opened, named) || !strings.HasPrefix(open, "The design gate of <tree> stands open") {
		t.Fatalf("the daemon's gate texts read %q, %q and %q, want each in the form the merger parts quote", closed, open, opened)
	}
	return closed, open, opened
}

// Both merger parts tell the merger how the closed design gate holds it and how the daemon reaches
// it (dispatch://INFRA-2040 R5, R7 and R8; c-merger-prompt). Each says that each task from the
// daemon opens with the gate's state at its send, quoting the closed form and the open form as the
// daemon writes them; that the closed form holds the merge as the closing message does and the
// open form says that the gate does not hold it; that the merger reads its hold in `read_record`
// before it merges or enqueues; that a merger the closed gate holds declares no wait and is not
// reminded while the gate stays closed, except for a tool call written as text; and that the
// daemon wakes a merger that was away at the gate's close and at its opening. The role part quotes
// the closing message and the opening message whole, the opening message naming every gated step
// that someone registered.
//
// Neither part says what R7 and R8 made false: that the daemon starts no merger while the gate is
// closed, that a merger the daemon has not started waits for the gate in its own start row, or
// that the opening comes once every step the deployment gates stands open. The texts are read
// folded, since a sentence can cross a line break.
func TestBothMergerPartsSayHowTheClosedDesignGateHoldsAndReachesTheMerger(t *testing.T) {
	closed, open, opened := mergerGateTexts(t)
	closedWords := closed[:strings.Index(closed, " is with its reviewer.")+len(" is with its reviewer.")]
	goMerger, err := goParts.ReadFile("go/merger.md")
	if err != nil {
		t.Fatalf("read go/merger.md: %v", err)
	}
	both := []string{
		"`" + closedWords,
		"`" + open + "`",
		"Each task from the daemon starts with the state of the gate at the time the daemon sent",
		"It holds the merge as the closing message does.",
		"tells you that the gate does not hold the merge",
		"read your hold with the `legion` tool's `read_record` for your issue.",
		"`workers.merger.claim.hold`",
		"merge nothing and enqueue nothing.",
		"While the closed gate holds you, do not declare a wait.",
		"While the gate stays closed, no reminder follows a turn that ends with your phase open.",
		"The one exception is a turn that ends with a tool call written as text.",
		"If you were away when the gate closed or opened, the daemon wakes you with a task.",
	}
	retired := []string{
		"While it is closed the daemon starts no merger",
		"A merger the daemon has not started yet waits for the gate in the daemon's own start row",
		"so a closed gate you are told of is one that closed while you were running",
		"every step the deployment gates now stands open",
	}
	for _, part := range []struct {
		name string
		body string
		says []string
	}{
		{name: "roles/merger.md", body: rolePart(t, "merger.md"), says: append([]string{"`" + closed + "`", "`" + opened + "`",
			"Before you merge (step 3) or enqueue (step 4)"}, both...)},
		{name: "go/merger.md", body: string(goMerger), says: append([]string{"Before you merge or enqueue"}, both...)},
	} {
		text := foldWhitespace(part.body)
		for _, want := range part.says {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", part.name, want)
			}
		}
		for _, gone := range retired {
			if strings.Contains(text, gone) {
				t.Errorf("%s still says %q", part.name, gone)
			}
		}
	}
}
