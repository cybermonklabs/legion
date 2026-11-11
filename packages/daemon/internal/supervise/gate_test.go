package supervise

// The stale-plugin refresh gate as its three callers see it: the lane holds K refreshes in the
// daemon's wake pacing and no more, a credit given back is a credit another refresh may take, and
// each sweep's correction sets the count from what the sweep re-read without taking back a credit
// granted since the correction before it (gate.go). Nothing here is paced, parked or launched:
// the gate counts, and these tests count with it.

import "testing"

// gateRate is the rate these tests build gates at — eight wakes a minute, the shipped default,
// which wake_test.go's own pacing uses — and so K, the refreshes the lane may hold at once.
const gateRate = 8

// admissions is how many further credits g grants before it refuses one, which is the whole of
// what a caller of the gate can see of the count it holds. It asks at most limit times, so a gate
// that has lost its bound fails the test that calls this rather than looping for ever.
func admissions(t *testing.T, g *RefreshGate, limit int) int {
	t.Helper()
	granted := 0
	for range limit {
		if !g.Admit() {
			return granted
		}
		granted++
	}
	t.Fatalf("the gate granted %d admissions in a row, want it to refuse one inside %d", granted, limit)
	return granted
}

// The lane takes one minute of the pacing and waits for the next: K credits are granted at once,
// the admission after them is refused, and the refused refresh is what the next sweep posts
// instead. A credit given back is granted again straight away, because the refresh it was taken
// for has left the pacing — a wake that ran, or a relaunch that raised none — and the lane's
// share is whole again.
//
// Releases beyond what the gate granted add nothing: the held count stops at zero, so a lane
// whose several exits give one credit back twice cannot talk the gate into more than K.
func TestTheRefreshGateGrantsTheRateAndNoMoreUntilACreditComesBack(t *testing.T) {
	g := NewRefreshGate(gateRate)
	for i := range gateRate {
		if !g.Admit() {
			t.Fatalf("admission %d of %d refused, want every credit of the rate granted at once", i+1, gateRate)
		}
	}
	if g.Admit() {
		t.Fatalf("a %dth admission was granted, want the lane held to %d until a credit comes back", gateRate+1, gateRate)
	}
	g.Release()
	if !g.Admit() {
		t.Fatal("the admission after a release was refused, want the credit the release gave back granted again")
	}
	if g.Admit() {
		t.Fatal("a second admission after one release was granted, want one credit back to grant one refresh")
	}
	for range gateRate + 3 {
		g.Release()
	}
	if granted := admissions(t, g, gateRate+1); granted != gateRate {
		t.Fatalf("a gate released %d times over granted %d admissions, want exactly %d: the held count floors at zero",
			gateRate+3, granted, gateRate)
	}
}

// A daemon that paces nothing is a daemon this gate bounds nothing for: the operator asked for no
// pacing, so the lane behaves as it did before the gate existed, every refresh posted as it is
// found. Nothing is counted either — the gate holds no credits to correct — so a sweep's
// correction over such a gate changes nothing, whatever it re-read.
func TestWithNoPacingTheRefreshGateGrantsEveryRefresh(t *testing.T) {
	for _, rate := range []int{0, -1} {
		g := NewRefreshGate(rate)
		for i := range gateRate * 4 {
			if !g.Admit() {
				t.Fatalf("admission %d at rate %d refused, want the pacing off to grant every refresh", i+1, rate)
			}
		}
		g.Correct(gateRate * 4)
		if g.held != 0 || g.granted != 0 {
			t.Fatalf("at rate %d the gate holds %d and counts %d granted, want neither counted at all", rate, g.held, g.granted)
		}
		for i := range gateRate * 4 {
			if !g.Admit() {
				t.Fatalf("admission %d at rate %d after a correction refused, want the correction to change nothing", i+1, rate)
			}
		}
		g.Release()
		if !g.Admit() {
			t.Fatalf("an admission at rate %d after a release refused, want the pacing off to grant every refresh", rate)
		}
	}
}

// The sweep's correction sets the count from the wakes it re-read, floored at the admissions
// granted since the correction before it. Both halves are the view lagging the decision that
// published it: a refresh admitted a moment before the sweep counted is a wake the count misses,
// and the floor is what keeps the gate from handing that credit out twice; a wake the re-read
// still shows after its credit came back is a credit held one sweep longer, which the floor case
// below pins as the cost it is. A reading above the admissions since is taken as it stands, which
// is how a credit lost to a path that returns none is taken back.
func TestTheRefreshGateCorrectsTheHeldCountFromTheWakesWaiting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// admit is the admissions granted since the previous correction, release the credits
		// given back before it, and waiting what the sweep's re-read reports.
		admit   int
		release int
		waiting int
		held    int
	}{
		{name: "a reading that sees every admitted wake is the count", admit: 5, waiting: 5, held: 5},
		{name: "a reading the published views lag is floored at the admissions since", admit: 5, waiting: 0, held: 5},
		{name: "a credit already given back is held one sweep longer", admit: 5, release: 5, waiting: 0, held: 5},
		{name: "a reading above the admissions since is taken as it stands", admit: 3, waiting: 6, held: 6},
		{name: "a credit lost to a path that returns none is taken back", admit: 0, release: 0, waiting: 2, held: 2},
		{name: "a lane whose wakes have all run is given its whole share again", admit: 0, waiting: 0, held: 0},
		{name: "a gate admitting its whole share keeps it", admit: gateRate, waiting: 2, held: gateRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewRefreshGate(gateRate)
			for i := range tc.admit {
				if !g.Admit() {
					t.Fatalf("admission %d of %d refused before the correction", i+1, tc.admit)
				}
			}
			for range tc.release {
				g.Release()
			}
			g.Correct(tc.waiting)
			if g.held != tc.held {
				t.Fatalf("the gate holds %d after a correction from %d waiting, %d admitted and %d released since, want %d",
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

// The floor is the admissions of one sweep, not of the gate's whole life: the correction resets
// that count where it reads it, so the sweep after a correction is floored at what the lane has
// taken since that correction and at nothing earlier. Without the reset, the second correction
// below would hold five credits for a lane that has one wake waiting, and the lane would lose a
// sweep's worth of refreshes at every sweep.
func TestTheRefreshGateCountsAdmissionsOnlySinceTheLastCorrection(t *testing.T) {
	g := NewRefreshGate(gateRate)
	for i := range 4 {
		if !g.Admit() {
			t.Fatalf("admission %d of 4 refused on a fresh gate of %d", i+1, gateRate)
		}
	}
	g.Correct(4)
	if !g.Admit() {
		t.Fatal("the admission after the first correction was refused, want 4 of the rate held and the rest free")
	}
	for range 5 {
		g.Release()
	}
	g.Correct(0)
	if g.held != 1 {
		t.Fatalf("the gate holds %d after a correction reading nothing with one admission since, want 1: the four before the previous correction are not counted again",
			g.held)
	}
	if granted := admissions(t, g, gateRate+1); granted != gateRate-1 {
		t.Fatalf("the gate granted %d admissions after that correction, want %d", granted, gateRate-1)
	}
}
