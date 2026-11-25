package record

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
)

const (
	checkStatesRepo = "acme/widgets"
	checkStatesH1   = "1111111111111111111111111111111111111111"
	checkStatesH3   = "3333333333333333333333333333333333333333"
)

// headStatesOf is what the store holds of issue's checks at head of checkStatesRepo.
func headStatesOf(t *testing.T, st *store.Store, issue, head string) HeadStates {
	t.Helper()
	var held HeadStates
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		held, err = NewStore().HeadStates(context.Background(), tx, issue, checkStatesRepo, head)
		must(t, err)
	})
	return held
}

// HeadStates reads each check of a head with its own roles and CI case: three checks of one head,
// each with roles of its own, one with none, come back each as it was put, so no read of one row
// carries into the state of another.
func TestHeadStatesReadsEachCheckAsItWasPut(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	put := []CheckState{
		{Issue: "ACME-1", Repo: checkStatesRepo, Head: checkStatesH1, Check: "k1", State: "yes", Roles: []string{"implementer", "tester"}, Seq: 3},
		{Issue: "ACME-1", Repo: checkStatesRepo, Head: checkStatesH1, Check: "k2", State: "yes", Roles: []string{"ci"}, CICase: "success", Seq: 4},
		{Issue: "ACME-1", Repo: checkStatesRepo, Head: checkStatesH1, Check: "k3", State: "no", Reason: "no_result", Roles: []string{}, CICase: "not_completed", Seq: 5},
	}
	want := HeadStates{Checks: []string{"k1", "k2", "k3"}, States: map[string]CheckState{}}
	inTx(t, st, func(tx pgx.Tx) {
		for _, state := range put {
			_, err := NewStore().PutCheckState(ctx, tx, state)
			must(t, err)
			want.States[state.Check] = state
		}
		_, err := NewStore().PutCheckSet(ctx, tx, "ACME-1", checkStatesRepo, checkStatesH1, want.Checks, 5)
		must(t, err)
	})
	if held := headStatesOf(t, st, "ACME-1", checkStatesH1); !reflect.DeepEqual(held, want) {
		t.Fatalf("ACME-1 at H1 = %#v, want %#v", held, want)
	}
}

// The store keeps, for each check at each head of an issue, the state of the newest Dispatch event:
// a state at a lower sequence than the one it holds changes nothing, and one at a higher sequence
// replaces it. A state of the same check at another head, or of another issue at the same head,
// is a row of its own. A head's set of checks is kept behind the same guard, and a head with no
// set held reads as none, apart from a set of no checks.
func TestTheDaemonKeepsTheNewestStateOfEachCheckAtEachHead(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	k1 := func(issue, head, state, reason string, seq int64) CheckState {
		return CheckState{Issue: issue, Repo: checkStatesRepo, Head: head, Check: "k1", State: state, Reason: reason,
			Roles: []string{"tester"}, CICase: "", Seq: seq}
	}
	put := func(state CheckState) bool {
		t.Helper()
		var changed bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			changed, err = records.PutCheckState(ctx, tx, state)
			must(t, err)
		})
		return changed
	}
	putSet := func(issue, head string, checks []string, seq int64) bool {
		t.Helper()
		var changed bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			changed, err = records.PutCheckSet(ctx, tx, issue, checkStatesRepo, head, checks, seq)
			must(t, err)
		})
		return changed
	}
	stateAt := func(issue, head string) CheckState {
		t.Helper()
		held := headStatesOf(t, st, issue, head)
		if len(held.States) != 1 {
			t.Fatalf("the states of %s at %s = %#v, want k1's alone", issue, head, held.States)
		}
		return held.States["k1"]
	}

	if held := headStatesOf(t, st, "ACME-1", checkStatesH1); held.Checks != nil || len(held.States) != 0 {
		t.Fatalf("the store before any event holds %#v at H1, want no set and no state", held)
	}
	yes7 := k1("ACME-1", checkStatesH1, "yes", "", 7)
	if !put(yes7) {
		t.Fatal("the state of k1 at H1 at sequence 7 changed nothing, want it kept")
	}
	if got := stateAt("ACME-1", checkStatesH1); !reflect.DeepEqual(got, yes7) {
		t.Fatalf("k1 at H1 after sequence 7 = %#v, want %#v", got, yes7)
	}
	if put(k1("ACME-1", checkStatesH1, "no", "fail", 5)) {
		t.Fatal("the late state of k1 at H1 at sequence 5 changed the row, want the state of sequence 7 kept")
	}
	if got := stateAt("ACME-1", checkStatesH1); !reflect.DeepEqual(got, yes7) {
		t.Fatalf("k1 at H1 after the late sequence 5 = %#v, want the state of sequence 7 kept: %#v", got, yes7)
	}
	no9 := k1("ACME-1", checkStatesH1, "no", "no_result", 9)
	no9.CICase = "not_completed"
	if !put(no9) {
		t.Fatal("the state of k1 at H1 at sequence 9 changed nothing, want it to replace sequence 7's")
	}
	if got := stateAt("ACME-1", checkStatesH1); !reflect.DeepEqual(got, no9) {
		t.Fatalf("k1 at H1 after sequence 9 = %#v, want %#v", got, no9)
	}
	if put(no9) {
		t.Fatal("the state of sequence 9 put again changed the row, want nothing changed")
	}

	yes3 := k1("ACME-1", checkStatesH3, "yes", "", 3)
	other := k1("ACME-2", checkStatesH1, "unknown", "pass_and_fail", 12)
	if !put(yes3) || !put(other) {
		t.Fatal("a state of k1 at H3, or of another issue at H1, changed nothing, want each kept as a row of its own")
	}
	if got := stateAt("ACME-1", checkStatesH1); !reflect.DeepEqual(got, no9) {
		t.Fatalf("k1 at H1 after the states at H3 and of ACME-2 = %#v, want sequence 9's untouched: %#v", got, no9)
	}
	if got := stateAt("ACME-1", checkStatesH3); !reflect.DeepEqual(got, yes3) {
		t.Fatalf("k1 at H3 = %#v, want %#v", got, yes3)
	}
	if got := stateAt("ACME-2", checkStatesH1); !reflect.DeepEqual(got, other) {
		t.Fatalf("ACME-2's k1 at H1 = %#v, want %#v", got, other)
	}

	if !putSet("ACME-1", checkStatesH1, []string{"k1", "k2"}, 7) {
		t.Fatal("the set of H1 at sequence 7 changed nothing, want it kept")
	}
	if putSet("ACME-1", checkStatesH1, []string{"k1"}, 5) {
		t.Fatal("the late set of H1 at sequence 5 changed the row, want the set of sequence 7 kept")
	}
	if got := headStatesOf(t, st, "ACME-1", checkStatesH1).Checks; !reflect.DeepEqual(got, []string{"k1", "k2"}) {
		t.Fatalf("the set of H1 after the late sequence 5 = %#v, want sequence 7's [k1 k2]", got)
	}
	if !putSet("ACME-1", checkStatesH1, nil, 9) {
		t.Fatal("the set of no checks at sequence 9 changed nothing, want it to replace sequence 7's")
	}
	if got := headStatesOf(t, st, "ACME-1", checkStatesH1).Checks; got == nil || len(got) != 0 {
		t.Fatalf("the set of H1 after sequence 9 = %#v, want a held set of no checks, not none", got)
	}
	if got := headStatesOf(t, st, "ACME-1", checkStatesH3).Checks; got != nil {
		t.Fatalf("the set of H3 = %#v, want none: no set of H3 was put", got)
	}
}

// The fence of a tester's handoff at serial S passes only where the daemon holds every state event
// of the issue from the lowest serial it holds through S: with 3, 4 and 6 held, the fence of 4
// passes and the fence of 6 waits for 5. A serial below the lowest held is an event published
// before the daemon held any, so its fence passes, and a fence of 0 is a handoff that carries no
// serial, which passes at once. A daemon that holds no serial of the issue waits for any fence
// above 0, and another issue's serials count for nothing.
func TestTheStatesFenceNeedsEverySerialThroughTheHandoffs(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	hold := func(issue string, serials ...int64) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			for _, serial := range serials {
				must(t, records.PutCheckStateSerial(ctx, tx, issue, serial))
			}
		})
	}
	reached := func(issue string, serial int64) bool {
		t.Helper()
		var passes bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			passes, err = records.StatesFenceReached(ctx, tx, issue, serial)
			must(t, err)
		})
		return passes
	}

	hold("ACME-2", 1, 2, 3, 4, 5, 6, 7)
	if !reached("ACME-1", 0) || reached("ACME-1", 1) {
		t.Fatal("with no serial of ACME-1 held, the fence of 0 must pass and the fence of 1 must wait, whatever ACME-2 holds")
	}
	hold("ACME-1", 3, 4, 6, 4)
	for _, tc := range []struct {
		serial int64
		passes bool
	}{{0, true}, {2, true}, {3, true}, {4, true}, {5, false}, {6, false}, {7, false}} {
		if got := reached("ACME-1", tc.serial); got != tc.passes {
			t.Errorf("with 3, 4 and 6 held, the fence of %d passes = %t, want %t", tc.serial, got, tc.passes)
		}
	}
	hold("ACME-1", 5)
	for _, tc := range []struct {
		serial int64
		passes bool
	}{{5, true}, {6, true}, {7, false}} {
		if got := reached("ACME-1", tc.serial); got != tc.passes {
			t.Errorf("with 3, 4, 5 and 6 held, the fence of %d passes = %t, want %t", tc.serial, got, tc.passes)
		}
	}

	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return records.PutCheckStateSerial(ctx, tx, "ACME-1", 0)
	}); err == nil ||
		!strings.Contains(err.Error(), "serial 0") {
		t.Fatalf("putting serial 0 answered %v, want an error naming it: a serial counts from 1", err)
	}
}

// The merge that closes an issue deletes every state, set and serial the store holds of it, and
// nothing of another issue.
func TestDeletingAnIssuesCheckStatesLeavesNothingOfIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	for _, issue := range []string{"ACME-1", "ACME-2"} {
		inTx(t, st, func(tx pgx.Tx) {
			changed, err := records.PutCheckState(ctx, tx, CheckState{Issue: issue, Repo: checkStatesRepo, Head: checkStatesH1,
				Check: "k1", State: "yes", Seq: 4})
			must(t, err)
			setChanged, err := records.PutCheckSet(ctx, tx, issue, checkStatesRepo, checkStatesH1, []string{"k1"}, 4)
			must(t, err)
			if !changed || !setChanged {
				t.Fatalf("the first state and set of %s changed nothing", issue)
			}
			must(t, records.PutCheckStateSerial(ctx, tx, issue, 1))
		})
	}
	inTx(t, st, func(tx pgx.Tx) { must(t, records.DeleteCheckStates(ctx, tx, "ACME-1")) })

	if held := headStatesOf(t, st, "ACME-1", checkStatesH1); held.Checks != nil || len(held.States) != 0 {
		t.Fatalf("ACME-1 at H1 after the delete = %#v, want no set and no state", held)
	}
	want := HeadStates{Checks: []string{"k1"}, States: map[string]CheckState{"k1": {Issue: "ACME-2", Repo: checkStatesRepo,
		Head: checkStatesH1, Check: "k1", State: "yes", Roles: []string{}, Seq: 4}}}
	if held := headStatesOf(t, st, "ACME-2", checkStatesH1); !reflect.DeepEqual(held, want) {
		t.Fatalf("ACME-2 at H1 after ACME-1's delete = %#v, want %#v", held, want)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if passes, err := records.StatesFenceReached(ctx, tx, "ACME-1", 1); err != nil || passes {
			t.Fatalf("ACME-1's fence of 1 after the delete = %t, %v; want it to wait: its serials are gone", passes, err)
		}
		if passes, err := records.StatesFenceReached(ctx, tx, "ACME-2", 1); err != nil || !passes {
			t.Fatalf("ACME-2's fence of 1 after ACME-1's delete = %t, %v; want it to pass", passes, err)
		}
	})
}
