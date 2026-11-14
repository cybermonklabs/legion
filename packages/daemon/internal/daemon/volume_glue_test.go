package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's own glue between an issue pod's launcher and the issue's architect: a sweep the
// launcher reports reaches volumeTrimReports.report, which turns it into the fact the workflow
// tells the architect through. The sandbox runtime's cases stop at the seam this glue fills and
// the workflow's start at the fact it applies, so this is the one case that drives the part in
// between, through the guarded pool and the workflow engine. The boot binds three handlers
// (workflow.go: blockers' mirror, the engine and admission; daemon.go, plan.volumeReports.bind).
// The engine is the one that turns this fact into the notice; admission runs its promote pass on
// it, as on any fact it has no case of its own for, and the mirror does not act on it. So the
// case binds the engine alone, and it holds the notice, not admission's pass.
//
// It holds the two things the glue alone decides. That a report is applied at all, with what the
// launcher measured carried into the notice the architect reads. And that the event id is the
// daemon's own receipt rather than the report's contents: the launcher sends one report per
// sweep with nothing in the frame that numbers it, and a second sweep of a volume that is full
// and not changing reports the same numbers, which an id built from those numbers would have
// ApplyFact drop as already applied (volumeTrimReports.report).
func TestAVolumeReportThroughTheDaemonsGlueReachesTheArchitectEverySweep(t *testing.T) {
	ctx := context.Background()
	raw := testpg.ClonePool(t, "legion_volume_glue_test")
	st, err := store.Open(ctx, raw.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	pods, err := st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(),
		Pods: pods, Linger: time.Hour, Clock: clock}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	tx, err := raw.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	if err := records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main",
		GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing,
		Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}

	reports := &volumeTrimReports{}
	reports.bind(store.NewPool(raw), []intake.Handler{engine})
	sweep := shimwire.LauncherVolumeReport{Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		UsedBytes: 46_000_000_000, FreeBytes: 3_000_000_000, FreedBytes: 0,
		StopReason: "no entry is eligible",
		Sizes:      []shimwire.LauncherVolumeSize{{Path: "/legion/scratch/tmp", Bytes: 21_000_000_000}},
		SizesAt:    now}

	notices := func() []record.Notice {
		t.Helper()
		rows, err := raw.Query(ctx, `select payload from public.outbox where kind = 'notice' and issue = $1
			and payload->>'kind' = $2 order by id`, "LEGION-208", string(record.VolumeNearFullNotice))
		if err != nil {
			t.Fatalf("read the notices: %v", err)
		}
		defer rows.Close()
		var out []record.Notice
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				t.Fatalf("scan a notice: %v", err)
			}
			var notice record.Notice
			if err := json.Unmarshal(payload, &notice); err != nil {
				t.Fatalf("decode a notice: %v", err)
			}
			out = append(out, notice)
		}
		return out
	}

	if err := reports.report(ctx, "LEGION-208", "pod-uid-1", sweep); err != nil {
		t.Fatalf("the glue refused the first sweep's report: %v", err)
	}
	first := notices()
	if len(first) != 1 {
		t.Fatalf("the architect has %d volume-near-full notices after one report through the daemon's glue, want 1: a report the glue does not apply never reaches the architect", len(first))
	}
	for _, want := range []string{"pod-uid-1", "trim-1", "46000000000", "/legion/scratch/tmp"} {
		if !strings.Contains(first[0].Reason, want) {
			t.Errorf("the notice the architect reads does not carry %q of what the launcher reported: %q", want, first[0].Reason)
		}
	}

	// The same volume an hour and more later, still full and not changing: the same numbers,
	// which is the pod this whole notice exists for.
	now = now.Add(2 * time.Hour)
	if err := reports.report(ctx, "LEGION-208", "pod-uid-1", sweep); err != nil {
		t.Fatalf("the glue refused the second sweep's report: %v", err)
	}
	if got := len(notices()); got != 2 {
		t.Errorf("the architect has %d volume-near-full notices after two identical sweeps two hours apart, want 2: an event id built from the report's own contents makes the second the first one again, and ApplyFact drops it", got)
	}
}
