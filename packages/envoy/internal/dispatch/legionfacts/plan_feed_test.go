package legionfacts

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// planFeedMark is the key the plan feed's check notifies on workflowstore.PlansChannel after a plan
// write commits (deliveredPlans). It names no issue, so a pass that takes it computes nothing for
// it.
const planFeedMark = "NO-ISSUE-0"

// planFeed is the consumer's Deps.Plans as the server wires it: ListenForPlans on a listener hub
// of its own over the history's database (pglisten.Hub), once its LISTEN holds, and stop, which
// takes the hub's connection away for good, as a process whose listener is down has none.
func planFeed(t *testing.T, c *consumer) (*PlanSignals, func()) {
	t.Helper()
	hub := &pglisten.Hub{Owner: "legionfacts plan feed test", Dial: c.database.Pool.Listener}
	plans := ListenForPlans(hub)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		testwait.Receive(t, "the plan feed's hub to stop", done)
	})
	t.Cleanup(stop)
	testwait.Receive(t, "the LISTEN on workflowstore.PlansChannel", connected)
	return plans, stop
}

// deliveredPlans notifies planFeedMark on workflowstore.PlansChannel and waits until the feed
// holds it, and answers the keys the feed holds then. Postgres delivers the notifications of
// different transactions in the order the transactions committed, so every notification of a plan
// write that committed before the mark has reached the feed by then: a key it lacks is one no
// notification carried.
func deliveredPlans(t *testing.T, c *consumer, plans *PlanSignals) map[string]bool {
	t.Helper()
	c.exec(t, "notify the mark on the plans' channel", `select pg_notify($1, $2)`, workflowstore.PlansChannel, planFeedMark)
	return testwait.Until(t, "the mark notified after the plan write to reach the plan feed", func() (map[string]bool, bool) {
		plans.mu.Lock()
		defer plans.mu.Unlock()
		held := maps.Clone(plans.keys)
		return held, held[planFeedMark]
	})
}

// owesCompare is whether the feed owes the next pass the compare of every stored row's
// plan_tuples with its count now.
func owesCompare(plans *PlanSignals) bool {
	plans.mu.Lock()
	defer plans.mu.Unlock()
	return plans.compare
}

// ownerPlan is the plan of the issue's run of generation as the Legion daemon writes it when the
// issue's owner gave one: the owner's plan, version 1 of its document, with one task of steps.
func ownerPlan(c *consumer, issue string, generation int64, steps ...string) workflowstore.PlanWrite {
	return workflowstore.PlanWrite{
		ID: uuid.NewString(), Issue: issue, Project: &c.fixture.Project, Generation: new(generation),
		Source:   new(workflowstore.PlanSourceOwner),
		Document: workflowstore.PlanDocument{ArtifactID: uuid.NewString(), Version: 1},
		Tasks:    []workflowstore.PlanTask{{ID: "work", Steps: steps}},
	}
}

// writePlan writes the plan as the Legion daemon writes it (workflowstore.PutPlan), at the moment
// given, in a transaction of its own.
func writePlan(t *testing.T, c *consumer, write workflowstore.PlanWrite, at time.Time) {
	t.Helper()
	daemon := model.Actor{Kind: "session", ID: "legion-daemon:" + c.fixture.Project}
	if err := pgx.BeginFunc(t.Context(), c.database.Pool, func(tx pgx.Tx) error {
		_, err := workflowstore.PutPlan(t.Context(), tx, write, daemon, at)
		return err
	}); err != nil {
		t.Fatalf("write version %d of the plan of run %d of %s: %v", write.Document.Version, *write.Generation, write.Issue, err)
	}
}

// planTuples is how many tuples the issue's plan nodes hold, every value tuple and relation from a
// node of kind plan that is part of the issue, read with a statement of this test's own.
func planTuples(t *testing.T, c *consumer, issue string) int64 {
	t.Helper()
	var count int64
	if err := c.database.Pool.QueryRow(t.Context(), `
		select count(*) from workflow_tuples
		where from_kind = 'plan' and from_id in (
			select from_id from workflow_tuples where to_kind = 'issue' and to_id = $1 and kind = 'part_of')
	`, issue).Scan(&count); err != nil {
		t.Fatalf("count the tuples of the plans of %s: %v", issue, err)
	}
	return count
}

// TestAPlanWriteRecomputesItsIssuesRowAndALostSignalIsFoundLater: a plan write moves the stored
// row of its issue, with no event and no observation of it, and a plan write whose signal no
// listener heard is found when the consumer starts again (check k36).
//
// The issue is one the sampler never saw, closed six hours after its implementer's first event,
// with that implementer's `implement` handoff at hour 2 and a `needs_review` at hour 4. With no
// plan it reads the default steps: the handoff starts testing, and the status names reviewing. Its
// first plan, of the steps `implementer reviewer acceptance:owner`, is written at hour 3 as the
// daemon writes it (workflowstore.PutPlan), and no event follows: the handoff, which falls before
// every plan, reads that first run's plan and starts reviewing. The consumer's listener hears the
// write's signal, and its next pass computes the issue again, so its row's hours by phase follow
// the plan, and its plan_tuples counts the plan's tuples.
//
// The listener then goes down, and a second run's plan, of the steps `implementer
// acceptance:owner`, is written at hour 3.5: `needs_review` at hour 4 reads it and names
// accepting. Its signal reaches no one. When the consumer starts again its first pass compares
// each stored row's plan_tuples with the count now, finds the issue, and computes it, so the row
// follows the second plan. Version 2 of the second plan's document, written at hour 3.75 with the
// same steps, moves no hour and still raises the count: the pass its signal wakes writes the row's
// count alone. A pass after that finds no issue whose count differs, and writes nothing. Each
// stored row is compared with the computation of the issue at that moment, and the run logs the
// row's hours by phase before and after each write.
func TestAPlanWriteRecomputesItsIssuesRowAndALostSignalIsFoundLater(t *testing.T) {
	t.Parallel()
	run := newConsumer(t, 1)
	project := run.fixture.Project
	issue := project + "-900"
	start := run.at.Add(-24 * time.Hour)
	hour := func(hours float64) time.Time { return start.Add(time.Duration(hours * float64(time.Hour))) }
	run.exec(t, "write the issue "+issue, `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at)
		values ($1, $2, 900, 'An issue its plans order', 'done', '00000900', '{"kind":"system","id":"test"}', $3, $4)
	`, issue, project, hour(-1), hour(6))
	run.writeEvent(t, issue, "implementer", "comment.created", `{}`, hour(0))
	run.exec(t, "write the implement handoff of "+issue, `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, 'implement', 1, '{}', $2::jsonb, $3)
	`, issue, legiontest.SessionActor("implementer", issue), hour(2))
	run.writeEventAs(t, issue, `{"kind":"user","id":"alice"}`, "issue.updated", `{"status":"needs_review"}`, hour(4))

	// signalled requires the feed to hold the issue's key once the plan write that came before it
	// committed, and the loop's wake to be set.
	signalled := func(plans *PlanSignals, when string) {
		t.Helper()
		if held := deliveredPlans(t, run, plans); !held[issue] {
			t.Errorf("%s, the plan feed holds %v and not %s: the write signalled nothing the listener heard", when, slices.Sorted(maps.Keys(held)), issue)
		}
		select {
		case <-plans.wakes():
		default:
			t.Errorf("%s, the loop's wake is empty", when)
		}
	}

	// stored is the issue's stored row, which it requires to equal the computation of the issue at
	// the consumer's clock, every field of it, and to count the tuples its plans hold now.
	stored := func(when string) Row {
		t.Helper()
		row, has := run.rows(t)[issue]
		if !has {
			t.Fatalf("%s, %s has no stored row", when, issue)
		}
		computed, _, err := computeKeys(t.Context(), run.database.Pool, project, []string{issue}, nil, run.at)
		if err != nil || len(computed) != 1 {
			t.Fatalf("%s, compute %s: %d rows, %v", when, issue, len(computed), err)
		}
		want := computed[0]
		want.UpdatedAt = row.UpdatedAt
		if difference := rowDifference(row, want); difference != "" {
			t.Errorf("%s, %s's stored row is not the computation of its inputs now: %s", when, issue, difference)
		}
		if count := planTuples(t, run, issue); row.PlanTuples != count {
			t.Errorf("%s, %s's stored row counts %d tuples of its plans, and its plans hold %d", when, issue, row.PlanTuples, count)
		}
		return row
	}
	// recomputed requires a pass to have changed the issue's row, and its hours by phase to have
	// moved from before's where moves says so and to stand where they stood otherwise.
	recomputed := func(when string, changed []string, before, after Row, moves bool) {
		t.Helper()
		if !slices.Contains(changed, issue) {
			t.Errorf("%s, the pass changed %v, not %s's row", when, changed, issue)
		}
		if moved := !maps.Equal(after.PhaseHours, before.PhaseHours); moved != moves {
			t.Errorf("%s, %s's hours by phase moved from %v to %v: %t, want %t", when, issue, before.PhaseHours, after.PhaseHours, moved, moves)
		}
	}

	plans, stop := planFeed(t, run)
	run.deps.Plans = plans
	run.pass(t)
	if owesCompare(plans) {
		t.Fatal("the first pass left the compare the feed owes at the consumer's start, so the pass after the plan write would compare as well")
	}
	select {
	case <-plans.wakes():
	default:
	}
	defaults := stored("before the first plan write")
	if defaults.PlanTuples != 0 {
		t.Fatalf("%s's row counts %d tuples of its plans before any plan was written", issue, defaults.PlanTuples)
	}
	t.Logf("before the first plan write, %s's row reads the default steps: %v (plan_tuples %d)", issue, defaults.PhaseHours, defaults.PlanTuples)

	writePlan(t, run, ownerPlan(run, issue, 1, "implementer", "reviewer", "acceptance:owner"), hour(3))
	signalled(plans, "after the first plan write")
	run.at = run.at.Add(time.Minute)
	changed := run.pass(t)
	planned := stored("after the first plan write")
	recomputed("after the first plan write", changed, defaults, planned, true)
	t.Logf("after the first plan write, the pass changed %v, and %s's row reads %v (plan_tuples %d)", changed, issue, planned.PhaseHours, planned.PlanTuples)

	stop()
	second := ownerPlan(run, issue, 2, "implementer", "acceptance:owner")
	writePlan(t, run, second, hour(3.5))
	if missed := planTuples(t, run, issue); missed == planned.PlanTuples {
		t.Fatalf("the second plan write left %s's plans at %d tuples, the count its row holds", issue, missed)
	}
	t.Logf("with the listener down, the second plan write leaves %s's row reading %v (plan_tuples %d, its plans hold %d)",
		issue, planned.PhaseHours, planned.PlanTuples, planTuples(t, run, issue))

	run.at = run.at.Add(time.Minute)
	run.restart()
	plans, _ = planFeed(t, run)
	run.deps.Plans = plans
	changed = run.pass(t)
	found := stored("after the consumer started again")
	recomputed("after the consumer started again", changed, planned, found, true)
	t.Logf("after the consumer started again, its first pass changed %v, and %s's row reads %v (plan_tuples %d)", changed, issue, found.PhaseHours, found.PlanTuples)

	select {
	case <-plans.wakes():
	default:
	}
	revised := second
	revised.Document.Version = 2
	writePlan(t, run, revised, hour(3.75))
	signalled(plans, "after version 2 of the second plan")
	run.at = run.at.Add(time.Minute)
	changed = run.pass(t)
	counted := stored("after version 2 of the second plan")
	recomputed("after version 2 of the second plan, which moves no hour", changed, found, counted, false)
	t.Logf("after version 2 of the second plan, the pass changed %v, and %s's row reads %v (plan_tuples %d)", changed, issue, counted.PhaseHours, counted.PlanTuples)

	moved, err := plansMoved(t.Context(), run.database.Pool, project)
	if err != nil {
		t.Fatalf("compare the stored counts of %s: %v", project, err)
	}
	if len(moved) != 0 {
		t.Errorf("the stored rows %v count other tuples of their plans than their plans hold", moved)
	}
	plans.connected()
	run.at = run.at.Add(time.Minute)
	changed = run.pass(t)
	if len(changed) != 0 {
		t.Errorf("a pass that compared every stored count changed %v, want no row", changed)
	}
	t.Logf("the compare finds the stored rows %v of %s whose count differs, and a pass that compares changes %v", moved, project, changed)
}
