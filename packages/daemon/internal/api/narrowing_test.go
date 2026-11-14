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
// log holds it: the caller, the contract that narrowing followed, the contract the daemon
// declares, the clause a value strip's line carries and a band's does not, and what that
// narrowing dropped (shaping's recordShaping).
type narrowingLine struct {
	Message        string `json:"msg"`
	Caller         string `json:"caller"`
	Pod            string `json:"pod"`
	PluginContract int    `json:"pluginContract"`
	DaemonContract int    `json:"daemonApiVersion"`
	Follows        string `json:"follows"`
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

// narrowingsExpected is what the daemon's log holds for a caller whose pod records contract, in
// the order recordShaping writes the lines: the `dropped` of every row of stateNarrowings the
// caller's strip stands below, and then the two value strips, which follow its restore. The
// bands are read from the table rather than written out here, so a narrowing added above the
// contract a row below reads is counted by that row rather than failing it.
//
// The two strips hold no row of that table, so they are read from what the shaping itself
// reports it took off state (tookOff), the one place their conditions are written. The fixture
// carries a retired stranded entry and no wait answered by a value younger than the floor, so a
// caller below 56 is expected one value line and no caller is expected two.
func narrowingsExpected(state State, contract int) []string {
	restore, strip := restoreContract(contract), stripContract(contract)
	var dropped []string
	for _, narrowing := range stateNarrowings {
		if strip < narrowing.contract {
			dropped = append(dropped, narrowing.dropped)
		}
	}
	var took tookOff
	shapeState(state, restore, strip, &took)
	if took.retiredStranded {
		dropped = append(dropped, droppedRetiredStranded)
	}
	if took.waitAnswer {
		dropped = append(dropped, droppedWaitAnswer)
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
	// A caller one contract below the mark is below that narrowing and below every row of
	// stateNarrowings above it, so its request writes one line for each of them, the mark's own
	// first, since the table is applied lowest contract first and the line below names the keys
	// of that one arm.
	t.Run("a caller below the contract is told nothing, and the log says what was taken", func(t *testing.T) {
		const remote = statePodAddr + ":41000"
		state := populatedState(t)
		body, narrowings := stateNarrowed(t, state, remote, podClaims(recordFieldsContract-1))

		if mark, carried := contractServed(t, body); carried {
			t.Errorf("the body served to a pod below %d carries the mark %v; a key in a narrowed body is what the narrowing exists to prevent",
				recordFieldsContract, mark)
		}
		for _, key := range append([]string{"contract"}, recordFieldKeys...) {
			if bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod below %d carries %q, which its plugin's schema does not name: %s",
					recordFieldsContract, key, body)
			}
		}

		want := narrowingsExpected(state, recordFieldsContract-1)
		if len(narrowings) != len(want) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, the mark's own and one for each contract above it: %+v",
				len(narrowings), recordFieldsContract-1, len(want), narrowings)
		}
		line := narrowings[0]
		if line.Caller != remote || line.Pod != statePodUID {
			t.Errorf("the line names the caller %q in pod %q, want %q in %q", line.Caller, line.Pod, remote, statePodUID)
		}
		if line.PluginContract != recordFieldsContract-1 || line.DaemonContract != DaemonAPIVersion {
			t.Errorf("the line names the contracts %d and %d, want the caller's %d and the daemon's %d",
				line.PluginContract, line.DaemonContract, recordFieldsContract-1, DaemonAPIVersion)
		}
		// What was dropped, key by key, so a line that named the contract alone — or one arm's
		// keys under another arm's narrowing — fails here.
		for _, key := range append([]string{"contract"}, recordFieldKeys...) {
			if !strings.Contains(line.Dropped, key) {
				t.Errorf("the line says it dropped %q, which does not name %s", line.Dropped, key)
			}
		}
		// The bands above the mark's own, which this caller stands below as well: each writes a
		// line of its own naming that arm's keys, so the lines are read apart rather than as
		// one. What each should say is read from the table (narrowingsExpected), so a band
		// added above the mark is counted by this row rather than failing it.
		for i, dropped := range want {
			if narrowings[i].Dropped != dropped {
				t.Errorf("line %d of a caller at %d says it dropped %q, want %q",
					i, recordFieldsContract-1, narrowings[i].Dropped, dropped)
			}
		}
	})

	// The body of a caller at the contract: the two numbers stand beside the record whose keys
	// the same contract added, so a reader that has the record can say which contract it reads.
	// Such a caller is still below every band above it, the tree quiet time among them, so its
	// issues carry no measure and its request writes one line for each of those bands.
	t.Run("a caller at the contract carries the two numbers beside the record", func(t *testing.T) {
		state := populatedState(t)
		body, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(recordFieldsContract))

		mark, carried := contractServed(t, body)
		if !carried {
			t.Fatalf("the body served to a pod at %d carries no `contract` mark: %s", recordFieldsContract, body)
		}
		want := map[string]any{"daemon": float64(DaemonAPIVersion), "caller": float64(recordFieldsContract)}
		if len(mark) != len(want) || mark["daemon"] != want["daemon"] || mark["caller"] != want["caller"] {
			t.Errorf("the mark is %v, want %v: the contract the daemon declares and the one the caller's pod records", mark, want)
		}
		for _, key := range recordFieldKeys {
			if !bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod at %d carries no %q; the mark is meant to stand beside the record: %s",
					recordFieldsContract, key, body)
			}
		}
		for _, key := range []string{TreeMovedAtField, QuietHoursField} {
			if bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod at %d carries %q, which a band above it holds back: %s",
					recordFieldsContract, key, body)
			}
		}
		// A caller at this contract stands below the contracts above it and is narrowed by those
		// alone: the log names every one of them and never the mark's own arm, whose line would
		// mean the record had been taken out of the body just read.
		wantLines := narrowingsExpected(state, recordFieldsContract)
		if len(narrowings) != len(wantLines) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, one for each contract above it: %+v",
				len(narrowings), recordFieldsContract, len(wantLines), narrowings)
		}
		for i, line := range narrowings {
			if line.Dropped != wantLines[i] {
				t.Errorf("line %d of a caller at %d says it dropped %q, want %q", i, recordFieldsContract, line.Dropped, wantLines[i])
			}
		}
	})

	// The operator's own answer, `legion state --json`: the daemon places that caller in no pod,
	// so it is served the state whole and narrowed by nothing, and the mark names the contract it
	// was built at twice: as the daemon's, and as the contract the caller is served (ContractView
	// says why a caller at the daemon's own contract is told it too).
	t.Run("the operator's own answer says which contract it was built at", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, populatedState(t), "192.0.2.1:41000", podClaims(recordFieldsContract-1))

		mark, carried := contractServed(t, body)
		if !carried {
			t.Fatalf("the operator's own answer carries no `contract` mark: %s", body)
		}
		if mark["daemon"] != float64(DaemonAPIVersion) {
			t.Errorf("the operator's answer was built at %v, want the contract the daemon declares, %d", mark["daemon"], DaemonAPIVersion)
		}
		if mark["caller"] != float64(DaemonAPIVersion) || len(mark) != 2 {
			t.Errorf("the operator's answer carries the mark %v, want the daemon's number as both the daemon's and the caller's: nothing narrows that reader", mark)
		}
		if len(narrowings) != 0 {
			t.Errorf("the daemon narrowed the operator's own answer: %+v", narrowings)
		}
	})

	// The other value strip, read on the fixture that carries what it takes: a pod at 64 is
	// served the wait of a root a person declined without its answered pair, and one line says
	// so, naming the pod's lowest contract as the number that strip followed. The report of
	// claims nothing will wake is empty in this state, so the strip above writes nothing and
	// this is the one value line of the request.
	t.Run("a value the caller does not admit is recorded as dropped", func(t *testing.T) {
		const behind = 64
		body, narrowings := stateNarrowed(t, stateCarryingTheRestOfTheShape(t), statePodAddr+":41000", podClaims(behind))

		if bytes.Contains(body, []byte(`"answeredBy"`)) {
			t.Errorf("the body served to a pod at %d carries `answeredBy`, the very value that caller does not admit: %s", behind, body)
		}
		var written []narrowingLine
		for _, line := range narrowings {
			if line.Dropped == droppedWaitAnswer {
				written = append(written, line)
			}
			if line.Dropped == droppedRetiredStranded {
				t.Errorf("the daemon says it dropped %q from a state whose report of claims nothing will wake is empty", line.Dropped)
			}
		}
		if len(written) != 1 {
			t.Fatalf("the daemon wrote %d lines for the wait answer it took, want one: %+v", len(written), narrowings)
		}
		if written[0].PluginContract != behind || written[0].Follows != followsTheLowest {
			t.Errorf("the line names contract %d and follows %q, want the pod's lowest %d and %q",
				written[0].PluginContract, written[0].Follows, behind, followsTheLowest)
		}
	})

	// Every narrowing says what it dropped, not only the mark's own: a caller below every row of
	// stateNarrowings is served one line per row, each naming its own keys, and one more for the
	// value strip that took the retired entries out of its body. One line for all of them, or a
	// line that named the contract and nothing else, would leave a reader of the log to find out
	// elsewhere what left the body.
	//
	// The value strip's line names the lowest contract the pod records, which is the floor for
	// this caller, where a band's line names the highest; the clause under `follows` is what
	// tells an operator the two numbers of one request apart.
	t.Run("each narrowing names its own keys", func(t *testing.T) {
		state := populatedState(t)
		_, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(supervise.PodUpdateContract-1))

		want := narrowingsExpected(state, supervise.PodUpdateContract-1)
		if len(want) != len(stateNarrowings)+1 {
			t.Fatalf("a caller below every row expects %d lines, want the %d rows and the retired entries' own: %v",
				len(want), len(stateNarrowings), want)
		}
		if len(narrowings) != len(want) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller below every contract, want %d: %+v",
				len(narrowings), len(want), narrowings)
		}
		dropped := map[string]bool{}
		for i, line := range narrowings {
			if line.Dropped == "" {
				t.Errorf("line %d says nothing of what it dropped", i)
			}
			if dropped[line.Dropped] {
				t.Errorf("two narrowings report the same keys, %q: each arm names its own", line.Dropped)
			}
			dropped[line.Dropped] = true
			if line.Dropped != want[i] {
				t.Errorf("the lines are out of the order recordShaping writes them: line %d says %q, want %q",
					i, line.Dropped, want[i])
			}
		}
		if band := narrowings[0]; band.Follows != "" {
			t.Errorf("a band's line says it follows %q, want no such clause: a band follows the pod's highest contract and says so by saying nothing",
				band.Follows)
		}
		value := narrowings[len(narrowings)-1]
		if value.PluginContract != ServingFloor || value.Follows != followsTheLowest {
			t.Errorf("the value strip's line names contract %d and follows %q, want the pod's lowest %d and %q",
				value.PluginContract, value.Follows, ServingFloor, followsTheLowest)
		}
	})
}

// recordFieldsContract is never above this daemon's declared contract. A caller is served the
// nine keys only where its pod records that number or more (podContracts), so a constant above
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
