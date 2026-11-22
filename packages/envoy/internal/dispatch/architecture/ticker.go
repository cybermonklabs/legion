package architecture

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// SyncInterval is the longest the server waits between two passes that re-import every enabled
// architecture source: each wait is more than 0.9 of it and at most it (duty.RunJittered). While
// delivery settings are saved, a push of a source's branch imports that source at once
// (Importer.Pushed, through the delivery intake, which runs only then), and the Refresh button and
// the `dispatch architecture-sync` command are the impatient paths; this pass is the repair where
// a push event never arrives (GitHub did not deliver it, or no process took it), and it keeps a
// model honest on its own.
const SyncInterval = 5 * time.Minute

// architectureImportLease names the lease one import pass runs under. Every pass re-reads each
// enabled source's tree from GitHub against one installation's rate limit, so N processes
// importing is N times those calls for one result.
const architectureImportLease = "architecture-import"

// Run re-imports every enabled architecture source about every SyncInterval, one source at a time,
// until ctx is cancelled. The first tick is jittered within one interval so a restart does not
// stampede GitHub, each later one comes at most SyncInterval after the pass before it ended, and a
// server without App credentials logs once and stops: no sync can be signed. Shares its
// jittered-ticker loop with delivery.Reconcile.Run via duty.RunJittered.
//
// Each pass runs under architectureImportLease for one period of duty.LeasePeriod(SyncInterval),
// so one process imports per period however many serve the database - the second process's tick,
// jittered to a phase of its own, finds that period imported and skips rather than spending the
// same GitHub calls again; leases is required, as it is of every leased loop (cmd/dispatch exits at
// boot if the lease pool cannot be opened).
//
// atStart runs one pass as Run starts, before its jittered ticks, for an end-to-end test that must
// see a pass inside its window (DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START, under the test hooks),
// as delivery.Reconcile.FirstPassAtStart does for the delivery reconcile. The pass takes the lease
// for the period its start falls in, as any tick's does: a bucket of duty.LeasePeriod(SyncInterval),
// aligned to the epoch. The first jittered tick comes within one SyncInterval of the start, so it
// can land in that bucket and find it served. A deployment never sets it: the jitter is what keeps
// a restart from stampeding GitHub.
func Run(ctx context.Context, importer *Importer, leases duty.Leases, atStart bool) {
	if atStart && importer.HasApp() {
		syncAllLeased(ctx, importer, leases, time.Now())
	}
	duty.RunJittered(ctx, SyncInterval, func() bool {
		if importer.HasApp() {
			return false
		}
		slog.Info("dispatch architecture: no GitHub App key — the architecture importer is idle")
		return true
	}, func(ctx context.Context, due time.Time) { syncAllLeased(ctx, importer, leases, due) })
}

// syncAllLeased runs one import pass under the lease, for the tick due at due. Its period is
// duty.LeasePeriod(SyncInterval), shorter than any wait the loop leaves between two ticks, so a
// lone process's tick is never refused as a period its own last pass served.
//
// The pass the lease runs is one attempt at reading GitHub (githubreads), recorded through
// importer.GitHubReads when it ends: the GitHub client notes on it each request and each failure
// no caller takes as data, and each sync notes the reads of GitHub it gave up (syncOnce). It opens
// here and not in the sync, which the Settings Refresh route, `dispatch architecture-sync` and the
// import a push starts (Importer.Pushed's worker, through syncAfter) also run. It is recorded on
// the context the lease handed the pass, so a pass this process's shutdown cut short records
// nothing.
func syncAllLeased(ctx context.Context, importer *Importer, leases duty.Leases, due time.Time) {
	duty.Exclusive(ctx, leases, architectureImportLease, due, duty.LeasePeriod(SyncInterval), func(caller context.Context) {
		ctx, attempt := githubreads.Begin(caller)
		defer githubreads.Finish(caller, githubreads.ArchitectureImport, attempt, importer.GitHubReads, nil)
		syncAll(ctx, importer)
	})
}

// syncAll imports every enabled source, ordered by project and then id; a
// source whose import fails leaves the rest to import.
func syncAll(ctx context.Context, importer *Importer) {
	sources, err := importer.ListEnabled(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("dispatch architecture: list sources", "error", err)
		}
		return
	}
	for _, enabled := range sources {
		if ctx.Err() != nil {
			return
		}
		source, err := importer.Sync(ctx, enabled.Project, enabled.Repo)
		switch {
		case err == nil:
			commit := ""
			if source.LastCommit != nil {
				commit = *source.LastCommit
			}
			slog.Debug("dispatch architecture: synced", "project", enabled.Project, "repo", enabled.Repo, "commit", commit)
		case errors.Is(err, context.Canceled):
			return
		default:
			// The failure is already on the source row (and, on a change, in a
			// sync_failed event); the log is for the operator watching stderr.
			slog.Warn("dispatch architecture: sync failed", "project", enabled.Project, "repo", enabled.Repo, "error", err)
		}
	}
}
