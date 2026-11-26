package replay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The replay counts the holders of the cap as the daemon does, by the daemon's one count of them
// (record.HoldersOf, cybermonklabs/legion#5488), which counts no slot of an issue the project does
// not hold. From the boot's pass at 09:00 the replay's database holds a slot of OTHER-1, an issue of
// another project, beside ACME-1's, at a cap of 2: the state the daemon serves over those records
// (projection.Project) names OTHER-1 in `admission.active`, as it names every slot holder the issues
// table holds, and the daemon's free slots over them, the cap less the holders the cap counts as
// the pass reads them (record.SlotRule.CapCounted), are 1. ACME-2 and ACME-3 are filed at 09:10 and
// 09:11, and the record starts neither. The replay's passes start exactly as many of them as the
// daemon's free slots, one, ACME-2, the first filed: its free slots are the daemon's.
func TestTheReplayCountsTheSlotOfAnIssueTheProjectDoesNotHoldAsTheDaemonDoes(t *testing.T) {
	t.Parallel()
	const elsewhere = "OTHER-1"
	b := newWindowBuilder(t, clock(9, 0), clock(10, 0))
	b.capped(2)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 0), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Fix the broken checkout", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.file(clock(9, 11), "ACME-3", filing{Title: "Restore the price feed", Status: "todo", Rank: "a2", Labels: handedToLegion})
	window := b.build()
	rule := mainRule(t)

	read, free := false, -1
	var active []string
	look := func(ctx context.Context, st *store.Store, at time.Time) error {
		if read {
			return nil
		}
		read = true
		return store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			records := record.NewStoreWithClock(func() time.Time { return at })
			if err := records.PutIssue(ctx, tx, record.Issue{Key: elsewhere, Tree: elsewhere, Project: "OTHER", Title: "Another project's work",
				Phase: phase.Implementing, Status: "in_progress", DispatchStatus: "in_progress", Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets"}); err != nil {
				return err
			}
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: elsewhere, Index: 100, AdmittedAt: at}); err != nil {
				return err
			}
			issues, err := records.LiveIssues(ctx, tx, window.Project)
			if err != nil {
				return err
			}
			slots, err := records.Slots(ctx, tx)
			if err != nil {
				return err
			}
			limit := capAt(window, at)
			free = limit - rule.CapCounted(record.HoldersOf(issues, slots))
			claims, err := st.ClaimsTx(ctx, tx)
			if err != nil {
				return err
			}
			state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: records, Project: window.Project, Steps: window.GateSteps,
				Claims: claims, Now: at, SlotRule: rule, Cap: limit})
			if err != nil {
				return err
			}
			active = state.Admission.Active
			return nil
		})
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir(), atSettled: look})
	if err != nil {
		t.Fatalf("replay the window under main: %v", err)
	}
	if !read {
		t.Fatal("the replay settled at no moment, so no slot of another project was written")
	}
	if !slices.Contains(active, elsewhere) {
		t.Errorf("the state over the replay's records names %v in admission.active, want %s among them", active, elsewhere)
	}
	if free != 1 {
		t.Errorf("the daemon's free slots over the replay's records are %d, want 1: the cap of 2 less ACME-1, and no slot of %s", free, elsewhere)
	}
	var started []string
	for _, key := range []string{"ACME-2", "ACME-3"} {
		if _, took := result.Scorecard.FirstSlot[key]; took {
			started = append(started, key)
		}
	}
	if len(started) != free || !slices.Equal(started, []string{"ACME-2"}) {
		t.Errorf("the replay's passes start %v of the queued roots, want as many as the daemon's %d free slots, ACME-2", started, free)
	}
}
