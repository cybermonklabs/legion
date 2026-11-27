package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A fact built from what Dispatch sent replaces the stored priority, null included; a fact the
// daemon builds from its own record keeps it. These are the two daemon-built paths of admission:
// the outbox's consumption of a hand-over, and the held summary a newer event refreshes.

// The outbox's consumption of a hand-over re-admits the child from its own record, which no
// Dispatch observation of this run brought, so the child keeps the priority that record holds.
func TestAHandOverTheOutboxConsumesKeepsTheChildsOwnPriority(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := priorityEngine()
	const root, child = "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}); err != nil {
			t.Fatalf("seed root: %v", err)
		}
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "B",
			Priority: record.P1, LastDispatchSeq: 1}); err != nil {
			t.Fatalf("seed child: %v", err)
		}
		if err := records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}); err != nil {
			t.Fatalf("seed fence: %v", err)
		}
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover", intake.Handover{Issue: child, Tree: root, Generation: 1}, engine, admission); err != nil {
		t.Fatalf("consume the hand-over: %v", err)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 || got.Priority != record.P1 {
		t.Fatalf("the consumed child is in tree %s at generation %d carrying %v, want its own tree at generation 2 at P1, the priority its record held",
			got.Tree, got.Generation, got.Priority)
	}
}

// A held, unrecorded key's summary is what release applies, and a newer event that creates no
// record refreshes it (refreshHeldSummary): the summary takes that event's priority, a newer event
// with none takes none, and an event at or behind the held summary's own sequence changes nothing.
func TestAHeldSummaryTakesANewerEventsPriorityAndKeepsItsOwnAgainstAnOlderOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event intake.DispatchIssue
		want  record.Priority
	}{
		{name: "a newer event's P0", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 7, Priority: record.P0}, want: record.P0},
		{name: "a newer event with no priority", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 7}, want: record.NoPriority},
		{name: "an older event's P0", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 4, Priority: record.P0}, want: record.P2},
		{name: "an event at the held sequence", event: intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 5, Priority: record.P0}, want: record.P2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admission := New(record.NewStore(), nil, 1, testProject, nil)
			admission.holdDeferred(map[string]intake.DispatchIssue{"LEGION-1": {Key: "LEGION-1", Status: "todo", Seq: 5, Priority: record.P2}}, 10)
			admission.refreshHeldSummary(tc.event)
			held, ok := stillHeld(admission)["LEGION-1"]
			if !ok {
				t.Fatal("LEGION-1 is no longer held, want it held: a refresh never releases a hold")
			}
			if held.Priority != tc.want {
				t.Errorf("the held summary after the event at sequence %d carrying %v holds %v, want %v", tc.event.Seq, tc.event.Priority, held.Priority, tc.want)
			}
		})
	}
}

// The two daemon-built paths of admission keep the resolved priority, with its source, for an issue
// whose own priority is null, and a newer event's resolved priority replaces the held summary's,
// a present null included (C1 of dispatch://INFRA-5636).
func TestTheDaemonBuiltPathsOfAdmissionKeepAnInheritedPriority(t *testing.T) {
	inherited := record.Resolved{Priority: record.P0, From: "OTHER-7", Known: true}
	t.Run("a consumed hand-over", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		const root, child = "LEGION-208", "LEGION-209"
		until := fixedNow.Add(time.Hour)
		parent := root
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			if err := records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}); err != nil {
				t.Fatalf("seed root: %v", err)
			}
			if err := records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "B",
				Resolved: inherited, LastDispatchSeq: 1}); err != nil {
				t.Fatalf("seed child: %v", err)
			}
			if err := records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}); err != nil {
				t.Fatalf("seed fence: %v", err)
			}
		})
		if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover", intake.Handover{Issue: child, Tree: root, Generation: 1}, priorityEngine(), admission); err != nil {
			t.Fatalf("consume the hand-over: %v", err)
		}
		if got := issue(t, pool, child); got.Generation != 2 || got.Priority != record.NoPriority || got.Resolved != inherited {
			t.Fatalf("the consumed child at generation %d carries %v and %+v, want generation 2, no own priority and the inherited %+v",
				got.Generation, got.Priority, got.Resolved, inherited)
		}
	})
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
