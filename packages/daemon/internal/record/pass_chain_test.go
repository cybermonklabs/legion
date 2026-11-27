package record

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// One pass walks one chain (chainOf): the hold under an accepted ancestor (Waiting) and the
// children-first cycle (childWaits) are two walks of the same records, so the map they walk is
// built in one place. It holds the pass's own records and the records above them that its read
// leaves out (LineFacts.Parents), which is what lets either walk cross an ancestor already done;
// the pass's own records stay a map of their own, since what runs and what an issue ranks against
// is read from those alone.
func TestOnePassWalksOneChain(t *testing.T) {
	root := chainIssue("LEGION-900", "LEGION-900", "", phase.Admitted, "in_progress")
	merging := chainIssue("LEGION-901", "LEGION-900", "LEGION-900", phase.Merging, "needs_review")
	doneParent := chainIssue("LEGION-902", "LEGION-900", "LEGION-901", phase.Done, "done")
	child := chainIssue("LEGION-903", "LEGION-900", "LEGION-902", phase.Admitted, "todo")
	issues := []Issue{root, merging, child}
	above := map[string]Issue{doneParent.Key: doneParent}

	chain := chainOf(byKey(issues), above)
	want := map[string]Issue{root.Key: root, merging.Key: merging, child.Key: child, doneParent.Key: doneParent}
	if !reflect.DeepEqual(chain, want) {
		t.Fatalf("the chain of the pass holds %v, want %v: its own records and the records above them",
			slices.Sorted(maps.Keys(chain)), slices.Sorted(maps.Keys(want)))
	}
	// The one walk both readers make crosses the done parent and names the merging ancestor,
	// which is the whole reason the records above the pass are in the map at all.
	if ancestor := AcceptedAncestor(child, chain); ancestor != merging.Key {
		t.Fatalf("the accepted ancestor of %s over the pass's chain = %q, want %s past the done parent %s", child.Key, ancestor, merging.Key, doneParent.Key)
	}
	if ancestor := AcceptedAncestor(child, byKey(issues)); ancestor != "" {
		t.Fatalf("the accepted ancestor of %s over the pass's own records alone = %q, want none: that walk stops at the parent they leave out", child.Key, ancestor)
	}
}

// aboveRead is a store whose read of the records above a pass (AncestorsOf) answers above, and
// whose every other read is the store's own.
type aboveRead struct {
	Store
	above []Issue
}

func (s aboveRead) AncestorsOf(context.Context, pgx.Tx, string, []string) ([]Issue, error) {
	return s.above, nil
}

// allocated is the bytes the heap gave out while do ran.
func allocated(do func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	do()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// One pass builds its parent chain once (passChain), and every walk of the pass reads that one
// map. The pass here reads it in each place passChain names, as the admission pass, the state and
// the controller's tick read it (blockers.JudgeLine, admit's tickRows, projection's HeldList). A
// started root and a root held before its retro, each with a child filed under it that no tree
// records, give ReadLineFacts its four walks: the cycle of each child, the child left out of each
// parent's open children, and the issues beneath the held root. An admitted child under a done
// parent beneath an issue in merging gives the line (Waiting) its walk past that parent, where the
// hold keeps the child out of the line (HeldUnderAccepted) and where the ladder names the hold
// (HeldReasonOf). The held list (HeldList) takes the chain for its own ladder, which a pass gives
// no issue to here: the line leaves a row for every issue it does not place, and the held list
// keeps that row.
//
// Nothing of a build shows outside the pass but its cost, so the builds are counted by their bytes.
// The records above the pass are many (aboveRecords, which the read of them answers), each a done
// tree of its own that no walk climbs, so one copy of them outweighs everything else the pass,
// its line and its held list allocate. Their bytes, less those of the read of those records
// (ReadParents, which ReadLineFacts makes as it is) and of the one walk that lists every record of
// the chain (openBeneathIn, for the held root), are a whole number of chains, each the bytes of
// one build (chainOf). The band around one catches a walk that builds a copy of its own, which
// adds one chain: a line and a held list that each build their own bring the pass to three.
func TestAPassBuildsItsParentChainOnce(t *testing.T) {
	const (
		started      = "LEGION-960" // a root in implementing, filed under a done issue
		held         = "LEGION-961" // a root held before its retro
		filedUnder   = "LEGION-962" // the done issue the started root was filed under
		startedChild = "LEGION-963" // filed under the started root, never released
		heldChild    = "LEGION-964" // filed under the held root, never released
		acceptedRoot = "LEGION-965" // the root of a running tree, at admitted
		merging      = "LEGION-966" // a member of that tree in merging, so accepted
		doneParent   = "LEGION-967" // done under the merging member: a record above the pass
		heldAtAdmit  = "LEGION-968" // admitted under the done parent, held under the merging member
		aboveRecords = 4000
	)
	ctx := context.Background()
	st := migratedStore(t)
	heldRoot := planHeld(held, held, nil, phase.Retro, HoldForChildren)
	tree := []Issue{
		chainIssue(acceptedRoot, acceptedRoot, "", phase.Admitted, "in_progress"),
		chainIssue(merging, acceptedRoot, acceptedRoot, phase.Merging, "needs_review"),
		chainIssue(doneParent, acceptedRoot, merging, phase.Done, DoneStatus),
		chainIssue(heldAtAdmit, acceptedRoot, doneParent, phase.Admitted, "todo"),
	}
	issues := []Issue{chainIssue(started, started, filedUnder, phase.Implementing, "in_progress"), heldRoot, tree[0], tree[1], tree[3]}
	above := []Issue{chainIssue(filedUnder, filedUnder, "", phase.Done, DoneStatus), tree[2]}
	for i := len(above); i < aboveRecords; i++ {
		key := fmt.Sprintf("LEGION-%d", 10000+i)
		above = append(above, chainIssue(key, key, "", phase.Done, DoneStatus))
	}
	records := aboveRead{Store: NewStore(), above: above}

	inTx(t, st, func(tx pgx.Tx) {
		for _, reading := range []DispatchReading{
			{Key: startedChild, Project: "LEGION", Seq: 1, Status: Carry("backlog"), Parent: Carry(started), Blockers: Carry[[]string](nil)},
			{Key: heldChild, Project: "LEGION", Seq: 1, Status: Carry("backlog"), Parent: Carry(held), Blockers: Carry[[]string](nil)},
		} {
			must(t, records.PutDispatchIssue(ctx, tx, reading))
		}
		// The accepted root's tree runs under an open gate, so its admitted child reaches the hold.
		for _, issue := range tree {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		approved := 1
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: acceptedRoot, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
		if _, err := treelifecycle.Open(ctx, tx, "legion", acceptedRoot, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree of %s: %v", acceptedRoot, err)
		}
		var facts LineFacts
		var line []LineEntry
		var dropped, heldRows []HeldIssue
		var err error
		pass := allocated(func() {
			if facts, err = ReadLineFacts(ctx, tx, records, "LEGION", issues, nil); err != nil {
				return
			}
			// The line is handed the children-first waits as blockers.JudgeLine hands them on.
			facts.ChildWaits = ChildWaitsOf(facts)
			line, dropped = Waiting(issues, nil, facts)
			heldRows = HeldList(issues, nil, line, dropped, facts)
		})
		must(t, err)
		if len(facts.Parents) != aboveRecords || len(facts.StartedParentWaits[started].Open) != 1 || len(facts.RetroHolds[held].Open) != 1 ||
			len(line)+len(heldRows) != len(issues) {
			t.Fatalf("the pass holds %d records above it, the open children %+v of %s and the holds %+v of %s, and its line and held list place %d of its %d issues; want %d, %s and %s open, and every issue placed: without the pass's walks this test proves nothing",
				len(facts.Parents), facts.StartedParentWaits[started].Open, started, facts.RetroHolds[held], held, len(line)+len(heldRows), len(issues),
				aboveRecords, startedChild, heldChild)
		}
		want := HeldIssue{Issue: heldAtAdmit, Reason: HeldAcceptedAncestor, HeldBy: []string{merging}}
		first := slices.IndexFunc(dropped, func(row HeldIssue) bool { return row.Issue == heldAtAdmit })
		named := slices.IndexFunc(heldRows, func(row HeldIssue) bool { return row.Issue == heldAtAdmit })
		if inLine(line, heldAtAdmit) || first < 0 || !reflect.DeepEqual(dropped[first], want) || named < 0 || !reflect.DeepEqual(heldRows[named], want) {
			t.Fatalf("the line holds %s: in the line %v, its rows %+v, the held list %+v; want it out of the line and held as %+v in both: without the line's walk past %s this test proves nothing",
				heldAtAdmit, inLine(line, heldAtAdmit), dropped, heldRows, want, doneParent)
		}
		// The control: with the records above the pass taken away the walk stops at the done
		// parent, and the line offers the child its first phase, so the hold is the chain's.
		bare := facts
		bare.Parents = nil
		if bareLine, _ := Waiting(issues, nil, bare); !inLine(bareLine, heldAtAdmit) {
			t.Fatalf("%s is out of the line with no record of %s above the pass: the hold is not the walk past it, and this test proves nothing", heldAtAdmit, doneParent)
		}
		read := allocated(func() { _, err = ReadParents(ctx, tx, records, "LEGION", issues) })
		must(t, err)
		var chain map[string]Issue
		one := allocated(func() { chain = chainOf(byKey(issues), facts.Parents) })
		listed := allocated(func() { openBeneathIn(heldRoot, chain) })
		beyond := int64(pass) - int64(read) - int64(listed)
		builds := float64(beyond) / float64(one)
		t.Logf("the pass, its line and its held list allocated %d bytes: %d for the read of the records above it, %d for the list of the held root's tree, and %.2f chains of %d bytes",
			pass, read, listed, builds, one)
		if builds < 0.5 || builds > 1.5 {
			t.Fatalf("the pass, its line and its held list built the parent chain %.2f times over (%d bytes beyond the read and the list, %d bytes a chain), want once: every walk reads the pass's one chain (passChain)",
				builds, beyond, one)
		}
	})
}
