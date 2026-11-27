package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// logEvent is one event of an issue's Dispatch event log as its events route serves it, which is
// the shape the durable stream carries: type typ at seq, written by an actor of kind, with payload
// as its payload.
func logEvent(seq int, typ, kind string, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":"LEGION-755","seq":%d,"type":%q,"actor":{"kind":%q,"id":"%s-1"},"notify":true,"created_at":"2026-10-14T17:00:%02dZ","payload":%s}`,
		seq, seq, typ, kind, kind, seq, payload))
}

// servedLog is log as dispatch.Client.EventsAfter hands it to its caller: a stand-in for Dispatch
// answers the read with log as one page, which the daemon's own client reads.
func servedLog(t *testing.T, log ...json.RawMessage) []dispatch.Event {
	t.Helper()
	page := make([]string, len(log))
	for i, raw := range log {
		page[i] = string(raw)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, "["+strings.Join(page, ",")+"]")
	}))
	defer server.Close()
	events, err := dispatch.New(server.URL, dispatch.BearerTransport("dispatch-token", nil)).EventsAfter(context.Background(), "LEGION-755", 0)
	if err != nil {
		t.Fatalf("read the served log: %v", err)
	}
	return events
}

// payloadlessLogEvent is an event of type typ at seq, written by a person, with no payload field.
func payloadlessLogEvent(seq int, typ string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":"LEGION-755","seq":%d,"type":%q,"actor":{"kind":"user","id":"user-1"},"notify":true,"created_at":"2026-10-14T17:00:%02dZ"}`,
		seq, seq, typ, seq))
}

// issueLogEvent is an issue.created or issue.updated event whose issue carries labels.
func issueLogEvent(seq int, typ, kind string, labels ...string) json.RawMessage {
	quoted := make([]string, len(labels))
	for i, label := range labels {
		quoted[i] = fmt.Sprintf("%q", label)
	}
	return logEvent(seq, typ, kind, fmt.Sprintf(`{"key":"LEGION-755","status":"triage","title":"Queue a person's handover","labels":[%s]}`, strings.Join(quoted, ",")))
}

// The label's setter is the actor of the event that added the legion label (spec R2): the newest
// issue.created or issue.updated event whose labels carry it while the previous such event's did
// not, read oldest to newest. Events of other kinds count for nothing, so a person's comment
// between a session's label and a later edit does not make the person the setter; a label removed
// and set again names whoever set it last; and the match on the label ignores case, as
// dispatch.CarriesLegionLabel does.
func TestLabelSetterIsTheActorOfTheEventThatAddedTheLabel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
		want   string
	}{
		{
			name:   "created with the label by a person",
			events: []json.RawMessage{issueLogEvent(1, "issue.created", "user", "legion")},
			want:   "user",
		},
		{
			name: "created unlabelled, then labelled by a session",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
			},
			want: "session",
		},
		{
			name: "labelled by a person, removed, labelled again by a session",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "user"),
				issueLogEvent(3, "issue.updated", "session", "legion"),
			},
			want: "session",
		},
		{
			name: "labelled by a person, removed, by a session, removed, by a person again",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "user"),
				issueLogEvent(3, "issue.updated", "session", "legion"),
				issueLogEvent(4, "issue.updated", "session"),
				issueLogEvent(5, "issue.updated", "user", "legion"),
			},
			want: "user",
		},
		{
			name: "the label spelled Legion counts",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "session", "bug", "Legion"),
			},
			want: "session",
		},
		{
			name: "a person's comment between a session's label and the person's unrelated label counts for nothing",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "session"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
				logEvent(3, "comment.created", "user", `{"id":"c-1","body":"Looks right."}`),
				issueLogEvent(4, "issue.updated", "user", "legion", "docs"),
			},
			want: "session",
		},
		{
			name: "a session's edit that keeps a person's label leaves the person the setter",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
			},
			want: "user",
		},
		{
			name: "an issue.closed carrying the label moves nothing",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "session"),
				logEvent(2, "issue.closed", "user", `{"key":"LEGION-755","status":"done","labels":["legion"]}`),
			},
			want: "",
		},
		{
			name: "no event added the label",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "user", "bug"),
			},
			want: "",
		},
		{
			name:   "a payload with no labels field is unlabelled",
			events: []json.RawMessage{logEvent(1, "issue.created", "user", `{"key":"LEGION-755","status":"triage"}`)},
			want:   "",
		},
		{name: "an empty log", events: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LabelSetter(servedLog(t, tc.events...))
			if err != nil {
				t.Fatalf("LabelSetter: %v", err)
			}
			if got != tc.want {
				t.Fatalf("LabelSetter = %q, want %q", got, tc.want)
			}
		})
	}
}

// A deleted post's events are read from the log masked - its comment.created or message.created
// carries deleted_at and deleted_by and no body (Dispatch's attachPostMarks) - and an author
// corrects a post with comment.edited, comment.deleted, message.edited and message.deleted. None
// of them is an issue event, so a log that holds them names the setter the same log without them
// names: here the session that labelled the issue, though a person's posts and their corrections
// stand before that label and between it and the person's later edit that keeps it.
func TestLabelSetterPassesOverDeletedPostsAndCorrections(t *testing.T) {
	const deleted = `"deleted_at":"2026-10-14T17:30:00Z","deleted_by":{"kind":"user","id":"user-1"}`
	posts := func(seq int) []json.RawMessage {
		return []json.RawMessage{
			logEvent(seq, "comment.created", "user", `{"id":"c-1","issue_key":"LEGION-755",`+deleted+`}`),
			logEvent(seq+1, "message.created", "user", `{"id":"m-1","issue_key":"LEGION-755",`+deleted+`}`),
			logEvent(seq+2, "comment.edited", "user", `{"id":"c-2","issue_key":"LEGION-755","body":"Looks right now.","edited_at":"2026-10-14T17:30:00Z"}`),
			logEvent(seq+3, "comment.deleted", "user", `{"id":"c-1","issue_key":"LEGION-755","body":"",`+deleted+`}`),
			logEvent(seq+4, "message.edited", "user", `{"id":"m-2","issue_key":"LEGION-755","body":"Still green.","edited_at":"2026-10-14T17:30:00Z"}`),
			logEvent(seq+5, "message.deleted", "user", `{"id":"m-1","issue_key":"LEGION-755","body":"",`+deleted+`}`),
		}
	}
	created := issueLogEvent(1, "issue.created", "user")
	labelled := issueLogEvent(8, "issue.updated", "session", "legion")
	kept := issueLogEvent(15, "issue.updated", "user", "legion", "docs")
	bare, err := LabelSetter(servedLog(t, created, labelled, kept))
	if err != nil || bare != "session" {
		t.Fatalf("LabelSetter of the issue's own events = %q, %v; want the session that labelled it", bare, err)
	}
	withPosts := slices.Concat([]json.RawMessage{created}, posts(2), []json.RawMessage{labelled}, posts(9), []json.RawMessage{kept})
	got, err := LabelSetter(servedLog(t, withPosts...))
	if err != nil {
		t.Fatalf("LabelSetter of the log with the deleted posts and corrections: %v", err)
	}
	if got != bare {
		t.Fatalf("LabelSetter of the log with the deleted posts and corrections = %q, want %q, as without them", got, bare)
	}
}

// The intake's walk of the event log (walkIssueEvents, main's #4590) reads a deleted post's
// events as every read of the log serves them - a comment.created or message.created with
// deleted_at and deleted_by and no body - and the author's corrections of its posts. It reads no
// post's body, so none of them is an error: the walk passes over each and gives the issue's own
// events alone, and no deleted post is ever a message the intake fails on and gives up on.
func TestWalkIssueEventsPassesOverADeletedPostsEventsWithNoError(t *testing.T) {
	const deleted = `"deleted_at":"2026-10-14T17:30:00Z","deleted_by":{"kind":"user","id":"user-1"}`
	events := []json.RawMessage{
		issueLogEvent(1, "issue.created", "user"),
		logEvent(2, "message.created", "user", `{"id":"m-1","issue_key":"LEGION-755",`+deleted+`}`),
		logEvent(3, "comment.created", "user", `{"id":"c-1","issue_key":"LEGION-755",`+deleted+`}`),
		logEvent(4, "message.deleted", "user", `{"id":"m-1","issue_key":"LEGION-755","body":"",`+deleted+`}`),
		logEvent(5, "message.edited", "user", `{"id":"m-2","issue_key":"LEGION-755","body":"Still green.","edited_at":"2026-10-14T17:30:00Z"}`),
		issueLogEvent(6, "issue.updated", "session", "legion"),
	}
	var given []int64
	for event, err := range walkIssueEvents(servedLog(t, events...)) {
		if err != nil {
			t.Fatalf("walkIssueEvents over a log holding a deleted post's events: %v", err)
		}
		given = append(given, event.Seq)
	}
	if !slices.Equal(given, []int64{1, 6}) {
		t.Fatalf("walkIssueEvents gave the events at %v, want the issue's own, at 1 and 6", given)
	}
}

// A log the daemon cannot read names no setter it can act on: LabelSetter returns the walk's error
// (TestWalkIssueEventsRefusesAnEventItCannotDecode), naming the event's position, here for an
// issue event with no payload, one whose payload is not an issue and a later one whose labels are
// not a list, so the consumer delivers the fact again rather than treating the label as nobody's.
// An event that does not decode at all is refused by the read itself (dispatch.Client.EventsAfter).
func TestLabelSetterRefusesAnEventItCannotDecode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
		want   string
	}{
		{
			name:   "an issue event with no payload",
			events: []json.RawMessage{issueLogEvent(1, "issue.created", "user", "legion"), payloadlessLogEvent(2, "issue.updated")},
			want:   "decode Dispatch issue payload of event 2 of 2",
		},
		{
			name:   "an issue event whose payload is not an issue",
			events: []json.RawMessage{logEvent(1, "issue.created", "user", `"a string"`)},
			want:   "decode Dispatch issue payload of event 1 of 1",
		},
		{
			name:   "a later issue event whose labels are not a list",
			events: []json.RawMessage{issueLogEvent(1, "issue.created", "user"), logEvent(2, "issue.updated", "user", `{"labels":"legion"}`)},
			want:   "decode Dispatch issue payload of event 2 of 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LabelSetter(servedLog(t, tc.events...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LabelSetter error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
