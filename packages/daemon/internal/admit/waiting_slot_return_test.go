package admit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// steppingClock gives admission a clock the test moves, starting at fixedNow, and answers the
// function that moves it. The moment a slot is given back is what the waiting line orders the
// released issues by (record.Issue.ReleasedForWaitAt), so a test that reads that order must be
// able to put two releases at two moments; every other fixture of this file is written at
// fixedNow, as the rest of the package's are.
func steppingClock(admission *Admission) func(time.Time) {
	now := fixedNow
	admission.now = func() time.Time { return now }
	return func(at time.Time) { now = at }
}

// dispatchEvent applies one Dispatch event of key through the daemon's own handler order
// (applyMirrored): the mirror writes what Dispatch now says — the status, and in order the keys
// it waits on — and admission promotes behind it in the same transaction. It is how a blocker
// reaches done here, and how an issue is given a link it did not have.
func dispatchEvent(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID, key, status string, waits ...string) {
	t.Helper()
	applyMirrored(t, pool, admission, eventID, intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: status, Title: key, Rank: key, HandedOver: handed, Blockers: waits}, engineStub{})
}

// observedTick applies the periodic turn with what its own re-read of Dispatch saw
// (intake.ControllerTick.Observed), through the same handler order: the mirror writes every row
// the turn observed, and admission judges from those rows in that transaction. It is the other
// way a blocker's done reaches the daemon — the turn catches one whose closing event never
// arrived — and the pass behind it is the ordinary one.
func observedTick(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, observed ...intake.IssueStatus) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), testProject)
	if _, err := intake.ApplyFact(context.Background(), pool, "controller", eventID,
		intake.ControllerTick{Observed: observed}, mirror, engineStub{}, admission); err != nil {
		t.Fatalf("apply the controller tick %s: %v", eventID, err)
	}
}

// slotFreeNow is what the pass would answer a controller asking whether a slot stands free
// (Admission.slotFree), read in a transaction of its own after the pass committed.
func slotFreeNow(t *testing.T, pool *pgxpool.Pool, admission *Admission) bool {
	t.Helper()
	var free bool
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if free, err = admission.slotFree(context.Background(), tx); err != nil {
			t.Fatalf("read whether a slot stands free: %v", err)
		}
	})
	return free
}

// assertWaitsOnce reads that key stands in the project's waiting line exactly once: an issue
// released for a wait stands at admitted as often as not, where the line's first-phase branch
// would otherwise reach it a second time (record.Waiting).
func assertWaitsOnce(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	line := waitingLine(t, pool, testProject, nil)
	first := slices.Index(line, key)
	if first < 0 {
		t.Fatalf("%s stands in no part of the waiting line %v: an issue released for a wait waits there for a row of its own again", key, line)
	}
	if slices.Index(line[first+1:], key) >= 0 {
		t.Fatalf("%s stands in the waiting line %v twice, want once", key, line)
	}
}

// The slot an issue gave back for a wait comes back to it with nobody acting, the moment the
// last issue it waits on is done, and in the line's own order. Nothing outside the daemon moves:
// no controller takes the issue again, no person admits it, no operator command is run. The pass
// that reads the wait ended is whichever pass comes next — a Dispatch event of the blocker, the
// close of the last open child, the periodic turn's own re-read — and each of the three is read
// here, because each is a way the daemon learns a wait is over and none of them is the others.
//
// The order is the whole of the second half. An issue whose wait ended stands among the issues
// whose work came back, at the moment its row was taken away (record.Issue.ReleasedForWaitAt),
// so it takes a free slot before every issue whose first phase has yet to start, and two issues
// whose waits end in one pass take theirs in the order they gave them back. That is a limit on
// work in progress finishing started work first: the issue was at work when the wait began, and
// the wait stopped nothing of it.
//
// While the wait stands the issue is no taker for the slot it freed: the controller is told of a
// free slot (Admission.slotFree, the `slot-free` notice), because a slot held open for an issue
// that cannot run would hide the project's capacity from the one reader that may hand Legion
// work. And an issue whose phase ended while it held no row owes its next worker a start with
// no mark beside it: the owed start is then the whole of what says the daemon started it, so it
// stands in the line once as a waiting issue, holds no row while its blocker is open, and the
// start waits on the row as every phase worker's start does
// (record.SuperviseRequest.NeedsSlot) until the row's return delivers it.
func TestTheSlotComesBackWhenTheLastWaitEnds(t *testing.T) {
	t.Run("the wait begins, the slot goes to the candidate behind it, and the last done brings it back", func(t *testing.T) {
		const (
			waiter    = "LEGION-10"
			atWork    = "LEGION-11"
			finishes  = "LEGION-12"
			behind    = "LEGION-20"
			unstarted = "LEGION-21"
			first     = "LEGION-90"
			second    = "LEGION-91"
		)
		pool := migratedPool(t)
		admission := quiet(t, 3)

		// The three issues the daemon has started, filling the cap: the one two unfinished
		// blockers will hold, one at work that waits on nothing, and one a person will finish.
		seedStarted(t, pool, waiter, phase.Implementing, nil)
		seedMirror(t, pool, waiter, "in_progress", first, second)
		seedStarted(t, pool, atWork, phase.Implementing, nil)
		seedMirror(t, pool, atWork, "in_progress")
		seedStarted(t, pool, finishes, phase.Implementing, nil)
		seedMirror(t, pool, finishes, "in_progress")
		seedMirror(t, pool, first, "in_progress")
		seedMirror(t, pool, second, "in_progress")
		// Two roots the daemon has not started, whose first phase has yet to start: the one that
		// takes the slot the wait frees, and the one the returning issue must go before, held
		// meanwhile by the second blocker so that both are offered the one free slot in the pass
		// that finds it done.
		seedWaiting(t, pool, behind, "B")
		seedMirror(t, pool, behind, "todo")
		seedWaiting(t, pool, unstarted, "C")
		seedMirror(t, pool, unstarted, "todo", second)

		tick(t, pool, admission, "the-wait-begins")

		// The waiting issue gave its row back and the promotion passed over it, so the free slot
		// went to the candidate behind it.
		assertRowHolders(t, pool, atWork, behind, finishes)
		if released := issue(t, pool, waiter); released.ReleasedForWaitAt == nil || !released.ReleasedForWaitAt.Equal(fixedNow) {
			t.Fatalf("%s was released at %v, want the moment of the pass, %v", waiter, released.ReleasedForWaitAt, fixedNow)
		}
		assertWaitsOnce(t, pool, waiter)

		// A slot comes free with the waiting issue still waiting: nothing takes it, which is what
		// makes the two readings below about the waits alone and not about the cap.
		dispatchEvent(t, pool, admission, "finished", finishes, "done")
		// A waiting issue is no taker for the slot, and the root the second blocker holds is
		// none either.
		assertRowHolders(t, pool, atWork, behind)

		dispatchEvent(t, pool, admission, "first-done", first, "done")

		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row with one of its two blockers still unfinished, want none: the row comes back when the last issue it waits on is done, not the first", waiter)
		}
		assertWaitsOnce(t, pool, waiter)

		dispatchEvent(t, pool, admission, "second-done", second, "done")

		// The issue whose wait ended takes the free slot, before the root whose first phase has
		// yet to start.
		assertRowHolders(t, pool, atWork, behind, waiter)
		back := issue(t, pool, waiter)
		if back.ReleasedForWaitAt != nil {
			t.Errorf("%s holds a row and is marked released for a wait at %v, want no mark: the mark goes with the row's return", waiter, back.ReleasedForWaitAt)
		}
		if back.Phase != phase.Implementing {
			t.Errorf("%s stands in %s, want implementing: the wait stopped nothing of its work and its end starts nothing", waiter, back.Phase)
		}
		if holdsSlot(t, pool, unstarted) {
			t.Fatalf("%s holds a row, want none: the one free slot went to the issue whose work came back, and this root's first phase has still to start", unstarted)
		}
		if line := waitingLine(t, pool, testProject, nil); !slices.Contains(line, unstarted) {
			t.Fatalf("the waiting line is %v, want %s still in it: it waits for the next slot", line, unstarted)
		}
	})

	t.Run("two waits ending in one pass are served in the order the rows were given back", func(t *testing.T) {
		const (
			older   = "LEGION-30"
			newer   = "LEGION-31"
			blocker = "LEGION-92"
		)
		pool := migratedPool(t)
		// One slot for the two of them, so the pass that ends both waits has exactly one to give
		// and the order is what decides which issue takes it.
		admission := quiet(t, 1)
		step := steppingClock(admission)
		seedStarted(t, pool, older, phase.Implementing, nil)
		seedMirror(t, pool, older, "in_progress", blocker)
		seedStarted(t, pool, newer, phase.Implementing, nil)
		seedMirror(t, pool, newer, "in_progress")
		seedMirror(t, pool, blocker, "in_progress")

		tick(t, pool, admission, "the-first-wait-begins")
		if holdsSlot(t, pool, older) {
			t.Fatalf("%s holds a row while it waits, want none", older)
		}

		// The second issue is given the same link an hour later, so the two rows were given back
		// at two moments and the line has an order to read.
		later := fixedNow.Add(time.Hour)
		step(later)
		dispatchEvent(t, pool, admission, "the-second-wait-begins", newer, "in_progress", blocker)
		for _, key := range []string{older, newer} {
			if holdsSlot(t, pool, key) {
				t.Fatalf("%s holds a row while it waits, want none", key)
			}
		}
		if got := issue(t, pool, newer); got.ReleasedForWaitAt == nil || !got.ReleasedForWaitAt.Equal(later) {
			t.Fatalf("%s was released at %v, want %v, the moment of the pass that found it waiting", newer, got.ReleasedForWaitAt, later)
		}

		step(later.Add(time.Hour))
		dispatchEvent(t, pool, admission, "both-waits-end", blocker, "done")

		// The one free slot goes to the issue that gave its row back first.
		assertRowHolders(t, pool, older)
		if line := waitingLine(t, pool, testProject, nil); !slices.Equal(line, []string{newer}) {
			t.Fatalf("the waiting line = %v, want %v: the issue behind it keeps its place for the next slot", line, []string{newer})
		}
	})

	t.Run("the controller is told of the slot a waiting issue freed", func(t *testing.T) {
		const (
			waiter  = "LEGION-40"
			atWork  = "LEGION-41"
			blocker = "LEGION-93"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		registerController(t, pool)
		seedStarted(t, pool, waiter, phase.Implementing, nil)
		seedMirror(t, pool, waiter, "in_progress")
		seedStarted(t, pool, atWork, phase.Implementing, nil)
		seedMirror(t, pool, atWork, "in_progress")
		seedMirror(t, pool, blocker, "in_progress")

		dispatchEvent(t, pool, admission, "the-wait-begins", waiter, "in_progress", blocker)

		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while it waits, want none", waiter)
		}
		if !slotFreeNow(t, pool, admission) {
			t.Fatalf("a slot stands free = false, want true: the waiting issue is no taker for the row it gave back, so the capacity it freed is the project's")
		}
		assertControllerNotices(t, pool, []effect{{kind: record.OutboxKindControllerNotice, issue: waiter,
			payload: record.ControllerNotice{Kind: record.SlotFreeNotice}}})
	})

	t.Run("an issue that waits and owes its next worker a start", func(t *testing.T) {
		const (
			waiter  = "LEGION-50"
			blocker = "LEGION-94"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		// An issue the daemon has started whose phase ended while it held no row: the transition
		// wrote the start its next worker is owed and nothing else, so the record holds no slot
		// row and carries no mark of a release for a wait. That owed start is the whole of what
		// says the daemon started this issue, and it must say so as loudly as a row or a mark:
		// judged unstarted, the issue would stand in the line as a return and the pass would
		// grant it a slot and deliver its start with its blocker still open.
		putIssue(t, pool, record.Issue{Key: waiter, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: testProject, Title: waiter, Tree: waiter,
			Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true})
		seedLifecycle(t, pool, waiter)
		oweStart(t, pool, waiter, waiter, phase.Testing)
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")

		tick(t, pool, admission, "the-wait-stands")

		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while its blocker is open, want none: an issue that owes a start and waits is an issue the daemon has started", waiter)
		}
		owed := startsOf(t, pool, waiter)
		if len(owed) != 1 || !owed[0].needsSlot || owed[0].claimable || owed[0].role != claim.RoleTester {
			t.Fatalf("the starts of %s while it waits = %+v, want the tester's one row, held for a slot and claimable by no lane", waiter, owed)
		}
		if got := issue(t, pool, waiter); got.ReleasedForWaitAt != nil {
			t.Fatalf("%s carries a mark of a release at %v, want none: it held no row for the pass to take away", waiter, got.ReleasedForWaitAt)
		}
		assertWaitsOnce(t, pool, waiter)

		dispatchEvent(t, pool, admission, "the-wait-ends", blocker, "done")

		if !holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds no row once its wait ended, want the one its promotion grants it", waiter)
		}
		delivered := startsOf(t, pool, waiter)
		if len(delivered) != 1 || !delivered[0].claimable {
			t.Fatalf("the starts of %s once its wait ended = %+v, want the one row claimable: the row's return is what delivers the task", waiter, delivered)
		}
	})

	t.Run("a children-first wait ends when the last open child closes", func(t *testing.T) {
		const (
			parent = "LEGION-60"
			child  = "LEGION-95"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		// A tree's own root whose design gate has opened: its own first phase is what waits, and
		// an open Dispatch child under it is what holds that phase (record.LineFacts.ChildWaits).
		seedStarted(t, pool, parent, phase.Admitted, nil)
		seedGate(t, pool, record.DesignGate{Issue: parent, ArtifactID: "artifact-60", LatestVersion: 1,
			ApprovedVersion: new(1), EverOpened: true})
		seedMirror(t, pool, parent, "in_progress")
		seedMirrorChild(t, pool, child, "backlog", parent)

		tick(t, pool, admission, "the-wait-begins")

		if holdsSlot(t, pool, parent) {
			t.Fatalf("%s holds a row while its open child goes first, want none", parent)
		}
		assertWaitsOnce(t, pool, parent)

		dispatchEvent(t, pool, admission, "the-child-closes", child, "done")

		if !holdsSlot(t, pool, parent) {
			t.Fatalf("%s holds no row once its last open child closed, want the one it gave back", parent)
		}
		if got := issue(t, pool, parent).Phase; got != phase.Planning {
			t.Fatalf("%s stands in %s once its wait ended, want planning: the row comes back and the line offers its first phase in the same pass", parent, got)
		}
	})

	t.Run("a blocker whose done reaches the daemon through the periodic turn's re-read", func(t *testing.T) {
		const (
			waiter  = "LEGION-70"
			blocker = "LEGION-96"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		seedStarted(t, pool, waiter, phase.Implementing, nil)
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")

		observedTick(t, pool, admission, "the-wait-begins")
		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while it waits, want none", waiter)
		}

		// No event of the blocker ever arrives: the turn's own re-read of Dispatch is what says
		// it is done, written by the mirror ahead of the pass in the same transaction.
		observedTick(t, pool, admission, "the-turn-reads-it-done",
			intake.IssueStatus{Key: blocker, Project: testProject, Status: "done", Seq: 2})

		if !holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds no row once the turn read its blocker done, want the one it gave back", waiter)
		}
		if got := issue(t, pool, waiter); got.ReleasedForWaitAt != nil || got.Phase != phase.Implementing {
			t.Fatalf("%s = %#v, want it standing in implementing with no mark of a release", waiter, got)
		}
	})
}
