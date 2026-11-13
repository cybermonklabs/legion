package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// architectRow is root's architect phases row, and the zero row where the record holds none.
func architectRow(t *testing.T, pool *pgxpool.Pool, root string) record.PhaseRow {
	t.Helper()
	var rows []record.PhaseRow
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if rows, err = record.NewStore().Phases(context.Background(), tx, root); err != nil {
			t.Fatalf("read the phase rows of %s: %v", root, err)
		}
	})
	return record.PhaseRowOf(rows, claim.RoleArchitect)
}

// putArchitectRow records root's architect phases row as the transition that raised entry left
// it: on that entry, carrying its work (seededEntryTask) and the moment raisedAt it was raised.
func putArchitectRow(t *testing.T, pool *pgxpool.Pool, root string, entry int64, raisedAt time.Time) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: root, Role: claim.RoleArchitect,
			Claim: architectToken(t, root), Entry: entry, EntryTask: seededEntryTask(root), EntryRaisedAt: raisedAt}); err != nil {
			t.Fatalf("record the architect's phases row of %s at entry %d: %v", root, entry, err)
		}
	})
}

// architectStartsOf is every architect start the outbox holds for root, oldest first.
func architectStartsOf(t *testing.T, pool *pgxpool.Pool, root string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == root &&
			start.Op == "start" && start.Role == claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// assertArchitectEntry holds the two writes of a root architect's start to each other: the
// architect's phases row names entry, carries the work of that entry and the moment admission
// raised it, and the one architect start of that root carries the same number and the same task.
// It returns that start, for a case that reads what the task says.
func assertArchitectEntry(t *testing.T, pool *pgxpool.Pool, root string, entry int64) record.SuperviseRequest {
	t.Helper()
	starts := architectStartsOf(t, pool, root)
	if len(starts) != 1 {
		t.Fatalf("%s holds %d architect starts, want the one its promotion wrote: %#v", root, len(starts), starts)
	}
	start := starts[0]
	row := architectRow(t, pool, root)
	if row.Entry != entry || row.EntryTask == "" || !row.EntryRaisedAt.Equal(fixedNow) {
		t.Fatalf("%s's architect row = entry %d, task %q, raised at %s; want entry %d carrying that entry's work, raised at %s",
			root, row.Entry, row.EntryTask, row.EntryRaisedAt, entry, fixedNow)
	}
	if start.Entry != entry || start.Task != row.EntryTask {
		t.Fatalf("%s's architect start = entry %d carrying %q, want entry %d carrying the %q its row names: the two are written in one transaction",
			root, start.Entry, start.Task, entry, row.EntryTask)
	}
	return start
}

// A first admission hands a tree's architect its first work, so it raises that architect's phase
// entry, writes the task as the work of that entry on the architect's phases row and carries the
// number on the start it queues (workflow.Engine.EnterArchitect, record.SuperviseRequest.Entry).
// A root's architect holds that row exactly as every phase worker does, so the work survives the
// outbox row that carried it: the record alone says what this architect was handed.
func TestAFirstAdmissionRaisesItsArchitectEntryAndCarriesItOnTheStart(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)

	apply(t, pool, admission, "first", intake.DispatchIssue{Key: "LEGION-NEW", ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.created", Status: "todo", Title: "new", Rank: "A", HandedOver: true}, admission.engine)

	start := assertArchitectEntry(t, pool, "LEGION-NEW", 1)
	if want := architectStart(t, pool, admission, "LEGION-NEW", ""); start.Task != want {
		t.Fatalf("the first admission's architect task = %q, want the derivation's %q", start.Task, want)
	}
}

// A queued root a person hands back is admitted again, and that promotion hands its architect
// work again: it raises the entry from where the generation before left it, rather than writing
// a number of its own, and the row carries this generation's own task. The re-admission cleared
// the work the earlier entry named with the rest of that generation
// (record.Store.ClearTreeGeneration), so the record would otherwise name an entry whose work
// nothing could send.
func TestAReadmittedQueuedRootRaisesItsArchitectEntryAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	parkedRoot(t, pool)
	// The row the generation before left: the architect was handed its first entry then. The
	// claim token is written rather than minted, because LEGION-ROOT is no issue key.
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-ROOT",
			Role: claim.RoleArchitect, Claim: "claim-root-architect", Entry: 1,
			EntryTask: "LEGION-ROOT is yours.", EntryRaisedAt: handedBackAt}); err != nil {
			t.Fatalf("record the architect row of the generation before: %v", err)
		}
	})

	readmitRoot(t, pool, admission)

	start := assertArchitectEntry(t, pool, "LEGION-ROOT", 2)
	if start.Task == "LEGION-ROOT is yours." {
		t.Fatalf("the re-admission's architect task = %q, want this generation's own work", start.Task)
	}
}

// A hand-over carries a root into a tree of its own at the phase it ran in elsewhere, and its
// promotion hands that architect the work of taking that phase up: the entry is raised and the
// start carries it, for a root standing past admitted exactly as for one standing at it.
func TestAHandOverRaisesTheCarriedRootsArchitectEntryAtThePhaseItCarries(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const root = "LEGION-301"
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root to promote", Phase: phase.Accepting,
		Generation: 2, Status: "todo", Rank: "A", HandedOver: true, HandedOverFrom: "LEGION-700"})
	seedMirror(t, pool, root, "todo")

	tick(t, pool, admission, "tick")

	if stands := issue(t, pool, root); stands.Phase != phase.Accepting {
		t.Fatalf("the carried root stands in %s after its promotion, want the accepting the hand-over carried", stands.Phase)
	}
	start := assertArchitectEntry(t, pool, root, 1)
	if !workflow.CarriesArchitectStart(start.Task) {
		t.Fatalf("the carried root's architect task = %q, want the first-task instruction the derivation writes for it", start.Task)
	}
}

// The promotion of a root a person answered resumes its architect through ResumeAwaitedRoot, so
// that resume raises the architect's entry and writes the resume task as the work of it, and the
// start the promotion writes carries that number. The answer is then a fact of the record: a
// start row that reaches no claim loses nothing a person said.
func TestAnAnsweredRootsResumeAwaitedRootRaisesItsArchitectEntry(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Awaiting: &record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
	}
	putIssue(t, pool, root)
	seedLifecycle(t, pool, root.Key)
	takeSlot(t, pool, root.Key)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutGate(context.Background(), tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
			t.Fatalf("seed the closed gate: %v", err)
		}
		// The row the start that began this wait left: the architect stands on entry 2.
		if err := records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect,
			Claim: architectToken(t, root.Key), Entry: 2, EntryTask: "Register your spec.", EntryRaisedAt: handedBackAt}); err != nil {
			t.Fatalf("record the architect row of the wait: %v", err)
		}
	})

	applyFact(t, pool, admission, "approved-gate",
		intake.DispatchArtifact{Key: root.Key, ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3})

	if resumed := issue(t, pool, root.Key); resumed.Awaiting != nil {
		t.Fatalf("the answered root = %#v, want its wait ended by the promotion that resumed it", resumed)
	}
	assertArchitectEntry(t, pool, root.Key, 3)
}

// A root the derivation serves no task is handed nothing by its promotion, so it raises no entry
// (workflow.ArchitectStartTask's second rule: a root past admitted under its own power, whose
// work its own phase's role or the daemon's own resume tells, with nothing decided while its tree
// could not hear it). Raising one would leave the architect's row above every entry its claim was
// ever given, with no work on the row to send it: the start carries no task, so the outbox
// delivers nothing for it.
func TestAQueuedRootTheDerivationServesNoTaskRaisesNoArchitectEntry(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const root = "LEGION-302"
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a root that finished and came back", Phase: phase.Done,
		Generation: 1, Status: "todo", Rank: "A", HandedOver: true})
	seedMirror(t, pool, root, "todo")

	tick(t, pool, admission, "tick")

	starts := architectStartsOf(t, pool, root)
	if len(starts) != 1 || starts[0].Task != "" {
		t.Fatalf("%s holds the architect starts %#v, want the one its promotion wrote, carrying no task", root, starts)
	}
	if starts[0].Entry != 0 {
		t.Fatalf("the task-less architect start is of entry %d, want none: it hands the architect no work", starts[0].Entry)
	}
	if row := architectRow(t, pool, root); row.Entry != 0 || row.EntryTask != "" {
		t.Fatalf("%s's architect row = entry %d carrying %q, want neither: nothing was handed to it", root, row.Entry, row.EntryTask)
	}
}
