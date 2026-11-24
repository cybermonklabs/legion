package admit

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// rmOpenChild is a Dispatch child of the implementing member that Legion does not run: it stands
// in backlog, so Dispatch shows it open under that member (dispatch://INFRA-3320 R9).
const rmOpenChild = "LEGION-310"

// dispatch://INFRA-3320 R9 over dispatch://INFRA-5471's return: a re-admitted tree's first run
// brings its mid-phase members back (admit's startMidPhaseMembers, workflow's StartPromotedWorker),
// and a member with an open Dispatch child under it gets no worker back. It moves to
// awaiting_children holding the start of the phase it stands in, whichever run brings it back —
// the pass that finds the tree running once its design gate opened first, or the root's promotion
// — with no start of its phase worker and nothing that stops or wakes one. The control row is the
// same tree with no open child, whose member gets its phase start back, waiting for a slot. The
// other members come back as #5303 brings them back either way.
func TestAReadmittedMemberWithAnOpenChildComesBackWaitingOnIt(t *testing.T) {
	for _, run := range []struct {
		name  string
		first func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{name: "the design gate opens first", first: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			registerApprovedSpec(t, pool, admission, "root-gate")
		}},
		{name: "the promotion comes first", first: promoteRoot},
	} {
		for _, row := range []struct {
			name  string
			child bool
		}{
			{name: "an open child holds the member", child: true},
			{name: "the control: no open child", child: false},
		} {
			t.Run(run.name+"/"+row.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission, _ := readmittedAdmission(t, 1)
				readmittedTree(t, pool, admission, false)
				if row.child {
					seedMirrorChild(t, pool, rmOpenChild, "backlog", rmImplementing)
				}

				run.first(t, pool, admission)

				member := issue(t, pool, rmImplementing)
				if got, want := memberStarts(t, pool, rmMerging), []memberStart{{role: claim.RoleMerger, withTask: true}}; !reflect.DeepEqual(got, want) {
					t.Fatalf("the starts of the merging %s = %+v, want %+v: the other members come back as before", rmMerging, got, want)
				}
				if !row.child {
					want := []memberStart{{role: claim.RoleImplementer, withTask: true, needsSlot: true}}
					if got := memberStarts(t, pool, rmImplementing); member.Phase != phase.Implementing || !reflect.DeepEqual(got, want) {
						t.Fatalf("the implementing %s = phase %s with starts %+v, want implementing with %+v: its phase start, waiting for a slot",
							rmImplementing, member.Phase, got, want)
					}
					return
				}
				if member.Phase != phase.AwaitingChildren || member.ChildrenWait == nil || member.ChildrenWait.Start != phase.Implementing {
					t.Fatalf("the member %s with an open child = phase %s, wait %+v; want awaiting_children holding the start of implementing",
						rmImplementing, member.Phase, member.ChildrenWait)
				}
				if len(member.ChildrenWait.Children) != 1 || member.ChildrenWait.Children[0] != rmOpenChild {
					t.Fatalf("the wait of %s names the children %v, want %s alone", rmImplementing, member.ChildrenWait.Children, rmOpenChild)
				}
				noWorkerLaunch(t, pool, rmImplementing, claim.RoleImplementer)
				if got := memberStarts(t, pool, rmImplementing); len(got) != 0 {
					t.Fatalf("the starts of the waiting %s = %+v, want none: no role of it comes back while its child is open", rmImplementing, got)
				}
			})
		}
	}

	for _, row := range []struct {
		name          string
		fileWhileHeld bool
	}{
		{name: "a new child stops an owed start while catch-up holds the root", fileWhileHeld: true},
		{name: "a new child stops an owed start on the root's return"},
	} {
		t.Run(row.name, func(t *testing.T) {
			const newChild = "LEGION-311"
			pool := migratedPool(t)
			admission, _ := readmittedAdmission(t, 1)
			readmittedTree(t, pool, admission, false)
			admission.HoldForCatchUp(rmRoot)
			// A mid-phase member that was already waiting stays there through re-admission.
			member := issue(t, pool, rmImplementing)
			member.Phase = phase.AwaitingChildren
			member.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: pwTask(rmImplementing, phase.Implementing),
				Reason: "an open child held implementing", Since: fixedNow, Children: []string{rmOpenChild}}
			member.ChildrenWaitedAt = &fixedNow
			putIssue(t, pool, member)
			seedMirrorChild(t, pool, rmOpenChild, "backlog", rmImplementing)
			registerApprovedSpec(t, pool, admission, "root-gate")

			moveChild(t, pool, admission, "child-done", rmOpenChild, rmImplementing, "done", 2)

			want := []memberStart{{role: claim.RoleImplementer, withTask: true, needsSlot: true}}
			if member, starts := issue(t, pool, rmImplementing), memberStarts(t, pool, rmImplementing); member.Phase != phase.Implementing || !reflect.DeepEqual(starts, want) || holdsSlot(t, pool, rmImplementing) {
				t.Fatalf("the member after its child closes = phase %s, starts %+v, slots %+v; want implementing with its start waiting for a slot",
					member.Phase, starts, slots(t, pool))
			}
			if !issue(t, pool, rmRoot).MembersOwed {
				t.Fatal("catch-up must still hold the root's member return")
			}
			if row.fileWhileHeld {
				moveChild(t, pool, admission, "new-child", newChild, rmImplementing, "backlog", 1)
				if member := issue(t, pool, rmImplementing); member.Phase != phase.AwaitingChildren {
					t.Fatalf("the member with a new child while its root is held = phase %s, want awaiting_children", member.Phase)
				}
				if !issue(t, pool, rmRoot).MembersOwed {
					t.Fatal("stopping the member must not bring back the root's members")
				}
			} else {
				// The release pass sees the open child and the owed start for the first time together.
				seedMirrorChild(t, pool, newChild, "backlog", rmImplementing)
			}

			admission.ReleaseCatchUp(rmRoot)
			spPass(t, pool, admission)

			member = issue(t, pool, rmImplementing)
			if member.Phase != phase.AwaitingChildren || member.ChildrenWait == nil ||
				member.ChildrenWait.Start != phase.Implementing || !slices.Equal(member.ChildrenWait.Children, []string{newChild}) ||
				!strings.Contains(member.ChildrenWait.Reason, "its start of implementing was written and had not reached the implementer") {
				t.Fatalf("the member after releasing catch-up = phase %s, wait %+v; want awaiting_children on %s with its implementer's start held undelivered",
					member.Phase, member.ChildrenWait, newChild)
			}
			noWorkerLaunch(t, pool, rmImplementing, claim.RoleImplementer)
			if starts := memberStarts(t, pool, rmImplementing); len(starts) != 0 {
				t.Fatalf("the member after releasing catch-up has starts %+v, want no role brought back while it waits", starts)
			}
			if issue(t, pool, rmRoot).MembersOwed {
				t.Fatal("the root's return should have cleared its mark")
			}
		})
	}
}

// The tree of the carried-root case: a root another tree handed to Legion carrying its slot in
// implementing (record.QueuedRoot), and a member under it in the middle of implementing.
const (
	crRoot      = spParent // pwDeliver finishes starts of spParent.
	crMember    = "LEGION-401"
	crOpenChild = "LEGION-402"
)

// noWorkerLaunch fails if a start of key's phase role, of work or of news alone, stands in the
// outbox (startsOf reads every one), or a row that retires the task or tells the role it ended.
func noWorkerLaunch(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) {
	t.Helper()
	for _, start := range startsOf(t, pool, key) {
		if start.role == role {
			t.Fatalf("the waiting %s has a start of its %s queued (%+v), want none: no worker is launched while its child is open", key, role, start)
		}
	}
	if retired := spRetired(t, pool, key); len(retired) != 0 {
		t.Fatalf("the waiting %s has task retirements %+v, want none: no worker of this generation took the phase", key, retired)
	}
	if ended := pwPayloads[record.PhaseEnded](t, pool, key); len(ended) != 0 {
		t.Fatalf("the waiting %s has phase-ended messages %+v, want none: no worker of this generation took the phase", key, ended)
	}
}

// dispatch://INFRA-3320 R9 at the promotion of a root handed over carrying its slot in
// implementing, all three sides in one tree (Verification v13's k12, INFRA-3320 item 45):
//
//  1. the root's carried implementing starts at its promotion, with its worker's launch queued,
//     open child or not: k12 exempts that start alone among carried worker phases;
//  2. a member under it in the middle of its phase, with an open Dispatch child under it, goes
//     to awaiting_children at that same promotion holding the start of its phase, with no launch
//     of its worker queued (admit's startMidPhaseMembers, workflow's StartPromotedWorker);
//  3. the root, with an open Dispatch child of its own (the member, which Dispatch shows
//     in_progress under it), is a parent with an open child once its tree runs, so once the outbox
//     has delivered its start, the first admission pass after that moves it to awaiting_children
//     holding its carried phase, with one phase-ended message to its worker (R10, workflow's
//     StopForChildren).
//
// Each runs with the root's design gate opened before the promotion and with none open at it.
// The control row has no open Dispatch child anywhere: the member's worker is launched at the
// promotion beside the root's, and the pass after it moves neither.
func TestAPromotedCarriedRootStartsItsPhaseWhileAMemberUnderItWaitsOnItsChild(t *testing.T) {
	for _, gate := range []struct {
		name string
		open bool
	}{
		{name: "the design gate opens first", open: true},
		{name: "the promotion finds no gate open", open: false},
	} {
		for _, row := range []struct {
			name  string
			child bool
		}{
			{name: "an open child holds the member", child: true},
			{name: "the control: no open child", child: false},
		} {
			t.Run(gate.name+"/"+row.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := pwAdmission(t, 2)
				carriedRoot(t, pool, crRoot, phase.Implementing, "A")
				takeSlot(t, pool, crRoot)
				midPhaseChild(t, pool, crRoot, crMember, phase.Implementing, "in_progress")
				if gate.open {
					seedGate(t, pool, record.DesignGate{Issue: crRoot, ArtifactID: "artifact-400", LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true})
				}
				if row.child {
					seedMirrorChild(t, pool, crMember, "in_progress", crRoot)
					seedMirrorChild(t, pool, crOpenChild, "backlog", crMember)
				}

				spPass(t, pool, admission)

				// 1. The root's carried implementing starts despite the open child.
				root := issue(t, pool, crRoot)
				if root.Phase != phase.Implementing || root.ChildrenWait != nil || root.HandedOverFrom != "" || !treeOpen(t, pool, crRoot) {
					t.Fatalf("the promoted root = phase %s, held start %+v, handed over from %q, tree open %t; want implementing in a tree of its own, nothing held",
						root.Phase, root.ChildrenWait, root.HandedOverFrom, treeOpen(t, pool, crRoot))
				}
				if starts := pwWorkerStarts(t, pool, crRoot); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task == "" {
					t.Fatalf("the root's worker starts = %+v, want the implementer's start of the phase it carries", starts)
				}

				// 2. The member under it waits on its open child, with no launch queued.
				member := issue(t, pool, crMember)
				if !row.child {
					if starts := pwWorkerStarts(t, pool, crMember); member.Phase != phase.Implementing || len(starts) != 1 || starts[0].role != claim.RoleImplementer {
						t.Fatalf("the member %s = phase %s with worker starts %+v, want implementing with its implementer's start", crMember, member.Phase, starts)
					}
				} else {
					if member.Phase != phase.AwaitingChildren || member.ChildrenWait == nil || member.ChildrenWait.Start != phase.Implementing {
						t.Fatalf("the member %s with an open child = phase %s, wait %+v; want awaiting_children holding the start of implementing",
							crMember, member.Phase, member.ChildrenWait)
					}
					noWorkerLaunch(t, pool, crMember, claim.RoleImplementer)
				}
				if ended := pwPayloads[record.PhaseEnded](t, pool, crRoot); len(ended) != 0 {
					t.Fatalf("phase-ended messages to the root at its promotion = %+v, want none", ended)
				}

				// 3. The outbox delivers the root's start, and the first pass after that stops the
				// root that has an open child, its worker told that the phase ended.
				token := putWorkerClaim(t, pool, crRoot, crRoot, claim.RoleImplementer, root.Generation, supervise.StateWorking, "")
				pwDeliver(t, pool, token, pnWorkerStart(t, pool, crRoot, claim.RoleImplementer))
				spPass(t, pool, admission)

				root = issue(t, pool, crRoot)
				ended := pwPayloads[record.PhaseEnded](t, pool, crRoot)
				if !row.child {
					if root.Phase != phase.Implementing || len(ended) != 0 {
						t.Fatalf("the root after the next pass = phase %s with phase-ended messages %+v, want implementing with none", root.Phase, ended)
					}
					return
				}
				if root.Phase != phase.AwaitingChildren || root.ChildrenWait == nil || root.ChildrenWait.Start != phase.Implementing {
					t.Fatalf("the root after the next pass = phase %s, held start %+v; want awaiting_children holding its carried implementing",
						root.Phase, root.ChildrenWait)
				}
				if len(ended) != 1 || ended[0].Role != claim.RoleImplementer || ended[0].To != phase.AwaitingChildren {
					t.Fatalf("phase-ended messages to the root = %+v, want one to its implementer, into awaiting_children", ended)
				}
				if member := issue(t, pool, crMember); member.Phase != phase.AwaitingChildren {
					t.Fatalf("the member after the next pass = phase %s, want still awaiting_children", member.Phase)
				}
				noWorkerLaunch(t, pool, crMember, claim.RoleImplementer)
			})
		}
	}
}
