package legionfacts

import (
	"testing"
	"time"
)

// scaleTrees is the population this file measures the sweep over: four times the 42 trees of the
// facts' own rig, which is 1008 issues and 840 of them with Legion phase work. INFRA holds about
// 252 issues today (the rig's own size, which
// TestTheSweepsPaceHoldsTheFreshnessRequirement measures), so four times it covers INFRA at twice
// its size and leaves room above that: a reader sees how the two costs grow with the population
// rather than one number at one size.
const scaleTrees = 4 * 42

// TestTheSweepsPaceHoldsAtFourTimesThePopulation: the two costs the freshness requirement rests
// on, measured again at four times the rig's population.
//
// The requirement is that a stored row stands no more than Freshness (one minute) behind the
// clock. A row is computed as of the start of the pass that writes it, and the next pass starts
// SweepEvery (30 s) after this one, so a row's age when it is rewritten is SweepEvery plus that
// pass's own length. A pass therefore has Freshness less SweepEvery, 30 s, to run in.
//
// The two numbers are the same two TestTheSweepsPaceHoldsTheFreshnessRequirement takes at 252
// issues: one whole-project compute and write (the backfill's own work), and one sweep pass that
// finds nothing new, which is what the loop does every SweepEvery while nothing moves. The sweep
// pass is the one the bound holds, because it is the pass that runs on the pace.
func TestTheSweepsPaceHoldsAtFourTimesThePopulation(t *testing.T) {
	run := newConsumer(t, scaleTrees)
	project := run.fixture.Project

	started := time.Now()
	rows, err := ComputeProject(t.Context(), run.database.Pool, project, run.at)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	written, err := Write(t.Context(), run.database.Pool, rows)
	if err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	whole := time.Since(started)

	// The pass that reads the three feeds through to their heads, so the sweep pass after it
	// finds nothing new and measures the sweep alone.
	run.pass(t)
	started = time.Now()
	changed := run.pass(t)
	sweep := time.Since(started)

	budget := Freshness - SweepEvery
	if sweep >= budget {
		t.Errorf("at %d issues a sweep pass takes %s, and a pass has Freshness less SweepEvery, %s, to run in",
			len(run.fixture.Keys), sweep.Round(time.Millisecond), budget.Round(time.Millisecond))
	}
	if whole >= budget {
		t.Errorf("at %d issues one ComputeProject and Write pass takes %s, above the %s a pass has to run in",
			len(run.fixture.Keys), whole.Round(time.Millisecond), budget.Round(time.Millisecond))
	}
	t.Logf("over %d issues, %d of them with Legion phase work: one ComputeProject and Write pass took %s and wrote %d rows; one sweep pass that finds nothing new took %s and changed %d rows",
		len(run.fixture.Keys), len(run.fixture.PhaseWork), whole.Round(time.Millisecond), len(written),
		sweep.Round(time.Millisecond), len(changed))
	t.Logf("a stored row therefore stands at most SweepEvery (%s) plus a pass (%s) behind the clock, against the %s Freshness allows: the sweep pass leaves %s of the %s a pass has",
		SweepEvery, sweep.Round(time.Millisecond), Freshness,
		(budget - sweep).Round(time.Millisecond), budget)
}
