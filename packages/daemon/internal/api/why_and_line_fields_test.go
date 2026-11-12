package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// whyAndLineFieldsState is waitingLineState carrying both halves of whyAndLineFieldsContract:
// every member of the line carries the sentence the projection renders for it
// (WaitingLineView.Reason) beside the part, kind, ranks and slot it already had, and its issue
// carries that same answer under `why`; LEGION-60, an issue whose claim has stopped, carries a
// `why` and stands in no line at all, so the strip is read on an issue of each kind — one that
// keeps a `waitingLine` the earlier contract describes, and one that keeps no key of either.
func whyAndLineFieldsState() State {
	state := waitingLineState()
	reasons := map[string]string{
		"LEGION-40": "A person answered it, and its work came back at 2026-11-08T08:00:00Z. It is first of one issue whose work came back, and it already holds a slot.",
		"LEGION-20": "Its tree has not been opened. It is first of two issues that have not started, behind one issue whose work came back.",
		"LEGION-30": "Its tree has not been opened. It is second of two issues that have not started, behind one issue whose work came back.",
	}
	issues := make(map[string]Issue, len(state.Issues)+1)
	for key, issue := range state.Issues {
		if issue.WaitingLine != nil {
			// The member is rebuilt rather than written through: waitingLineState's own views
			// are what TestWaitingLinePartAndKeyAreServedOnTheWire holds to the wire.
			member := *issue.WaitingLine
			member.Reason = reasons[key]
			issue.WaitingLine = &member
			issue.Why = &WhyView{State: WhyWaiting, Reason: member.Reason}
		}
		issues[key] = issue
	}
	issues["LEGION-60"] = Issue{Key: "LEGION-60", Phase: phase.Implementing, Status: "in_progress",
		Why: &WhyView{State: WhyClaimStopped,
			Reason: "Its phase is implementing and the implementer's claim is stranded at failed, so nothing will wake it."}}
	state.Issues = issues
	return state
}

// whysServed is the `why` of every issue of a state body that carries one, by issue key. The
// key is read by name out of the marshalled body, as waitingLinesServed reads the places,
// because whether the key is on the wire at all is what the strip is read by: an issue served
// to a caller below whyAndLineFieldsContract is to carry no such key, whether that caller's own
// schema would refuse one it does not name or quietly drop it.
func whysServed(t *testing.T, body []byte) map[string]WhyView {
	t.Helper()
	answers := make(map[string]WhyView)
	for key, raw := range issueKeyServed(t, body, "why") {
		var why WhyView
		if err := json.Unmarshal(raw, &why); err != nil {
			t.Fatalf("decode the why of %s, %s: %v", key, raw, err)
		}
		answers[key] = why
	}
	return answers
}

// lineFieldsOf is one member view as the keys it writes on the wire, in the shape
// waitingLineFieldsServed reads a served member in: the fields the view holds facts for, each
// left as its JSON text, which is what a caller at the contract is to be served.
func lineFieldsOf(t *testing.T, view WaitingLineView) map[string]string {
	t.Helper()
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal the member %+v: %v", view, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("read the member back, %s: %v", encoded, err)
	}
	written := make(map[string]string, len(fields))
	for name, value := range fields {
		written[name] = string(value)
	}
	return written
}

// fieldNames is the keys of one served member, sorted, for a failure that names what reached the
// wire rather than printing two objects to be compared by eye.
func fieldNames(fields map[string]string) []string {
	return slices.Sorted(maps.Keys(fields))
}

// Three bands read this route, and the two strips make them in order (api's stateFor). A caller
// below waitingLineContract reads no `waitingLine` and no `why` at all, which withoutWaitingLine
// and withoutWhyAndLineFields leave it. A caller from waitingLineContract up to the contract
// below whyAndLineFieldsContract reads `waitingLine` with `place` and `length` alone and no
// `why`, the body the first of those contracts describes, which withoutWhyAndLineFields leaves
// it. A caller at whyAndLineFieldsContract reads every field of the line and the answer of
// every issue that carries one.
//
// Each band is read out of the marshalled body by key, as waitingLinesServed reads a place, so
// what is proved is what reaches the wire and not what a Go struct holds: a field the strip
// zeroed but the encoder still wrote would pass a reader of the struct and fail a reader of its
// keys, and it is the keys a plugin's schema meets.
func TestWaitingLineContractBands(t *testing.T) {
	state := whyAndLineFieldsState()
	full := lineFieldsOf(t, *state.Issues["LEGION-20"].WaitingLine)
	for _, name := range []string{"partPlace", "holdsSlot", "reason"} {
		if _, carried := full[name]; !carried {
			t.Fatalf("the fixture's member carries no %s, only %v; this test proves nothing", name, fieldNames(full))
		}
	}

	// The first band: below the contract that added the field, no member of the line and no
	// issue carries a key of either half.
	below := stateFrom(t, state, statePodAddr+":4100", podClaims(waitingLineContract-1))
	if served := waitingLineFieldsServed(t, below); len(served) != 0 {
		t.Errorf("the pod below %d is served the members %+v, want none: the field is not its contract's",
			waitingLineContract, served)
	}
	if answers := whysServed(t, below); len(answers) != 0 {
		t.Errorf("the pod below %d is served the answers %+v, want none: `why` is not its contract's either",
			waitingLineContract, answers)
	}

	// The second band: from the contract that added the field to the one below the contract
	// that added these, the member carries its place and its length and nothing else, and no
	// issue carries an answer. Both edges of the band are read, since a strip applied at the
	// wrong edge would serve one of them a body of the band beside it.
	for _, contract := range []int{waitingLineContract, whyAndLineFieldsContract - 1} {
		body := stateFrom(t, state, statePodAddr+":4100", podClaims(contract))
		served := waitingLineFieldsServed(t, body)
		if len(served) != len(state.Admission.Waiting) {
			t.Errorf("the pod at %d is served %d members of the line, want the %d standing in it: %s",
				contract, len(served), len(state.Admission.Waiting), body)
		}
		for _, key := range state.Admission.Waiting {
			fields, carried := served[key]
			if !carried {
				t.Fatalf("the pod at %d is served no %s for %s, want its place: %s",
					contract, WaitingLineField, key, body)
			}
			member := lineFieldsOf(t, *state.Issues[key].WaitingLine)
			want := map[string]string{"place": member["place"], "length": member["length"]}
			if !reflect.DeepEqual(fields, want) {
				t.Errorf("the pod at %d is served %s's %s as %v, want its place and its length alone, %v",
					contract, key, WaitingLineField, fields, want)
			}
		}
		if answers := whysServed(t, body); len(answers) != 0 {
			t.Errorf("the pod at %d is served the answers %+v, want none: `why` arrives at %d",
				contract, answers, whyAndLineFieldsContract)
		}
	}

	// The third band: at the contract that added them, every field the member holds a fact for
	// reaches the wire, `reason` among them, and so does the answer of every issue carrying one.
	at := stateFrom(t, state, statePodAddr+":4100", podClaims(whyAndLineFieldsContract))
	served := waitingLineFieldsServed(t, at)
	for _, key := range state.Admission.Waiting {
		want := lineFieldsOf(t, *state.Issues[key].WaitingLine)
		if !reflect.DeepEqual(served[key], want) {
			t.Errorf("the pod at %d is served %s's %s as %v, want every field of it, %v",
				whyAndLineFieldsContract, key, WaitingLineField, served[key], want)
		}
		for _, name := range []string{"part", "kind", "partPlace", "partLength", "holdsSlot", "reason"} {
			if _, carried := served[key][name]; !carried {
				t.Errorf("the pod at %d is served %s's %s without %s, only %v",
					whyAndLineFieldsContract, key, WaitingLineField, name, fieldNames(served[key]))
			}
		}
	}
	answers := whysServed(t, at)
	for key, issue := range state.Issues {
		served, carried := answers[key]
		if issue.Why == nil {
			if carried {
				t.Errorf("the pod at %d is served %s the answer %+v, want none: it carries no `why`",
					whyAndLineFieldsContract, key, served)
			}
			continue
		}
		if !carried || served != *issue.Why {
			t.Errorf("the pod at %d is served %s the answer %+v (carried %v), want its own %+v",
				whyAndLineFieldsContract, key, served, carried, *issue.Why)
		}
	}
}

// whyAndLineFieldsContract is the contract whose entry in version.go added the issues' answer
// and the waiting line's own fields, and this daemon's own contract is never below it: a caller
// at this contract is served both halves. It stands above waitingLineContract because the band
// between the two is the whole reason the second strip exists: a caller there is served the
// field the earlier contract describes and no field of this one.
func TestWhyAndLineFieldsContractIsTheContractThatAddedThem(t *testing.T) {
	const addedWhyAndLineFields = 76
	if whyAndLineFieldsContract != addedWhyAndLineFields {
		t.Fatalf("whyAndLineFieldsContract = %d, want %d, the contract whose entry in version.go added each issue's answer and the waiting line's own fields",
			whyAndLineFieldsContract, addedWhyAndLineFields)
	}
	if whyAndLineFieldsContract > DaemonAPIVersion {
		t.Fatalf("whyAndLineFieldsContract = %d, above DaemonAPIVersion %d: no caller could be served either half",
			whyAndLineFieldsContract, DaemonAPIVersion)
	}
	if whyAndLineFieldsContract <= waitingLineContract {
		t.Fatalf("whyAndLineFieldsContract = %d, not above waitingLineContract %d: the band served place and length alone would be empty",
			whyAndLineFieldsContract, waitingLineContract)
	}
}
