package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The task a root architect is given with its start, pinned whole: it is the one thing a fresh
// architect is given in the turn its own launch starts, so what it says is the whole of what that
// turn has to go on. It names the root, the generation its tree is open at, the first act, and
// that nothing else and nobody will start the tree; where a person decided something on the tree
// while nobody in it could hear them, that decision is read first and the instruction follows it.
func TestArchitectStartTask(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 2}
	const lead = "LEGION-208 is yours and its tree is open at generation 2, holding an admission slot while nothing in it runs. " +
		"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then"
	const tail = " Nothing else starts this tree, and no message is coming."
	for _, tc := range []struct {
		name    string
		steps   []record.DesignStep
		decided string
		want    string
	}{
		{
			name:  "a deployment that gates the spec",
			steps: []record.DesignStep{record.DesignStepSpec},
			want:  lead + " request approval of its primary document and register that step of its design gate." + tail,
		},
		{
			name:  "a deployment that gates two steps names both documents",
			steps: []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria},
			want: lead + " request approval of each document this deployment gates and register each of their steps of its design gate: " +
				"its decisions document and its Verification document." + tail,
		},
		{
			name: "a deployment that gates no step",
			want: lead + " register that spec at its current version, which opens the design gate with no approval." + tail,
		},
		{
			name:    "a decision kept while nobody in the tree could hear it",
			steps:   []record.DesignStep{record.DesignStepSpec},
			decided: "A person decided on the tree: dispatch://LEGION-208/spec (design-approved by user:alice).",
			want: "A person decided on the tree: dispatch://LEGION-208/spec (design-approved by user:alice).\n\n" +
				lead + " request approval of its primary document and register that step of its design gate." + tail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArchitectStartTask(root, tc.steps, tc.decided); got != tc.want {
				t.Fatalf("the architect's start task =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
