package admit

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tree a release here takes a member out of and the tree that takes it: closedTree's root is
// finished with no linger left to run, which is the tree C4 of dispatch://INFRA-3107 calls
// closed, and liveTree is the tree whose architect releases its members.
const liveTree, liveTreeChild, closedTree = "LEGION-168", "LEGION-169", "LEGION-500"

// closedTreeRepo is the repository the closed tree ran on, which is not the live tree's: an
// adopted member keeps the repository it was recorded with, and a test that ran both trees on one
// repository could not see that.
const closedTreeRepo, closedTreeBranch = "acme/tools", "release/v7.1"

// closedRoot is a tree root nothing runs again: finished (record.DoneStatus) at the generation it
// closed on, with a linger deadline that has run out, which is the root whose members another
// tree may take.
//
// The deadline is set rather than left nil because that is the shape every closed root has: the
// close arms it (workflow's beginLinger) and nothing but a re-admission clears it, so a root
// carrying none is one no close ever made. The route reads whether that linger is still running
// (record.Lingering) rather than whether one was ever armed, and this is the case that proves it.
func closedRoot(key string, generation uint64) record.Issue {
	ran := fixedNow.Add(-time.Hour)
	return record.Issue{Key: key, Repo: closedTreeRepo, DefaultBranch: closedTreeBranch, GitHubRepo: closedTreeRepo,
		Project: testProject, Title: key, Tree: key, Phase: phase.Done, Generation: generation, LingerUntil: &ran,
		Status: record.DoneStatus, DispatchStatus: record.DoneStatus, Rank: "C", HandedOver: true, LastDispatchSeq: 1}
}

// treeMember is a member of tree that never started a phase: admitted and todo at generation, on
// the repository its tree ran on, under the Dispatch parent parent.
func treeMember(key, tree, parent, rank string, generation uint64) record.Issue {
	return record.Issue{Key: key, Repo: closedTreeRepo, DefaultBranch: closedTreeBranch, GitHubRepo: closedTreeRepo,
		Project: testProject, Title: key, Tree: tree, Parent: record.ParentOf(parent), Phase: phase.Admitted,
		Generation: generation, Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, LastDispatchSeq: 1}
}

// releaseInto applies one architect's release of children recorded outside its tree, the fact the
// API's wave applies (api's adoptReleasedChildren), and answers the refusal admission gave it,
// nil when the wave was accepted. Each call needs an event id of its own: intake commits a
// refused fact, so a second call under one id would be a duplicate and apply nothing.
func releaseInto(t *testing.T, pool *pgxpool.Pool, admission *Admission, id, tree string, children ...intake.AdoptedChild) *intake.Refusal {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", id, intake.ChildAdoption{Tree: tree, Children: children}, admission.engine, admission)
	if err != nil {
		t.Fatalf("release %v into the tree of %s: %v", children, tree, err)
	}
	return result.Refusal
}

// snapshot is the record of each key, for a comparison that a pass moved none of them.
func snapshot(t *testing.T, pool *pgxpool.Pool, keys ...string) map[string]record.Issue {
	t.Helper()
	got := make(map[string]record.Issue, len(keys))
	for _, key := range keys {
		got[key] = issue(t, pool, key)
	}
	return got
}

// heldReasons is the word admission.held gives each issue of the project, read through the
// projection the state's own body is built from (internal/projection), which is record.HeldList
// over its own candidate read rather than the reason any drop site wrote.
func heldReasons(t *testing.T, pool *pgxpool.Pool, admission *Admission) map[string]record.HeldReason {
	t.Helper()
	reasons := map[string]record.HeldReason{}
	inTx(t, pool, func(tx pgx.Tx) {
		state, err := projection.Project(context.Background(), tx, record.NewStore(), testProject,
			admission.engine.GatedSteps(), nil, nil, fixedNow)
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		for _, row := range state.Admission.Held {
			reasons[row.Issue] = row.Reason
		}
	})
	return reasons
}

// assertHeld asserts each key stands in admission.held with the word want.
func assertHeld(t *testing.T, pool *pgxpool.Pool, admission *Admission, want record.HeldReason, keys ...string) {
	t.Helper()
	held := heldReasons(t, pool, admission)
	for _, key := range keys {
		if got := held[key]; got != want {
			t.Fatalf("admission.held holds %s for %q, want %q: %#v", key, got, want, held)
		}
	}
}

// handoverRows is the number of hand-over fences standing, the rows a closed root's linger wrote
// (workflow's handOver) and an adoption deletes.
func handoverRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from handovers`).Scan(&rows); err != nil {
		t.Fatalf("count hand-over fences: %v", err)
	}
	return rows
}

// dispatchWrites is the number of Dispatch status writes queued for key, or for every issue when
// no key is given.
func dispatchWrites(t *testing.T, pool *pgxpool.Pool, keys ...string) int {
	t.Helper()
	writes := 0
	for _, effect := range effects(t, pool) {
		if _, ok := effect.payload.(record.StatusWrite); !ok {
			continue
		}
		if len(keys) == 0 {
			writes++
			continue
		}
		for _, key := range keys {
			if effect.issue == key {
				writes++
			}
		}
	}
	return writes
}

// seedClosedTreeAndItsTaker records the live tree that takes members, a finished child of it, and
// a closed tree holding one member at admitted (C4's member: it never started a phase and holds
// no admission slot). It answers the member's key.
func seedClosedTreeAndItsTaker(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	const member = "LEGION-501"
	seedOpenTree(t, pool, liveTree)
	putIssue(t, pool, record.Issue{Key: liveTreeChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: liveTreeChild, Tree: liveTree, Parent: record.ParentOf(liveTree), Phase: phase.Done,
		Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A1", LastDispatchSeq: 1})
	putIssue(t, pool, closedRoot(closedTree, 4))
	putIssue(t, pool, treeMember(member, closedTree, closedTree, "B", 2))
	return member
}

// A member of a closed tree that never started a phase is adopted by the tree whose architect
// releases it, and that release makes a root of none of them: C4 of dispatch://INFRA-3107. The
// record enters the taking tree admitted, keeps the repository it was recorded with and takes the
// generation it had raised by one, and nothing moves it but an architect's release — a pass over
// the same records adopts nothing, and until a release comes the member stands in admission.held
// with the word tree_closed.
//
// The refusals the route keeps are read beside it: a member of a tree that is live is another
// architect's work, a member of a parked tree and one of a tree still in its linger are their own
// trees' to answer, a member that already started a phase is not covered, a member the linger's
// hand-over made a root of its own stays one, and a member holding an admission slot is work the
// daemon started. Each refuses the whole wave it stands in, which writes nothing at all.
func TestAClosedTreesMemberIsAdoptedByTheTreeThatTakesIt(t *testing.T) {
	t.Run("the release takes the members the architect named", func(t *testing.T) {
		const plain, blocked, deep, blocker = "LEGION-501", "LEGION-502", "LEGION-503", "LEGION-900"
		pool := migratedPool(t)
		// One slot, which the live tree's root holds: an adopted member waits in the line for one
		// of its own, so what this test reads is where the adoption left it.
		admission := quiet(t, 1)
		seedClosedTreeAndItsTaker(t, pool)
		putIssue(t, pool, treeMember(blocked, closedTree, closedTree, "C", 3))
		putIssue(t, pool, treeMember(deep, closedTree, closedTree, "D", 5))
		seedMirror(t, pool, blocked, "todo", blocker)
		seedMirror(t, pool, blocker, "in_progress")
		// The fence the closed tree's linger left on one member, which the adoption deletes.
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutHandover(context.Background(), tx, record.Handover{Issue: plain, Tree: closedTree, Generation: 4}); err != nil {
				t.Fatalf("seed the hand-over fence: %v", err)
			}
		})
		members := []string{plain, blocked, deep}
		before := snapshot(t, pool, members...)

		// Nothing is adopted by a pass alone, and a second pass over the same records writes no
		// further hand-over row: each member stands where it stood, held for its closed tree.
		for _, id := range []string{"tick-1", "tick-2"} {
			applyFact(t, pool, admission, id, intake.ControllerTick{})
			if got := snapshot(t, pool, members...); !reflect.DeepEqual(got, before) {
				t.Fatalf("the records after %s = %#v, want them as they stood, %#v: only an architect's release adopts a member", id, got, before)
			}
			if rows := handoverRows(t, pool); rows != 1 {
				t.Fatalf("hand-over fences after %s = %d, want the one the closed tree's linger left", id, rows)
			}
			if writes := dispatchWrites(t, pool); writes != 0 {
				t.Fatalf("Dispatch writes after %s = %d, want none: the daemon starts nothing of a closed tree", id, writes)
			}
			assertHeld(t, pool, admission, record.HeldTreeClosed, members...)
			assertWaiting(t, pool, nil)
		}

		if refusal := releaseInto(t, pool, admission, "release", liveTree,
			intake.AdoptedChild{Issue: plain, Parent: liveTree},
			intake.AdoptedChild{Issue: blocked, Parent: liveTree},
			// Dispatch shows this one at a depth of two under the live tree's root.
			intake.AdoptedChild{Issue: deep, Parent: liveTreeChild},
		); refusal != nil {
			t.Fatalf("the release of %v = %#v, want it accepted", members, refusal)
		}

		for _, key := range members {
			got, was := issue(t, pool, key), before[key]
			if got.Tree != liveTree || got.Phase != phase.Admitted || got.Generation != was.Generation+1 {
				t.Fatalf("%s = %#v, want it admitted in the tree of %s at generation %d", key, got, liveTree, was.Generation+1)
			}
			if got.Repo != closedTreeRepo || got.DefaultBranch != closedTreeBranch || got.GitHubRepo != closedTreeRepo {
				t.Fatalf("%s = %#v, want it on the repository it was recorded with, %s at %s", key, got, closedTreeRepo, closedTreeBranch)
			}
			if lifecycleRows(t, pool, key) != 0 {
				t.Fatalf("%s has a tree lifecycle of its own, want none: the release made a root of none of them", key)
			}
			if want := []string{key + " joined the tree of " + liveTree + " from the admission queue"}; !reflect.DeepEqual(joinedNotices(t, pool, key), want) {
				t.Fatalf("child-status notices of %s = %q, want %q", key, joinedNotices(t, pool, key), want)
			}
		}
		if got := issue(t, pool, deep); got.Parent == nil || *got.Parent != liveTreeChild {
			t.Fatalf("%s's parent = %v, want %s, the parent the release read", deep, issue(t, pool, deep).Parent, liveTreeChild)
		}
		if rows := handoverRows(t, pool); rows != 0 {
			t.Fatalf("hand-over fences after the release = %d, want none: the adoption deletes the fence it finds", rows)
		}
		if writes := dispatchWrites(t, pool); writes != 0 {
			t.Fatalf("Dispatch writes after the release = %d, want none: the adoption writes the record alone", writes)
		}
		// The two the line can offer a slot wait in it; the one an unfinished Dispatch issue
		// holds is recorded and held where it stands, as a released child a link holds is today.
		assertWaiting(t, pool, []string{plain, deep})
		assertHeld(t, pool, admission, record.HeldDispatchBlocker, blocked)
		assertSlots(t, pool, []record.Slot{{Issue: liveTree, Index: 0, AdmittedAt: fixedNow}})
		if starts := architectStarts(t, pool, plain); starts != 0 {
			t.Fatalf("%s has %d architect starts queued, want none: it runs in the tree of %s", plain, starts, liveTree)
		}
	})

	// A release may name a child Dispatch shows in triage, backlog or icebox (api's
	// releasableChild). Such a member enters the live tree as a parked child at done, which a
	// person's todo runs again (workflow's ReenterChild), rather than at admitted with a status
	// that is out of the workflow, which no list of the state would name and nothing would start.
	// It is Adopt's own branch for the same record, and AdoptMember keeps it.
	t.Run("a member a person parked enters the tree parked", func(t *testing.T) {
		const parked = "LEGION-505"
		pool := migratedPool(t)
		admission := quiet(t, 1)
		seedClosedTreeAndItsTaker(t, pool)
		member := treeMember(parked, closedTree, closedTree, "E", 2)
		member.Status, member.DispatchStatus = "backlog", "backlog"
		putIssue(t, pool, member)

		if refusal := releaseInto(t, pool, admission, "release-parked", liveTree,
			intake.AdoptedChild{Issue: parked, Parent: liveTree}); refusal != nil {
			t.Fatalf("the release of %s = %#v, want it accepted", parked, refusal)
		}

		got := issue(t, pool, parked)
		if got.Tree != liveTree || got.Phase != phase.Done || got.Generation != member.Generation+1 {
			t.Fatalf("%s = %#v, want it done in the tree of %s at generation %d", parked, got, liveTree, member.Generation+1)
		}
		if got.Status != "backlog" || got.Repo != closedTreeRepo {
			t.Fatalf("%s = %#v, want the status a person gave it and the repository it was recorded with", parked, got)
		}
		if writes := dispatchWrites(t, pool); writes != 0 {
			t.Fatalf("Dispatch writes after the release = %d, want none: a parked member is recorded, never started", writes)
		}
		assertWaiting(t, pool, nil)
	})

	t.Run("one child it cannot take refuses the whole wave", func(t *testing.T) {
		const refused = "LEGION-601"
		for _, tc := range []struct {
			name, code, reason string
			seed               func(t *testing.T, pool *pgxpool.Pool)
			pass               bool
		}{
			{name: "a member of a tree that is live", code: "CHILD_IN_OTHER_TREE", reason: "runs in the tree of LEGION-600, not " + liveTree,
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					// Another architect's tree, running on a slot of its own.
					seedSlotted(t, pool, "LEGION-600", "E")
					putIssue(t, pool, treeMember(refused, "LEGION-600", "LEGION-600", "E1", 1))
				}},
			{name: "a member of a parked tree", code: "CHILD_IN_OTHER_TREE", reason: "runs in the tree of LEGION-600, not " + liveTree,
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					parked := closedRoot("LEGION-600", 2)
					parked.Status, parked.DispatchStatus = "backlog", "backlog"
					putIssue(t, pool, parked)
					putIssue(t, pool, treeMember(refused, "LEGION-600", "LEGION-600", "E1", 1))
				}},
			{name: "a member of a tree still in its linger", code: "CHILD_IN_OTHER_TREE", reason: "runs in the tree of LEGION-600, not " + liveTree,
				pass: true,
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					until := fixedNow.Add(time.Hour)
					lingering := closedRoot("LEGION-600", 2)
					lingering.LingerUntil = &until
					putIssue(t, pool, lingering)
					putIssue(t, pool, treeMember(refused, "LEGION-600", "LEGION-600", "E1", 1))
				}},
			{name: "a member of the closed tree that started a phase", code: "CHILD_IN_OTHER_TREE", reason: "runs in the tree of " + closedTree + ", not " + liveTree,
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					started := treeMember(refused, closedTree, closedTree, "E1", 2)
					started.Phase, started.Status, started.DispatchStatus = phase.Implementing, "in_progress", "in_progress"
					putIssue(t, pool, started)
				}},
			{name: "a member the hand-over made a root of its own", code: "CHILD_NOT_ADOPTABLE", reason: "it was handed over as a root of its own",
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					// The linger's hand-over made this one a tree of its own, which its
					// re-admission recorded (record.Issue.HandedOverFrom); C4 leaves it one.
					handed := treeMember(refused, refused, closedTree, "E1", 3)
					handed.HandedOverFrom = closedTree
					putIssue(t, pool, handed)
				}},
			{name: "a member that holds an admission slot", code: "CHILD_NOT_ADOPTABLE", reason: "it holds admission slot 1 in the tree of " + closedTree,
				seed: func(t *testing.T, pool *pgxpool.Pool) {
					putIssue(t, pool, treeMember(refused, closedTree, closedTree, "E1", 2))
					inTx(t, pool, func(tx pgx.Tx) {
						if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: refused, Index: 1, AdmittedAt: fixedNow}); err != nil {
							t.Fatalf("seed the member's slot: %v", err)
						}
					})
				}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 2)
				adoptable := seedClosedTreeAndItsTaker(t, pool)
				tc.seed(t, pool)
				before := snapshot(t, pool, adoptable, refused)
				slotsBefore := slots(t, pool)

				refusal := releaseInto(t, pool, admission, "release", liveTree,
					intake.AdoptedChild{Issue: adoptable, Parent: liveTree},
					intake.AdoptedChild{Issue: refused, Parent: liveTree})

				if refusal == nil || refusal.Status != 409 || refusal.Code != tc.code ||
					!strings.Contains(refusal.Message, refused) || !strings.Contains(refusal.Message, tc.reason) {
					t.Fatalf("the release of %s = %#v, want 409 %s naming %s and %q", refused, refusal, tc.code, refused, tc.reason)
				}
				if got := snapshot(t, pool, adoptable, refused); !reflect.DeepEqual(got, before) {
					t.Fatalf("the records after the refused wave = %#v, want them as they stood, %#v: one child it cannot take writes nothing at all", got, before)
				}
				if got := slots(t, pool); !reflect.DeepEqual(got, slotsBefore) {
					t.Fatalf("slots after the refused wave = %#v, want %#v: nothing is promoted on a refused wave", got, slotsBefore)
				}
				if writes := dispatchWrites(t, pool); writes != 0 {
					t.Fatalf("Dispatch writes after the refused wave = %d, want none", writes)
				}
				if !tc.pass {
					return
				}
				// A member of a tree still in its linger is left where it stands by a pass too,
				// held for its tree rather than queued or started.
				applyFact(t, pool, admission, "tick", intake.ControllerTick{})
				if got := snapshot(t, pool, adoptable, refused); !reflect.DeepEqual(got, before) {
					t.Fatalf("the records after a pass = %#v, want them as they stood, %#v", got, before)
				}
				assertHeld(t, pool, admission, record.HeldTreeClosed, refused)
			})
		}
	})
}
