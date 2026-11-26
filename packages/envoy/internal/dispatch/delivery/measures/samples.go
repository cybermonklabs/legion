package measures

import (
	"cmp"
	"math"
	"slices"
	"sort"
)

// SortedSamples is every sample of one series among a set of parts, in ascending order: the one
// list a quantile reads (MergedSamples). It is that list merged and sorted, or, where each part's
// samples already ascend and hold no -0 and no NaN (SortableSamples), the parts' lists as they
// stand, of which each rank a fold reads is selected across the lists when it is read (At).
//
// The stored figures fold one series' days into many ranges, each of up to 85 days (evals,
// figures.go), so the fold sorts each day's samples once, and every range that holds the day
// reads them sorted: a range's median and 90th percentile are two selections across its days'
// lists, each of the two adjacent ranks a quantile reads (adjacent), where a merge and a sort of
// every sample is paid again for each range. k8 measured selection on the bench's busiest quantile
// series (dispatch://INFRA-2916, k8 phase C, qbench), one rank at a time: for the ranges a change
// 60 days back rewrites, selection took 19-26 ms where the merge and sort took 108-227 ms, and the
// two answered every range alike, bit for bit. A rank selected is the value the sort puts at that
// rank: the lists hold no -0 and no NaN, and of the values left no two that compare equal differ in
// their bits.
type SortedSamples struct {
	// merged is the sorted merge, nil where lists answers.
	merged []float64
	// lists is the parts' sorted lists, each non-empty, read by selection; nil where merged
	// answers.
	lists [][]float64
	n     int
}

// Len is how many samples s holds.
func (s SortedSamples) Len() int { return s.n }

// At is the k-th smallest sample (from 0) of s; k is below Len.
func (s SortedSamples) At(k int) float64 {
	if s.lists == nil {
		return s.merged[k]
	}
	if k == s.n-1 {
		largest := math.Inf(-1)
		for _, list := range s.lists {
			largest = max(largest, list[len(list)-1])
		}
		return largest
	}
	value, _ := kth(s.lists, k)
	return value
}

// adjacent is the k-th and the (k+1)-th smallest samples (from 0) of s; k+1 is below Len. The
// merged list answers both by index, and the lists by one selection (kth), where At would select
// each rank across the lists again.
func (s SortedSamples) adjacent(k int) (float64, float64) {
	if s.lists == nil {
		return s.merged[k], s.merged[k+1]
	}
	return kth(s.lists, k)
}

// Max is the largest sample of s, which holds at least one.
func (s SortedSamples) Max() float64 { return s.At(s.n - 1) }

// Quantile is p's quantile of s, which holds at least one sample, by Quantile's rule: linear
// between the two nearest ranks, each product converted to float64 before the sum it feeds, for
// the reason Quantile gives. It reads the two ranks together (adjacent), which across the lists is
// one selection where two reads of At would be two.
func (s SortedSamples) Quantile(p float64) float64 {
	i := float64(float64(s.n-1) * p)
	lo := int(math.Floor(i))
	if lo+1 >= s.n {
		return s.At(s.n - 1)
	}
	low, high := s.adjacent(lo)
	return low + float64((high-low)*(i-float64(lo)))
}

// MergedSamples is every sample of series among parts in ascending order: the one list a quantile
// reads. Where each part of series among parts holds samples that already ascend and hold no -0
// and no NaN (as the stored figures' fold leaves them), it reads their lists as they stand
// (SortedSamples), which answers every rank as the sort would; else it merges them, in the parts'
// order, and sorts the merge. A part read for a request holds its samples in the order they were
// stored, whose first descent ends the check.
func MergedSamples(parts []DayPart, series string) SortedSamples {
	var lists [][]float64
	n, ascending := 0, true
	for _, part := range parts {
		if part.Series != series || len(part.Samples) == 0 {
			continue
		}
		lists = append(lists, part.Samples)
		n += len(part.Samples)
		ascending = ascending && ascends(part.Samples)
	}
	switch {
	case len(lists) == 0:
		return SortedSamples{}
	case ascending && len(lists) == 1:
		return SortedSamples{merged: lists[0], n: n}
	case ascending:
		return SortedSamples{lists: lists, n: n}
	}
	merged := make([]float64, 0, n)
	for _, list := range lists {
		merged = append(merged, list...)
	}
	slices.Sort(merged)
	return SortedSamples{merged: merged, n: n}
}

// ascends reports whether samples are in ascending order and hold no -0 and no NaN
// (SortableSamples): over samples with no NaN, which SortableSamples refuses, slices.IsSorted's
// order is <.
func ascends(samples []float64) bool {
	return slices.IsSorted(samples) && SortableSamples(samples)
}

// SortableSamples reports whether samples hold no -0 and no NaN, the lists MergedSamples reads as
// they stand once sorted: sorted, such a list holds each value at the one rank selection finds it
// at. A -0 beside a 0, which the sort holds equal, may stand on either side of it after a sort,
// and a quantile that reads the two would differ in its sign of zero between the sort and a
// selection.
func SortableSamples(samples []float64) bool {
	for _, value := range samples {
		if math.IsNaN(value) || value == 0 && math.Signbit(value) {
			return false
		}
	}
	return true
}

// kth is the k-th and the (k+1)-th smallest values (from 0) of the union of lists, each sorted
// ascending and non-empty, k+1 below their total length, exactly: each round counts every list's
// values below and equal to a pivot, the weighted median of the lists' middle values (so a round
// leaves at least a quarter of what remains behind), by binary search, and keeps the side that
// holds rank k, until the pivot's values hold it.
//
// That round answers rank k+1 too. The windows' values equal to the pivot hold their ranks less to
// less+equal-1, so where k+1 < less+equal rank k+1 is the pivot again. Otherwise it is the smallest
// value of the lists above the pivot: the smallest list[through] of the lists whose through is
// below their length, since the values of each list before through are at most the pivot and
// those from through on are above it. Within the list's window (lo to hi) that is what through
// counts. A value before lo was dropped by a round that raised lo, as at most that round's pivot,
// and that round left only values above its pivot in the windows, this pivot among them. A value
// at hi or beyond was dropped by a round that lowered hi, as at least that round's pivot, and that
// round left only values below its pivot in the windows, this pivot among them. The value at rank
// k+1 is the one the sort puts there, bit for bit, for the reason SortedSamples gives.
func kth(lists [][]float64, k int) (float64, float64) {
	// One allocation holds every list's window, which a round sorts by its middle value: a fold of
	// a range's quantiles selects for each of them, and garbage collection was 13 % of the CPU of a
	// pass that rewrites the stored figures (dispatch://INFRA-2916, phase H). A window names its
	// list by index and holds no pointer, so the collector has nothing to scan in it and a sort
	// moves it without a write barrier.
	windows := make([]listWindow, len(lists))
	for i, list := range lists {
		windows[i] = listWindow{list: i, hi: len(list)}
	}
	for {
		// The windows that hold values come first, in the order of their middle values.
		live, total := 0, 0
		for i := range windows {
			if w := windows[i]; w.hi > w.lo {
				w.middle = lists[w.list][w.lo+(w.hi-w.lo)/2]
				windows[i], windows[live] = windows[live], w
				live, total = live+1, total+w.hi-w.lo
			}
		}
		slices.SortFunc(windows[:live], func(a, b listWindow) int { return cmp.Compare(a.middle, b.middle) })
		pivot, seen := windows[live-1].middle, 0
		for i := range live {
			seen += windows[i].hi - windows[i].lo
			if 2*seen >= total {
				pivot = windows[i].middle
				break
			}
		}
		less, equal := 0, 0
		for i := range windows {
			w := &windows[i]
			values := lists[w.list][w.lo:w.hi]
			w.below = w.lo + sort.SearchFloat64s(values, pivot)
			w.through = w.lo + sort.Search(len(values), func(j int) bool { return values[j] > pivot })
			less += w.below - w.lo
			equal += w.through - w.below
		}
		switch {
		case k < less:
			for i := range windows {
				windows[i].hi = windows[i].below
			}
		case k+1 < less+equal:
			return pivot, pivot
		case k < less+equal:
			next := math.Inf(1)
			for _, w := range windows {
				if list := lists[w.list]; w.through < len(list) {
					next = min(next, list[w.through])
				}
			}
			return pivot, next
		default:
			k -= less + equal
			for i := range windows {
				windows[i].lo = windows[i].through
			}
		}
	}
}

// listWindow is one list of a selection (kth), by its index among the lists: the values it still
// holds (lo to hi), the ends of those below and up to the round's pivot (below, through), and its
// middle value.
type listWindow struct {
	list, lo, hi, below, through int
	middle                       float64
}
