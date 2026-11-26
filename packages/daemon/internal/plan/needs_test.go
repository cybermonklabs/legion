package plan

import (
	"reflect"
	"strings"
	"testing"
)

// needWord is a need written as Dispatch writes a reference to the block id of the plan document.
func needWord(id string) string { return "dispatch://ACME-1/artifact/plan-md#b-" + id }

// taskNeeding is a `plan_task` block id, with the steps `implementer reviewer`, whose `needs`
// writes the words needs, and no `needs` when there are none.
func taskNeeding(id string, needs ...string) Block {
	attributes := map[string]string{"steps": "implementer reviewer"}
	if len(needs) > 0 {
		attributes["needs"] = strings.Join(needs, " ")
	}
	return Block{ID: id, Type: TaskBlockType, Attributes: attributes, Text: "What task " + id + " does."}
}

// planOfTasksAToE is the plan of five tasks a to e, each need written as a reference to a block:
// c needs b, d needs a and c, and e needs d.
func planOfTasksAToE(t *testing.T) Plan {
	t.Helper()
	return planOf(t, 1, taskNeeding("a"), taskNeeding("b"), taskNeeding("c", needWord("b")),
		taskNeeding("d", needWord("a"), needWord("c")), taskNeeding("e", needWord("d")))
}

// FromBlocks gives each task's Needs as the ids of the tasks they name, in the order the block
// writes them: a reference's block id after its last `#b-`, decoded as Dispatch encodes one (`%2D`
// is a `-`) with a pin after it left out, and a word with no `#b-` as itself. A task named twice is
// needed once. Each word that names no task stays in Unresolved. So no caller holds a plan whose
// needs are words. The fault is a plan read with the words of its needs, which no reader of a task
// id can read, and which Validate takes; the control is a FromBlocks that keeps each word whole,
// under which d needs the words.
func TestFromBlocksReadsEachNeedAsTheTaskItNames(t *testing.T) {
	read := planOf(t, 1, taskNeeding("a-b"), taskNeeding("c"),
		taskNeeding("d", needWord("c")+"@v2", needWord("a%2Db"), "x", "c"),
		taskNeeding("e", "d", needWord("d")+"@vx"))
	if got := read.Tasks[2]; !reflect.DeepEqual(got.Needs, []string{"c", "a-b"}) || !reflect.DeepEqual(got.Unresolved, []string{"x"}) {
		t.Fatalf("d needs %q and leaves %q unresolved, want [c a-b] and [x]", got.Needs, got.Unresolved)
	}
	// A pin that is no `@v<N>` makes the reference name no block, as Dispatch reads it.
	if got := read.Tasks[3]; !reflect.DeepEqual(got.Needs, []string{"d"}) || !reflect.DeepEqual(got.Unresolved, []string{needWord("d") + "@vx"}) {
		t.Fatalf("e needs %q and leaves %q unresolved, want [d] and the reference with the bad pin", got.Needs, got.Unresolved)
	}
	if got := read.Tasks[0]; got.Needs != nil || got.Unresolved != nil {
		t.Fatalf("a, which needs nothing, needs %q and leaves %q unresolved", got.Needs, got.Unresolved)
	}
}

// A need that names no task of the plan waits for a task that never runs, so Validate refuses
// it for good, once for each such word in the order the block writes them, naming the word as the
// block writes it. The fault is such a plan taken; the control is a FromBlocks that keeps each
// word whole, under which the reference to b-x is refused by its id and the plan of tasks a to e
// answers four refusals of a need of no task.
func TestANeedThatNamesNoTaskIsRefused(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	for _, tc := range []struct {
		name string
		plan Plan
		want []Refusal
	}{
		{name: "a word", plan: planOf(t, 1, taskNeeding("a"), taskNeeding("d", "x")),
			want: []Refusal{{Task: "d", Reason: "task d needs x, which names no task of this plan."}}},
		{name: "a reference to the block of no task", plan: planOf(t, 1, taskNeeding("a"), taskNeeding("d", needWord("x"))),
			want: []Refusal{{Task: "d", Reason: "task d needs dispatch://ACME-1/artifact/plan-md#b-x, which names no task of this plan."}}},
		{name: "each word in the block's order", plan: planOf(t, 1, taskNeeding("a"), taskNeeding("d", "y", needWord("a"), "x")),
			want: []Refusal{
				{Task: "d", Reason: "task d needs y, which names no task of this plan."},
				{Task: "d", Reason: "task d needs x, which names no task of this plan."},
			}},
		{name: "a word written twice", plan: planOf(t, 1, taskNeeding("a"), taskNeeding("d", "x", "x")),
			want: []Refusal{{Task: "d", Reason: "task d needs x, which names no task of this plan."}}},
		{name: "the plan of tasks a to e", plan: planOfTasksAToE(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(tc.plan, d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A task can need only a task of its own plan, and an issue waits for another issue by a
// blocked_by link between the two, so a need written as a reference into another issue (an issue
// key after `dispatch://` that is not the plan's) is kept out of the task's needs, whatever task of
// this plan its block id would name, and Validate refuses it for good, once for each such word,
// with a reason of its own that names the word and that issue. Every other word is read as before:
// a reference into the plan's own issue by its block id, so a block of another document of that
// issue that is no task gets the refusal of a need of no task, and a word with no `dispatch://` as
// a block of the plan's own document. The fault is a reference into another issue read as the task
// of this plan its block id names, which Validate takes (d needs a); the control is the read
// without the check of the issue, under which the first four cases fail.
func TestANeedIntoAnotherIssueIsRefused(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	into := func(word string) Refusal {
		return Refusal{Task: "d", Reason: "task d needs " + word + ", which names another issue, ACME-2: a task can need only a task of this plan, " +
			"and an issue waits for another issue by a blocked_by link between the two."}
	}
	other := "dispatch://ACME-2/artifact/plan-md#b-a"
	for _, tc := range []struct {
		name  string
		block Block
		needs []string
		want  []Refusal
	}{
		{name: "a reference to a task of another issue's plan", block: taskNeeding("d", other), want: []Refusal{into(other)}},
		{name: "that reference pinned and written twice, beside a task of this plan", block: taskNeeding("d", other+"@v2", needWord("a"), other+"@v2"),
			needs: []string{"a"}, want: []Refusal{into(other + "@v2")}},
		{name: "a reference to a block of another issue that is no task here", block: taskNeeding("d", "dispatch://ACME-2/spec#b-r1"),
			want: []Refusal{into("dispatch://ACME-2/spec#b-r1")}},
		{name: "another issue itself", block: taskNeeding("d", "dispatch://ACME-2"), want: []Refusal{into("dispatch://ACME-2")}},
		{name: "a block of another document of the plan's issue that is no task", block: taskNeeding("d", "dispatch://ACME-1/spec#b-r1"),
			want: []Refusal{{Task: "d", Reason: "task d needs dispatch://ACME-1/spec#b-r1, which names no task of this plan."}}},
		{name: "a reference with no dispatch://, to the plan's own document", block: taskNeeding("d", "plan-md#b-a"), needs: []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planOf(t, 1, taskNeeding("a"), tc.block)
			if got := p.Tasks[1].Needs; !reflect.DeepEqual(got, tc.needs) {
				t.Fatalf("d needs %q, want %q", got, tc.needs)
			}
			if got := Validate(p, d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A need written as a `dispatch://` reference names a task of this plan only when the reference is
// of the plan's own issue, whose document alone holds the plan, so a reference that starts with no
// issue key (a block of a project's document or of an agent's, a key that is no issue key, or no
// key at all) is kept out of the task's needs, whatever task of this plan its block id would name,
// and Validate refuses it for good, once for each such word, in the block's order beside a
// reference into another issue, with a reason of its own that names the word. The fault is such a
// reference read as the task of this plan its block id names, which Validate takes (d needs a);
// the control is the read that keeps out only a reference that starts with another issue's key,
// under which every case fails.
func TestANeedOfNoIssueIsRefused(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	ofNoIssue := func(word string) Refusal {
		return Refusal{Task: "d", Reason: "task d needs " + word + ", which names no issue: a task can need only a task of this plan, " +
			"and a reference to one starts with the plan's issue key."}
	}
	project, noKey := "dispatch://ACME/artifact/notes-md#b-a", "dispatch://#b-a"
	agent, lowerCase := "dispatch://agent/session-1/artifact/notes-md#b-a", "dispatch://acme-1/artifact/plan-md#b-a"
	other := "dispatch://ACME-2/artifact/plan-md#b-a"
	for _, tc := range []struct {
		name  string
		block Block
		needs []string
		want  []Refusal
	}{
		{name: "a block of a project's document", block: taskNeeding("d", project), want: []Refusal{ofNoIssue(project)}},
		{name: "a reference with no key at all", block: taskNeeding("d", noKey), want: []Refusal{ofNoIssue(noKey)}},
		{name: "a block of an agent's document", block: taskNeeding("d", agent), want: []Refusal{ofNoIssue(agent)}},
		{name: "the plan's issue key in lower case", block: taskNeeding("d", lowerCase), want: []Refusal{ofNoIssue(lowerCase)}},
		{name: "a pinned one written twice, in the block's order beside another issue and a task of this plan",
			block: taskNeeding("d", project+"@v2", other, needWord("a"), project+"@v2"), needs: []string{"a"},
			want: []Refusal{ofNoIssue(project + "@v2"), {Task: "d", Reason: "task d needs " + other + ", which names another issue, ACME-2: " +
				"a task can need only a task of this plan, and an issue waits for another issue by a blocked_by link between the two."}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planOf(t, 1, taskNeeding("a"), tc.block)
			if got := p.Tasks[1].Needs; !reflect.DeepEqual(got, tc.needs) {
				t.Fatalf("d needs %q, want %q", got, tc.needs)
			}
			if got := Validate(p, d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A task that needs itself through other tasks never starts, so Validate refuses each set of
// tasks that need each other for good, once, with one cycle of the set: the shortest from its
// first task in id order back to that task, each task's needs taken in the order its block writes
// them. A need that closes no cycle is in none. So a set through which several cycles run gives
// one reason, which names its first task, and the owner who breaks that cycle reads the next one
// at the next try. The fault is a cycle that walks for ever or is taken, or a set named by no
// cycle. The control is Validate without the check of cycles, under which the first case answers
// no refusal.
func TestATaskThatNeedsItselfIsRefused(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	itself := func(task, chain string) Refusal {
		return Refusal{Task: task, Reason: "task " + task + " needs itself: " + chain + "."}
	}
	for _, tc := range []struct {
		name string
		plan Plan
		want []Refusal
	}{
		{name: "a cycle of three tasks", plan: planOf(t, 1, taskNeeding("a", needWord("b")), taskNeeding("b", needWord("c")), taskNeeding("c", needWord("a"))),
			want: []Refusal{itself("a", "a needs b, b needs c, c needs a")}},
		{name: "a task that names itself", plan: planOf(t, 1, taskNeeding("a", "a")),
			want: []Refusal{itself("a", "a needs a")}},
		{name: "two cycles", plan: planOf(t, 1, taskNeeding("a", "b"), taskNeeding("b", "a"), taskNeeding("c", "d"), taskNeeding("d", "c")),
			want: []Refusal{itself("a", "a needs b, b needs a"), itself("c", "c needs d, d needs c")}},
		{name: "a cycle named by its first task in id order, whatever the document's order", plan: planOf(t, 1,
			taskNeeding("c", needWord("a")), taskNeeding("b", needWord("c")), taskNeeding("a", needWord("b"))),
			want: []Refusal{itself("a", "a needs b, b needs c, c needs a")}},
		{name: "two cycles through one task are one set, named by the shortest cycle from its first task", plan: planOf(t, 1,
			taskNeeding("a", "c", "b"), taskNeeding("b", "a"), taskNeeding("c", "d"), taskNeeding("d", "a")),
			want: []Refusal{itself("a", "a needs b, b needs a")}},
		{name: "two cycles that share a task are one set, named once", plan: planOf(t, 1, taskNeeding("a", "b"), taskNeeding("b", "a", "c"), taskNeeding("c", "b")),
			want: []Refusal{itself("a", "a needs b, b needs a")}},
		{name: "a need outside the cycle", plan: planOf(t, 1, taskNeeding("a", "b"), taskNeeding("b", "a"), taskNeeding("e", "a")),
			want: []Refusal{itself("a", "a needs b, b needs a")}},
		{name: "the plan of tasks a to e", plan: planOfTasksAToE(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(tc.plan, d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
