package docs

import "testing"

// appendGateRead.after is how a transaction that has queued writes to a document's history reads
// the gate: the head its queue leaves, and, for a queued rebuild, a history with nothing of the
// reading room's left in it. The rebuild is carried as oldest 0, the value an empty history
// reads, and historyIntact is the only thing that reads oldest; were it to take 0 the other way
// round - no row deleted, so the history is intact - a room standing behind a rebuild this
// transaction queued would be admitted at the head instead of refused (historyReplaced), and
// every case below but the first would fail.
func TestAppendGateReadAfterAQueuedRebuildHoldsNothingOfTheRoom(t *testing.T) {
	// A history standing at version 9 whose oldest stored row is 3: a room holding every version
	// through 5 is behind an intact history, which a browser's update is admitted on.
	stored := appendGateRead{latest: 9, oldest: 3}
	for _, row := range []struct {
		name     string
		read     appendGateRead
		through  int64
		latest   int64
		intact   bool
		replaced bool
	}{
		{name: "no queued write", read: stored.after(nil), through: 5, latest: 9, intact: true},
		{
			name: "appends queued", read: stored.after(&pendingRoom{head: 12, appended: 3}),
			through: 5, latest: 12, intact: true,
		},
		{
			name:    "a rebuild queued",
			read:    stored.after(&pendingRoom{head: 10, appended: 1, rebuilt: true}),
			through: 5, latest: 10, intact: false, replaced: true,
		},
		{
			name:    "a rebuild queued, a room at the head it leaves",
			read:    stored.after(&pendingRoom{head: 10, appended: 1, rebuilt: true}),
			through: 10, latest: 10, intact: false, replaced: true,
		},
		{
			name:    "a rebuild queued, a room holding nothing",
			read:    stored.after(&pendingRoom{head: 10, appended: 1, rebuilt: true}),
			through: 0, latest: 10, intact: false, replaced: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.read.latest != row.latest {
				t.Errorf("the read's head after the queue is %d, want %d", row.read.latest, row.latest)
			}
			if intact := row.read.historyIntact(row.through); intact != row.intact {
				t.Errorf("a room holding every version through %d reads the history as intact=%t, want %t",
					row.through, intact, row.intact)
			}
			if replaced := historyReplaced(row.through, row.read.latest, row.read.historyIntact(row.through)); replaced != row.replaced {
				t.Errorf("a room holding every version through %d reads the history as replaced=%t, want %t",
					row.through, replaced, row.replaced)
			}
		})
	}
}
