package intake

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every Dispatch issue event carries the issue's own priority, 0 to 3 for P0 to P3 or null for
// none, and the decoder carries it onto the fact. An event with no priority field reads as none.
// A value Dispatch never writes — out of range, or no integer at all — reads as none and is
// reported, never poison: the event may move the issue's status, which the workflow acts on.
func TestADispatchIssueEventCarriesItsOwnPriority(t *testing.T) {
	t.Parallel()
	const subject = "notifications.dispatch.issue.CAPTURE-3.issue.updated"
	original := string(capturedIssueUpdatedEnvelope(t))
	anchor := `\"priority\":null,`
	if !strings.Contains(original, anchor) {
		t.Fatal("captured issue.updated fixture carries no null priority to change")
	}
	for _, tc := range []struct {
		name   string
		field  string
		want   record.Priority
		unread bool
	}{
		{name: "P0", field: `\"priority\":0,`, want: record.P0},
		{name: "P2", field: `\"priority\":2,`, want: record.P2},
		{name: "P3", field: `\"priority\":3,`, want: record.P3},
		{name: "null", field: anchor, want: record.NoPriority},
		{name: "no field", field: "", want: record.NoPriority},
		{name: "a level Dispatch never writes", field: `\"priority\":9,`, want: record.NoPriority, unread: true},
		{name: "no integer", field: `\"priority\":\"high\",`, want: record.NoPriority, unread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, []byte(strings.Replace(original, anchor, tc.field, 1)))
			if err != nil {
				t.Fatalf("decode: %v, want no poison", err)
			}
			issue, ok := decoded.Fact.(DispatchIssue)
			if !ok {
				t.Fatalf("fact = %#v, want a Dispatch issue", decoded.Fact)
			}
			if issue.Priority != tc.want || (len(decoded.Unread) != 0) != tc.unread {
				t.Fatalf("priority = %v, unread %q; want %v, reported %t", issue.Priority, decoded.Unread, tc.want, tc.unread)
			}
		})
	}
}

// The boot listing and the detail read carry the issue's own priority onto the fact they make
// (ListedIssue, ObservedIssue, and OwnStatusWrite over the detail), as an event does, so the
// record takes it from whichever read reaches it first.
func TestTheListingAndTheDetailCarryTheIssuesPriorityOntoTheFact(t *testing.T) {
	t.Parallel()
	level := func(n int) *int { return &n }
	for _, tc := range []struct {
		name  string
		level *int
		want  record.Priority
	}{
		{name: "P0", level: level(0), want: record.P0},
		{name: "P1", level: level(1), want: record.P1},
		{name: "none", level: nil, want: record.NoPriority},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listed := ListedIssue(dispatch.IssueSummary{Key: "LEGION-7", Status: "todo", Rank: "b", LastSeq: 4, Priority: tc.level})
			if listed.Priority != tc.want {
				t.Fatalf("the listing's fact carries %v, want %v", listed.Priority, tc.want)
			}
			detail := dispatch.Issue{Key: "LEGION-7", Status: "todo", Rank: "b", LastSeq: 4, Priority: tc.level}
			if observed := ObservedIssue(detail); observed.Priority != tc.want {
				t.Fatalf("the detail's fact carries %v, want %v", observed.Priority, tc.want)
			}
			if own := OwnStatusWrite(detail); own.Priority != tc.want {
				t.Fatalf("the daemon's own status write carries %v, want %v", own.Priority, tc.want)
			}
		})
	}
}
