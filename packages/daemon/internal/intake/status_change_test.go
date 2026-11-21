package intake

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// statusLogEvent is an issue event of type typ at seq whose labelled issue stands in status under
// title, as the person wrote it.
func statusLogEvent(seq int, typ, status, title string) json.RawMessage {
	return logEvent(seq, typ, "user", fmt.Sprintf(`{"key":"LEGION-755","status":%q,"title":%q,"labels":["legion"]}`, status, title))
}

// approvalLogEvent is a person's approval at seq of version 1 of the issue's document art-1.
func approvalLogEvent(seq int) json.RawMessage {
	return logEvent(seq, "artifact.approved", "user", `{"artifact_id":"art-1","version":1}`)
}

// The status changed after an approval at seq when an issue event after it (issue.created,
// issue.updated or issue.closed) carries another status than the issue event before it, and
// statusChangedAfter is the first such event's sequence. A rename keeps the status, so it is no
// change, and a todo and a park again after the approval is a change at the todo, though the
// status ends where it stood. Events of other kinds count for nothing. With no issue event at or
// before seq, only a change between the later ones counts.
func TestStatusChangedAfterIsTheFirstLaterIssueEventOfAnotherStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
		seq    int64
		want   int64
	}{
		{
			name: "a park before the approval and a rename after it",
			events: []json.RawMessage{
				statusLogEvent(1, "issue.created", "triage", "root"),
				statusLogEvent(5, "issue.updated", "icebox", "root"),
				approvalLogEvent(9),
				statusLogEvent(11, "issue.updated", "icebox", "the root, renamed"),
			},
			seq: 9,
		},
		{
			name: "a todo after the approval, then a park again in the same status",
			events: []json.RawMessage{
				statusLogEvent(1, "issue.created", "triage", "root"),
				statusLogEvent(5, "issue.updated", "icebox", "root"),
				approvalLogEvent(9),
				statusLogEvent(10, "issue.updated", "todo", "root"),
				statusLogEvent(11, "issue.updated", "icebox", "root"),
			},
			seq:  9,
			want: 10,
		},
		{
			name: "a close after the approval",
			events: []json.RawMessage{
				statusLogEvent(1, "issue.created", "triage", "root"),
				statusLogEvent(5, "issue.updated", "icebox", "root"),
				approvalLogEvent(9),
				statusLogEvent(12, "issue.closed", "done", "root"),
			},
			seq:  9,
			want: 12,
		},
		{
			name: "claims, comments and reviews after the approval",
			events: []json.RawMessage{
				statusLogEvent(1, "issue.created", "triage", "root"),
				statusLogEvent(5, "issue.updated", "icebox", "root"),
				approvalLogEvent(9),
				logEvent(10, "issue.claimed", "session", `{"session":"session-10"}`),
				logEvent(11, "comment.created", "user", `{"id":"c-1","body":"Looks right."}`),
				logEvent(12, "artifact.changes_requested", "user", `{"artifact_id":"art-1","version":1,"reason":"Say why."}`),
				logEvent(13, "issue.released", "session", `{"session":"session-10"}`),
			},
			seq: 9,
		},
		{
			name: "no issue event at or before the approval, and later ones of one status",
			events: []json.RawMessage{
				approvalLogEvent(9),
				statusLogEvent(10, "issue.updated", "icebox", "root"),
				statusLogEvent(11, "issue.updated", "icebox", "the root, renamed"),
			},
			seq: 9,
		},
		{
			name: "no issue event at or before the approval, and a change between later ones",
			events: []json.RawMessage{
				approvalLogEvent(9),
				statusLogEvent(10, "issue.updated", "icebox", "root"),
				statusLogEvent(11, "issue.updated", "todo", "root"),
			},
			seq:  9,
			want: 11,
		},
		{name: "an empty log", seq: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := statusChangedAfter(tc.events, tc.seq)
			if err != nil {
				t.Fatalf("statusChangedAfter: %v", err)
			}
			if got != tc.want {
				t.Fatalf("statusChangedAfter(%d) = %d, want %d", tc.seq, got, tc.want)
			}
		})
	}
}

// A log the daemon cannot read cannot say the status stayed: statusChangedAfter returns the walk's
// error (TestWalkIssueEventsRefusesAnEventItCannotDecode), here for an issue event with no payload,
// so the preparation fails and the approval is delivered again rather than applied as though
// nothing changed.
func TestStatusChangedAfterRefusesAnEventItCannotDecode(t *testing.T) {
	const want = "decode Dispatch issue payload of event 2 of 2"
	_, err := statusChangedAfter([]json.RawMessage{approvalLogEvent(9), payloadlessLogEvent(10, "issue.updated")}, 9)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("statusChangedAfter error = %v, want one containing %q", err, want)
	}
}
