package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedMirror writes what Dispatch says of key — its status and, in order, the keys it waits on —
// as the blockers.Mirror handler writes one from an event. Its sequence is 1, so an event a test
// then applies through the mirror (sequence 2 or more) wins the row's fence.
func seedMirror(t *testing.T, pool *pgxpool.Pool, key, status string, waits ...string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry(waits)})
	})
}

// seedMirrorChild writes what Dispatch says of a child filed under parent: its status and the
// parent that makes it one of parent's open children (record.ChildWaits reads the pair).
func seedMirrorChild(t *testing.T, pool *pgxpool.Pool, key, status, parent string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry[[]string](nil)})
	})
}

// applyMirrored applies fact through the daemon's own handler order (daemon/workflow.go): the
// mirror first, so the engine behind it reads the row this very event writes.
func applyMirrored(t *testing.T, pool *pgxpool.Pool, engine *Engine, eventID string, fact intake.Fact) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), "LEGION")
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, mirror, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// startUnblockedChildren is admission's own pass over the waiting line with nothing held
// (admissionPass): the engine no longer chooses which issues start — record.Waiting does, from
// the tree lifecycles, the design gates and the slots — so the pass reads the line as admission
// reads it and gives each issue whose first phase can start what its slot would give it.
func startUnblockedChildren(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	admissionPass(t, pool, engine)
}

// seedHeldTree records a running root whose design gate is gate, admitted at the engine's own
// clock: the tree the held children of these tests belong to.
func seedHeldTree(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, gate)
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// seedAdmittedChild records a child of that tree where enterChild leaves one it does not start.
func seedAdmittedChild(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: key, Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 4, Status: "todo", Rank: "V", LastDispatchSeq: 1})
}

// A child released into a tree whose gate is open, but which waits on an issue Dispatch has not
// finished, is recorded and left at phase.Admitted with nothing queued for it — exactly where a
// child whose tree's gate is still closed is left. A sibling that waits on nothing is released as
// it always was.
func TestAChildReleasedWhileABlockerIsUnfinishedStaysAdmitted(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "child-blocked", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a blocked child", Parent: "LEGION-208", Rank: "V", Blockers: []string{"LEGION-300"}})

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s, want it left at admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	// Neither child starts at its release now: each is an issue the cap counts, so each waits at
	// admitted until the line reaches it. The pass below is the line's.
	applyMirrored(t, pool, engine, "child-free", intake.DispatchIssue{
		Key: "LEGION-210", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "a child that waits on nothing", Parent: "LEGION-208", Rank: "W"})
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Admitted {
		t.Fatalf("the unblocked child = %s at its release, want it left at admitted until it has a slot", got)
	}
	assertOutboxKinds(t, pool, []string{})

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s after the line's pass, want it left at admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the unblocked child = %s once the line reached it, want planning", got)
	}
	// The first supervise row is the child's own architect's start, queued behind its branch row
	// and before its planner's (Engine.gateOpened). The second notice is the child-status that
	// every child the line starts tells the architect above it (ChildStartedNotice), whether a
	// blocker held it or its tree's gate did.
	assertOutboxKinds(t, pool, []string{"issue_branch", "supervise", "dispatch_status", "plan_record", "supervise", "notice", "notice"})
}

// The close of the last issue a held child waited on starts it: its branch row, its own
// architect's start behind that row, the in_progress its entry into planning writes to Dispatch,
// its planner's start behind the branch row, and one child-status notice to the architect above it
// (here the tree's) naming the issues it waited on and saying that the child's own architect owns
// it while that architect holds its role.
func TestAHeldChildStartsWhenItsLastBlockerCloses(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300", "LEGION-301")
	seedMirror(t, pool, "LEGION-300", "done")
	seedMirror(t, pool, "LEGION-301", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while one issue it waits on is unfinished, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	applyMirrored(t, pool, engine, "blocker-done", intake.DispatchIssue{
		Key: "LEGION-301", ResolvedRepo: "sjawhar/legion", Seq: 4, Type: "issue.updated", Status: "done", Title: "the last blocker"})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once every issue it waited on is done, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"issue_branch", "supervise", "dispatch_status", "plan_record", "supervise", "notice", "notice"})
	assertBranchRow(t, pool, "LEGION-209", 4)
	assertPlannerStart(t, pool, "LEGION-209")
	want := "LEGION-209 starts planning under LEGION-208: every issue it waited on (LEGION-300, LEGION-301) is done" + childStartedOwnership
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// The child-status notice a child's start tells the architect above it names the tree the child
// starts in as the Dispatch chain gives it, never the tree its record stores (R15 of
// dispatch://INFRA-3320): a child Dispatch moved under the live tree after it was recorded under
// another root is told as starting under the live tree's root.
//
// Negative control, run when this was written: with the notice naming the record's stored tree,
// it reads "starts planning under LEGION-100".
func TestAChildsStartNamesTheTreeOfItsChainNotItsStoredTree(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-100", Project: "LEGION", Title: "LEGION-209", Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 4, Status: "todo", Rank: "V", LastDispatchSeq: 1})
	startUnblockedChildren(t, pool, testEngine(config.DesignGateRootIssues, nil))

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once the line reached it, want planning", got)
	}
	want := "LEGION-209 starts planning under LEGION-208: it reached the front of the waiting line and took an admission slot" + childStartedOwnership
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// A held child whose links a person takes off while it waits is held by nothing any longer, and
// starts at the next look: left admitted, it would stand unstarted with nothing naming it, since
// the state's `blocked` names only a child with an unfinished blocker and the edit, which changes
// no status, is not one the engine's handling of the event acts on. It starts as a child its tree's
// gate opened onto does, and the architect above it (here its tree's) is told so by the same
// child-status notice a blocker's close sends, its reason saying it reached the front of the
// waiting line, since no issue it waited on closed.
func TestAHeldChildWhoseLinksAreTakenOffStarts(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while the issue it waits on is unfinished, want admitted", got)
	}

	applyMirrored(t, pool, engine, "links-taken-off", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"})
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s on the edit itself, want it still admitted: the event changes no status", got)
	}
	if got := blockedNow(t, pool); len(got) != 0 {
		t.Fatalf("blocked = %#v once the child names no blocker, want nothing", got)
	}
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once it names no blocker, want planning", got)
	}
	assertBranchRow(t, pool, "LEGION-209", 4)
	assertPlannerStart(t, pool, "LEGION-209")
	want := "LEGION-209 starts planning under LEGION-208: it reached the front of the waiting line and took an admission slot" + childStartedOwnership
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// blockedNow is the state's `blocked` as the project stands: every recorded issue and the whole
// mirror, judged as the projection judges them, with no root waiting for a slot.
func blockedNow(t *testing.T, pool *pgxpool.Pool) []record.BlockedIssue {
	t.Helper()
	var blocked []record.BlockedIssue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		issues, err := store.Issues(t.Context(), tx)
		if err != nil {
			return err
		}
		mirrored, err := store.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			return err
		}
		keys := make([]string, len(issues))
		for i, issue := range issues {
			keys[i] = issue.Key
		}
		chains, err := record.ReadChains(t.Context(), tx, store, "LEGION", keys)
		if err != nil {
			return err
		}
		blocked = blockers.Blocked(blockers.Rows(mirrored), issues, nil, record.LineFacts{Chains: chains})
		return nil
	})
	return blocked
}

// The opening of a tree's design gate tells every phase worker of the tree and the root's
// architect, and releases every admitted member it was holding, and the waiting line then starts
// each of them at its own slot — except one that waits on an unfinished issue of its own, whose own
// blockers hold it where the gate left it. The opening queues the reconcile of the tree's news, and
// the root's planner, whose record knows no version it took, is owed that news and is told the
// opening by the task that carries it, where the root's architect, whose view begins at this first
// opening, is sent the opened message.
func TestTheGateOpeningDoesNotStartAHeldChild(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "backlog")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s when the tree's gate opened, want it left at admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the child that waits on nothing = %s once the line reached it, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"design_changes", "role_message", "notice", "issue_branch", "supervise", "dispatch_status", "plan_record", "supervise", "notice", "notice"})
}

// An unblocked child whose tree would not start it now is in no line either: the tree's gate is
// what holds it, and the line reads that gate (record.Waiting's LineFirstPhase).
func TestAnUnblockedChildWaitsForItsTreesGate(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "done")

	startUnblockedChildren(t, pool, testEngine(config.DesignGateRootIssues, nil))

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while its tree's gate is closed, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})
}

// A root whose tree already runs and whose design gate opens while Dispatch shows it blocked by an
// issue that is not done does not start its first phase: that phase is what waits now, so the
// release for a wait takes the row its architect wrote the design in back (admit's
// releaseSlotsForWaits, judged by blockers.waitsOf), marks it (record.Issue.ReleasedForWaitAt),
// and the root stands in the line as an issue whose work came back, which nothing starts while
// its blocker is open (record.LineWaitingOnIssue). A tree Legion took back after it finished and
// lingered is no exception: blockers.Exempt, which names it, is the promotion's rule about what
// a candidate may start in spite of its links, and the release does not read it. Only three
// roots keep their slot through such a wait, and neither case here is one of them
// (blockers.waitsOf). This replaces an older test that said the root's first phase starts
// whatever its links say, which held only because its stand-in pass never ran the release.
func TestTheGateOpeningLeavesARootWaitingOnItsBlockerWithItsSlotGivenBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		takenBack bool
	}{
		{name: "a root that took its slot before a blocker was added"},
		{name: "a tree taken back after it finished and lingered", takenBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U",
				HandedOver: true, TakenBack: tc.takenBack, LastDispatchSeq: 1})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			seedMirror(t, pool, "LEGION-208", "in_progress", "LEGION-300")
			seedMirror(t, pool, "LEGION-300", "backlog")
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)})

			engine := testEngine(config.DesignGateRootIssues, nil)
			applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
				Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
			startUnblockedChildren(t, pool, engine)

			if got := phaseOf(t, pool, "LEGION-208"); got != phase.Admitted {
				t.Fatalf("the admitted root = %s when its tree's gate opened, want admitted: its first phase waits on its unfinished blocker", got)
			}
			var slotted int
			if err := pool.QueryRow(t.Context(), "select count(*) from slots where issue = 'LEGION-208'").Scan(&slotted); err != nil {
				t.Fatalf("count the root's slot rows: %v", err)
			}
			if slotted != 0 {
				t.Fatalf("the root holds %d slot rows once its gate opened, want none: it gave its row back for the wait", slotted)
			}
			if got := issueRecord(t, pool, "LEGION-208"); got == nil || got.ReleasedForWaitAt == nil {
				t.Fatalf("the root = %+v, want it marked released for a wait", got)
			}
			var started int
			if err := pool.QueryRow(t.Context(), "select count(*) from outbox where kind = 'supervise' and issue = 'LEGION-208' and payload->>'role' = 'planner'").Scan(&started); err != nil {
				t.Fatalf("count the planner starts: %v", err)
			}
			if started != 0 {
				t.Fatalf("the root's planner was started %d times, want never while its blocker is open", started)
			}
		})
	}
}

// R2: the notice that reports the move out of admitted carries no role. The gate's opening is
// the one move out of that phase (Table), and admitted is the architect's own (phase.RoleFor), so
// a role on the notice would tell the architect that its own phase ended. noticeFor writes the
// notice of a move out of admitted with no role.
func TestThePhaseFinishedNoticeOutOfAdmittedCarriesNoRole(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U",
		LastDispatchSeq: 1})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedMirror(t, pool, "LEGION-208", "in_progress")

	engine := testEngine(config.DesignGateRootIssues, nil)
	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-208"); got != phase.Planning {
		t.Fatalf("the admitted root = %s when its tree's gate opened, want planning", got)
	}
	// The role is written only when there is one (record.Notice's own encoding), so an absent
	// key and an empty one both read as "no role".
	var kind, role, from string
	if err := pool.QueryRow(t.Context(),
		`select payload->>'kind', coalesce(payload->>'role', ''), payload->>'phase' from outbox
		where kind = 'notice' and payload->>'kind' = 'phase-finished'`).Scan(&kind, &role, &from); err != nil {
		t.Fatalf("read the phase-finished notice of the gate's opening: %v", err)
	}
	if kind != "phase-finished" || role != "" || from != string(phase.Admitted) {
		t.Fatalf("notice = %s role %q phase %s, want a phase-finished for admitted carrying no role", kind, role, from)
	}
}

// An issue's run earns its mark (record.Issue.TakenBack) as it moves into a phase of its own, and
// the mark belongs to that run: a child's re-entry into its next generation in its own tree opens a
// new run, which starts without it — held there by a blocker, it is checked like any child that
// never ran — while the root's leave into a linger opens no run, and its record keeps it. So the record
// a tree that ran leaves for its taken-back root is already one promotion exempts
// (blockers.Exempt), whatever re-admission later writes of its phase and generation, which keeps
// the mark of a root whose own tree comes back — while a member that never started carries none.
func TestTheRunMarkIsWrittenAsAPhaseStartsAndBelongsToThatRun(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U",
		HandedOver: true, LastDispatchSeq: 1})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	engine := testEngine(config.DesignGateRootIssues, nil)
	marked := func(key string) bool {
		t.Helper()
		var got bool
		if err := pool.QueryRow(t.Context(), "select taken_back from issues where key = $1", key).Scan(&got); err != nil {
			t.Fatalf("read the mark of %s: %v", key, err)
		}
		return got
	}
	if marked("LEGION-208") {
		t.Fatalf("LEGION-208 is marked before any phase of it started")
	}

	// The gate opens with no Dispatch child under the root, so its own first phase starts (an
	// open child would hold it first: the children-first wait).
	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-208"); got != phase.Planning || !marked("LEGION-208") {
		t.Fatalf("LEGION-208 = %s marked %t once the gate opened, want planning marked true", got, marked("LEGION-208"))
	}

	// The children its architect files after it: one starts its first phase and is marked, and
	// one an unfinished link holds stays at admitted unmarked.
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-210", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "in_progress")
	for _, key := range []string{"LEGION-209", "LEGION-210"} {
		if marked(key) {
			t.Fatalf("%s is marked before any phase of it started", key)
		}
	}
	startUnblockedChildren(t, pool, engine)
	for key, want := range map[string]struct {
		at     phase.Phase
		marked bool
	}{
		"LEGION-208": {phase.Planning, true}, "LEGION-209": {phase.Planning, true}, "LEGION-210": {phase.Admitted, false},
	} {
		if got := phaseOf(t, pool, key); got != want.at || marked(key) != want.marked {
			t.Fatalf("%s = %s marked %t once its first phase could start, want %s marked %t", key, got, marked(key), want.at, want.marked)
		}
	}

	// The root leaves Legion's work: it stops where it stands, and its record keeps the mark.
	before := phaseOf(t, pool, "LEGION-208")
	applyMirrored(t, pool, engine, "root-backlog", intake.DispatchIssue{
		Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "backlog",
		Title: "root", Rank: "U", HandedOver: true})
	var root *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		root, err = record.NewStore().Issue(t.Context(), tx, "LEGION-208")
		return err
	})
	if root.Phase != before || root.Status != "backlog" || !root.TakenBack {
		t.Fatalf("the root that left = %#v, want it stopped in %s and still marked", *root, before)
	}
	// A record at admitted carrying the mark is exempt by that alone: it carries no run.
	taken := *root
	taken.Phase, taken.Status = phase.Admitted, "todo"
	if taken.CarriesRun() || !blockers.Exempt(taken) {
		t.Fatalf("the root taken back = %#v, want it exempt by its mark alone", taken)
	}
	if waits := blockers.WaitsOn(map[string]blockers.Row{"LEGION-208": {Status: "todo", Blockers: []string{"LEGION-300"}}}, taken); len(waits) != 0 {
		t.Fatalf("the root taken back waits on %v, want nothing: it ran", waits)
	}
}

func phaseOf(t *testing.T, pool *pgxpool.Pool, key string) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", key).Scan(&got); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return phase.Phase(got)
}

func assertBranchRow(t *testing.T, pool *pgxpool.Pool, issue string, generation int) {
	t.Helper()
	var gotIssue string
	var gotGeneration int
	if err := pool.QueryRow(t.Context(), "select issue, (payload->>'generation')::int from outbox where kind = 'issue_branch'").Scan(&gotIssue, &gotGeneration); err != nil {
		t.Fatalf("read the branch row: %v", err)
	}
	if gotIssue != issue || gotGeneration != generation {
		t.Fatalf("branch row = %s at generation %d, want %s at %d", gotIssue, gotGeneration, issue, generation)
	}
}

func assertPlannerStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	var gotIssue, op, role string
	if err := pool.QueryRow(t.Context(), "select issue, payload->>'op', payload->>'role' from outbox where kind = 'supervise' and coalesce(payload->>'task', '') <> ''").Scan(&gotIssue, &op, &role); err != nil {
		t.Fatalf("read the supervise row: %v", err)
	}
	if gotIssue != issue || op != "start" || role != "planner" {
		t.Fatalf("supervise row = %s %s %s, want a planner start for %s", gotIssue, op, role, issue)
	}
}

// childStartedOwnership is how every child-status notice of a child's start ends, after its
// reason: the child's own architect starts with it and owns it while that architect holds its
// role, so the architect it tells hears only what goes above that architect (ChildStartedNotice).
const childStartedOwnership = ". Its own architect starts with it and owns it while that architect holds its role: its " +
	"decomposition, its children and its acceptance. You hear its child-status and child-closed notices, a worker-died or " +
	"worker-stalled of its architect, and its other notices while its architect does not hold its role, which the daemon's " +
	"holdsRole decides"

func childStatusNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'child-status' order by id")
	if err != nil {
		t.Fatalf("read the child-status notices: %v", err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan a child-status notice: %v", err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the child-status notices: %v", err)
	}
	return reasons
}
