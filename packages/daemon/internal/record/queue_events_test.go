package record

import "testing"

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
