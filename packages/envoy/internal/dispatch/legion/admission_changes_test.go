package legion

//ci:reads packages/contracts/fixtures/daemon-api/state-events.json TestTheAdmissionDecodeReadsTheDaemonsGoldens TestTheAdmissionObservationKeepsItsShape os.ReadFile (`packages/envoy/internal/dispatch/legion/intake_test.go#readStateEventsGolden`, ../../../../contracts/fixtures/daemon-api): the daemon's golden of its state lane, whose first admission event carries each admission block these tests read in a state message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The daemon's goldens of its admission record, copied into testdata from the branches that serve
// it (testdata/README.md names each one's commit and the command that writes it again): the state
// of dispatch://INFRA-5616's daemon with two changes of the cap, and its state before the first
// change, which serves `capChanges` empty; the state of dispatch://INFRA-5899's daemon with five
// changes of admission by signals, and its state before the first, which serves `signalChanges`
// empty.
const (
	capChangesGolden         = "state-capchanges.json"
	capChangesEmptyGolden    = "state-capchanges-empty.json"
	signalChangesGolden      = "state-signalchanges.json"
	signalChangesEmptyGolden = "state-signalchanges-empty.json"
)

// admissionGolden is the bytes of name, one of the daemon's goldens in testdata.
func admissionGolden(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read the daemon's golden %s: %v", name, err)
	}
	return body
}

// mustEncode is value's JSON.
func mustEncode(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return encoded
}

// withAdmissionMembers is the state document body with each of members set in its admission block
// to the value given, or taken out of it where the value is nil: a document the daemon did not
// write, which a test names so where it reads one.
func withAdmissionMembers(t *testing.T, body []byte, members map[string]json.RawMessage) []byte {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode the state document: %v", err)
	}
	block := goldenAdmission(t, body)
	for member, value := range members {
		if value == nil {
			delete(block, member)
			continue
		}
		block[member] = value
	}
	document["admission"] = mustEncode(t, block)
	return mustEncode(t, document)
}

// admissionRead is the admission block of a state document as one of the two paths Dispatch reads
// such a block on decodes it, and the `admission` observation that path records of it, the value
// legion_observations is written.
type admissionRead struct {
	path        string
	admission   *Admission
	observation json.RawMessage
}

// readAdmissionEachWay reads the admission block of the state document body on each path Dispatch
// reads one on: a whole read, which decodes the state route's body (Client.State) and records the
// state's observations (State.Observations), and a state message, whose admission event
// decodeStateMessage decodes and observes. The message is the daemon's own (withAdmissionEvent),
// carrying body's block.
func readAdmissionEachWay(t *testing.T, body []byte) []admissionRead {
	t.Helper()
	state, _, err := New(stateServer(t, string(body)).URL).State(t.Context())
	if err != nil {
		t.Fatalf("decode the state document as a whole read does: %v", err)
	}
	var document struct {
		Admission json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("read the state document's admission block: %v", err)
	}
	envelope, seq := withAdmissionEvent(t, document.Admission)
	message, err := decodeStateMessage(&envelope, nil)
	if err != nil {
		t.Fatalf("decode the state message carrying the block: %v", err)
	}
	index := slices.IndexFunc(message.Events, func(event stateEvent) bool { return event.Seq == seq })
	if index < 0 {
		t.Fatalf("the decoded state message holds no event at sequence %d, the admission event", seq)
	}
	event := message.Events[index]
	return []admissionRead{
		{path: "a whole read", admission: state.Admission, observation: admissionObservation(t, "a whole read", state.Observations())},
		{path: "a state message", admission: event.Admission, observation: admissionObservation(t, "a state message", event.observations)},
	}
}

// withAdmissionEvent is the daemon's own state message, the first of its golden of its state lane
// (readStateEventsGolden) to carry an admission event, with block in place of that event's
// admission block, and the event's sequence.
func withAdmissionEvent(t *testing.T, block json.RawMessage) (contracts.Envelope, int64) {
	t.Helper()
	for _, step := range readStateEventsGolden(t) {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(step.envelope.Payload), &payload); err != nil {
			t.Fatalf("decode the message of step %q: %v", step.name, err)
		}
		var events []map[string]json.RawMessage
		if err := json.Unmarshal(payload["events"], &events); err != nil {
			t.Fatalf("decode the events of step %q: %v", step.name, err)
		}
		for _, event := range events {
			var kind string
			if err := json.Unmarshal(event["kind"], &kind); err != nil || kind != eventAdmission {
				continue
			}
			var seq int64
			if err := json.Unmarshal(event["seq"], &seq); err != nil {
				t.Fatalf("read the sequence of step %q's admission event: %v", step.name, err)
			}
			event["admission"] = block
			payload["events"] = mustEncode(t, events)
			envelope := step.envelope
			envelope.Payload = string(mustEncode(t, payload))
			return envelope, seq
		}
	}
	t.Fatal("the daemon's state events golden carries no admission event, so no state message here carries a block")
	return contracts.Envelope{}, 0
}

// admissionObservation is the value of the one `admission` observation among observations, which
// path recorded.
func admissionObservation(t *testing.T, path string, observations []Observation) json.RawMessage {
	t.Helper()
	for _, observation := range observations {
		if observation.IssueKey == AdmissionKey && observation.Field == FieldAdmission {
			return observation.Value
		}
	}
	t.Fatalf("%s records no admission block among %v", path, observations)
	return nil
}

// sameServed reports whether got is want: both served or neither, with the same entries, where an
// empty list and none are alike.
func sameServed[T any](got, want served[T]) bool {
	return got.Served == want.Served && len(got.Entries) == len(want.Entries) &&
		(len(want.Entries) == 0 || reflect.DeepEqual(got.Entries, want.Entries))
}

// servedText is list as a failure names it: whether it is served, and its entries as JSON.
func servedText[T any](t *testing.T, list served[T]) string {
	t.Helper()
	return fmt.Sprintf("served=%t %s", list.Served, mustEncode(t, list.Entries))
}

// TestTheAdmissionDecodeReadsTheDaemonsGoldens: the admission block's two records of changes
// decode from the daemon's own goldens (dispatch://INFRA-2918, D17), on both paths Dispatch reads a
// block on, a whole read and a state message's admission event: `capChanges`
// (dispatch://INFRA-5616) and `signalChanges` (dispatch://INFRA-5899), each entry in every member
// the daemon writes. The cap's record holds a clear, whose entry carries no `cap`. The signals'
// record holds every `what` there is, a hold, a value, a limit and the switch, whose entry names
// no signal, and a value the daemon cannot compute, whose entry carries no value and says why
// (`absent`). A list the daemon serves empty, as each daemon does before its first change, is
// served with no entries; a list the document leaves out, as each golden leaves out the other's
// list, or sends as `null` (a document made by hand from the first golden), is not served: the
// admission eval is registered for a daemon that keeps a record and for no other (D18).
func TestTheAdmissionDecodeReadsTheDaemonsGoldens(t *testing.T) {
	at := func(text string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		return parsed
	}
	controller := Actor{Kind: "controller", Session: "ses_controller"}
	operator := Actor{Kind: "operator"}
	capChanges := []CapChange{
		{ID: 2, Cap: new(5), CapBefore: 8, CapAfter: 5, Ceiling: 8, SetBy: controller, SetAt: at("2026-09-22T09:00:00Z"),
			Reason: "the merge queue waits 52 minutes, above its limit of 45"},
		// A clear, which sets no cap.
		{ID: 1, CapBefore: 3, CapAfter: 8, Ceiling: 8, SetBy: operator, SetAt: at("2026-09-21T18:00:00Z"),
			Reason: "the provider outage is over"},
	}
	signalChanges := []SignalChange{
		{ID: 5, At: at("2026-11-26T07:00:00Z"), Signal: new("kills"), What: "holds", Value: new(1.0)},
		{ID: 4, At: at("2026-11-26T07:00:00Z"), Signal: new("kills"), What: "value", Value: new(3.0)},
		{ID: 3, At: at("2026-11-26T06:00:00Z"), Signal: new("kills"), What: "limit", Value: new(2.0),
			SetBy: &controller, Reason: new("the owner set the limit in the weekly plan")},
		// A value the daemon cannot compute: no value, and why.
		{ID: 2, At: at("2026-11-26T05:00:00Z"), Signal: new("queue"), What: "value",
			Absent: new("the daemon has not begun keeping the merge queue's events")},
		// The switch, which names no signal.
		{ID: 1, At: at("2026-11-26T05:00:00Z"), What: "switch", Value: new(1.0),
			SetBy: &operator, Reason: new("the owner turned admission by signals on")},
	}
	var capEntries []map[string]json.RawMessage
	if err := json.Unmarshal(goldenAdmission(t, admissionGolden(t, capChangesGolden))["capChanges"], &capEntries); err != nil || len(capEntries) != 2 {
		t.Fatalf("the golden's capChanges is %v (error %v), want its two entries", capEntries, err)
	}
	if _, carried := capEntries[1]["cap"]; carried {
		t.Fatalf("the golden's clear carries a cap, %s, so this test reads no clear without one", capEntries[1]["cap"])
	}

	for _, tc := range []struct {
		name string
		body []byte
		// absent is the members the block leaves out.
		absent        []string
		capChanges    served[CapChange]
		signalChanges served[SignalChange]
	}{
		{name: capChangesGolden, body: admissionGolden(t, capChangesGolden), absent: []string{"signalChanges"},
			capChanges: served[CapChange]{Served: true, Entries: capChanges}},
		{name: signalChangesGolden, body: admissionGolden(t, signalChangesGolden), absent: []string{"capChanges"},
			signalChanges: served[SignalChange]{Served: true, Entries: signalChanges}},
		{name: capChangesEmptyGolden, body: admissionGolden(t, capChangesEmptyGolden), absent: []string{"signalChanges"},
			capChanges: served[CapChange]{Served: true}},
		{name: signalChangesEmptyGolden, body: admissionGolden(t, signalChangesEmptyGolden), absent: []string{"capChanges"},
			signalChanges: served[SignalChange]{Served: true}},
		{name: "both lists null, by hand from " + capChangesGolden,
			body: withAdmissionMembers(t, admissionGolden(t, capChangesGolden), map[string]json.RawMessage{
				"capChanges": json.RawMessage("null"), "signalChanges": json.RawMessage("null"),
			})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := goldenAdmission(t, tc.body)
			for _, member := range tc.absent {
				if value, carried := block[member]; carried {
					t.Fatalf("the block carries %s, %s, so this case reads no block that leaves it out", member, value)
				}
			}
			for _, read := range readAdmissionEachWay(t, tc.body) {
				if read.admission == nil {
					t.Fatalf("%s decodes no admission block", read.path)
				}
				if got := read.admission.CapChanges; !sameServed(got, tc.capChanges) {
					t.Errorf("%s decodes capChanges to %s, want %s", read.path, servedText(t, got), servedText(t, tc.capChanges))
				}
				if got := read.admission.SignalChanges; !sameServed(got, tc.signalChanges) {
					t.Errorf("%s decodes signalChanges to %s, want %s", read.path, servedText(t, got), servedText(t, tc.signalChanges))
				}
			}
		})
	}
}

// TestTheAdmissionObservationKeepsItsShape: the `admission` observation records the block's own
// members alone (dispatch://INFRA-2918, D17), on a whole read and on a state message alike. Of a
// state whose block carries both records of changes, dispatch://INFRA-5899's golden, which carries
// `signalChanges`, with dispatch://INFRA-5616's `capChanges` added by hand, it is, byte for byte,
// the observation of the same state with neither: the stored value keeps the shape it had before
// a daemon kept either record, and the Legion board, which reads it (AdmissionOf), reads what it
// read then.
func TestTheAdmissionObservationKeepsItsShape(t *testing.T) {
	signals := admissionGolden(t, signalChangesGolden)
	both := withAdmissionMembers(t, signals, map[string]json.RawMessage{
		"capChanges": goldenAdmission(t, admissionGolden(t, capChangesGolden))["capChanges"],
	})
	neither := withAdmissionMembers(t, signals, map[string]json.RawMessage{"capChanges": nil, "signalChanges": nil})
	withLists, withoutLists := readAdmissionEachWay(t, both), readAdmissionEachWay(t, neither)
	for index, read := range withLists {
		if !read.admission.CapChanges.Served || !read.admission.SignalChanges.Served {
			t.Fatalf("%s decodes capChanges to %s and signalChanges to %s, want both served, or this test proves nothing",
				read.path, servedText(t, read.admission.CapChanges), servedText(t, read.admission.SignalChanges))
		}
		if without := withoutLists[index]; !bytes.Equal(read.observation, without.observation) {
			t.Errorf("%s records the block with both records of changes as\n%s\nwant the observation of the same block with neither\n%s",
				read.path, read.observation, without.observation)
		}
	}
}
