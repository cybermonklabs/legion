package intake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// readDispatchEnvelope is the envelope testdata/dispatch/<name> holds.
func readDispatchEnvelope(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "dispatch", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

// editEnvelope is the envelope data with the event its payload holds changed by edit: the
// envelope's payload is the Dispatch event as JSON text, so a member of the event, or of the ask or
// issue the event carries, is changed by decoding that text, editing it and writing it again.
func editEnvelope(t *testing.T, data []byte, edit func(event map[string]any)) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(envelope["payload"].(string)), &event); err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	edit(event)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	envelope["payload"] = string(encoded)
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return out
}

// payloadOf is the payload of the event an edit is given (editEnvelope).
func payloadOf(event map[string]any) map[string]any { return event["payload"].(map[string]any) }

// An ask lifecycle row is one ask event of LEGION-7 at its sequence 12 (askEventMark), the fact the
// decoder makes of it, and the ask the approvals signal's lifecycle rule (signals.NextAsk) leaves
// from the ask before it (before, which the row says whether an earlier event made: known).
type askLifecycleRow struct {
	name      string
	eventType string
	payload   map[string]any
	before    signals.Ask
	known     bool
	fact      string
	turn      signals.AskEvent
	after     signals.Ask
}

// askLifecycleRows is every kind of event of an ask that the approvals signal reads, as the replay
// reads each (replay's readAskEvent): the opening and a restored one, the hand-on of the ask to a
// person and its clearing, an edit of a question and the edit that moves an approval request, a
// hand-back, an answer and a resolution, a restored close, and a reply of each kind, one that comes
// from a callback among them.
func askLifecycleRows() []askLifecycleRow {
	const ask = "ask-1"
	open := signals.Ask{ID: ask, Issue: "LEGION-7", Open: true, Human: true}
	waitingOnAgent := signals.Ask{ID: ask, Issue: "LEGION-7", Open: true}
	handedToDana := signals.Ask{ID: ask, Issue: "LEGION-7", Open: true, Human: true, AskedOf: "dana"}
	closed := signals.Ask{ID: ask, Issue: "LEGION-7"}
	// An ask that closes keeps the turn it had, which a closed ask is never read by.
	answered := signals.Ask{ID: ask, Issue: "LEGION-7", Human: true}
	event := func(kind signals.AskEventKind, state signals.AskReport) signals.AskEvent {
		return signals.AskEvent{Kind: kind, Ask: ask, Issue: "LEGION-7", State: state}
	}
	return []askLifecycleRow{
		{name: "ask-opened", eventType: "ask.opened", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "asked_of": nil},
			fact: "DispatchAskOpened", turn: signals.AskEvent{Kind: signals.AskOpened, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen, AskedOfSet: true},
			after: open},
		{name: "ask-opened-handed-to-a-person", eventType: "ask.opened", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "asked_of": "dana"},
			fact: "DispatchAskOpened", turn: signals.AskEvent{Kind: signals.AskOpened, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen, AskedOfSet: true, AskedOf: "dana"},
			after: handedToDana},
		{name: "ask-opened-restored", eventType: "ask.opened", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "restored": true},
			before: closed, known: true, fact: "DispatchAskRestored", turn: event(signals.AskOpened, signals.ReportsOpen), after: open},
		{name: "ask-asked-of", eventType: "ask.asked_of", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "asked_of": "dana"},
			before: open, known: true, fact: "DispatchAskTurn",
			turn:  signals.AskEvent{Kind: signals.AskAskedOf, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen, AskedOfSet: true, AskedOf: "dana"},
			after: handedToDana},
		{name: "ask-asked-of-cleared", eventType: "ask.asked_of", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "asked_of": nil},
			before: handedToDana, known: true, fact: "DispatchAskTurn",
			turn:  signals.AskEvent{Kind: signals.AskAskedOf, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen, AskedOfSet: true},
			after: open},
		{name: "ask-edited-question", eventType: "ask.edited", payload: map[string]any{"id": ask, "kind": "question", "state": "open", "asked_of": "dana"},
			before: handedToDana, known: true, fact: "DispatchAskTurn",
			turn:  signals.AskEvent{Kind: signals.AskEdited, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen, AskedOfSet: true, AskedOf: "dana"},
			after: handedToDana},
		{name: "ask-edited-approval-request-at-its-handed-version", eventType: "ask.edited",
			payload: map[string]any{"id": ask, "kind": "approval", "state": "open",
				"approval": map[string]any{"artifact_id": "artifact-7", "version": 4, "requested_version": 4}},
			before: open, known: true, fact: "DispatchAskTurn",
			turn: signals.AskEvent{Kind: signals.AskEdited, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen,
				Approval: &signals.AskApproval{Version: new(4), RequestedVersion: new(4)}},
			after: open},
		{name: "ask-edited-approval-request-moved", eventType: "ask.edited",
			payload: map[string]any{"id": ask, "kind": "approval", "state": "open",
				"approval": map[string]any{"artifact_id": "artifact-7", "version": 4, "requested_version": 3}},
			before: open, known: true, fact: "DispatchApprovalMoved",
			turn: signals.AskEvent{Kind: signals.AskEdited, Ask: ask, Issue: "LEGION-7", State: signals.ReportsOpen,
				Approval: &signals.AskApproval{Version: new(4), RequestedVersion: new(3)}},
			after: waitingOnAgent},
		{name: "ask-handed-back", eventType: "ask.handed_back", payload: map[string]any{"id": ask, "kind": "approval", "state": "open"},
			before: waitingOnAgent, known: true, fact: "DispatchAskReply", turn: event(signals.AskHandedBack, signals.ReportsOpen), after: open},
		{name: "ask-answered", eventType: "ask.answered", payload: map[string]any{"id": ask, "kind": "question", "state": "answered"},
			before: open, known: true, fact: "DispatchAskClosed", turn: event(signals.AskAnswered, signals.ReportsClosed), after: answered},
		{name: "ask-resolved", eventType: "ask.resolved", payload: map[string]any{"id": ask, "kind": "question", "state": "resolved"},
			before: open, known: true, fact: "DispatchAskClosed", turn: event(signals.AskResolved, signals.ReportsClosed), after: answered},
		{name: "ask-answered-restored", eventType: "ask.answered", payload: map[string]any{"id": ask, "kind": "question", "state": "answered", "restored": true},
			before: open, known: true, fact: "DispatchAskRestored", turn: event(signals.AskAnswered, signals.ReportsClosed), after: answered},
		{name: "comment-created-leaving-the-turn-with-the-agent", eventType: "comment.created",
			payload: map[string]any{"id": "comment-1", "ask_id": ask, "ask_state": "open", "ask_waiting_on": "agent"},
			before:  open, known: true, fact: "DispatchAskReply",
			turn:  signals.AskEvent{Kind: signals.AskCommented, Ask: ask, Issue: "LEGION-7", WaitingOn: signals.WaitsOnAgent},
			after: waitingOnAgent},
		{name: "comment-answered-leaving-the-turn-with-the-person", eventType: "comment.answered",
			payload: map[string]any{"id": "comment-2", "ask_id": ask, "ask_state": "open", "ask_waiting_on": "human"},
			before:  waitingOnAgent, known: true, fact: "DispatchAskReply",
			turn:  signals.AskEvent{Kind: signals.AskCommented, Ask: ask, Issue: "LEGION-7", WaitingOn: signals.WaitsOnHuman},
			after: open},
	}
}

// Every ask event the approvals signal reads decodes to a fact that carries the event as that
// signal reads it, at its place in the issue's log, on the live stream and from the event log alike;
// the ask that event leaves, by the lifecycle rule package signals owns, is the one the row names.
// A row is one kind of event, so an event decoded to nothing, or kept without its sequence, fails
// the row of its kind.
func testAskLifecycle(t *testing.T) {
	t.Helper()
	person := map[string]any{"kind": "user", "id": "alice"}
	for _, row := range askLifecycleRows() {
		for from, decode := range map[string]func(*testing.T, string, map[string]any, map[string]any) decodedMessage{
			"the live stream": decodeEvent, "the event log": decodeLoggedEvent,
		} {
			t.Run(row.name+" from "+from, func(t *testing.T) {
				t.Parallel()
				decoded := decode(t, row.eventType, person, row.payload)
				if got := strings.TrimPrefix(reflect.TypeOf(decoded.Fact).String(), "intake."); got != row.fact {
					t.Fatalf("%s decoded to a %s (%#v), want a %s", row.eventType, got, decoded.Fact, row.fact)
				}
				turn, seq, ok := AskTurnOf(decoded.Fact)
				if !ok || seq != askEventMark.Seq {
					t.Fatalf("the fact carries the turn %+v at %d (read: %t), want one at the event's sequence %d", turn, seq, ok, askEventMark.Seq)
				}
				if !reflect.DeepEqual(turn, row.turn) {
					t.Fatalf("the fact carries the turn %+v, want %+v", turn, row.turn)
				}
				after, changed := signals.NextAsk(row.before, row.known, turn)
				if !changed || after != row.after {
					t.Fatalf("the ask after the event = %+v (changed %t), want %+v", after, changed, row.after)
				}
			})
		}
	}
}

// readEventLog is the events testdata/dispatch-events/<name> holds, as DecodeDispatchEvent reads
// each.
func readEventLog(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	return readDispatchEvents(t, name)
}

// the project's default assignee settings event is the project's own move of it, at its sequence
// and time in the project's log: a login. An event that names no assignee, one of another type or
// another project's, and one with no sequence or time is poison, since Dispatch writes none such.
func testProjectDefaultAssignee(t *testing.T) {
	t.Helper()
	const subject = "notifications.dispatch.project.ACME.settings.project_default_assignee.updated"
	data := readDispatchEnvelope(t, "project-default-assignee.json")
	got, err := decodeMessage(subject, "ACME", nil, data)
	if err != nil {
		t.Fatalf("decode the move of the default assignee: %v", err)
	}
	want := DispatchDefaultAssignee{Assignee: "person-1@example.invalid", Seq: 4225, At: time.Date(2026, 11, 15, 7, 1, 29, 499724000, time.UTC)}
	if !reflect.DeepEqual(got.Fact, want) || got.Source != "dispatch" || got.EventID != "dispatch-563443" {
		t.Fatalf("decoded %#v from %q under %q, want %#v from dispatch under dispatch-563443", got.Fact, got.Source, got.EventID, want)
	}
	for name, edit := range map[string]func(map[string]any){
		"an event with no assignee":       func(e map[string]any) { delete(payloadOf(e), "assignee") },
		"an event with a null assignee":   func(e map[string]any) { payloadOf(e)["assignee"] = nil },
		"an event with an empty assignee": func(e map[string]any) { payloadOf(e)["assignee"] = "" },
		"an event of another type":        func(e map[string]any) { e["type"] = "settings.repo_project.updated" },
		"an event of another project":     func(e map[string]any) { e["project"] = "OTHER" },
		"an event with no sequence":       func(e map[string]any) { delete(e, "seq") },
		"an event with no time":           func(e map[string]any) { delete(e, "created_at") },
	} {
		if poison, err := decodeMessage(subject, "ACME", nil, editEnvelope(t, data, edit)); err == nil {
			t.Errorf("%s decoded to %#v, want it refused", name, poison.Fact)
		}
	}
	// A subject of another project's default assignee is none this daemon's durable takes, and is
	// refused rather than taken for its own.
	if other, err := decodeMessage("notifications.dispatch.project.OTHER.settings.project_default_assignee.updated", "ACME", nil, data); err == nil {
		t.Errorf("another project's default assignee subject decoded to %#v, want it refused", other.Fact)
	}
}

// An issue event names the issue's assignee, a login, null for none, or nothing at all from a
// Dispatch that predates assignees, which says nothing of it (as claims are). The real events of
// the capture carry it, so the decoder reads the member the replay reads from the same events.
func testIssueAssignee(t *testing.T) {
	t.Helper()
	original := readDispatchEnvelope(t, "issue-updated.json")
	const subject = "notifications.dispatch.issue.CAPTURE-3.issue.updated"
	for _, tc := range []struct {
		name   string
		edit   func(map[string]any)
		person string
		known  bool
		unread bool
	}{
		{name: "an assignee", person: "smoke", known: true, edit: func(map[string]any) {}},
		{name: "no assignee", known: true, edit: func(e map[string]any) { payloadOf(e)["assignee"] = nil }},
		{name: "an event from before assignees", edit: func(e map[string]any) { delete(payloadOf(e), "assignee") }},
		{name: "an assignee that is no login", unread: true, edit: func(e map[string]any) { payloadOf(e)["assignee"] = 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, editEnvelope(t, original, tc.edit))
			if err != nil {
				t.Fatalf("decode: %v, want no poison", err)
			}
			issue := decoded.Fact.(DispatchIssue)
			if issue.Assignee != tc.person || issue.AssigneeKnown != tc.known || (len(decoded.Unread) != 0) != tc.unread {
				t.Fatalf("assignee = %q known %t unread %q, want %q known %t unread %t", issue.Assignee, issue.AssigneeKnown, decoded.Unread, tc.person, tc.known, tc.unread)
			}
		})
	}
	// The assignee of each real event: the one that names a person, from the capture's own log.
	events := readEventLog(t, "acme-29-signal-inputs.json")
	fact, _, err := DecodeDispatchEvent("ACME", "ACME-29", events[0])
	if err != nil {
		t.Fatalf("decode the captured issue event: %v", err)
	}
	if issue := fact.(DispatchIssue); issue.Seq != 2 || issue.Assignee != "person-1@example.invalid" || !issue.AssigneeKnown {
		t.Fatalf("the captured issue event decoded to seq %d assignee %q known %t, want seq 2 person-1@example.invalid known", issue.Seq, issue.Assignee, issue.AssigneeKnown)
	}
}

// An ask's asked_of is the person a person handed it to: named by an event (null clears it) or left
// out of it. The real events of the capture carry it as null, an ask opened and an ask answered,
// and the hand-on's envelope carries a person. It is a missing member, not an empty one, that
// leaves the person as it stood.
func testAskAskedOf(t *testing.T) {
	t.Helper()
	const subject = "notifications.dispatch.issue.CAPTURE-4.ask.asked_of"
	original := readDispatchEnvelope(t, "ask-asked-of.json")
	const askID = "b1d3a9c2-5f70-4f0a-9c6e-1d2b3a4c5d6e"
	mark := record.EventMark{Seq: 13, At: time.Date(2026, 9, 23, 0, 46, 38, 0, time.UTC)}
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want signals.AskEvent
	}{
		{name: "a hand-on to a person", edit: func(map[string]any) {},
			want: signals.AskEvent{Kind: signals.AskAskedOf, Ask: askID, Issue: "CAPTURE-4", State: signals.ReportsOpen, AskedOfSet: true, AskedOf: "smoke"}},
		{name: "a hand-on back to the issue's assignee", edit: func(e map[string]any) { payloadOf(e)["asked_of"] = nil },
			want: signals.AskEvent{Kind: signals.AskAskedOf, Ask: askID, Issue: "CAPTURE-4", State: signals.ReportsOpen, AskedOfSet: true}},
		{name: "an event that names no asked_of", edit: func(e map[string]any) { delete(payloadOf(e), "asked_of") },
			want: signals.AskEvent{Kind: signals.AskAskedOf, Ask: askID, Issue: "CAPTURE-4", State: signals.ReportsOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, editEnvelope(t, original, tc.edit))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			want := DispatchAskTurn{Key: "CAPTURE-4", Turn: tc.want, Mark: mark}
			if !reflect.DeepEqual(decoded.Fact, want) {
				t.Fatalf("decoded %#v, want %#v", decoded.Fact, want)
			}
		})
	}
	events := readEventLog(t, "acme-29-signal-inputs.json")
	opened, _, err := DecodeDispatchEvent("ACME", "ACME-29", events[1])
	if err != nil {
		t.Fatalf("decode the captured ask.opened: %v", err)
	}
	turn, seq, ok := AskTurnOf(opened)
	want := signals.AskEvent{Kind: signals.AskOpened, Ask: "00000000-0000-4000-8000-00000000000e", Issue: "ACME-29", State: signals.ReportsOpen, AskedOfSet: true}
	if !ok || seq != 23 || !reflect.DeepEqual(turn, want) {
		t.Fatalf("the captured ask.opened carries the turn %+v at %d (read %t), want %+v at 23: its asked_of is null, which names nobody", turn, seq, ok, want)
	}
	answered, _, err := DecodeDispatchEvent("ACME", "ACME-29", events[2])
	if err != nil {
		t.Fatalf("decode the captured ask.answered: %v", err)
	}
	turn, seq, ok = AskTurnOf(answered)
	want = signals.AskEvent{Kind: signals.AskAnswered, Ask: "00000000-0000-4000-8000-00000000000a", Issue: "ACME-29", State: signals.ReportsClosed, AskedOfSet: true,
		Approval: &signals.AskApproval{Version: new(4), RequestedVersion: new(4)}}
	if !ok || seq != 40 || !reflect.DeepEqual(turn, want) {
		t.Fatalf("the captured ask.answered carries the turn %+v at %d (read %t), want %+v at 40", turn, seq, ok, want)
	}
}
