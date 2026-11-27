package admit

import (
	"bytes"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A P0 issue starts at once, outside the cap (C2 and C4 of dispatch://INFRA-5636, R10 of
// dispatch://INFRA-3199): an issue is P0 when its own Dispatch priority is P0, the cap counts only
// the holders that are not P0, and every other hold on a start stays. Each case below stands at a
// cap its holders that are not P0 fill, so a candidate that takes a slot took it outside the cap.
// Every priority a case reads is taken from p0Priorities, never a sample of it.

// p0Priorities is every priority an issue can carry, P0 first.
var p0Priorities = []record.Priority{record.P0, record.P1, record.P2, record.P3, record.NoPriority}

// p0Holder records key as a started root of its own tree at priority, holding one of the cap's
// slots, as seedSlotted does.
func p0Holder(t *testing.T, pool *pgxpool.Pool, key, rank string, priority record.Priority) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, key, rank, phase.Admitted, func(issue *record.Issue) { issue.Priority = priority })
}

// p0Queued records key as a root handed to Legion that never ran, at priority, as seedWaiting
// does; shape says what else its record carries.
func p0Queued(t *testing.T, pool *pgxpool.Pool, key, rank string, priority record.Priority, shape func(*record.Issue)) {
	t.Helper()
	queued := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank, HandedOver: true, Priority: priority}
	if shape != nil {
		shape(&queued)
	}
	putIssue(t, pool, queued)
}

// p0Tree records root as the running root of its own tree at priority: its lifecycle open, its
// design gate approved when approved is true, and the root in accepting, which holds no slot, so
// every slot of a case is a holder the case names. A tree whose gate is not approved has its
// root at admitted, holding the row it took at its admission.
func p0Tree(t *testing.T, pool *pgxpool.Pool, root, rank string, priority record.Priority, approved bool) {
	t.Helper()
	gate := record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1}
	if !approved {
		seedSlottedInPhase(t, pool, testProject, root, rank, phase.Admitted, func(issue *record.Issue) { issue.Priority = priority })
		seedGate(t, pool, gate)
		return
	}
	gate.ApprovedVersion, gate.ApprovedMark = new(1), record.EventMark{Seq: 4, At: fixedNow.Add(-time.Hour)}
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: root, Tree: root, Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review", Rank: rank,
		HandedOver: true, Priority: priority})
	seedLifecycle(t, pool, root)
	seedGate(t, pool, gate)
}

// p0Member records key under root in stands at priority, with the Dispatch status a member in that
// phase shows and no legion label: a child needs none, its tree's root carries it.
func p0Member(t *testing.T, pool *pgxpool.Pool, root, key, rank string, stands phase.Phase, status string, priority record.Priority) {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: root, Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: rank,
		LastDispatchSeq: 2, Priority: priority})
}

// priorityEvent is a Dispatch event of key that changes nothing of its record but its priority:
// the event a person's change of the priority in Dispatch makes.
func priorityEvent(key, status, rank string, seq int64, priority record.Priority) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: status, Title: key,
		Rank: rank, Priority: priority, HandedOver: handed}
}

// A root of each priority waits in the line at a cap two holders below P0 fill. The pass gives the
// P0 root a slot and no other, and takes no slot from either holder.
func TestAP0RootStartsAtAFullCapAndNoRootBelowP0Does(t *testing.T) {
	for _, priority := range p0Priorities {
		t.Run(priorityName(priority), func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 2)
			p0Holder(t, pool, "LEGION-1", "A", record.P1)
			p0Holder(t, pool, "LEGION-2", "B", record.NoPriority)
			p0Queued(t, pool, "LEGION-9", "C", priority, nil)

			tick(t, pool, admission, "tick")

			want := []string{"LEGION-1", "LEGION-2"}
			if priority == record.P0 {
				want = append(want, "LEGION-9")
			}
			assertRowHolders(t, pool, want...)
		})
	}
}

// Under a running tree's root of each priority, a released child of each priority waits for its
// first phase at a cap two holders below P0 fill. Only the children whose own priority is P0 take a
// slot: a P0 root makes none of its children P0 (it reads the child's own priority, never
// record.PriorityOf's), and no child carries the legion label, which a child needs none of.
func TestOnlyAChildsOwnP0StartsItUnderARootOfEachPriority(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	p0Holder(t, pool, "LEGION-1", "A", record.P2)
	p0Holder(t, pool, "LEGION-2", "B", record.P3)
	var want []string
	for r, rootPriority := range p0Priorities {
		root := "LEGION-" + string(rune('A'+r))
		p0Tree(t, pool, root, "C"+string(rune('a'+r)), rootPriority, true)
		for c, childPriority := range p0Priorities {
			child := root + string(rune('0'+c))
			p0Member(t, pool, root, child, "C"+string(rune('a'+r))+string(rune('0'+c)), phase.Admitted, "todo", childPriority)
			if childPriority == record.P0 {
				want = append(want, child)
			}
		}
	}

	tick(t, pool, admission, "tick")

	assertRowHolders(t, pool, append(slices.Clone(want), "LEGION-1", "LEGION-2")...)
	for _, child := range want {
		if got := issue(t, pool, child); got.Phase != phase.Planning {
			t.Errorf("the P0 child %s stands in %s after the pass, want planning: its slot starts its first phase", child, got.Phase)
		}
	}
}

// Every hold on a start but the cap stays for a P0 issue: a root without the legion label, a root
// not in todo, a blocked_by link to an open issue, a closed design gate, an ancestor's hold, a
// lingering tree and a paused project each keep a P0 issue out of a slot. Each case holds a free
// P0 root beside the held one, which does start, so a case in which nothing started would read as
// a pass that never ran; the paused project starts nothing, and starts both once its pause ends.
func TestEveryHoldButTheCapStaysForAP0Issue(t *testing.T) {
	const held, free = "LEGION-50", "LEGION-60"
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "a root without the legion label", seed: func(t *testing.T, pool *pgxpool.Pool) {
			p0Queued(t, pool, held, "C", record.P0, func(issue *record.Issue) { issue.HandedOver = false })
		}},
		{name: "a root not in todo", seed: func(t *testing.T, pool *pgxpool.Pool) {
			p0Queued(t, pool, held, "C", record.P0, func(issue *record.Issue) { issue.Status = "backlog" })
		}},
		{name: "a blocked_by link to an open issue", seed: func(t *testing.T, pool *pgxpool.Pool) {
			p0Queued(t, pool, held, "C", record.P0, nil)
			seedMirror(t, pool, held, "todo", "LEGION-70")
			seedMirror(t, pool, "LEGION-70", "in_progress")
		}},
		{name: "a closed design gate", seed: func(t *testing.T, pool *pgxpool.Pool) {
			// The tree's root holds the row it took at its admission, below P0, so the cap has one
			// slot for no one: the holders below P0 fill it with this root and LEGION-1.
			p0Tree(t, pool, "LEGION-40", "Bz", record.P2, false)
			p0Member(t, pool, "LEGION-40", held, "C", phase.Admitted, "todo", record.P0)
		}},
		{name: "an ancestor's hold", seed: func(t *testing.T, pool *pgxpool.Pool) {
			p0Queued(t, pool, held, "C", record.P0, func(issue *record.Issue) { issue.Ancestry = []string{"LEGION-1"} })
		}},
		{name: "a lingering tree", seed: func(t *testing.T, pool *pgxpool.Pool) {
			until := fixedNow.Add(time.Hour)
			putIssue(t, pool, record.Issue{Key: "LEGION-40", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: "a lingering root", Tree: "LEGION-40", Phase: phase.Done, Generation: 1, Status: "done",
				Rank: "Bz", HandedOver: true, LingerUntil: &until, LastDispatchSeq: 1})
			p0Member(t, pool, "LEGION-40", held, "C", phase.Admitted, "todo", record.P0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 2)
			p0Holder(t, pool, "LEGION-1", "A", record.P1)
			if tc.name != "a closed design gate" {
				p0Holder(t, pool, "LEGION-2", "B", record.P1)
			}
			tc.seed(t, pool)
			p0Queued(t, pool, free, "D", record.P0, nil)

			tick(t, pool, admission, "tick")

			if holdsSlot(t, pool, held) {
				t.Fatalf("%s, at P0, holds a slot: %s holds a P0 issue as it holds every other", held, tc.name)
			}
			if !holdsSlot(t, pool, free) {
				t.Fatalf("%s, at P0 with no other hold, holds no slot: the pass started no P0 issue, so this case proves nothing", free)
			}
		})
	}

	t.Run("a paused project", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Queued(t, pool, held, "C", record.P0, nil)
		p0Queued(t, pool, free, "D", record.P0, nil)
		pause := pauseTestProject(t, pool, fixedNow)

		tick(t, pool, admission, "paused")
		assertRowHolders(t, pool, "LEGION-1")

		endTestPause(t, pool, pause, fixedNow)
		tick(t, pool, admission, "resumed")
		assertRowHolders(t, pool, "LEGION-1", held, free)
	})
}

// A root that waits below P0 at a full cap takes a slot at the pass of the Dispatch event that
// raises it to P0, with no other change of its record.
func TestAnIssueRaisedToP0WhileItWaitsStartsAtTheNextPass(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	engine := priorityEngine()
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)

	tick(t, pool, admission, "tick")
	assertRowHolders(t, pool, "LEGION-1")

	apply(t, pool, admission, "raised", priorityEvent("LEGION-9", "todo", "C", 2, record.P0), engine)
	assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
}

// A P0 issue that comes back and needs its slot again takes it at a full cap, whatever the kind of
// its place in the line: its work came back holding no row (returning), its wait on another issue
// ended (waiting_on_issue), and a root a person set back to todo after it finished (queued_root).
// A released child's first phase (first_phase) is TestOnlyAChildsOwnP0StartsItUnderARootOfEachPriority's.
// An answered root (answered) keeps its row through its wait and takes none.
func TestAP0IssueThatComesBackTakesItsSlotAtAFullCap(t *testing.T) {
	t.Run("returning", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		p0Tree(t, pool, "LEGION-100", "A", record.P1, true)
		p0Member(t, pool, "LEGION-100", "LEGION-101", "A1", phase.Implementing, "in_progress", record.P0)
		oweMemberStart(t, pool, "LEGION-100", "LEGION-101", phase.Implementing)
		p0Holder(t, pool, "LEGION-1", "B", record.P1)
		p0Holder(t, pool, "LEGION-2", "C", record.P3)

		tick(t, pool, admission, "tick")

		assertRowHolders(t, pool, "LEGION-1", "LEGION-2", "LEGION-101")
		if !claimableStart(t, pool, "LEGION-101") {
			t.Fatalf("the starts of LEGION-101 = %#v, want its implementer's task claimable on the slot it took", startsOf(t, pool, "LEGION-101"))
		}
	})

	t.Run("waiting_on_issue", func(t *testing.T) {
		const waiter, other, blocker = "LEGION-10", "LEGION-11", "LEGION-90"
		pool := migratedPool(t)
		admission := quiet(t, 1)
		seedStarted(t, pool, waiter, phase.Implementing, func(issue *record.Issue) { issue.Priority = record.P0 })
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedStarted(t, pool, other, phase.Implementing, func(issue *record.Issue) { issue.Priority = record.P1 })
		seedMirror(t, pool, other, "in_progress")
		seedMirror(t, pool, blocker, "in_progress")

		tick(t, pool, admission, "the-wait-begins")
		assertRowHolders(t, pool, other)

		dispatchEvent(t, pool, admission, "blocker-done", blocker, "done")
		assertRowHolders(t, pool, other, waiter)
	})

	t.Run("queued_root", func(t *testing.T) {
		const key = "LEGION-LINGER"
		pool := migratedPool(t)
		admission := quiet(t, 2)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Holder(t, pool, "LEGION-2", "B", record.P2)
		until := fixedNow.Add(time.Hour)
		putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: key, Tree: key, Phase: phase.Done, Generation: 1, Status: "done", Rank: "C", Priority: record.P0, LingerUntil: &until,
			LastDispatchSeq: 1, HandedOver: true})

		apply(t, pool, admission, "back-to-todo", priorityEvent(key, "todo", "C", 2, record.P0), priorityEngine())

		if got := issue(t, pool, key); got.Generation != 2 {
			t.Fatalf("%s stands at generation %d, want 2: the person's todo re-admits it", key, got.Generation)
		}
		assertRowHolders(t, pool, "LEGION-1", "LEGION-2", key)
	})
}

// A holder raised to P0 leaves the cap's count at the pass of the event that raises it, which gives
// the slot it leaves to the line below P0; a holder lowered below P0 keeps its slot and counts
// against the cap from then on. Neither event takes a slot from anyone.
func TestAHoldersP0LeavesTheCapsCountAndALoweredOneKeepsItsSlot(t *testing.T) {
	t.Run("raised", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
		tick(t, pool, admission, "tick")
		assertRowHolders(t, pool, "LEGION-1")

		apply(t, pool, admission, "raised", priorityEvent("LEGION-1", "in_progress", "A", 2, record.P0), priorityEngine())

		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	})

	t.Run("lowered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P0)
		p0Holder(t, pool, "LEGION-2", "B", record.P1)
		p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
		tick(t, pool, admission, "tick")
		assertRowHolders(t, pool, "LEGION-1", "LEGION-2")

		apply(t, pool, admission, "lowered", priorityEvent("LEGION-1", "in_progress", "A", 2, record.P2), priorityEngine())
		assertRowHolders(t, pool, "LEGION-1", "LEGION-2")

		// The holder below P0 that filled the cap closes: the lowered holder now fills it alone.
		closeTree(t, pool, admission, "second-closed", "LEGION-2", "B")
		assertRowHolders(t, pool, "LEGION-1")
	})
}

// slotFree reads the cap as the promotion reads it: a P0 holder takes none of the cap's slots, so
// a cap of one with a P0 holder alone has a slot free, and one with a holder below P0 beside it
// has none.
func TestSlotFreeCountsTheHoldersThePromotionCounts(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P0)
	if !slotFreeNow(t, pool, admission) {
		t.Fatal("slotFree says the cap of 1 is full with only a P0 issue holding a slot, want a slot free: the cap counts no P0 holder")
	}
	p0Holder(t, pool, "LEGION-2", "B", record.P1)
	if slotFreeNow(t, pool, admission) {
		t.Fatal("slotFree says a slot of the cap of 1 is free with a holder below P0, want none")
	}
}

// Each P0 start writes one line once its pass commits, naming the issue, the P0 holders, the other
// holders and the cap.
func TestAP0StartIsLoggedWithTheHoldersAndTheCap(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 2, slog.New(slog.NewJSONHandler(&logged, nil)))
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	p0Holder(t, pool, "LEGION-2", "B", record.P2)
	p0Holder(t, pool, "LEGION-3", "C", record.P0)
	p0Queued(t, pool, "LEGION-9", "D", record.P0, nil)

	tick(t, pool, admission, "tick")

	starts := loggedAs(t, &logged, p0StartMessage)
	if len(starts) != 1 {
		t.Fatalf("the pass logged %d lines %q, want one for LEGION-9; the log:\n%s", len(starts), p0StartMessage, logged.String())
	}
	got := starts[0]
	if got["issue"] != "LEGION-9" || got["p0_holders"] != float64(2) || got["other_holders"] != float64(2) || got["cap"] != float64(2) {
		t.Fatalf("the P0 start's line = %v, want issue LEGION-9, p0_holders 2 (LEGION-3 and LEGION-9), other_holders 2 and cap 2", got)
	}
}

// p0StartMessage is the message of the line each P0 start writes.
const p0StartMessage = "admission starts a P0 issue outside the cap"

// priorityName is the name a subtest of a priority takes: P0 to P3, or none.
func priorityName(priority record.Priority) string {
	if name := priority.String(); name != "" {
		return name
	}
	return "none"
}
