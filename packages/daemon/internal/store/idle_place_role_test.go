package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The idle view reads the running role of an issue (pods.IdleView.PlaceRole) with the subquery the
// issue's own read states (record.Issue.PlaceRole), one subquery (plansql.PlaceRole) over each
// read's alias. At every place of every plan of record.PlaceWorker's table the
// two reads give one role: the architect where the agent at the place is the owner, the step's
// role at a step, and the role of the retro's agent at the retro.
func TestTheIdleViewAndTheIssueReadOnePlaceRole(t *testing.T) {
	for _, tc := range []struct {
		steps string
		types []plan.Type
	}{
		{"implementer tester", []plan.Type{plan.TypeImplementation, plan.TypeTesting}},
		{"implementer:x reviewer implementer:y", []plan.Type{plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation}},
		{"tester", []plan.Type{plan.TypeTesting}},
		{"implementer:owner reviewer", []plan.Type{plan.TypeImplementation, plan.TypeReview}},
		{"implementer reviewer implementer:owner", []plan.Type{plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation}},
		{"coder checker", []plan.Type{plan.TypeImplementation, plan.TypeTesting}},
		{"implementer reviewer acceptance", []plan.Type{plan.TypeImplementation, plan.TypeReview, plan.TypeReview}},
	} {
		t.Run(tc.steps, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			records := record.NewStore()
			var issue *record.Issue
			if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				var err error
				issue, err = records.Issue(ctx, tx, rootIssue)
				if err != nil || issue == nil {
					return err
				}
				v := record.PlanVersion{Issue: rootIssue, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact",
					Source: record.PlanSourceOwner, GivenBy: claim.Token("legion-legion-legion-208-architect"), GivenAt: time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)}
				task := record.PlanTask{Issue: rootIssue, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
				var steps []record.PlanStep
				for i, word := range strings.Fields(tc.steps) {
					role, key, _ := strings.Cut(word, ":")
					steps = append(steps, record.PlanStep{Issue: rootIssue, Generation: issue.Generation, Task: "a", Position: i + 1, Role: role,
						AgentKey: key, Type: tc.types[i]})
				}
				return records.PutPlan(ctx, tx, v, []record.PlanTask{task}, record.NameAgents(nil, nil, nil, steps))
			}); err != nil || issue == nil {
				t.Fatalf("seed the plan %s of %s: %v (issue %v)", tc.steps, rootIssue, err, issue)
			}
			places := []record.TaskPlace{{Kind: record.PlaceRetro}}
			for i := range tc.types {
				places = append(places, record.TaskPlace{Kind: record.PlaceStep, Position: i + 1})
			}
			for _, place := range places {
				var read *record.Issue
				if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
					if err := records.MoveTask(ctx, tx, rootIssue, issue.Generation, "a", place, record.TaskRunning); err != nil {
						return err
					}
					var err error
					read, err = records.Issue(ctx, tx, rootIssue)
					return err
				}); err != nil {
					t.Fatalf("move %s's task to %+v: %v", rootIssue, place, err)
				}
				view := idleViewOf(t, st, rootIssue)
				if view.PlaceRole != read.PlaceRole || read.PlaceRole == "" {
					t.Errorf("%s at %+v: the idle view reads %q and the issue %q, want one role", tc.steps, place, view.PlaceRole, read.PlaceRole)
				}
			}
		})
	}
}
