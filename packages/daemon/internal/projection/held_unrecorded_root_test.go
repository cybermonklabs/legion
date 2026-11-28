package projection

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An issue whose Dispatch parent the copy holds no row of stands on a chain the daemon cannot
// read to its top, so it is neither a root nor an issue under a parent, and it is held with
// chain_unknown and the link the chain stopped at (R16 of dispatch://INFRA-3320), whatever else
// is true of it: chain_unknown is read before every other word, because each later word reads
// the chain. Today's daemon read such an issue as the member of a tree whose root the record does
// not hold and gave it unclassified (C2 of dispatch://INFRA-3107); the chain says why nothing of
// it runs, and that it waits for the daemon to read Dispatch again.
func TestAnIssueWhoseParentTheCopyDoesNotHoldIsChainUnknown(t *testing.T) {
	unread := lineIssue("LEGION-301", "LEGION-300", "todo", phase.Admitted, "LEGION-301")
	unread.Parent = record.ParentOf("LEGION-300")
	gatedRoot := lineIssue("LEGION-310", "LEGION-310", "todo", phase.Admitted, "LEGION-310")
	gatedMember := lineIssue("LEGION-311", "LEGION-310", "todo", phase.Admitted, "LEGION-311")
	gatedMember.Parent = record.ParentOf("LEGION-310")

	for _, tc := range []struct {
		name  string
		store projectionStore
		issue string
		want  api.HeldIssue
	}{
		{
			name:  "a parent the copy holds no row of and nothing else true of the issue",
			store: projectionStore{issues: []record.Issue{unread}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldChainUnknown, HeldBy: []string{"LEGION-300"}},
		},
		{
			// The gate is a row of its own, so the word must not turn on whether one was
			// written for the parent the copy does not hold.
			name: "a parent the copy holds no row of with a gate row standing open",
			store: projectionStore{issues: []record.Issue{unread},
				gates: map[string]map[record.DesignStep]record.DesignGate{
					"LEGION-300": openGate("LEGION-300", projectedAt.Add(-time.Hour))}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldChainUnknown, HeldBy: []string{"LEGION-300"}},
		},
		{
			// chain_unknown is read before the issue's own link, which a known chain would
			// read next.
			name: "a parent the copy holds no row of and an unfinished Dispatch link",
			store: projectionStore{issues: []record.Issue{unread},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-301", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}, Parent: "LEGION-300"},
					{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
				}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldChainUnknown, HeldBy: []string{"LEGION-300"}},
		},
		{
			// The control: a known chain under a root the record holds, under a gate a person
			// has not approved at its current version.
			name: "a recorded root under a shut gate",
			store: projectionStore{issues: []record.Issue{gatedRoot, gatedMember},
				slots: []record.Slot{{Issue: "LEGION-310", Index: 0, AdmittedAt: projectedAt}},
				gates: map[string]map[record.DesignStep]record.DesignGate{"LEGION-310": shutGate("LEGION-310")}},
			issue: "LEGION-311",
			want:  api.HeldIssue{Issue: "LEGION-311", Reason: record.HeldDesignGate, HeldBy: []string{"LEGION-310"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wordFor(t, tc.store, tc.issue)
			if got.Reason != tc.want.Reason || !equalKeys(got.HeldBy, tc.want.HeldBy) {
				t.Fatalf("the held row of %s = %#v, want %#v", tc.issue, got, tc.want)
			}
		})
	}
}

// equalKeys is whether two lists of issue keys are the same list, an empty one and a nil one
// counting as one: the row's own wire form is what says whether `heldBy` is carried, and that is
// read by the test of C1's shapes rather than here.
func equalKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
