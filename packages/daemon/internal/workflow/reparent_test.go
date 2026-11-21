package workflow

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/record/fencetest"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The issues of these tests: two roots at work, LEGION-208 and LEGION-300, each with its architect
// running, and LEGION-500 at work under LEGION-208, implementing in its slot, its own architect
// running and its spec step open on its own approval.
const (
	reparentFirst  = "LEGION-208"
	reparentSecond = "LEGION-300"
	reparentMoved  = "LEGION-500"
)

// seedReparentFixture records the issues of these tests (each with its copy row, seedIssue) and
// the architect claims of all three.
func seedReparentFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedLiveRoot(t, pool, reparentFirst, 0)
	seedLiveRoot(t, pool, reparentSecond, 1)
	seedIssue(t, pool, record.Issue{Key: reparentMoved, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: reparentFirst, Parent: new(reparentFirst), Project: "LEGION", Title: "the moved issue", Phase: phase.Implementing, Generation: 2,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "Z", LastDispatchSeq: 4})
	seedGate(t, pool, record.DesignGate{Issue: reparentMoved, ArtifactID: "artifact-500", LatestVersion: 2, ApprovedVersion: new(2)})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: reparentMoved, Index: 2, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	})
	for _, key := range []string{reparentFirst, reparentSecond, reparentMoved} {
		token, err := claim.NewToken("legion", key, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, 'legion', $2, $2, 'architect', 1, $3, $4, $5, 0, 0, 0, 0, '')`,
			string(token), key, "ses-"+key, "/sessions/"+key+".jsonl", string(supervise.StateWorking)); err != nil {
			t.Fatalf("seed the architect claim of %s: %v", key, err)
		}
	}
}

// moveParent applies one Dispatch event of the moved issue under parent ("" for none) at seq
// through the mirror and the engine, then records the observation as admission's
// recordObservation does after them, so the next event reads the record's parent as this one left
// it.
func moveParent(t *testing.T, pool *pgxpool.Pool, engine *Engine, id, parent string, seq int64) {
	t.Helper()
	applyMirrored(t, pool, engine, id, intake.DispatchIssue{Key: reparentMoved, ResolvedRepo: "sjawhar/legion", Seq: seq,
		Type: "issue.updated", Status: "in_progress", Title: "the moved issue", Parent: parent, Rank: "Z"})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(t.Context(), tx, reparentMoved)
		if err != nil {
			return err
		}
		issue.Parent, issue.LastDispatchSeq = record.ParentOf(parent), seq
		return record.NewStore().PutIssue(t.Context(), tx, *issue)
	})
}

// When an event changes the Dispatch parent of an issue in Legion's work, the engine queues one
// re-parent seed in the event's own transaction and writes the new parent as told (R18 of
// dispatch://INFRA-3320, record.Issue.ToldParent): the seed names the issue's run, the old parent
// the record held before the event and the new one, the phase under way, its slot and its gate
// steps, and still carries the gate root and the words the release before reads. An event that
// repeats the parent queues nothing, a move back to an earlier parent is told anew, and so is a
// move to no parent; an issue not in Legion's work is told nothing.
func TestAParentChangeIsSeededOncePerNewParent(t *testing.T) {
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	engine := testEngine(config.DesignGateRootIssues, nil)

	moveParent(t, pool, engine, "moved-under-300", reparentSecond, 5)
	seeds := reparentSeeds(t, pool, reparentMoved)
	want := record.ReparentSeed{Parent: reparentSecond, Tree: reparentSecond, Ran: reparentRan, Generation: 2, OldParent: reparentFirst,
		Phase: phase.Implementing, Slot: true, Steps: []record.ReparentStep{{Step: record.DesignStepSpec, Open: true}}}
	if len(seeds) != 1 || !reflect.DeepEqual(seeds[0], want) {
		t.Fatalf("the re-parent seeds of %s = %#v, want one reading %#v", reparentMoved, seeds, want)
	}
	if got := toldParent(t, pool, reparentMoved); got != reparentSecond {
		t.Fatalf("the parent recorded as told = %q, want %s", got, reparentSecond)
	}

	moveParent(t, pool, engine, "edited-under-300", reparentSecond, 6)
	if again := reparentSeeds(t, pool, reparentMoved); len(again) != 1 {
		t.Fatalf("the re-parent seeds after an event under the same parent = %#v, want the first alone", again)
	}

	moveParent(t, pool, engine, "moved-back-under-208", reparentFirst, 7)
	moveParent(t, pool, engine, "unlinked", "", 8)
	seeds = reparentSeeds(t, pool, reparentMoved)
	if len(seeds) != 3 || seeds[1].Parent != reparentFirst || seeds[1].OldParent != reparentSecond || seeds[2].Parent != "" || seeds[2].OldParent != reparentFirst {
		t.Fatalf("the re-parent seeds after the move back and the unlink = %#v, want a seed of each, naming the parent it left", seeds)
	}

	seedIssue(t, pool, record.Issue{Key: "LEGION-501", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: reparentFirst, Parent: new(reparentFirst), Project: "LEGION", Title: "parked", Phase: phase.Implementing, Generation: 1,
		Status: "backlog", DispatchStatus: "backlog", Rank: "Y", LastDispatchSeq: 4})
	applyMirrored(t, pool, engine, "parked-moved", intake.DispatchIssue{Key: "LEGION-501", ResolvedRepo: "sjawhar/legion", Seq: 5,
		Type: "issue.updated", Status: "backlog", Title: "parked", Parent: reparentSecond, Rank: "Y"})
	if got := reparentSeeds(t, pool, "LEGION-501"); len(got) != 0 {
		t.Fatalf("the re-parent seeds of an issue on the backlog = %#v, want none: it is not in Legion's work", got)
	}
}

// A root at admitted is its architect's to work (phase.RoleFor), so the seed of its move under
// another parent carries its architect's phase entry: the entry numbers of an admitted root are
// the architect's, and the row another role holds on the issue, here a planner's, is not what the
// seed names.
func TestAReparentSeedOfARootAtAdmittedCarriesItsArchitectsEntry(t *testing.T) {
	pool := migratedPool(t)
	seedLiveRoot(t, pool, reparentSecond, 1)
	const admitted = "LEGION-600"
	seedIssue(t, pool, record.Issue{Key: admitted, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: admitted, Project: "LEGION", Title: "the admitted root", Phase: phase.Admitted, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "X", LastDispatchSeq: 4})
	seedSlot(t, pool, record.Slot{Issue: admitted, Index: 2, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	raised := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	seedPhase(t, pool, record.PhaseRow{Issue: admitted, Role: claim.RoleArchitect, Claim: "architect-claim",
		Entry: 3, EntryTask: "write the spec", EntryRaisedAt: raised})
	seedPhase(t, pool, record.PhaseRow{Issue: admitted, Role: claim.RolePlanner, Claim: "planner-claim",
		Entry: 7, EntryTask: "plan it", EntryRaisedAt: raised})
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "admitted-moved-under-300", intake.DispatchIssue{Key: admitted, ResolvedRepo: "sjawhar/legion", Seq: 5,
		Type: "issue.updated", Status: "in_progress", Title: "the admitted root", Parent: reparentSecond, Rank: "X"})
	seeds := reparentSeeds(t, pool, admitted)
	if len(seeds) != 1 || seeds[0].Phase != phase.Admitted || seeds[0].Entry != 3 {
		t.Fatalf("the re-parent seeds of %s = %#v, want one at admitted carrying entry 3, its architect's", admitted, seeds)
	}
}

// The seed's read of the new parent comes back as a fact (intake.ReparentRead), which tells each
// distinct architect among the issue's own, the one above it before and the one above it now,
// once, each by a reparented notice that names it (R18 of dispatch://INFRA-3320). Every notice
// reads the same words: the old and the new parent, the phase, the slot and the gate steps, which
// stay, and what the architect above it now does. A read of a move the issue has moved on from
// since tells nothing, and a move between two issues under the same architect tells that
// architect once.
func TestTheReparentReadTellsEachDistinctArchitectOnce(t *testing.T) {
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	engine := testEngine(config.DesignGateRootIssues, nil)
	moveParent(t, pool, engine, "moved-under-300", reparentSecond, 5)
	seed := reparentSeeds(t, pool, reparentMoved)[0]

	applyMirrored(t, pool, engine, "read", intake.ReparentRead{Issue: reparentMoved, Seed: seed})
	told := reparentedNotices(t, pool, reparentMoved)
	if got := slices.Sorted(slices.Values(toldTo(told))); !slices.Equal(got, []string{reparentFirst, reparentSecond, reparentMoved}) {
		t.Fatalf("the reparented notices go to %v, want one each to %s, %s and %s", got, reparentFirst, reparentSecond, reparentMoved)
	}
	words := told[0].Reason
	for _, want := range []string{"changed from LEGION-208 to LEGION-300", "phase implementing", "holding its admission slot", "spec open",
		"The architect of LEGION-300 now stands above it", "park_child"} {
		if !strings.Contains(words, want) {
			t.Errorf("the words of the change %q do not say %q", words, want)
		}
	}
	for _, notice := range told {
		if notice.Reason != words {
			t.Errorf("the notice to %s reads %q, want the same words as the others", notice.To, notice.Reason)
		}
	}

	// The read again is the same fact; a read of a seed whose parent the issue is no longer told
	// under tells nothing.
	applyMirrored(t, pool, engine, "read", intake.ReparentRead{Issue: reparentMoved, Seed: seed})
	stale := seed
	stale.Parent = reparentFirst
	applyMirrored(t, pool, engine, "stale-read", intake.ReparentRead{Issue: reparentMoved, Seed: stale})
	if again := reparentedNotices(t, pool, reparentMoved); len(again) != 3 {
		t.Fatalf("the reparented notices after the same read and a stale one = %d, want the first three alone", len(again))
	}
}

// A move between two issues under one architect tells that architect once: LEGION-501 and
// LEGION-502 stand under LEGION-208, whose architect is the one above the moved issue before the
// move and after it, and neither has an architect of its own.
func TestAMoveUnderTheSameArchitectTellsItOnce(t *testing.T) {
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	for _, key := range []string{"LEGION-501", "LEGION-502"} {
		seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: reparentFirst, Parent: new(reparentFirst), Project: "LEGION", Title: key, Phase: phase.Planning, Generation: 1,
			Status: "in_progress", Rank: "Y"})
	}
	engine := testEngine(config.DesignGateRootIssues, nil)
	moveParent(t, pool, engine, "under-501", "LEGION-501", 5)
	moveParent(t, pool, engine, "under-502", "LEGION-502", 6)
	seeds := reparentSeeds(t, pool, reparentMoved)
	applyMirrored(t, pool, engine, "read", intake.ReparentRead{Issue: reparentMoved, Seed: seeds[1]})
	if got := slices.Sorted(slices.Values(toldTo(reparentedNotices(t, pool, reparentMoved)))); !slices.Equal(got, []string{reparentFirst, reparentMoved}) {
		t.Fatalf("the reparented notices of a move from LEGION-501 to LEGION-502 go to %v, want %s once and %s", got, reparentFirst, reparentMoved)
	}
}

// A seed of INFRA-4728's shape, which a daemon of that release queued for a root that ran when a
// person made it a child of a live tree and left at this release's first boot, names no run. Its
// read is told as R18 says: its old parent is the record's where the record names another, and
// what goes on is read from the record.
func TestAReparentSeedOfTheReleaseBeforeIsToldAsR18Says(t *testing.T) {
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(t.Context(), tx, reparentMoved)
		if err != nil {
			return err
		}
		issue.ToldParent = reparentSecond
		return record.NewStore().PutIssue(t.Context(), tx, *issue)
	})
	mirrorChild(t, pool, reparentMoved, reparentSecond, "in_progress", 9)
	engine := testEngine(config.DesignGateRootIssues, nil)
	applyMirrored(t, pool, engine, "old-read", intake.ReparentRead{Issue: reparentMoved,
		Seed: record.ReparentSeed{Parent: reparentSecond, Tree: reparentSecond, Ran: "it ran as a tree of its own"}})
	told := reparentedNotices(t, pool, reparentMoved)
	if got := slices.Sorted(slices.Values(toldTo(told))); !slices.Equal(got, []string{reparentFirst, reparentSecond, reparentMoved}) {
		t.Fatalf("the reparented notices of the release before's seed go to %v, want %s, %s and %s", got, reparentFirst, reparentSecond, reparentMoved)
	}
	if !strings.Contains(told[0].Reason, "changed from LEGION-208 to LEGION-300") || !strings.Contains(told[0].Reason, "phase implementing") {
		t.Fatalf("the words %q do not name the record's old parent and its phase", told[0].Reason)
	}
}

// seedLiveRoot records a root of a live tree: admitted in its slot, its gate open, planning.
func seedLiveRoot(t *testing.T, pool *pgxpool.Pool, key string, slot int) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U", HoldsGate: true})
	seedGate(t, pool, record.DesignGate{Issue: key, ArtifactID: "artifact-" + key, LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: key, Index: slot, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// reparentedNotices is every reparented notice the daemon has queued on issue, oldest first.
func reparentedNotices(t *testing.T, pool *pgxpool.Pool, issue string) []record.Notice {
	t.Helper()
	var notices []record.Notice
	for _, payload := range outboxStrings(t, pool, "select payload::text from outbox where kind = 'notice' and payload->>'kind' = 'reparented' and issue = $1 order by id", issue) {
		var notice record.Notice
		if err := json.Unmarshal([]byte(payload), &notice); err != nil {
			t.Fatalf("decode a reparented notice of %s: %v", issue, err)
		}
		notices = append(notices, fencetest.Unfenced(notice).(record.Notice))
	}
	return notices
}

// toldTo is the issue each notice names as the architect it tells.
func toldTo(notices []record.Notice) []string {
	to := make([]string, len(notices))
	for i, notice := range notices {
		to[i] = notice.To
	}
	return to
}

// reparentSeeds is every re-parent seed the outbox holds for issue, oldest first.
func reparentSeeds(t *testing.T, pool *pgxpool.Pool, issue string) []record.ReparentSeed {
	t.Helper()
	var seeds []record.ReparentSeed
	for _, payload := range outboxStrings(t, pool, "select payload::text from outbox where kind = 'reparent_seed' and issue = $1 order by id", issue) {
		var seed record.ReparentSeed
		if err := json.Unmarshal([]byte(payload), &seed); err != nil {
			t.Fatalf("decode a re-parent seed of %s: %v", issue, err)
		}
		seeds = append(seeds, seed)
	}
	return seeds
}

func outboxStrings(t *testing.T, pool *pgxpool.Pool, query, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), query, issue)
	if err != nil {
		t.Fatalf("read the outbox of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan an outbox row of %s: %v", issue, err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the outbox of %s: %v", issue, err)
	}
	return got
}

// toldParent is the parent the issue's record says it was last told of.
func toldParent(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var issue *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(t.Context(), tx, key)
		return err
	})
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return issue.ToldParent
}
