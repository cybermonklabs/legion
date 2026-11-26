package signals

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"
)

// QueueEvent is one event of a pull request's merge queue timeline, normalized: the pull request by
// its canonical repository and number, the moment of the event, and whether it enters the queue
// (added_to_merge_queue) or leaves it (a merge, or a removal). ID is the event's own id, which a
// caller that reads one event twice, from two files or two deliveries, gives both times: an event
// with an id is counted once, at the first sight of it. 0 names no event, and an event with it is
// never taken for another's.
type QueueEvent struct {
	ID          int64
	Repo        string
	PullRequest int
	At          time.Time
	Enters      bool
}

// pullKey is a pull request of the merge queue.
type pullKey struct {
	repo   string
	number int
}

// MergeQueue is the merge queue's waits as the events seen so far make them, for a caller that
// applies each event as it comes (Apply) in place of folding its history again at each pass. The
// zero value is an empty queue. Events of one pull request are applied in the order of their
// moments; WaitsOf sorts a history into that order.
type MergeQueue struct {
	waits []Wait
	open  map[pullKey]int
	seen  map[int64]struct{}
}

// Apply applies e, and says whether it changed the queue. An event with an id already seen, an
// entry of a pull request that stands in the queue and an exit of one that does not (the removal
// GitHub records beside a merge) change nothing. An entry of a pull request that left makes a new
// wait of its own, so a drop and a re-add are two waits.
func (q *MergeQueue) Apply(e QueueEvent) bool {
	if e.ID > 0 {
		if _, seen := q.seen[e.ID]; seen {
			return false
		}
		if q.seen == nil {
			q.seen = make(map[int64]struct{})
		}
		q.seen[e.ID] = struct{}{}
	}
	return q.fold(e)
}

// fold is the effect of an event on the waits, with no look at its id.
func (q *MergeQueue) fold(e QueueEvent) bool {
	pull := pullKey{repo: e.Repo, number: e.PullRequest}
	open, standing := q.open[pull]
	switch {
	case e.Enters && !standing:
		if q.open == nil {
			q.open = make(map[pullKey]int)
		}
		q.waits = append(q.waits, Wait{Added: e.At})
		q.open[pull] = len(q.waits) - 1
		return true
	case !e.Enters && standing:
		q.waits[open].Left = e.At
		delete(q.open, pull)
		return true
	}
	return false
}

// Waits is each wait the events applied so far make, in the order they began to be applied, a wait
// that stands at the end of the record with its Left zero. The caller must not change it.
func (q *MergeQueue) Waits() []Wait { return q.waits }

// WaitsOf is each wait the history events makes, by the moment it began: the events of every pull
// request together, each event id once, at the first event in the order given that carries it, then
// each pull request's events in the order of their moments, the order given among those of one
// moment, folded as MergeQueue.Apply folds them, pull requests in the order of their repository and
// number. It makes a wait from each entry of a pull request standing in no wait to its next exit.
func WaitsOf(events []QueueEvent) []Wait {
	seen := make(map[int64]bool)
	byPull := make(map[pullKey][]QueueEvent)
	for _, e := range events {
		if e.ID > 0 {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
		}
		pull := pullKey{repo: e.Repo, number: e.PullRequest}
		byPull[pull] = append(byPull[pull], e)
	}
	var q MergeQueue
	for _, pull := range slices.SortedFunc(maps.Keys(byPull), func(a, b pullKey) int {
		return cmp.Or(strings.Compare(a.repo, b.repo), cmp.Compare(a.number, b.number))
	}) {
		history := byPull[pull]
		slices.SortStableFunc(history, func(a, b QueueEvent) int { return a.At.Compare(b.At) })
		for _, e := range history {
			q.fold(e)
		}
	}
	slices.SortStableFunc(q.waits, func(a, b Wait) int { return a.Added.Compare(b.Added) })
	return q.waits
}
