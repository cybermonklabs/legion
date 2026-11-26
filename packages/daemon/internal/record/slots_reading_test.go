package record

import (
	"cmp"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The holders below the first issue that waits for a slot are counted in the line's own order
// (spec R6, BelowFirstWaiting), which is Waiting's: under a rule that bands the line
// (SlotRule.PriorityFirst, the daemon's own) the band of the priority that places each comes
// first, and within one band the Dispatch rank of its tree root and then its own; under a rule
// that does not band it, the rank alone. Each row is read under both rules, over the line Waiting
// builds from the records.
//
// The two holders have no priority, of their own or from a tree root, so under the banded rule
// they stand in the band after P3, behind a first waiting issue at P0 whatever their ranks: the
// count is both of them, where the rank alone counts the one ranked below it. With no priority on
// either side there is one band, and the count falls to the rank under both rules.
func TestTheHoldersBelowTheFirstWaitingIssueAreCountedByItsBandFirst(t *testing.T) {
	admitted := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	// root is a root of a tree of its own under the project ACME, with priority as its own.
	root := func(key, status string, stands phase.Phase, rank string, priority Priority) Issue {
		issue := withPriority(linePartIssue(key, key, status, stands, rank), priority)
		issue.Project = "ACME"
		return issue
	}
	said := func(line []LineEntry) []string {
		keys := make([]string, len(line))
		for i, entry := range line {
			keys[i] = entry.Issue.Key + " at " + cmp.Or(entry.Priority.String(), "no priority")
		}
		return keys
	}
	unbanded := mainRuleWith("main with the line by no priority", func(rule *SlotRule) { rule.PriorityFirst = false })
	for _, tc := range []struct {
		name             string
		waiting          Priority
		banded, unbanded int
	}{
		{name: "a P0 first waiting issue ranked between two holders with no priority", waiting: P0, banded: 2, unbanded: 1},
		{name: "a first waiting issue with no priority, so one band on both sides", waiting: NoPriority, banded: 1, unbanded: 1},
	} {
		for _, under := range []struct {
			name string
			rule SlotRule
			want int
		}{
			{"under the daemon's own rule, which bands the line", MainRule, tc.banded},
			{"under a rule that does not band the line", unbanded, tc.unbanded},
		} {
			t.Run(tc.name+" "+under.name, func(t *testing.T) {
				// A root that never ran, ranked in the middle of the board, waits for a slot, and
				// two roots at work in trees of their own hold one each, one ranked above it and
				// one below.
				waiter := root("ACME-1", "todo", phase.Admitted, "m", tc.waiting)
				above := root("ACME-2", "in_progress", phase.Implementing, "b", NoPriority)
				below := root("ACME-3", "in_progress", phase.Implementing, "y", NoPriority)
				issues := []Issue{waiter, above, below}
				slots := []Slot{{Issue: above.Key, Index: 0, AdmittedAt: admitted}, {Issue: below.Key, Index: 1, AdmittedAt: admitted}}
				facts := LineFacts{Rule: under.rule, Lifecycles: map[string]treelifecycle.Lifecycle{
					above.Key: {Tree: above.Key, Authority: treelifecycle.AuthorityWorkflow},
					below.Key: {Tree: below.Key, Authority: treelifecycle.AuthorityWorkflow},
				}}

				line, _ := Waiting(issues, slots, facts)
				ranks := map[string]string{waiter.Key: waiter.Rank, above.Key: above.Rank, below.Key: below.Rank}
				members := make([]SlotsLineMember, len(line))
				for i, entry := range line {
					members[i] = SlotsLineMember{Entry: entry, RootRank: ranks[entry.Issue.Tree]}
				}
				holders := []SlotHolder{
					{Issue: above, RootRank: above.Rank, State: HolderWorking},
					{Issue: below, RootRank: below.Rank, State: HolderWorking},
				}
				reading := SlotsReadingOf(4, holders, members, nil)
				if want := (BelowFirstWaiting{Count: under.want, First: waiter.Key}); reading.BelowFirstWaiting != want {
					t.Errorf("belowFirstWaiting = %+v, want %+v (the line %v, the holders %s ranked %q and %s ranked %q)",
						reading.BelowFirstWaiting, want, said(line), above.Key, above.Rank, below.Key, below.Rank)
				}
			})
		}
	}
}
