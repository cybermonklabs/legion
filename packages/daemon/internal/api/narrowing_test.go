package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
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
		for _, key := range []string{TreeMovedAtField, QuietHoursField} {
			if bytes.Contains(body, []byte(`"`+key+`"`)) {
				t.Errorf("the body served to a pod at %d carries %q, which a band above it holds back: %s",
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

	// The newest narrowing is a row of the table like every other, so the caller below it is
	// told what it lost: a caller one contract below the highest row stands below that row
	// alone, so the log holds that row's line and nothing else. This subtest does not catch a
	// strip applied beside the loop, because it reads both its expectation and its answer from
	// stateNarrowings; the subtest below, which names the line as a literal, is what catches
	// that.
	t.Run("the highest narrowing writes its own line", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, populatedState(t), statePodAddr+":41000", podClaims(cachesSourceContract-1))

		if bytes.Contains(body, []byte(`"source":"`+cachesSource+`"`)) {
			t.Errorf("the body served to a pod below %d carries a capability row whose source is %q, the word that contract added: %s",
				cachesSourceContract, cachesSource, body)
		}
		want := narrowingsExpected(cachesSourceContract - 1)
		if len(narrowings) != len(want) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, one for each contract above it: %+v",
				len(narrowings), cachesSourceContract-1, len(want), narrowings)
		}
		for i, line := range narrowings {
			if line.Dropped != want[i] {
				t.Errorf("line %d of a caller at %d says it dropped %q, want %q",
					i, cachesSourceContract-1, line.Dropped, want[i])
			}
		}
	})

	// The assignments line is named here as a literal rather than read from stateNarrowings,
	// because every other row of this file reads its expectation and its answer from that same
	// table: take the row out, apply the strip beside the loop as an `if` of its own, and the
	// expectation falls with the answer, so the body is still narrowed, no line is written and
	// nothing fails. That is exactly the state R12 of dispatch://INFRA-3083 forbids. Measured
	// on 83073e209f96, the commit that adds this subtest, with the row removed and that `if`
	// restored: every other test of internal/api passed, the subtest above included, and this
	// one alone failed.
	t.Run("the assignments narrowing names its own keys in the log", func(t *testing.T) {
		const dropped = "the issues' assignments"
		_, narrowings := stateNarrowed(t, populatedState(t), statePodAddr+":41000", podClaims(assignmentsContract-1))

		for _, line := range narrowings {
			if line.Dropped == dropped {
				return
			}
		}
		t.Fatalf("a caller at %d was served no line saying it dropped %q; the daemon wrote %+v",
			assignmentsContract-1, dropped, narrowings)
	})

	// Every narrowing says what it dropped, not only the mark's own: a caller below every contract
	// in stateNarrowings is served one line per row that finds something of its own, and a caller
	// at quietTimeContract, the row just before waitingOnIssueContract's, the line of every row
	// above it, the one row the first caller's body no longer reaches among them. One line for all
	// of them, or a line that named the contract and nothing else, would leave a reader of the log
	// to find out elsewhere what left the body. Three checks share the work. The table check
	// guards the table: no two rows of stateNarrowings name the same keys, whichever callers would
	// see them, so a line names one row. The per-caller check guards what one caller is told: no
	// line twice. The last loop guards the sample: between the two callers every row writes its
	// line.
	t.Run("each narrowing names its own keys", func(t *testing.T) {
		namedBy := map[string]int{}
		for _, narrowing := range stateNarrowings {
			if contract, named := namedBy[narrowing.dropped]; named {
				t.Errorf("the rows of contracts %d and %d both drop %q: each row names its own keys",
					contract, narrowing.contract, narrowing.dropped)
			}
			namedBy[narrowing.dropped] = narrowing.contract
		}
		dropped := map[string]bool{}
		for _, contract := range []int{supervise.PodUpdateContract - 1, quietTimeContract} {
			told := map[string]bool{}
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
				if told[line.Dropped] {
					t.Errorf("a caller at %d is told %q twice", contract, line.Dropped)
				}
				told[line.Dropped] = true
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

// capabilityRowServed is the capability row named name of a state body, decoded as a plugin's
// reader sees it, and whether the body carries such a row at all.
func capabilityRowServed(t *testing.T, body []byte, name capabilities.Name) (map[string]any, bool) {
	t.Helper()
	var state struct {
		Capabilities []map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, row := range state.Capabilities {
		if row["name"] == string(name) {
			return row, true
		}
	}
	return nil, false
}

// A caller below cachesSourceContract reads a capability row's `source` with the two words
// contract 25 gave it and refuses the whole state body for a third, so it is served the
// repository-cache-room row with every other field of it — its status, its detail naming the
// full cache, its config line — and without its `source`, and the daemon's log names what it
// took; every row of another source keeps its own. A caller at the contract is served `source:
// "caches"`, and so is every caller nothing narrows: one the daemon places in no pod, the
// operator's `legion state`, and every pane of a daemon that places no caller in a pod, which is
// the tmux runtime. The row is never taken out, and the state the source answered keeps the word
// for the next caller.
func TestACallerBelowTheCachesSourceContractGetsNoCachesSource(t *testing.T) {
	const dropped = "the source of each capability read from the repository caches"
	state := populatedState(t)
	encoded, err := json.Marshal(state.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	for _, row := range rows {
		if row["name"] == string(capabilities.RepositoryCacheRoom) {
			full = row
		}
	}
	if full["source"] != cachesSource || full["status"] != capabilities.StatusOpen || full["configLine"] == nil {
		t.Fatalf("the fixture's %s row is %v, want an open row from the caches; this test proves nothing", capabilities.RepositoryCacheRoom, full)
	}
	withoutSource := maps.Clone(full)
	delete(withoutSource, "source")

	t.Run("a caller below the contract reads the row without its source", func(t *testing.T) {
		body, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(cachesSourceContract-1))
		row, carried := capabilityRowServed(t, body, capabilities.RepositoryCacheRoom)
		if !carried || !reflect.DeepEqual(row, withoutSource) {
			t.Errorf("a pod at %d is served the %s row %v (carried %v), want every field but source, %v",
				cachesSourceContract-1, capabilities.RepositoryCacheRoom, row, carried, withoutSource)
		}
		for name, source := range map[capabilities.Name]capabilities.Source{
			capabilities.Secrets:      capabilities.SourceConfiguration,
			capabilities.PoolCapacity: capabilities.SourceBoot,
		} {
			if row, _ := capabilityRowServed(t, body, name); row["source"] != string(source) {
				t.Errorf("a pod at %d is served the %s row %v, want its source %q kept: that word is not this contract's",
					cachesSourceContract-1, name, row, source)
			}
		}
		// Named as a literal, as the assignments line is in TestANarrowedBodySaysSo: a strip
		// applied beside the table's loop would write no line and leave every row read from the
		// table green.
		told := false
		for _, line := range narrowings {
			told = told || line.Dropped == dropped
		}
		if !told {
			t.Errorf("a caller at %d was served no line saying it dropped %q; the daemon wrote %+v",
				cachesSourceContract-1, dropped, narrowings)
		}
	})

	for _, tc := range []struct {
		name     string
		remote   string
		contract int
		// tmux is a daemon that places no caller in a pod, as the tmux runtime's is.
		tmux bool
	}{
		{name: "a caller at the contract", remote: statePodAddr + ":41000", contract: cachesSourceContract},
		{name: "the operator's own answer", remote: "192.0.2.1:41000", contract: cachesSourceContract - 1},
		{name: "a tmux pane", remote: statePodAddr + ":41000", tmux: true, contract: cachesSourceContract - 1},
	} {
		t.Run(tc.name+" reads source caches", func(t *testing.T) {
			options := stateOptions(state, podClaims(tc.contract))
			if tc.tmux {
				options.CallerPod = nil
			}
			body := stateAsked(t, NewServer("127.0.0.1", 8437, options).Handler, tc.remote)
			if row, carried := capabilityRowServed(t, body, capabilities.RepositoryCacheRoom); !carried || !reflect.DeepEqual(row, full) {
				t.Errorf("%s is served the %s row %v (carried %v), want it whole, %v", tc.name, capabilities.RepositoryCacheRoom, row, carried, full)
			}
		})
	}

	if got := state.Capabilities; !slices.ContainsFunc(got, func(row CapabilityState) bool { return row.Source == cachesSource }) {
		t.Errorf("the strip wrote through to the state the source answered: its capability rows are %+v", got)
	}
}

// cachesSourceContract is the contract whose entry in version.go added the word, written here
// as that literal rather than bounded
// (docs/solutions/legion/a-constant-that-must-equal-a-released-number-is-pinned-to-a-literal-never-bounded-pr2538.md):
// a forward merge that moves DaemonAPIVersion and leaves the constant behind would serve the word
// to a plugin of every number between the two, each built with the enum of two words, and a bound
// passes that as surely as the right value. Every other test of this narrowing spells the number
// relatively because this one pins it, so an edit of the literal is a contract change, not a test
// fix. The constant is never above the daemon's own contract, and it stands above
// entryTimesContract, the row before it in stateNarrowings.
func TestTheCachesSourceContractIsItsReleasedNumber(t *testing.T) {
	const addedCachesSource = 181
	if cachesSourceContract != addedCachesSource {
		t.Fatalf("cachesSourceContract = %d, want %d, the contract whose entry in version.go added the source `caches`",
			cachesSourceContract, addedCachesSource)
	}
	if cachesSourceContract > DaemonAPIVersion {
		t.Fatalf("cachesSourceContract = %d, above DaemonAPIVersion %d: no caller could be served the word",
			cachesSourceContract, DaemonAPIVersion)
	}
	if cachesSourceContract <= entryTimesContract {
		t.Fatalf("cachesSourceContract = %d, not above entryTimesContract %d, the row before it", cachesSourceContract, entryTimesContract)
	}
}
