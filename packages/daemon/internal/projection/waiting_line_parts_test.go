package projection

import (
	"context"
	"encoding/json"
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
// that part, whether it holds one of the cap's slots, what placed it — the moment its work
// came back for a member of the returned part, the Dispatch ranks and the tree root's key for a
// member of the starting part — and the sentence rendered from all of them
// (waitingLineReason), which is the same fields read as a person reads them. The part's length
// is counted within the member's priority band, so the band with no priority answers 3 and 1 for
// its two parts and the P0 band 1, and never the length of the line.
//
// The fixture is internal/record's TestWaitingLinePartAndKeyOnEveryEntry written for this
// package, so the projected view and the entries it is read from say the same thing about the
// same facts: an owed start, an answered root and a tree root's own first phase in the returned
// part, and a queued root and a child's first phase in the starting one. The queued root is P0
// of its own, so it stands first in the whole line, ahead of the band with no priority.
func TestWaitingLinePartAndKeyReachTheProjectedView(t *testing.T) {
	opened, owedAt, answeredAt := projectedAt.Add(-3*time.Hour), projectedAt.Add(-2*time.Hour), projectedAt.Add(-time.Hour)
	approved := 1
	answered := lineIssue("LEGION-40", "LEGION-40", "in_progress", phase.AwaitingPerson, "D")
	answered.Awaiting = &record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 9},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByReply}
	p0Root := lineIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A")
	p0Root.Priority = record.P0
	store := projectionStore{
		issues: []record.Issue{
			lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
			lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
			// The member that owes its worker a start stands at testing and not in accepting,
			// retro or merging, which need none of the cap's slots and so stand in no line
			// (record.SlotRule.PhaseKeepsSlot), as internal/record's own fixture has it.
			lineIssue("LEGION-12", "LEGION-10", "in_progress", phase.Testing, "O"),
			p0Root,
			answered,
		},
		lifecycles: []string{"LEGION-10"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-10": {record.DesignStepSpec: {Issue: "LEGION-10", Step: record.DesignStepSpec, LatestVersion: 1,
				ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}},
		},
		owed: []record.OwedStart{{Issue: "LEGION-12", ID: 42, CreatedAt: owedAt}},
		// The root whose gate opened and the answered root each kept the row they hold, as
		// neither waits on another issue (record.KeepsSlot); the other three members hold none.
		slots: []record.Slot{{Issue: "LEGION-10", Index: 0, AdmittedAt: opened},
			{Issue: "LEGION-40", Index: 1, AdmittedAt: answeredAt}},
	}

	got, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	line := []string{"LEGION-20", "LEGION-10", "LEGION-12", "LEGION-40", "LEGION-11"}
	if !reflect.DeepEqual(got.Admission.Waiting, line) {
		t.Fatalf("admission.waiting = %#v, want %#v: this test reads each member of that line", got.Admission.Waiting, line)
	}
	holds, waits := true, false
	// LEGION-20 is P0 of its own and waits at it. None of the other four has a priority, of its
	// own or by its tree root, so each is placed after P3 and carries a null priority, and its
	// sentence says so and counts within that band.
	none, p0 := api.PriorityView(record.NoPriority), api.PriorityView(record.P0)
	want := map[string]api.WaitingLineView{
		"LEGION-20": {Place: 1, Length: 5, Part: "starting", Kind: "queued_root", PartPlace: 1, PartLength: 1,
			HoldsSlot: &waits, RootRank: "A", OwnRank: "A", RootKey: "LEGION-20", Priority: &p0,
			Reason: "It has not started. It waits at P0, its own priority. " +
				"It is 1st of 1 issue at P0 that has not started, by its own Dispatch rank."},
		"LEGION-10": {Place: 2, Length: 5, Part: "returned", Kind: "first_phase", PartPlace: 1, PartLength: 3,
			HoldsSlot: &holds, Since: &opened, Priority: &none,
			Reason: "Its design gate opened at " + opened.UTC().Format(time.RFC3339) + ". " +
				"It has no priority, so it waits after every issue at P0 to P3. " +
				"It is 1st of 3 issues with no priority whose work came back. " +
				"It already holds its admission slot, so it resumes at the next pass whatever the cap says."},
		"LEGION-12": {Place: 3, Length: 5, Part: "returned", Kind: "returning", PartPlace: 2, PartLength: 3,
			HoldsSlot: &waits, Since: &owedAt, Priority: &none,
			Reason: "Its work came back at " + owedAt.UTC().Format(time.RFC3339) + ". " +
				"Neither it nor its tree root LEGION-10 has a priority, so it waits after every issue at P0 to P3. " +
				"It is 2nd of 3 issues with no priority whose work came back, " +
				"and they take the free slots before the issues with no priority that have not started."},
		"LEGION-40": {Place: 4, Length: 5, Part: "returned", Kind: "answered", PartPlace: 3, PartLength: 3,
			HoldsSlot: &holds, Since: &answeredAt, Priority: &none,
			Reason: "A person answered it, and its work came back at " + answeredAt.UTC().Format(time.RFC3339) + ". " +
				"It has no priority, so it waits after every issue at P0 to P3. " +
				"It is 3rd of 3 issues with no priority whose work came back. It already holds its admission slot, " +
				"so it resumes at the next pass whatever the cap says."},
		"LEGION-11": {Place: 5, Length: 5, Part: "starting", Kind: "first_phase", PartPlace: 1, PartLength: 1,
			HoldsSlot: &waits, RootRank: "M", OwnRank: "N", RootKey: "LEGION-10", Priority: &none,
			Reason: "It has not started. Neither it nor its tree root LEGION-10 has a priority, so it waits after every issue at P0 to P3. " +
				"It is 1st of 1 issue with no priority that has not started, " +
				"by the Dispatch rank of its tree root LEGION-10, and 3 issues with no priority whose work came back stand ahead of all of them."},
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

// lineViewSaid is one waiting-line view as the JSON it would be served as: every member the
// view holds, named, with the two pointers followed, which `%+v` prints as addresses that say
// nothing about the part a member stands in. It is read out of the value rather than written
// member by member so that a member added to the view later is in the failure too: a renderer
// naming today's members prints `got` and `want` identically when they differ only in a member
// it does not know, which is a failure nobody can read.
func lineViewSaid(view api.WaitingLineView) string {
	said, err := json.Marshal(view)
	if err != nil {
		return fmt.Sprintf("a view that will not marshal, %+v: %v", view, err)
	}
	return string(said)
}
