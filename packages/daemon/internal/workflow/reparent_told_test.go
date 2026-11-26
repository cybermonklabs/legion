package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// workingArchitectClaim records a working architect claim of key, as seedReparentFixture records
// the fixture's three.
func workingArchitectClaim(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
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

// reparentedCount is how many reparented notices of issue the outbox holds for each architect.
func reparentedCount(t *testing.T, pool *pgxpool.Pool, issue string) map[string]int {
	t.Helper()
	count := map[string]int{}
	for _, notice := range noticesOfKind(t, pool, issue, "reparented") {
		count[notice.To]++
	}
	return count
}

// R18 of dispatch://INFRA-3320: "When the Dispatch parent of an issue in Legion's work changes,
// the daemon tells, once and on each role topic, the issue's own architect, the architect above
// it before and the architect above it now." LEGION-500, at work under LEGION-208, is moved under
// LEGION-300 and then under LEGION-400 before the outbox runs the first move's seed, as when the
// daemon boots with both events in its stream or its outbox runs behind. Each seed is then read in
// order, as the outbox runs them. Two changes of parent happened: LEGION-208's architect, whose
// issue left it, is told at least once, LEGION-300's (above it before the second) at least once,
// LEGION-400's (above it now) once, and the issue's own at least once. The same two moves with the
// first seed read before the second move tell each of them; only when the seed is read differs.
func TestTwoQuickReparentsTellTheArchitectAboveBeforeEachMove(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	const third = "LEGION-400"
	seedLiveRoot(t, pool, third, 3)
	workingArchitectClaim(t, pool, third)
	engine := testEngine(config.DesignGateRootIssues, nil)

	moveParent(t, pool, engine, "moved-under-300", reparentSecond, 5)
	moveParent(t, pool, engine, "moved-under-400", third, 6)
	seeds := reparentSeeds(t, pool, reparentMoved)
	if len(seeds) != 2 || seeds[0].OldParent != reparentFirst || seeds[0].Parent != reparentSecond ||
		seeds[1].OldParent != reparentSecond || seeds[1].Parent != third {
		t.Fatalf("the re-parent seeds of %s = %#v, want one for each move: %s to %s, then %s to %s", reparentMoved, seeds,
			reparentFirst, reparentSecond, reparentSecond, third)
	}
	for i, seed := range seeds {
		applyMirrored(t, pool, engine, fmt.Sprintf("read-%d", i), intake.ReparentRead{Issue: reparentMoved, Seed: seed})
	}
	count := reparentedCount(t, pool, reparentMoved)
	if count[reparentFirst] == 0 {
		t.Errorf("reparented notices by architect = %v: the architect of %s, above %s before it moved to %s, is told nothing", count,
			reparentFirst, reparentMoved, reparentSecond)
	}
	if count[reparentSecond] == 0 {
		t.Errorf("reparented notices by architect = %v: the architect of %s, above %s before it moved to %s, is told nothing", count,
			reparentSecond, reparentMoved, third)
	}
	if count[third] != 1 {
		t.Errorf("reparented notices by architect = %v: the architect of %s, above %s now, is told %d times, want once", count,
			third, reparentMoved, count[third])
	}
	if count[reparentMoved] == 0 {
		t.Errorf("reparented notices by architect = %v: the issue's own architect is told nothing", count)
	}
}

// chainKnownAbove is whether key's chain reads known in the copy, and the issues above it in
// Legion's work, as every decision reads them (record.ReadChainsOf).
func chainKnownAbove(t *testing.T, pool *pgxpool.Pool, key string) (bool, []string) {
	t.Helper()
	var known bool
	var above []string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		chains, err := record.ReadChainsOf(t.Context(), tx, record.NewStore(), "LEGION", []string{key})
		if err != nil {
			return err
		}
		known, above = chains.Chain(key).Known(), chains.Above(key)
		return nil
	})
	return known, above
}

// R18 of dispatch://INFRA-3320 tells the architect above an issue now of a change of its Dispatch
// parent, and R16 reads an unknown chain again "until the chain is known". LEGION-500, at work
// under LEGION-208, is moved under LEGION-700, which Dispatch shows on the backlog under
// LEGION-300, a root at work, and of which the copy holds no row yet, so the chain the move's seed
// is read on is unknown. The daemon reads the chain again as rereadChain applies it (the link's
// reading, then the issue's own view), and the chain is known: LEGION-300's architect stands
// above LEGION-500 now. Every seed queued since is read and an admission pass runs. LEGION-300's
// architect has been told of the change of parent, and the architect above it before and its own
// are told once each.
func TestAReparentOntoAnUnknownChainTellsTheArchitectAboveOnceKnown(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedReparentFixture(t, pool)
	const unread = "LEGION-700"
	engine := testEngine(config.DesignGateRootIssues, nil)

	moveParent(t, pool, engine, "moved-under-700", unread, 5)
	seeds := reparentSeeds(t, pool, reparentMoved)
	if len(seeds) != 1 || seeds[0].Parent != unread {
		t.Fatalf("the re-parent seeds of %s = %#v, want one, to %s", reparentMoved, seeds, unread)
	}
	if known, _ := chainKnownAbove(t, pool, reparentMoved); known {
		t.Fatalf("the chain of %s under %s, whose row the copy lacks, reads known at the seed's read", reparentMoved, unread)
	}
	applyMirrored(t, pool, engine, "read-0", intake.ReparentRead{Issue: reparentMoved, Seed: seeds[0]})

	applyMirrored(t, pool, engine, "reread-links", intake.ControllerTick{Observed: []intake.IssueStatus{
		{Key: unread, Project: "LEGION", Status: "backlog", Parent: reparentSecond, Seq: 7, Labels: []string{}, Rank: "Q"},
	}})
	applyMirrored(t, pool, engine, "reread-issue", intake.ObservedIssue(dispatch.Issue{Key: reparentMoved, Status: "in_progress",
		Title: "the moved issue", Parent: new(unread), Rank: "Z", LastSeq: 5, ResolvedRepo: "sjawhar/legion"}))
	for i, seed := range reparentSeeds(t, pool, reparentMoved)[1:] {
		applyMirrored(t, pool, engine, fmt.Sprintf("read-%d", i+1), intake.ReparentRead{Issue: reparentMoved, Seed: seed})
	}
	admissionPass(t, pool, engine)
	if known, above := chainKnownAbove(t, pool, reparentMoved); !known || !slices.Equal(above, []string{reparentSecond}) {
		t.Fatalf("after the re-read the chain of %s reads known %t with %v above it in work, want known with %s above it", reparentMoved, known, above, reparentSecond)
	}

	count := reparentedCount(t, pool, reparentMoved)
	if count[reparentSecond] == 0 {
		t.Errorf("reparented notices by architect = %v: the architect of %s, above %s now that its chain is known, is never told of its change of parent from %s to %s",
			count, reparentSecond, reparentMoved, reparentFirst, unread)
	}
	if count[reparentFirst] != 1 || count[reparentMoved] != 1 {
		t.Errorf("reparented notices by architect = %v, want the architect above it before (%s) and its own told once each", count, reparentFirst)
	}
}
