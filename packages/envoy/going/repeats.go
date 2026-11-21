package going

// repeats.go holds the statistics of a bake-off run that asks each case more than once: how a
// model's right count spreads over the repeats, and the paired test of two models over the same
// cases and the same repeats, which says how far a difference in their right choices can be
// trusted beyond the noise of the calls themselves. The test is evidence beside R7's choice and
// names no model: Choose ranks by the right choices alone.

import (
	"math"
	"math/big"
)

// PairedAlpha is the paired test's two-sided level, which PairedTest.Real reads the p-value
// against. It is the test's own reading, reported beside the choice, and no choice reads it.
const PairedAlpha = 0.05

// Spread is how a count varied over a run's repeats: its mean, its least and greatest value, and
// its sample standard deviation, zero for one repeat.
type Spread struct {
	Mean float64 `json:"mean"`
	Min  int     `json:"min"`
	Max  int     `json:"max"`
	SD   float64 `json:"sd"`
}

// SpreadOf is the spread of counts, one for each repeat; the zero Spread for none.
func SpreadOf(counts []int) Spread {
	if len(counts) == 0 {
		return Spread{}
	}
	s := Spread{Min: counts[0], Max: counts[0]}
	sum := 0
	for _, c := range counts {
		s.Min, s.Max = min(s.Min, c), max(s.Max, c)
		sum += c
	}
	s.Mean = float64(sum) / float64(len(counts))
	if len(counts) > 1 {
		// Each square is rounded to a float64 before it is added (the explicit conversion), so
		// no compiler fuses the multiply and the add: arm64 does where amd64 does not, and the
		// fused sum differs in the last digit, so a record measured on one architecture would
		// not hold the spread the other computes from the same repeats.
		squares := 0.0
		for _, c := range counts {
			d := float64(c) - s.Mean
			squares += float64(d * d)
		}
		s.SD = math.Sqrt(squares / float64(len(counts)-1))
	}
	return s
}

// PairedTest is one model compared with another over the same cases and the same repeats
// (ComparePaired).
type PairedTest struct {
	// Against is the model this one is compared with.
	Against string `json:"against"`
	// Pairs is the (case, repeat) pairs compared, one for each case in each repeat. Better is
	// the pairs this model chose right and the other wrong, and Worse the pairs the other chose
	// right and this one wrong: the discordant pairs, the only ones that tell the two apart.
	Pairs  int `json:"pairs"`
	Better int `json:"better"`
	Worse  int `json:"worse"`
	// PValue is McNemar's exact two-sided p-value on Better and Worse: the chance that two
	// models answering equally well split their discordant pairs at least this unevenly. Real
	// is whether it is under PairedAlpha. Both say how far a lead can be trusted, and neither
	// names a model.
	PValue float64 `json:"p_value"`
	Real   bool    `json:"real"`
	// MeanDifference is this model's right count less the other's in one repeat, averaged over
	// the repeats, which is (Better-Worse)/repeats, and Interval its two-sided 95% interval by
	// Student's t over the repeats; null for one repeat, which leaves no spread to measure by.
	// The interval rests on a normal approximation that few differing repeats do not bear out,
	// so it is reported beside the verdict and never decides it.
	MeanDifference float64     `json:"mean_difference"`
	Interval       *[2]float64 `json:"interval"`
}

// ComparePaired is the paired test of one model against another: a[r][i] and b[r][i] are
// whether each chose case i right in repeat r, so both hold the same cases in the same repeats.
// Against is the caller's to fill.
//
// The test is McNemar's, exact. The cases are fixed and every call is a draw of its own from its
// model, so each (case, repeat) pair is an independent paired trial: the same words asked of both
// models in the same pass. A pair both chose right, or both wrong, says nothing about which model
// is better, and the test sets them aside. Where neither model is better, each discordant pair is
// as likely to favour one as the other, so Better is binomial over the discordant pairs with even
// odds, and the p-value is that binomial's own tail rather than the chi-squared approximation,
// which is poor at the handful of discordant pairs a run of two close models gives. Where the
// models differ case by case yet tie in all, Better spreads no wider than that binomial, so the
// test leans, if at all, toward calling the difference noise, never toward calling it real.
func ComparePaired(a, b [][]bool) PairedTest {
	var t PairedTest
	differences := make([]int, len(a))
	for r := range a {
		for i := range a[r] {
			t.Pairs++
			switch {
			case a[r][i] && !b[r][i]:
				t.Better++
				differences[r]++
			case !a[r][i] && b[r][i]:
				t.Worse++
				differences[r]--
			}
		}
	}
	t.PValue = mcnemarExact(t.Better, t.Worse)
	t.Real = t.PValue < PairedAlpha
	spread := SpreadOf(differences)
	t.MeanDifference = spread.Mean
	if n := len(differences); n > 1 {
		half := studentT975(n-1) * spread.SD / math.Sqrt(float64(n))
		t.Interval = &[2]float64{spread.Mean - half, spread.Mean + half}
	}
	return t
}

// mcnemarExact is the two-sided p-value of McNemar's exact test: twice the chance that better and
// worse discordant pairs, each falling either way with even odds, split at least as unevenly as
// they did, capped at 1. It counts in whole numbers, so it is exact for any count of pairs.
func mcnemarExact(better, worse int) float64 {
	n := better + worse
	tail, term := new(big.Int), big.NewInt(1) // term is n choose k, from k = 0
	for k := range min(better, worse) + 1 {
		if k > 0 {
			term.Mul(term, big.NewInt(int64(n-k+1)))
			term.Quo(term, big.NewInt(int64(k)))
		}
		tail.Add(tail, term)
	}
	p, _ := new(big.Rat).SetFrac(tail.Lsh(tail, 1), new(big.Int).Lsh(big.NewInt(1), uint(n))).Float64()
	return min(p, 1)
}

// studentT975 is the 97.5th percentile of Student's t distribution with df degrees of freedom,
// the multiplier of a two-sided 95% interval, found by bisection on the distribution function:
// for t at or above zero, 1 - I(df/(df+t²); df/2, 1/2)/2, I the regularized incomplete beta
// function. The percentile is 12.71 at one degree of freedom and falls toward 1.96, so it lies
// between 0 and 64.
func studentT975(df int) float64 {
	v := float64(df)
	lo, hi := 0.0, 64.0
	for range 100 {
		mid := (lo + hi) / 2
		if 1-regularizedBeta(v/(v+mid*mid), v/2, 0.5)/2 < 0.975 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// regularizedBeta is I(x; a, b), the regularized incomplete beta function, by its continued
// fraction, taken on the side of x where the fraction converges fast.
func regularizedBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaFraction(x, a, b) / a
	}
	return 1 - front*betaFraction(1-x, b, a)/b
}

// betaFraction is the continued fraction of the incomplete beta function at x, evaluated by
// Lentz's method.
func betaFraction(x, a, b float64) float64 {
	const tiny = 1e-300
	floor := func(v float64) float64 {
		if math.Abs(v) < tiny {
			return tiny
		}
		return v
	}
	c, d := 1.0, 1/floor(1-(a+b)*x/(a+1))
	h := d
	for m := 1.0; m <= 300; m++ {
		even := m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		d = 1 / floor(1+even*d)
		c = floor(1 + even/c)
		h *= d * c
		odd := -(a + m) * (a + b + m) * x / ((a + 2*m) * (a + 2*m + 1))
		d = 1 / floor(1+odd*d)
		c = floor(1 + odd/c)
		step := d * c
		h *= step
		if math.Abs(step-1) < 1e-15 {
			break
		}
	}
	return h
}
