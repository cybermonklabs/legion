package blockers

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// slotsAdmitted is the moment every slot of the slots reading's fixture was taken.
var slotsAdmitted = time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)

// slotsIssue is one record of the slots reading's fixture under the project ACME, with the
// fields the waiting line reads and its own Dispatch priority: internal/admit's lineIssue
// (line_order_test.go) written for this package. A key that is not its tree's hangs directly under
// that tree's root.
func slotsIssue(key, tree, status string, stands phase.Phase, rank string, priority record.Priority) record.Issue {
	issue := record.Issue{Key: key, Tree: tree, Project: "ACME", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, Priority: priority, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if key != tree {
		issue.Parent = record.ParentOf(tree)
	}
	return issue
}

// slotsLine is one project's records as the slots reading reads them: the issues, the slots and
// the facts the line is built from.
type slotsLine struct {
	issues []record.Issue
	slots  []record.Slot
	facts  record.LineFacts
}

// add records issue, holding no slot.
func (f *slotsLine) add(issue record.Issue) {
	f.issues = append(f.issues, issue)
}

// hold records issue as a slot holder: its record, its slot, and its tree's lifecycle open under
// the workflow.
func (f *slotsLine) hold(issue record.Issue) {
	f.add(issue)
	f.slots = append(f.slots, record.Slot{Issue: issue.Key, Index: len(f.slots), AdmittedAt: slotsAdmitted})
	f.facts.Lifecycles[issue.Tree] = treelifecycle.Lifecycle{Tree: issue.Tree, Authority: treelifecycle.AuthorityWorkflow}
}

// The slots reading counts the holders below the first issue that waits for a slot in the line's
// own order (spec R6): under a rule that bands the line (record.SlotRule.PriorityFirst, the
// daemon's own) by the band of the priority that places each first, and within one band by its
// tree root's rank and then its own; under a rule that does not band it, by the rank alone. The
// line is the one record.Waiting builds over the records, and the reading is SlotsReading, the
// one the state and the tick both read.
//
// So a holder is placed by the priority that would place it in the line (record.PriorityOf): its
// own, else its tree root's, read from the records or, where they leave the root out, from the
// root's record the facts carry (record.LineFacts.Roots), which is where its tree root's rank is
// read from too; else none, whose band stands after P3. Under a rule that does not band, no
// priority places a holder, as none places an entry of the line. Each row is read under both
// rules, and in every row but the last the two give different counts.
func TestTheSlotsReadingPlacesEachHolderByThePriorityThatPlacesIt(t *testing.T) {
	unbanded := record.MainRule
	unbanded.Name = "main with the line by no priority"
	unbanded.KeepsSlotIn = slices.Clone(record.MainRule.KeepsSlotIn)
	unbanded.PriorityFirst = false
	said := func(line []record.LineEntry) []string {
		keys := make([]string, len(line))
		for i, entry := range line {
			keys[i] = entry.Issue.Key + " at " + cmp.Or(entry.Priority.String(), "no priority")
		}
		return keys
	}
	for _, tc := range []struct {
		name             string
		build            func(f *slotsLine)
		first            string
		banded, unbanded int
	}{
		{
			name: "a P0 first waiting issue ranked late and a P3 holder ranked early",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "y", record.P0))
				f.hold(slotsIssue("ACME-2", "ACME-2", "in_progress", phase.Implementing, "b", record.P3))
			},
			first: "ACME-1", banded: 1, unbanded: 0,
		},
		{
			name: "a P3 first waiting issue ranked early and a P0 holder ranked late",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "b", record.P3))
				f.hold(slotsIssue("ACME-2", "ACME-2", "in_progress", phase.Implementing, "y", record.P0))
			},
			first: "ACME-1", banded: 0, unbanded: 1,
		},
		{
			name: "a holder with no priority of its own under a P0 tree root the records hold",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "b", record.P3))
				f.hold(slotsIssue("ACME-2", "ACME-2", "in_progress", phase.Implementing, "y", record.P0))
				f.hold(slotsIssue("ACME-3", "ACME-2", "in_progress", phase.Implementing, "c", record.NoPriority))
			},
			first: "ACME-1", banded: 0, unbanded: 2,
		},
		{
			name: "a holder with no priority of its own under a P0 tree root the records leave out",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "b", record.P3))
				f.facts.Roots["ACME-2"] = slotsIssue("ACME-2", "ACME-2", "done", phase.Done, "y", record.P0)
				f.hold(slotsIssue("ACME-3", "ACME-2", "in_progress", phase.Implementing, "c", record.NoPriority))
			},
			first: "ACME-1", banded: 0, unbanded: 1,
		},
		{
			name: "a holder with no priority on itself or its tree root ranked early",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "y", record.P3))
				f.hold(slotsIssue("ACME-2", "ACME-2", "in_progress", phase.Implementing, "b", record.NoPriority))
			},
			first: "ACME-1", banded: 1, unbanded: 0,
		},
		{
			name: "a first waiting issue with no priority of its own under a P0 tree root",
			build: func(f *slotsLine) {
				// A running P0 tree ranked late, its root holding the row of its run and waiting on
				// its open child, so the root stands in no line, and that child, released with no
				// priority of its own and ranked after its root, waiting for its first phase.
				f.hold(slotsIssue("ACME-1", "ACME-1", "in_progress", phase.Admitted, "y", record.P0))
				f.add(slotsIssue("ACME-2", "ACME-1", "todo", phase.Admitted, "z", record.NoPriority))
				approved := 1
				f.facts.Gates["ACME-1"] = record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {
					Issue: "ACME-1", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved,
					ApprovedMark: record.EventMark{Seq: 7, At: slotsAdmitted}, EverOpened: true}}}
				f.facts.ChildWaits["ACME-1"] = record.ChildWait{Parent: "ACME-1",
					Open: []record.OpenChild{{Key: "ACME-2", Status: "todo", Released: true}}}
				// A P3 root at work, ranked early.
				f.hold(slotsIssue("ACME-3", "ACME-3", "in_progress", phase.Implementing, "b", record.P3))
			},
			first: "ACME-2", banded: 1, unbanded: 0,
		},
		{
			name: "one band on both sides, which falls to rank",
			build: func(f *slotsLine) {
				f.add(slotsIssue("ACME-1", "ACME-1", "todo", phase.Admitted, "m", record.P1))
				f.hold(slotsIssue("ACME-2", "ACME-2", "in_progress", phase.Implementing, "b", record.P1))
				f.hold(slotsIssue("ACME-3", "ACME-3", "in_progress", phase.Implementing, "y", record.P1))
			},
			first: "ACME-1", banded: 1, unbanded: 1,
		},
	} {
		for _, under := range []struct {
			name string
			rule record.SlotRule
			want int
		}{
			{"under the daemon's own rule, which bands the line", record.MainRule, tc.banded},
			{"under a rule that does not band the line", unbanded, tc.unbanded},
		} {
			t.Run(tc.name+" "+under.name, func(t *testing.T) {
				f := &slotsLine{facts: record.LineFacts{Rule: under.rule, Lifecycles: map[string]treelifecycle.Lifecycle{},
					Gates: map[string]record.TreeGate{}, Roots: map[string]record.Issue{}, ChildWaits: map[string]record.ChildWait{}}}
				tc.build(f)

				line, _ := record.Waiting(f.issues, f.slots, f.facts)
				reading, _ := SlotsReading(SlotsInputs{Cap: 4, Issues: f.issues, Slots: f.slots, Facts: f.facts, Line: line})
				if want := (record.BelowFirstWaiting{Count: under.want, First: tc.first}); reading.BelowFirstWaiting != want {
					t.Errorf("belowFirstWaiting = %+v, want %+v (the line %v, the slots %v)",
						reading.BelowFirstWaiting, want, said(line), f.slots)
				}
			})
		}
	}
}
