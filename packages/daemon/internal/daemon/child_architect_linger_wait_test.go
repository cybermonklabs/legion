package daemon

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A child's architect's first start that reaches the outbox while its tree does not run waits for
// the tree to run again rather than finishing, and acts once it does: the start the child's first
// phase start queued is the one row that creates that architect's claim, and a re-admission brings
// back only an architect that holds one (admit's startMidPhaseMembers). The child is released, the
// root leaves the workflow before the outbox runs the child's rows, and the outbox leaves the start
// queued with no claim created. The person then sets the root back to todo. Once the tree runs
// again, the start's next attempt, on its backoff, creates and spawns the child's architect claim on
// the child and in the root's tree, and the booted architect's first notice is its catch-up naming
// the child. That holds whether the root comes back inside its linger or after its deadline, which
// a re-admission does not read (record.Issue.Readmittable), and the start goes on waiting while the
// re-admitted root waits in the line for a slot, since its tree is opened only by the promotion
// that gives it one.
func TestAChildsArchitectFirstStartWaitsForItsTreeToRunAgain(t *testing.T) {
	t.Run("a reopen inside the linger", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		token := mustClaimToken(t, doorChild, claim.RoleArchitect)
		parkTheRootBeforeTheChildsRowsRun(rig)

		called := len(rig.rt.Calls())
		reopenTheRoot(rig)
		wantTheWaitingStartToAct(rig, token, called)
	})

	t.Run("a linger that has run out", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		token := mustClaimToken(t, doorChild, claim.RoleArchitect)
		rig.written("release", childAt("todo", 2))
		rig.apply("root-parked", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog", HandedOver: true,
			Title: "root", Rank: "U"})
		// The linger's deadline is moved into the past before the outbox runs the child's rows: the
		// tree still lingers (record.Issue.Lingers), and its linger no longer runs.
		root := readIssue(t, rig.pool, doorRoot)
		if root == nil || !root.Lingers() {
			t.Fatalf("the parked root = %+v, want its tree lingering", root)
		}
		expired := time.Now().Add(-time.Minute)
		root.LingerUntil = &expired
		rig.put(*root)
		rig.drain()
		if attempts := architectStartAttempts(rig); attempts == 0 {
			t.Fatal("the outbox never tried the start of the child's architect after the linger ran out")
		}
		if _, found := rig.sup.Machine(token); found {
			t.Fatal("the child got an architect claim while its tree lingers past its deadline, want its start waiting with no claim created")
		}

		called := len(rig.rt.Calls())
		reopenTheRoot(rig)
		wantTheWaitingStartToAct(rig, token, called)
	})

	t.Run("a re-admitted root waiting for its slot", func(t *testing.T) {
		rig := newChildArchitectRig(t)
		token := mustClaimToken(t, doorChild, claim.RoleArchitect)
		parkTheRootBeforeTheChildsRowsRun(rig)

		// Three other trees run, each root at work in a slot, which fills the rig's admission cap of
		// three: the park gave back the slots of the root and the child.
		var held int
		if err := rig.pool.QueryRow(rig.ctx, `select count(*) from slots`).Scan(&held); err != nil || held != 0 {
			t.Fatalf("the parked tree holds %d slots (%v), want the park to have given back every one", held, err)
		}
		others, ranks := []string{"LEGION-20", "LEGION-21", "LEGION-22"}, []string{"W", "X", "Y"}
		for i, key := range others {
			rig.put(record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: key,
				Title: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: ranks[i], HandedOver: true, LastDispatchSeq: 1})
			openTree(t, rig.pool, "LEGION", key)
		}
		holdSlot(t, rig.pool, others...)

		called := len(rig.rt.Calls())
		rig.apply("root-todo", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo", HandedOver: true,
			Title: "root", Rank: "U"})
		if root := readIssue(t, rig.pool, doorRoot); root.Lingers() || root.Status != "todo" || root.Phase != phase.Admitted {
			t.Fatalf("the root after its todo under a full cap = %s at %s lingering until %v, want it re-admitted and waiting in the line at admitted in todo",
				root.Status, root.Phase, root.LingerUntil)
		}
		// The start runs again on its backoff (record.RetryBackoff) while the root waits: it is
		// drained until at least one more attempt has run, and none of them created the claim.
		before := architectStartAttempts(rig)
		testwait.Eventually(t, "another attempt of the waiting start of the child's architect while its root waits for a slot", func() bool {
			rig.drain()
			return architectStartAttempts(rig) > before
		})
		if _, found := rig.sup.Machine(token); found {
			t.Fatal("the child got an architect claim while its re-admitted root waits for a slot, want its start waiting until the tree is opened")
		}
		if launches := launchesSince(rig.rt, token, called); len(launches) != 0 {
			t.Fatalf("the child's architect was launched %+v while its root waits for a slot, want no launch", launches)
		}

		// A person parks one of the other trees, which gives its slot back: the same fact's
		// admission pass promotes the waiting root, which opens its tree again.
		rig.apply("other-parked", intake.DispatchIssue{Key: others[0], ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", HandedOver: true,
			Title: others[0], Rank: "W"})
		if root := readIssue(t, rig.pool, doorRoot); root.Status != "in_progress" {
			t.Fatalf("the root after a slot came free stands %s at %s, want it promoted in_progress", root.Status, root.Phase)
		}
		wantTheWaitingStartToAct(rig, token, called)
	})
}

// A start queued for news alone (record.SuperviseRequest.News) of a child's own architect has the
// shape of that architect's first start: the architect's role on a child, with no phase and no
// task. It is no first start (childArchitectFirstStart). It carries news to a claim that already
// holds its role, and one that reaches no claim finishes, since the start that creates or revives
// the claim carries the news. So while the child's tree does not run it never waits on its backoff
// as the first start does (startRoot). Under a lingering tree it finishes as every start of a
// member of that tree does. Under a re-admitted root waiting in the line for its tree to be opened,
// it finishes as a news-only start that reaches no claim (newsOnlyRefusal). Neither creates the
// child's architect claim. Read as a first start, each returns the wait instead.
func TestANewsOnlyStartOfAChildsArchitectWithNoClaimFinishesWhileItsTreeDoesNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(*record.Issue)
		// readmitted clears the root's design gate, as a re-admission clears it with the rest of
		// the generation (admit's readmit), so the root's record says its tree has not run.
		readmitted bool
		says       string
	}{
		{"a lingering tree", func(root *record.Issue) {
			until := time.Now().Add(time.Hour)
			root.Phase, root.Status, root.LingerUntil = phase.Done, "backlog", &until
		}, false, "outbox start of a member of a lingering tree; finished without acting"},
		{"a re-admitted root waiting in the line", func(root *record.Issue) {
			root.Phase, root.Status = phase.Admitted, "todo"
		}, true, "outbox news-only start reaches no claim; finished without acting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newChildArchitectRig(t)
			var logged testlog.Buffer
			// The outbox reads the line under the design-gate steps the daemon gates, as the daemon
			// wires it (startRoot).
			rig.runner.log, rig.runner.gatedSteps = slog.New(slog.NewTextHandler(&logged, nil)), config.DesignGateRootIssues.Steps()
			root := readIssue(t, rig.pool, doorRoot)
			tc.root(root)
			rig.put(*root, rig.member(doorChild, doorRoot, phase.Accepting, "needs_review", "V"))
			if tc.readmitted {
				if _, err := rig.pool.Exec(rig.ctx, `delete from design_gates where issue = $1`, doorRoot); err != nil {
					t.Fatalf("clear the design gate of the re-admitted %s: %v", doorRoot, err)
				}
				// A setup guard: the case is the root waiting in the line, or it tests nothing.
				if !record.QueuedRoot(*root, record.TreeGate{Steps: rig.runner.gatedSteps}) {
					t.Fatalf("the re-admitted root %+v with no gate does not wait in the line, want it waiting (record.QueuedRoot)", root)
				}
			}
			row, err := record.NewOutboxRow(doorChild, record.SuperviseRequest{Op: "start", Tree: doorRoot, Role: claim.RoleArchitect,
				Generation: 1, News: true}, time.Now())
			if err != nil {
				t.Fatalf("build the news-only start of %s's architect: %v", doorChild, err)
			}

			if err := rig.runner.execute(rig.ctx, row); err != nil {
				t.Fatalf("the news-only start of %s's architect, with no claim, under %s = %v, want it finished without acting", doorChild, tc.name, err)
			}
			if _, found := rig.sup.Machine(mustClaimToken(t, doorChild, claim.RoleArchitect)); found {
				t.Fatalf("the news-only start of %s's architect created its claim, want none created", doorChild)
			}
			if !strings.Contains(logged.String(), tc.says) {
				t.Fatalf("the outbox logged %q, want %q", logged.String(), tc.says)
			}
		})
	}
}

// parkTheRootBeforeTheChildsRowsRun is the child released, then its root moved out of the workflow
// before the outbox runs the child's rows, and those rows drained: the tree lingers, and the start
// of the child's architect waits with no claim created.
func parkTheRootBeforeTheChildsRowsRun(rig *childArchitectRig) {
	rig.t.Helper()
	rig.written("release", childAt("todo", 2))
	rig.apply("root-parked", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog", HandedOver: true,
		Title: "root", Rank: "U"})
	rig.drain()
	if _, found := rig.sup.Machine(mustClaimToken(rig.t, doorChild, claim.RoleArchitect)); found {
		rig.t.Fatal("the lingering tree's child got an architect claim, want its start waiting with no claim created")
	}
	if starts := rig.architectStarts(doorChild); starts != 1 {
		rig.t.Fatalf("the lingering tree's outbox left %d starts of the child's architect queued, want its first start waiting", starts)
	}
}

// reopenTheRoot is the person setting the root back to todo, which re-admits it and, with a slot
// free, opens its tree again with the child in it.
func reopenTheRoot(rig *childArchitectRig) {
	rig.t.Helper()
	rig.apply("root-todo", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo", HandedOver: true,
		Title: "root", Rank: "U"})
	if got := readIssue(rig.t, rig.pool, doorChild); got.Phase != phase.Planning || got.Tree != doorRoot {
		rig.t.Fatalf("the child after the root's reopen = %s in the tree of %s, want planning in the tree of %s", got.Phase, got.Tree, doorRoot)
	}
}

// wantTheWaitingStartToAct is the waiting start of the child's architect run once its tree runs
// again: it spawns one fresh claim (token) among the runtime's calls after its first called, and
// the booted architect is told the child (wantChildsArchitectToldItsIssues). The start runs again
// on its backoff (record.RetryBackoff), a few seconds after its last attempt, so the outbox is
// drained until that attempt has come and gone.
func wantTheWaitingStartToAct(rig *childArchitectRig, token claim.Token, called int) {
	rig.t.Helper()
	testwait.Eventually(rig.t, "the waiting start of the child's architect to run once the tree runs again", func() bool {
		rig.drain()
		return rig.architectStarts(doorChild) == 0
	})
	launches := launchesSince(rig.rt, token, called)
	if len(launches) != 1 || launches[0].Method != "Spawn" {
		rig.t.Fatalf("the tree running again launched the child's architect %+v, want one spawn of a fresh claim", launches)
	}
	rig.wantChildsArchitectToldItsIssues([]string{doorChild})
}

// architectStartAttempts is how many times the outbox has tried the one queued start of the
// child's architect, failing the test when none is queued: the start finished or acted.
func architectStartAttempts(rig *childArchitectRig) int {
	rig.t.Helper()
	var attempts int
	if err := rig.pool.QueryRow(rig.ctx, `select attempts from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'architect'`, doorChild).Scan(&attempts); err != nil {
		rig.t.Fatalf("read the attempts of the queued start of %s's architect, want one start waiting: %v", doorChild, err)
	}
	return attempts
}
