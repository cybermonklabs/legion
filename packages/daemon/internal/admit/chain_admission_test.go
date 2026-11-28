package admit

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tests of k29 (R19 of dispatch://INFRA-3320): admission reads Dispatch's chain from the copy
// and nothing else. A root starts by a person's handover; an issue under a parent at work starts
// on todo whatever its label and waits for every gate above it; the three holds of an issue
// under a parent are read from its chain; and an issue with nothing above it in Legion's work is
// a root. The line's own order (TestTheLineRanksFirstStartsByTheTopIssue) is beside the line's
// other tests in line_order_test.go.

// chainRecord is the record of key in the test project, standing at stands at the Dispatch
// status given, under the Dispatch parent named ("" for none): putIssue writes the copy's row of
// it with that parent, which is where every decision reads its chain from.
func chainRecord(key, parent string, stands phase.Phase, status string) record.Issue {
	issue := record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: key}
	if parent != "" {
		issue.Parent = new(parent)
	}
	return issue
}

// seedRunning records key as an issue the daemon runs in implementing under parent ("" for a
// root): its record, its slot and its tree lifecycle, with its own design gate approved at its
// latest version where it is a root.
func seedRunning(t *testing.T, pool *pgxpool.Pool, key, parent string) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, key, key, phase.Implementing, func(issue *record.Issue) {
		if parent != "" {
			issue.Parent = new(parent)
		}
	})
	if parent == "" {
		seedApprovedGate(t, pool, key, 1, 1)
	}
}

// seedApprovedGate registers key's spec step at version latest, approved at version approved.
func seedApprovedGate(t *testing.T, pool *pgxpool.Pool, key string, latest, approved int) {
	t.Helper()
	seedGate(t, pool, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "spec", LatestVersion: latest,
		ApprovedVersion: &approved, EverOpened: true, ApprovedMark: record.EventMark{Seq: 3, At: fixedNow.Add(-time.Hour)}})
}

// passJudgement is the waiting line and the held list one admission pass judges, read as the
// pass reads them (record.ReadLineFacts, blockers.JudgeLine, record.HeldList): the keys of the
// line in order, and each held row by issue key.
func passJudgement(t *testing.T, pool *pgxpool.Pool, admission *Admission) ([]string, map[string]record.HeldIssue) {
	t.Helper()
	var keys []string
	held := map[string]record.HeldIssue{}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		slotted, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list the slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, admission.engine.GatedSteps())
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		_, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, records, testProject, issues, slotted, facts, nil)
		if err != nil {
			t.Fatalf("judge the line: %v", err)
		}
		for _, entry := range line {
			keys = append(keys, entry.Issue.Key)
		}
		for _, row := range record.HeldList(issues, slotted, line, dropped, facts) {
			held[row.Issue] = row
		}
	})
	return keys, held
}

// assertHeldBy fails unless key is held with reason by exactly the issues named.
func assertHeldBy(t *testing.T, held map[string]record.HeldIssue, key string, reason record.HeldReason, by ...string) {
	t.Helper()
	row, listed := held[key]
	if !listed || row.Reason != reason || !slices.Equal(row.HeldBy, by) {
		t.Errorf("the held row of %s = %+v (listed %v), want %s held by %v", key, row, listed, reason, by)
	}
}

// assertNotStarted fails if key holds a slot or stands past admitted.
func assertNotStarted(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if holdsSlot(t, pool, key) {
		t.Errorf("%s holds a slot, want it started nowhere", key)
	}
	if stored := maybeIssue(t, pool, key); stored != nil && stored.Phase != phase.Admitted {
		t.Errorf("%s stands at %s, want it not started", key, stored.Phase)
	}
}

// A labelled todo root takes a free slot, which is its start; one that waits for a slot leaves
// the line when its label is taken off; and taking the label off a root at work stops nothing.
func TestARootStartsOnAPersonsHandover(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const working, waiting = "LEGION-30", "LEGION-31"
	todo := func(key string, seq int64, labelled bool, status string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: status,
			Title: key, Rank: key, HandedOver: labelled}
	}

	apply(t, pool, admission, "hand-over-working", todo(working, 1, true, "todo"), engineStub{})
	if !holdsSlot(t, pool, working) {
		t.Fatalf("%s, a labelled todo root at a free slot, holds none: a person's handover starts a root", working)
	}
	apply(t, pool, admission, "hand-over-waiting", todo(waiting, 1, true, "todo"), engineStub{})
	assertWaiting(t, pool, []string{waiting})

	apply(t, pool, admission, "label-off-waiting", todo(waiting, 2, false, "todo"), engineStub{})
	assertWaiting(t, pool, nil)

	apply(t, pool, admission, "label-off-working", todo(working, 2, false, "in_progress"), engineStub{})
	if stored := issue(t, pool, working); !holdsSlot(t, pool, working) || stored.Phase != phase.Admitted || stored.Status != "in_progress" {
		t.Fatalf("%s after its label came off = %s/%s (slot %v), want it going on: the label starts a root and stops nothing",
			working, stored.Phase, stored.Status, holdsSlot(t, pool, working))
	}
}

// An issue under a parent at work starts when Dispatch shows it todo, whoever wrote the todo and
// whether or not it carries the label: the label is a person's handover of a root, and an issue
// under a parent at work is no root.
func TestAnIssueUnderAParentAtWorkStartsOnTodo(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	const parent, unlabelled, labelled = "LEGION-40", "LEGION-41", "LEGION-42"
	seedRunning(t, pool, parent, "")

	releaseChild(t, pool, admission, "release-unlabelled", unlabelled, parent, 1)
	applyMirrored(t, pool, admission, "labelled-todo", intake.DispatchIssue{Key: labelled, ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.updated", Status: "todo", Title: labelled, Parent: parent, Rank: labelled, HandedOver: true}, admission.engine)

	for _, key := range []string{unlabelled, labelled} {
		stored := maybeIssue(t, pool, key)
		if stored == nil || stored.Phase == phase.Admitted || !holdsSlot(t, pool, key) {
			t.Errorf("%s under %s at work = %+v (slot %v), want it started in its first phase", key, parent, stored, holdsSlot(t, pool, key))
		}
	}
}

// An issue under a parent at work waits for the gate of each issue above it in Legion's work
// that holds its own gate: under B, whose gate is open, and A, which holds its own gate with its
// latest version not approved, a child C of A waits, held by A's gate.
func TestAnIssueWaitsForEveryGateAboveIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const b, a, c = "LEGION-50", "LEGION-51", "LEGION-52"
	seedRunning(t, pool, b, "")
	seedSlottedInPhase(t, pool, testProject, a, a, phase.Implementing, func(issue *record.Issue) {
		issue.Parent, issue.HoldsGate = new(b), true
	})
	seedApprovedGate(t, pool, a, 2, 1)
	putIssue(t, pool, chainRecord(c, a, phase.Admitted, "todo"))

	tick(t, pool, admission, "gates-above")
	assertNotStarted(t, pool, c)
	_, held := passJudgement(t, pool, admission)
	assertHeldBy(t, held, c, record.HeldDesignGate, a)
}

// A label added to an issue under a parent at work that Dispatch shows on the backlog starts
// nothing: it is no root, and only todo starts an issue under a parent at work.
func TestALabelUnderAParentAtWorkStartsNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	const parent, child = "LEGION-55", "LEGION-56"
	seedRunning(t, pool, parent, "")

	applyMirrored(t, pool, admission, "label-on-backlog", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.updated", Status: "backlog", Title: child, Parent: parent, Rank: child, HandedOver: true}, admission.engine)

	assertNotStarted(t, pool, child)
	if line, _ := passJudgement(t, pool, admission); slices.Contains(line, child) {
		t.Errorf("the waiting line = %v, want no %s: a labelled issue on the backlog under a parent at work waits for nothing", line, child)
	}
}

// The three holds of an issue under a parent, each with the issue that holds it: ancestry while
// its parent is not in Legion's work and an issue above the parent is, root_awaiting_person while
// its top issue waits on a person, and accepted_ancestor while an issue above it stands in retro
// or merging.
func TestTheThreeHoldsOfAnIssueUnderAParent(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const (
		atWork, onBacklog, underBacklog = "LEGION-60", "LEGION-61", "LEGION-62"
		awaiting, underAwaiting         = "LEGION-70", "LEGION-71"
		merging, underMerging           = "LEGION-80", "LEGION-81"
	)
	seedRunning(t, pool, atWork, "")
	putIssue(t, pool, chainRecord(onBacklog, atWork, phase.Admitted, "backlog"))
	putIssue(t, pool, chainRecord(underBacklog, onBacklog, phase.Admitted, "todo"))

	waitingRoot := quietRootRecord(awaiting, awaiting, phase.AwaitingPerson)
	putIssue(t, pool, waitingRoot)
	takeSlot(t, pool, awaiting)
	seedLifecycle(t, pool, awaiting)
	putIssue(t, pool, chainRecord(underAwaiting, awaiting, phase.Admitted, "todo"))

	putIssue(t, pool, chainRecord(merging, "", phase.Merging, "retro"))
	seedApprovedGate(t, pool, merging, 1, 1)
	putIssue(t, pool, chainRecord(underMerging, merging, phase.Admitted, "todo"))

	tick(t, pool, admission, "three-holds")
	_, held := passJudgement(t, pool, admission)
	assertHeldBy(t, held, underBacklog, record.HeldAncestry, atWork)
	assertHeldBy(t, held, underAwaiting, record.HeldRootAwaitingPerson, awaiting)
	assertHeldBy(t, held, underMerging, record.HeldAcceptedAncestor, merging)
	for _, key := range []string{underBacklog, underAwaiting, underMerging} {
		assertNotStarted(t, pool, key)
	}
}

// A run that starts as a root's does (record.Chains.StartsAsRoot) but stands under a parent — a
// gate holder on a run past its first — meets the holds of an issue under a parent before it
// starts, as every issue under a parent does (R19 of dispatch://INFRA-3320), and starts nothing
// on a chain the copy cannot show to its top (R16). A person's todo on one under a parent on the
// backlog below a root at work (ancestry), under a root that waits on a person
// (root_awaiting_person), under a root in merging (accepted_ancestor) or under a parent the copy
// holds no row of (chain_unknown) starts nothing of it, and the held list names it. The word is the
// ladder's first true one (record.HeldReasonOf): the hold itself where the ladder reads it before
// the issue's own gate, and under the root in merging its own design gate, which no architect has
// designed for the new run and which the ladder reads before accepted_ancestor for every issue at
// admitted. Once the hold ends — the parent back in work, the root resumed, the root closed, the
// parent's row read — the todo it stands at queues it, and its promotion starts its architect.
func TestARunThatStartsAsARootMeetsTheHoldsOfAnIssueUnderAParent(t *testing.T) {
	gateHolder := func(key, parent string) record.Issue {
		issue := chainRecord(key, parent, phase.Admitted, "in_progress")
		issue.Generation, issue.HoldsGate, issue.HandedOver = 2, true, true
		return issue
	}
	for _, tc := range []struct {
		name, key, parent, holder string
		word                      record.HeldReason
		seed, end                 func(t *testing.T, pool *pgxpool.Pool)
	}{
		{
			name: "under a parent on the backlog below a root at work", key: "LEGION-112", parent: "LEGION-111", holder: "LEGION-110",
			word: record.HeldAncestry,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedRunning(t, pool, "LEGION-110", "")
				putIssue(t, pool, chainRecord("LEGION-111", "LEGION-110", phase.Admitted, "backlog"))
			},
			end: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, chainRecord("LEGION-111", "LEGION-110", phase.Admitted, "todo"))
			},
		},
		{
			name: "under a root that waits on a person", key: "LEGION-121", parent: "LEGION-120", holder: "LEGION-120",
			word: record.HeldRootAwaitingPerson,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, quietRootRecord("LEGION-120", "LEGION-120", phase.AwaitingPerson))
				takeSlot(t, pool, "LEGION-120")
				seedLifecycle(t, pool, "LEGION-120")
			},
			end: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, quietRootRecord("LEGION-120", "LEGION-120", phase.Implementing))
			},
		},
		{
			name: "under a root in merging", key: "LEGION-131", parent: "LEGION-130", holder: "LEGION-131",
			word: record.HeldDesignGate,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, chainRecord("LEGION-130", "", phase.Merging, "retro"))
				seedApprovedGate(t, pool, "LEGION-130", 1, 1)
			},
			end: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, chainRecord("LEGION-130", "", phase.Done, record.DoneStatus))
			},
		},
		{
			name: "under a parent the copy holds no row of", key: "LEGION-141", parent: "LEGION-140", holder: "LEGION-140",
			word: record.HeldChainUnknown,
			seed: func(*testing.T, *pgxpool.Pool) {},
			end: func(t *testing.T, pool *pgxpool.Pool) {
				inTx(t, pool, func(tx pgx.Tx) {
					putCopyTx(t, tx, record.Issue{Key: "LEGION-140", Project: testProject, Status: "in_progress"})
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			tc.seed(t, pool)
			putIssue(t, pool, gateHolder(tc.key, tc.parent))

			apply(t, pool, admission, "persons-todo", intake.DispatchIssue{Key: tc.key, ResolvedRepo: "acme/widgets", Seq: 10,
				Type: "issue.updated", Status: "todo", Title: tc.key, Parent: tc.parent, Rank: tc.key, HandedOver: true}, admission.engine)

			if starts := architectStarts(t, pool, tc.key); starts != 0 {
				t.Fatalf("a person's todo on %s started %d architects of it, want none while it is held %s", tc.key, starts, tc.word)
			}
			assertNotStarted(t, pool, tc.key)
			_, held := passJudgement(t, pool, admission)
			assertHeldBy(t, held, tc.key, tc.word, tc.holder)

			tc.end(t, pool)
			tick(t, pool, admission, "the-hold-ends")

			if starts := architectStarts(t, pool, tc.key); starts != 1 || !holdsSlot(t, pool, tc.key) {
				t.Fatalf("once its hold ended %s started %d architects (slot %v), want its promotion's one at the todo it stands at",
					tc.key, starts, holdsSlot(t, pool, tc.key))
			}
		})
	}
}

// An issue whose parent is done with no issue above it in Legion's work is a root: one at work
// goes on, and one that has not started waits for a person's handover. The shapes of INFRA-1238
// (a done parent under an issue in Legion's work) and INFRA-500 (a done parent under an issue in
// merging) are held with ancestry and accepted_ancestor.
func TestAnIssueWithNothingAboveItInWorkIsARoot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const (
		doneAlone, goesOn, waitsForPerson = "LEGION-90", "LEGION-91", "LEGION-92"
		atWork, doneUnderWork, shape1238  = "LEGION-95", "LEGION-96", "LEGION-97"
		merging, doneUnderMerge, shape500 = "LEGION-100", "LEGION-101", "LEGION-102"
	)
	putIssue(t, pool, chainRecord(doneAlone, "", phase.Done, record.DoneStatus))
	seedSlottedInPhase(t, pool, testProject, goesOn, goesOn, phase.Implementing, func(issue *record.Issue) {
		issue.Parent, issue.HandedOver = new(doneAlone), false
	})
	putIssue(t, pool, chainRecord(waitsForPerson, doneAlone, phase.Admitted, "todo"))

	seedRunning(t, pool, atWork, "")
	putIssue(t, pool, chainRecord(doneUnderWork, atWork, phase.Done, record.DoneStatus))
	putIssue(t, pool, chainRecord(shape1238, doneUnderWork, phase.Admitted, "todo"))

	putIssue(t, pool, chainRecord(merging, "", phase.Merging, "retro"))
	seedApprovedGate(t, pool, merging, 1, 1)
	putIssue(t, pool, chainRecord(doneUnderMerge, merging, phase.Done, record.DoneStatus))
	putIssue(t, pool, chainRecord(shape500, doneUnderMerge, phase.Admitted, "todo"))

	tick(t, pool, admission, "done-parents")
	if stored := issue(t, pool, goesOn); stored.Phase != phase.Implementing || !holdsSlot(t, pool, goesOn) {
		t.Errorf("%s under a done parent = %s (slot %v), want it going on in implementing as a root at work",
			goesOn, stored.Phase, holdsSlot(t, pool, goesOn))
	}
	assertNotStarted(t, pool, waitsForPerson)
	line, held := passJudgement(t, pool, admission)
	if slices.Contains(line, waitsForPerson) {
		t.Errorf("the waiting line = %v, want no %s: a root that has not started waits for a person's handover", line, waitsForPerson)
	}
	assertHeldBy(t, held, waitsForPerson, record.HeldLabelOff)
	assertHeldBy(t, held, shape1238, record.HeldAncestry, atWork)
	assertHeldBy(t, held, shape500, record.HeldAcceptedAncestor, merging)
}
