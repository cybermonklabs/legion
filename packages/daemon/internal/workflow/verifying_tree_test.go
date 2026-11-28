package workflow

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The workflow's half of the rules beneath a merged issue (D10 of the done-by-checks plan): a
// child held under its root starts at the root's merge, a child in verifying holds the close and
// not the sign-off, a filed child in backlog holds the close, and a next change in a closed tree
// is not adopted.

// lineOffersASlot says whether the waiting line offers key one of the project's slots, read as
// an admission pass reads it (record.ReadLineFacts, record.Waiting).
func lineOffersASlot(t *testing.T, rig *waitRig, key string) bool {
	t.Helper()
	offered := false
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		issues, err := records.LiveIssues(t.Context(), tx, "LEGION")
		if err != nil {
			return err
		}
		facts, err := record.ReadLineFacts(t.Context(), tx, records, "LEGION", issues, nil)
		if err != nil {
			return err
		}
		line, _ := record.Waiting(issues, nil, facts)
		offered = slices.ContainsFunc(line, func(entry record.LineEntry) bool { return entry.Issue.Key == key })
		return nil
	})
	return offered
}

// beneath is the keys the stored wait of key names as the issues beneath it.
func (r *waitRig) beneath(key string) []string {
	r.t.Helper()
	var keys []string
	for _, item := range r.stored(key).Beneath {
		keys = append(keys, item.Key)
	}
	return keys
}

// A child held at admitted under its root in merging starts at the root's merge: a root in
// verifying is not accepted (record.Accepted), so the hold ends, the line offers the child a
// slot, and the root's close waits for it. Before the merge the hold stands and the root's own
// wait leaves the child out, which is what keeps the two from deadlocking.
func TestAHeldChildStartsAtItsRootsMerge(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.seedChild(waitChild, phase.Admitted, "todo")
	seedGate(t, rig.pool, record.DesignGate{Issue: waitRoot, ArtifactID: "spec-208", LatestVersion: 1, ApprovedVersion: new(1)})

	root, child := rig.issue(waitRoot), rig.issue(waitChild)
	members := []record.Issue{root, child}
	if !record.HeldUnderAccepted(child, recordsByKey(members)) {
		t.Fatalf("%s is not held under %s in merging: this case is the hold the merge ends", waitChild, waitRoot)
	}
	if lineOffersASlot(t, rig, waitChild) {
		t.Fatalf("the line offers %s a slot while %s is merging, want it held", waitChild, waitRoot)
	}

	rig.mergeRoot()

	rig.stands(waitRoot, phase.Verifying, "testing")
	if record.Accepted(rig.issue(waitRoot)) {
		t.Fatalf("%s in verifying reads as accepted, want not: its head merged already", waitRoot)
	}
	if !lineOffersASlot(t, rig, waitChild) {
		t.Fatalf("the line offers %s no slot after its root's merge, want it started", waitChild)
	}
	if got := rig.beneath(waitRoot); !slices.Equal(got, []string{waitChild}) {
		t.Fatalf("the stored wait's issues beneath %s = %v, want %s", waitRoot, got, waitChild)
	}
}

// recordsByKey indexes members by key, as the acceptance rules walk the chain over.
func recordsByKey(members []record.Issue) map[string]record.Issue {
	byKey := make(map[string]record.Issue, len(members))
	for _, member := range members {
		byKey[member.Key] = member
	}
	return byKey
}

// A child whose own close waits in verifying holds its parent's close and not its sign-off: the
// parent's head should carry the child's merged work, which it does, so the sign-off is taken,
// while the parent's own close waits for the child until it closes. The control is the same child
// at work in implementing, whose sign-off is refused.
func TestAChildInVerifyingHoldsTheCloseAndNotTheSignOff(t *testing.T) {
	t.Parallel()
	t.Run("the sign-off is taken", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Accepting, "needs_review")
		rig.seedChild(waitChild, phase.Verifying, "testing")
		result := rig.fact(rootsSignOff(t, rig.pool, rig.engine, waitRoot))
		if result.Refusal != nil {
			t.Fatalf("the sign-off over a child in verifying = %+v, want it taken", result.Refusal)
		}
		rig.stands(waitRoot, phase.Retro, "retro")
	})

	t.Run("control: a child at work holds the sign-off", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Accepting, "needs_review")
		rig.seedChild(waitChild, phase.Implementing, "in_progress")
		result := rig.fact(rootsSignOff(t, rig.pool, rig.engine, waitRoot))
		if result.Refusal == nil || result.Refusal.Code != signOffChildInWorkflow {
			t.Fatalf("the sign-off over a child at work = %+v, want %s", result.Refusal, signOffChildInWorkflow)
		}
	})

	t.Run("the close waits for it", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Verifying, "testing")
		rig.mergeRoot()
		if got := rig.beneath(waitRoot); !slices.Equal(got, []string{waitChild}) {
			t.Fatalf("the stored wait's issues beneath %s = %v, want %s: a child in verifying holds the close", waitRoot, got, waitChild)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
	})
}

// A child the architect filed in Dispatch and never released, standing in backlog with no record
// of its own, holds its root's close: the read answers it under the issue, and the children-first
// rule reads it as open. Its move to icebox frees the close at the next read.
func TestTheCloseWaitsForAFiledChildInBacklog(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	filed := []dispatch.IssueChild{{Key: "LEGION-290", Status: "backlog"}}

	rig.read(waitRoot, answerOf(waitRoot, filed, passing("c-after", waitRootMerge)))

	rig.stands(waitRoot, phase.Verifying, "testing")
	stored := rig.stored(waitRoot)
	if len(stored.OpenChildren) != 1 || stored.OpenChildren[0].Key != "LEGION-290" {
		t.Fatalf("the stored wait's open children = %+v, want the filed child in backlog", stored.OpenChildren)
	}

	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-290", Status: "icebox"}}, passing("c-after", waitRootMerge)))

	closedOnChecks(t, rig, waitRoot)
}

// A child's next change waits at admitted in a tree whose root is done and no longer lingers.
// Another tree's release_children names it, and it is refused: its generation and its gate stand
// as they were, since an adoption would clear the change the daemon started. The control is the
// same member on the generation its tree left it at, which the release takes.
func TestANextChangeInAClosedTreeIsNotAdopted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		nextChange uint64
		adoptable  bool
	}{
		{name: "its next change waits", nextChange: changeGeneration},
		{name: "control: no change of its own", adoptable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWaitRig(t, "")
			rig.seedRoot(phase.Done, "done")
			rig.seedChild(waitChild, phase.Admitted, "todo")
			closed := rig.issue(waitRoot)
			lingered := rig.now.Add(-time.Hour)
			closed.LingerUntil = &lingered
			seedIssue(t, rig.pool, closed)
			member := rig.issue(waitChild)
			member.Generation, member.NextChange = changeGeneration, tc.nextChange
			seedIssue(t, rig.pool, member)
			seedGate(t, rig.pool, record.DesignGate{Issue: waitChild, ArtifactID: "spec-209", LatestVersion: 1, ApprovedVersion: new(1)})
			before, gates := rig.issue(waitChild), gatesOf(t, rig.pool, waitChild)
			other := otherRoot(t, rig.pool)

			var refusal *intake.Refusal
			seedRecord(t, rig.pool, func(tx pgx.Tx) error {
				var err error
				refusal, err = rig.engine.AdoptionRefusal(context.Background(), tx, before, other.Key)
				return err
			})

			if tc.adoptable {
				if refusal != nil {
					t.Fatalf("the adoption of the control = %+v, want it taken: its tree closed and it started nothing", refusal)
				}
				return
			}
			if refusal == nil {
				t.Fatalf("the adoption of %s, whose next change waits, was taken; want it refused", waitChild)
			}
			if got := rig.issue(waitChild); got.Generation != before.Generation || got.NextChange != before.NextChange {
				t.Fatalf("%s after the refusal stands at generation %d with change %d, want %d and %d",
					waitChild, got.Generation, got.NextChange, before.Generation, before.NextChange)
			}
			if got := gatesOf(t, rig.pool, waitChild); len(got) != len(gates) {
				t.Fatalf("%s's gates after the refusal = %+v, want them as they were, %+v", waitChild, got, gates)
			}
		})
	}
}
