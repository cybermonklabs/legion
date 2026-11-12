package projection

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every member of the waiting line is given the reason for its place in words
// (api.WaitingLineView.Reason), and the five sentences below are what the five kind-and-part
// pairs read, word for word: the three kinds of the returned part and the two of the starting
// one. The render is a function of the view beside it and the length of the other part
// (waitingLineReason), so the views are built here and no database is read.
//
// The five pairs do not reach five of the rules the sentence keeps, so each has a case of its
// own: a member holding a slot, which waits for no free slot and is told so whatever its kind;
// the ordinals 11th and 12th, which take `th` where the digit before them would say `st` and
// `nd`; a part holding one issue, whose count reads `1 issue` with its verb in the singular; and
// a starting member with nothing in the other part, whose sentence ends after the rank clause
// because there is nothing standing ahead of it to name.
func TestWaitingLineReasonNamesTheMembersPlaceInWords(t *testing.T) {
	cameBack := time.Date(2026, 11, 10, 15, 11, 32, 0, time.UTC)
	answeredAt := time.Date(2026, 11, 10, 17, 8, 0, 0, time.UTC)
	holds, waits := true, false
	for _, tc := range []struct {
		name      string
		view      api.WaitingLineView
		otherPart int
		want      string
	}{
		{
			name: "an owed start in the returned part",
			view: api.WaitingLineView{Part: "returned", Kind: "returning", PartPlace: 2, PartLength: 7,
				HoldsSlot: &waits, Since: &cameBack},
			otherPart: 18,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 2nd of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name: "an answered root in the returned part",
			view: api.WaitingLineView{Part: "returned", Kind: "answered", PartPlace: 3, PartLength: 7,
				HoldsSlot: &waits, Since: &answeredAt},
			otherPart: 18,
			want: "A person answered it, and its work came back at 2026-11-10T17:08:00Z. " +
				"It is 3rd of 7 issues whose work came back, and they take the free slots first.",
		},
		{
			name: "a tree root's own first phase in the returned part",
			view: api.WaitingLineView{Part: "returned", Kind: "first_phase", PartPlace: 1, PartLength: 7,
				HoldsSlot: &waits, Since: &cameBack},
			otherPart: 18,
			want: "Its design gate opened at 2026-11-10T15:11:32Z. It is 1st of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name: "a queued root in the starting part",
			view: api.WaitingLineView{Part: "starting", Kind: "queued_root", PartPlace: 4, PartLength: 18,
				HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
			otherPart: 7,
			want: "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank, " +
				"and 7 issues whose work came back stand ahead of all of them.",
		},
		{
			name: "a child's first phase in the starting part",
			view: api.WaitingLineView{Part: "starting", Kind: "first_phase", PartPlace: 5, PartLength: 18,
				HoldsSlot: &waits, RootRank: "M", OwnRank: "N", RootKey: "LEGION-10"},
			otherPart: 7,
			want: "It has not started. It is 5th of 18 issues that have not started, " +
				"by the Dispatch rank of its tree root LEGION-10, and 7 issues whose work came back stand ahead of all of them.",
		},
		// A holder takes no free slot and waits for none, so the clause about the free slots is
		// replaced by that truth — read from holdsSlot and never from the kind, since every kind
		// can hold a slot and every kind can stand in the line without one.
		{
			name: "an answered holder of the returned part",
			view: api.WaitingLineView{Part: "returned", Kind: "answered", PartPlace: 1, PartLength: 7,
				HoldsSlot: &holds, Since: &answeredAt},
			otherPart: 18,
			want: "A person answered it, and its work came back at 2026-11-10T17:08:00Z. " +
				"It is 1st of 7 issues whose work came back. It already holds its admission slot, " +
				"so it resumes at the next pass whatever the cap says.",
		},
		{
			name: "a queued root holding a slot",
			view: api.WaitingLineView{Part: "starting", Kind: "queued_root", PartPlace: 4, PartLength: 18,
				HoldsSlot: &holds, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
			otherPart: 7,
			want: "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank. " +
				"It already holds its admission slot, so it resumes at the next pass whatever the cap says.",
		},
		// 11 and 12 take `th`, where 1 and 2 take `st` and `nd`: the teens are the rule the
		// last digit alone gets wrong.
		{
			name: "the eleventh of its part",
			view: api.WaitingLineView{Part: "returned", Kind: "returning", PartPlace: 11, PartLength: 13,
				HoldsSlot: &waits, Since: &cameBack},
			otherPart: 2,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 11th of 13 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name: "the twelfth of its part",
			view: api.WaitingLineView{Part: "starting", Kind: "queued_root", PartPlace: 12, PartLength: 21,
				HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
			otherPart: 2,
			want: "It has not started. It is 12th of 21 issues that have not started, by its own Dispatch rank, " +
				"and 2 issues whose work came back stand ahead of all of them.",
		},
		// A count of 1 reads `1 issue` with its verb in the singular, in the part the member
		// stands in and in the part it waits behind alike.
		{
			name: "the one issue of the returned part",
			view: api.WaitingLineView{Part: "returned", Kind: "returning", PartPlace: 1, PartLength: 1,
				HoldsSlot: &waits, Since: &cameBack},
			otherPart: 18,
			want: "Its work came back at 2026-11-10T15:11:32Z. It is 1st of 1 issue whose work came back, " +
				"and it takes the free slots first.",
		},
		{
			name: "the one issue of the starting part, behind one issue whose work came back",
			view: api.WaitingLineView{Part: "starting", Kind: "queued_root", PartPlace: 1, PartLength: 1,
				HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
			otherPart: 1,
			want: "It has not started. It is 1st of 1 issue that has not started, by its own Dispatch rank, " +
				"and 1 issue whose work came back stands ahead of all of them.",
		},
		// Nothing has come back, so the sentence ends after the rank clause: there is no other
		// part to stand ahead of this one.
		{
			name: "a starting member with the other part empty",
			view: api.WaitingLineView{Part: "starting", Kind: "queued_root", PartPlace: 4, PartLength: 18,
				HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
			otherPart: 0,
			want:      "It has not started. It is 4th of 18 issues that have not started, by its own Dispatch rank.",
		},
		// A member of the returned part whose record knows no moment names none: the clause ends
		// at its verb (record.TreeGate.OpenedAt, which a gate registered from no event leaves
		// unset).
		{
			name: "an owed start with no moment",
			view: api.WaitingLineView{Part: "returned", Kind: "returning", PartPlace: 2, PartLength: 7,
				HoldsSlot: &waits},
			otherPart: 18,
			want:      "Its work came back. It is 2nd of 7 issues whose work came back, and they take the free slots first.",
		},
		{
			name: "an answered root with no moment",
			view: api.WaitingLineView{Part: "returned", Kind: "answered", PartPlace: 3, PartLength: 7,
				HoldsSlot: &waits},
			otherPart: 18,
			want: "A person answered it, and its work came back. It is 3rd of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
		{
			name: "a design gate with no moment",
			view: api.WaitingLineView{Part: "returned", Kind: "first_phase", PartPlace: 1, PartLength: 7,
				HoldsSlot: &waits},
			otherPart: 18,
			want: "Its design gate opened. It is 1st of 7 issues whose work came back, " +
				"and they take the free slots first.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingLineReason(tc.view, tc.otherPart); got != tc.want {
				t.Fatalf("waitingLineReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every number in a member's sentence is that member's own: its place within its part, the
// length of that part and the length of the other part, read from the view beside it and the
// argument and from no count of the line held anywhere else. Two members of one line are
// rendered here with no fact in common but the lengths, and each sentence carries its own place
// and neither carries the other's.
//
// The ordinals are checked past the teens as well, where the suffix follows the last digit
// again: 21st, 22nd, 23rd, 101st, and 111th, 112th and 113th, which take `th` as 11, 12 and 13
// do (englishOrdinal).
func TestWaitingLineReasonCountsNothingBeyondItsOwnEntry(t *testing.T) {
	cameBack := time.Date(2026, 11, 10, 15, 11, 32, 0, time.UTC)
	waits := false
	returned := api.WaitingLineView{Part: "returned", Kind: "returning", PartPlace: 2, PartLength: 3,
		HoldsSlot: &waits, Since: &cameBack}
	starting := api.WaitingLineView{Part: "starting", Kind: "first_phase", PartPlace: 1, PartLength: 2,
		HoldsSlot: &waits, RootRank: "M", OwnRank: "N", RootKey: "LEGION-10"}
	if got, want := waitingLineReason(returned, 2), "Its work came back at 2026-11-10T15:11:32Z. "+
		"It is 2nd of 3 issues whose work came back, and they take the free slots first."; got != want {
		t.Fatalf("the returned member's reason = %q, want %q", got, want)
	}
	if got, want := waitingLineReason(starting, 3), "It has not started. It is 1st of 2 issues that have not started, "+
		"by the Dispatch rank of its tree root LEGION-10, and 3 issues whose work came back stand ahead of all of them."; got != want {
		t.Fatalf("the starting member's reason = %q, want %q", got, want)
	}
	// The same two members with every number moved: the sentence moves with them and with
	// nothing else.
	moved := returned
	moved.PartPlace, moved.PartLength = 3, 4
	if got := waitingLineReason(moved, 2); !strings.Contains(got, "It is 3rd of 4 issues whose work came back") {
		t.Fatalf("the moved member's reason = %q, want its own place of its own part", got)
	}
	behind := starting
	behind.PartPlace, behind.PartLength = 2, 5
	if got := waitingLineReason(behind, 1); !strings.Contains(got, "It is 2nd of 5 issues that have not started") ||
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
	// A part or a kind this release has no word for renders no sentence at all, so a member of
	// it is served no reason rather than one about a line it does not stand in
	// (record.LinePart.String, record.LineKind.String give the empty string for such a value).
	for _, view := range []api.WaitingLineView{
		{Part: record.LinePart(0).String(), Kind: "returning", PartPlace: 1, PartLength: 1},
		{Part: "returned", Kind: record.LineKind(0).String(), PartPlace: 1, PartLength: 1},
		{Part: "starting", Kind: "answered", PartPlace: 1, PartLength: 1},
	} {
		if got := waitingLineReason(view, 1); got != "" {
			t.Errorf("waitingLineReason of %q as %q = %q, want no sentence at all", view.Part, view.Kind, got)
		}
	}
}
