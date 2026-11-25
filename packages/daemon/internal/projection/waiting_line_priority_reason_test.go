package projection

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A member's sentence says the priority that placed it and whose it is: its own, its tree root's
// for a member with none of its own, and, where neither has one,
// that it waits after every issue at P0 to P3. The counts name the band it waits in: `at P0`, or
// `with no priority`.
func TestWaitingLineReasonSaysThePriorityThatPlacedTheMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry record.LineEntry
		want  string
	}{
		{
			name: "a child placed at its tree root's P0",
			entry: record.LineEntry{Issue: record.Issue{Key: "LEGION-11", Tree: "LEGION-10"},
				Kind: record.LineFirstPhase, Priority: record.P0, Part: record.PartStarting, PartPlace: 1},
			want: "It has not started. It waits at P0, the priority of its tree root LEGION-10. " +
				"It is 1st of 2 issues at P0 that have not started, by the Dispatch rank of its tree root LEGION-10, " +
				"and 1 issue at P0 whose work came back stands ahead of all of them.",
		},
		{
			name: "a root with no priority",
			entry: record.LineEntry{Issue: record.Issue{Key: "LEGION-20", Tree: "LEGION-20"},
				Kind: record.LineQueuedRoot, Part: record.PartStarting, PartPlace: 2},
			want: "It has not started. It has no priority, so it waits after every issue at P0 to P3. " +
				"It is 2nd of 2 issues with no priority that have not started, by its own Dispatch rank, " +
				"and 1 issue with no priority whose work came back stands ahead of all of them.",
		},
		{
			name: "a child whose root has no priority either",
			entry: record.LineEntry{Issue: record.Issue{Key: "LEGION-31", Tree: "LEGION-30"},
				Kind: record.LineReturning, Part: record.PartReturned, PartPlace: 1},
			want: "Its work came back. Neither it nor its tree root LEGION-30 has a priority, so it waits after every issue at P0 to P3. " +
				"It is 1st of 2 issues with no priority whose work came back, " +
				"and they take the free slots before the issues with no priority that have not started.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingLineReason(tc.entry, 2, 1); got != tc.want {
				t.Fatalf("waitingLineReason = %q, want %q", got, tc.want)
			}
		})
	}
}
