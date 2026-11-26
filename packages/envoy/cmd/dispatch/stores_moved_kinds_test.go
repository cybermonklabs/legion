package main

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// movedKinds are the three kinds dispatch://INFRA-1098's R32 moved into the graph store.
var movedKinds = []string{prioritystore.KindPriority, workflowstore.KindStep, workflowstore.KindPlan}

// movedKindsActor is who the writes below write as.
var movedKindsActor = model.Actor{Kind: "user", ID: "alice", Proven: true}

// writeEveryMovedShape writes, through prioritystore and workflowstore in one transaction, one node
// of every shape each moved kind's writer has: a priority created with every attribute and moved
// in rank; a run of a step started and ended failed with every field a run of kind step holds, a
// retro ended and rated, and a merge; and a default plan and an owner's plan.
func writeEveryMovedShape(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	storesmovetest.Issue(t, pool, "KIND", "KIND-1", 1)
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	text := func(value string) *string { return &value }
	number := func(value int) *int { return &value }
	generation := int64(1)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for index, id := range []string{"every-attribute", "second-goal"} {
			if err := prioritystore.Create(ctx, tx, id, map[string]string{
				prioritystore.AttributeTitle: "Goal " + id, prioritystore.AttributeDescription: "All of it",
				prioritystore.AttributeOwner: "alice", prioritystore.AttributeRank: "a" + string(rune('0'+index)),
			}, movedKindsActor, at); err != nil {
				return err
			}
		}
		if _, err := prioritystore.Move(ctx, tx, "every-attribute", "", "second-goal", movedKindsActor, at.Add(time.Minute)); err != nil {
			return err
		}
		identity := func(id, kind, role string, place int) workflowstore.StepWrite {
			return workflowstore.StepWrite{ID: id, Issue: "KIND-1", Project: text("KIND"), Task: text("default"),
				Kind: text(kind), Role: text(role), Generation: &generation, Place: number(place)}
		}
		step := identity("0f1e2d3c-0000-4000-8000-0000000000d1", workflowstore.StepKindStep, "implementer", 2)
		step.Type, step.Agent, step.Session, step.Model = text("backend"), text("an-agent"), text("a-session"), text("a-model")
		step.StartedAt = &at
		if _, err := workflowstore.PutStep(ctx, tx, step, movedKindsActor, at); err != nil {
			return err
		}
		ended := at.Add(10 * time.Minute)
		end := identity(step.ID, workflowstore.StepKindStep, "implementer", 2)
		end.Result, end.EndedAt, end.ReturnedTo = text(workflowstore.ResultFailed), &ended, number(1)
		end.Report = text("0f1e2d3c-0000-4000-8000-0000000000e1")
		if _, err := workflowstore.PutStep(ctx, tx, end, movedKindsActor, ended); err != nil {
			return err
		}
		retro := identity("0f1e2d3c-0000-4000-8000-0000000000d2", "retro", "implementer", 0)
		retro.Result, retro.EndedAt = text(workflowstore.ResultPassed), &ended
		retro.Rating = &workflowstore.StepRating{Rating: 4, Reason: "It held."}
		if _, err := workflowstore.PutStep(ctx, tx, retro, movedKindsActor, ended); err != nil {
			return err
		}
		merge := identity("0f1e2d3c-0000-4000-8000-0000000000d3", "merge", "merger", 0)
		merge.StartedAt = &ended
		if _, err := workflowstore.PutStep(ctx, tx, merge, movedKindsActor, ended); err != nil {
			return err
		}
		tasks := []workflowstore.PlanTask{{ID: "default", Steps: []string{"planner", "implementer"}}}
		if _, err := workflowstore.PutPlan(ctx, tx, workflowstore.PlanWrite{ID: "0f1e2d3c-0000-4000-8000-0000000000f1", Issue: "KIND-1",
			Project: text("KIND"), Source: text(workflowstore.PlanSourceDefault), Generation: &generation, Tasks: tasks}, movedKindsActor, at); err != nil {
			return err
		}
		second := int64(2)
		_, err := workflowstore.PutPlan(ctx, tx, workflowstore.PlanWrite{ID: "0f1e2d3c-0000-4000-8000-0000000000f2", Issue: "KIND-1",
			Project: text("KIND"), Source: text(workflowstore.PlanSourceOwner), Generation: &second,
			Document: workflowstore.PlanDocument{ArtifactID: "0f1e2d3c-0000-4000-8000-0000000000a9", Version: 3}, Tasks: tasks}, movedKindsActor, at)
		return err
	}); err != nil {
		t.Fatalf("write every shape of the moved kinds: %v", err)
	}
}

// TestTheMovedKindsAttributeNodesMatchTheirWriters holds the attribute nodes that
// 20261126000300_stores_moved declares under the supertags priority, step and plan to the writers
// of those kinds: over a migrated database it writes one node of every shape each writer has
// (writeEveryMovedShape), collects the attribute names of their value tuples less the base
// attributes, and compares, for each kind, that set with the attribute nodes whose attribute_of
// tuple names the kind's supertag, naming each one missing or extra.
func TestTheMovedKindsAttributeNodesMatchTheirWriters(t *testing.T) {
	database := storetest.Open(t)
	writeEveryMovedShape(t, database.Pool)
	for _, kind := range movedKinds {
		written := readNames(t, database.Pool, "the attributes the writers of "+kind+" state", `
			select distinct t.kind from graph_tuples t
			where t.from_kind = $1 and t.to_kind = '`+graphstore.KindValue+`'
			  and t.kind not in ('`+graphstore.AttributeVisibility+`', '`+graphstore.AttributeState+`', '`+
			graphstore.AttributeProject+`', '`+graphstore.AttributeParties+`')
			order by 1`, kind)
		declared := readNames(t, database.Pool, "the attribute nodes of the supertag "+kind, `
			select a.attributes->>'name' from graph_nodes a
			where a.kind = 'attribute'
			  and exists (select 1 from graph_tuples o
			              where o.from_kind = 'attribute' and o.from_id = a.id and o.kind = 'attribute_of'
			                and o.to_kind = 'supertag' and o.to_id = $1 and o.asserted)
			order by 1`, kind)
		t.Logf("%s: the writers state %v; the migration declares %v", kind, written, declared)
		if len(written) == 0 {
			t.Errorf("the writers of %s stated no attribute, so the comparison proves nothing", kind)
		}
		for _, name := range written {
			if !slices.Contains(declared, name) {
				t.Errorf("%s: the writer states the attribute %s, and no attribute node of the supertag %s declares it", kind, name, kind)
			}
		}
		for _, name := range declared {
			if !slices.Contains(written, name) {
				t.Errorf("%s: the attribute node %s/%s declares an attribute no writer of %s states", kind, kind, name, kind)
			}
		}
	}
}

// readNames reads one text column of query over pool.
func readNames(t *testing.T, pool *store.Pool, what, query string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	return names
}

// TestTheStoreCensusCountsTheMovedKindsAndRefusesNoZero runs `envoy-dispatch census --store` over
// three databases and reads its lines and its report for priority, step and plan
// (graphcensus.StoreCensus):
//   - a migrated database where nothing of the three kinds is written: the census prints a zero
//     count for each, says it refuses nothing for them, and leaves none of them among the kinds it
//     refuses for checking no row;
//   - one this build's writers wrote every shape of the three kinds to (writeEveryMovedShape): it
//     prints each count above zero and reads no difference of them;
//   - one whose old tables held the release before's rows and which the boot's move then moved
//     (storesmovetest.Write, storesmove.Move): it prints each count above zero and reads no
//     difference of them.
//
// The census over a database holding no row of most other kinds refuses for those, and the test
// reads nothing of them.
func TestTheStoreCensusCountsTheMovedKindsAndRefusesNoZero(t *testing.T) {
	ctx := context.Background()
	empty := storetest.Open(t)
	report := movedKindsCensus(t, empty, nil)
	for _, kind := range movedKinds {
		read := report[kind]
		if read.Nodes != 0 || read.Rows != 0 {
			t.Errorf("%s over a database with none of it: nodes=%d rows=%d, want 0 and 0", kind, read.Nodes, read.Rows)
		}
	}

	written := storetest.Open(t)
	writeEveryMovedShape(t, written.Pool)
	movedKindsCensus(t, written, map[string]int64{prioritystore.KindPriority: 2, workflowstore.KindStep: 3, workflowstore.KindPlan: 2})

	moved := storetest.Open(t)
	fixtures := storesmovetest.Write(t, moved.Pool)
	if _, err := storesmove.Move(ctx, moved.Pool.Config().ConnString(), storesmove.Options{}); err != nil {
		t.Fatalf("move the stores: %v", err)
	}
	movedKindsCensus(t, moved, map[string]int64{
		prioritystore.KindPriority: int64(len(fixtures.Priorities) - 1),
		workflowstore.KindStep:     int64(len(fixtures.Runs)), workflowstore.KindPlan: int64(len(fixtures.Plans)),
	})

	// A priority's row changed by hand, with no tuple, is a row and a node that disagree.
	if _, err := moved.Pool.Exec(ctx, `update graph_nodes set attributes = attributes || '{"title": "Changed by hand"}'
		where kind = $1 and id = $2`, prioritystore.KindPriority, fixtures.Priorities[0]); err != nil {
		t.Fatalf("change a priority's row by hand: %v", err)
	}
	planted, err := graphcensus.StoreCensus(ctx, moved.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range planted.Kinds {
		if kind.Kind == prioritystore.KindPriority {
			t.Logf("after the change by hand: %s reads %d row and %d node differences: %v", kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows)
			if kind.RowDifferences != 1 {
				t.Errorf("a priority whose row was changed with no tuple reads %d row differences, want 1", kind.RowDifferences)
			}
		}
	}
}

// movedKindsCensus runs `census --store` over database, prints its lines of the three kinds, and
// fails unless each kind refuses nothing - no difference, and no place among the kinds the census
// refuses for checking no row - and, where counts names it, counts that many rows and nodes of it.
// It answers the report's kinds by name.
func movedKindsCensus(t *testing.T, database *store.Store, counts map[string]int64) map[string]graphcensus.KindReport {
	t.Helper()
	ctx := context.Background()
	url := database.Pool.Config().ConnString()
	var out, errOut strings.Builder
	code := census(ctx, []string{"--store"}, url, &out, &errOut)
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		for _, kind := range movedKinds {
			if strings.HasPrefix(line, "census: store kind="+kind+" ") {
				lines = append(lines, line)
			}
		}
	}
	t.Logf("census --store exited %d; its lines of the moved kinds:\n%s\nstderr: %s", code, strings.Join(lines, "\n"), errOut.String())
	if code == 2 {
		t.Fatalf("census --store could not be taken: %s", errOut.String())
	}
	report, err := graphcensus.StoreCensus(ctx, url, false, nil)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	kinds := map[string]graphcensus.KindReport{}
	for _, kind := range report.Kinds {
		kinds[kind.Kind] = kind
	}
	for _, kind := range movedKinds {
		read, held := kinds[kind]
		switch {
		case !held:
			t.Errorf("the store census reads no kind %s; it reads %v", kind, slices.Sorted(maps.Keys(kinds)))
			continue
		case slices.Contains(report.Unpopulated(), kind):
			t.Errorf("the store census refuses %s for checking no row, want it to refuse no zero of it", kind)
		case read.RowDifferences+read.NodeDifferences != 0:
			t.Errorf("%s: %d rows without a node and %d nodes without a row: %v %v", kind, read.RowDifferences, read.NodeDifferences, read.ReportedRows, read.ReportedNodes)
		}
		if !slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, "census: store kind="+kind+" nodes=") && strings.Contains(line, " rows=")
		}) {
			t.Errorf("census --store printed no count line of %s", kind)
		}
		if want, named := counts[kind]; named && (read.Rows != want || read.Nodes != want) {
			t.Errorf("%s: the census counted rows=%d nodes=%d, want %d of each", kind, read.Rows, read.Nodes, want)
		}
		if read.Rows == 0 && !slices.ContainsFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, "census: store kind="+kind+" checked no row") && strings.Contains(line, "refuses nothing")
		}) {
			t.Errorf("census --store printed no line that it refuses nothing of %s at zero rows", kind)
		}
	}
	return kinds
}
