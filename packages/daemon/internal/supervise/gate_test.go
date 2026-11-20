package supervise

import "testing"

const gateRate = 8

func admissions(t *testing.T, g *RefreshGate, limit int) int {
	t.Helper()
	granted := 0
	for range limit {
		if g.Admit() == nil {
			return granted
		}
		granted++
	}
	t.Fatalf("the gate granted %d admissions and did not refuse one, want it bounded at %d", granted, limit)
	return 0
}

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
	// Extra releases are harmless.
	credits[0].Release()
	if granted := admissions(t, g, gateRate+1); granted != gateRate {
		t.Fatalf("got %d admissions after all released, want %d", granted, gateRate)
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
		if g.held != 0 || g.unpublished != 0 {
			t.Fatalf("at rate %d the gate holds %d and counts %d unpublished, want neither counted", rate, g.held, g.unpublished)
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

func TestTheRefreshGateCorrectsTheHeldCountFromTheWakesWaiting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		admit   int
		publish int // published then released
		release int // released unpublished
		waiting int
		held    int
	}{
		{name: "all published, all in waiting", admit: 5, publish: 5, waiting: 5, held: 5},
		{name: "unpublished admissions are the floor", admit: 5, waiting: 0, held: 5},
		{name: "published and released is no floor", admit: 5, publish: 5, waiting: 0, held: 0},
		{name: "unpublished release lowers floor", admit: 5, release: 2, waiting: 0, held: 3},
		{name: "a reading above the floor", admit: 3, publish: 3, waiting: 6, held: 6},
		{name: "a credit lost to a path that returns none", admit: 0, waiting: 2, held: 2},
		{name: "nothing held nothing waiting", admit: 0, waiting: 0, held: 0},
		{name: "K admitted, 2 published, rest unpublished", admit: gateRate, publish: 2, waiting: 2, held: gateRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewRefreshGate(gateRate)
			credits := make([]*RefreshCredit, tc.admit)
			for i := range tc.admit {
				credits[i] = admit(t, g)
			}
			for i := range tc.publish {
				credits[i].Publish()
				credits[i].Release()
			}
			for i := range tc.release {
				credits[tc.publish+i].Release() // release unpublished
			}
			g.Correct(tc.waiting)
			if g.held != tc.held {
				t.Fatalf("held %d after Correct(%d), want %d (admit=%d publish=%d release=%d)",
					g.held, tc.waiting, tc.held, tc.admit, tc.publish, tc.release)
			}
			want := max(gateRate-tc.held, 0)
			if granted := admissions(t, g, gateRate+1); granted != want {
				t.Fatalf("got %d free, want %d", granted, want)
			}
		})
	}
}

func TestTheRefreshGateCountsAdmissionsOnlySinceTheLastCorrection(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, 4)
	for i := range 4 {
		credits[i] = admit(t, g)
	}
	for _, c := range credits {
		c.Publish()
	}
	g.Correct(4)
	fifth := admit(t, g)
	g.Correct(0) // only the fifth is unpublished
	if g.held != 1 {
		t.Fatalf("held %d, want 1: the four published credits are in waiting, not the floor", g.held)
	}
	fifth.Release()
	for _, c := range credits {
		c.Release()
	}
}

func TestTheRefreshGateGivesItsWholeShareBackAtTheNextSweep(t *testing.T) {
	g := NewRefreshGate(gateRate)
	for sweep := 1; sweep <= 3; sweep++ {
		g.Correct(0)
		credits := make([]*RefreshCredit, 0, gateRate)
		for {
			c := g.Admit()
			if c == nil {
				break
			}
			credits = append(credits, c)
		}
		if len(credits) != gateRate {
			t.Fatalf("sweep %d: got %d admissions, want %d", sweep, len(credits), gateRate)
		}
		// Each admitted wake is published, then released (its turn ran).
		for _, c := range credits {
			c.Publish()
			c.Release()
		}
	}
}

func TestTwoCorrectionsInOneWindowDoNotLoseTheGrantedFloor(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, gateRate)
	for i := range gateRate {
		credits[i] = admit(t, g)
	}
	credits[0].Publish()
	g.Correct(1)
	if got := admissions(t, g, gateRate+1); got != 0 {
		t.Fatalf("got %d after first Correct(1), want 0: 7 unpublished hold the floor", got)
	}
	credits[1].Publish()
	credits[1].Release()
	credits[2].Publish()
	credits[2].Release()
	g.Correct(1) // 1 still waiting, 5 unpublished: held = 1+5 = 6
	if got := admissions(t, g, gateRate+1); got != 2 {
		t.Fatalf("got %d after second Correct(1), want 2: 1 waiting + 5 unpublished = 6 held", got)
	}
}

func TestAPublishBetweenCorrectionsDropsTheFloorByOne(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, 4)
	for i := range 4 {
		credits[i] = admit(t, g)
	}
	g.Correct(0) // held = 0 + 4 = 4
	if g.held != 4 {
		t.Fatalf("held %d, want 4", g.held)
	}
	credits[0].Publish()
	g.Correct(1) // held = 1 + 3 = 4
	if g.held != 4 {
		t.Fatalf("held %d after 1 published, want 4", g.held)
	}
	credits[1].Publish()
	g.Correct(2) // held = 2 + 2 = 4
	if g.held != 4 {
		t.Fatalf("held %d after 2 published, want 4", g.held)
	}
	for _, c := range credits {
		c.Publish()
		c.Release()
	}
	g.Correct(0)
	if g.held != 0 {
		t.Fatalf("held %d after all returned, want 0", g.held)
	}
}

func TestAReleaseOfAnUnpublishedCreditDropsTheFloor(t *testing.T) {
	g := NewRefreshGate(gateRate)
	credits := make([]*RefreshCredit, 4)
	for i := range 4 {
		credits[i] = admit(t, g)
	}
	credits[0].Release() // unpublished release: held=3, unpublished=3
	g.Correct(0)         // held = 0 + 3 = 3
	if g.held != 3 {
		t.Fatalf("held %d after releasing an unpublished credit, want 3", g.held)
	}
	credits[1].Release() // another unpublished release
	g.Correct(0)         // held = 0 + 2 = 2
	if g.held != 2 {
		t.Fatalf("held %d after two unpublished releases, want 2", g.held)
	}
	credits[2].Publish()
	credits[2].Release()
	credits[3].Publish()
	credits[3].Release()
	g.Correct(0)
	if g.held != 0 {
		t.Fatalf("held %d after all returned, want 0", g.held)
	}
}

func TestACreditReleasedTwiceAndPublishedAfterReleaseChangesNothingMore(t *testing.T) {
	g := NewRefreshGate(gateRate)
	c := admit(t, g)
	c.Release()
	c.Release()  // idempotent
	c.Publish()  // harmless after release
	g.Correct(0) // held = 0 + 0 = 0
	if g.held != 0 {
		t.Fatalf("held %d after double release and publish-after-release, want 0", g.held)
	}
	if got := admissions(t, g, gateRate+1); got != gateRate {
		t.Fatalf("got %d free, want %d", got, gateRate)
	}
}
