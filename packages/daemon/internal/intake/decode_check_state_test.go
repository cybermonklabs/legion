package intake

//ci:reads packages/contracts/fixtures/dispatch-events/check-state-changed.json TestTheIntakeDecodesACheckStateChangedEvent decodes both forms of the payload Envoy's golden writes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// checkStateChangedFixture is packages/contracts/fixtures/dispatch-events/check-state-changed.json,
// which Envoy's TestCheckStateChangedEventPayloadGolden writes from the Go payload Dispatch
// publishes: the event's type and actor, and its payload in the check form and in the set form.
type checkStateChangedFixture struct {
	Type      string          `json:"type"`
	Actor     json.RawMessage `json:"actor"`
	CheckForm json.RawMessage `json:"check_form"`
	SetForm   json.RawMessage `json:"set_form"`
}

func readCheckStateChangedFixture(t *testing.T) checkStateChangedFixture {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "dispatch-events", "check-state-changed.json"))
	if err != nil {
		t.Fatalf("read the shared fixture of check.state_changed: %v", err)
	}
	var fixture checkStateChangedFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the shared fixture of check.state_changed: %v", err)
	}
	if fixture.Type == "" || len(fixture.CheckForm) == 0 || len(fixture.SetForm) == 0 {
		t.Fatalf("the shared fixture of check.state_changed = %+v, want its type and both forms", fixture)
	}
	return fixture
}

// checkStateEventAt is when Dispatch wrote each event these tests build.
var checkStateEventAt = time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)

// checkStateEvent is one event of key's Dispatch event log at seq, as the log serves it and the
// durable stream carries it: the fixture's type and actor, with payload.
func checkStateEvent(t *testing.T, fixture checkStateChangedFixture, key string, seq int64, payload json.RawMessage) []byte {
	t.Helper()
	event, err := json.Marshal(map[string]any{
		"id": 900 + seq, "issue_key": key, "seq": seq, "type": fixture.Type, "notify": false,
		"created_at": "2026-11-21T06:00:00Z", "actor": fixture.Actor, "payload": payload,
	})
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	return event
}

// withPayloadField is payload with field set to value, or taken out where value is nil.
func withPayloadField(t *testing.T, payload json.RawMessage, field string, value any) json.RawMessage {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	if value == nil {
		delete(fields, field)
	} else {
		fields[field] = value
	}
	changed, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	return changed
}

// A check.state_changed event decodes to one DispatchCheckState, from the live consumer's decoder
// and from the decoder of the boot catch-up alike: each field of the payload the workflow reads, and
// the event's place in the issue's event log. Both forms of the shared fixture decode, the set form
// with no check and no state. An event of another project's issue is no fact. A payload the
// workflow cannot read is an error, so the consumer terminates it as poison rather than passing it
// over in silence.
func TestTheIntakeDecodesACheckStateChangedEvent(t *testing.T) {
	fixture := readCheckStateChangedFixture(t)
	for _, tc := range []struct {
		form    string
		key     string
		seq     int64
		payload json.RawMessage
		want    DispatchCheckState
	}{
		{"the check form", "ACME-1", 41, fixture.CheckForm, DispatchCheckState{
			Key: "ACME-1", Mark: record.EventMark{Seq: 41, At: checkStateEventAt}, Repo: "acme/widgets",
			Head: "1111111111111111111111111111111111111111", PullRequests: []int{70}, Checks: []string{"k1", "k2"},
			Check: "k1", CheckVersion: 1, State: "no", Reason: "fail", Roles: []string{}, CICase: "", Serial: 1}},
		{"the set form", "ACME-2", 42, fixture.SetForm, DispatchCheckState{
			Key: "ACME-2", Mark: record.EventMark{Seq: 42, At: checkStateEventAt}, Repo: "acme/widgets",
			Head: "2222222222222222222222222222222222222222", PullRequests: []int{71}, Checks: []string{},
			Check: "", CheckVersion: 0, State: "", Reason: "", Roles: []string{}, CICase: "", Serial: 1}},
		// The fixture's forms carry no role and no CI case, so a decoder that dropped either would
		// pass both; this form names each.
		{"the check form with roles and a CI run going on", "ACME-1", 44,
			withPayloadField(t, withPayloadField(t, fixture.CheckForm, "roles", []string{"implementer", "tester"}), "ci_case", "not_completed"),
			DispatchCheckState{
				Key: "ACME-1", Mark: record.EventMark{Seq: 44, At: checkStateEventAt}, Repo: "acme/widgets",
				Head: "1111111111111111111111111111111111111111", PullRequests: []int{70}, Checks: []string{"k1", "k2"},
				Check: "k1", CheckVersion: 1, State: "no", Reason: "fail", Roles: []string{"implementer", "tester"}, CICase: "not_completed", Serial: 1}},
	} {
		t.Run(tc.form, func(t *testing.T) {
			event := checkStateEvent(t, fixture, tc.key, tc.seq, tc.payload)
			subject := "notifications.dispatch.issue.ACME." + tc.key + "." + fixture.Type
			live, err := decodeMessage(subject, "ACME", nil, envelopeJSON(t, "dispatch-check-state", "dispatch", string(event)))
			if err != nil || !reflect.DeepEqual(live.Fact, tc.want) || len(live.Unread) != 0 {
				t.Fatalf("the live consumer decoded %s to %#v, unread %q, %v; want %#v and nothing unread", tc.form, live.Fact, live.Unread, err, tc.want)
			}
			caught, unread, err := DecodeDispatchEvent("ACME", tc.key, event)
			if err != nil || !reflect.DeepEqual(caught, tc.want) || len(unread) != 0 {
				t.Fatalf("the catch-up decoded %s to %#v, unread %q, %v; want %#v and nothing unread", tc.form, caught, unread, err, tc.want)
			}
			other, err := decodeMessage(subject, "LEGION", nil, envelopeJSON(t, "dispatch-check-state", "dispatch", string(event)))
			if err != nil || other.Fact != nil {
				t.Fatalf("a LEGION daemon's consumer decoded ACME's %s to %#v, %v; want no fact", tc.form, other.Fact, err)
			}
			if fact, _, err := DecodeDispatchEvent("LEGION", tc.key, event); err != nil || fact != nil {
				t.Fatalf("a LEGION daemon's catch-up decoded ACME's %s to %#v, %v; want no fact", tc.form, fact, err)
			}
		})
	}

	for _, poison := range []struct {
		shape   string
		payload json.RawMessage
		names   string
	}{
		{"a head that is no commit", withPayloadField(t, fixture.CheckForm, "head", "abc"), `head "abc"`},
		{"a head in capitals", withPayloadField(t, fixture.CheckForm, "head", strings.Repeat("A", 40)), "head"},
		{"no repo", withPayloadField(t, fixture.CheckForm, "repo", nil), "repo"},
		{"an empty repo", withPayloadField(t, fixture.CheckForm, "repo", ""), "repo"},
		{"a state outside the three words", withPayloadField(t, fixture.CheckForm, "state", "pass"), `state "pass"`},
		{"a check with no state", withPayloadField(t, fixture.CheckForm, "state", nil), "state"},
		{"a state of no check", withPayloadField(t, fixture.SetForm, "state", "yes"), "state"},
		{"another issue's payload", withPayloadField(t, fixture.CheckForm, "issue", "ACME-9"), "ACME-9"},
		{"no serial", withPayloadField(t, fixture.CheckForm, "serial", 0), "serial"},
	} {
		t.Run(poison.shape, func(t *testing.T) {
			event := checkStateEvent(t, fixture, "ACME-1", 43, poison.payload)
			live, err := decodeMessage("notifications.dispatch.issue.ACME.ACME-1."+fixture.Type, "ACME", nil,
				envelopeJSON(t, "dispatch-check-state", "dispatch", string(event)))
			if err == nil || live.Fact != nil || !strings.Contains(err.Error(), poison.names) {
				t.Fatalf("the live consumer decoded %s to %#v, %v; want an error naming %s", poison.shape, live.Fact, err, poison.names)
			}
			if fact, _, err := DecodeDispatchEvent("ACME", "ACME-1", event); err == nil || fact != nil || !strings.Contains(err.Error(), poison.names) {
				t.Fatalf("the catch-up decoded %s to %#v, %v; want an error naming %s", poison.shape, fact, err, poison.names)
			}
		})
	}
}
