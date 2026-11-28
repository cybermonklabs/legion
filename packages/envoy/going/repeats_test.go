package going_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// outcomes is a run of repeats over cases, each chose right except where wrong says otherwise.
func outcomes(repeats, cases int, wrong func(repeat, c int) bool) [][]bool {
	run := make([][]bool, repeats)
	for r := range run {
		run[r] = make([]bool, cases)
		for c := range run[r] {
			run[r][c] = !wrong(r, c)
		}
	}
	return run
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-4 }

// A known discordant table gives its known exact p-value: with one pair the first model alone
// chose right and nine the second alone did, the two-sided tail is 2(1+10)/2^10.
func TestThePairedTestGivesMcNemarsExactPValue(t *testing.T) {
	for _, tc := range []struct {
		name          string
		better, worse int
		p             float64
	}{
		{"one against nine", 1, 9, 22.0 / 1024},
		{"two against eight", 2, 8, 112.0 / 1024},
		{"ten against none", 10, 0, 2.0 / 1024},
		{"five against five", 5, 5, 1},
		{"no discordant pair", 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One repeat of 20 cases: the first model misses the worse cases, the second the better
			// ones, and both miss case 19, a concordant pair the test sets aside.
			a := outcomes(1, 20, func(_, c int) bool { return c < tc.worse || c == 19 })
			b := outcomes(1, 20, func(_, c int) bool { return (c >= tc.worse && c < tc.worse+tc.better) || c == 19 })
			test := going.ComparePaired(a, b)
			if test.Pairs != 20 || test.Better != tc.better || test.Worse != tc.worse {
				t.Fatalf("pairs %d, better %d, worse %d; want 20, %d and %d", test.Pairs, test.Better, test.Worse, tc.better, tc.worse)
			}
			if test.PValue != tc.p {
				t.Errorf("p = %v, want %v", test.PValue, tc.p)
			}
			if test.Real != (tc.p < going.PairedAlpha) {
				t.Errorf("real %t at p = %v", test.Real, test.PValue)
			}
			if test.Interval != nil {
				t.Errorf("one repeat gives the interval %v, want none", *test.Interval)
			}
		})
	}
}

// The mean paired difference is the per-repeat difference in right choices averaged over the
// repeats, and its 95% interval is Student's t over them: differences 1 to 5 over five repeats
// have mean 3, standard deviation √2.5 and t(0.975, 4) = 2.776445.
func TestTheMeanDifferenceCarriesItsStudentInterval(t *testing.T) {
	a := outcomes(5, 6, func(int, int) bool { return false })
	b := outcomes(5, 6, func(r, c int) bool { return c <= r })
	test := going.ComparePaired(a, b)
	if test.Better != 15 || test.Worse != 0 || test.MeanDifference != 3 {
		t.Fatalf("better %d, worse %d, mean %v; want 15, 0 and 3", test.Better, test.Worse, test.MeanDifference)
	}
	half := 2.776445 * math.Sqrt(2.5) / math.Sqrt(5)
	if test.Interval == nil || !near(test.Interval[0], 3-half) || !near(test.Interval[1], 3+half) {
		t.Fatalf("interval %v, want %v to %v", test.Interval, 3-half, 3+half)
	}
	// The same difference in every repeat has no spread, so its interval is that difference.
	steady := going.ComparePaired(outcomes(10, 6, func(int, int) bool { return false }), outcomes(10, 6, func(_, c int) bool { return c == 0 }))
	if steady.Interval == nil || steady.Interval[0] != 1 || steady.Interval[1] != 1 {
		t.Fatalf("one case apart in every repeat: interval %v, want 1 to 1", steady.Interval)
	}
	// Two repeats leave one degree of freedom, where t(0.975, 1) = 12.706205: differences 0 and
	// 2 have mean 1 and standard deviation √2, so the half-width is 12.706205.
	two := going.ComparePaired(outcomes(2, 6, func(int, int) bool { return false }), outcomes(2, 6, func(r, c int) bool { return r == 1 && c < 2 }))
	if two.Interval == nil || !near(two.Interval[0], 1-12.706205) || !near(two.Interval[1], 1+12.706205) {
		t.Fatalf("two repeats: interval %v, want %v to %v", two.Interval, 1-12.706205, 1+12.706205)
	}
}

// One case apart in one run is noise; the same case apart in each of ten repeats is real.
func TestOneCaseApartIsRealOnlyWhereTheRepeatsBearItOut(t *testing.T) {
	missesOne := func(_, c int) bool { return c == 0 }
	never := func(int, int) bool { return false }
	if once := going.ComparePaired(outcomes(1, 117, never), outcomes(1, 117, missesOne)); once.Real || once.PValue != 1 {
		t.Errorf("one case apart in one run: real %t at p = %v, want noise at 1", once.Real, once.PValue)
	}
	ten := going.ComparePaired(outcomes(10, 117, never), outcomes(10, 117, missesOne))
	if !ten.Real || ten.PValue != 2.0/1024 {
		t.Errorf("one case apart in ten repeats: real %t at p = %v, want real at %v", ten.Real, ten.PValue, 2.0/1024)
	}
}

// Two models that answer alike, each case right with the same odds in every call, give noise:
// over many runs the test calls at most about one in twenty real, its level, and the interval
// holds zero as often.
func TestEqualModelsGiveNoise(t *testing.T) {
	const runs = 400
	calledReal, coverZero := 0, 0
	for seed := range uint64(runs) {
		r := rand.New(rand.NewPCG(seed, 1588))
		odds := make([]float64, 117)
		for c := range odds {
			odds[c] = 0.85 + 0.15*r.Float64()
		}
		draw := func(_, c int) bool { return r.Float64() >= odds[c] }
		test := going.ComparePaired(outcomes(20, 117, draw), outcomes(20, 117, draw))
		if test.Real {
			calledReal++
		}
		if test.Interval[0] <= 0 && test.Interval[1] >= 0 {
			coverZero++
		}
	}
	if share := float64(calledReal) / runs; share > 0.075 {
		t.Errorf("equal models are called real in %.1f%% of runs, want about 5%% at most", share*100)
	}
	if share := float64(coverZero) / runs; share < 0.92 {
		t.Errorf("the 95%% interval of equal models holds zero in %.1f%% of runs, want about 95%%", share*100)
	}
}

// A model that misses two cases in ten where the other misses one in a hundred is told apart in
// every run of ten repeats, and its interval lies wholly on its side of zero.
func TestAClearDifferenceGivesReal(t *testing.T) {
	for seed := range uint64(100) {
		r := rand.New(rand.NewPCG(seed, 3924))
		test := going.ComparePaired(
			outcomes(10, 117, func(int, int) bool { return r.Float64() < 0.01 }),
			outcomes(10, 117, func(int, int) bool { return r.Float64() < 0.2 }),
		)
		if !test.Real || test.Better <= test.Worse || test.Interval[0] <= 0 {
			t.Fatalf("seed %d: real %t, better %d, worse %d, p %v, interval %v; want real in the first model's favour",
				seed, test.Real, test.Better, test.Worse, test.PValue, *test.Interval)
		}
		swapped := going.ComparePaired(
			outcomes(10, 117, func(int, int) bool { return r.Float64() < 0.2 }),
			outcomes(10, 117, func(int, int) bool { return r.Float64() < 0.01 }),
		)
		if !swapped.Real || swapped.MeanDifference >= 0 || swapped.Interval[1] >= 0 {
			t.Fatalf("seed %d swapped: real %t, mean %v, interval %v; want real against the first model", seed, swapped.Real, swapped.MeanDifference, *swapped.Interval)
		}
	}
}

// The spread of the totals is their mean, least, greatest and sample standard deviation, which is
// zero for one repeat.
func TestTheSpreadOfTheTotals(t *testing.T) {
	s := going.SpreadOf([]int{115, 116, 117, 116})
	if s.Mean != 116 || s.Min != 115 || s.Max != 117 || !near(s.SD, math.Sqrt(2.0/3)) {
		t.Errorf("spread %+v, want mean 116, 115 to 117, sd %v", s, math.Sqrt(2.0/3))
	}
	if one := going.SpreadOf([]int{116}); one != (going.Spread{Mean: 116, Min: 116, Max: 116}) {
		t.Errorf("one repeat: spread %+v, want 116 with no deviation", one)
	}
}
