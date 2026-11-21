package harness

import (
	"math"
	"math/rand/v2"
	"slices"
)

// The interval method every score uses (R2, plan t10): a 95% cluster bootstrap over the split
// groups (issue trees joined by the near-copy rule), and the exact bound over the groups where
// no failure or no success is seen.
const (
	// DefaultDraws is the bootstrap's resamples.
	DefaultDraws = 10000
	// DefaultSeed is the bootstrap's seed where a run names none.
	DefaultSeed = 3231
	// confidence is the one-sided confidence of the exact bound.
	confidence = 0.95
)

// The methods an interval comes from.
const (
	MethodBootstrap      = "cluster_bootstrap"
	MethodExact          = "exact_over_groups"
	MethodByConstruction = "by_construction"
)

// Interval is a score's 95% interval and the method that gave it.
type Interval struct {
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	Method string  `json:"method"`
}

// cluster is one split group's part of a score: the sum of its cases' values and its cases.
type cluster struct{ sum, n float64 }

// clusters sums values by group, in the order of the groups' keys.
func clusters(groups []string, values []float64) []cluster {
	by := map[string]*cluster{}
	var keys []string
	for i, g := range groups {
		c := by[g]
		if c == nil {
			c = &cluster{}
			by[g] = c
			keys = append(keys, g)
		}
		c.sum += values[i]
		c.n++
	}
	slices.Sort(keys)
	out := make([]cluster, len(keys))
	for i, k := range keys {
		out[i] = *by[k]
	}
	return out
}

// bootstrap is the 95% percentile interval of the ratio sum/n over clusters, by draws resamples
// of as many clusters as there are, with replacement, from a PCG source seeded with seed.
func bootstrap(cs []cluster, draws int, seed uint64) Interval {
	rng := rand.New(rand.NewPCG(seed, 0))
	stats := make([]float64, draws)
	for d := range stats {
		var sum, n float64
		for range cs {
			c := cs[rng.IntN(len(cs))]
			sum += c.sum
			n += c.n
		}
		stats[d] = sum / n
	}
	slices.Sort(stats)
	return Interval{Low: percentile(stats, 0.025), High: percentile(stats, 0.975), Method: MethodBootstrap}
}

// percentile is the nearest-rank percentile p of sorted values.
func percentile(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// exactBound is the exact one-sided 95% bound on the share of groups with an event when none of
// g groups shows one (Clopper-Pearson with each group one trial): 1 − 0.05^(1/g). It is 1 for no
// group.
func exactBound(g int) float64 {
	if g == 0 {
		return 1
	}
	return 1 - math.Pow(1-confidence, 1/float64(g))
}

// Share is a share of cases: the cases, the hits, the split groups they fall in, the share and
// its interval. Share and CI95 are absent for no case.
type Share struct {
	Cases  int       `json:"cases"`
	Hits   int       `json:"hits"`
	Groups int       `json:"groups"`
	Share  *float64  `json:"share,omitempty"`
	CI95   *Interval `json:"ci95,omitempty"`
}

// newShare is the share of hits among cases, each in its split group: a bootstrap interval, and
// the exact bound over the groups when every case or no case is a hit.
func newShare(groups []string, hits []bool, draws int, seed uint64) Share {
	s := Share{Cases: len(groups)}
	values := make([]float64, len(hits))
	for i, h := range hits {
		if h {
			s.Hits++
			values[i] = 1
		}
	}
	if s.Cases == 0 {
		return s
	}
	cs := clusters(groups, values)
	s.Groups = len(cs)
	share := float64(s.Hits) / float64(s.Cases)
	s.Share = &share
	var ci Interval
	switch s.Hits {
	case 0:
		ci = Interval{Low: 0, High: exactBound(s.Groups), Method: MethodExact}
	case s.Cases:
		ci = Interval{Low: 1 - exactBound(s.Groups), High: 1, Method: MethodExact}
	default:
		ci = bootstrap(cs, draws, seed)
	}
	s.CI95 = &ci
	return s
}

// Mean is a mean over cases: the cases, their split groups, the mean and its interval. Mean and
// CI95 are absent for no case.
type Mean struct {
	Cases  int       `json:"cases"`
	Groups int       `json:"groups"`
	Mean   *float64  `json:"mean,omitempty"`
	CI95   *Interval `json:"ci95,omitempty"`
}

// newMean is the mean of values, each case in its split group, with its bootstrap interval. A
// mean whose values are all one value has that value as its interval.
func newMean(groups []string, values []float64, draws int, seed uint64) Mean {
	m := Mean{Cases: len(values)}
	if m.Cases == 0 {
		return m
	}
	cs := clusters(groups, values)
	m.Groups = len(cs)
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(m.Cases)
	m.Mean = &mean
	ci := bootstrap(cs, draws, seed)
	m.CI95 = &ci
	return m
}

// byConstruction is a score that is zero with no call: its share or mean, and an interval at
// zero.
func byConstructionShare(groups []string) Share {
	s := newShare(groups, make([]bool, len(groups)), 1, 0)
	if s.CI95 != nil {
		s.CI95 = &Interval{Method: MethodByConstruction}
	}
	return s
}

func byConstructionMean(groups []string) Mean {
	m := newMean(groups, make([]float64, len(groups)), 1, 0)
	if m.CI95 != nil {
		m.CI95 = &Interval{Method: MethodByConstruction}
	}
	return m
}
