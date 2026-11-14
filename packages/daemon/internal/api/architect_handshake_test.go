package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// entryQueuedField and entryConfirmedField are the names the two times carry on the wire. The
// tests read the keys by these names rather than repeating the strings, as the sibling contract's
// tests read `phaseEntry` by phaseEntryField.
const (
	entryQueuedField    = "entryQueuedAt"
	entryConfirmedField = "entryConfirmedAt"
)

// architectHandshakeState is a state of three roots standing at admitted, the phase a root's own
// architect works, which the handshake answers differently:
//
//   - LEGION-20's architect claim recorded the handshake of the entry its row names and a turn of
//     its worker confirmed a task of that entry, so the root carries both numbers and both times;
//   - LEGION-30's architect claim recorded that entry queued and no turn has confirmed it, so the
//     root carries the queued time alone, which is the root whose architect may have taken nothing;
//   - LEGION-40's claim recorded no handshake, so the root carries the two numbers and neither
//     time, which is what a root of the release before this one reads as: its migration gives the
//     claim the two numbers and no handshake.
func architectHandshakeState(queued, confirmed time.Time) State {
	architect := func(session string) *ClaimView {
		return &ClaimView{Session: session, State: "working"}
	}
	root := func(key string) Issue {
		return Issue{Key: key, Phase: phase.Admitted, Status: "in_progress", Tree: key,
			PhaseEntry: 1, GivenEntry: 1, Architect: architect("ses_architect_" + key),
			Workers: map[claim.Role]PhaseView{}}
	}
	both, queuedOnly, neither := root("LEGION-20"), root("LEGION-30"), root("LEGION-40")
	both.EntryQueuedAt, both.EntryConfirmedAt = &queued, &confirmed
	queuedOnly.EntryQueuedAt = &queued
	return State{Issues: map[string]Issue{
		"LEGION-20": both, "LEGION-30": queuedOnly, "LEGION-40": neither,
	}}
}

// entryTimesServed is, for every issue of a state body, whether it carries each of the two times
// and what each is. Presence is kept apart from value because what the route is held to here is
// whether the key is on the wire at all: a root whose claim was queued no task of its entry
// carries neither key.
func entryTimesServed(t *testing.T, body []byte) map[string]struct {
	queued, confirmed *time.Time
} {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := map[string]struct{ queued, confirmed *time.Time }{}
	for key, issue := range state.Issues {
		at := func(field string) *time.Time {
			raw, carried := issue[field]
			if !carried {
				return nil
			}
			var value time.Time
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("decode the %s of %s, %s: %v", field, key, raw, err)
			}
			return &value
		}
		served[key] = struct{ queued, confirmed *time.Time }{at(entryQueuedField), at(entryConfirmedField)}
	}
	return served
}

// The state route serves a root the handshake between the daemon and its architect: the entry its
// architect's work stands at, the entry its claim was queued, the time a task of that entry was
// first queued and the time a turn of its worker first confirmed one. A root whose claim recorded
// no handshake is served the two numbers and neither time, which is what a root of the release
// before this one reads as.
func TestTheStateRouteServesARootsArchitectHandshake(t *testing.T) {
	queued := time.Date(2026, 11, 12, 9, 15, 0, 0, time.UTC)
	confirmed := queued.Add(41 * time.Second)
	body := stateFrom(t, architectHandshakeState(queued, confirmed), "10.1.24.9:4100", podClaims(DaemonAPIVersion))

	entries := phaseEntriesServed(t, body)
	times := entryTimesServed(t, body)
	for _, key := range []string{"LEGION-20", "LEGION-30", "LEGION-40"} {
		entry, shown := entries[key]
		if !shown {
			t.Fatalf("%s is in no issue of %s; this test proves nothing", key, body)
		}
		if !entry.carried || entry.entry != 1 || entry.given != 1 {
			t.Errorf("the root %s is served %+v, want entry 1 given 1: the architect is the role that works admitted. %s",
				key, entry, body)
		}
	}

	held := times["LEGION-20"]
	if held.queued == nil || !held.queued.Equal(queued) || held.confirmed == nil || !held.confirmed.Equal(confirmed) {
		t.Errorf("the root whose architect took its task is served queued %v confirmed %v, want %s and %s: %s",
			held.queued, held.confirmed, queued, confirmed, body)
	}
	unconfirmed := times["LEGION-30"]
	if unconfirmed.queued == nil || !unconfirmed.queued.Equal(queued) || unconfirmed.confirmed != nil {
		t.Errorf("the root whose architect confirmed nothing is served queued %v confirmed %v, want %s and no key: %s",
			unconfirmed.queued, unconfirmed.confirmed, queued, body)
	}
	before := times["LEGION-40"]
	if before.queued != nil || before.confirmed != nil {
		t.Errorf("the root whose claim was queued no task of its entry is served queued %v confirmed %v, want neither key: %s",
			before.queued, before.confirmed, body)
	}
}

// A plugin below entryTimesContract is served no issue carrying either time: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the keys, so the strip is what keeps the times
// from a caller that did not ask for them. The route authenticates nobody, so it knows that
// plugin by the pod the request came from. The two entry numbers are contract 94's and stay: a
// pod between the two contracts is served them and no time.
func TestTheArchitectHandshakeTimesDowngradeBelowTheirContract(t *testing.T) {
	queued := time.Date(2026, 11, 12, 9, 15, 0, 0, time.UTC)
	confirmed := queued.Add(41 * time.Second)
	state := architectHandshakeState(queued, confirmed)

	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(entryTimesContract-1))
	for key, issue := range entryTimesServed(t, behind) {
		if issue.queued != nil || issue.confirmed != nil {
			t.Errorf("the pod below %d is served %+v for %s, want neither key: the times are not its contract's",
				entryTimesContract, issue, key)
		}
	}
	for key, issue := range phaseEntriesServed(t, behind) {
		if !issue.carried || issue.entry != 1 || issue.given != 1 {
			t.Errorf("the pod below %d is served %+v for %s, want entry 1 given 1: the two numbers are contract %d's and stay",
				entryTimesContract, issue, key, phaseEntriesContract)
		}
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(entryTimesContract))
	served := entryTimesServed(t, at)["LEGION-20"]
	if served.queued == nil || !served.queued.Equal(queued) || served.confirmed == nil || !served.confirmed.Equal(confirmed) {
		t.Errorf("the pod at %d is served %+v for LEGION-20, want queued %s confirmed %s: %s",
			entryTimesContract, served, queued, confirmed, at)
	}
}

// A root's `architectStalls` arrived with the two times, at contract 117, and leaves with them: a
// caller at that contract is served the arms the daemon names for the root, and one below it is
// served none. The state here carries no time at all, so the arms alone are what the strip must
// see as something of that contract to take away (carriesEntryTimes); a strip that looked for
// the times alone would serve the key to the strict reader that refuses it.
func TestTheArchitectStallArmsDowngradeWithTheEntryTimes(t *testing.T) {
	state := State{Issues: map[string]Issue{
		"LEGION-50": {Key: "LEGION-50", Phase: phase.Admitted, Status: "in_progress", Tree: "LEGION-50",
			Workers: map[claim.Role]PhaseView{}, ArchitectStalls: []record.ArchitectStallArm{record.ArchitectClaimUnreachable}},
	}}
	arms := func(body []byte) (json.RawMessage, bool) {
		t.Helper()
		var served struct {
			Issues map[string]map[string]json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal(body, &served); err != nil {
			t.Fatalf("decode state %s: %v", body, err)
		}
		raw, carried := served.Issues["LEGION-50"]["architectStalls"]
		return raw, carried
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(entryTimesContract))
	if raw, carried := arms(at); !carried || string(raw) != `["claim-unreachable"]` {
		t.Errorf("the pod at %d is served the arms %s (carried %t) for LEGION-50, want [\"claim-unreachable\"]: %s",
			entryTimesContract, raw, carried, at)
	}
	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(entryTimesContract-1))
	if raw, carried := arms(behind); carried {
		t.Errorf("the pod below %d is served the arms %s for LEGION-50, want no key: the arms are not its contract's: %s",
			entryTimesContract, raw, behind)
	}
}

// entryTimesContract is the contract whose entry in version.go added the two times, and this
// daemon's own contract is never below it: a caller at this contract is served them.
func TestTheArchitectHandshakeTimesContractIsTheOneThatAddedThem(t *testing.T) {
	const addedEntryTimes = 117
	if entryTimesContract != addedEntryTimes {
		t.Fatalf("entryTimesContract = %d, want %d, the contract whose entry in version.go added the issues' entry queued and confirmed times",
			entryTimesContract, addedEntryTimes)
	}
	if entryTimesContract > DaemonAPIVersion {
		t.Fatalf("entryTimesContract = %d, above DaemonAPIVersion %d: no caller could be served the two times",
			entryTimesContract, DaemonAPIVersion)
	}
}
