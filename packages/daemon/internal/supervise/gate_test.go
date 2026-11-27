package supervise

import "testing"

const gateRate = 8

// grant is every credit the gate grants in a row, up to limit, before it refuses one. The credits
// it grants stay out. A gate that grants limit credits without refusing one fails the test rather
// than granting for ever.
func grant(t *testing.T, g *RefreshGate, limit int) []*RefreshCredit {
	t.Helper()
	var credits []*RefreshCredit
	for range limit {
		c := g.Admit()
		if c == nil {
			return credits
		}
		credits = append(credits, c)
	}
	t.Fatalf("the gate granted %d admissions in a row, want it to refuse one inside %d asks", len(credits), limit)
	return nil
}

// admissions is how many credits the gate grants in a row, up to limit, before it refuses one.
// The credits it grants stay out.
func admissions(t *testing.T, g *RefreshGate, limit int) int {
	t.Helper()
	return len(grant(t, g, limit))
}

// admit is one admission the test needs granted.
func admit(t *testing.T, g *RefreshGate) *RefreshCredit {
	t.Helper()
	c := g.Admit()
	if c == nil {
		t.Fatal("the gate refused an admission")
	}
	return c
}

func TestTheRefreshGateGrantsTheRateAndNoMoreUntilACreditComesBack(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, gateRate)
	for i := range gateRate {
		credits[i] = admit(t, g)
	}
	if g.Admit() != nil {
		t.Fatalf("a %dth admission was granted, want the lane held to %d", gateRate+1, gateRate)
	}
	credits[0].Release()
	c := admit(t, g)
	if g.Admit() != nil {
		t.Fatal("a second admission after one release was granted")
	}
	c.Release()
	for _, c := range credits[1:] {
		c.Release()
	}
	// A credit given back again gives nothing back: Release is idempotent.
	credits[0].Release()
	if granted := admissions(t, g, gateRate+1); granted != gateRate {
		t.Fatalf("got %d admissions after every credit came back, want %d", granted, gateRate)
	}
}

func TestWithNoPacingTheRefreshGateGrantsEveryRefresh(t *testing.T) {
	for _, rate := range []int{0, -1} {
		g := NewRefreshGate(rate)
		for i := range gateRate * 4 {
			if g.Admit() == nil {
				t.Fatalf("admission %d at rate %d refused", i+1, rate)
			}
		}
		g.Correct(gateRate * 4)
		if g.held != 0 || len(g.out) != 0 {
			t.Fatalf("at rate %d the gate holds %d with %d credits out, want neither counted", rate, g.held, len(g.out))
		}
		for i := range gateRate * 4 {
			if g.Admit() == nil {
				t.Fatalf("admission %d at rate %d after a correction refused", i+1, rate)
			}
		}
		g.Admit().Release()
		if g.Admit() == nil {
			t.Fatalf("an admission at rate %d after a release refused", rate)
		}
	}
}

// The sweep's correction sets the count to the refresh wakes it re-read waiting or to the credits
// out, whichever is more: a re-read misses the refresh posted a moment before it and the one whose
// turn is running, so it never lowers the count below the credits out, and a re-read above them
// counts the lane's wakes with no admission behind them.
func TestTheRefreshGateCorrectsTheHeldCountFromTheWakesWaiting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// admit is the credits granted, release how many of them came back, and waiting what the
		// sweep's re-read reports.
		admit   int
		release int
		waiting int
		held    int
	}{
		{name: "a reading that sees every credit's wake is the count", admit: 5, waiting: 5, held: 5},
		{name: "a reading that sees none of the credits out is floored at them", admit: 5, waiting: 0, held: 5},
		{name: "a credit given back is no floor", admit: 5, release: 5, waiting: 0, held: 0},
		{name: "the floor is the credits still out", admit: 5, release: 2, waiting: 0, held: 3},
		{name: "a reading above the credits out is taken as it stands", admit: 3, waiting: 6, held: 6},
		{name: "a wake with no admission behind it is counted", admit: 0, waiting: 2, held: 2},
		{name: "a lane whose credits are all back is given its whole share again", admit: 0, waiting: 0, held: 0},
		{name: "a gate whose whole share is out keeps it", admit: gateRate, waiting: 2, held: gateRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewRefreshGate(gateRate)
			credits := make([]*RefreshCredit, tc.admit)
			for i := range tc.admit {
				credits[i] = admit(t, g)
			}
			for i := range tc.release {
				credits[i].Release()
			}
			g.Correct(tc.waiting)
			if g.held != tc.held {
				t.Fatalf("the gate holds %d after a correction from %d waiting with %d granted and %d back, want %d",
					g.held, tc.waiting, tc.admit, tc.release, tc.held)
			}
			want := max(gateRate-tc.held, 0)
			if granted := admissions(t, g, gateRate+1); granted != want {
				t.Fatalf("the gate granted %d admissions after that correction, want %d: %d of %d credits held",
					granted, want, tc.held, gateRate)
			}
		})
	}
}

// The floor is the credits still out, not every credit of the gate's life: a sweep after every
// credit of the last batch came back sees no floor, and counts only what was taken since.
func TestTheRefreshGateCountsOnlyTheCreditsStillOut(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, 4)
	for i := range 4 {
		credits[i] = admit(t, g)
	}
	g.Correct(4)
	for _, c := range credits {
		c.Release()
	}
	admit(t, g)
	g.Correct(0)
	if g.held != 1 {
		t.Fatalf("the gate holds %d after a correction reading nothing with one credit out, want 1: the four given back are no floor", g.held)
	}
	if granted := admissions(t, g, gateRate+1); granted != gateRate-1 {
		t.Fatalf("the gate granted %d admissions after that correction, want %d", granted, gateRate-1)
	}
}

// A lane whose batch came back is given its whole share at every sweep: the rate, every minute.
func TestTheRefreshGateGivesItsWholeShareBackAtTheNextSweep(t *testing.T) {
	g := NewRefreshGate(gateRate)
	for sweep := 1; sweep <= 3; sweep++ {
		g.Correct(0)
		credits := grant(t, g, gateRate+1)
		if len(credits) != gateRate {
			t.Fatalf("the gate granted %d admissions at sweep %d, want the rate, %d", len(credits), sweep, gateRate)
		}
		for _, c := range credits {
			c.Release()
		}
	}
}

// Two corrections in one window — the sweep's ticker buffers a tick while a sweep runs long, and
// the next sweep runs at once — keep the credits out as the floor of both. A floor counted from
// the admissions since the previous correction is zeroed by the first and lost by the second,
// which then admits on top of the batch the first sweep left out.
func TestTwoCorrectionsInOneWindowKeepTheCreditsOutAsTheFloor(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, gateRate)
	for i := range gateRate {
		credits[i] = admit(t, g)
	}
	g.Correct(1)
	if got := admissions(t, g, gateRate+1); got != 0 {
		t.Fatalf("the gate granted %d after a correction reading one wake with %d credits out, want 0", got, gateRate)
	}
	credits[1].Release()
	credits[2].Release()
	g.Correct(1)
	if got := admissions(t, g, gateRate+1); got != 2 {
		t.Fatalf("the gate granted %d after a second correction reading one wake with %d credits out, want 2", got, gateRate-2)
	}
}

// A credit whose turn is running is out until that turn's decision ends, though no view shows its
// wake any longer: the turn took the wake off the machine, and the credit goes back only at the end
// of the decision (endRefreshLane). A correction in that window reads no wake for it and must still
// count it, or the gate grants one more than its share while the turn runs.
func TestACreditWhoseTurnIsRunningStaysCountedThroughACorrection(t *testing.T) {
	g := NewRefreshGate(gateRate)
	running := admit(t, g)
	g.Correct(0)
	if got := admissions(t, g, gateRate+1); got != gateRate-1 {
		t.Fatalf("the gate granted %d with one credit's turn running and no wake of it waiting, want %d", got, gateRate-1)
	}
	running.Release()
}

// A credit the daemon's pass gives back because its post went nowhere lowers the count by one: the
// claim is no longer supervised, so no machine will ever end a decision holding that credit.
func TestADroppedPostsCreditIsReleasedByTheCaller(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, gateRate)
	for i := range gateRate {
		credits[i] = admit(t, g)
	}
	credits[3].Release()
	g.Correct(gateRate - 1)
	if g.held != gateRate-1 {
		t.Fatalf("the gate holds %d after the dropped post's credit came back, want %d", g.held, gateRate-1)
	}
	if got := admissions(t, g, 2); got != 1 {
		t.Fatalf("the gate granted %d after a dropped post, want 1: the credit it gave back is free again", got)
	}
}

// A credit given back twice gives back one: every path that ends a credit's life may release it,
// and a second release takes nothing more off the count, before a correction or after it.
func TestACreditReleasedTwiceComesBackOnce(t *testing.T) {
	g := NewRefreshGate(gateRate)
	kept := admit(t, g)
	c := admit(t, g)
	c.Release()
	c.Release()
	if g.held != 1 || len(g.out) != 1 {
		t.Fatalf("the gate holds %d with %d out after one credit released twice and one kept, want 1 and 1", g.held, len(g.out))
	}
	g.Correct(0)
	if g.held != 1 || len(g.out) != 1 {
		t.Fatalf("the gate holds %d with %d out after a correction, with one credit released twice and one kept, want 1 and 1", g.held, len(g.out))
	}
	kept.Release()
	if got := admissions(t, g, gateRate+1); got != gateRate {
		t.Fatalf("the gate granted %d once both credits were back, want %d", got, gateRate)
	}
}

// A credit no machine has taken yet is held by the post that carries it, which the decision of
// that post or the stop's close of the inbox it waits in gives back (the gate's rule of return),
// so no correction takes it back however many sweeps the post waits: a post a machine has not yet
// handled keeps its credit, as the lane's bound requires. The gate has no machine to ask about it,
// and asks none.
//
// The negative control is a correction that takes back every credit granted before the previous
// one whose machine it cannot ask: the post's credit is then gone at the second correction, and
// the gate grants its whole share while the post still waits to be decided.
func TestACreditStillInItsPostIsKeptByEveryCorrection(t *testing.T) {
	g := NewRefreshGate(gateRate)
	posted := admit(t, g)
	for sweep := 1; sweep <= 3; sweep++ {
		g.Correct(0)
		if g.held != 1 || len(g.out) != 1 {
			t.Fatalf("the gate holds %d with %d out after correction %d over a credit still in its post, want 1 and 1",
				g.held, len(g.out), sweep)
		}
	}
	posted.Release()
	if got := admissions(t, g, gateRate+1); got != gateRate {
		t.Fatalf("the gate granted %d once the post's credit was back, want %d", got, gateRate)
	}
}
