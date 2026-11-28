package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A run that starts as a root's does and that Dispatch shows in work at a status other than todo
// waits for a person's todo (LineFacts.AwaitsTodo, R19 and R20 of dispatch://INFRA-3320) where it
// holds no slot row, whether it is a new run or a root's first run a person moved from todo to
// in_progress before its promotion, and it reads the word after it where it does hold one: a run
// whose promotion started its architect holds its row at the in_progress the promotion wrote, and
// its own gate, which that architect designs, is what holds it. The waiting line's drop rows carry
// that word for the slot holder (Waiting), and the stalled-tree measure reads them (admit's
// silencedTree, where design_gate silences a tree and awaiting_todo does not), so the slot is read
// on both paths. A run at todo, a root whose label is off, an issue under a parent on its first
// run, which starts on todo as any issue under a parent does, and a gate holder's new run under a
// top issue that waits on a person, which waits for that issue's resume, read no awaiting_todo.
func TestOnlyARunThatHoldsNoSlotAndStandsOutsideTodoAwaitsTodo(t *testing.T) {
	newRun := Issue{Key: "LEGION-900", Tree: "LEGION-900", Project: "LEGION", Phase: phase.Admitted, Generation: 2,
		Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true}
	firstRun := newRun
	firstRun.Generation = 1
	factsOf := func(issues ...Issue) LineFacts {
		return LineFacts{Chains: chainsOf(issues, copyOf(issues...))}
	}

	for _, run := range []Issue{newRun, firstRun} {
		if word, by := HeldReasonOf(run, false, factsOf(run)); word != HeldAwaitingTodo || by != nil {
			t.Fatalf("the root at generation %d holding no slot reads %s held by %v, want awaiting_todo held by no issue",
				run.Generation, word, by)
		}
		if word, by := HeldReasonOf(run, true, factsOf(run)); word != HeldDesignGate || !slices.Equal(by, []string{run.Key}) {
			t.Fatalf("the root at generation %d holding its slot reads %s held by %v, want design_gate held by its own gate",
				run.Generation, word, by)
		}
	}
	for _, tc := range []struct {
		name  string
		slots []Slot
		want  HeldReason
	}{
		{"holding no slot", nil, HeldAwaitingTodo},
		{"holding its slot", []Slot{{Issue: newRun.Key}}, HeldDesignGate},
	} {
		if _, dropped := Waiting([]Issue{newRun}, tc.slots, factsOf(newRun)); len(dropped) == 0 || dropped[0].Reason != tc.want {
			t.Errorf("the waiting line leaves the new run %s behind as %+v, want its first row %s", tc.name, dropped, tc.want)
		}
	}

	atTodo := newRun
	atTodo.Status, atTodo.DispatchStatus = "todo", "todo"
	labelOff := newRun
	labelOff.HandedOver = false
	parent := Issue{Key: "LEGION-901", Tree: "LEGION-901", Project: "LEGION", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true}
	underParent := Issue{Key: "LEGION-902", Tree: "LEGION-901", Project: "LEGION", Phase: phase.Admitted, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, HoldsGate: true, Parent: ParentOf(parent.Key)}
	waitingTop := parent
	waitingTop.Phase, waitingTop.Awaiting = phase.AwaitingPerson, &Awaiting{Ask: "ask-901", Since: EventMark{Seq: 5}}
	underWaitingTop := underParent
	underWaitingTop.Generation = 2
	for _, tc := range []struct {
		name  string
		issue Issue
		facts LineFacts
		want  HeldReason
	}{
		{"a new run at todo", atTodo, factsOf(atTodo), HeldDesignGate},
		{"a new run whose label is off", labelOff, factsOf(labelOff), HeldLabelOff},
		{"an issue under a parent on its first run", underParent, factsOf(parent, underParent), HeldDesignGate},
		{"a gate holder's new run under a top issue that waits on a person", underWaitingTop, factsOf(waitingTop, underWaitingTop),
			HeldRootAwaitingPerson},
	} {
		if word, _ := HeldReasonOf(tc.issue, false, tc.facts); word != tc.want {
			t.Errorf("%s reads %s, want %s: it waits for no todo", tc.name, word, tc.want)
		}
	}
}
