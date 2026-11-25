package replay

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// coverState is what one settle of a replay shows of a parent and the issue beneath it: the phase
// the parent stands in, whether each holds a slot, and the holder of the parent's cover ("" for
// none).
type coverState struct {
	at                      time.Time
	parentPhase             phase.Phase
	parentHolds, childHolds bool
	parentHolder            string
}

// readCoverState reads parent's and child's coverState at at from the replay's database.
func readCoverState(ctx context.Context, st *store.Store, at time.Time, parent, child string) (coverState, error) {
	state := coverState{at: at}
	err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, slot := range slots {
			state.parentHolds = state.parentHolds || slot.Issue == parent
			state.childHolds = state.childHolds || slot.Issue == child
		}
		issue, err := records.Issue(ctx, tx, parent)
		if err != nil || issue == nil {
			return err
		}
		state.parentPhase = issue.Phase
		if err := tx.QueryRow(ctx, `select coalesce((select holder from replay_covers where waiter = $1), '')`, parent).Scan(&state.parentHolder); err != nil {
			return err
		}
		return nil
	})
	return state, err
}

// A parent past its review whose sign-off the workflow holds behind an issue beneath it waits on
// that issue under the rule cover (spec v4 R2's first addition; HeldSignOff,
// record.LineFacts.BeneathWaits), and a started issue it waits on that holds a slot and covers
// nobody holds the parent's slot for it. The test replays the fixture window every-trigger under
// cover: ACME-1's architect signs off in accepting while its child ACME-3 works on its own slot,
// and the workflow holds the sign-off. Some settle must show ACME-1 in accepting holding no slot,
// covered by ACME-3, which holds one; and once ACME-3 is done the cover is gone and ACME-1 holds its
// slot again, at the same settle as the cover's end.
func TestAHeldSignOffIsAWaitACoverHolds(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	var states []coverState
	cfg := RunConfig{Window: fixtureWindow(t, "every-trigger"), Rule: replayedRule(t, "cover"), Branch: MainBranch, Seed: 1,
		Slowdown: 1, PGDSN: dsn, Out: t.TempDir()}
	cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
		state, err := readCoverState(ctx, st, at, "ACME-1", "ACME-3")
		states = append(states, state)
		return err
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay every-trigger under cover: %v", err)
	}
	covered := -1
	for i, state := range states {
		if state.parentPhase == phase.Accepting && state.parentHolder == "ACME-3" && !state.parentHolds && state.childHolds {
			covered = i
			break
		}
	}
	if covered < 0 {
		t.Fatalf("no settle shows ACME-1 in accepting covered by ACME-3 behind its held sign-off: %+v", states)
	}
	for _, state := range states[covered:] {
		if state.parentHolder == "ACME-3" {
			continue
		}
		if !state.parentHolds && state.parentPhase != phase.Done {
			t.Errorf("at %s ACME-1's cover by ACME-3 ended and ACME-1, in %s, holds no slot of its own", stamp(state.at), state.parentPhase)
		}
		return
	}
	t.Errorf("ACME-1's cover by ACME-3 never ended: %+v", states[len(states)-1])
}
