package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Tests of R18's telling of a change of parent through admission's own path (the mirror, the
// engine, then admission's record of the observation): R18 of dispatch://INFRA-3320, "When the
// Dispatch parent of an issue in Legion's work changes, the daemon tells, once and on each role
// topic, the issue's own architect, the architect above it before and the architect above it now",
// and "The architect above it now reads the issue's spec against its own requirements, as it reads
// a child's spec ..., and keeps it, or writes done, icebox or backlog on it".

// The issues of these tests: two roots at work, LEGION-208 and LEGION-300, each with its
// architect working, and LEGION-500 implementing in its slot under LEGION-208, its architect and
// implementer idle. LEGION-700 is an issue of which the copy holds no row.
const (
	rstOld    = "LEGION-208"
	rstNew    = "LEGION-300"
	rstChild  = "LEGION-500"
	rstUnread = "LEGION-700"
)

func rstFixture(t *testing.T) (*pgxpool.Pool, *Admission) {
	t.Helper()
	pool := migratedPool(t)
	admission := quiet(t, 4)
	seedStarted(t, pool, rstOld, phase.Planning, nil)
	seedStarted(t, pool, rstNew, phase.Planning, nil)
	seedSlottedInPhase(t, pool, testProject, rstChild, rstChild, phase.Implementing, func(child *record.Issue) {
		child.Tree, child.Parent, child.DispatchStatus, child.LastDispatchSeq = rstOld, record.ParentOf(rstOld), "in_progress", 1
	})
	spSeedPhase(t, pool, record.PhaseRow{Issue: rstChild, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
		EntryTask: "Continue " + rstChild + ".", EntryRaisedAt: fixedNow.Add(-time.Hour)})
	for _, key := range []string{rstOld, rstNew} {
		putWorkerClaim(t, pool, key, key, claim.RoleArchitect, 1, supervise.StateWorking, "")
	}
	putWorkerClaim(t, pool, rstOld, rstChild, claim.RoleArchitect, 1, supervise.StateIdle, "")
	putWorkerClaim(t, pool, rstOld, rstChild, claim.RoleImplementer, 1, supervise.StateIdle, "")
	return pool, admission
}

// rstMove applies Dispatch's event of LEGION-500 at status under parent at seq, a person's.
func rstMove(t *testing.T, pool *pgxpool.Pool, admission *Admission, status, parent string, seq int64) {
	t.Helper()
	fact := srMove(rstChild, status, seq)
	fact.Parent = parent
	apply(t, pool, admission, fmt.Sprintf("rst-move-%d", seq), fact, admission.engine)
}

type rstSeedRow struct {
	id   int64
	seed record.ReparentSeed
}

// rstSeeds is every re-parent seed of LEGION-500 the outbox holds, oldest first.
func rstSeeds(t *testing.T, pool *pgxpool.Pool) []rstSeedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload::text from outbox where kind = 'reparent_seed' and issue = $1 order by id`, rstChild)
	if err != nil {
		t.Fatalf("read the re-parent seeds: %v", err)
	}
	defer rows.Close()
	var seeds []rstSeedRow
	for rows.Next() {
		var row rstSeedRow
		var payload string
		if err := rows.Scan(&row.id, &payload); err != nil {
			t.Fatalf("scan a re-parent seed: %v", err)
		}
		if err := json.Unmarshal([]byte(payload), &row.seed); err != nil {
			t.Fatalf("decode a re-parent seed: %v", err)
		}
		seeds = append(seeds, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the re-parent seeds: %v", err)
	}
	return seeds
}

// rstReadSeeds applies the read of every seed of LEGION-500 past after as the outbox's executor
// applies it (seedReparent: the fact's id is the row's), and answers the last row it read.
func rstReadSeeds(t *testing.T, pool *pgxpool.Pool, admission *Admission, after int64) int64 {
	t.Helper()
	last := after
	for _, row := range rstSeeds(t, pool) {
		if row.id <= after {
			continue
		}
		if _, err := intake.ApplyFact(context.Background(), pool, "outbox", fmt.Sprintf("reparent-seed:%s:%d", rstChild, row.id),
			intake.ReparentRead{Issue: rstChild, Seed: row.seed}, admission.engine, admission); err != nil {
			t.Fatalf("apply the read of seed %d: %v", row.id, err)
		}
		last = row.id
	}
	return last
}

// rstToldTo is the words of every reparented notice of LEGION-500 the outbox holds for to's
// architect.
func rstToldTo(t *testing.T, pool *pgxpool.Pool, to string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'reason', '') from outbox where issue = $1 and kind = 'notice'
		and payload->>'kind' = 'reparented' and payload->>'to' = $2 order by id`, rstChild, to)
	if err != nil {
		t.Fatalf("read the reparented notices to %s: %v", to, err)
	}
	defer rows.Close()
	var told []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan a reparented notice: %v", err)
		}
		told = append(told, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the reparented notices: %v", err)
	}
	return told
}

func rstLogTold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Logf("re-parent seeds of %s: %+v", rstChild, rstSeeds(t, pool))
	for _, to := range []string{rstChild, rstOld, rstNew} {
		t.Logf("reparented notices to %s: %q", to, rstToldTo(t, pool, to))
	}
}

// A control of TestAMoveOntoAnUnknownChainThenAStopAndAReturnTellsTheArchitectAboveNow: a move
// under LEGION-300 while LEGION-500 is in Legion's work is told to LEGION-300's architect once,
// which shows these tests read the notice where the daemon queues it.
func TestAMoveInWorkTellsTheArchitectAboveNow(t *testing.T) {
	t.Parallel()
	pool, admission := rstFixture(t)
	rstMove(t, pool, admission, "in_progress", rstNew, 2)
	rstReadSeeds(t, pool, admission, 0)
	rstLogTold(t, pool)
	if told := rstToldTo(t, pool, rstNew); len(told) != 1 {
		t.Fatalf("the architect of %s was told %d times of the move of %s under it, want once", rstNew, len(told), rstChild)
	}
}

// R18 of dispatch://INFRA-3320 tells the architect above an issue now of a change of its Dispatch
// parent made while the issue is in Legion's work. A person moves LEGION-500 under LEGION-700,
// of which the copy holds no row, so the seed waits (ReparentChainPending); before R16's read of
// the gap lands, the person writes backlog on it.
// Out of work, the seed no longer waits, and the outbox runs it. Then the read of the gap lands
// (LEGION-700 on the backlog under LEGION-300), and the person writes todo on LEGION-500. The
// change of parent happened while the issue was in Legion's work, and the architect above it now,
// LEGION-300's, must be told of it.
func TestAMoveOntoAnUnknownChainThenAStopAndAReturnTellsTheArchitectAboveNow(t *testing.T) {
	t.Parallel()
	pool, admission := rstFixture(t)
	rstMove(t, pool, admission, "in_progress", rstUnread, 2)
	if seeds := rstSeeds(t, pool); len(seeds) != 1 || seeds[0].seed.Parent != rstUnread {
		t.Fatalf("the re-parent seeds of %s = %+v, want one, to %s: the case's premise", rstChild, seeds, rstUnread)
	}
	rstMove(t, pool, admission, "backlog", rstUnread, 3)
	last := rstReadSeeds(t, pool, admission, 0)
	spMirror(t, pool, rstUnread, "backlog", rstNew, 9)
	rstMove(t, pool, admission, "todo", rstUnread, 4)
	back := issue(t, pool, rstChild)
	if !record.InLegionsWork(back) || back.Phase != phase.Implementing || back.Generation != 1 {
		t.Fatalf("%s after the todo = %s/%s generation %d, want it back in implementing at generation 1: the case's premise", rstChild, back.Phase, back.Status, back.Generation)
	}
	rstReadSeeds(t, pool, admission, last)
	rstLogTold(t, pool)
	if told := rstToldTo(t, pool, rstNew); len(told) == 0 {
		t.Errorf("%s was moved under %s while in Legion's work, and the architect of %s, above it now that the chain is known, was never told of the change of parent",
			rstChild, rstUnread, rstNew)
	}
}

// The other control of TestAMoveOntoAnUnknownChainThenAStopAndAReturnTellsTheArchitectAboveNow:
// the same move onto LEGION-700, whose row the copy lacks, with no stop. The seed waits while the
// chain is unknown (its read queues it again); R16's read of the gap lands (LEGION-700 on the
// backlog under LEGION-300) while LEGION-500 is still in work, and the seed's read tells
// LEGION-300's architect, above it now. Only the stop differs.
func TestAMoveOntoAnUnknownChainKnownWhileInWorkTellsTheArchitectAboveNow(t *testing.T) {
	t.Parallel()
	pool, admission := rstFixture(t)
	rstMove(t, pool, admission, "in_progress", rstUnread, 2)
	last := rstReadSeeds(t, pool, admission, 0)
	if told := rstToldTo(t, pool, rstOld); len(told) != 0 {
		t.Logf("told %s while the chain was unknown: %q", rstOld, told)
	}
	spMirror(t, pool, rstUnread, "backlog", rstNew, 9)
	rstReadSeeds(t, pool, admission, last)
	rstLogTold(t, pool)
	if told := rstToldTo(t, pool, rstNew); len(told) != 1 {
		t.Errorf("the architect of %s, above %s now that the chain is known, was told %d times, want once", rstNew, rstChild, len(told))
	}
}
