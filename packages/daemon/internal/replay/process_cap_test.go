package replay

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A daemon process serves from its first line under the cap its own lines name, though the window
// dates a cap change by the first line naming it, which the process writes minutes into its boot
// (collect's caps). The window runs a cap of 1 that ACME-1 holds from 09:00, with ACME-2 waiting
// behind it; the daemon restarts at 10:00, and the new process's first slot line, at 10:03, names a
// cap of 2 and gives ACME-2 the second slot. Replayed under main, the record's rule, ACME-2 takes its
// slot in the restart's own boot, between 10:00 and 10:04. Read at the process's start alone, the cap
// was the old 1 for the whole process, and ACME-2 took no slot before the window's end: in window
// one every raise of the cap was served one restart late, and the cap backtest gave out 35 slots
// where the record gave 49.
func TestABootServesUnderTheCapItsOwnLinesName(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 0), "ACME-1")
	b.file(clock(9, 2), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.turnEnds(clock(9, 30), "ACME-1", claim.RoleArchitect)
	b.stopped(clock(9, 58))
	b.boot(clock(10, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(10, 3), Cap: 2})
	b.rootPromoted(clock(10, 3), "ACME-2")
	window := b.build()
	if capOf := capAt(window, clock(10, 0)); capOf != 1 {
		t.Fatalf("the window's cap at the restart's start is %d, want 1: the premise is a change dated after the process began", capOf)
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir()})
	if err != nil {
		t.Fatalf("replay the window: %v", err)
	}
	first, ok := result.Scorecard.FirstSlot["ACME-2"]
	if !ok || first.Before(clock(10, 0)) || !first.Before(clock(10, 4)) {
		t.Errorf("ACME-2's first slot is %s, want at or after 10:00 and before 10:04: the restart serves the cap of 2 its lines name",
			stamp(first))
	}
}

// capOfProcess reads a process's cap from the first change its own lines name, and from nothing
// outside the process: a change before its start, one after the line saying it finished its boot,
// and one at or after the next process's start each leave it the cap that stood at its start.
func TestTheCapOfAProcessIsTheFirstChangeItsOwnLinesName(t *testing.T) {
	t.Parallel()
	w := Window{Caps: []CapAt{{At: clock(8, 0), Cap: 10}, {At: clock(10, 3), Cap: 20}, {At: clock(11, 0), Cap: 30}}}
	for _, c := range []struct {
		name                 string
		from, started, until time.Time
		want                 int
	}{
		{"a change in the boot", clock(10, 0), clock(10, 4), clock(11, 0), 20},
		{"a change at the boot's started line", clock(10, 0), clock(10, 3), time.Time{}, 20},
		{"a change after the boot's started line", clock(10, 0), clock(10, 2), time.Time{}, 10},
		{"no started line, a change before the next process", clock(10, 0), time.Time{}, clock(10, 30), 20},
		{"no started line, a change at the next process", clock(10, 0), time.Time{}, clock(10, 3), 10},
		{"a change before the process", clock(9, 0), time.Time{}, clock(10, 0), 10},
	} {
		if got := capOfProcess(w, c.from, c.started, c.until); got != c.want {
			t.Errorf("%s: the cap is %d, want %d", c.name, got, c.want)
		}
	}
}
