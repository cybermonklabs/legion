package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
)

// TestTheMovedStoresWriteAfterTheDropStep holds every writer of the three kinds the move of
// dispatch://INFRA-1098's R32 took into the graph store to the graph store alone: over a database
// the boot moved (storesmove.Move) and whose boot step then dropped the four views, the two
// triggers with their functions and the index graph_tuples_workflow_inbound
// (startup.StoresMovedRetirement, at a boot that created the database from empty), each writer
// writes once through its route and each write reads back - a priority's create, its rank move and
// its delete, an issue's advances (writeIssueAdvances), a run's start and end (PutStep), a plan
// (PutPlan) and a retro's rating. A writer that still wrote through one of the four names would
// fail on a relation the database no longer holds, and its route would answer 500.
func TestTheMovedStoresWriteAfterTheDropStep(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	pool := database.Pool
	result, err := storesmove.Move(ctx, pool.Config().ConnString(), storesmove.Options{})
	if err != nil || !result.Moved {
		t.Fatalf("move the stores: moved=%v, %v; want them moved", result != nil && result.Moved, err)
	}
	var logged bytes.Buffer
	startup.Retire(ctx, pool, "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0", true, slog.New(slog.NewTextHandler(&logged, nil)),
		[]startup.Retirement{startup.StoresMovedRetirement})
	t.Logf("the boot step logged:\n%s", logged.String())
	var standing []string
	if err := pool.QueryRow(ctx, `
		select array(select name from unnest(array['priority_nodes', 'priority_tuples', 'workflow_nodes', 'workflow_tuples']) name
		             where to_regclass(name) is not null)
		    || array(select tgname::text from pg_trigger where tgname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project'))
		    || array(select proname::text from pg_proc where proname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project'))
		    || array(select relname::text from pg_class where relname = 'graph_tuples_workflow_inbound')`,
	).Scan(&standing); err != nil {
		t.Fatalf("read what the drop left: %v", err)
	}
	if len(standing) != 0 {
		t.Fatalf("the boot step left %v standing, want none of the nine objects", standing)
	}

	// A priority's create, its rank move and its delete.
	createTestPriority(t, handler, map[string]any{"id": "first-goal", "title": "First goal"}, "alice")
	createTestPriority(t, handler, map[string]any{"id": "second-goal", "title": "Second goal"}, "alice")
	createTestPriority(t, handler, map[string]any{"id": "dropped-goal", "title": "Dropped goal"}, "alice")
	patchTestPriority(t, handler, "first-goal", map[string]any{"rank": map[string]any{"after": "second-goal"}}, "alice")
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/dropped-goal", nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete dropped-goal: status=%d body=%s, want 204", deleted.Code, deleted.Body.String())
	}
	var listed []string
	for _, row := range listTestPriorities(t, handler) {
		listed = append(listed, row.ID)
	}
	if !slices.Equal(listed, []string{"second-goal", "first-goal"}) {
		t.Errorf("GET /api/v1/priorities lists %v after the create, the rank move and the delete, want [second-goal first-goal]", listed)
	}

	// An issue's advances.
	createTestProject(t, handler, "DROP")
	issue := createAdvancingIssue(t, handler, map[string]any{"project": "DROP", "title": "Advances a goal", "force": true}, "alice")
	advanced := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"advances": []string{"first-goal"}}, "alice")
	if advanced.Code != http.StatusOK {
		t.Fatalf("the advances of %s: status=%d body=%s, want 200", issue.Key, advanced.Code, advanced.Body.String())
	}
	if got := advanceEntries(readTestIssue(t, handler, issue.Key).Advances); !slices.Equal(got, []string{"first-goal own"}) {
		t.Errorf("%s advances %v, want first-goal alone", issue.Key, got)
	}

	// A run's start and end, a plan and a retro's rating.
	plan := uuid.NewString()
	putPlanRecordOK(t, handler, issue.Key, plan, defaultPlanBody(daemonSessionOf("DROP"), 1))
	started, ended := at(0), at(5)
	runs := recordRuns(t, handler, "DROP", issue.Key,
		run{kind: "step", role: "planner", place: 1, started: &started, ended: &ended, result: "passed"},
		run{kind: "retro", role: "implementer", place: 0, ended: &ended, result: "passed"})
	rateRetro(t, handler, "DROP", issue.Key, runs[1], 1, 4, "It held.")
	read := map[string]string{}
	for _, task := range readSteps(t, handler, issue.Key).Tasks {
		for _, step := range task.Steps {
			rating := "unrated"
			if step.Rating != nil {
				rating = "rated"
			}
			read[step.ID] = step.Kind + " " + step.Result + " " + rating
		}
	}
	if read[runs[0]] != "step passed unrated" || read[runs[1]] != "retro passed rated" {
		t.Errorf("GET /api/v1/issues/%s/steps answers %v, want the planner's run passed and the retro's passed and rated", issue.Key, read)
	}
	var planSource string
	if err := pool.QueryRow(ctx, `select attributes->>'source' from graph_nodes where kind = 'plan' and id = $1`, plan).Scan(&planSource); err != nil || planSource != "default" {
		t.Errorf("the plan %s reads back source %q from graph_nodes (%v), want default", plan, planSource, err)
	}
	if strings.Contains(logged.String(), "failed") {
		t.Errorf("the boot step logged a failure:\n%s", logged.String())
	}
}
