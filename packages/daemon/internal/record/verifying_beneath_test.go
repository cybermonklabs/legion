package record

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The rules beneath a merged issue (D10 of dispatch://INFRA-5580/artifact/plan-md): a child's next
// change under an accepted ancestor is not held, and a child whose close waits in verifying holds
// its parent's close and not its parent's own first phase.

// A child's next change starts under an ancestor the daemon has already accepted: the change
// follows a merge that is on the base already, so the hold that keeps an ancestor from merging a
// head without its children's work does not hold it. The line offers it a slot, and no drop site
// names it accepted_ancestor. The control is the child's first run at admitted under the same
// ancestor, which that hold keeps out of the line.
func TestAChildsNextChangeUnderAnAcceptedParentIsNotHeld(t *testing.T) {
	ctx := context.Background()
	const root, merging, child = "LEGION-700", "LEGION-701", "LEGION-702"
	for _, tc := range []struct {
		name       string
		nextChange uint64
		inLine     bool
	}{
		{name: "its next change", nextChange: 2, inLine: true},
		{name: "control: its first run", nextChange: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			records := NewStore()
			beneath := chainIssue(child, root, merging, phase.Admitted, "todo")
			beneath.Generation, beneath.NextChange = 1, tc.nextChange
			if tc.nextChange != 0 {
				beneath.Generation = tc.nextChange
				beneath.Merge = Merge{Number: 70, Head: "head-70", SHA: "merge-70", Base: "main", Repo: "acme/widgets"}
			}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, chainIssue(root, root, "", phase.Implementing, "in_progress")))
				must(t, records.PutIssue(ctx, tx, chainIssue(merging, root, root, phase.Merging, "retro")))
				must(t, records.PutIssue(ctx, tx, beneath))
				approved := 1
				must(t, records.PutGate(ctx, tx, DesignGate{Issue: root, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
				if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
					t.Fatalf("open the tree of %s: %v", root, err)
				}
			})

			inTx(t, st, func(tx pgx.Tx) {
				issues, err := records.LiveIssues(ctx, tx, "LEGION")
				if err != nil {
					t.Fatalf("list the live issues: %v", err)
				}
				facts, err := ReadLineFacts(ctx, tx, records, "LEGION", issues, nil)
				if err != nil {
					t.Fatalf("read the line's facts: %v", err)
				}
				line, dropped := Waiting(issues, nil, facts)
				if got := inLine(line, child); got != tc.inLine {
					t.Fatalf("%s stands in the waiting line = %t, want %t", child, got, tc.inLine)
				}
				for _, row := range dropped {
					if row.Issue != child || row.Reason != HeldAcceptedAncestor {
						continue
					}
					if tc.inLine {
						t.Fatalf("%s is held under %v as an accepted ancestor, want its next change offered a slot", child, row.HeldBy)
					}
					return
				}
				if !tc.inLine {
					t.Fatalf("the control %s reads no accepted_ancestor word, want the hold this case is the exception to", child)
				}
			})
		})
	}
}

// A child whose own close waits in verifying holds its parent's close and not its parent's first
// phase: the child's work is on the base already, so the children-first rule reads it as closed
// (ChildOpen) while the issues beneath the parent still name it (OpenBeneath), and the sign-off's
// set leaves it out (WorkBeneath). The controls are the same child at work in implementing, which
// holds all three, and the child done, which holds none.
func TestAChildInVerifyingDoesNotHoldItsParentsFirstPhase(t *testing.T) {
	const parent, child = "LEGION-710", "LEGION-711"
	parentKey := parent
	for _, tc := range []struct {
		name                 string
		stands               phase.Phase
		status               string
		open, beneath, works bool
	}{
		{name: "waiting in verifying", stands: phase.Verifying, status: "testing", beneath: true},
		{name: "control: at work in implementing", stands: phase.Implementing, status: "in_progress", open: true, beneath: true, works: true},
		{name: "control: done", stands: phase.Done, status: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorded := Issue{Key: child, Tree: parent, Parent: &parentKey, Project: "LEGION", Phase: tc.stands, Generation: 1,
				Status: tc.status, DispatchStatus: tc.status}
			above := Issue{Key: parent, Tree: parent, Project: "LEGION", Phase: phase.Admitted, Generation: 1, Status: "in_progress"}
			members := []Issue{above, recorded}
			if got := ChildOpen(DispatchIssueRow{Key: child, Status: tc.status}, &recorded); got != tc.open {
				t.Errorf("ChildOpen(%s in %s) = %t, want %t", child, tc.stands, got, tc.open)
			}
			want := []string{}
			if tc.beneath {
				want = []string{child}
			}
			if got := OpenBeneath(above, members); !slices.Equal(got, want) {
				t.Errorf("OpenBeneath = %v, want %v: the parent's close waits for a child in verifying", got, want)
			}
			work := []string{}
			if tc.works {
				work = []string{child}
			}
			if got := WorkBeneath(above, members); !slices.Equal(got, work) {
				t.Errorf("WorkBeneath = %v, want %v: the sign-off does not wait for a child in verifying", got, work)
			}
		})
	}
}

// A parent held before its retro for the issues beneath it (HoldForChildren) is released by the
// workflow's acceptanceHeld, which waits for WorkBeneath (D10): a child whose close waits in
// verifying has merged its work, so it holds the parent's acceptance no longer. The admission
// pass's facts carry the same hold (RetroHolds.Beneath, retroHoldsOf), which the held list and the
// line's word read and which decides whether the pass reads the parent's release
// (admit's readsHeldRetro), so they must name what the workflow waits for and nothing more. Read
// over OpenBeneath, they told the controller that the parent waited on a merged child, and where
// that child's check waits for the root's merge, they named a hold the root's own retro could
// never end. The controls are the child at work, which holds the parent, and the child done.
func TestARetroHeldParentDoesNotWaitOnAChildInVerifying(t *testing.T) {
	const parent, child = "LEGION-720", "LEGION-721"
	parentKey := parent
	for _, tc := range []struct {
		name   string
		stands phase.Phase
		status string
		holds  bool
	}{
		{name: "the child waits in verifying", stands: phase.Verifying, status: "testing"},
		{name: "control: the child at work in implementing", stands: phase.Implementing, status: "in_progress", holds: true},
		{name: "control: the child done", stands: phase.Done, status: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := Issue{Key: parent, Tree: parent, Project: "LEGION", Phase: phase.Held, Hold: &Hold{From: phase.Retro, Reason: HoldForChildren},
				Generation: 1, Status: "retro"}
			beneath := Issue{Key: child, Tree: parent, Parent: &parentKey, Project: "LEGION", Phase: tc.stands, Generation: 1,
				Status: tc.status, DispatchStatus: tc.status}
			issues := []Issue{held, beneath}
			heldParents := RetroHeldParents(issues)
			if !slices.Equal(heldParents, []string{parent}) {
				t.Fatalf("the parents held before their retro = %v, want %s: the fixture is no such parent", heldParents, parent)
			}
			holds := retroHoldsOf(&passChain{issues: issues}, heldParents, nil, nil)[parent]
			want := []string{}
			if tc.holds {
				want = []string{child}
			}
			if got := append([]string{}, holds.Beneath...); !slices.Equal(got, want) {
				t.Errorf("the pass's hold beneath %s = %v, want %v: it names what the workflow's acceptanceHeld waits for (WorkBeneath)",
					parent, holds.Beneath, want)
			}
		})
	}
}
