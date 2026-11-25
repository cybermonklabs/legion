package plan

import (
	"reflect"
	"strings"
	"testing"
)

// planOf is the plan FromBlocks reads from blocks, which the test fails on when it cannot.
func planOf(t *testing.T, version int, blocks ...Block) Plan {
	t.Helper()
	p, err := FromBlocks("plan-md", planDocument, version, blocks)
	if err != nil {
		t.Fatalf("FromBlocks(version %d): %v", version, err)
	}
	return p
}

// A later version changes a task when its block says anything else: its steps, its text, or an
// attribute Legion reads, such as `needs`. A version that changes only the prose outside the task
// blocks changes no task. A task the held plan has and the version does not is gone, and one the
// version adds is new; both are changed, in the order of their ids.
func TestChangedTasksReadsStepsTextAndAttributes(t *testing.T) {
	intro := Block{ID: "intro", Type: "paragraph", Text: "Why the plan is so."}
	task := func(attributes map[string]string, text string) Block {
		return Block{ID: "a", Type: "plan_task", Attributes: attributes, Text: text}
	}
	held := planOf(t, 1, intro, task(map[string]string{"steps": "implementer reviewer"}, "Do the change."))

	for _, tc := range []struct {
		name string
		next Plan
		want []string
	}{
		{name: "the same blocks", next: planOf(t, 2, intro, task(map[string]string{"steps": "implementer reviewer"}, "Do the change.")), want: []string{}},
		{name: "the steps", next: planOf(t, 2, intro, task(map[string]string{"steps": "implementer tester reviewer"}, "Do the change.")), want: []string{"a"}},
		{name: "the text", next: planOf(t, 2, intro, task(map[string]string{"steps": "implementer reviewer"}, "Do the change, and its test.")), want: []string{"a"}},
		{name: "needs added", next: planOf(t, 2, intro, task(map[string]string{"steps": "implementer reviewer", "needs": "dispatch://ACME-1/artifact/plan-md#b-b"}, "Do the change.")), want: []string{"a"}},
		{name: "the steps taken away", next: planOf(t, 2, intro, task(nil, "Do the change.")), want: []string{"a"}},
		{name: "a paragraph outside the block", next: planOf(t, 2, Block{ID: "intro", Type: "paragraph", Text: "Why the plan is so, said again."},
			task(map[string]string{"steps": "implementer reviewer"}, "Do the change.")), want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChangedTasks(held, tc.next); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ChangedTasks = %#v, want %#v", got, tc.want)
			}
		})
	}

	defaults, _ := ParseSteps("planner implementer tester reviewer acceptance:owner")
	heldDefault := Plan{Tasks: []Task{{ID: "default", Steps: defaults, Text: "The project's default steps."}}}
	next := planOf(t, 1, task(map[string]string{"steps": "implementer reviewer"}, "Do the change."))
	if got, want := ChangedTasks(heldDefault, next), []string{"a", "default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedTasks(default, {a}) = %#v, want %#v (one new, one gone)", got, want)
	}
}

// A running task takes a later version that changes its text alone, or `serves`, which says what
// the task is for and nothing Legion carries out. Every other change of its block, and its removal,
// is refused, with a reason that names the task and says that it runs. So is a change of the block
// its `parent` names, from one requirement to another, and a parent added or removed. A pin of the
// same parent (`@v<N>`) names a version of the same block and no other block, so it changes nothing.
func TestARunningTaskTakesAChangeOfItsTextAlone(t *testing.T) {
	attributes := func(changes ...string) map[string]string {
		held := map[string]string{"steps": "implementer reviewer", "serves": "dispatch://ACME-1/spec#b-r1"}
		for i := 0; i < len(changes); i += 2 {
			held[changes[i]] = changes[i+1]
		}
		return held
	}
	task := func(attributes map[string]string, text string) Block {
		return Block{ID: "a", Type: TaskBlockType, Attributes: attributes, Text: text}
	}
	held := planOf(t, 1, task(attributes(), "Text one."))
	const r1, r2 = "dispatch://ACME-1/spec#b-r1", "dispatch://ACME-1/spec#b-r2"
	underR1 := planOf(t, 1, task(attributes("parent", r1), "Text one."))

	for _, tc := range []struct {
		name string
		// held is the version in force, the plan held above where it holds no task.
		held    Plan
		next    Plan
		refused bool
	}{
		{name: "the text alone", next: planOf(t, 2, task(attributes(), "Text two."))},
		{name: "serves alone", next: planOf(t, 2, task(attributes("serves", "dispatch://ACME-1/spec#b-r2"), "Text one."))},
		{name: "the steps", next: planOf(t, 2, task(attributes("steps", "implementer tester reviewer"), "Text one.")), refused: true},
		{name: "needs", next: planOf(t, 2, task(attributes("needs", "dispatch://ACME-1/artifact/plan-md#b-b"), "Text one.")), refused: true},
		{name: "owner", next: planOf(t, 2, task(attributes("owner", "sub-architect"), "Text one.")), refused: true},
		{name: "the task it is a part of", next: planOf(t, 2, Block{ID: "f", Type: TaskBlockType, Attributes: map[string]string{"owner": "sub-architect"}, Text: "Text of f."},
			withParent(Block{ID: "a", Type: TaskBlockType, Attributes: attributes(), Text: "Text one."}, "f")), refused: true},
		{name: "merges", next: planOf(t, 2, task(attributes("merges", "true"), "Text one.")), refused: true},
		{name: "the task removed", next: planOf(t, 2, Block{ID: "b", Type: TaskBlockType, Attributes: attributes(), Text: "Text one."}), refused: true},
		{name: "from r1 to r2", held: underR1, next: planOf(t, 2, task(attributes("parent", r2), "Text one.")), refused: true},
		{name: "a parent added", next: planOf(t, 2, task(attributes("parent", r1), "Text one.")), refused: true},
		{name: "a parent removed", held: underR1, next: planOf(t, 2, task(attributes(), "Text one.")), refused: true},
		{name: "the same parent, a new text", held: underR1, next: planOf(t, 2, task(attributes("parent", r1), "Text two."))},
		{name: "the same parent, pinned", held: underR1, next: planOf(t, 2, task(attributes("parent", r1+"@v3"), "Text two."))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := tc.held
			if from.Tasks == nil {
				from = held
			}
			got := RunningChange(from, tc.next, "a")
			if !tc.refused {
				if got != nil {
					t.Fatalf("RunningChange = %+v, want the change taken", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("RunningChange took the change, want it refused")
			}
			if got.Task != "a" || !strings.Contains(got.Reason, "task a is running") {
				t.Fatalf("RunningChange = %+v, want a refusal of task a whose reason says that task a is running", *got)
			}
		})
	}
}
