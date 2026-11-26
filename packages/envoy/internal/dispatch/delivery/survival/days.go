package survival

import (
	"crypto/sha256"
	"hash"
	"slices"
	"time"
)

// The day rules of dispatch://INFRA-3206 (plan, "The rules the computation holds"). A day is the
// UTC date of a commit's committer time; every day here is that date's UTC midnight.
const (
	oneDay = 24 * time.Hour
	// markAge is a change's age at its mark.
	markAge = 30 * oneDay
	// waitDays is how many days after its own a day starts to wait: its newest change's mark,
	// which is before the end of its 31st day, and two days' grace (Departure 2).
	waitDays = 33
	// rangeDays is how many days after the newest selected day the replay range ends, at 00:00:
	// past the mark of every change of that day.
	rangeDays = 31
	// tailDays is how many days after the newest selected day a measurement fingerprints: the days
	// its changes' marks reach.
	tailDays = 30
	// blockDays is how many days before a commit's own a commit that cannot be read blocks: the
	// selected days whose 30 days hold it.
	blockDays = 31
	// runDays is the most days one run measures, counted from its first.
	runDays = 28
	// minWaiting is how many waiting days start a run with no marked day.
	minWaiting = 7
	// retryAfter is how long a blocked day waits before it is tried again.
	retryAfter = 7 * oneDay
	// startDays is how many days end on the newest waiting day from a repository's start day.
	startDays = 84
)

// DayState is a stored day's state, as code_days holds it: replayed (a fingerprint alone, which
// leaves the day waiting), measured, marked (measured, then found changed) or blocked.
type DayState string

// The day states.
const (
	DayReplayed DayState = "replayed"
	DayMeasured DayState = "measured"
	DayMarked   DayState = "marked"
	DayBlocked  DayState = "blocked"
)

// dayNumber is how many UTC days t is after the Unix epoch, rounded down.
func dayNumber(t time.Time) int64 {
	seconds := t.Unix()
	number := seconds / 86400
	if seconds%86400 < 0 {
		number--
	}
	return number
}

// dayOfNumber is the UTC midnight of day number.
func dayOfNumber(number int64) time.Time { return time.Unix(number*86400, 0).UTC() }

// DayOf is the UTC midnight of t's day.
func DayOf(t time.Time) time.Time { return dayOfNumber(dayNumber(t)) }

// Mark is the 30-day mark of a change committed at committedAt. A line of the change is alive at
// its mark when no commit with a committer time at or before the mark removed it
// (scripts/replaylib.py, alive_at).
func Mark(committedAt time.Time) time.Time { return committedAt.Add(markAge) }

// WaitsFrom is the moment day starts to wait to be measured: day + 33 days 00:00 UTC, two days
// after the mark of its last change.
func WaitsFrom(day time.Time) time.Time { return DayOf(day).Add(waitDays * oneDay) }

// StartDay is the start day a repository's first daily pass at now sets: the day the 84 days
// that end on the newest waiting day start on. No day before it is measured.
func StartDay(now time.Time) time.Time {
	return dayOfNumber(dayNumber(now) - waitDays - (startDays - 1))
}

// ReplayRange is the commits of chain, a first-parent chain in chain order, that a measurement of
// the days oldest to newest replays: from the first commit dated on or after oldest 00:00 to the
// last dated at or before newest + 31 days 00:00, every commit between them whatever its date. It
// is a part of chain, nil when no commit falls in it.
func ReplayRange(chain []Commit, oldest, newest time.Time) []Commit {
	from := DayOf(oldest)
	start := slices.IndexFunc(chain, func(commit Commit) bool { return !commit.CommittedAt.Before(from) })
	if start < 0 {
		return nil
	}
	cutoff := DayOf(newest).Add(rangeDays * oneDay)
	for end := len(chain) - 1; end >= start; end-- {
		if !chain[end].CommittedAt.After(cutoff) {
			return chain[start : end+1]
		}
	}
	return nil
}

// Fingerprints is the fingerprint of each day from oldest to newest in chain, a first-parent
// chain in chain order: the count of its commits dated that day, and the sha256 of their SHAs in
// chain order joined by "\n" (a day with no commit has the sha256 of nothing).
func Fingerprints(chain []Commit, oldest, newest time.Time) []Fingerprint {
	first, last := dayNumber(oldest), dayNumber(newest)
	if last < first {
		return nil
	}
	out := make([]Fingerprint, last-first+1)
	digests := make([]hash.Hash, len(out))
	for index := range out {
		out[index].Day = dayOfNumber(first + int64(index))
		digests[index] = sha256.New()
	}
	var buffer []byte
	for _, commit := range chain {
		index := dayNumber(commit.CommittedAt) - first
		if index < 0 || index >= int64(len(out)) {
			continue
		}
		buffer = buffer[:0]
		if out[index].Commits > 0 {
			buffer = append(buffer, '\n')
		}
		buffer = append(buffer, commit.SHA...)
		digests[index].Write(buffer)
		out[index].Commits++
	}
	for index := range out {
		digests[index].Sum(out[index].Digest[:0])
	}
	return out
}

// ChangedDays is every day of stored, the fingerprints a repository holds, whose fingerprint
// chain no longer gives, in day order: a force-push's mark (R8).
func ChangedDays(stored []Fingerprint, chain []Commit) []time.Time {
	if len(stored) == 0 {
		return nil
	}
	oldest := slices.MinFunc(stored, func(a, b Fingerprint) int { return a.Day.Compare(b.Day) }).Day
	newest := slices.MaxFunc(stored, func(a, b Fingerprint) int { return a.Day.Compare(b.Day) }).Day
	fresh := Fingerprints(chain, oldest, newest)
	first := dayNumber(oldest)
	var changed []int64
	for _, fingerprint := range stored {
		number := dayNumber(fingerprint.Day)
		if now := fresh[number-first]; now.Commits != fingerprint.Commits || now.Digest != fingerprint.Digest {
			changed = append(changed, number)
		}
	}
	return daysOf(changed)
}

// MarkedDays is every day of measured that a changed day marks, in day order: a changed day X
// marks itself, if measured, and every measured day of X − 31 to X, whose 30 days hold it.
func MarkedDays(changed, measured []time.Time) []time.Time {
	isMeasured := make(map[int64]bool, len(measured))
	for _, day := range measured {
		isMeasured[dayNumber(day)] = true
	}
	var marked []int64
	for _, day := range changed {
		last := dayNumber(day)
		for number := last - blockDays; number <= last; number++ {
			if isMeasured[number] {
				marked = append(marked, number)
			}
		}
	}
	return daysOf(marked)
}

// daysOf is the UTC midnights of numbers, in order and each once; nil for none.
func daysOf(numbers []int64) []time.Time {
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	if len(numbers) == 0 {
		return nil
	}
	out := make([]time.Time, len(numbers))
	for index, number := range numbers {
		out[index] = dayOfNumber(number)
	}
	return out
}

// DayStatus is one stored day's state, and for a blocked day when it was last tried.
type DayStatus struct {
	Day     time.Time
	State   DayState
	TriedAt time.Time
}

// Selection is what SelectDays reads: the moment of the run, the repository's start day, and its
// stored days' states. A day from the start day that holds no state, or only a fingerprint
// (replayed), waits once it is 33 days old.
type Selection struct {
	Now      time.Time
	StartDay time.Time
	Days     []DayStatus
}

// SelectDays is the days a run measures, in order, or none when no run starts. D0, its first, is
// the oldest marked day; else, with at least seven days waiting, the oldest waiting day; else the
// oldest blocked day last tried at least seven days before, so a blocked day is tried at most
// once a week and never ahead of waiting days. The run measures every marked, waiting or due
// blocked day of D0 to D0 + 27. No day before the start day is selected.
func SelectDays(in Selection) []time.Time {
	start := dayNumber(in.StartDay)
	newestWaiting := dayNumber(in.Now) - waitDays
	states := make(map[int64]DayStatus, len(in.Days))
	eligible := map[int64]bool{}
	var marked, due []int64
	for _, status := range in.Days {
		number := dayNumber(status.Day)
		if number < start {
			continue
		}
		states[number] = status
		switch {
		case status.State == DayMarked:
			marked = append(marked, number)
			eligible[number] = true
		case status.State == DayBlocked && !in.Now.Before(status.TriedAt.Add(retryAfter)):
			due = append(due, number)
			eligible[number] = true
		}
	}
	var waiting []int64
	for number := start; number <= newestWaiting; number++ {
		if status, stored := states[number]; !stored || status.State == DayReplayed {
			waiting = append(waiting, number)
			eligible[number] = true
		}
	}
	var first int64
	switch {
	case len(marked) > 0:
		first = slices.Min(marked)
	case len(waiting) >= minWaiting:
		first = waiting[0]
	case len(due) > 0:
		first = slices.Min(due)
	default:
		return nil
	}
	var selected []time.Time
	for number := first; number < first+runDays; number++ {
		if eligible[number] {
			selected = append(selected, dayOfNumber(number))
		}
	}
	return selected
}
