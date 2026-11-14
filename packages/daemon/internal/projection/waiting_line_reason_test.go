package projection

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// lineEntry is one entry of this test's line: the part it stands in, the kind that put it
// there, its place within that part, whether it holds a slot and the issue it is of, which is
// the record every clause that names a key reads (record.LineEntry). The sentence is rendered
// from the entry and the two part lengths and from nothing else (waitingLineReason), so the
// entries are built here and no database is read.
func lineEntry(part record.LinePart, kind record.LineKind, place int, holds bool, tree string) record.LineEntry {
	return record.LineEntry{Issue: record.Issue{Key: "LEGION-1", Tree: tree, Rank: "N"},
		Kind: kind, Part: part, PartPlace: place, Holds: holds}
}

// Every member of the waiting line is given the reason for its place in words
// (api.WaitingLineView.Reason), and the seven sentences below are what the kind-and-part pairs
// read, word for word: the four kinds of the returned part, the waiting-on-issue kind once with
// its wait still open and once with it ended (its entry's Waits empty), and the two kinds of the
// starting one.
//
// Those pairs do not reach every rule the sentence keeps, so each other rule has a case of its
// own: a member holding a slot, which waits for no free slot and is told so whatever its kind;
// the ordinals 11th and 12th, which take `th` where the digit before them would say `st` and
// `nd`; a part holding one issue, whose count reads `1 issue` with its verb in the singular; and
// a starting member with nothing in the other part, whose sentence ends after the rank clause
// because there is nothing standing ahead of it to name.
func TestWaitingLineReasonNamesTheMembersPlaceInWords(t *testing.T) {
	cameBack := time.Date(2026, 11, 10, 15, 11, 32, 0, time.UTC)
	answeredAt := time.Date(2026, 11, 10, 17, 8, 0, 0, time.UTC)
	since := func(entry record.LineEntry, at time.Time) record.LineEntry {
		entry.Since = at
		return entry
	}
	for _, tc := range []struct {
		name       string
		entry      record.LineEntry
		partLength int
		otherPart  int
		want       string
	}{
		{
			name:       "an owed start in the returned part",
			entry:      since(lineEntry(record.PartReturned, record.LineReturning, 2, false, "LEGION-10"), cameBack),
			partLength: 7,
			otherPart:  18,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 2nd of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name:       "an answered root in the returned part",
			entry:      since(lineEntry(record.PartReturned, record.LineAnswered, 3, false, "LEGION-10"), answeredAt),
			partLength: 7,
			otherPart:  18,
			want: "A person answered it, and its work came back at 2026-11-10T17:08:00Z. " +
				"It is 3rd of 7 issues whose work came back, and they take the free slots first.",
		},
		{
			name:       "a tree root's own first phase in the returned part",
			entry:      since(lineEntry(record.PartReturned, record.LineFirstPhase, 1, false, "LEGION-10"), cameBack),
			partLength: 7,
			otherPart:  18,
			want: "Its design gate opened at 2026-11-10T15:11:32Z. It is 1st of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name:       "a queued root in the starting part",
			entry:      lineEntry(record.PartStarting, record.LineQueuedRoot, 4, false, "LEGION-20"),
			partLength: 18,
			otherPart:  7,
			want: "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank, " +
				"and 7 issues whose work came back stand ahead of all of them.",
		},
		{
			name:       "a child's first phase in the starting part",
			entry:      lineEntry(record.PartStarting, record.LineFirstPhase, 5, false, "LEGION-10"),
			partLength: 18,
			otherPart:  7,
			want: "It has not started. It is 5th of 18 issues that have not started, " +
				"by the Dispatch rank of its tree root LEGION-10, and 7 issues whose work came back stand ahead of all of them.",
		},
		// A member that gave its slot back to wait on another issue is passed over while it
		// waits, so it is told when it takes a slot again rather than that it takes them first.
		{
			name: "a started issue waiting on another issue in the returned part",
			entry: func() record.LineEntry {
				entry := since(lineEntry(record.PartReturned, record.LineWaitingOnIssue, 2, false, "LEGION-10"), cameBack)
				entry.Waits = []string{"LEGION-7", "LEGION-8"}
				return entry
			}(),
			partLength: 3,
			otherPart:  18,
			want: "It gave its admission slot back to wait on LEGION-7, LEGION-8 at 2026-11-10T15:11:32Z. " +
				"It is 2nd of 3 issues whose work came back. The promotion passes over it while that wait lasts, " +
				"and after the wait it takes a free slot again by itself.",
		},
		// The same kind with no wait left: its blockers closed while the cap was full, so it is
		// told the wait has ended and that it takes the free slots first.
		{
			name:       "a started issue whose wait on another issue has ended",
			entry:      since(lineEntry(record.PartReturned, record.LineWaitingOnIssue, 1, false, "LEGION-10"), cameBack),
			partLength: 1,
			otherPart:  18,
			want: "It gave its admission slot back to wait on another issue at 2026-11-10T15:11:32Z, and that wait has ended. " +
				"It is 1st of 1 issue whose work came back, and it takes the free slots first.",
		},
		// A holder takes no free slot and waits for none, so the clause about the free slots is
		// replaced by that truth — read from the entry's Holds and never from the kind, since
		// every kind can hold a slot and every kind can stand in the line without one.
		{
			name:       "an answered holder of the returned part",
			entry:      since(lineEntry(record.PartReturned, record.LineAnswered, 1, true, "LEGION-10"), answeredAt),
			partLength: 7,
			otherPart:  18,
			want: "A person answered it, and its work came back at 2026-11-10T17:08:00Z. " +
				"It is 1st of 7 issues whose work came back. It already holds its admission slot, " +
				"so it resumes at the next pass whatever the cap says.",
		},
		{
			name:       "a queued root holding a slot",
			entry:      lineEntry(record.PartStarting, record.LineQueuedRoot, 4, true, "LEGION-20"),
			partLength: 18,
			otherPart:  7,
			want: "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank. " +
				"It already holds its admission slot, so it resumes at the next pass whatever the cap says.",
		},
		// 11 and 12 take `th`, where 1 and 2 take `st` and `nd`: the teens are the rule the
		// last digit alone gets wrong.
		{
			name:       "the eleventh of its part",
			entry:      since(lineEntry(record.PartReturned, record.LineReturning, 11, false, "LEGION-10"), cameBack),
			partLength: 13,
			otherPart:  2,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 11th of 13 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name:       "the twelfth of its part",
			entry:      lineEntry(record.PartStarting, record.LineQueuedRoot, 12, false, "LEGION-20"),
			partLength: 21,
			otherPart:  2,
			want: "It has not started. It is 12th of 21 issues that have not started, by its own Dispatch rank, " +
				"and 2 issues whose work came back stand ahead of all of them.",
		},
		// A count of 1 reads `1 issue` with its verb in the singular, in the part the member
		// stands in and in the part it waits behind alike.
		{
			name:       "the one issue of the returned part",
			entry:      since(lineEntry(record.PartReturned, record.LineReturning, 1, false, "LEGION-10"), cameBack),
			partLength: 1,
			otherPart:  18,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 1st of 1 issue whose work came back, " +
				"and it takes the free slots first.",
		},
		{
			name:       "the one issue of the starting part, behind one issue whose work came back",
			entry:      lineEntry(record.PartStarting, record.LineQueuedRoot, 1, false, "LEGION-20"),
			partLength: 1,
			otherPart:  1,
			want: "It has not started. It is 1st of 1 issue that has not started, by its own Dispatch rank, " +
				"and 1 issue whose work came back stands ahead of all of them.",
		},
		// Nothing has come back, so the sentence ends after the rank clause: there is no other
		// part to stand ahead of this one.
		{
			name:       "a starting member with the other part empty",
			entry:      lineEntry(record.PartStarting, record.LineQueuedRoot, 4, false, "LEGION-20"),
			partLength: 18,
			otherPart:  0,
			want:       "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank.",
		},
		// A member of the returned part whose record knows no moment names none: the clause ends
		// at its verb (record.TreeGate.OpenedAt, which a gate registered from no event leaves
		// unset).
		{
			name:       "an owed start with no moment",
			entry:      lineEntry(record.PartReturned, record.LineReturning, 2, false, "LEGION-10"),
			partLength: 7,
			otherPart:  18,
			want:       "Its work came back. It is 2nd of 7 issues whose work came back, and they take the free slots first.",
		},
		{
			name:       "an answered root with no moment",
			entry:      lineEntry(record.PartReturned, record.LineAnswered, 3, false, "LEGION-10"),
			partLength: 7,
			otherPart:  18,
			want: "A person answered it, and its work came back. It is 3rd of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name:       "a design gate with no moment",
			entry:      lineEntry(record.PartReturned, record.LineFirstPhase, 1, false, "LEGION-10"),
			partLength: 7,
			otherPart:  18,
			want: "Its design gate opened. It is 1st of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingLineReason(tc.entry, tc.partLength, tc.otherPart); got != tc.want {
				t.Fatalf("waitingLineReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every number in a member's sentence is that member's own: its place within its part, the
// length of that part and the length of the other part, read from the entry beside it and the
// two arguments and from no count of the line held anywhere else. Two members of one line are
// rendered here with no fact in common but the lengths, and each sentence carries its own place
// and neither carries the other's.
//
// The ordinals are checked past the teens as well, where the suffix follows the last digit
// again: 21st, 22nd, 23rd, 101st, and 111th, 112th and 113th, which take `th` as 11, 12 and 13
// do (englishOrdinal).
func TestWaitingLineReasonCountsNothingBeyondItsOwnEntry(t *testing.T) {
	cameBack := time.Date(2026, 11, 10, 15, 11, 32, 0, time.UTC)
	returned := record.LineEntry{Issue: record.Issue{Key: "LEGION-12", Tree: "LEGION-10", Rank: "O"},
		Kind: record.LineReturning, Part: record.PartReturned, PartPlace: 2, Since: cameBack}
	starting := record.LineEntry{Issue: record.Issue{Key: "LEGION-11", Tree: "LEGION-10", Rank: "N"},
		Kind: record.LineFirstPhase, Part: record.PartStarting, PartPlace: 1, RootRank: "M"}
	if got, want := waitingLineReason(returned, 3, 2), "Its work came back at 2026-11-10T15:11:32Z. "+
		"It is 2nd of 3 issues whose work came back, and they take the free slots first."; got != want {
		t.Fatalf("the returned member's reason = %q, want %q", got, want)
	}
	if got, want := waitingLineReason(starting, 2, 3), "It has not started. It is 1st of 2 issues that have not started, "+
		"by the Dispatch rank of its tree root LEGION-10, and 3 issues whose work came back stand ahead of all of them."; got != want {
		t.Fatalf("the starting member's reason = %q, want %q", got, want)
	}
	// The same two members with every number moved: the sentence moves with them and with
	// nothing else.
	moved := returned
	moved.PartPlace = 3
	if got := waitingLineReason(moved, 4, 2); !strings.Contains(got, "It is 3rd of 4 issues whose work came back") {
		t.Fatalf("the moved member's reason = %q, want its own place of its own part", got)
	}
	behind := starting
	behind.PartPlace = 2
	if got := waitingLineReason(behind, 5, 1); !strings.Contains(got, "It is 2nd of 5 issues that have not started") ||
		!strings.Contains(got, "and 1 issue whose work came back stands ahead of all of them.") {
		t.Fatalf("the moved member's reason = %q, want its own place and the one issue ahead of its part", got)
	}
	for place, want := range map[int]string{
		21: "21st", 22: "22nd", 23: "23rd", 101: "101st", 111: "111th", 112: "112th", 113: "113th",
	} {
		if got := englishOrdinal(place); got != want {
			t.Errorf("englishOrdinal(%d) = %q, want %q", place, got, want)
		}
	}
}
