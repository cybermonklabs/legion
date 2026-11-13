package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
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

// narrowingState is the state golden with one more member of the waiting line, LEGION-213, a
// started issue that gave its slot back to wait on another issue (waitingOnIssueContract): the
// golden itself stands no member of that kind, so without it the row of that contract would
// find nothing to narrow and write no line, and the rows below could not say that every row of
// stateNarrowings writes its own.
func narrowingState(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	issues := make(map[string]Issue, len(state.Issues)+1)
	maps.Copy(issues, state.Issues)
	issues["LEGION-213"] = waitingOnIssueMember()
	state.Issues = issues
	return state
}

// narrowingsExpected is what the daemon's log holds for a caller at contract, in the order
// stateFor applies them: the `dropped` of every row of stateNarrowings whose contract is above
// the caller's, since narrowingState carries something of every row. It does not run the rows,
// so it checks stateFor rather than repeating it. One row writes no line where its contract
// alone says it should: below whyAndLineFieldsContract, waitingOnIssueContract's row finds no
// member's `kind` left, because the earlier row already took it, and so drops nothing.
func narrowingsExpected(contract int) []string {
	var dropped []string
	for _, narrowing := range stateNarrowings {
		if contract >= narrowing.contract {
			continue
		}
		if narrowing.contract == waitingOnIssueContract && contract < whyAndLineFieldsContract {
			continue
		}
		dropped = append(dropped, narrowing.dropped)
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
		body, narrowings := stateNarrowed(t, narrowingState(t), remote, podClaims(recordFieldsContract-1))

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

		want := narrowingsExpected(recordFieldsContract - 1)
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
	})

	// The body of a caller at the contract: the two numbers stand beside the record whose keys
	// the same contract added, so a reader that has the record can say which contract it reads.
	t.Run("a caller at the contract carries the two numbers beside the record", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, narrowingState(t), statePodAddr+":41000", podClaims(recordFieldsContract))

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
		// A caller at this contract stands below the contracts above it and is narrowed by those
		// alone: the log names every one of them and never the mark's own arm, whose line would
		// mean the record had been taken out of the body just read.
		wantLines := narrowingsExpected(recordFieldsContract)
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
	// was built at and no caller's.
	t.Run("the operator's own answer says which contract it was built at", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, narrowingState(t), "192.0.2.1:41000", podClaims(recordFieldsContract-1))

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
	// contract in stateNarrowings is served one line per row that finds something of its own,
	// each naming its own keys, and a caller at phaseEntriesContract, the row just before
	// waitingOnIssueContract's, the line of the one row the first caller's body no longer
	// reaches. Between them every row writes its line, and none writes it twice. One line for all
	// of them, or a line that named the contract and nothing else, would leave a reader of the
	// log to find out elsewhere what left the body.
	t.Run("each narrowing names its own keys", func(t *testing.T) {
		dropped := map[string]bool{}
		for _, contract := range []int{supervise.PodUpdateContract - 1, phaseEntriesContract} {
			_, narrowings := stateNarrowed(t, narrowingState(t), statePodAddr+":41000", podClaims(contract))
			want := narrowingsExpected(contract)
			if len(narrowings) != len(want) {
				t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d: %+v",
					len(narrowings), contract, len(want), narrowings)
			}
			for i, line := range narrowings {
				if line.Dropped == "" {
					t.Errorf("line %d of a caller at %d says nothing of what it dropped", i, contract)
				}
				if dropped[line.Dropped] {
					t.Errorf("two narrowings report the same keys, %q: each arm names its own", line.Dropped)
				}
				dropped[line.Dropped] = true
				if line.Dropped != want[i] {
					t.Errorf("the lines are out of the table's order: line %d of a caller at %d says %q, want %q",
						i, contract, line.Dropped, want[i])
				}
			}
		}
		for _, narrowing := range stateNarrowings {
			if !dropped[narrowing.dropped] {
				t.Errorf("no caller was told the row of contract %d dropped %q", narrowing.contract, narrowing.dropped)
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
