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
	"github.com/sjawhar/legion/daemon/internal/claim"
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
	return stateNarrowedStating(t, state, remote, claims, 0)
}

// stateNarrowedStating is stateNarrowed asked stating contract in pluginContractHeader where it
// is positive and stating none where it is zero (stateStating).
func stateNarrowedStating(t *testing.T, state State, remote string, claims []supervise.Claim, contract int) ([]byte, []narrowingLine) {
	t.Helper()
	var logged bytes.Buffer
	options := stateOptions(state, claims)
	options.Log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := stateStating(t, NewServer("127.0.0.1", 8437, options).Handler, remote, contract)

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
// golden itself stands no member of that kind, so without it the strip of the waiting line's
// `kind` would find nothing to take and write no line, and the rows below could not say that
// every strip writes its own.
func narrowingState(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	issues := make(map[string]Issue, len(state.Issues)+1)
	maps.Copy(issues, state.Issues)
	issues["LEGION-213"] = waitingOnIssueMember()
	state.Issues = issues
	return state
}

// narrowingsExpected is what the daemon's log holds for a caller whose pod records contract, in
// the order recordShaping writes the lines: the `dropped` of every row of stateNarrowings the
// caller's strip stands below, and then the `dropped` of every row of valueStrips that took a
// value off state, which follow its restore. The bands are read from the table rather than
// written out here, so a narrowing added above the contract a row below reads is counted by that
// row rather than failing it.
//
// Whether a value strip takes something turns on the values state carries as well as on
// restore, so the strips are read from what the shaping itself reports it took off state
// (tookOff), the one place their conditions are written, in the order of their table. The
// fixture carries a retired stranded entry, no wait answered by a value younger than the floor,
// and the repository-cache-room row of the source `caches`, and narrowingState adds a member of
// the kind `waiting_on_issue`. So a caller below 56 is expected the stranded entries' line, a
// caller from whyAndLineFieldsContract up to below waitingOnIssueContract the line of the waiting
// line's `kind`, and no caller both: below whyAndLineFieldsContract the band of that contract
// takes `kind` first, and the strip finds nothing left to take. Every caller a pod places is
// below cachesSourceContract, so each is expected the caches source's line as well.
func narrowingsExpected(state State, contract int) []string {
	restore, strip := restoreContract(contract), stripContract(contract)
	var dropped []string
	for _, narrowing := range stateNarrowings {
		if strip < narrowing.contract {
			dropped = append(dropped, narrowing.dropped)
		}
	}
	var took tookOff
	stateFor(state, restore, strip, &took)
	for place, row := range valueStrips {
		if took.value(place) {
			dropped = append(dropped, row.dropped)
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
	// A caller one contract below the mark is below that narrowing and below every row of
	// stateNarrowings above it, so its request writes one line for each of them, the mark's own
	// first, since the table is applied lowest contract first and the line below names the keys
	// of that one arm.
	t.Run("a caller below the contract is told nothing, and the log says what was taken", func(t *testing.T) {
		const remote = statePodAddr + ":41000"
		state := narrowingState(t)
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
		state := narrowingState(t)
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
		body, narrowings := stateNarrowed(t, narrowingState(t), "192.0.2.1:41000", podClaims(recordFieldsContract-1))

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

	// The wait strip, read on the fixture that carries what it takes: a pod at 64 is served the
	// wait of a root a person declined without its answered pair, and one line says so, naming
	// the pod's lowest contract as the number that strip followed. The report of claims nothing
	// will wake is empty in this state and no issue of it stands in the waiting line, so no other
	// value strip writes anything and this is the one value line of the request.
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
			if line.Dropped == droppedStrandedState {
				t.Errorf("the daemon says it dropped %q from a state whose report of claims nothing will wake is empty", line.Dropped)
			}
		}
		if len(written) != 1 {
			t.Fatalf("the daemon wrote %d lines for the wait answer it took, want one: %+v", len(written), narrowings)
		}
		if written[0].PluginContract != behind || written[0].Follows != followsRestore {
			t.Errorf("the line names contract %d and follows %q, want the pod's lowest %d and %q",
				written[0].PluginContract, written[0].Follows, behind, followsRestore)
		}
	})

	// The assignments narrowing is a row of the table like every other, so a caller below it is
	// told what it lost: a caller one contract below assignmentsContract stands below that row and
	// every row above it, so the log holds those rows' lines and nothing else. This subtest does
	// not catch a strip applied beside the loop, because it reads both its expectation and its
	// answer from stateNarrowings; the subtest below, which names the line as a literal, is what
	// catches that.
	t.Run("the narrowings above a caller write their own lines", func(t *testing.T) {
		state := populatedState(t)
		body, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(assignmentsContract-1))

		if bytes.Contains(body, []byte(`"assignments"`)) {
			t.Errorf("the body served to a pod below %d carries `assignments`, the field that contract added: %s",
				assignmentsContract, body)
		}
		want := narrowingsExpected(state, assignmentsContract-1)
		if len(narrowings) != len(want) {
			t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d, one for each contract above it: %+v",
				len(narrowings), assignmentsContract-1, len(want), narrowings)
		}
		for i, line := range narrowings {
			if line.Dropped != want[i] {
				t.Errorf("line %d of a caller at %d says it dropped %q, want %q",
					i, assignmentsContract-1, line.Dropped, want[i])
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

	// Every narrowing says what it dropped, not only the mark's own: a caller below every row of
	// stateNarrowings is served one line per row, each naming its own keys, and two more for the
	// value strips that took the retired entries out of its body and the `caches` source off the
	// repository-cache-room row; and a caller at quietTimeContract is served the line of every row
	// above it, the line of the value strip that took LEGION-213's `kind`, the `waiting_on_issue`
	// the first caller's body no longer carries, since a band below it has taken `kind` already,
	// and the `caches` source's line again. One line for all of them, or a line
	// that named the contract and nothing else, would leave a reader of the log to find out
	// elsewhere what left the body. Three checks share the work. The table check guards the table:
	// no two rows of stateNarrowings name the same keys, whichever callers would see them, so a
	// line names one row. The per-caller check guards what one caller is told: no line twice. The
	// last two loops guard the sample: between the two callers every row writes its line, and so
	// does every value strip whose value narrowingState carries.
	//
	// A value strip's line names the lowest contract the pod records, which is restore for this
	// caller, where a band's line names the highest; the clause under `follows` is what tells an
	// operator the two numbers of one request apart.
	t.Run("each narrowing names its own keys", func(t *testing.T) {
		if want := narrowingsExpected(narrowingState(t), supervise.PodUpdateContract-1); len(want) != len(stateNarrowings)+2 {
			t.Fatalf("a caller below every row expects %d lines, want the %d rows, the retired entries' own and the caches source's: %v",
				len(want), len(stateNarrowings), want)
		}
		namedBy := map[string]int{}
		for _, narrowing := range stateNarrowings {
			if contract, named := namedBy[narrowing.dropped]; named {
				t.Errorf("the rows of contracts %d and %d both drop %q: each row names its own keys",
					contract, narrowing.contract, narrowing.dropped)
			}
			namedBy[narrowing.dropped] = narrowing.contract
		}
		told := map[string]bool{}
		for _, contract := range []int{supervise.PodUpdateContract - 1, quietTimeContract} {
			state := narrowingState(t)
			_, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(contract))
			want := narrowingsExpected(state, contract)
			if len(narrowings) != len(want) {
				t.Fatalf("the daemon wrote %d narrowing lines for a caller at %d, want %d: %+v",
					len(narrowings), contract, len(want), narrowings)
			}
			dropped := map[string]bool{}
			for i, line := range narrowings {
				if line.Dropped == "" {
					t.Errorf("line %d of a caller at %d says nothing of what it dropped", i, contract)
				}
				if dropped[line.Dropped] {
					t.Errorf("two narrowings of a caller at %d report the same keys, %q: each arm names its own", contract, line.Dropped)
				}
				dropped[line.Dropped] = true
				told[line.Dropped] = true
				if line.Dropped != want[i] {
					t.Errorf("the lines are out of the order recordShaping writes them: line %d of a caller at %d says %q, want %q",
						i, contract, line.Dropped, want[i])
				}
			}
			if band := narrowings[0]; band.Follows != "" {
				t.Errorf("a band's line says it follows %q, want no such clause: a band follows the pod's highest contract and says so by saying nothing",
					band.Follows)
			}
			value := narrowings[len(narrowings)-1]
			if restore := restoreContract(contract); value.PluginContract != restore || value.Follows != followsRestore {
				t.Errorf("the value strip's line of a caller at %d names contract %d and follows %q, want the pod's lowest %d and %q",
					contract, value.PluginContract, value.Follows, restore, followsRestore)
			}
		}
		for _, narrowing := range stateNarrowings {
			if !told[narrowing.dropped] {
				t.Errorf("no caller was told the row of contract %d dropped %q", narrowing.contract, narrowing.dropped)
			}
		}
		for _, value := range []string{droppedStrandedState, droppedLineKind, droppedCapabilitySource} {
			if !told[value] {
				t.Errorf("no caller was told the value strip dropped %q", value)
			}
		}
	})
}

// A band logs a drop only of a key the body carried on the wire. narrowingsExpected reads a
// fully populated state, where every band has a key to take, so this case reads the other end:
// a state with one issue that carries no optional field, served to a caller below every band.
// The issue holds an empty `gatedSteps`, which a root of a deployment that gates no step carries
// (GatedStepViewsOf), and an empty `ancestry`. `omitempty` keeps each empty list off the wire as
// it keeps a nil list. The issue has no worker and no check, so no `pass` and no `role` is on the
// wire either, and no band of an issue may log a drop.
//
// What each band takes from the wire is measured here and not listed: the band's halves are
// applied to the body the bands start from, and the two bodies are marshalled and compared. A
// band that changes no byte has taken no key, and the log must say nothing of it. Today the
// bands of the body's own keys do change the bytes, because every state carries
// `admission.trees`, `wakesWaiting`, `claimsToRefresh`, the mark, `admission.held` and
// `lowestPluginContract` for these claims, all but withoutRepositoryApps: this state serves no
// repository, so that band has no `apps` to take and logs nothing.
func TestAnEmptyStateLogsNoDropOfAKeyItNeverCarried(t *testing.T) {
	const below = supervise.PodUpdateContract - 1
	claims := podClaims(below)
	state := State{Issues: map[string]Issue{"LEGION-300": {
		Key:        "LEGION-300",
		Workers:    map[claim.Role]PhaseView{},
		GatedSteps: []GatedStepView{},
		Ancestry:   []string{},
	}}}
	_, narrowings := stateNarrowed(t, state, statePodAddr+":41000", claims)

	// The body the bands start from: the state as the route answers it, with the fleet's lowest
	// contract it writes (projectState) and the mark stateFor writes before any band runs.
	served := state
	served.LowestPluginContract = lowestPluginContract(claims)
	served.Contract = &ContractView{Daemon: DaemonAPIVersion, Caller: stripContract(below)}
	marshalled := func(body State) []byte {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal the state: %v", err)
		}
		return encoded
	}
	before := marshalled(served)
	var want []string
	for _, narrowing := range stateNarrowings {
		narrowed := served
		if narrowing.state != nil {
			narrowed, _ = narrowing.state(narrowed)
		}
		if narrowing.issue != nil {
			issues := make(map[string]Issue, len(narrowed.Issues))
			for key, issue := range narrowed.Issues {
				issues[key], _ = narrowing.issue(issue)
			}
			narrowed.Issues = issues
		}
		if !bytes.Equal(before, marshalled(narrowed)) {
			want = append(want, narrowing.dropped)
		}
	}
	got := make([]string, 0, len(narrowings))
	for _, line := range narrowings {
		got = append(got, line.Dropped)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the daemon logged the drops %q for the empty state, want only the bands that changed the wire, %q", got, want)
	}
	for _, narrowing := range stateNarrowings {
		if narrowing.state == nil && slices.Contains(got, narrowing.dropped) {
			t.Errorf("the daemon logged %q for an issue that carries none of those keys", narrowing.dropped)
		}
	}
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

// capabilityRowServed is the capability row named name of a state body (capabilityRowsServed),
// and whether the body carries such a row at all.
func capabilityRowServed(t *testing.T, body []byte, name capabilities.Name) (map[string]any, bool) {
	t.Helper()
	for _, row := range capabilityRowsServed(t, body) {
		if row["name"] == string(name) {
			return row, true
		}
	}
	return nil, false
}

// cachesSource is the word capabilities.SourceCaches writes on the wire, the one value of a
// capability row's `source` that cachesSourceContract added (answers.go's capabilitySources).
const cachesSource = string(capabilities.SourceCaches)

// A caller whose reader is below cachesSourceContract reads a capability row's `source` with the
// two words contract 25 gave it and refuses the whole state body for a third, so it is served the
// repository-cache-room row with every other field of it — its status, its detail naming the
// full cache, its config line — and without its `source`, and the daemon's log names what it
// took; every row of another source keeps its own. The word is a value and no key, so the strip
// follows the contract the caller states, else its pod's lowest (fieldArrivals.heldBack): a
// plugin of shapedAnswersContract or later states its contract, and an older one is placed by its
// pod. A caller at the contract is served `source: "caches"`, and so is every caller nothing
// narrows: a pod whose claims all state their own contract, one the daemon places in no pod, the
// operator's `legion state`, and every pane of a daemon that places no caller in a pod, which is
// the tmux runtime. The row is never taken out, and the state the source answered keeps the word
// for the next caller.
func TestACallerBelowTheCachesSourceContractGetsNoCachesSource(t *testing.T) {
	// Named as a literal, as the assignments line is in TestANarrowedBodySaysSo: a strip applied
	// beside the table's loop would write no line and leave every row read from the table green.
	const dropped = "the source of every capability row of a source this caller does not admit"
	state := populatedState(t)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := capabilityRowServed(t, encoded, capabilities.RepositoryCacheRoom)
	if full["source"] != cachesSource || full["status"] != capabilities.StatusOpen || full["configLine"] == nil {
		t.Fatalf("the fixture's %s row is %v, want an open row from the caches; this test proves nothing", capabilities.RepositoryCacheRoom, full)
	}
	withoutSource := maps.Clone(full)
	delete(withoutSource, "source")

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		// stated is the contract the caller states, none where it is zero, and restore the
		// contract the strip follows for it, which its line names.
		stated, restore int
	}{
		{name: "a caller that states the contract below", remote: "192.0.2.1:41000", claims: podClaims(pauseContract),
			stated: cachesSourceContract - 1, restore: recordedContractAtOrBelow(cachesSourceContract - 1)},
		{name: "a pod whose claims record a contract below", remote: statePodAddr + ":41000", claims: podClaims(pauseContract),
			restore: pauseContract},
	} {
		t.Run(tc.name+" reads the row without its source", func(t *testing.T) {
			body, narrowings := stateNarrowedStating(t, state, tc.remote, tc.claims, tc.stated)
			row, carried := capabilityRowServed(t, body, capabilities.RepositoryCacheRoom)
			if !carried || !reflect.DeepEqual(row, withoutSource) {
				t.Errorf("%s is served the %s row %v (carried %v), want every field but source, %v",
					tc.name, capabilities.RepositoryCacheRoom, row, carried, withoutSource)
			}
			for name, source := range map[capabilities.Name]capabilities.Source{
				capabilities.Secrets:      capabilities.SourceConfiguration,
				capabilities.PoolCapacity: capabilities.SourceBoot,
			} {
				if row, _ := capabilityRowServed(t, body, name); row["source"] != string(source) {
					t.Errorf("%s is served the %s row %v, want its source %q kept: that word arrived with the field",
						tc.name, name, row, source)
				}
			}
			var written []narrowingLine
			for _, line := range narrowings {
				if line.Dropped == dropped {
					written = append(written, line)
				}
			}
			if len(written) != 1 {
				t.Fatalf("%s was served %d lines saying it dropped %q, want one; the daemon wrote %+v",
					tc.name, len(written), dropped, narrowings)
			}
			if written[0].PluginContract != tc.restore || written[0].Follows != followsRestore {
				t.Errorf("the line names contract %d and follows %q, want %d and %q",
					written[0].PluginContract, written[0].Follows, tc.restore, followsRestore)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		// tmux is a daemon that places no caller in a pod, as the tmux runtime's is.
		tmux bool
	}{
		{name: "a caller that states the contract", remote: "192.0.2.1:41000", claims: podClaims(pauseContract), stated: cachesSourceContract},
		{name: "a pod whose claims state their own contract", remote: statePodAddr + ":41000", claims: podClaims(cachesSourceContract)},
		{name: "the operator's own answer", remote: "192.0.2.1:41000", claims: podClaims(pauseContract)},
		{name: "a tmux pane", remote: statePodAddr + ":41000", claims: podClaims(pauseContract), tmux: true},
	} {
		t.Run(tc.name+" reads source caches", func(t *testing.T) {
			body := stateStating(t, shapedServer(t, state, tc.claims, &bytes.Buffer{}, !tc.tmux), tc.remote, tc.stated)
			if row, carried := capabilityRowServed(t, body, capabilities.RepositoryCacheRoom); !carried || !reflect.DeepEqual(row, full) {
				t.Errorf("%s is served the %s row %v (carried %v), want it whole, %v", tc.name, capabilities.RepositoryCacheRoom, row, carried, full)
			}
		})
	}

	if !slices.ContainsFunc(state.Capabilities, func(row CapabilityState) bool { return row.Source == cachesSource }) {
		t.Errorf("the strip wrote through to the state the source answered: its capability rows are %+v", state.Capabilities)
	}
}
