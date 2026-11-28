package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// stateEventFails is a handler that fails every fact after the workflow applied it, as any later
// handler or the commit can.
type stateEventFails struct{}

func (stateEventFails) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	return intake.Result{}, errors.New("a later handler failed")
}

// A state event's state, its head's set and its serial are written in the fact's own transaction,
// so they commit with the event's record as processed and never apart from it: a failure after
// the workflow's handler leaves none of them and no processed record, and the event applied again
// leaves all four.
func TestAStateEventIsStoredInTheFactsOwnTransaction(t *testing.T) {
	t.Parallel()
	r := newStatesRig(t, phase.Testing)
	state := intake.DispatchCheckState{Check: "k1", State: "no", Reason: "fail", Roles: []string{"implementer"}, CICase: "not_completed",
		Checks: []string{"k1", "k2"}, Serial: 3}
	fact, _, err := intake.DecodeDispatchEvent("LEGION", "LEGION-208", checkstatetest.Event(t, "LEGION-208", statesRepo, headOne, checkstatetest.Seq(3), state))
	if err != nil {
		t.Fatalf("decode the state event: %v", err)
	}
	const eventID = "dispatch-event:LEGION-208:1003"
	stored := func() (record.HeadStates, bool, int) {
		var held record.HeadStates
		var reached bool
		var processed int
		seedRecord(t, r.pool, func(tx pgx.Tx) error {
			records := record.NewStore()
			var err error
			if held, err = records.HeadStates(context.Background(), tx, "LEGION-208", statesRepo, headOne); err != nil {
				return err
			}
			if reached, err = records.StatesFenceReached(context.Background(), tx, "LEGION-208", 3); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `select count(*) from processed_events where source = 'dispatch' and event_id = $1`, eventID).Scan(&processed)
		})
		return held, reached, processed
	}

	if _, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", eventID, fact, r.engine, stateEventFails{}); err == nil {
		t.Fatal("the state event applied with a failing handler after the workflow's, want the failure")
	}
	if held, reached, processed := stored(); held.Checks != nil || len(held.States) != 0 || reached || processed != 0 {
		t.Fatalf("after the failed apply the store holds the set %v, states %v, serial 3 %t and %d processed records; want none of them", held.Checks, held.States, reached, processed)
	}

	if result, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", eventID, fact, r.engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the state event: %+v, %v", result.Refusal, err)
	}
	held, reached, processed := stored()
	want := record.CheckState{Issue: "LEGION-208", Repo: statesRepo, Head: headOne, Check: "k1", State: "no", Reason: "fail", CICase: "not_completed",
		Roles: []string{"implementer"}, Seq: checkstatetest.Seq(3)}
	if !reflect.DeepEqual(held.Checks, []string{"k1", "k2"}) || !reflect.DeepEqual(held.States, map[string]record.CheckState{"k1": want}) || !reached || processed != 1 {
		t.Fatalf("after the apply the store holds the set %v, states %+v, serial 3 %t and %d processed records; want [k1 k2], k1 %+v, the serial and the record", held.Checks, held.States, reached, processed, want)
	}
}
