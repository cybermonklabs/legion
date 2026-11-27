package admit

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The started parent most cases here drive, the Dispatch child that holds it, and an issue of
// another tree that holds the cap's one slot where a case needs the cap full.
const (
	spParent = "LEGION-300"
	spChild  = "LEGION-301"
	spFiller = "LEGION-390"
)

// spMergeLine is the line that asks the implementer to end its forward-merge comment with the full
// test round it owes, which no task of a parent carries: the return asks for no forward merge
// (dispatch://INFRA-3320 R12).
const spMergeLine = "`children merged: full test round`"

// spPass runs one admission pass over the stored state in a transaction of its own, which settles
// the runs its moves end and whose commit runs the pass's commit hooks, as the pass every fact runs
// does (intake.ApplyFact).
func spPass(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, admission)
		if _, _, err := admission.promote(ctx, tx); err != nil {
			return err
		}
		return settle(ctx, tx)
	}); err != nil {
		t.Fatalf("the admission pass: %v", err)
	}
}

// spSeedParent records key as the root of a running tree standing in at, at the status that
// phase shows, holding an admission slot where slotted, with shape for whatever else the record
// carries.
func spSeedParent(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, slotted bool, shape func(*record.Issue)) {
	t.Helper()
	status := cmp.Or(workflow.PhaseStatus(at), "in_progress")
	if slotted {
		seedStarted(t, pool, key, at, func(issue *record.Issue) {
			issue.Status = status
			if shape != nil {
				shape(issue)
			}
		})
		return
	}
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Tree: key, Phase: at, Generation: 1, Status: status, Rank: key, HandedOver: true}
	if shape != nil {
		shape(&issue)
	}
	putIssue(t, pool, issue)
	seedLifecycle(t, pool, key)
}

// spWaiting records key as the root of a running tree waiting in awaiting_children, holding no
// slot, with the start it held, its task and its reason, and the moment the wait began, as the
// move leaves a parent.
func spWaiting(t *testing.T, pool *pgxpool.Pool, key string, start phase.Phase, task, reason string, shape func(*record.Issue)) time.Time {
	t.Helper()
	waited := fixedNow.Add(-2 * time.Hour)
	spSeedParent(t, pool, key, phase.AwaitingChildren, false, func(issue *record.Issue) {
		issue.ChildrenWait = &record.ChildrenWait{Start: start, Task: task, Reason: reason, Since: waited}
		issue.ChildrenWaitedAt = &waited
		if shape != nil {
			shape(issue)
		}
	})
	return waited
}

// spMirror writes what Dispatch says of key now — its status, its parent and the keys it waits
// on — at sequence seq, which a later write must pass to replace it: seedMirrorChild's row at a
// later sequence, for a test that changes what Dispatch shows.
func spMirror(t *testing.T, pool *pgxpool.Pool, key, status, parent string, seq int64, waits ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		reading := record.DispatchReading{Key: key, Project: testProject, Seq: seq,
			Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry(waits)}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, reading); err != nil {
			t.Fatalf("write the mirror of %s: %v", key, err)
		}
	})
}

// spSeedPhase records one phases row.
func spSeedPhase(t *testing.T, pool *pgxpool.Pool, row record.PhaseRow) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, row); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", row.Role, row.Issue, err)
		}
	})
}

// spPhaseRow is role's phases row of key as the record holds it.
func spPhaseRow(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := record.NewStore().Phases(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read the phases rows of %s: %v", key, err)
		}
		row = record.PhaseRowOf(rows, role)
	})
	return row
}

// spNotices is every awaiting-children notice queued for key, in outbox order.
func spNotices(t *testing.T, pool *pgxpool.Pool, key string) []record.Notice {
	t.Helper()
	return gateNotices(t, pool, key, workflow.AwaitingChildrenNoticeKind)
}

// spRetired is every task retirement queued for key (a `retire_task` supervise row), in outbox
// order.
func spRetired(t *testing.T, pool *pgxpool.Pool, key string) []record.SuperviseRequest {
	t.Helper()
	var retired []record.SuperviseRequest
	for _, request := range pwPayloads[record.SuperviseRequest](t, pool, key) {
		if request.Op == "retire_task" {
			retired = append(retired, request)
		}
	}
	return retired
}

// spLine is the project's waiting line as the pass judges it (blockers.JudgeLine over the stored
// state), entry by entry, with the kind and the part each stands in.
func spLine(t *testing.T, pool *pgxpool.Pool) []record.LineEntry {
	t.Helper()
	var line []record.LineEntry
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		if _, _, line, _, err = blockers.JudgeLine(ctx, tx, records, testProject, issues, slots, facts, nil, fixedNow); err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
	})
	return line
}

// spEntry is key's entry of line, nil where it stands in none.
func spEntry(line []record.LineEntry, key string) *record.LineEntry {
	for i := range line {
		if line[i].Issue.Key == key {
			return &line[i]
		}
	}
	return nil
}

// The admission pass moves a started parent an open Dispatch child holds into awaiting_children in
// the three cases no transition meets (dispatch://INFRA-3320 R9): a parent in accepting, one whose
// start is written and not delivered, and one whose worker is in the middle of its phase. Each
// move goes through the engine's one move, so the parent shows in_progress and carries the moment
// of the wait, its architect is told once, and no children-first notice is sent, since a started
// parent's children are no children-first wait.
func TestTheAdmissionPassStopsAStartedParentAnOpenChildHolds(t *testing.T) {
	t.Run("a parent in accepting holds accepting", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		spSeedParent(t, pool, spParent, phase.Accepting, false, nil)
		seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		wait := got.ChildrenWait
		if got.Phase != phase.AwaitingChildren || wait == nil || wait.Start != phase.Accepting || !wait.Since.Equal(fixedNow) {
			t.Fatalf("the parent = phase %s, held start %+v; want awaiting_children holding accepting since %s", got.Phase, wait, fixedNow)
		}
		if !strings.Contains(wait.Task, "Phase: accepting.") || !strings.Contains(wait.Reason, "the head the step before accepting passed waited in accepting") ||
			!strings.Contains(wait.Reason, spChild+" (todo)") {
			t.Fatalf("the held start = %+v, want accepting's task and a reason naming the wait and %s", wait, spChild)
		}
		if got.Status != "in_progress" || got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(fixedNow) {
			t.Fatalf("the parent = status %s, waited at %v; want in_progress, %s", got.Status, got.ChildrenWaitedAt, fixedNow)
		}
		if told := spNotices(t, pool, spParent); len(told) != 1 || !strings.Contains(told[0].Reason, spChild+" (todo)") {
			t.Fatalf("awaiting-children notices = %+v, want one naming %s", told, spChild)
		}
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: no worker runs in accepting", ended)
		}
		if retired := spRetired(t, pool, spParent); len(retired) != 0 {
			t.Fatalf("task retirements = %+v, want none: no worker's phase stopped", retired)
		}
		if told := childrenFirstNotices(t, pool, spParent); len(told) != 0 {
			t.Fatalf("children-first notices = %+v, want none for a started parent", told)
		}
	})

	t.Run("a parent whose start is written and not delivered holds that start", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		const retried = "Continue LEGION-300. Issue: LEGION-300. Phase: testing. Reason: retry held phase."
		spSeedParent(t, pool, spParent, phase.Testing, false, nil)
		seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
		// The retry of a held testing whose worker's start waits for the slot the parent no
		// longer holds: the retry writes its own task on the phases row as the task the tester was
		// given for entry 2 in generation 1 (workflow's restart), and its start carries it.
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleTester, Claim: "tester", Entry: 2,
			EntryTask: "Continue LEGION-300. Issue: LEGION-300. Phase: testing.", EntryRaisedAt: fixedNow.Add(-time.Hour),
			Given: record.GivenTask{Task: retried, Entry: 2, Generation: 1}})
		inTx(t, pool, func(tx pgx.Tx) {
			if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), spParent, record.SuperviseRequest{Op: "start",
				Tree: spParent, Role: claim.RoleTester, Task: retried, Generation: 1, Phase: phase.Testing, Entry: 2, ResendEntry: true}, fixedNow); err != nil {
				t.Fatalf("owe the tester's start: %v", err)
			}
		})
		seedMirrorChild(t, pool, spChild, "backlog", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != phase.Testing || got.ChildrenWait.Task != retried {
			t.Fatalf("the parent = phase %s, held start %+v; want awaiting_children holding testing with the task the phases row keeps as given for entry 2", got.Phase,
				got.ChildrenWait)
		}
		if !strings.Contains(got.ChildrenWait.Reason, "its start of testing was written and had not reached the tester") {
			t.Fatalf("the held reason = %q, want it to say the start had not been delivered", got.ChildrenWait.Reason)
		}
		if starts := startsOf(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("starts of %s = %+v, want none: the owed start is finished so it never delivers", spParent, starts)
		}
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: no worker had the phase", ended)
		}
		if retired := spRetired(t, pool, spParent); len(retired) != 0 {
			t.Fatalf("task retirements = %+v, want none: no worker had the phase", retired)
		}
		if told := spNotices(t, pool, spParent); len(told) != 1 {
			t.Fatalf("awaiting-children notices = %+v, want one", told)
		}
	})

	t.Run("a parent whose worker is mid-phase holds that phase, frees its slot and tells its worker", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		const given = "Continue LEGION-300. Issue: LEGION-300. Phase: implementing. Reason: a review asked for changes."
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
			EntryTask: given, EntryRaisedAt: fixedNow.Add(-time.Hour)})
		seedMirrorChild(t, pool, spChild, "backlog", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != phase.Implementing || got.ChildrenWait.Task != given {
			t.Fatalf("the parent = phase %s, held start %+v; want awaiting_children holding implementing with the entry's task", got.Phase, got.ChildrenWait)
		}
		ended := pwPayloads[record.PhaseEnded](t, pool, spParent)
		if len(ended) != 1 || ended[0].Role != claim.RoleImplementer || ended[0].From != phase.Implementing ||
			ended[0].To != phase.AwaitingChildren || !strings.Contains(ended[0].Reason, spChild+" (backlog)") ||
			!strings.Contains(ended[0].Reason, "the admission pass stopped the implementer's implementing phase") {
			t.Fatalf("phase-ended messages = %+v, want one to the implementer out of implementing naming %s", ended, spChild)
		}
		// The phase stops at this pass and the slot goes with it: awaiting_children keeps none.
		assertSlots(t, pool, nil)
		if starts := startsOf(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("starts of %s = %+v, want none", spParent, starts)
		}
		// The stopped worker's task of the phase goes with it, bounded by the entry the phase stood
		// on, so the return's own start is never taken.
		retired := spRetired(t, pool, spParent)
		if want := []record.SuperviseRequest{{Op: "retire_task", Tree: spParent, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Entry: 3}}; !reflect.DeepEqual(retired, want) {
			t.Fatalf("task retirements = %+v, want %+v", retired, want)
		}
		var held []record.HeldIssue
		inTx(t, pool, func(tx pgx.Tx) {
			reading, err := admission.tickRead(context.Background(), tx, func() (*passJudgement, error) { return admission.finalJudgement(context.Background(), tx) })
			if err != nil {
				t.Fatalf("read what is held: %v", err)
			}
			held = reading.held
		})
		want := record.HeldIssue{Issue: spParent, Reason: record.HeldAwaitingChildren, HeldBy: []string{spChild}}
		if !slices.ContainsFunc(held, func(row record.HeldIssue) bool { return reflect.DeepEqual(row, want) }) {
			t.Fatalf("the held list = %+v, want %+v: the held word names the open children", held, want)
		}
	})
}

// A started parent the pass may not move stays where it stands, an open child or not: a parent
// whose pull request has merged, a queued root whose tree has still to be opened, one held (its
// release is the retry's to read), and one in retro or merging, past its acceptance.
func TestTheAdmissionPassLeavesTheseParentsWhereTheyStand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      phase.Phase
		slotted bool
		// queued is a root that waits in the line for its tree to be opened: its record alone,
		// with no tree lifecycle.
		queued bool
		shape  func(*record.Issue)
		pr     record.PullRequestState
	}{
		{name: "a merged pull request", at: phase.Implementing, slotted: true, pr: record.PullRequestMerged},
		{name: "a queued root", at: phase.Implementing, queued: true},
		{name: "held from implementing", at: phase.Held, slotted: true, shape: func(issue *record.Issue) {
			issue.Hold = &record.Hold{From: phase.Implementing}
		}},
		{name: "retro", at: phase.Retro},
		{name: "merging", at: phase.Merging},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// The cap admits nothing, so the pass makes its moves and starts no queued root.
			admission := quiet(t, 0)
			if tc.queued {
				putIssue(t, pool, record.Issue{Key: spParent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: testProject, Title: spParent, Tree: spParent, Phase: tc.at, Generation: 1, Status: "todo", Rank: spParent,
					HandedOver: true, HandedOverFrom: "LEGION-299"})
			} else {
				spSeedParent(t, pool, spParent, tc.at, tc.slotted, tc.shape)
			}
			seedPullRequest(t, pool, spParent, 42, cmp.Or(tc.pr, record.PullRequestOpen))
			seedMirrorChild(t, pool, spChild, "todo", spParent)

			spPass(t, pool, admission)

			if got := issue(t, pool, spParent); got.Phase != tc.at || got.ChildrenWait != nil {
				t.Fatalf("the parent = phase %s, held start %+v; want it left in %s", got.Phase, got.ChildrenWait, tc.at)
			}
			if told := spNotices(t, pool, spParent); len(told) != 0 {
				t.Fatalf("awaiting-children notices = %+v, want none", told)
			}
			if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
				t.Fatalf("phase-ended messages = %+v, want none", ended)
			}
		})
	}
}

// A second pass over a parent the first one moved writes nothing again: the held start, the
// architect's notice and the worker's message are each the first pass's alone.
func TestASecondPassOverAMovedParentSendsNothingAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
	seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
	spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
		EntryTask: "implement it", EntryRaisedAt: fixedNow.Add(-time.Hour)})
	seedMirrorChild(t, pool, spChild, "todo", spParent)

	spPass(t, pool, admission)
	first := issue(t, pool, spParent)
	spPass(t, pool, admission)

	again := issue(t, pool, spParent)
	if again.Phase != phase.AwaitingChildren || !sameHeldStart(again.ChildrenWait, first.ChildrenWait) {
		t.Fatalf("the held start after a second pass = %+v, want the first pass's %+v", again.ChildrenWait, first.ChildrenWait)
	}
	if told := spNotices(t, pool, spParent); len(told) != 1 {
		t.Fatalf("awaiting-children notices = %+v, want still one", told)
	}
	if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 1 {
		t.Fatalf("phase-ended messages = %+v, want still one", ended)
	}
}

// When no open child is left the pass takes the parent back with nobody acting
// (dispatch://INFRA-3320 R12): a held planning or implementing starts that phase with the task it
// held, and a held testing, reviewing or accepting starts testing with a task that names the held
// start and its reason and, since the parent has a Dispatch child, asks for the round on the head
// merged with its base's tip once (pnBaseMerge). No task asks for a forward merge. The start
// raises a new phase entry, marked a move back's, clears the starting role's handoff, counts no
// pass and no round, and reads no design gate (none is registered here). The parent holds no
// slot, so with the cap full its start waits in the line among the issues whose work came back.
// The seeded wait records no children (record.ChildrenWait.Children), so no task names one.
func TestTheReturnStartsTheHeldPhaseOrTesting(t *testing.T) {
	const reason = "the tester completed testing, while these Dispatch children of LEGION-300 were open: LEGION-301 (todo)"
	for _, tc := range []struct {
		held phase.Phase
		to   phase.Phase
	}{
		{held: phase.Planning, to: phase.Planning},
		{held: phase.Implementing, to: phase.Implementing},
		{held: phase.Testing, to: phase.Testing},
		{held: phase.Reviewing, to: phase.Testing},
		{held: phase.Accepting, to: phase.Testing},
	} {
		t.Run(string(tc.held), func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			seedStarted(t, pool, spFiller, phase.Implementing, nil)
			heldTask := "Continue LEGION-300. Issue: LEGION-300. Phase: " + string(tc.held) + ". Handoff commit: head. Pull request: #42."
			waited := spWaiting(t, pool, spParent, tc.held, heldTask, reason, nil)
			seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
			seedMirrorChild(t, pool, spChild, "done", spParent)
			role := phase.RoleFor(tc.to)
			spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: role, Claim: "worker", Entry: 2, EntryTask: "before the wait",
				EntryRaisedAt: waited.Add(-time.Hour), HandoffCommit: "before", Summary: "before", Pass: 3, Rounds: 1})

			spPass(t, pool, admission)

			got := issue(t, pool, spParent)
			if got.Phase != tc.to || got.ChildrenWait != nil || got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(waited) || got.Status != workflow.PhaseStatus(tc.to) {
				t.Fatalf("the parent = phase %s, held start %+v, waited at %v, status %s; want %s with no held start, the wait's moment kept, %s",
					got.Phase, got.ChildrenWait, got.ChildrenWaitedAt, got.Status, tc.to, workflow.PhaseStatus(tc.to))
			}
			starts := startsOf(t, pool, spParent)
			if len(starts) != 1 || starts[0].role != role || !starts[0].needsSlot || starts[0].claimable {
				t.Fatalf("starts of %s = %+v, want one %s start waiting for a slot", spParent, starts, role)
			}
			work := starts[0].task
			pnAsksNoForwardMerge(t, "the "+string(role)+"'s task", work)
			if tc.to != phase.Testing && work != heldTask {
				t.Fatalf("the %s's task = %q, want the held task %q", role, work, heldTask)
			}
			if tc.to == phase.Testing {
				for _, want := range []string{"Phase: testing.", "start of " + string(tc.held), reason} {
					if !strings.Contains(work, want) {
						t.Fatalf("the tester's task = %q, want it to name %q", work, want)
					}
				}
				pnBaseMergeTimes(t, "the tester's task", work, 1)
			}
			row := spPhaseRow(t, pool, spParent, role)
			if row.Entry != 3 || row.EntryTask != work || !row.EntryMovedBack || row.EntryFrom != phase.AwaitingChildren ||
				row.HandoffCommit != "" || row.Summary != "" || row.Pass != 3 || row.Rounds != 1 {
				t.Fatalf("the %s's phases row = %+v, want entry 3 raised on this task and marked a move back out of awaiting_children, the handoff cleared, pass 3 and round 1 as they were", role, row)
			}
			entry := spEntry(spLine(t, pool), spParent)
			if entry == nil || entry.Kind != record.LineReturning || entry.Part != record.PartReturned {
				t.Fatalf("the parent's place in the line = %+v, want a returning entry among the issues whose work came back", entry)
			}
			if told := spNotices(t, pool, spParent); len(told) != 0 {
				t.Fatalf("awaiting-children notices = %+v, want none: the return tells no move into the wait", told)
			}
		})
	}
}

// A parent whose pull request merged during the wait goes back to the very start it held, with
// the task it held and no forward merge. A held testing goes back as testing, its tester started
// on the held task, which as the testing task of a parent ends with the base-merge sentence once,
// and its architect is told nothing of the return. A held accepting goes back as accepting, where
// no worker starts, so its architect is told once, under the wait's own notice kind, that the issue
// waits on its sign_off; the notice that moved the issue into accepting was replaced by the wait's.
func TestAParentWhosePullRequestMergedGoesBackToTheStartItHeld(t *testing.T) {
	t.Run("a held testing starts its tester", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		const heldTask = "Continue LEGION-300. Issue: LEGION-300. Phase: testing. Handoff commit: head. Pull request: #42."
		spWaiting(t, pool, spParent, phase.Testing, heldTask, "the implementer completed implementing", nil)
		seedPullRequest(t, pool, spParent, 42, record.PullRequestMerged)
		seedMirrorChild(t, pool, spChild, "done", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		if got.Phase != phase.Testing || got.ChildrenWait != nil || got.Status != "testing" {
			t.Fatalf("the parent = phase %s, held start %+v, status %s; want testing as it was held", got.Phase, got.ChildrenWait, got.Status)
		}
		starts := startsOf(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleTester || starts[0].task != heldTask+" "+pnBaseMerge {
			t.Fatalf("starts of %s = %+v, want one tester start on the held task ending with the base-merge sentence once: %q", spParent,
				starts, heldTask+" "+pnBaseMerge)
		}
		if told := spNotices(t, pool, spParent); len(told) != 0 {
			t.Fatalf("awaiting-children notices = %+v, want none: the tester's start is what the return gives", told)
		}
	})

	t.Run("a held accepting tells its architect", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		spWaiting(t, pool, spParent, phase.Accepting, "Continue LEGION-300. Issue: LEGION-300. Phase: accepting. Pull request: #42.",
			"a review approved the head", nil)
		seedPullRequest(t, pool, spParent, 42, record.PullRequestMerged)
		seedMirrorChild(t, pool, spChild, "done", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		if got.Phase != phase.Accepting || got.ChildrenWait != nil || got.Status != "needs_review" {
			t.Fatalf("the parent = phase %s, held start %+v, status %s; want accepting as it was held", got.Phase, got.ChildrenWait, got.Status)
		}
		if starts := startsOf(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("starts of %s = %+v, want none: no worker starts in accepting", spParent, starts)
		}
		want := record.Notice{Kind: workflow.AwaitingChildrenNoticeKind, Role: claim.RoleArchitect, Phase: phase.AwaitingChildren,
			Reason: "LEGION-300 left awaiting_children: no Dispatch child of it is open now, and its pull request merged during the wait, so it is back in accepting with no forward merge. It waits on your sign_off, as an issue in accepting does."}
		if told := spNotices(t, pool, spParent); len(told) != 1 || !reflect.DeepEqual(told[0], want) {
			t.Fatalf("awaiting-children notices = %+v, want the one %+v", told, want)
		}
	})
}

// A parent a blocked_by link still holds when its children are finished goes back to its start
// all the same, and that start waits as an issue waiting on another issue (#3715's
// record.LineWaitingOnIssue), placed by the moment its wait on the children ended: the start's own
// row is written at the return. Two parents whose waits end in two passes stand in the order the
// waits ended, whatever their keys.
func TestABlockedByLinkHoldsAReturnedParentByTheMomentItsWaitEnded(t *testing.T) {
	const (
		blocker    = "LEGION-900"
		early      = "LEGION-360"
		earlyChild = "LEGION-361"
		late       = "LEGION-350"
		lateChild  = "LEGION-351"
	)
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedMirror(t, pool, blocker, "todo")
	for parent, child := range map[string]string{early: earlyChild, late: lateChild} {
		spWaiting(t, pool, parent, phase.Implementing, "Continue "+parent+". Issue: "+parent+". Phase: implementing.", "a review asked for changes", nil)
		seedMirror(t, pool, parent, "in_progress", blocker)
		seedMirrorChild(t, pool, child, "todo", parent)
	}

	spPass(t, pool, admission)
	for _, key := range []string{early, late} {
		if got := issue(t, pool, key).Phase; got != phase.AwaitingChildren {
			t.Fatalf("%s while its child is open = %s, want awaiting_children", key, got)
		}
	}

	spMirror(t, pool, earlyChild, "done", early, 2)
	spPass(t, pool, admission)
	if entry := spEntry(spLine(t, pool), early); entry == nil || entry.Kind != record.LineWaitingOnIssue || !slices.Equal(entry.Waits, []string{blocker}) {
		t.Fatalf("%s after its return = %+v, want it waiting on %s in the line", early, entry, blocker)
	}

	spMirror(t, pool, lateChild, "done", late, 2)
	spPass(t, pool, admission)

	line := spLine(t, pool)
	var order []string
	for _, entry := range line {
		if entry.Kind != record.LineWaitingOnIssue {
			t.Fatalf("the line = %+v, want every returned parent waiting on %s", line, blocker)
		}
		order = append(order, entry.Issue.Key)
	}
	if want := []string{early, late}; !slices.Equal(order, want) {
		t.Fatalf("the line = %v, want %v: ordered by the moment each wait on the children ended", order, want)
	}
	assertSlots(t, pool, nil)
	for _, key := range []string{early, late} {
		if starts := startsOf(t, pool, key); len(starts) != 1 || starts[0].claimable {
			t.Fatalf("starts of %s = %+v, want one held start that no slot releases while %s is open", key, starts, blocker)
		}
	}
}

// A grandparent waits while its child waits, comes back while its child's children work and
// returns: the child stays open in the workflow until it closes (record.ChildOpen reads a released
// child by its Dispatch status), so the grandparent comes back only after the child has come back
// and closed.
func TestAGrandparentComesBackAfterItsChildHasComeBackAndClosed(t *testing.T) {
	const (
		grandparent = "LEGION-370"
		parent      = "LEGION-371"
		grandchild  = "LEGION-372"
	)
	pool := migratedPool(t)
	admission := quiet(t, 3)
	spSeedParent(t, pool, grandparent, phase.Implementing, true, nil)
	putIssue(t, pool, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: parent, Tree: grandparent, Parent: record.ParentOf(grandparent), Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: parent})
	for _, key := range []string{grandparent, parent} {
		spSeedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implementer", Entry: 1,
			EntryTask: "Continue " + key + ". Issue: " + key + ". Phase: implementing.", EntryRaisedAt: fixedNow.Add(-time.Hour)})
	}
	seedMirrorChild(t, pool, parent, "in_progress", grandparent)
	seedMirrorChild(t, pool, grandchild, "todo", parent)

	spPass(t, pool, admission)
	for _, key := range []string{grandparent, parent} {
		if got := issue(t, pool, key).Phase; got != phase.AwaitingChildren {
			t.Fatalf("%s while %s is open = %s, want awaiting_children", key, grandchild, got)
		}
	}

	spMirror(t, pool, grandchild, "done", parent, 2)
	spPass(t, pool, admission)
	if got := issue(t, pool, parent).Phase; got != phase.Implementing {
		t.Fatalf("%s once its child is done = %s, want implementing", parent, got)
	}
	if got := issue(t, pool, grandparent).Phase; got != phase.AwaitingChildren {
		t.Fatalf("%s while %s works after its return = %s, want awaiting_children still", grandparent, parent, got)
	}

	closed := issue(t, pool, parent)
	closed.Phase, closed.Status = phase.Done, "done"
	putIssue(t, pool, closed)
	spMirror(t, pool, parent, "done", grandparent, 2)
	spPass(t, pool, admission)
	if got := issue(t, pool, grandparent).Phase; got != phase.Implementing {
		t.Fatalf("%s once %s closed = %s, want implementing", grandparent, parent, got)
	}
}

// The return takes no slot it does not have: under a full cap the returned parent's start waits
// in the line, among the issues whose work came back and so ahead of a root whose first phase has
// never started, and the first slot that comes free is the parent's.
func TestAReturnedParentWaitsForASlotAheadOfWorkThatNeverStarted(t *testing.T) {
	const unstarted = "LEGION-395"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedStarted(t, pool, spFiller, phase.Implementing, nil)
	seedWaiting(t, pool, unstarted, "A")
	spWaiting(t, pool, spParent, phase.Implementing, "Continue LEGION-300. Issue: LEGION-300. Phase: implementing.", "a review asked for changes", nil)
	seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
	seedMirrorChild(t, pool, spChild, "done", spParent)

	spPass(t, pool, admission)

	assertSlots(t, pool, []record.Slot{{Issue: spFiller, Index: 0, AdmittedAt: fixedNow}})
	if got := waitingLine(t, pool, testProject, nil); !slices.Equal(got, []string{spParent, unstarted}) {
		t.Fatalf("the waiting line = %v, want the returned parent ahead of the root that never started", got)
	}
	if starts := startsOf(t, pool, spParent); len(starts) != 1 || !starts[0].needsSlot || starts[0].claimable {
		t.Fatalf("starts of %s = %+v, want one waiting for a slot", spParent, starts)
	}

	// The filler's work passes its review, so its slot comes free.
	moved := issue(t, pool, spFiller)
	moved.Phase, moved.Status = phase.Accepting, "needs_review"
	putIssue(t, pool, moved)
	spPass(t, pool, admission)

	if got := slots(t, pool); len(got) != 1 || got[0].Issue != spParent {
		t.Fatalf("slots = %+v, want the returned parent's alone", got)
	}
	if starts := startsOf(t, pool, spParent); len(starts) != 1 || !starts[0].claimable {
		t.Fatalf("starts of %s = %+v, want its start released by the slot", spParent, starts)
	}
	if got := issue(t, pool, unstarted); got.Phase != phase.Admitted {
		t.Fatalf("%s = %s, want it still waiting at admitted", unstarted, got.Phase)
	}
}
