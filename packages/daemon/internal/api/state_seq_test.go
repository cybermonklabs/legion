package api

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (populatedState) is the body of a daemon whose snapshot holds the
// project's state events up to 43 (DaemonInfo.StateSeq), counted in the store whose id it names
// (DaemonInfo.StateID), served by the daemon run it names (DaemonInfo.StateRun), so one state
// proves the three keys come off the wire below their contract and stay on it at that contract.

// stateSeqServed is the `daemon.stateSeq` of a state body, and whether the key is on the wire at
// all. The two are read apart because 0 is a sequence the route serves, a project with no state
// event yet, and an absent key is the body served to a caller below the contract: the one must
// never read as the other.
func stateSeqServed(t *testing.T, body []byte) (seq int64, carried bool) {
	t.Helper()
	raw, carried := daemonKeyServed(t, body, "stateSeq")
	if !carried {
		return 0, false
	}
	if err := json.Unmarshal(raw, &seq); err != nil {
		t.Fatalf("decode daemon.stateSeq %s: %v", raw, err)
	}
	return seq, true
}

// stateIDServed is the `daemon.stateId` of a state body, "" where the key is not on the wire.
func stateIDServed(t *testing.T, body []byte) string {
	t.Helper()
	return daemonStringServed(t, body, "stateId")
}

// stateRunServed is the `daemon.stateRun` of a state body, "" where the key is not on the wire.
func stateRunServed(t *testing.T, body []byte) string {
	t.Helper()
	return daemonStringServed(t, body, "stateRun")
}

// daemonStringServed is the string key of a state body's daemon block, "" where the key is not on
// the wire.
func daemonStringServed(t *testing.T, body []byte, key string) string {
	t.Helper()
	raw, carried := daemonKeyServed(t, body, key)
	if !carried {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode daemon.%s %s: %v", key, raw, err)
	}
	return value
}

// daemonKeyServed is the key of a state body's daemon block as it is on the wire, and whether it
// is there at all.
func daemonKeyServed(t *testing.T, body []byte, key string) (json.RawMessage, bool) {
	t.Helper()
	var state struct {
		Daemon map[string]json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	raw, carried := state.Daemon[key]
	return raw, carried
}

// A caller below stateSeqContract is served the daemon block with none of `stateSeq`, `stateId`
// and `stateRun`, since a plugin built before sjawhar/legion#2788 reads the state with a strict
// schema that refuses the whole body for a key it does not name. The route places a caller by the
// contract it states (`Legion-Plugin-Contract`), and a caller that states none by the pod the
// request came from; a pod's claims at shapedAnswersContract or above are in no pod's reckoning,
// since every such build states its contract (podContracts), so the pods below the contract here
// record one below shapedAnswersContract. A caller stating the contract, a pod whose claims record
// it or no contract, and a caller the daemon cannot place in a pod — the operator's `legion state`,
// and Dispatch, which reads the sequence, the store id and the run to line the body up with the
// state events — are served the three keys as the source answered them.
func TestTheStateSeqIsStrippedBelowItsContract(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(shapedAnswersContract-1))
	want, carried := stateSeqServed(t, full)
	wantID, wantRun := stateIDServed(t, full), stateRunServed(t, full)
	if !carried || want == 0 || wantID == "" || wantRun == "" {
		t.Fatalf("the fixture's state %s carries no stateSeq above 0, no stateId or no stateRun; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		// stated is the contract the caller states in the header, and 0 where it states none.
		stated  int
		carried bool
	}{
		{name: "a caller stating one contract below it", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion), stated: stateSeqContract - 1},
		{name: "a pod below shapedAnswersContract", remote: statePodAddr + ":41000", claims: podClaims(shapedAnswersContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(shapedAnswersContract - 1)},
		{name: "a caller stating the contract", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion), stated: stateSeqContract, carried: true},
		{name: "a pod at the contract", remote: statePodAddr + ":41000", claims: podClaims(stateSeqContract), carried: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), carried: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(shapedAnswersContract - 1), carried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateStating(t, stateServer(t, populatedState(t), tc.claims), tc.remote, tc.stated)
			seq, carried := stateSeqServed(t, body)
			id, run := stateIDServed(t, body), stateRunServed(t, body)
			if !tc.carried {
				if carried || id != "" || run != "" {
					t.Errorf("the state %s carries daemon.stateSeq = %d (on the wire = %v), daemon.stateId = %q or daemon.stateRun = %q; the reader before stateSeqContract refuses the three keys", body, seq, carried, id, run)
				}
				return
			}
			if !carried || seq != want || id != wantID || run != wantRun {
				t.Errorf("the state %s carries daemon.stateSeq %d (the key is on the wire = %v), daemon.stateId %q and daemon.stateRun %q, want the source's %d, %q and %q", body, seq, carried, id, run, want, wantID, wantRun)
			}
		})
	}
}

// A project whose daemon has committed no state event yet is served `stateSeq: 0`, never an
// absent key: the pointer keeps the sequence 0 apart from the body served down, which a reader at
// this contract takes as a daemon that serves no sequence at all.
func TestAStateSeqOfZeroIsServedAtItsContract(t *testing.T) {
	state := populatedState(t)
	state.Daemon.StateSeq = new(int64(0))
	body := stateFrom(t, state, statePodAddr+":41000", podClaims(stateSeqContract))
	if seq, carried := stateSeqServed(t, body); !carried || seq != 0 {
		t.Fatalf("the state %s carries daemon.stateSeq %d (the key is on the wire = %v), want 0 on the wire", body, seq, carried)
	}
}

// The body a pod below the contract is served is a copy: the next caller of the same daemon, in no
// pod, is served the state as the source answered it, sequence, store and run and all.
func TestTheStateServedToAPodBeforeTheStateSeqContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(shapedAnswersContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if _, carried := stateSeqServed(t, behind); carried || stateRunServed(t, behind) != "" {
		t.Fatalf("the pod before the contract was served %s, want no stateSeq and no stateRun", behind)
	}
	if seq, carried := stateSeqServed(t, operator); !carried || seq == 0 || stateRunServed(t, operator) == "" {
		t.Fatalf("the operator was served %s, want the sequence and the run the source answered", operator)
	}
}

// stateSeqContract is the contract whose entry in version.go added the key, and this daemon's own
// contract is never below it: a caller at this contract is served the sequence.
func TestTheStateSeqContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedStateSeq = 293
	if stateSeqContract != addedStateSeq {
		t.Fatalf("stateSeqContract = %d, want %d, the contract whose entry in version.go added `daemon.stateSeq`",
			stateSeqContract, addedStateSeq)
	}
	if stateSeqContract > DaemonAPIVersion {
		t.Fatalf("stateSeqContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			stateSeqContract, DaemonAPIVersion)
	}
}
