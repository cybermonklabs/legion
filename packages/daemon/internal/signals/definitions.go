package signals

import (
	"math"
	"slices"
	"time"
)

// The definitions below take what a caller knows at a moment as plain values, frozen as it stands
// at that moment: a function named Next reads the moments after it as the caller's record would
// stand if no new event came, and answers the first moment at which the signal would start or stop
// binding by the comparison of Value.Binds.

// IdleShare is the share of holders that are idle with an idle time (idleSince, one for each idle
// holder) at least after before at: 0 where there is no holder.
func IdleShare(holders int, idleSince []time.Time, at time.Time, after time.Duration) float64 {
	if holders == 0 {
		return 0
	}
	long := 0
	for _, since := range idleSince {
		if !since.Add(after).After(at) {
			long++
		}
	}
	return float64(long) / float64(holders)
}

// IdleAt is the idle signal at at: the share of the holders idle since a moment at least after
// before at (IdleShare), unrecorded where c does not hold the signal.
func IdleAt(c Coverage, holders int, idleSince []time.Time, at time.Time, after time.Duration) Value {
	if !c.held(at) {
		return Value{}
	}
	return Value{Recorded: true, Value: IdleShare(holders, idleSince, at, after)}
}

// IdleNext is the idle signal's next crossing of limit after at, the holders standing as they do at
// at: the moment a holder idle since s reaches s + after, or an edge of the record. A holder leaves
// idle only at an event, so the signal falls at no moment of its own, and a crossing downward is
// due only where the record ends.
func IdleNext(c Coverage, holders int, idleSince []time.Time, at time.Time, after time.Duration, limit float64) (time.Time, bool) {
	moments := slices.Clone(c.Edges)
	for _, since := range idleSince {
		moments = append(moments, since.Add(after))
	}
	return FirstChange(at, moments, func(t time.Time) bool {
		return IdleAt(c, holders, idleSince, t, after).Binds(limit)
	})
}

// Wait is one wait of a pull request in the merge queue: from its entering the queue to the merge
// or the removal that ends it, Left zero while it stands at the end of the caller's record.
type Wait struct {
	Added, Left time.Time
}

// QueueWait is the queue signal at at as the record stands at known, at or before at, in
// durations: the larger of the median wait of the pull requests that left the queue in the 6 hours
// before at, (at − 6h, at], and the wait so far of the one longest in the queue. A wait that began
// after known, or ended after it, is read as the record stands at known.
func QueueWait(waits []Wait, at, known time.Time) time.Duration {
	var exits []time.Duration
	var longest time.Duration
	for _, wait := range waits {
		switch {
		case wait.Added.After(known):
		case wait.Left.IsZero() || wait.Left.After(known):
			longest = max(longest, at.Sub(wait.Added))
		case wait.Left.After(at.Add(-QueueExitSpan)):
			exits = append(exits, wait.Left.Sub(wait.Added))
		}
	}
	return max(medianDuration(exits), longest)
}

// QueueAt is the queue signal at at, in hours, unrecorded where c does not hold the signal.
func QueueAt(c Coverage, waits []Wait, at time.Time) Value {
	if !c.held(at) {
		return Value{}
	}
	return Value{Recorded: true, Value: QueueWait(waits, at, at).Hours()}
}

// QueueNext is the queue signal's next crossing of limit, in hours, after at: the waits standing as
// they do at at, the moment the wait of the pull request longest in the queue reaches the limit,
// the moment an exit leaves the 6 hours, or an edge of the record. Each moment is judged by the
// value a reading gives, in hours, against the limit, so a moment is a crossing only where the
// reading there binds.
func QueueNext(c Coverage, waits []Wait, at time.Time, limit float64) (time.Time, bool) {
	reach, reachable := reachOf(limit)
	moments := slices.Clone(c.Edges)
	for _, wait := range waits {
		switch {
		case wait.Added.After(at):
		case wait.Left.IsZero() || wait.Left.After(at):
			if reachable {
				moments = append(moments, wait.Added.Add(reach))
			}
		case wait.Left.After(at.Add(-QueueExitSpan)):
			moments = append(moments, wait.Left.Add(QueueExitSpan))
		}
	}
	return FirstChange(at, moments, func(t time.Time) bool {
		return Value{Recorded: c.held(t), Value: QueueWait(waits, t, at).Hours()}.Binds(limit)
	})
}

// reachOf is the shortest wait that reads limit hours or more: the least d with d.Hours() at or
// above limit, found by that comparison itself and so exact where a product of floating-point
// numbers would be a nanosecond off. It answers false for a limit no wait can reach.
func reachOf(limit float64) (time.Duration, bool) {
	if math.IsNaN(limit) || limit*float64(time.Hour) >= math.MaxInt64 {
		return 0, false
	}
	if limit <= 0 {
		return 0, true
	}
	d := time.Duration(math.Ceil(limit * float64(time.Hour)))
	for i := 0; i < 4 && d > 0 && (d-1).Hours() >= limit; i++ {
		d--
	}
	for i := 0; i < 4 && d.Hours() < limit; i++ {
		d++
	}
	return d, true
}

// medianDuration is the median of waits, the mean of the middle two of an even number, 0 for none.
func medianDuration(waits []time.Duration) time.Duration {
	if len(waits) == 0 {
		return 0
	}
	slices.Sort(waits)
	middle := len(waits) / 2
	if len(waits)%2 == 1 {
		return waits[middle]
	}
	return (waits[middle-1] + waits[middle]) / 2
}

// KillCount is the kills of the hour before at, (at − 1h, at], that the record holds by known, at
// or before at; kills are the moment of each kill, in order.
func KillCount(kills []time.Time, at, known time.Time) int {
	return max(upTo(kills, earlier(at, known))-upTo(kills, at.Add(-KillSpan)), 0)
}

// KillsAt is the kills signal at at: the count of the kills of the hour before at, so a kill counts
// from its own moment to an hour later. It is unrecorded where c does not hold the signal.
func KillsAt(c Coverage, kills []time.Time, at time.Time) Value {
	if !c.held(at) {
		return Value{}
	}
	return Value{Recorded: true, Value: float64(KillCount(kills, at, at))}
}

// KillsNext is the kills signal's next crossing of limit after at: the kills standing as they do at
// at, the moment an old kill leaves the hour, or an edge of the record. Kills come only at events,
// so the count rises at no moment of its own.
func KillsNext(c Coverage, kills []time.Time, at time.Time, limit float64) (time.Time, bool) {
	moments := slices.Clone(c.Edges)
	for _, kill := range kills[upTo(kills, at.Add(-KillSpan)):upTo(kills, at)] {
		moments = append(moments, kill.Add(KillSpan))
	}
	return FirstChange(at, moments, func(t time.Time) bool {
		return Value{Recorded: c.held(t), Value: float64(KillCount(kills, t, at))}.Binds(limit)
	})
}

// Minute is the provider's record of one whole minute as the headroom reads it: its uncached input
// tokens, and the limit of the project's own that bounds them in the minute (0 where none does:
// none set, one taken away, one of another scope, or a minute outside the record of the headroom).
type Minute struct {
	Uncached int64
	Limit    int64
}

// Series is a caller's record of the provider's minutes: the minute that starts at start, and
// whether the record holds it.
type Series func(start time.Time) (Minute, bool)

// MinuteBefore is the start of the last whole minute that ends at or before at, the minute the
// provider's headroom at at reads.
func MinuteBefore(at time.Time) time.Time { return at.Truncate(time.Minute).Add(-time.Minute) }

// ProviderAt is the provider's headroom signal at at: the uncached input tokens of the minute
// before at over the limit in force in it. The minute holds no headroom, and the signal is
// unrecorded, where the record holds no such minute or no limit binds in it.
func ProviderAt(read Series, at time.Time) Value {
	minute, held := read(MinuteBefore(at))
	if !held || minute.Limit <= 0 {
		return Value{}
	}
	return Value{Recorded: true, Value: float64(minute.Uncached) / float64(minute.Limit)}
}

// ProviderNext is the headroom's next crossing of limit after at: the first minute boundary, up to
// through, at which the caller's series, read as ProviderAt reads it, starts or stops binding. The
// caller says how far its record reaches (through), since the minutes after at are what it already
// holds.
func ProviderNext(read Series, at, through time.Time, limit float64) (time.Time, bool) {
	now := ProviderAt(read, at).Binds(limit)
	for boundary := at.Truncate(time.Minute).Add(time.Minute); !boundary.After(through); boundary = boundary.Add(time.Minute) {
		if ProviderAt(read, boundary).Binds(limit) != now {
			return boundary, true
		}
	}
	return time.Time{}, false
}
