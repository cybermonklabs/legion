package intake

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The walk gives the log's issue events (issue.created, issue.updated and issue.closed) in the
// log's order, each with its sequence, type, actor kind, status and labels, and passes over every
// other event. A loop that stops early leaves the later events undecoded, so an event the walk
// cannot decode after the stop is no error.
func TestWalkIssueEventsGivesEachIssueEventAndPassesOverTheRest(t *testing.T) {
	t.Parallel()
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

// An event the walk cannot read is an error naming its position, and the walk ends there: an event
// that does not decode as a Dispatch event, and an issue event whose payload is no JSON object (a
// string, no payload field, or `null`), as the live decoder refuses each. The events before it are
// given, and none after it.
func TestWalkIssueEventsRefusesAnEventItCannotDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bad  json.RawMessage
		want string
	}{
		{name: "an event that is not JSON", bad: json.RawMessage(`{not json`), want: "decode Dispatch event 2 of 3"},
		{name: "an issue event whose payload is a string", bad: logEvent(2, "issue.updated", "user", `"a string"`), want: "decode Dispatch issue payload of event 2 of 3"},
		{name: "an issue event with no payload", bad: payloadlessLogEvent(2, "issue.updated"), want: "decode Dispatch issue payload of event 2 of 3"},
		{name: "an issue event whose payload is null", bad: logEvent(2, "issue.updated", "user", `null`), want: "decode Dispatch issue payload of event 2 of 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []json.RawMessage{issueLogEvent(1, "issue.created", "user"), tc.bad, issueLogEvent(3, "issue.updated", "user", "legion")}
			var given []int64
			var walkErr error
			for event, err := range walkIssueEvents(events) {
				if err != nil {
					walkErr = err
					continue
				}
				given = append(given, event.Seq)
			}
			if walkErr == nil || !strings.Contains(walkErr.Error(), tc.want) {
				t.Fatalf("walkIssueEvents error = %v, want one containing %q", walkErr, tc.want)
			}
			if !reflect.DeepEqual(given, []int64{1}) {
				t.Fatalf("walkIssueEvents gave the events at %v, want only the one before the error, at 1", given)
			}
		})
	}
}
