package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAHeldSummaryTakesANewerEventsPriorityAndKeepsItsOwnAgainstAnOlderOne(t *testing.T) {
	for _, tc := range []struct {
		name           string
		seq            int64
		priority, want record.Priority
	}{
		{"newer P0", 7, record.P0, record.P0},
		{"newer null", 7, record.NoPriority, record.NoPriority},
		{"older P0", 4, record.P0, record.P2},
		{"same sequence", 5, record.P0, record.P2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			const key, parent = "LEGION-21", "LEGION-20"
			summary := dispatch.IssueSummary{Key: key, Title: key, Status: "todo", Rank: "A", LastSeq: 5, Parent: new(parent),
				ResolvedRepo: "acme/widgets", HandedOver: true, Priority: new(2)}
			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{summary}, 10, 2, false)
			apply(t, pool, admission, "held-event", intake.DispatchIssue{Key: key, Title: key, Status: "todo", Seq: tc.seq,
				Parent: parent, Rank: "A", Priority: tc.priority, ResolvedRepo: summary.ResolvedRepo, HandedOver: true}, priorityEngine())
			if got := maybeIssue(t, pool, key); got != nil {
				t.Fatalf("the unknown chain recorded an issue before release: %+v", got)
			}
			apply(t, pool, admission, "parent-found", intake.DispatchIssue{Key: parent, Title: parent, Status: "backlog", Seq: 1}, priorityEngine())
			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "release",
				intake.DispatchConsumerPosition{AckFloorStream: 10}, admission); err != nil {
				t.Fatal(err)
			}
			if got := issue(t, pool, key); got.Priority != tc.want {
				t.Fatalf("the released issue has %v, want %v", got.Priority, tc.want)
			}
		})
	}
}
