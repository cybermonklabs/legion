package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// embeddingProjectKeyFillLease names the lease the one pass of the embeddings project_key fill
// runs under. Two server processes in a roll must never both fill: the pass is about an hour of
// 6 KB row rewrites on a deployment of a million vectors (store.EmbeddingProjectKeyBatch holds
// the measurements), and the second process would walk the same keyset behind the first's
// commits, reading every row the first had already labelled for no row of its own.
//
// Lower case and hyphenated, saying what the pass does rather than which release wanted it, as
// every other lease name is (they are listed in store.LeasePoolSize's comment).
const embeddingProjectKeyFillLease = "embeddings-project-key-fill"

// RunEmbeddingProjectKeyFill makes one pass of store.FillEmbeddingProjectKeys under
// embeddingProjectKeyFillLease. The boot starts it in the background (cmd/dispatch/main.go),
// which is where what it does and why it does not block the boot is written; this is the lease
// around it. It lives here beside the other leased passes over the store rather than in store
// itself, because duty's own tests read the store package, so a store that imported duty would
// be an import cycle in test.
//
// duty.Exclusive itself, with no period, rather than one of duty's loops: this pass is
// once-only. Once every stored row is labelled there is nothing left for a later pass to do,
// and the cursor row of migration 20261109120100 records exactly that, so a pass that finds it
// finished reads one row and returns. duty.RunLeased and duty.RunLeasedAfterEachPass are the
// shapes of a loop that has work again every interval - the maintenance sweep beside this one,
// the block index - and either of them here would take the lease and read that one row for the
// life of the process, for ever, to learn again what the boot already learned. The period is
// zero for the same reason: a period is how duty_passes decides which process serves a
// repeating loop's interval (duty/exclusive.go, readPeriod), and a pass made once in the life
// of a release serves no interval. That pairing - a lease and no period - is what the delivery
// consumers' bind takes for its own non-periodic work (delivery/intake.go:660).
//
// A duty.Hold is the other primitive and is the wrong one here: a hold is for a duty one
// process must be seen to be holding and another must take over within seconds of it stopping,
// as the events outbox does, and it costs a row, a renewal every two seconds and a take-over
// path. Nothing here needs a take-over. The lease is a session-level lock, so a process killed
// mid-pass gives it back as its backend exits, and the batches it committed stand: the cursor
// keeps the place, and the next process to boot - which is what a roll or an out-of-memory kill
// produces - starts this again and resumes from there.
//
// A pass skipped because the other process holds the lease writes duty's own "leased pass
// skipped" line and nothing of ours: one skip, one line. A failure is logged and the process
// serves on, for the reason cmd/dispatch/main.go gives; a shutdown is not a failure, since ctx
// ending is how every background pass stops.
func RunEmbeddingProjectKeyFill(ctx context.Context, database *store.Store, leases duty.Leases) {
	duty.Exclusive(ctx, leases, embeddingProjectKeyFillLease, time.Time{}, 0, func(ctx context.Context) {
		// The rows it had labelled are on the failure line: every batch before the one that
		// failed is committed and the cursor records them, so the count says how much of the
		// pass the next boot has left rather than how much it lost.
		labelled, err := database.FillEmbeddingProjectKeys(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("dispatch: the embeddings project_key fill failed", "error", err, "rows", labelled)
		}
	})
}
