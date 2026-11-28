package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// firstPaintsSummary is the summary line the boot's fold of the stale cuts' first paints logs.
const firstPaintsSummary = `msg="dispatch: folded the first paints of the stale cuts of the stored eval figures"`

// The boot's fold of the stale cuts' first paints (foldFirstPaints): over a project whose stored
// figures are built and a mark of every eval of its cut of a repository, its first paint owed, as
// the boot's rename pass leaves one, the fold, with no process holding the refresher's lease,
// records the mark's first paint folded through the end day the figures are built through, and
// logs its summary at INFO with the marks and evals it folded, that it waited for no one and how
// long it took. Red where the boot's step folds nothing, or logs no summary.
func TestTheBootFoldsTheFirstPaintsOfTheStaleCutsAndLogsItsSummary(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	through := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice');
		insert into eval_figures_built (project_key, through, built_by) values ('ACME', '2026-10-03', 'test');
		insert into eval_figure_cuts_stale (project_key, repo, generation, ready_through, next_day, evals) values ('ACME', 'acme/widgets', 1, null, null, null);
	`); err != nil {
		t.Fatalf("seed a stale cut whose first paint is owed: %v", err)
	}
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	foldFirstPaints(ctx, database.Pool, storetest.Leases(t, database.Pool), "test", firstPaintBootTimeout)

	if out := logs.String(); !strings.Contains(out, `level=INFO `+firstPaintsSummary+` marks=1 evals=0 waited=false took_ms=`) {
		t.Errorf("logged, want the summary at INFO with marks=1 evals=0 waited=false and the time it took:\n%s", out)
	}
	var ready *time.Time
	if err := database.Pool.QueryRow(ctx, `select ready_through from eval_figure_cuts_stale where project_key = 'ACME' and repo = 'acme/widgets'`).Scan(&ready); err != nil {
		t.Fatalf("read the mark: %v", err)
	}
	if ready == nil || !ready.Equal(through) {
		t.Errorf("the mark's first paint stands folded through %v, want %s", ready, through.Format(time.DateOnly))
	}
}
