package fold

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// GroupFigures is one group series folded over a window's parts: share, the summed numerator over
// the summed denominator of its measured days (the lines alive at their marks, or removed in the
// series' kind, over the lines the group's changes added), never the mean of the days' shares;
// lines, that numerator; added, that denominator; changes, the group's changes; and
// via_repository, those of them with no issue. Every figure is nil, "no data" and never 0, when the
// window holds a marked or blocked day, a row of a repository the App cannot read, no measured
// day, or no part of the series; and share alone is nil when the group added no line.
type GroupFigures struct {
	Share         *float64
	Lines         *float64
	Added         *float64
	Changes       *float64
	ViaRepository *float64
}

// FoldSurvival is a survival:<g> series of the code survival eval folded over parts, the parts of
// a window of days of that eval.
func FoldSurvival(parts []measures.DayPart, series string) GroupFigures {
	return foldGroup(parts, series)
}

// FoldRework is a rework:<g>, iteration:<g>, noissue:<g> or other:<g> series of the post-merge
// rework eval folded over parts, the parts of a window of days of that eval: the lines removed in
// that kind over the lines the group added. Only rework:<g> is the post-merge rework number.
func FoldRework(parts []measures.DayPart, series string) GroupFigures {
	return foldGroup(parts, series)
}

// foldGroup is FoldSurvival and FoldRework: both fold a share of lines over lines added.
func foldGroup(parts []measures.DayPart, series string) GroupFigures {
	if !readable(parts) {
		return GroupFigures{}
	}
	folded := measures.FoldSeries(parts, series)
	if folded.Parts == 0 {
		return GroupFigures{}
	}
	changes, via := 0, 0
	for _, part := range parts {
		if part.Series == series {
			changes += part.N
			via += part.ViaRepository
		}
	}
	figures := GroupFigures{Lines: new(folded.Num), Added: new(folded.Den), Changes: new(float64(changes)),
		ViaRepository: new(float64(via))}
	if folded.Den > 0 {
		figures.Share = new(folded.Num / folded.Den)
	}
	return figures
}

// readable says whether a window's parts can be read as a number: they hold a measured day, and
// no marked or blocked day and no row of a repository the App cannot read (R7 to R9, R11).
func readable(parts []measures.DayPart) bool {
	measured := false
	for _, part := range parts {
		if part.Counts[CountRepoUnreadable] > 0 {
			return false
		}
		if part.Series != SeriesStatus {
			continue
		}
		if part.Counts[CountMarked] > 0 || part.Counts[CountBlocked] > 0 {
			return false
		}
		if part.Counts[CountMeasured] > 0 {
			measured = true
		}
	}
	return measured
}

// Status is the status series folded over the days of a range: how many are measured, marked and
// blocked, the files the blocking commits withheld, the oldest marked day (nil with none), the
// groups the rows were built under (nil with no row), how many rows name a repository the App
// cannot read, and how many days of the range wait: those after the newest day with a row, every
// day of the range when none has one.
type Status struct {
	MeasuredDays   int
	MarkedDays     int
	BlockedDays    int
	BlockedFiles   int
	FirstMarkedDay *time.Time
	Groups         *int
	RepoUnreadable int
	WaitingDays    int
}

// FoldStatus is the status series of either eval folded over parts, for the range of UTC days
// from from up to to (to not included).
func FoldStatus(parts []measures.DayPart, from, to time.Time) Status {
	var status Status
	waitFrom := from
	for _, part := range parts {
		if part.Day.Before(from) || !part.Day.Before(to) {
			continue
		}
		if after := part.Day.Add(24 * time.Hour); after.After(waitFrom) {
			waitFrom = after
		}
		if part.Series != SeriesStatus {
			continue
		}
		status.MeasuredDays += int(part.Counts[CountMeasured])
		status.MarkedDays += int(part.Counts[CountMarked])
		status.BlockedDays += int(part.Counts[CountBlocked])
		status.BlockedFiles += int(part.Counts[CountBlockedFiles])
		if first, ok := part.Counts[CountFirstMarkedDay]; ok && part.Counts[CountMarked] > 0 {
			if day := time.Unix(int64(first), 0).UTC(); status.FirstMarkedDay == nil || day.Before(*status.FirstMarkedDay) {
				status.FirstMarkedDay = &day
			}
		}
		if groups := int(part.Counts[CountGroups]); status.Groups == nil || groups > *status.Groups {
			status.Groups = &groups
		}
		if part.Counts[CountRepoUnreadable] > 0 {
			status.RepoUnreadable++
		}
	}
	if waitFrom.Before(to) {
		status.WaitingDays = int(to.Sub(waitFrom) / (24 * time.Hour))
	}
	return status
}
