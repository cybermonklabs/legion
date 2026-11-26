package survival

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Commit is one first-parent commit of a branch: its SHA, its parents (the first parent first),
// its committer time, and the facts of the pull request it merged (a zero PR for a commit that
// merged none).
type Commit struct {
	SHA         string
	Parents     []string
	CommittedAt time.Time
	Change      ChangeFacts
}

// Patch is one commit's patch as Measure reads it: its file diffs, or, where the commit's patch
// could not be read whole, how many of its files were withheld. A patch with withheld files
// blocks the selected days whose 30 days hold its commit, and its files are not applied.
type Patch struct {
	Files    []FileDiff
	Withheld int
}

// MeasureInput is what Measure reads: the selected days (UTC midnights), the first-parent commits
// of their replay range in chain order (ReplayRange), and Patch, which answers the patch of
// Chain[i]. Measure calls Patch once for each commit, in chain order, and keeps none of a patch
// once it asks for the next: the caller can release each as Measure takes it.
type MeasureInput struct {
	Days  []time.Time
	Chain []Commit
	Patch func(i int) (Patch, error)
}

// Change is one measured change: the commit it landed as, its day, its committer time, its pull
// request's facts, the lines it added, the lines of those alive at its 30-day mark, and the lines
// of those that commits which merged no pull request removed at or before its mark.
type Change struct {
	SHA         string
	Day         time.Time
	CommittedAt time.Time
	ChangeFacts
	Added          int
	AliveAtMark    int
	RemovedByNonPR int
}

// Pair is a measured change (the adder) and a later change that removed some of its lines at or
// before the adder's mark (the remover): the remover's facts, the lines it removed, and the days
// between the two commits' committer times (below 0 for a remover dated before its adder).
type Pair struct {
	AdderSHA    string
	RemoverSHA  string
	Remover     ChangeFacts
	Lines       int
	DaysBetween float64
}

// DayResult is one selected day's state after a measurement: measured, or blocked by the first
// commit in chain order whose patch could not be read whole, with how many files it withheld.
type DayResult struct {
	Day          time.Time
	State        DayState
	BlockedSHA   string
	BlockedFiles int
}

// Fingerprint is one day's fingerprint (R8): the count of the first-parent commits dated that day
// and the sha256 of their SHAs in chain order, joined by "\n".
type Fingerprint struct {
	Day     time.Time
	Commits int
	Digest  [32]byte
}

// MeasureOutput is what Measure answers: each change of a measured day and each of its pairs, in
// chain order; the fingerprint of every day it replayed (the selected days' oldest to the 30 days
// after their newest); each selected day's state, in day order; and the most tracked lines the
// replay held at once.
type MeasureOutput struct {
	Changes      []Change
	Pairs        []Pair
	Fingerprints []Fingerprint
	Days         []DayResult
	PeakLines    int
}

// Measure measures the changes of the selected days, a pure function of its input that streams
// the patches: it replays every commit of the range in chain order, tracking the lines of the
// changes dated on a selected day, and reads each change's lines at its 30-day mark as
// scripts/replaylib.py's alive_at does (a removal by a commit with a committer time at or before
// the mark counts), and each removal as a pair or, by a commit that merged no pull request, as the
// change's removed_by_non_pr. A commit whose patch could not be read whole blocks every selected
// day of its day − 31 to its day: the replay drops those days' lines and goes on for the rest.
func Measure(in MeasureInput) (MeasureOutput, error) {
	if len(in.Days) == 0 {
		return MeasureOutput{}, errors.New("survival: no day to measure")
	}
	if in.Patch == nil {
		return MeasureOutput{}, errors.New("survival: no patch to read")
	}
	numbers := make([]int64, len(in.Days))
	for index, day := range in.Days {
		if !day.Equal(DayOf(day)) {
			return MeasureOutput{}, fmt.Errorf("survival: the day %s is no UTC midnight", day)
		}
		numbers[index] = dayNumber(day)
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	m := measurement{in: in, replay: NewReplay(), selected: map[int64]bool{}, changes: map[int64][]string{},
		blocked: map[int64]DayResult{}}
	for _, number := range numbers {
		m.selected[number] = true
	}
	for _, commit := range in.Chain {
		if number := dayNumber(commit.CommittedAt); commit.Change.PR != 0 && m.selected[number] {
			m.replay.Track(commit.SHA)
			m.changes[number] = append(m.changes[number], commit.SHA)
		}
	}
	for index := range in.Chain {
		if err := m.step(index); err != nil {
			return MeasureOutput{}, err
		}
	}
	out := m.results()
	out.Fingerprints = Fingerprints(in.Chain, dayOfNumber(numbers[0]), dayOfNumber(numbers[len(numbers)-1]+tailDays))
	for _, number := range numbers {
		state, blocked := m.blocked[number]
		if !blocked {
			state = DayResult{Day: dayOfNumber(number), State: DayMeasured}
		}
		out.Days = append(out.Days, state)
	}
	return out, nil
}

// measurement is one Measure's state.
type measurement struct {
	in       MeasureInput
	replay   *Replay
	selected map[int64]bool
	// changes is the SHAs of each selected day's changes.
	changes map[int64][]string
	blocked map[int64]DayResult
}

// step reads and applies the patch of the commit at index; in a frame of its own, so the patch
// is no longer reachable from Measure once it returns.
func (m *measurement) step(index int) error {
	commit := m.in.Chain[index]
	patch, err := m.in.Patch(index)
	if err != nil {
		return fmt.Errorf("survival: the patch of %s: %w", commit.SHA, err)
	}
	if patch.Withheld > 0 {
		m.block(commit, patch.Withheld)
		patch.Files = nil
	}
	m.replay.Apply(commit, patch.Files)
	return nil
}

// block blocks every selected day whose 30 days hold commit, which withheld files, and drops
// their changes' lines; a day a commit before it blocked stays blocked by that one.
func (m *measurement) block(commit Commit, files int) {
	last := dayNumber(commit.CommittedAt)
	for number := last - blockDays; number <= last; number++ {
		if _, done := m.blocked[number]; done || !m.selected[number] {
			continue
		}
		m.blocked[number] = DayResult{Day: dayOfNumber(number), State: DayBlocked, BlockedSHA: commit.SHA,
			BlockedFiles: files}
		m.replay.Drop(m.changes[number]...)
	}
}

// results is each change of a measured day, read at its mark, and its pairs, in chain order.
func (m *measurement) results() MeasureOutput {
	out := MeasureOutput{PeakLines: m.replay.PeakLines()}
	for _, commit := range m.in.Chain {
		number := dayNumber(commit.CommittedAt)
		if _, blocked := m.blocked[number]; commit.Change.PR == 0 || !m.selected[number] || blocked {
			continue
		}
		mark := Mark(commit.CommittedAt)
		change := Change{SHA: commit.SHA, Day: dayOfNumber(number), CommittedAt: commit.CommittedAt,
			ChangeFacts: commit.Change, Added: m.replay.Added(commit.SHA)}
		change.AliveAtMark = change.Added
		for _, removal := range m.replay.removals(commit.SHA) {
			remover := m.in.Chain[removal.By]
			if remover.CommittedAt.After(mark) {
				continue
			}
			change.AliveAtMark -= removal.Lines
			if remover.Change.PR == 0 {
				change.RemovedByNonPR += removal.Lines
				continue
			}
			out.Pairs = append(out.Pairs, Pair{AdderSHA: commit.SHA, RemoverSHA: remover.SHA,
				Remover: remover.Change, Lines: removal.Lines,
				DaysBetween: float64(remover.CommittedAt.Sub(commit.CommittedAt)) / float64(oneDay)})
		}
		out.Changes = append(out.Changes, change)
	}
	return out
}
