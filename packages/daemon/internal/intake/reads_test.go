package intake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// probeHandler is a handler that runs probe in its ReadFact, and in its Apply too where inApply is
// set. It keeps what each run got. ApplyFact runs both on one goroutine, so it needs no lock.
type probeHandler struct {
	probe   func(ctx context.Context) (bool, error)
	inApply bool
	read    []probed
	applied []probed
}

// probed is what one run of a probe got, and its error.
type probed struct {
	got bool
	err error
}

func (h *probeHandler) ReadFact(ctx context.Context, _ store.TxPool, _ Fact) context.Context {
	got, err := h.probe(ctx)
	h.read = append(h.read, probed{got: got, err: err})
	return ctx
}

func (h *probeHandler) Apply(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
	if h.inApply {
		got, err := h.probe(ctx)
		h.applied = append(h.applied, probed{got: got, err: err})
	}
	return Result{}, nil
}

// tryFactLock tries, on a connection of raw of its own, the lock every fact applies under
// (SerializeFacts). It lets the lock go again where it got it.
func tryFactLock(ctx context.Context, raw *pgxpool.Pool) (bool, error) {
	conn, err := raw.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock(hashtext('legion.apply_fact'))").Scan(&got); err != nil {
		return false, err
	}
	if got {
		if _, err := conn.Exec(ctx, "select pg_advisory_unlock(hashtext('legion.apply_fact'))"); err != nil {
			return true, err
		}
	}
	return got, nil
}

// turnGuard is how long a probe waits for the pool's turn before it gives up.
const turnGuard = 500 * time.Millisecond

// applyProbing applies one fact on pool with a handler that runs probe, in its Apply too where
// inApply is set, and answers the handler.
func applyProbing(t *testing.T, pool store.TxPool, eventID string, probe func(ctx context.Context) (bool, error), inApply bool) *probeHandler {
	t.Helper()
	handler := &probeHandler{probe: probe, inApply: inApply}
	if _, err := ApplyFact(context.Background(), pool, "test", eventID, DispatchConsumerPosition{AckFloorStream: 1}, handler); err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}
	return handler
}

// A handler that reads before a fact's transaction (Reader) reads outside it. Its read holds no
// fact lock, and no pool's turn, so a read that waits holds up no other fact.
//   - The fact lock: inside ReadFact, another connection takes the fact lock and gets it. The
//     control runs the same probe inside the transaction, where the fact holds the lock.
//   - The pool's turn: inside ReadFact, the handler asks the pool for its turn and gets it. The
//     control asks inside the transaction, where the fact holds the turn, and waits out its guard.
//   - A fact applied inside a transaction already open reads nothing, since its read would run
//     inside that transaction.
func TestApplyFactReadsBeforeItsTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("the fact lock", func(t *testing.T) {
		t.Parallel()
		raw := migratedPool(t)
		lock := func(ctx context.Context) (bool, error) { return tryFactLock(ctx, raw) }
		handler := applyProbing(t, raw, "lock-in-read", lock, false)
		if len(handler.read) != 1 {
			t.Fatalf("ApplyFact ran ReadFact %d times, want once, before the fact's transaction", len(handler.read))
		}
		if got := handler.read[0]; !got.got || got.err != nil {
			t.Fatalf("inside ReadFact another connection got the fact lock %t (%v), want true: the read ran under the fact lock", got.got, got.err)
		}
		t.Run("control: the same probe inside the transaction", func(t *testing.T) {
			handler := applyProbing(t, raw, "lock-in-apply", lock, true)
			if len(handler.applied) != 1 || handler.applied[0].err != nil || handler.applied[0].got {
				t.Fatalf("inside the fact's transaction the probe got %+v, want one run that did not get the fact lock", handler.applied)
			}
		})
	})
	t.Run("the pool's turn", func(t *testing.T) {
		t.Parallel()
		pool := store.NewPool(migratedPool(t))
		takeTurn := func(ctx context.Context) (bool, error) {
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnGuard)
			defer cancel()
			release, err := pool.FactTurn(bounded)
			if err != nil {
				return false, err
			}
			release()
			return true, nil
		}
		handler := applyProbing(t, pool, "turn-in-read", takeTurn, false)
		if len(handler.read) != 1 {
			t.Fatalf("ApplyFact ran ReadFact %d times, want once, before the fact took its pool's turn", len(handler.read))
		}
		if got := handler.read[0]; !got.got || got.err != nil {
			t.Fatalf("inside ReadFact the pool's turn was taken %t (%v), want true: the read ran while the fact held the turn", got.got, got.err)
		}
		t.Run("control: the same probe inside the transaction", func(t *testing.T) {
			handler := applyProbing(t, pool, "turn-in-apply", takeTurn, true)
			if len(handler.applied) != 1 || handler.applied[0].got || !errors.Is(handler.applied[0].err, context.DeadlineExceeded) {
				t.Fatalf("inside the fact's transaction the probe got %+v, want one run that waited out its guard of %s for the turn", handler.applied, turnGuard)
			}
		})
	})
	t.Run("a fact inside an open transaction reads nothing", func(t *testing.T) {
		t.Parallel()
		pool := store.NewPool(migratedPool(t))
		handler := &probeHandler{probe: func(context.Context) (bool, error) { return true, nil }}
		if err := store.Transact(ctx, pool, func(ctx context.Context, _ pgx.Tx) error {
			_, err := ApplyFact(ctx, pool, "test", "joined", DispatchConsumerPosition{AckFloorStream: 1}, handler)
			return err
		}); err != nil {
			t.Fatalf("ApplyFact inside an open transaction: %v", err)
		}
		if len(handler.read) != 0 {
			t.Fatalf("ApplyFact inside an open transaction ran ReadFact %d times, want none: a read there runs inside that transaction", len(handler.read))
		}
	})
}
