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

// readDispatchEvents is the event log a file under testdata/dispatch-events holds, one raw event
// for each element, as dispatch.Client.EventsAfter hands the events to its caller.
func readDispatchEvents(t *testing.T, name string) []json.RawMessage {
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
// The daemon's set-back names the session that dispatch.DaemonSession gives for the issue, which
// is the actor the check passes over. A write of the status the issue already shows changes
// nothing in Dispatch, which then writes no changed list, and the reader takes it as no change.
func TestStatusChangeOfReadsTheEventsDispatchWrites(t *testing.T) {
	session := func(id string) dispatch.Actor { return dispatch.Actor{Kind: "session", ID: id} }
	type read struct {
		typ     string
		changed bool
		actor   dispatch.Actor
	}
	for _, log := range []struct {
		file string
		// first is the sequence of the log's first event, and want what each event reads, in order.
		first int64
		want  []read
	}{
		{file: "acme-3021-after-11.json", first: 12, want: []read{
			{"message.created", false, session(capturedParentArchitect)},
			{"issue.closed", true, session(capturedParentArchitect)},
			{"issue.updated", true, session(capturedDaemonOfTheIssue)},
			{"comment.created", false, session(capturedParentArchitect)},
			{"message.created", false, session(capturedController)},
			{"issue.closed", true, session(capturedController)},
		}},
		{file: "acme-2766-after-21.json", first: 22, want: []read{
			{"message.created", false, session(capturedLaterController)},
			{"issue.closed", true, session(capturedLaterController)},
			{"issue.updated", true, session(capturedDaemonOfTheIssue)},
			{"artifact.version", false, session(capturedOtherArchitect)},
			{"artifact.version", false, session(capturedOtherArchitect)},
			{"artifact.version", false, session(capturedOtherArchitect)},
		}},
		{file: "acme-2766-after-40.json", first: 41, want: []read{
			{"issue.updated", false, session(capturedOtherArchitect)},
		}},
	} {
		events := readDispatchEvents(t, log.file)
		if len(events) != len(log.want) {
			t.Fatalf("%s holds %d events, want %d", log.file, len(events), len(log.want))
		}
		for i, raw := range events {
			want, seq := log.want[i], log.first+int64(i)
			t.Run(fmt.Sprintf("%s #%d %s", log.file, seq, want.typ), func(t *testing.T) {
				var envelope struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type != want.typ {
					t.Fatalf("the captured event #%d is %q (%v), want %q: the table no longer describes the fixture", seq, envelope.Type, err, want.typ)
				}
				got, err := StatusChangeOf(raw)
				if err != nil {
					t.Fatalf("read the event: %v", err)
				}
				if wantChange := (StatusChange{Seq: seq, Changed: want.changed, Actor: want.actor}); got != wantChange {
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
		if err := json.Unmarshal(readDispatchEvents(t, "acme-2766-after-40.json")[0], &event); err != nil {
			t.Fatalf("decode event #41: %v", err)
		}
		delete(event["payload"].(map[string]any), "changed")
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("encode event #41 without changed: %v", err)
		}
		got, err := StatusChangeOf(raw)
		if err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if want := (StatusChange{Seq: 41, Actor: session(capturedOtherArchitect)}); got != want {
			t.Fatalf("the event reads %+v, want %+v: it changed no status", got, want)
		}
	})
}

// An event the reader cannot decode is an error, so the set-back's check never takes a log it
// cannot read for a log with no status change in it.
func TestStatusChangeOfRefusesAnEventItCannotDecode(t *testing.T) {
	for _, tc := range []struct {
		name, event, want string
	}{
		{"an event that is not JSON", `{not json`, "decode Dispatch event"},
		{"an issue.updated whose payload is not an issue", `{"seq":7,"type":"issue.updated","actor":{"kind":"user","id":"a"},"payload":"a string"}`,
			"decode the issue payload of Dispatch event 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := StatusChangeOf(json.RawMessage(tc.event)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StatusChangeOf(%s) = %v, want an error naming %q", tc.event, err, tc.want)
			}
		})
	}
}
