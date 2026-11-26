package replay

import (
	"context"
	"testing"
	"time"
)

// A replay admits under R10's P0 bound in force at each pass, as the window records its changes
// (Window.P0Bounds, each applied as the daemon's own change of the bound), and makes a pass at each
// change, as the daemon does, so a raise of the bound starts at its own moment the P0 issue it lets
// in. The window runs a cap of 1 that
// ACME-1, with no priority, holds from 09:00, under a bound of 1 from 09:00. ACME-2, at P0, is filed
// at 09:05 and takes its slot outside the cap; ACME-3, at P0, is filed at 09:10 and waits at the
// bound, which is raised to 2 at 09:30, where the record starts it. No other fact falls after 09:10,
// and the window records no tick, so only the pass of the raise starts ACME-3. Under p0-at-once
// ACME-2 takes its slot at 09:05 and ACME-3 at 09:30; under before-p0, which starts no P0 issue
// outside the cap, neither takes one while ACME-1 holds the cap.
func TestARaiseOfTheP0BoundStartsAP0IssueAtItsOwnPass(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(9, 0), clock(11, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.window.P0Bounds = []BoundAt{{At: clock(9, 0), Bound: 1}, {At: clock(9, 30), Bound: 2}}
	level := func(n int) *int { return &n }
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 0), "ACME-1")
	b.file(clock(9, 5), "ACME-2", filing{Title: "Fix the broken checkout", Status: "todo", Rank: "a1", Labels: handedToLegion, Priority: level(0)})
	b.rootPromoted(clock(9, 5), "ACME-2")
	b.file(clock(9, 10), "ACME-3", filing{Title: "Restore the price feed", Status: "todo", Rank: "a2", Labels: handedToLegion, Priority: level(0)})
	b.rootPromoted(clock(9, 30), "ACME-3")
	window := b.build()
	for _, run := range []struct {
		rule string
		want map[string]time.Time
	}{
		{"p0-at-once", map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 5), "ACME-3": clock(9, 30)}},
		{"before-p0", map[string]time.Time{"ACME-1": clock(9, 0)}},
	} {
		t.Run(run.rule, func(t *testing.T) {
			t.Parallel()
			result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, run.rule), Branch: MainBranch, Seed: 1,
				Slowdown: 1, PGDSN: replayDSN(t), Out: t.TempDir()})
			if err != nil {
				t.Fatalf("replay the window under %s: %v", run.rule, err)
			}
			got := result.Scorecard.FirstSlot
			for _, key := range []string{"ACME-1", "ACME-2", "ACME-3"} {
				first, took := got[key]
				want, wants := run.want[key]
				if took != wants || (took && !first.Equal(want)) {
					t.Errorf("under %s %s takes its first slot at %s (%t), want at %s (%t)", run.rule, key, stamp(first), took, stamp(want), wants)
				}
			}
		})
	}
}
