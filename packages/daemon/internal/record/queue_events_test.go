package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// A notified move of the merge queue needs one more event of its direction on GitHub's timeline
// than the store holds only where it changes where the pull request stands: an entry of one out of
// the queue, an exit of one in it. Where the pull request already stands where the move leaves it,
// the notification is a duplicate or its timeline read came first, and a read that waited for one
// more event would wait for what GitHub never writes.
func TestAQueueMoveNeedsOneMoreEventOnlyWhereItChangesWhereThePullRequestStands(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		progress                QueueProgress
		wantEntry, wantExit     int
		entryReason, exitReason string
	}{
		{name: "nothing is kept", progress: QueueProgress{}, wantEntry: 1, wantExit: 1,
			entryReason: "the first entry must show", exitReason: "an exit of a pull request the store has not seen enter must show one"},
		{name: "in the queue after its entry", progress: QueueProgress{Entered: 1, Open: true}, wantEntry: 1, wantExit: 1,
			entryReason: "a second notice of the entry it holds", exitReason: "its first exit must show"},
		{name: "out of the queue after an entry and a removal", progress: QueueProgress{Entered: 1, Exited: 1}, wantEntry: 2, wantExit: 1,
			entryReason: "a re-add is a second entry", exitReason: "a second notice of the exit it holds"},
		{name: "in the queue again after a re-add", progress: QueueProgress{Entered: 2, Exited: 1, Open: true}, wantEntry: 2, wantExit: 2,
			entryReason: "a second notice of the re-add it holds", exitReason: "the second exit must show"},
		{name: "out of the queue after a merge and its removal", progress: QueueProgress{Entered: 1, Exited: 2}, wantEntry: 2, wantExit: 2,
			entryReason: "a re-add is a second entry", exitReason: "the merge and the removal it holds are both exits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.progress.Needs(true); got != tc.wantEntry {
				t.Errorf("an entry needs %d, want %d: %s", got, tc.wantEntry, tc.entryReason)
			}
			if got := tc.progress.Needs(false); got != tc.wantExit {
				t.Errorf("an exit needs %d, want %d: %s", got, tc.wantExit, tc.exitReason)
			}
		})
	}
}

// A timeline read judges whether GitHub's timeline shows the notified move by one rule of two. A
// read of a pull request the store had kept moves of counts the timeline's events in the move's
// direction against MinCount, whatever the newest of them is. An Unkept read, whose MinCount of 1
// the moves before the store kept any already meet, takes the move as shown only where the
// timeline's newest move is in its direction. Either answers how many events it counted, and says
// what the timeline shows short of the move where it does not show it.
func TestAQueueTimelineReadShowsItsMoveByTheCountOrByTheNewestMove(t *testing.T) {
	t.Parallel()
	enter, exit := signals.QueueEvent{Enters: true}, signals.QueueEvent{}
	for _, tc := range []struct {
		name       string
		read       QueueTimelineRead
		events     []signals.QueueEvent
		wantShown  int
		wantBehind string
	}{
		{name: "a kept re-add whose second entry shows",
			read:   QueueTimelineRead{Move: QueueEnqueued, MinCount: 2},
			events: []signals.QueueEvent{enter, exit, enter}, wantShown: 2},
		{name: "a kept re-add whose timeline holds only the first entry",
			read:   QueueTimelineRead{Move: QueueEnqueued, MinCount: 2},
			events: []signals.QueueEvent{enter, exit}, wantShown: 1,
			wantBehind: "shows 1 of the 2 enqueued moves it should"},
		{name: "a kept entry whose count is met although the newest move is an exit",
			read:   QueueTimelineRead{Move: QueueEnqueued, MinCount: 1},
			events: []signals.QueueEvent{enter, exit}, wantShown: 1},
		{name: "a kept merge counted among the exits",
			read:   QueueTimelineRead{Move: QueueMerged, MinCount: 2},
			events: []signals.QueueEvent{enter, exit, enter, exit}, wantShown: 2},
		{name: "a kept exit on an empty timeline",
			read:       QueueTimelineRead{Move: QueueDequeued, MinCount: 1},
			wantBehind: "shows 0 of the 1 dequeued moves it should"},
		{name: "an unkept entry whose timeline ends in it",
			read:   QueueTimelineRead{Move: QueueEnqueued, MinCount: 1, Unkept: true},
			events: []signals.QueueEvent{enter, exit, enter}, wantShown: 2},
		{name: "an unkept entry whose timeline still ends in an earlier exit",
			read:   QueueTimelineRead{Move: QueueEnqueued, MinCount: 1, Unkept: true},
			events: []signals.QueueEvent{enter, exit}, wantShown: 1,
			wantBehind: "ends in no move of the enqueued notification's direction, and the store had kept no move of it"},
		{name: "an unkept exit whose timeline ends in it",
			read:   QueueTimelineRead{Move: QueueDequeued, MinCount: 1, Unkept: true},
			events: []signals.QueueEvent{enter, exit}, wantShown: 1},
		{name: "an unkept merge on an empty timeline",
			read:       QueueTimelineRead{Move: QueueMerged, MinCount: 1, Unkept: true},
			wantBehind: "ends in no move of the merged notification's direction, and the store had kept no move of it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shown, behind := tc.read.Shows(tc.events)
			if shown != tc.wantShown || behind != tc.wantBehind {
				t.Errorf("Shows(%+v) = %d, %q, want %d, %q", tc.events, shown, behind, tc.wantShown, tc.wantBehind)
			}
		})
	}
}
