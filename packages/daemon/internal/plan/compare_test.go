package plan

import (
	"reflect"
	"testing"
)

// planOf is the plan FromBlocks reads from blocks, which the test fails on when it cannot.
func planOf(t *testing.T, version int, blocks ...Block) Plan {
	t.Helper()
	p, err := FromBlocks("plan-md", version, blocks)
	if err != nil {
		t.Fatalf("FromBlocks(version %d): %v", version, err)
	}
	return p
}

// A later version changes a task when its block says anything else: its steps, its text, or an
// attribute Legion reads, such as `needs`. A version that changes only the prose outside the task
// and role blocks changes no task. A task the held plan has and the version does not is gone, and one the
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

// A later version that changes a role the plan defines changes each task with a step that names
// it: its prompt, its model, its type, or the role block taken away. A role added that no step
// names changes no task. So a version that would give a running agent another prompt or another
// model changes its task, and the workflow refuses it while the task runs.
func TestAChangedRoleChangesTheTasksThatNameIt(t *testing.T) {
	a := Block{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer prober"}, Text: "Do the change and probe it."}
	b := Block{ID: "b", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer"}, Text: "Do another change."}
	prober := func(attributes map[string]string, prompt string) Block {
		return Block{ID: "prober", Type: RoleBlockType, Attributes: attributes, Text: prompt}
	}
	held := planOf(t, 1, a, b, prober(map[string]string{"type": "testing", "model": "m"}, "Probe."))

	for _, tc := range []struct {
		name string
		next Plan
		want []string
	}{
		{name: "the prompt", next: planOf(t, 2, a, b, prober(map[string]string{"type": "testing", "model": "m"}, "Probe again.")), want: []string{"a"}},
		{name: "the model", next: planOf(t, 2, a, b, prober(map[string]string{"type": "testing", "model": "n"}, "Probe.")), want: []string{"a"}},
		{name: "the type", next: planOf(t, 2, a, b, prober(map[string]string{"type": "review", "model": "m"}, "Probe.")), want: []string{"a"}},
		{name: "a role no step names added", next: planOf(t, 2, a, b, prober(map[string]string{"type": "testing", "model": "m"}, "Probe."),
			Block{ID: "critic", Type: RoleBlockType, Attributes: map[string]string{"type": "review"}, Text: "Criticise."}), want: []string{}},
		{name: "the role block taken away", next: planOf(t, 2, a, b), want: []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChangedTasks(held, tc.next); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ChangedTasks = %#v, want %#v", got, tc.want)
			}
		})
	}
}
