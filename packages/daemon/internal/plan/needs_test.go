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

// The work of an issue merges as one change, at the end of the one task that no task needs, so
// a plan with more than one such task is refused as not carried out yet, naming them in id order.
// A plan whose tasks all lead to one task is not refused for it (it is still refused for its
// several tasks, until this release runs them). The fault is two tasks whose work would each
// merge by itself, taken; the control is Supported without the refusal, whose answer to a and b
// lacks the second reason.
func TestTwoTasksThatNoTaskNeedsAreNotCarriedOutYet(t *testing.T) {
	two := planOf(t, 1, taskNeeding("b"), taskNeeding("a"))
	if got := Sinks(two.Tasks); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Sinks(b, a) = %q, want [a b], in id order", got)
	}
	want := []Refusal{
		{Task: "", Reason: "the plan has 2 tasks (b, a), and this release runs one task for each issue." + yet},
		{Task: "", Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself." + yet},
	}
	if got := Supported(two, builtin); !reflect.DeepEqual(got, want) {
		t.Fatalf("Supported(a, b) =\n%+v\nwant\n%+v", got, want)
	}

	aToE := planOfTasksAToE(t)
	if got := Sinks(aToE.Tasks); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("Sinks(the plan of tasks a to e) = %q, want [e]", got)
	}
	got := Supported(aToE, builtin)
	if len(got) == 0 || got[0].Reason != "the plan has 5 tasks (a, b, c, d, e), and this release runs one task for each issue."+yet {
		t.Fatalf("Supported(the plan of tasks a to e) = %+v, want the refusal of its five tasks first", got)
	}
	for _, refusal := range got {
		if strings.Contains(refusal.Reason, "no task needs") {
			t.Fatalf("Supported(the plan of tasks a to e) = %+v, want no refusal of tasks that no task needs: e alone is one", got)
		}
	}
}
