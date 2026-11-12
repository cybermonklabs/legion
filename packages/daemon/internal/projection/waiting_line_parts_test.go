package projection

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every issue `admission.waiting` names carries the line's own fields beside its place: the part
// it waits in and why it waits there, in the words the record gives the wire
// (record.LinePart.String, record.LineKind.String), its place within that part, the length of
// that part, whether it holds one of the cap's slots, and what placed it — the moment its work
// came back for a member of the returned part, the Dispatch ranks and the tree root's key for a
// member of the starting part. The part's length is counted over the whole line this pass holds,
// so the two parts of a five-member line answer 3 and 2 and never the length of the line.
//
// The fixture is internal/record's TestWaitingLinePartAndKeyOnEveryEntry written for this
// package, so the projected view and the entries it is read from say the same thing about the
// same facts: an owed start, an answered root and a tree root's own first phase in the returned
// part, and a queued root and a child's first phase in the starting one.
func TestWaitingLinePartAndKeyReachTheProjectedView(t *testing.T) {
	opened, owedAt, answeredAt := projectedAt.Add(-3*time.Hour), projectedAt.Add(-2*time.Hour), projectedAt.Add(-time.Hour)
	approved := 1
	answered := lineIssue("LEGION-40", "LEGION-40", "in_progress", phase.AwaitingPerson, "D")
	answered.Awaiting = &record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 9},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByReply}
	store := projectionStore{
		issues: []record.Issue{
			lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
			lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
			lineIssue("LEGION-12", "LEGION-10", "retro", phase.Merging, "O"),
			lineIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A"),
			answered,
		},
		lifecycles: []string{"LEGION-10"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-10": {record.DesignStepSpec: {Issue: "LEGION-10", Step: record.DesignStepSpec, LatestVersion: 1,
				ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}},
		},
		owed: []record.OwedStart{{Issue: "LEGION-12", ID: 42, CreatedAt: owedAt}},
		// The root whose gate opened and the answered root each kept the row they hold through
		// the wait (record.KeepsSlot); the other three members hold none.
		slots: []record.Slot{{Issue: "LEGION-10", Index: 0, AdmittedAt: opened},
			{Issue: "LEGION-40", Index: 1, AdmittedAt: answeredAt}},
	}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	line := []string{"LEGION-10", "LEGION-12", "LEGION-40", "LEGION-20", "LEGION-11"}
	if !reflect.DeepEqual(got.Admission.Waiting, line) {
		t.Fatalf("admission.waiting = %#v, want %#v: this test reads each member of that line", got.Admission.Waiting, line)
	}
	holds, waits := true, false
	want := map[string]api.WaitingLineView{
		"LEGION-10": {Place: 1, Length: 5, Part: "returned", Kind: "first_phase", PartPlace: 1, PartLength: 3,
			HoldsSlot: &holds, Since: &opened},
		"LEGION-12": {Place: 2, Length: 5, Part: "returned", Kind: "returning", PartPlace: 2, PartLength: 3,
			HoldsSlot: &waits, Since: &owedAt},
		"LEGION-40": {Place: 3, Length: 5, Part: "returned", Kind: "answered", PartPlace: 3, PartLength: 3,
			HoldsSlot: &holds, Since: &answeredAt},
		"LEGION-20": {Place: 4, Length: 5, Part: "starting", Kind: "queued_root", PartPlace: 1, PartLength: 2,
			HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20"},
		"LEGION-11": {Place: 5, Length: 5, Part: "starting", Kind: "first_phase", PartPlace: 2, PartLength: 2,
			HoldsSlot: &waits, RootRank: "M", OwnRank: "N", RootKey: "LEGION-10"},
	}
	for _, member := range line {
		view := got.Issues[member].WaitingLine
		if view == nil {
			t.Fatalf("%s stands in %v and carries no waitingLine", member, line)
		}
		if !reflect.DeepEqual(*view, want[member]) {
			t.Errorf("%s's waitingLine = %s, want %s", member, lineViewSaid(*view), lineViewSaid(want[member]))
		}
	}
}

// lineViewSaid is one waiting-line view read as a line of text, with the two pointers followed:
// %+v of the view prints the addresses, which say nothing about the part a member stands in.
func lineViewSaid(view api.WaitingLineView) string {
	holds, since := "absent", "absent"
	if view.HoldsSlot != nil {
		holds = fmt.Sprintf("%v", *view.HoldsSlot)
	}
	if view.Since != nil {
		since = view.Since.String()
	}
	return fmt.Sprintf("place %d of %d, %s part as %s, place %d of %d there, holding %s, since %s, ranks %q under %q of %q",
		view.Place, view.Length, view.Part, view.Kind, view.PartPlace, view.PartLength,
		holds, since, view.OwnRank, view.RootRank, view.RootKey)
}
