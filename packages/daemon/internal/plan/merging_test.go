package plan

import (
	"slices"
	"testing"
)

// tasksAToE is the five tasks a to e, where c needs b, d needs a and c, and e needs d, and where
// task b says merges when merges is "b".
func tasksAToE(merges string) []Task {
	return []Task{{ID: "a"}, {ID: "b", Merges: merges == "b"}, {ID: "c", Needs: []string{"b"}},
		{ID: "d", Needs: []string{"a", "c"}}, {ID: "e", Needs: []string{"d"}}}
}

// fourApart is four tasks that need no task.
var fourApart = []Task{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}

// The work of a task merges by itself where its block says merges, and where it is a task of the
// top level that no other task needs, in id order. A task that says merges merges though another
// task needs it. A task of the top level that no task needs merges though its block writes
// merges="false". A part of another task (PartOf) is no task of the top level, so it merges only
// where its block says merges.
//
// The faults it catches are a task that says merges and is needed, held back (its control: the
// tasks that no task needs alone, under which the tasks a to e with b saying merges give [e]), and a
// task that no task needs whose block writes merges="false", held back (its control: the tasks that
// say merges alone, under which the tasks a to e give none).
func TestATaskMergesWhenItSaysSoAndWhenNoTaskNeedsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []Task
		want  []string
	}{
		{"the tasks a to e", tasksAToE(""), []string{"e"}},
		{"the tasks a to e, where b says merges", tasksAToE("b"), []string{"b", "e"}},
		{"four tasks with no needs", fourApart, []string{"a", "b", "c", "d"}},
		{"c, d needing c, a, g, x and y", []Task{{ID: "c"}, {ID: "d", Needs: []string{"c"}}, {ID: "a"}, {ID: "g"}, {ID: "x"}, {ID: "y"}},
			[]string{"a", "d", "g", "x", "y"}},
		{"a task that no task needs, whose block writes merges=\"false\"",
			planOf(t, 1, Block{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"merges": "false"}, Text: "Do it."}).Tasks, []string{"a"}},
		{"one task, with no merges", []Task{{ID: "a"}}, []string{"a"}},
		{"f and its part f1, which no task needs", []Task{{ID: "f"}, {ID: "f1", PartOf: "f"}}, []string{"f"}},
		{"f and its part f1, which says merges", []Task{{ID: "f"}, {ID: "f1", PartOf: "f", Merges: true}}, []string{"f", "f1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Merging(tc.tasks); !slices.Equal(got, tc.want) {
				t.Fatalf("Merging = %q, want %q", got, tc.want)
			}
		})
	}
}
