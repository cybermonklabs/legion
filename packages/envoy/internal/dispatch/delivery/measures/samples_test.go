package measures

import (
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// sortedParts is one series' days as the stored figures' fold holds them: each day's samples
// sorted.
func sortedParts(series string, days [][]float64) []DayPart {
	parts := make([]DayPart, len(days))
	for index, samples := range days {
		sorted := slices.Clone(samples)
		slices.Sort(sorted)
		parts[index] = DayPart{Series: series, Day: time.Date(2026, 1, 1+index, 0, 0, 0, 0, time.UTC), Samples: sorted}
	}
	return parts
}

// mergedOf is every sample of days merged and sorted, as a read of the rows folds them.
func mergedOf(days [][]float64) []float64 {
	var merged []float64
	for _, samples := range days {
		merged = append(merged, samples...)
	}
	slices.Sort(merged)
	return merged
}

// TestASelectedRankIsTheSortsRankBitForBit: over day lists of every shape a series' days take (one
// sample, none, many, repeated values, values that differ in their last bit, negatives, zeros and
// very large and very small magnitudes), every rank selected across the days' sorted lists, the
// largest and each quantile a fold reads are the merge and sort's, bit for bit, as the stored
// figures' fold needs them to be (SortedSamples).
func TestASelectedRankIsTheSortsRankBitForBit(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	value := func() float64 {
		switch random.IntN(6) {
		case 0:
			return float64(random.IntN(5))
		case 1:
			return -random.Float64() * 100
		case 2:
			return math.Nextafter(1, 2)
		case 3:
			return random.Float64() * 1e300
		case 4:
			return random.Float64() * 1e-300
		}
		return random.Float64() * 1000
	}
	for trial := range 200 {
		days := make([][]float64, 1+random.IntN(85))
		for index := range days {
			samples := make([]float64, random.IntN(30))
			for k := range samples {
				samples[k] = value()
			}
			days[index] = samples
		}
		merged := mergedOf(days)
		selected := MergedSamples(sortedParts("minutes", days), "minutes")
		if selected.Len() != len(merged) {
			t.Fatalf("trial %d: the selection holds %d samples, the merge %d", trial, selected.Len(), len(merged))
		}
		if len(merged) == 0 {
			continue
		}
		// Every rank of a merge of up to 100 samples; of a longer one, its two ends and 100 ranks
		// at random.
		ranks := []int{0, len(merged) - 1}
		for k := range min(len(merged), 100) {
			if len(merged) > 100 {
				k = random.IntN(len(merged))
			}
			ranks = append(ranks, k)
		}
		for _, k := range ranks {
			if got, want := selected.At(k), merged[k]; math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("trial %d: rank %d selected is %v, the sort's is %v", trial, k, got, want)
			}
		}
		if got, want := selected.Max(), merged[len(merged)-1]; math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("trial %d: the largest selected is %v, the sort's is %v", trial, got, want)
		}
		for _, p := range []float64{0, 0.1, 0.5, 0.9, 1} {
			if got, want := selected.Quantile(p), Quantile(merged, p); math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("trial %d: quantile %v selected is %v, the sort's is %v", trial, p, got, want)
			}
		}
	}
}

// TestAPartNotSortedMergesEveryPart: a fold of parts one of which holds its samples in the order
// they were stored, as a part read for a request does, merges and sorts every part's samples,
// whatever the others hold; so does one whose sorted list holds a -0 or a NaN, which is no list a
// sort holds bit for bit (SortableSamples), since a -0 beside a 0 may sort to either side of it.
func TestAPartNotSortedMergesEveryPart(t *testing.T) {
	days := [][]float64{{3, 1}, {2, 5}, {4}}
	parts := sortedParts("minutes", days)
	if selected := MergedSamples(parts, "minutes"); selected.lists == nil {
		t.Fatal("a fold over parts whose samples are all sorted merges them")
	}
	parts[1] = DayPart{Series: "minutes", Samples: []float64{5, 2}}
	merged := MergedSamples(parts, "minutes")
	if merged.lists != nil {
		t.Fatalf("a fold over a part whose samples are not sorted selects across the lists %v", merged.lists)
	}
	for k, want := range []float64{1, 2, 3, 4, 5} {
		if got := merged.At(k); got != want {
			t.Errorf("rank %d is %v, want %v", k, got, want)
		}
	}
	negativeZero := math.Copysign(0, -1)
	parts[1] = DayPart{Series: "minutes", Samples: []float64{negativeZero, 0}}
	if signed := MergedSamples(parts, "minutes"); signed.lists != nil {
		t.Fatalf("a fold over a part whose sorted list holds a -0 selects across the lists %v", signed.lists)
	}
	if !SortableSamples([]float64{0, 1, -2}) {
		t.Error("a list of a 0, a 1 and a -2 reads as one a sort does not hold bit for bit")
	}
	for _, samples := range [][]float64{{1, negativeZero}, {math.NaN()}} {
		if SortableSamples(samples) {
			t.Errorf("the list %v reads as one a sort holds bit for bit", samples)
		}
	}
}

// plainQuantile is p's quantile of sorted, the plain sort's answer the selection must match: the
// arithmetic Quantile's rule states, over the sorted list by index.
func plainQuantile(sorted []float64, p float64) float64 {
	i := float64(float64(len(sorted)-1) * p)
	lo := int(math.Floor(i))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + float64((sorted[lo+1]-sorted[lo])*(i-float64(lo)))
}

// sameBits reports whether got and want are one float, bit for bit.
func sameBits(got, want float64) bool {
	return math.Float64bits(got) == math.Float64bits(want)
}

// expectRanksOf checks every pair of adjacent ranks of lists, each sorted ascending and holding no
// -0 and no NaN, and each quantile of ps and of the middle of each pair, against the sorted merge
// of the lists, bit for bit: selected across the lists (SortedSamples' lists, adjacent and
// Quantile) and read by index from the merge (its merged form). It answers how many pairs it
// checked.
func expectRanksOf(t *testing.T, what string, lists [][]float64, ps []float64) int {
	t.Helper()
	merged := mergedOf(lists)
	selected := SortedSamples{lists: lists, n: len(merged)}
	indexed := SortedSamples{merged: merged, n: len(merged)}
	quantiles := slices.Clone(ps)
	for k := 0; k+1 < len(merged); k++ {
		for _, samples := range []SortedSamples{selected, indexed} {
			if low, high := samples.adjacent(k); !sameBits(low, merged[k]) || !sameBits(high, merged[k+1]) {
				t.Fatalf("%s: ranks %d and %d of %v read %v and %v (across lists %t), the sort's are %v and %v",
					what, k, k+1, lists, low, high, samples.lists != nil, merged[k], merged[k+1])
			}
		}
		quantiles = append(quantiles, (float64(k)+0.5)/float64(len(merged)-1))
	}
	for _, p := range quantiles {
		want := plainQuantile(merged, p)
		for _, samples := range []SortedSamples{selected, indexed} {
			if got := samples.Quantile(p); !sameBits(got, want) {
				t.Fatalf("%s: quantile %v of %v reads %v (across lists %t), the sort's is %v", what, p, lists, got, samples.lists != nil, want)
			}
		}
		if got := Quantile(merged, p); !sameBits(got, want) {
			t.Fatalf("%s: quantile %v of the sorted %v reads %v, the sort's is %v", what, p, merged, got, want)
		}
	}
	return max(len(merged)-1, 0)
}

// TestTwoAdjacentRanksSelectedAreTheSortsBitForBit: over thousands of sets of sorted lists, drawn
// from a few values each so that equal values run within and across lists (repeated values, values
// that differ in their last bit, negatives, zeros, infinities and very large and very small
// magnitudes), of single-element lists and longer ones, some sitting wholly above others, every
// pair of adjacent ranks one selection answers (adjacent, kth) is the sorted merge's, bit for bit,
// and so is every quantile read through it. The test prints its seed, and MEASURES_RANKS_SEED draws
// the same lists again.
func TestTwoAdjacentRanksSelectedAreTheSortsBitForBit(t *testing.T) {
	seed := rand.Uint64()
	if given := os.Getenv("MEASURES_RANKS_SEED"); given != "" {
		parsed, err := strconv.ParseUint(given, 10, 64)
		if err != nil {
			t.Fatalf("MEASURES_RANKS_SEED=%q is no seed: %v", given, err)
		}
		seed = parsed
	}
	t.Logf("the lists' seed is %d: MEASURES_RANKS_SEED=%d draws them again", seed, seed)
	random := rand.New(rand.NewPCG(seed, 0))
	value := func() float64 {
		switch random.IntN(8) {
		case 0:
			return float64(random.IntN(4))
		case 1:
			return -random.Float64() * 100
		case 2:
			return math.Nextafter(1, 2)
		case 3:
			return math.Nextafter(1, 0)
		case 4:
			return random.Float64() * 1e300
		case 5:
			return random.Float64() * 1e-300
		case 6:
			return math.Inf(2*random.IntN(2) - 1)
		}
		return random.Float64() * 1000
	}
	sets, pairs := 3000, 0
	for set := range sets {
		pool := make([]float64, 1+random.IntN(10))
		for index := range pool {
			pool[index] = value()
		}
		slices.Sort(pool)
		lists := make([][]float64, 1+random.IntN(12))
		for index := range lists {
			length := 1
			if random.IntN(3) > 0 {
				length += random.IntN(40)
			}
			// Each list draws from a window of the pool, so some sit wholly above others.
			from := random.IntN(len(pool))
			window := pool[from : from+1+random.IntN(len(pool)-from)]
			list := make([]float64, length)
			for k := range list {
				list[k] = window[random.IntN(len(window))]
			}
			slices.Sort(list)
			lists[index] = list
		}
		pairs += expectRanksOf(t, "set "+strconv.Itoa(set), lists, []float64{0, 0.1, 0.5, 0.9, 1})
	}
	t.Logf("%d sets of lists: %d pairs of adjacent ranks selected, each the sort's, bit for bit", sets, pairs)
}

// TestTheQuantileEdgesSelectedAreTheSortsBitForBit: the shapes where the rank after a selected one
// is read otherwise than the rank itself, each pair of adjacent ranks and each quantile, through
// Quantile and the two-rank selection both, the sorted merge's bit for bit: ties at rank k and
// k+1; several copies of the pivot, rank k+1 inside their run (the pivot again) and just past it
// (the smallest value above it in all of the lists); even and odd counts; one sample and two; one
// list and many; a list whose values all sit above the pivot, some beyond its window; and values
// that differ in their last bit.
func TestTheQuantileEdgesSelectedAreTheSortsBitForBit(t *testing.T) {
	one, many := []float64{1}, make([][]float64, 20)
	for index := range many {
		many[index] = []float64{float64(index % 7), float64(index%7) + 0.5, float64(index % 3)}
		slices.Sort(many[index])
	}
	above := math.Nextafter(1, 2)
	for _, shape := range []struct {
		name  string
		lists [][]float64
	}{
		{"ties at rank k and k+1", [][]float64{{1, 2, 2, 3}, {2, 2, 4}}},
		{"several copies of the pivot", [][]float64{{1, 5, 5, 5, 9}, {5, 5, 7}, {5, 8}, {0, 5}}},
		{"an even count", [][]float64{{1, 3}, {2, 4}, {0.5, 6}}},
		{"an odd count", [][]float64{{1, 3, 5}, {2, 4}}},
		{"one sample", [][]float64{one}},
		{"two samples in one list", [][]float64{{3, 4}}},
		{"two samples in two lists", [][]float64{{4}, {3}}},
		{"one list", [][]float64{{1, 2, 2, 2, 3, 8, 9}}},
		{"many lists", many},
		{"a list whose values all sit above the pivot", [][]float64{{1, 2, 3, 4}, {100, 200}, {0.5, 1.5, 2.5}}},
		{"values that differ in their last bit", [][]float64{{1, above}, {above, math.Nextafter(above, 2)}, {math.Nextafter(1, 0), 1}}},
	} {
		expectRanksOf(t, shape.name, shape.lists, []float64{0, 0.1, 0.25, 0.5, 0.75, 0.9, 1})
	}
}

// TestASelectionAllocatesOnce: one selection of two ranks across lists, a fortnight of days' each,
// makes one allocation, the slice of the lists' windows (kth).
func TestASelectionAllocatesOnce(t *testing.T) {
	lists := make([][]float64, 14)
	for index := range lists {
		lists[index] = []float64{float64(index % 5), float64(index%5) + 1, float64(index % 3 * 4)}
		slices.Sort(lists[index])
	}
	if allocations := testing.AllocsPerRun(100, func() { kth(lists, 20) }); allocations > 1 {
		t.Errorf("a selection of two ranks across %d lists allocates %v times, want once", len(lists), allocations)
	}
}
