package independent

import (
	"slices"
	"time"
)

// spans.go is the measure this reading counts hours with. Every rule is written over stretches of
// time rather than over clock hours, because "the hours an issue spent in a phase" is a measure of
// stretches: a phase that held for ten minutes is ten minutes of that phase.

// span is the stretch from from up to but not including to.
type span struct {
	from time.Time
	to   time.Time
}

// length is how long the span covers, and zero for one that ends where it starts or earlier.
func (s span) length() time.Duration {
	if !s.to.After(s.from) {
		return 0
	}
	return s.to.Sub(s.from)
}

// normalize is the set the stretches cover, in order and without overlaps: the empty ones
// dropped, the rest sorted by their start, and two that touch or overlap joined into one. Every
// answer below is in this shape, so no rule has to think about the order it was handed.
func normalize(stretches []span) []span {
	kept := make([]span, 0, len(stretches))
	for _, stretch := range stretches {
		if stretch.length() > 0 {
			kept = append(kept, stretch)
		}
	}
	slices.SortFunc(kept, func(a, b span) int { return a.from.Compare(b.from) })
	joined := make([]span, 0, len(kept))
	for _, stretch := range kept {
		last := len(joined) - 1
		if last >= 0 && !stretch.from.After(joined[last].to) {
			if stretch.to.After(joined[last].to) {
				joined[last].to = stretch.to
			}
			continue
		}
		joined = append(joined, stretch)
	}
	return joined
}

// intersect is the time both sets cover.
func intersect(left, right []span) []span {
	var shared []span
	right = normalize(right)
	for _, l := range normalize(left) {
		for _, r := range right {
			overlap := l
			if r.from.After(overlap.from) {
				overlap.from = r.from
			}
			if r.to.Before(overlap.to) {
				overlap.to = r.to
			}
			if overlap.length() > 0 {
				shared = append(shared, overlap)
			}
		}
	}
	return normalize(shared)
}

// subtract is the time the left set covers and the right set does not.
func subtract(left, right []span) []span {
	rest := normalize(left)
	for _, cut := range normalize(right) {
		remaining := make([]span, 0, len(rest)+1)
		for _, stretch := range rest {
			if !cut.to.After(stretch.from) || !stretch.to.After(cut.from) {
				remaining = append(remaining, stretch)
				continue
			}
			if cut.from.After(stretch.from) {
				remaining = append(remaining, span{from: stretch.from, to: cut.from})
			}
			if stretch.to.After(cut.to) {
				remaining = append(remaining, span{from: cut.to, to: stretch.to})
			}
		}
		rest = remaining
	}
	return normalize(rest)
}

// hoursOf is how many hours a set of stretches covers, which is the unit every measure of R6
// states its time in.
func hoursOf(stretches []span) float64 {
	var covered time.Duration
	for _, stretch := range stretches {
		covered += stretch.length()
	}
	return covered.Hours()
}

// markStretches is the stretches in which a field's recorded value satisfies holds. A value is
// recorded only where it moved, so each mark holds until the next mark of that field, and the
// last holds to end.
func markStretches(marks []mark, end time.Time, holds func(value string) bool) []span {
	var stretches []span
	for index, recorded := range marks {
		if !holds(recorded.value) {
			continue
		}
		stretch := span{from: recorded.at, to: end}
		if index+1 < len(marks) {
			stretch.to = marks[index+1].at
		}
		stretches = append(stretches, stretch)
	}
	return normalize(stretches)
}
