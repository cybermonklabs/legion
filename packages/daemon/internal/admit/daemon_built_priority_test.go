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

// A held summary, the one daemon-built path of admission that carries a priority, keeps the
// resolved priority, with its source, for an issue whose own priority is null, and a newer event's
// resolved priority replaces the held summary's, a present null included (C1 of
// dispatch://INFRA-5636).
func TestTheDaemonBuiltPathsOfAdmissionKeepAnInheritedPriority(t *testing.T) {
	inherited := record.Resolved{Priority: record.P0, From: "OTHER-7", Known: true}
	for _, tc := range []struct {
		name  string
		event intake.DispatchIssue
		want  record.Resolved
	}{
		{name: "a newer event's resolved P1", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 7,
			Resolved: record.Resolved{Priority: record.P1, From: "OTHER-8", Known: true}}, want: record.Resolved{Priority: record.P1, From: "OTHER-8", Known: true}},
		{name: "a newer event's present null", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 7,
			Resolved: record.Resolved{Known: true}}, want: record.Resolved{Known: true}},
		{name: "an older event's resolved P1", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 4,
			Resolved: record.Resolved{Priority: record.P1, Known: true}}, want: inherited},
	} {
		t.Run("a held summary: "+tc.name, func(t *testing.T) {
			admission := New(record.NewStore(), nil, 1, testProject, nil)
			admission.holdDeferred(map[string]intake.DispatchIssue{"LEGION-1": {Key: "LEGION-1", Status: "todo", Seq: 5, Resolved: inherited}}, 10)
			admission.refreshHeldSummary(tc.event)
			held, ok := stillHeld(admission)["LEGION-1"]
			if !ok {
				t.Fatal("LEGION-1 is no longer held, want it held: a refresh never releases a hold")
			}
			if held.Resolved != tc.want {
				t.Errorf("the held summary after the event at sequence %d holds %+v, want %+v", tc.event.Seq, held.Resolved, tc.want)
			}
		})
	}
}
