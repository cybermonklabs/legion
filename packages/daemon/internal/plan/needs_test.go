package plan

import (
	"fmt"
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

// Dispatch takes an owner's plan of 10,000 tasks in a ring, or of 300 tasks each needing all the
// others, and a search from every need of a set, which named the shortest cycle through each one,
// followed about 10⁸ needs on that ring. So the check of cycles takes time linear in the plan: one
// pass finds the sets of tasks that need each other (components), and one breadth-first search
// from the first task of each set that holds a cycle names it (cycleFrom). Each plan below gives
// its cycles: the ring's one cycle through every task, the complete graph's shortest cycle from
// its first task, and one cycle for each task that needs itself. And its searches follow at most
// the plan's needs, as cycles counts them: a task joins at most one search, so each need is
// followed at most once. The ring's one search follows each of its 10,000 needs, the bound
// itself, so a search from a second task of a set fails the bound there. The fault is a search
// from every task of a set; planted, the ring follows 100,000,000 needs and the complete graph
// 134,551 of its 89,700. The count sees only the needs cycleFrom follows: not components' loop,
// not cycles' own loops over the tasks, and not work done inside one follow or once for each set,
// such as a reset of every mark in parent before each search. It counts work and reads no clock.
func TestTheCheckOfCyclesFollowsEachNeedAtMostOnce(t *testing.T) {
	ring := make([]Task, 10_000)
	ringCycle := make([]string, len(ring))
	for i := range ring {
		ring[i] = Task{ID: fmt.Sprintf("t%05d", i), Needs: []string{fmt.Sprintf("t%05d", (i+1)%len(ring))}}
		ringCycle[i] = ring[i].ID
	}
	complete := make([]Task, 300)
	for i := range complete {
		complete[i].ID = fmt.Sprintf("t%03d", i)
		for j := range complete {
			if j != i {
				complete[i].Needs = append(complete[i].Needs, fmt.Sprintf("t%03d", j))
			}
		}
	}
	itself := make([]Task, 10_000)
	itselfCycles := make([][]string, len(itself))
	for i := range itself {
		itself[i] = Task{ID: fmt.Sprintf("t%05d", i), Needs: []string{fmt.Sprintf("t%05d", i)}}
		itselfCycles[i] = []string{itself[i].ID}
	}
	for _, tc := range []struct {
		name  string
		tasks []Task
		want  [][]string
	}{
		{"a ring of 10,000 tasks", ring, [][]string{ringCycle}},
		{"300 tasks each needing all the others", complete, [][]string{{"t000", "t001"}}},
		{"10,000 tasks each needing itself", itself, itselfCycles},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := 0
			for _, task := range tc.tasks {
				needs += len(task.Needs)
			}
			found, followed := cycles(tc.tasks)
			if !reflect.DeepEqual(found, tc.want) {
				t.Fatalf("cycles(%s) found %d cycles, want %d: %s", tc.name, len(found), len(tc.want), firstOther(found, tc.want))
			}
			if followed > needs {
				t.Fatalf("cycles(%s) followed %d needs, want at most the plan's %d", tc.name, followed, needs)
			}
			t.Logf("cycles(%s) followed %d needs of the plan's %d", tc.name, followed, needs)
		})
	}
}

// firstOther names the first cycle found that differs from the one wanted at its place, each by
// its first five tasks and its length, so a failure of a 10,000-task ring stays readable.
func firstOther(found, want [][]string) string {
	short := func(cycle []string) string {
		return fmt.Sprintf("%v (%d tasks)", cycle[:min(len(cycle), 5)], len(cycle))
	}
	for i := range max(len(found), len(want)) {
		switch {
		case i >= len(found):
			return fmt.Sprintf("cycle %d is missing, want %s", i, short(want[i]))
		case i >= len(want):
			return fmt.Sprintf("cycle %d is %s, want none", i, short(found[i]))
		case !reflect.DeepEqual(found[i], want[i]):
			return fmt.Sprintf("cycle %d is %s, want %s", i, short(found[i]), short(want[i]))
		}
	}
	return "every cycle as wanted"
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
	if got := Supported(two); !reflect.DeepEqual(got, want) {
		t.Fatalf("Supported(a, b) =\n%+v\nwant\n%+v", got, want)
	}

	aToE := planOfTasksAToE(t)
	if got := Sinks(aToE.Tasks); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("Sinks(the plan of tasks a to e) = %q, want [e]", got)
	}
	got := Supported(aToE)
	if len(got) == 0 || got[0].Reason != "the plan has 5 tasks (a, b, c, d, e), and this release runs one task for each issue."+yet {
		t.Fatalf("Supported(the plan of tasks a to e) = %+v, want the refusal of its five tasks first", got)
	}
	for _, refusal := range got {
		if strings.Contains(refusal.Reason, "no task needs") {
			t.Fatalf("Supported(the plan of tasks a to e) = %+v, want no refusal of tasks that no task needs: e alone is one", got)
		}
	}
}
