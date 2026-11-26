package docs

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stateCopiesAndReadability queues the copy relations and ask readability under the version's
// event, after the block index has planned the nodes. A settlement without a version uses NoEvent.
// The readability is what the reconciliation read of each ask's block, stated on the ask's node by
// the one writer of its tuples (asks.PlanInvalidReasons): the reason each block that fails the
// strict read fails it for, where the block has an ask row, as the node's `invalid` value, and
// the retraction of that value for each ask whose block reads again.
func (st *settlement) stateCopiesAndReadability(ctx context.Context, at time.Time, cause int64) error {
	who := st.credit.versionActor()
	copies, err := st.reconciliation.claimCopies(ctx, st.tx, st.room, who, at)
	if err != nil {
		return err
	}
	readability, err := asks.PlanInvalidReasons(ctx, st.tx, st.reconciliation.invalid, st.reconciliation.readable, who, at)
	if err != nil || copies == nil && readability == nil {
		return err
	}
	return write(ctx, st.tx, func(b *store.WriteBatch) error {
		if copies != nil {
			if err := copies(b, cause); err != nil {
				return err
			}
		}
		if readability != nil {
			return readability(b, cause)
		}
		return nil
	})
}
