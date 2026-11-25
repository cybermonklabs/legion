package admit

import (
	"context"
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

// rmOpenChild is a Dispatch child of a mid-phase member that Legion does not run: it stands in
// backlog, so Dispatch shows it open under that member (dispatch://INFRA-3320 R9).
const rmOpenChild = "LEGION-310"

// dispatch://INFRA-3320 R9 over dispatch://INFRA-5471's return: a re-admitted tree's first run
// brings its mid-phase members back (admit's startMidPhaseMembers, workflow's StartPromotedWorker),
// and a member with an open Dispatch child under it gets no role back. It moves to
// awaiting_children holding the start of the phase it stands in, whichever run brings it back —
// the pass that finds the tree running once its design gate opened first, or the root's promotion
// — with no start of any role of it, nothing that stops or wakes one, and no start of its step.
// That holds for the implementing member, whose phase worker the return starts, and for the member
// in accepting, whose role is an architect that no phase start brings back (workflow's
// StopPromotedWorker). The control row is the same tree with no open child: its implementing
// member gets its phase start back, waiting for a slot, and its member in accepting gets back each
// role of it that ran, with no task, and the start of its step. The other members come back as
// #5303 brings them back either way.
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
			name string
			// held is the member the open child is filed under, "" for the control, stands the
			// phase it stands in and role that phase's role.
			held   string
			stands phase.Phase
			role   claim.Role
		}{
			{name: "an open child holds the implementing member", held: rmImplementing, stands: phase.Implementing, role: claim.RoleImplementer},
			{name: "an open child holds the accepting member", held: rmAccepting, stands: phase.Accepting, role: claim.RoleArchitect},
			{name: "the control: no open child"},
		} {
			t.Run(run.name+"/"+row.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission, _ := readmittedAdmission(t, 1)
				readmittedTree(t, pool, admission, false)
				if row.held != "" {
					seedMirrorChild(t, pool, rmOpenChild, "backlog", row.held)
				}

				run.first(t, pool, admission)

				if got, want := memberStarts(t, pool, rmMerging), []memberStart{{role: claim.RoleMerger, withTask: true}}; !reflect.DeepEqual(got, want) {
					t.Fatalf("the starts of the merging %s = %+v, want %+v: the other members come back as before", rmMerging, got, want)
				}
				if row.held == "" {
					want := []memberStart{{role: claim.RoleImplementer, withTask: true, needsSlot: true}}
					if member, got := issue(t, pool, rmImplementing), memberStarts(t, pool, rmImplementing); member.Phase != phase.Implementing || !reflect.DeepEqual(got, want) {
						t.Fatalf("the implementing %s = phase %s with starts %+v, want implementing with %+v: its phase start, waiting for a slot",
							rmImplementing, member.Phase, got, want)
					}
					want = []memberStart{{role: claim.RoleArchitect}, {role: claim.RoleImplementer}}
					if member, got, steps := issue(t, pool, rmAccepting), memberStarts(t, pool, rmAccepting), stepStarts(t, pool, rmAccepting); member.Phase != phase.Accepting ||
						!reflect.DeepEqual(got, want) || len(steps) != 1 {
						t.Fatalf("the accepting %s = phase %s with starts %+v and step starts %+v, want accepting with %+v and one step start: each role of it that ran, with no task",
							rmAccepting, member.Phase, got, steps, want)
					}
					return
				}
				member := issue(t, pool, row.held)
				if member.Phase != phase.AwaitingChildren || member.ChildrenWait == nil || member.ChildrenWait.Start != row.stands {
					t.Fatalf("the member %s with an open child = phase %s, wait %+v; want awaiting_children holding the start of %s",
						row.held, member.Phase, member.ChildrenWait, row.stands)
				}
				if len(member.ChildrenWait.Children) != 1 || member.ChildrenWait.Children[0] != rmOpenChild {
					t.Fatalf("the wait of %s names the children %v, want %s alone", row.held, member.ChildrenWait.Children, rmOpenChild)
				}
				noWorkerLaunch(t, pool, row.held, row.role)
				if got := memberStarts(t, pool, row.held); len(got) != 0 {
					t.Fatalf("the starts of the waiting %s = %+v, want none: no role of it comes back while its child is open", row.held, got)
				}
				if steps := stepStarts(t, pool, row.held); len(steps) != 0 {
					t.Fatalf("the step starts of the waiting %s = %+v, want none: its step starts again only with the return", row.held, steps)
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
	crRoot      = "LEGION-400"
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

// stepStarts is each step record row of key's outbox that starts a run (stepWrites), in the order
// they were written.
func stepStarts(t *testing.T, pool *pgxpool.Pool, key string) []record.StepRecord {
	t.Helper()
	return slices.DeleteFunc(stepWrites(t, pool, key), func(w record.StepRecord) bool { return w.EndedAt != nil })
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
// promotion beside the root's, and the pass after it leaves the root in implementing, told nothing.
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

				// 1. The root's carried implementing starts, open child or not.
				root := issue(t, pool, crRoot)
				if root.Phase != phase.Implementing || root.ChildrenWait != nil || root.HandedOverFrom != "" || !treeOpen(t, pool, crRoot) {
					t.Fatalf("the promoted root = phase %s, held start %+v, handed over from %q, tree open %t; want implementing in a tree of its own, nothing held",
						root.Phase, root.ChildrenWait, root.HandedOverFrom, treeOpen(t, pool, crRoot))
				}
				if starts := pwWorkerStarts(t, pool, crRoot); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task == "" {
					t.Fatalf("the root's worker starts = %+v, want the implementer's start of the phase it carries", starts)
				}

				// 2. The member under it waits on its open child with no launch queued; in the control
				// row it has its implementer's start.
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

				// 3. The outbox delivers the root's start, and the first pass after that stops a root
				// that has an open child, its worker told that the phase ended; the control row's root
				// stays in implementing, told nothing.
				token := putWorkerClaim(t, pool, crRoot, crRoot, claim.RoleImplementer, root.Generation, supervise.StateWorking, "")
				pwDeliver(t, pool, crRoot, token, pnWorkerStart(t, pool, crRoot, claim.RoleImplementer))
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

// dispatch://INFRA-3320 R9 over a re-admitted tree a daemon of the release before promoted
// (promotedByEarlierRelease). Its promotion brought the members back and never cleared the mark of
// their return, so the first pass of this release that finds the root's design gate open clears
// the mark and starts no member. A member of that tree in the middle of implementing, with an open
// Dispatch child under it, is a started parent with an open child on that same pass: the pass moves
// it to awaiting_children holding its phase, with one phase-ended message to its worker (R10,
// workflow's StopForChildren). The return that would have decided how to hold it starts nothing, so
// the stop does not leave the member to it (startsMembersThisPass). The control row is the same
// tree with no open child, whose member stays in implementing, told nothing.
func TestAMemberOfARootAnEarlierReleasePromotedStopsOnItsOpenChildOnTheFirstPass(t *testing.T) {
	for _, row := range []struct {
		name  string
		child bool
	}{
		{name: "an open child holds the member", child: true},
		{name: "the control: no open child", child: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			readmittedTree(t, pool, admission, false)
			earlierReleasePromotes(t, pool, admission)
			// The member's worker took the start that promotion queued, so it is in the middle of
			// implementing when its child is filed: the outbox delivered that start to the
			// implementer's claim, which works it.
			token := pwRoleToken(t, rmImplementing, claim.RoleImplementer)
			pwDeliver(t, pool, rmImplementing, token, pnWorkerStart(t, pool, rmImplementing, claim.RoleImplementer))
			if _, err := pool.Exec(context.Background(), `update claims set state = $2, suspended_by = '' where token = $1`,
				string(token), string(supervise.StateWorking)); err != nil {
				t.Fatalf("set the implementer of %s working: %v", rmImplementing, err)
			}
			takeSlot(t, pool, rmImplementing)
			if row.child {
				seedMirrorChild(t, pool, rmOpenChild, "backlog", rmImplementing)
			}
			if root := issue(t, pool, rmRoot); !root.MembersOwed || !holdsSlot(t, pool, rmRoot) {
				t.Fatalf("the root the earlier release promoted = %#v, slots %+v; want it still marked and holding its slot: the case's premise", root, slots(t, pool))
			}
			if member, starts := issue(t, pool, rmImplementing), memberStarts(t, pool, rmImplementing); member.Phase != phase.Implementing || len(starts) != 0 {
				t.Fatalf("the member %s before the pass = phase %s with starts %+v; want implementing with its start delivered: the case's premise", rmImplementing, member.Phase, starts)
			}
			openRootGate(t, pool)

			spPass(t, pool, admission)

			if issue(t, pool, rmRoot).MembersOwed {
				t.Fatal("the root the earlier release promoted still owes its members' return after the pass, want its stale mark cleared")
			}
			member := issue(t, pool, rmImplementing)
			ended := pwPayloads[record.PhaseEnded](t, pool, rmImplementing)
			if !row.child {
				if member.Phase != phase.Implementing || len(ended) != 0 {
					t.Fatalf("the member %s with no open child after the first pass = phase %s with phase-ended messages %+v, want implementing with none",
						rmImplementing, member.Phase, ended)
				}
				return
			}
			if member.Phase != phase.AwaitingChildren || member.ChildrenWait == nil || member.ChildrenWait.Start != phase.Implementing ||
				!slices.Equal(member.ChildrenWait.Children, []string{rmOpenChild}) {
				t.Fatalf("the member %s with an open child after the first pass = phase %s, wait %+v; want awaiting_children holding the start of implementing on %s: the pass that finds its tree running stops it",
					rmImplementing, member.Phase, member.ChildrenWait, rmOpenChild)
			}
			if len(ended) != 1 || ended[0].Role != claim.RoleImplementer || ended[0].To != phase.AwaitingChildren {
				t.Fatalf("phase-ended messages to the member %s = %+v, want one to its implementer, into awaiting_children", rmImplementing, ended)
			}
		})
	}
}
