package docs

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stateCopiesAndReadability queues the copy relations and ask readability under the version's
// event, after the block index has planned the nodes. A settlement without a version uses NoEvent.
func (st *settlement) stateCopiesAndReadability(ctx context.Context, at time.Time, cause int64) error {
	who := st.credit.versionActor()
	copies, err := st.reconciliation.claimCopies(ctx, st.tx, st.room, who, at)
	if err != nil {
		return err
	}
	readability, err := st.reconciliation.stateInvalidReasons(ctx, st.tx, who, at)
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
