package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// narrowingLine is one line the daemon wrote for a narrowing of one state request, as its own
// log holds it: the caller, the contract that caller's pod records, the contract the daemon
// declares and what that narrowing dropped (server's stateFor).
type narrowingLine struct {
	Message        string `json:"msg"`
	Caller         string `json:"caller"`
	Pod            string `json:"pod"`
	PluginContract int    `json:"pluginContract"`
	DaemonContract int    `json:"daemonApiVersion"`
	Dropped        string `json:"dropped"`
}

// stateNarrowed is GET /legion/v1/state asked from remote, answered with the body served and the
// narrowing lines that same call wrote. The two are read together because a narrowed body cannot
// say of itself that it was narrowed: the mark the daemon would put on it is the key its plugin
// refuses, so the record of the narrowing is in the daemon's log and nowhere else.
func stateNarrowed(t *testing.T, state State, remote string, claims []supervise.Claim) ([]byte, []narrowingLine) {
	t.Helper()
	var logged bytes.Buffer
	options := stateOptions(state, claims)
	options.Log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := stateAsked(t, NewServer("127.0.0.1", 8437, options).Handler, remote)

	var narrowings []narrowingLine
	for _, written := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if written == "" {
			continue
		}
		var line narrowingLine
		if err := json.Unmarshal([]byte(written), &line); err != nil {
			t.Fatalf("the daemon logged %q, which is not one JSON record: %v", written, err)
		}
		if line.Message != narrowedTheState {
			continue
		}
		narrowings = append(narrowings, line)
	}
	return body, narrowings
}

// contractServed is the `contract` mark of a state body: the two numbers it names, and whether
// the key is on the wire at all, which the zero mark is not.
func contractServed(t *testing.T, body []byte) (mark map[string]any, carried bool) {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	value, carried := served["contract"]
	if !carried {
		return nil, false
	}
	mark, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("the state's `contract` is %T, want the object the route writes: %s", value, body)
	}
	return mark, true
}

// narrowingsExpected is what the daemon's log holds for a caller at contract, in the order
// stateFor applies them: the `dropped` of every row of stateNarrowings that caller stands below.
// The fixture's state carries something of every row, so each of them writes its line. It is
// read from the table rather than written out here, so a narrowing added above the contract a
// row below reads is counted by that row rather than failing it.
func narrowingsExpected(contract int) []string {
	var dropped []string
	for _, narrowing := range stateNarrowings {
		if contract < narrowing.contract {
			dropped = append(dropped, narrowing.dropped)
		}
	}
	return dropped
}

// A narrowed answer is recorded as narrowed (dispatch://INFRA-3083, R12). The route serves a
// caller below the contract that carries a field a body without it, which is how a plugin older
// than the one that strips unknown keys goes on reading the state; the body itself may say
// nothing of that, since a key added to a narrowed body is exactly what the narrowing exists to
// prevent. So the daemon writes the narrowing to its own log — one line naming the caller, the
// contract that caller's pod records, the contract the daemon declares and what was dropped —
// and the body carries those same two numbers for a caller at or above the contract that added
// the mark and for no caller below it.
//
// The rows read the answer and the log of that same call, which is what makes a narrowing with
// no line fail here: a body with the keys gone and nothing anywhere saying so is the state this
// requirement exists to end, where an absent fact and a withheld fact read alike.
func TestANarrowedBodySaysSo(t *testing.T) {
	// A caller one contract below the decided record's narrowing is below that row and below
	// every row of stateNarrowings above it — the issues' answers with the waiting line's own
	// fields, and the merge marks — so its request writes one line for each of them, the
	// record's own first, since the table is applied lowest contract first. Between them they
	// name every key the body is checked for: the `contract` mark and the record's keys from
	// the first line, the four merge-mark keys from the last.
	t.Run("a caller below the contract is told nothing, and the log says what was taken", func(t *testing.T) {
		const remote = statePodAddr + ":41000"
		body, narrowings := stateNarrowed(t, populatedState(t), remote, podClaims(recordFieldsContract-1))

		if mark, carried := contractServed(t, body); carried {
			t.Errorf("the body served to a pod below %d carries the mark %v; a key in a narrowed body is what the narrowing exists to prevent",
				recordFieldsContract, mark)
		}
		taken := append(append([]string{"contract"}, recordFieldKeys...), MergeMarkFields...)
		for _, key := range taken {
			if bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod below %d carries %q, which its plugin's schema does not name: %s",
					recordFieldsContract, key, body)
			}
		}

		want := narrowingsExpected(recordFieldsContract - 1)
		if len(narrowings) != len(want) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, the mark's own and one for each contract above it: %+v",
				len(narrowings), recordFieldsContract-1, len(want), narrowings)
		}
		dropped := ""
		for _, line := range narrowings {
			if line.Caller != remote || line.Pod != statePodUID {
				t.Errorf("the line names the caller %q in pod %q, want %q in %q", line.Caller, line.Pod, remote, statePodUID)
			}
			if line.PluginContract != recordFieldsContract-1 || line.DaemonContract != DaemonAPIVersion {
				t.Errorf("the line names the contracts %d and %d, want the caller's %d and the daemon's %d",
					line.PluginContract, line.DaemonContract, recordFieldsContract-1, DaemonAPIVersion)
			}
			dropped += " " + line.Dropped
		}
		// What was dropped, key by key, so a line that named the contract alone fails here.
		for _, key := range taken {
			if !strings.Contains(dropped, key) {
				t.Errorf("the lines say they dropped%q, which does not name %s", dropped, key)
			}
		}
	})

	// The body of a caller at the contract: the two numbers stand beside the keys the newest
	// narrowings added, the decided record's among them, so a reader that has the record can say
	// which contract it reads.
	t.Run("a caller at the contract carries the two numbers beside the record", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, populatedState(t), statePodAddr+":41000", podClaims(mergeMarksContract))

		mark, carried := contractServed(t, body)
		if !carried {
			t.Fatalf("the body served to a pod at %d carries no `contract` mark: %s", mergeMarksContract, body)
		}
		want := map[string]any{"daemon": float64(DaemonAPIVersion), "caller": float64(mergeMarksContract)}
		if len(mark) != len(want) || mark["daemon"] != want["daemon"] || mark["caller"] != want["caller"] {
			t.Errorf("the mark is %v, want %v: the contract the daemon declares and the one the caller's pod records", mark, want)
		}
		for _, key := range append(append([]string{}, recordFieldKeys...), MergeMarkFields...) {
			if !bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod at %d carries no %q; the mark is meant to stand beside the record: %s",
					mergeMarksContract, key, body)
			}
		}
		// A caller at this contract is narrowed by the rows above it alone, of which there are
		// none while this contract is the highest the table holds: the log names each of them
		// and never the mark's own arm, whose line would mean the record had been taken out of
		// the body just read.
		wantLines := narrowingsExpected(mergeMarksContract)
		if len(narrowings) != len(wantLines) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, one for each contract above it: %+v",
				len(narrowings), mergeMarksContract, len(wantLines), narrowings)
		}
		for i, line := range narrowings {
			if line.Dropped != wantLines[i] {
				t.Errorf("line %d of a caller at %d says it dropped %q, want %q", i, mergeMarksContract, line.Dropped, wantLines[i])
			}
		}
	})

	// The operator's own answer, `legion state --json`: the daemon places that caller in no pod,
	// so it is served the state whole and narrowed by nothing, and the mark names the contract it
	// was built at and no caller's.
	t.Run("the operator's own answer says which contract it was built at", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, populatedState(t), "192.0.2.1:41000", podClaims(recordFieldsContract-1))

		mark, carried := contractServed(t, body)
		if !carried {
			t.Fatalf("the operator's own answer carries no `contract` mark: %s", body)
		}
		if mark["daemon"] != float64(DaemonAPIVersion) {
			t.Errorf("the operator's answer was built at %v, want the contract the daemon declares, %d", mark["daemon"], DaemonAPIVersion)
		}
		if _, narrowed := mark["caller"]; narrowed || len(mark) != 1 {
			t.Errorf("the operator's answer carries the mark %v, want the daemon's number alone: nothing narrows that reader", mark)
		}
		if len(narrowings) != 0 {
			t.Errorf("the daemon narrowed the operator's own answer: %+v", narrowings)
		}
	})

	// Every narrowing says what it dropped, not only the mark's own: a caller below every
	// contract in stateNarrowings is served one line per row of it, each naming its own keys.
	// One line for all of them, or a line that named the contract and nothing else, would leave
	// a reader of the log to find out elsewhere what left the body.
	t.Run("each narrowing names its own keys", func(t *testing.T) {
		_, narrowings := stateNarrowed(t, populatedState(t), statePodAddr+":41000", podClaims(supervise.PodUpdateContract-1))

		if len(narrowings) != len(stateNarrowings) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller below every contract, want %d: %+v",
				len(narrowings), len(stateNarrowings), narrowings)
		}
		dropped := map[string]bool{}
		for i, line := range narrowings {
			if line.Dropped == "" {
				t.Errorf("the line for contract %d says nothing of what it dropped", stateNarrowings[i].contract)
			}
			if dropped[line.Dropped] {
				t.Errorf("two narrowings report the same keys, %q: each arm names its own", line.Dropped)
			}
			dropped[line.Dropped] = true
			if line.Dropped != stateNarrowings[i].dropped {
				t.Errorf("the lines are out of the table's order: line %d says %q, want %q",
					i, line.Dropped, stateNarrowings[i].dropped)
			}
		}
	})
}

// recordFieldsContract is never above this daemon's declared contract. A caller is served the
// nine keys only where its pod records that number or more (podContract), so a constant above
// DaemonAPIVersion is a contract no caller could ever reach and nine keys nothing serves, with
// every row above this one still green. version.go tells the next author to consider renumbering
// DaemonAPIVersion at each merge, and this is what makes a renumber that leaves this constant
// behind fail here, as every other narrowing constant's own bound does.
func TestTheRecordFieldsContractIsNotAboveTheDaemonsOwn(t *testing.T) {
	if recordFieldsContract > DaemonAPIVersion {
		t.Fatalf("recordFieldsContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			recordFieldsContract, DaemonAPIVersion)
	}
}
