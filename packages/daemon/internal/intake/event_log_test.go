package intake

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The walk gives the log's issue events (issue.created, issue.updated and issue.closed) in the
// log's order, each with its sequence, type, actor kind, status and labels, and passes over every
// other event. A loop that stops early leaves the later events undecoded, so an event the walk
// cannot decode after the stop is no error.
func TestWalkIssueEventsGivesEachIssueEventAndPassesOverTheRest(t *testing.T) {
	events := []json.RawMessage{
		issueLogEvent(1, "issue.created", "user"),
		logEvent(2, "comment.created", "user", `{"id":"c-1","body":"Looks right."}`),
		issueLogEvent(3, "issue.updated", "session", "legion"),
		logEvent(4, "issue.claimed", "session", `{"session":"session-4"}`),
		logEvent(5, "issue.closed", "user", `{"key":"LEGION-755","status":"done","labels":["legion"]}`),
	}
	var got []issueEvent
	for event, err := range walkIssueEvents(events) {
		if err != nil {
			t.Fatalf("walkIssueEvents: %v", err)
		}
		got = append(got, event)
	}
	want := []issueEvent{
		{Seq: 1, Type: "issue.created", ActorKind: "user", Status: "triage", Labels: []string{}},
		{Seq: 3, Type: "issue.updated", ActorKind: "session", Status: "triage", Labels: []string{"legion"}},
		{Seq: 5, Type: "issue.closed", ActorKind: "user", Status: "done", Labels: []string{"legion"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walkIssueEvents gave %#v, want %#v", got, want)
	}

	stopped := []json.RawMessage{issueLogEvent(1, "issue.created", "user"), json.RawMessage(`{not json`)}
	for event, err := range walkIssueEvents(stopped) {
		if err != nil {
			t.Fatalf("a walk stopped at its first event decoded the next: %v", err)
		}
		if event.Seq == 1 {
			break
		}
	}
}
