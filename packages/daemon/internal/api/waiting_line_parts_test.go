package api

import (
	"reflect"
	"strings"
	"testing"
)

// waitingLineFieldsServed is the `waitingLine` object of every issue of a state body that
// carries one, each key of it left as the JSON text the wire holds, by issue key. It reads the
// keys rather than decoding into WaitingLineView, as waitingLinesServed does, because what this
// proves is which keys reach the wire at all: a field of the part the member does not stand in
// is to be absent, and a member holding no slot is to carry `holdsSlot` written `false` rather
// than nothing.
func waitingLineFieldsServed(t *testing.T, body []byte) map[string]map[string]string {
	t.Helper()
	served := make(map[string]map[string]string)
	for key, raw := range issueKeyServed(t, body, WaitingLineField) {
		served[key] = jsonFields(t, "the "+WaitingLineField+" of "+key, raw)
	}
	return served
}

// The waiting line's own fields reach the wire under the names a program reads them by, and a
// member carries the fields of its own part alone: the answered root, whose work came back,
// carries `since` and no rank, and the two roots that have never started carry `rootRank`,
// `ownRank` and `rootKey` and no `since`. `holdsSlot` is on every member, written `false` for
// one that holds no slot: the view carries a pointer so that the value is a value and not an
// absence (WaitingLineView.HoldsSlot).
func TestWaitingLinePartAndKeyAreServedOnTheWire(t *testing.T) {
	state := waitingLineState()
	body := stateFrom(t, state, statePodAddr+":4100", podClaims(DaemonAPIVersion))
	served := waitingLineFieldsServed(t, body)

	want := map[string]map[string]string{
		"LEGION-40": {"place": "1", "length": "3", "part": `"returned"`, "kind": `"answered"`,
			"partPlace": "1", "partLength": "1", "holdsSlot": "true", "since": `"2026-11-08T08:00:00Z"`},
		"LEGION-20": {"place": "2", "length": "3", "part": `"starting"`, "kind": `"queued_root"`,
			"partPlace": "1", "partLength": "2", "holdsSlot": "false",
			"rootRank": `"C"`, "ownRank": `"C"`, "rootKey": `"LEGION-20"`},
		"LEGION-30": {"place": "3", "length": "3", "part": `"starting"`, "kind": `"queued_root"`,
			"partPlace": "2", "partLength": "2", "holdsSlot": "false",
			"rootRank": `"E"`, "ownRank": `"E"`, "rootKey": `"LEGION-30"`},
	}
	for _, member := range state.Admission.Waiting {
		fields, carried := served[member]
		if !carried {
			t.Fatalf("%s stands in %v and is served no %s: %s", member, state.Admission.Waiting, WaitingLineField, body)
		}
		if !reflect.DeepEqual(fields, want[member]) {
			t.Errorf("%s's %s is served %s, want %s", member, WaitingLineField, said(fields), said(want[member]))
		}
	}
	// The two absences the body above is held to, named here as well, so a failure says which
	// rule broke rather than printing two objects to be compared by eye.
	for name, member := range map[string]string{"since": "LEGION-20", "rootKey": "LEGION-40"} {
		if value, carried := served[member][name]; carried {
			t.Errorf("%s is served `%s`: %s, want no such key: it is a field of the part %s does not stand in",
				member, name, value, member)
		}
	}
	if value, carried := served["LEGION-20"]["holdsSlot"]; !carried || value != "false" {
		t.Errorf("LEGION-20 is served holdsSlot %q (carried %v), want `false` written out: it holds no slot, which is a fact of the line",
			value, carried)
	}
}

// said is one served `waitingLine` object as a line of text, its keys in the order fieldNames
// puts them in, for a failure a reader can compare key by key.
func said(fields map[string]string) string {
	names := fieldNames(fields)
	written := make([]string, 0, len(names))
	for _, name := range names {
		written = append(written, name+": "+fields[name])
	}
	return "{" + strings.Join(written, ", ") + "}"
}
