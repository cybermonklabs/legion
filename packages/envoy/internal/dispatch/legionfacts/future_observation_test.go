package legionfacts

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
)

// TestAnObservationStampedAfterThePassHoldsNoOtherProjectsFeed: an observation's observed_at
// comes from its daemon's clock, so a daemon whose clock leads Dispatch's writes rows stamped
// after the moment a pass computes at. Such a row is no input of that pass, and it holds back
// that row alone: the observations of every other project, written after it and stamped at the
// pass's moment, move their rows in the same pass. The pass says it held an input back, so the
// loop asks for another soon (runPass), and the board's header reads the held feed in the
// cursors' age (CursorAge), which stands at the last pass that held nothing rather than at the
// pass that held the row. The first pass whose moment is past the row's reads it and moves its
// row, and the age is then 0 again.
//
// The history is two projects with Legion phase work. The first pass reads both whole, so every
// stored row is right before the two observations: one of LEGION, stamped an hour after the
// next pass's moment, as a daemon whose clock leads writes it, and then one of OTHER, stamped
// at that moment, whose id is therefore above the held row's.
func TestAnObservationStampedAfterThePassHoldsNoOtherProjectsFeed(t *testing.T) {
	t.Parallel()
	run := newConsumer(t, 2)
	other := &consumer{
		history: history{
			database: run.database,
			fixture:  legiontest.Seed(t, run.database.Pool, legiontest.Spec{Project: "OTHER", Trees: 2, Now: run.now}),
			now:      run.now,
		},
	}
	run.pass(t)
	settled := run.at

	ahead, behind := run.fixture.Open[0], other.fixture.Open[0]
	run.at = run.at.Add(time.Minute)
	other.at = run.at
	before, beforeOther := run.rows(t)[ahead], other.rows(t)[behind]
	stamped := run.at.Add(time.Hour)
	run.observe(t, ahead, legion.FieldPhase, legion.PhaseTesting, stamped)
	other.observe(t, behind, legion.FieldPhase, legion.PhaseTesting, run.at)
	held, read := observationID(t, run, ahead), observationID(t, other, behind)
	if read <= held {
		t.Fatalf("the observation of %s took the id %d, not above the held observation of %s at %d", behind, read, ahead, held)
	}

	changed, heldBack := passHolding(t, run)
	if !heldBack {
		t.Errorf("the pass at %s met an observation of %s stamped %s and says it held no input back", run.at.UTC(), ahead, stamped.UTC())
	}
	if slices.Contains(changed, ahead) || !sameRow(run.rows(t)[ahead], before) {
		t.Errorf("the pass at %s changed %v and %s's row from an observation stamped %s, after its moment", run.at.UTC(), changed, ahead, stamped.UTC())
	}
	moved := other.rows(t)[behind]
	if !slices.Contains(changed, behind) || sameRow(moved, beforeOther) {
		t.Errorf("the pass at %s changed %v and left %s's row as it was: the observation of %s stamped %s held the observation of %s at its moment",
			run.at.UTC(), changed, behind, ahead, stamped.UTC(), behind)
	}
	want := computedWhole(t, other)[behind]
	want.UpdatedAt = moved.UpdatedAt
	if difference := rowDifference(moved, want); difference != "" {
		t.Errorf("the stored row of %s is not the row a computation of %s whole answers at %s: %s", behind, other.fixture.Project, run.at.UTC(), difference)
	}
	cursors := run.cursors(t)
	if at := cursors[observationsCursor].position; at >= held {
		t.Errorf("the pass that held the observation at %d left the observations cursor at %d, at or past it", held, at)
	}
	if at := cursors[observationsAheadCursor].position; at != read {
		t.Errorf("the pass that read the observation at %d past the held one left the cursor of the rows read ahead at %d", read, at)
	}
	heldAge := cursorAge(t, run)
	if heldAge != run.at.Sub(settled) {
		t.Errorf("after the pass at %s held the observation of %s the facts are %s old, want the %s since the last pass that held nothing, at %s",
			run.at.UTC(), ahead, heldAge, run.at.Sub(settled), settled.UTC())
	}

	run.at = stamped.Add(time.Minute)
	other.at = run.at
	later, heldBack := passHolding(t, run)
	if heldBack {
		t.Errorf("the pass at %s, past the observation of %s stamped %s, says it held an input back", run.at.UTC(), ahead, stamped.UTC())
	}
	stored := run.rows(t)[ahead]
	if !slices.Contains(later, ahead) || sameRow(stored, before) {
		t.Errorf("the pass at %s, past the observation of %s stamped %s, changed %v and left its row as it was", run.at.UTC(), ahead, stamped.UTC(), later)
	}
	want = computedWhole(t, run)[ahead]
	want.UpdatedAt = stored.UpdatedAt
	if difference := rowDifference(stored, want); difference != "" {
		t.Errorf("the stored row of %s is not the row a computation of %s whole answers at %s: %s", ahead, run.fixture.Project, run.at.UTC(), difference)
	}
	heads, cursors := run.feedHeads(t), run.cursors(t)
	for _, name := range []string{observationsCursor, observationsAheadCursor} {
		if at := cursors[name].position; at != heads[name] {
			t.Errorf("after the pass past the held observation the %s cursor stands at %d, want its feed's head %d", name, at, heads[name])
		}
	}
	if age := cursorAge(t, run); age != 0 {
		t.Errorf("after the pass at %s, which held nothing, the facts are %s old, want 0", run.at.UTC(), age)
	}
	t.Logf("an observation of %s (id %d) stamped %s held its own row alone: the pass at %s changed %v, %s (id %d) among them, and read the facts %s old; the pass at %s changed %v and read them 0 old",
		ahead, held, stamped.UTC(), stamped.Add(-time.Hour).UTC(), changed, behind, read, heldAge, run.at.UTC(), later)
}

// passHolding is one pass as of the consumer's clock, with the safe horizon awaited first as the
// rig's pass awaits it, and answers the keys whose stored rows it changed and whether it held an
// input back: a check of a pass that holds an observation newer than its moment reads the second.
func passHolding(t *testing.T, run *consumer) ([]string, bool) {
	t.Helper()
	if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	changed, heldBack, err := pass(t.Context(), run.deps, run.sched)
	if err != nil {
		t.Fatalf("the pass at %s failed: %v", run.at.UTC(), err)
	}
	slices.Sort(changed)
	return changed, heldBack
}

// observationID is the id of the newest observation of the key in the consumer's project.
func observationID(t *testing.T, run *consumer, key string) int64 {
	t.Helper()
	var id int64
	if err := run.database.Pool.QueryRow(t.Context(), `
		select coalesce(max(id), 0) from legion_observations where project = $1 and issue_key = $2
	`, run.fixture.Project, key).Scan(&id); err != nil {
		t.Fatalf("read the id of the newest observation of %s: %v", key, err)
	}
	return id
}

// cursorAge is the cursors' age the board's header reads (CursorAge), at the consumer's clock.
func cursorAge(t *testing.T, run *consumer) time.Duration {
	t.Helper()
	age, has, err := CursorAge(t.Context(), run.database.Pool, run.at)
	if err != nil {
		t.Fatalf("read the cursors' age: %v", err)
	}
	if !has {
		t.Fatal("a database a pass has committed on answers no age")
	}
	return age
}
