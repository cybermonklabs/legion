package prompts

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// sequenceNamed is the library's sequence of that name. A name the library no longer holds ends
// the test, since every reading below would then check a zero sequence and pass on nothing.
func sequenceNamed(t *testing.T, name string) phase.Sequence {
	t.Helper()
	sequence, ok := phase.SequenceFor(name)
	if !ok {
		t.Fatalf("the library holds no sequence %q; it holds %q", name, phase.SequenceNames())
	}
	return sequence
}

// The line a started worker's task carries names its issue's workflow, then each phase of it in
// the order the issue walks them with the role that works it (dispatch://INFRA-3570). The default
// gets no line at all, so a default issue's task reads word for word as it did before the library
// existed. The lines are written out in full rather than built the way WorkflowLine builds them,
// so a change to how it builds them shows here as a changed line.
func TestTheWorkflowLineNamesEachPhaseInOrderAndTheDefaultHasNone(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{phase.DefaultSequenceName, ""},
		{"light", "Workflow: light, in order: implementing (implementer), reviewing (reviewer), merging (merger)."},
		{"no-planner", "Workflow: no-planner, in order: implementing (implementer), testing (tester), reviewing (reviewer), " +
			"accepting (architect), retro (implementer), merging (merger)."},
	} {
		if got := WorkflowLine(sequenceNamed(t, tc.name)); got != tc.want {
			t.Errorf("the workflow line of %s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every phase worker and the merger is told how to read the line (go/worker-common.md): by the
// label WorkflowLine starts it with, and that a task carrying none is of an issue on the default
// workflow. That second sentence is what keeps a default issue's prompt true beside a task that
// carries no line, and the first is what ties the prompts to the one place the line is written:
// a label spelled one way in the line and another in the prompt would leave every worker reading
// for a line no task carries.
func TestEveryComposedWorkerPromptReadsTheWorkflowLineByItsLabel(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	label := "`" + WorkflowLabel + "` line"
	if line := WorkflowLine(sequenceNamed(t, "light")); !strings.HasPrefix(line, WorkflowLabel+" ") {
		t.Fatalf("the light workflow's line %q does not start with the label %q the prompts read", line, WorkflowLabel)
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		text := composedText(t, composer, role, false)
		for _, want := range []string{
			"The task of an issue on any other workflow carries a " + label,
			"A task with no " + label + " is of an issue on the `default` workflow.",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the composed %s prompt omits %q", role, want)
			}
		}
	}
}

// The merger verifies, before it merges, the phases its own issue's workflow ran before merging
// and no other (dispatch://INFRA-3570): step 1 of its part names the default workflow's phases and
// tells it to verify instead those a `Workflow:` line lists before `merging` wherever its task
// names one, and that line is WorkflowLine's. So what the merger of an issue is told to verify is
// step 1's own words for an issue whose task names no line, and the line's phases before merging
// for one whose task names a line. A light issue's names no tester and no retro. A default issue's
// names both, which is the control: it shows the reading below sees the two words where they are,
// so the light case's silence is the line's and not a reading that found nothing.
func TestTheMergerVerifiesOnlyThePhasesItsIssuesWorkflowRanBeforeTheMerge(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := composedText(t, composer, claim.RoleMerger, false)
	const opens, closes = "1. Verify ", ", then read the pull request:"
	start, end := strings.Index(text, opens), strings.Index(text, closes)
	if start < 0 || end < start {
		t.Fatalf("the composed merger prompt has no step 1 from %q to %q", opens, closes)
	}
	// Without the instruction that sends the merger to the line, a light issue's merger would
	// verify the default's phases whatever its task names.
	defaultWords, pointer, found := strings.Cut(text[start+len(opens):end], " (")
	if !found || !strings.Contains(pointer, "`"+WorkflowLabel+"` line") || !strings.Contains(pointer, "before `merging`") {
		t.Fatalf("step 1 of the composed merger prompt does not send the merger to the `%s` line for the phases before merging: %q",
			WorkflowLabel, text[start:end])
	}
	verifies := func(name string) string {
		line := WorkflowLine(sequenceNamed(t, name))
		if line == "" {
			return defaultWords
		}
		before, _, found := strings.Cut(line, string(phase.Merging)+" (")
		if !found {
			t.Fatalf("the %s workflow's line %q lists no %s", name, line, phase.Merging)
		}
		return before
	}

	light := verifies("light")
	for _, absent := range []string{"tester", "testing", "retro"} {
		if strings.Contains(light, absent) {
			t.Errorf("the merger of a light issue is told to verify %q, which names %q", light, absent)
		}
	}
	if !strings.Contains(light, "reviewing (reviewer)") {
		t.Errorf("the merger of a light issue is told to verify %q, which leaves out the reviewer", light)
	}
	control := verifies(phase.DefaultSequenceName)
	for _, present := range []string{"tester", "retro"} {
		if !strings.Contains(control, present) {
			t.Errorf("the merger of a default issue is told to verify %q, which leaves out %q", control, present)
		}
	}
}
