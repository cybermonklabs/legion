package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A chain the copy of Dispatch's issues cannot show to its top is unknown (R16 of
// dispatch://INFRA-3320), and nothing is decided on it until the daemon has read Dispatch again:
// a missing row, a loop that two moves arriving out of order make, and a chain of 33 links, one
// past Dispatch's bound. Over t1's harness (the mirror, the workflow and admission, and the API's
// routes), an admission pass starts nothing on such a chain, though the issue holds its slot and
// its design gate stands approved, and makes no root of a labelled todo issue on one: an issue not
// at work on it is listed held with the reason chain_unknown, and one at work goes on and stays in
// the list it stands in. Each architect's call on an issue of such a chain - sign_off,
// close_root, register_gate, release_children and rerun_child - is refused CHAIN_UNKNOWN.
func TestAnUnknownChainStartsNothingAndRefusesCallsAndSignOff(t *testing.T) {
	t.Parallel()
	const (
		missingTodo, missingAdmitted = "CHAIN-10", "CHAIN-12"
		loopA, loopB                 = "CHAIN-20", "CHAIN-21"
		atWork                       = "CHAIN-30"
		deepTodo, deepAtWork         = "CHAIN-99", "CHAIN-100"
	)
	labelledTodo := func(key, parent string) chainRow {
		return chainRow{key: key, parent: parent, status: "todo", labelled: true}
	}
	inWork := func(key, parent string, at phase.Phase, status string) chainRow {
		return chainRow{key: key, parent: parent, status: status, labelled: true, phase: at, tree: key, slot: true, gate: true,
			lifecycle: true, architect: holdsItsRole}
	}
	rows := []chainRow{
		// A row the copy lacks: CHAIN-11 has no row, and Dispatch serves none either. Under it, a
		// labelled todo issue the daemon has not recorded, and a root admitted at todo with its
		// slot and its gate approved.
		labelledTodo(missingTodo, "CHAIN-11"),
		inWork(missingAdmitted, "CHAIN-11", phase.Admitted, "todo"),
		// A loop two late moves made: each names the other as its parent.
		labelledTodo(loopA, loopB),
		labelledTodo(loopB, loopA),
		// An issue at work under a parent the copy lacks.
		inWork(atWork, "CHAIN-31", phase.Implementing, "in_progress"),
		// A chain of 33 links: CHAIN-100 at work under 101, and so on up to 133, which has no
		// parent, and under it a labelled todo issue, 34 links from the top, open under an issue
		// at work: the children-first wait is no decision taken on an unknown chain, so CHAIN-100
		// is not moved into awaiting_children and goes on in the list it stands in.
		labelledTodo(deepTodo, deepAtWork),
		inWork(deepAtWork, "CHAIN-101", phase.Implementing, "in_progress"),
	}
	for i := 101; i <= 133; i++ {
		parent := ""
		if i < 133 {
			parent = fmt.Sprintf("CHAIN-%d", i+1)
		}
		rows = append(rows, chainRow{key: fmt.Sprintf("CHAIN-%d", i), parent: parent, status: "backlog"})
	}
	calls := map[string][]chainCall{
		missingAdmitted: {{opSignOff, missingAdmitted, "CHAIN_UNKNOWN"}, {opCloseRoot, missingAdmitted, "CHAIN_UNKNOWN"},
			{opRegister, missingAdmitted, "CHAIN_UNKNOWN"}},
		missingTodo: {{opRelease, missingAdmitted, "CHAIN_UNKNOWN"}},
		loopA:       {{opRelease, atWork, "CHAIN_UNKNOWN"}},
		atWork:      {{opSignOff, atWork, "CHAIN_UNKNOWN"}, {opRerun, atWork, "CHAIN_UNKNOWN"}},
		deepAtWork:  {{opSignOff, deepAtWork, "CHAIN_UNKNOWN"}, {opRegister, deepAtWork, "CHAIN_UNKNOWN"}},
		deepTodo:    {{opRelease, deepAtWork, "CHAIN_UNKNOWN"}},
	}
	for i := range rows {
		rows[i].calls = calls[rows[i].key]
	}

	d := newChainDaemonOver(t, "CHAIN", rows, false)
	answers := map[string]map[string]string{}
	answer := func(key, label, value string) {
		if answers[key] == nil {
			answers[key] = map[string]string{}
		}
		answers[key][label] = value
	}
	d.pass(answer)
	d.calls(answer)

	for _, key := range []string{missingTodo, loopA, loopB, deepTodo} {
		if place := answers[key][placeLabel]; !strings.HasPrefix(place, heldPlace(record.HeldChainUnknown)) {
			t.Errorf("%s, not at work on an unknown chain, stands %q after a pass, want it held %s", key, place, record.HeldChainUnknown)
		}
	}
	for _, key := range []string{missingAdmitted, atWork, deepAtWork} {
		if place := answers[key][placeLabel]; place != placeActive {
			t.Errorf("%s, at work on an unknown chain in its slot, stands %q after a pass, want it still %s", key, place, placeActive)
		}
	}
	for _, key := range []string{missingTodo, missingAdmitted, loopA, loopB, atWork, deepTodo, deepAtWork} {
		if starts := answers[key][startsLabel]; starts != startsAnswer(false) {
			t.Errorf("%s on an unknown chain: the pass answers %q, want it to start nothing", key, starts)
		}
	}
	for key, made := range calls {
		for _, call := range made {
			if got := answers[key][callLabel(call)]; got != call.want {
				t.Errorf("%s of %s: %s answers %q, want %s", call.op, key, callLabel(call), got, call.want)
			}
		}
	}
}
