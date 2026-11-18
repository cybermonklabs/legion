package architecture

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// SyncInterval is how often the server re-imports every enabled architecture
// source. The Refresh button and the `dispatch architecture-sync` command are the
// impatient paths; this one keeps a model honest on its own.
const SyncInterval = 5 * time.Minute

// architectureImportLease names the lease one import pass runs under. Every pass re-reads each
// enabled source's tree from GitHub against one installation's rate limit, so N processes
// importing is N times those calls for one result.
const architectureImportLease = "architecture-import"

// Run re-imports every enabled architecture source on SyncInterval, one
// source at a time, until ctx is cancelled. The first tick is jittered so a
// restart does not stampede GitHub, and a server without App credentials logs
// once and stops: no sync can be signed. Shares its jittered-ticker loop with
// delivery.Reconcile.Run via duty.RunJittered.
//
// Each pass runs under architectureImportLease for one SyncInterval, so one process imports per
// interval however many serve the database - the second process's tick, jittered to a phase of
// its own, finds that interval imported and skips rather than spending the same GitHub calls
// again; leases is required, as it is of every leased loop (cmd/dispatch exits at boot if the
// lease pool cannot be opened).
//
// atStart runs one pass as Run starts, before its jittered ticks, for an end-to-end test that must
// see a pass inside its window (DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START, under the test hooks),
// as delivery.Reconcile.FirstPassAtStart does for the delivery reconcile. The pass takes the lease
// for the current interval as any tick's does, so a jittered tick inside that interval finds it
// served. A deployment never sets it: the jitter is what keeps a restart from stampeding GitHub.
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

// syncAllLeased runs one import pass under the lease, for the SyncInterval this loop ticks on and
// the tick due at due.
//
// The pass the lease runs is one attempt at reading GitHub (githubreads), recorded through
// importer.GitHubReads when it ends: the GitHub client notes on it each request and each failure
// no caller takes as data, and each sync notes the reads of GitHub it gave up (syncOnce). It opens
// here and not in Sync, which the Settings Refresh route and `dispatch architecture-sync` also
// run, and it is recorded on the context the lease handed the pass, so a pass this process's
// shutdown cut short records nothing.
func syncAllLeased(ctx context.Context, importer *Importer, leases duty.Leases, due time.Time) {
	duty.Exclusive(ctx, leases, architectureImportLease, due, SyncInterval, func(caller context.Context) {
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
