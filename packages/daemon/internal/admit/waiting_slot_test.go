package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedStarted records key as an issue the daemon has started: a root of a live tree of its own,
// standing in the phase given, holding an admission slot. It is the package's slotted-issue
// fixture (seedSlottedInPhase) for a phase other than admitted — every issue this test releases
// or keeps has already been started, which is what the slot row says — ranked by its own key,
// and shape is where a test says what else the record carries.
func seedStarted(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, shape func(*record.Issue)) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, key, key, at, shape)
}

// An issue the daemon has started and that waits on another issue holds no admission slot: the
// cap counts the issues Legion is working on, and an issue waiting for another to finish is not
// one of them. One admission pass takes the row of every such issue away, whatever phase it
// stands in and whatever rule other deciders exempt it by, and leaves the row of every issue
// that waits on nothing.
//
// Two waits count. A Dispatch blocker of the issue that is not done — in_progress, parked, or an
// issue the daemon holds no mirror row of at all, which is the safe reading
// (blockers.Unfinished) — and an open Dispatch child the issue's own first phase waits for
// (record.LineFacts.ChildWaits). The phase the issue stands in does not enter it: an issue in
// testing, held from its implementing, merging and accepting each gives its row back, as one in
// implementing does.
//
// Three started issues keep theirs though a wait names them. A root in awaiting_person waits on a
// person, and the row it holds is what its resume takes back. A tree's own root whose design
// gate has never opened is its architect writing the design, the whole of that tree's work
// (record.AtWork at phase.Admitted): releasing it would take the slot of the one issue of the
// tree that is working, and it is still the root that may hand its work to a person, which this
// test reads of it last (record.CanWaitOnPerson). And a root that arrived from another tree
// carrying the row it held there (record.QueuedRoot) keeps it, because the line judges such a
// root by the queued-root rule, which starts a root carrying a run whatever its links say: the
// subtest below reads that one.
//
// blockers.Exempt is read by no part of this. That rule is the promotion's, about what a
// candidate may start in spite of its links, so an issue carrying a run — which every issue in a
// worker phase does — gives its row back like any other.
func TestAWaitingIssueHoldsNoSlot(t *testing.T) {
	const (
		blocker     = "LEGION-90"
		finished    = "LEGION-91"
		unrecorded  = "LEGION-92"
		openChild   = "LEGION-93"
		gatedChild  = "LEGION-94"
		implBlocked = "LEGION-10"
		childWait   = "LEGION-11"
		testing_    = "LEGION-12"
		heldBlocked = "LEGION-13"
		merging     = "LEGION-14"
		accepting   = "LEGION-15"
		noRow       = "LEGION-16"
		carriesRun  = "LEGION-17"
		allDone     = "LEGION-20"
		noLink      = "LEGION-21"
		onPerson    = "LEGION-22"
		gateClosed  = "LEGION-23"
	)
	pool := migratedPool(t)
	admission := quiet(t, 12)

	// What Dispatch says of the issues the waits name: one blocker still running, one finished,
	// and one the daemon holds no row of, which is named by nothing here.
	seedMirror(t, pool, blocker, "in_progress")
	seedMirror(t, pool, finished, "done")

	// The issues that must give their rows back.
	for _, waiting := range []struct {
		key   string
		at    phase.Phase
		shape func(*record.Issue)
	}{
		{key: implBlocked, at: phase.Implementing},
		{key: testing_, at: phase.Testing},
		{key: heldBlocked, at: phase.Held, shape: func(issue *record.Issue) {
			issue.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
		}},
		{key: merging, at: phase.Merging},
		{key: accepting, at: phase.Accepting},
		// A tree Legion took back, in a worker phase: blockers.Exempt names it twice over, by
		// the run it carries and by the mark of the tree coming back, and this rule reads
		// neither.
		{key: carriesRun, at: phase.Planning, shape: func(issue *record.Issue) { issue.TakenBack = true }},
	} {
		seedStarted(t, pool, waiting.key, waiting.at, waiting.shape)
		seedMirror(t, pool, waiting.key, "in_progress", blocker)
	}
	// A link naming a key the daemon has no mirror row of waits too: the row arrives with the
	// next event, the periodic turn or the next boot, and holding is the safe reading.
	seedStarted(t, pool, noRow, phase.Implementing, nil)
	seedMirror(t, pool, noRow, "in_progress", unrecorded)
	// A root at admitted under an open design gate whose own first phase waits for an open
	// Dispatch child under it, which no link of it names.
	seedStarted(t, pool, childWait, phase.Admitted, nil)
	seedGate(t, pool, record.DesignGate{Issue: childWait, ArtifactID: "artifact-11", LatestVersion: 1,
		ApprovedVersion: new(1), EverOpened: true})
	seedMirror(t, pool, childWait, "in_progress")
	seedMirrorChild(t, pool, openChild, "backlog", childWait)

	// The issues that must keep theirs.
	seedStarted(t, pool, allDone, phase.Implementing, nil)
	seedMirror(t, pool, allDone, "in_progress", finished)
	seedStarted(t, pool, noLink, phase.Implementing, nil)
	seedMirror(t, pool, noLink, "in_progress")
	seedStarted(t, pool, onPerson, phase.AwaitingPerson, func(issue *record.Issue) {
		issue.Awaiting = &record.Awaiting{Ask: "ask-22", Since: record.EventMark{Seq: 2, At: fixedNow}}
	})
	seedMirror(t, pool, onPerson, "in_progress", blocker)
	// A root whose architect is writing the design: its gate has never opened, so a link of it
	// and a child under it hold nothing — the work of the whole tree is its own.
	seedStarted(t, pool, gateClosed, phase.Admitted, nil)
	seedMirror(t, pool, gateClosed, "in_progress", blocker)
	seedMirrorChild(t, pool, gatedChild, "backlog", gateClosed)

	tick(t, pool, admission, "tick")

	assertRowHolders(t, pool, allDone, noLink, onPerson, gateClosed)

	// Each released issue stands in the line for a row of its own again, marked with the moment
	// its row was taken away, and nothing of its work moved: its phase is where it was.
	for _, key := range []string{implBlocked, childWait, testing_, heldBlocked, merging, accepting, noRow, carriesRun} {
		released := issue(t, pool, key)
		if released.ReleasedForWaitAt == nil || !released.ReleasedForWaitAt.Equal(fixedNow) {
			t.Errorf("%s was released at %v, want the moment of the pass, %v: the mark is what tells it from an issue that never started", key, released.ReleasedForWaitAt, fixedNow)
		}
	}
	if got := issue(t, pool, implBlocked).Phase; got != phase.Implementing {
		t.Errorf("the released issue stands in %s, want implementing: taking the slot away stops nothing of its work", got)
	}
	if got := issue(t, pool, heldBlocked); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Implementing {
		t.Errorf("the released held issue = %#v, want it held from implementing exactly as it stood", got)
	}
	for _, key := range []string{implBlocked, childWait, testing_, heldBlocked, merging, accepting, noRow, carriesRun} {
		assertWaitsOnce(t, pool, key)
	}
	line := waitingLine(t, pool, testProject, nil)
	for _, key := range []string{allDone, noLink, onPerson, gateClosed} {
		if slices.Contains(line, key) {
			t.Errorf("%s stands in the waiting line %v, want it out of the line: it holds its slot and waits for none", key, line)
		}
	}

	// The root whose gate has never opened kept its slot and so may still hand its work to a
	// person: the two answers are one fact about it, that the daemon has started it and its
	// architect has work (record.CanWaitOnPerson).
	root := issue(t, pool, gateClosed)
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list the project's issues: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		if !record.CanWaitOnPerson(root, facts.Gates[root.Tree], true) {
			t.Fatalf("the root whose gate has never opened may wait on no person, want it able to: it kept the slot that says the daemon started it")
		}
	})

	// A root that arrived from another tree carrying the row it held there keeps it, though a
	// blocker of it is not done. Such a root stands in the line as a queued root
	// (record.QueuedRoot), whose own rule exempts the run it carries (blockers.Exempt), so the
	// release and the promotion must read one judgement of it: released, its row goes to the
	// candidate ahead of it in the line and no pass ever gives it back, though the line promises
	// it keeps the row it arrived with.
	t.Run("a root that arrived from another tree keeps the row it carries", func(t *testing.T) {
		const (
			arrived   = "LEGION-30"
			left      = "LEGION-31"
			returning = "LEGION-32"
			blocker   = "LEGION-98"
		)
		pool := migratedPool(t)
		// One row for the project, the one this root carries: with a free row to spare the
		// promotion would hand it straight back and hide the release behind its own grant.
		admission := quiet(t, 1)
		seedStarted(t, pool, arrived, phase.Implementing, func(issue *record.Issue) {
			issue.HandedOverFrom = left
			issue.Status = "todo"
		})
		seedMirror(t, pool, arrived, "todo", blocker)
		seedMirror(t, pool, blocker, "in_progress")
		// An issue whose work came back and owes its implementer a start: it stands in the
		// line's first part, ahead of every queued root, so a row freed here is taken before
		// the root that gave it up could have it back.
		putIssue(t, pool, record.Issue{Key: returning, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: testProject, Title: returning, Tree: returning,
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true})
		seedLifecycle(t, pool, returning)
		seedMirror(t, pool, returning, "in_progress")
		oweStart(t, pool, returning, returning, phase.Implementing)

		tick(t, pool, admission, "tick")

		assertRowHolders(t, pool, arrived)
		if got := issue(t, pool, arrived); got.ReleasedForWaitAt != nil {
			t.Fatalf("%s is marked released for a wait at %v, want no mark: it holds the row it arrived with", arrived, got.ReleasedForWaitAt)
		}
	})
}
