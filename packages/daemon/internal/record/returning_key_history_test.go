package record

import "testing"

// All three versions are in one generation: a key keeps its name while absent and on return,
// even when another key takes its old role and both keys return on different roles.
func TestAReturningKeyKeepsItsOwnNameAcrossVersions(t *testing.T) {
	first := NameAgents(nil, nil, nil, []PlanStep{
		{Generation: 1, Task: "first-version", Position: 1, Role: "reviewer", AgentKey: "a"},
	})
	if len(first) != 1 || first[0].Agent != "reviewer" {
		t.Fatalf("key a in the first version = %+v, want reviewer", first)
	}
	second := NameAgents(first, nil, nil, []PlanStep{
		{Generation: 1, Task: "later-version", Position: 1, Role: "reviewer", AgentKey: "b"},
	})
	if len(second) != 1 || second[0].Agent == "" || second[0].Agent == "reviewer" {
		t.Fatalf("key b while a is absent = %+v, want its own name, not reviewer", second)
	}
	history := append(first, second...)
	for _, keys := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(keys[0]+"-then-"+keys[1], func(t *testing.T) {
			back := NameAgents(history, nil, nil, []PlanStep{
				{Generation: 1, Task: "return-version", Position: 1, Role: "planner", AgentKey: keys[0]},
				{Generation: 1, Task: "return-version", Position: 2, Role: "tester", AgentKey: keys[1]},
			})
			if len(back) != 2 {
				t.Fatalf("returning agents = %+v, want both keys", back)
			}
			for i, key := range keys {
				want := second[0].Agent
				if key == "a" {
					want = "reviewer"
				}
				if back[i].AgentKey != key || back[i].Agent != want {
					t.Fatalf("returning key %s = %+v, want its own name %q", key, back[i], want)
				}
			}
		})
	}
}

// Earlier rows can record one name used by two keys in turn. When both return in one version,
// each gets a distinct name, regardless of their order. All rows are in generation 1.
func TestTwoKeysOfOneHistoryNeverShareAName(t *testing.T) {
	earlier := []PlanStep{
		{Generation: 1, Task: "first-version", Position: 1, Role: "reviewer", AgentKey: "a", Agent: "reviewer"},
		{Generation: 1, Task: "later-version", Position: 1, Role: "reviewer", AgentKey: "b", Agent: "reviewer"},
	}
	for _, keys := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(keys[0]+"-then-"+keys[1], func(t *testing.T) {
			steps := []PlanStep{
				{Generation: 1, Task: "return-version", Position: 1, Role: "reviewer", AgentKey: keys[0]},
				{Generation: 1, Task: "return-version", Position: 2, Role: "reviewer", AgentKey: keys[1]},
			}
			for _, history := range [][]PlanStep{nil, earlier} {
				named := NameAgents(history, nil, nil, steps)
				if len(named) != 2 || named[0].AgentKey != keys[0] || named[1].AgentKey != keys[1] {
					t.Fatalf("returning keys = %+v, want both requested agents", named)
				}
				if named[0].Agent == "" || named[1].Agent == "" {
					t.Fatalf("unnamed returning agent: %+v", named)
				}
				if named[0].Agent == named[1].Agent {
					t.Fatalf("two keys share historical name %q: %+v", named[0].Agent, named)
				}
			}
		})
	}
}
