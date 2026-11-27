package intake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// The files under testdata/dispatch-events are real Dispatch events: each is the answer of one
// GET of an issue's event log, read on 2026-11-20 with the deployment's Dispatch token, <KEY>
// being the issue's key in that deployment:
//
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues/<KEY>/events?after=11&limit=6"
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues/<KEY>/events?after=21&limit=6"
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues/<KEY>/events?after=40&limit=1"
//
// This repository is public, so each answer went through the filter below before it was kept,
// and then through `bunx biome format --write`. The filter keeps each event's envelope and its
// actor's kind and id as Dispatch served them. It removes the text a person or an agent wrote
// (titles, bodies, document diffs and summaries), the names of people and of repositories, and
// the machine and directory of each session. StatusChangeOf reads none of those fields.
//
//	jq 'map((if .actor.origin then .actor.origin |= {host, session_title} else . end)
//	  | .payload |= with_entries(select(.key | IN("key", "project", "number", "parent", "ancestors",
//	    "status", "changed", "closed_at", "last_seq", "labels", "blocked_by", "rank", "priority",
//	    "claim", "created_at", "updated_at", "id", "issue_key", "artifact_id", "name"))))'
//
// The captures came from a real Dispatch, and their project key was then replaced with the
// neutral ACME in every string, as AGENTS.md asks, so each issue keeps its number and every other
// byte of each event is as the filter left it.
const dispatchEventsDir = "testdata/dispatch-events"

// The sessions that wrote the captured events: the architect of ACME-3021's parent, the two
// controller sessions of the project, and the architect of ACME-2766's parent.
const (
	capturedParentArchitect  = "01a1b4c8-6677-7524-b42e-6cff930d6286"
	capturedController       = "01a1bcac-dd96-74fc-9a20-4fc75e8968ca"
	capturedLaterController  = "01a1de41-6350-7124-a481-cdf23be15a2b"
	capturedOtherArchitect   = "01a1db45-7f94-7519-998f-4556d50e219a"
	capturedDaemonOfTheIssue = "legion-daemon:ACME"
)

// readDispatchLog is the event log a file under testdata/dispatch-events holds, one raw event for
// each element, as Dispatch's events route answered it.
func readDispatchLog(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dispatchEventsDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return events
}

// StatusChangeOf reads each event Dispatch writes as the set-back's check needs it: a session's
// close, the daemon's set-back and the controller's close each change the status. A message, a
// comment, a document version and an issue.updated that moved blocked_by alone do not, though
// each one moves the sequence. Each event gives its own sequence and the actor Dispatch names.
// The daemon's set-back names the session that dispatch.DaemonSession gives for the issue, so it
// alone reads as the daemon's own event (DaemonActor), which the check passes over. A write of the
// status the issue already shows changes nothing in Dispatch, which then writes no changed list,
// and the reader takes it as no change.
func TestStatusChangeOfReadsTheEventsDispatchWrites(t *testing.T) {
	t.Parallel()
	session := func(id string) dispatch.Actor { return dispatch.Actor{Kind: "session", ID: id} }
	type read struct {
		typ     string
		changed bool
		actor   dispatch.Actor
		// daemon says whether the event is the daemon's own.
		daemon bool
	}
	for _, log := range []struct {
		file string
		// first is the sequence of the log's first event, and want what each event reads, in order.
		first int64
		want  []read
	}{
		{file: "acme-3021-after-11.json", first: 12, want: []read{
			{"message.created", false, session(capturedParentArchitect), false},
			{"issue.closed", true, session(capturedParentArchitect), false},
			{"issue.updated", true, session(capturedDaemonOfTheIssue), true},
			{"comment.created", false, session(capturedParentArchitect), false},
			{"message.created", false, session(capturedController), false},
			{"issue.closed", true, session(capturedController), false},
		}},
		{file: "acme-2766-after-21.json", first: 22, want: []read{
			{"message.created", false, session(capturedLaterController), false},
			{"issue.closed", true, session(capturedLaterController), false},
			{"issue.updated", true, session(capturedDaemonOfTheIssue), true},
			{"artifact.version", false, session(capturedOtherArchitect), false},
			{"artifact.version", false, session(capturedOtherArchitect), false},
			{"artifact.version", false, session(capturedOtherArchitect), false},
		}},
		{file: "acme-2766-after-40.json", first: 41, want: []read{
			{"issue.updated", false, session(capturedOtherArchitect), false},
		}},
	} {
		events := servedLog(t, readDispatchLog(t, log.file)...)
		if len(events) != len(log.want) {
			t.Fatalf("%s holds %d events, want %d", log.file, len(events), len(log.want))
		}
		for i, event := range events {
			want, seq := log.want[i], log.first+int64(i)
			t.Run(fmt.Sprintf("%s #%d %s", log.file, seq, want.typ), func(t *testing.T) {
				if event.Type != want.typ {
					t.Fatalf("the captured event #%d is %q, want %q: the table no longer describes the fixture", seq, event.Type, want.typ)
				}
				got, err := StatusChangeOf(event)
				if err != nil {
					t.Fatalf("read the event: %v", err)
				}
				if wantChange := (StatusChange{Seq: seq, Changed: want.changed, Actor: want.actor, DaemonActor: want.daemon}); got != wantChange {
					t.Fatalf("the event reads %+v, want %+v", got, wantChange)
				}
			})
		}
	}

	t.Run("the daemon's set-back is the session dispatch.DaemonSession names", func(t *testing.T) {
		if daemon := dispatch.DaemonSession("ACME-3021"); daemon != capturedDaemonOfTheIssue {
			t.Fatalf("dispatch.DaemonSession(ACME-3021) = %q, want %q, the actor of the daemon's captured set-back", daemon, capturedDaemonOfTheIssue)
		}
	})

	t.Run("a write of the status the issue already shows", func(t *testing.T) {
		// Event #41 with no changed list: the event Dispatch writes for a write that moved no field.
		var event map[string]any
		if err := json.Unmarshal(readDispatchLog(t, "acme-2766-after-40.json")[0], &event); err != nil {
			t.Fatalf("decode event #41: %v", err)
		}
		delete(event["payload"].(map[string]any), "changed")
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("encode event #41 without changed: %v", err)
		}
		got, err := StatusChangeOf(servedLog(t, raw)[0])
		if err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if want := (StatusChange{Seq: 41, Actor: session(capturedOtherArchitect)}); got != want {
			t.Fatalf("the event reads %+v, want %+v: it changed no status", got, want)
		}
	})
}

// An issue.updated whose payload the reader cannot decode is an error, so the set-back's check
// never takes a log it cannot read for a log with no status change in it. An event that does not
// decode at all is refused by the read itself (dispatch.Client.EventsAfter).
func TestStatusChangeOfRefusesAnEventItCannotDecode(t *testing.T) {
	t.Parallel()
	event := servedLog(t, json.RawMessage(`{"seq":7,"type":"issue.updated","actor":{"kind":"user","id":"a"},"payload":"a string"}`))[0]
	if _, err := StatusChangeOf(event); err == nil || !strings.Contains(err.Error(), "decode the issue payload of Dispatch event 7") {
		t.Fatalf("StatusChangeOf(an issue.updated whose payload is a string) = %v, want an error naming the event", err)
	}
}

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
	t.Parallel()
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
			got, err := statusChangedAfter(servedLog(t, tc.events...), tc.seq)
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
	t.Parallel()
	const want = "decode Dispatch issue payload of event 2 of 2"
	_, err := statusChangedAfter(servedLog(t, approvalLogEvent(9), payloadlessLogEvent(10, "issue.updated")), 9)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("statusChangedAfter error = %v, want one containing %q", err, want)
	}
}
