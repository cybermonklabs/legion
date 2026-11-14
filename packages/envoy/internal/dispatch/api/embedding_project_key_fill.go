package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// embeddingProjectKeyFillLease names the lease the one pass of the embeddings project_key fill
// runs under. Two server processes in a roll must never both fill: the pass is one to two hours
// of labelling on a deployment of a million vectors, each label about 253 bytes of heap and
// 4.9 KiB of growth in the vector index (store.EmbeddingProjectKeyBatch holds the measurements),
// and the second process would walk the same keyset behind the first's commits, reading every
// row the first had already labelled for no row of its own.
//
// Lower case and hyphenated, saying what the pass does rather than which release wanted it, as
// every other lease name is (they are listed in store.LeasePoolSize's comment).
const embeddingProjectKeyFillLease = "embeddings-project-key-fill"

// embeddingProjectKeyFillRetry is how long a process waits before trying for the lease again
// after a try that neither finished the labelling nor ran at all. Five minutes: the pass itself
// is one to two hours, so five minutes is lost in the noise of it, and it is the whole of what a
// holder that dies mid-pass costs - the other process picks the pass up within five minutes of
// that session ending, rather than when something next boots. A process that keeps skipping pays
// one try for the lease and one read of a one-row table every five minutes for as long as the
// labelling lasts, which is a dozen round trips an hour.
//
// It is a variable only so a test can drive the retry without waiting five minutes, as
// store.embeddingProjectKeyProgressEvery is a variable only so a test can see a progress line.
var embeddingProjectKeyFillRetry = 5 * time.Minute

// RunEmbeddingProjectKeyFill makes store.FillEmbeddingProjectKeys' pass under
// embeddingProjectKeyFillLease, and keeps trying for that lease until the labelling is over,
// whichever process finishes it. The boot starts it in the background (cmd/dispatch/main.go),
// which is where what it does and why it does not block the boot is written; this is the lease
// around it. It lives here beside the other leased passes over the store rather than in store
// itself, because duty's own tests read the store package, so a store that imported duty would
// be an import cycle in test.
//
// Why it tries again rather than making one attempt. duty.Exclusive makes exactly one: a
// process that finds the lease held logs duty's own "leased pass skipped" line and is done with
// the fill for the life of that process. This code made that one attempt until INFRA-988's
// second round measured what it costs. Two processes were raced at a database of unlabelled
// rows: the loser skipped and never looked again, the winner was killed mid-pass, and three rows
// stayed unlabelled with a live process beside them that would have labelled them - until
// something booted. A project-scoped meaning search keeps the owner-joined predicate for exactly
// as long as that lasts (api/search.go, meaningProjectScope), so "until something boots" is the
// recall the slower read serves. Trying again every embeddingProjectKeyFillRetry bounds it: the
// lease is the session's, so a dead holder's goes back as its backend exits, and the next try
// takes it and resumes from the cursor the dead holder's committed batches left.
//
// Why the loop is written here and not taken from duty. duty.RunLeased and
// duty.RunLeasedAfterEachPass are loops that never end: they tick for the life of the process,
// which is right for a maintenance loop that has work again every interval - the sweep beside
// this one, the block index - and wrong for a pass that is over for good once the rows are
// labelled, since both would go on taking the lease and reading the cursor row for ever to learn
// what this process already knows. Neither takes a stop condition. Both also gate on a period
// through duty_passes (duty/exclusive.go, readPeriod), and a pass made once in the life of a
// release serves no interval, which is why the period here is zero: a lease and no period, the
// pairing the delivery consumers' bind takes for its own non-periodic work
// (delivery/intake.go:660). So what duty gives this file is the gate (Exclusive) and the jitter
// (Jittered), and the stop is the one thing written here.
//
// A duty.Hold is the other primitive and is still the wrong one: a hold is for a duty one
// process must be seen to be holding and another must take over within seconds of it stopping,
// as the events outbox does, and it costs a row, a renewal every two seconds and a take-over
// path. Nothing here needs a take-over within seconds; a five-minute retry is the whole of the
// take-over this pass wants.
//
// What it logs is the fill's own lines (store.FillEmbeddingProjectKeys) and duty's "leased pass
// skipped" for each try that found the lease held. A failure is logged and the process serves
// on, for the reason cmd/dispatch/main.go gives, and is tried again at the next interval; a
// shutdown is not a failure, since ctx ending is how every background pass stops. The one
// failure it does not try again on is the progress row not being there to read - the row gone,
// or the table it lives in gone (progressRowGone) - which is the loop's third way out.
func RunEmbeddingProjectKeyFill(ctx context.Context, database *store.Store, leases duty.Leases) {
	for {
		finished, gone := false, false
		duty.Exclusive(ctx, leases, embeddingProjectKeyFillLease, time.Time{}, 0, func(ctx context.Context) {
			// The rows it had labelled are on the failure line: every batch before the one that
			// failed is committed and the cursor records them, so the count says how much of the
			// pass the next try has left rather than how much it lost.
			labelled, err := database.FillEmbeddingProjectKeys(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if gone = progressRowGone(err); gone {
					return
				}
				slog.Error("dispatch: the embeddings project_key fill failed", "error", err, "rows", labelled)
				return
			}
			finished = true
		})
		if finished || gone || ctx.Err() != nil {
			return
		}
		// The try was skipped or it failed. Jittered, so two processes that booted together do
		// not try in step for the length of the labelling.
		select {
		case <-ctx.Done():
			return
		case <-time.After(duty.Jittered(embeddingProjectKeyFillRetry)):
		}
		// Another process may have finished it while this one waited, and that is this loop's
		// other way out: one row read, no lease taken, and no "nothing to do" line of a pass that
		// never had anything to do.
		done, err := database.EmbeddingProjectKeyFillDone(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if progressRowGone(err) {
				return
			}
			slog.Error("dispatch: reading whether the embeddings project_key fill is finished failed", "error", err)
			continue
		}
		if done {
			return
		}
	}
}

// progressRowGone answers whether err is the fill's progress row not being there to read - the
// row gone, or the table it lives in gone (store.ErrEmbeddingProjectKeyProgressGone) - and
// writes the line that says so when it is, which is why it is called in place of each failure's
// own log line rather than beside it. What the two absences mean is the store's to say, and it
// says it in one sentinel every read of that row answers with, rather than in a SQLSTATE this
// package would have to match for itself.
//
// It is a reason to stop rather than to try again. Nothing in this release deletes either -
// 20261109120100 creates the table and inserts the row, and no statement removes either one -
// so a database without them is one somebody repaired by hand, one restored from a dump taken
// before that migration, or one somebody is in the middle of moving; it is not a binary newer
// than its database, since the boot migrates before it serves (cmd/dispatch/main.go). Waiting
// does not bring either back: the pass cannot read the cursor it starts from and the loop's own
// check cannot read its done, so both of them fail every time. Left to try again, this process
// logged two errors and took the lease every embeddingProjectKeyFillRetry for the rest of its
// life (INFRA-988). It says so once and stops; a project-scoped meaning search answers by the
// owner each arm joins meanwhile, as it does whenever that row cannot be read (api/search.go,
// embeddingProjectKeysFilled).
func progressRowGone(err error) bool {
	if !errors.Is(err, store.ErrEmbeddingProjectKeyProgressGone) {
		return false
	}
	slog.Error("dispatch: the embeddings project_key fill has no progress row to record its place in, so this process will not try to fill again",
		"error", err, "table", "embeddings_project_key_backfill")
	return true
}
