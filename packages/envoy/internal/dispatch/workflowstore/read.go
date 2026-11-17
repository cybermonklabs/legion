package workflowstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// IssueSteps answers every run of every step of one issue, each with its attempt, in the order
// the runs happened: by generation, then by start, a run whose start never arrived first, then by
// id. The attempt is the place of a run among the issue's runs of the same task, step and kind in
// that order, from 1, so the retro and the merge, both at step 0, each count from 1. An issue
// Legion never ran answers none.
//
// It reads the `part_of` tuples into the issue through workflow_tuples_inbound, whose predicate
// the literal to_kind implies, and joins each to its node row, so it costs the issue's own runs
// and nothing else.
func IssueSteps(ctx context.Context, q Queryer, issue string) ([]StepRead, error) {
	rows, err := q.Query(ctx, `
		select n.id, n.attributes
		from workflow_tuples t
		join workflow_nodes n on n.kind = t.from_kind and n.id = t.from_id
		where t.to_kind = '`+KindIssue+`' and t.to_id = $1 and t.kind = '`+RelationPartOf+`'
			and t.from_kind = '`+KindStep+`' and t.asserted
	`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the steps of %s: %w", issue, err)
	}
	defer rows.Close()
	var steps []StepRead
	for rows.Next() {
		var id string
		var attributes []byte
		if err := rows.Scan(&id, &attributes); err != nil {
			return nil, fmt.Errorf("read the steps of %s: %w", issue, err)
		}
		step, err := stepFrom(id, attributes)
		if err != nil {
			return nil, err
		}
		steps = append(steps, StepRead{Step: step})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the steps of %s: %w", issue, err)
	}

	slices.SortFunc(steps, func(left, right StepRead) int { return compareRuns(left.Step, right.Step) })
	type place struct {
		task, kind string
		step       int
	}
	attempts := map[place]int{}
	for index := range steps {
		key := place{steps[index].Task, steps[index].Kind, steps[index].Place}
		attempts[key]++
		steps[index].Attempt = attempts[key]
	}
	return steps, nil
}

// compareRuns is the order the runs of an issue happened in: by generation, then by start, a run
// whose start never arrived first, then by id.
func compareRuns(left, right Step) int {
	if order := cmp.Compare(left.Generation, right.Generation); order != 0 {
		return order
	}
	switch {
	case left.StartedAt == nil && right.StartedAt != nil:
		return -1
	case left.StartedAt != nil && right.StartedAt == nil:
		return 1
	case left.StartedAt != nil:
		if order := left.StartedAt.Compare(*right.StartedAt); order != 0 {
			return order
		}
	}
	return cmp.Compare(left.ID, right.ID)
}

// ProjectPlans answers each finished issue of project, an issue whose status is done and whose
// closed_at is set, newest close first: since and until, each nil for no bound, bound the close
// to [since, until). Each issue carries its plan (the node of its newest generation, nil where it
// has none), the rating of each retro in the order the retros ran, the earliest start and the
// latest end its runs hold (nil where none holds one), and its count of runs of kind step that
// failed. A finished issue Legion never ran is listed with none of these.
//
// It is one statement for the whole project, whatever the number of issues: the project's
// finished issues, each left-joined through the `part_of` tuples into it (workflow_tuples_inbound,
// whose predicate the literal to_kind implies) to the node rows of its runs and plans.
func ProjectPlans(ctx context.Context, q Queryer, project string, since, until *time.Time) ([]IssuePlan, error) {
	rows, err := q.Query(ctx, `
		select i.key, i.title, i.closed_at, n.kind, n.id, n.attributes, n.made_at
		from issues i
		left join (workflow_tuples t join workflow_nodes n on n.kind = t.from_kind and n.id = t.from_id)
			on t.to_kind = '`+KindIssue+`' and t.to_id = i.key and t.kind = '`+RelationPartOf+`'
			and t.from_kind in ('`+KindStep+`', '`+KindPlan+`') and t.asserted
		where i.project_key = $1 and i.status = 'done' and i.closed_at is not null
			and ($2::timestamptz is null or i.closed_at >= $2)
			and ($3::timestamptz is null or i.closed_at < $3)
		order by i.closed_at desc, i.key
	`, project, since, until)
	if err != nil {
		return nil, fmt.Errorf("read the plans of %s: %w", project, err)
	}
	defer rows.Close()

	var finished []IssuePlan
	var runs [][]Step
	var planMade []time.Time
	for rows.Next() {
		var key, title string
		var closed time.Time
		var kind, id *string
		var attributes []byte
		var made *time.Time
		if err := rows.Scan(&key, &title, &closed, &kind, &id, &attributes, &made); err != nil {
			return nil, fmt.Errorf("read the plans of %s: %w", project, err)
		}
		if len(finished) == 0 || finished[len(finished)-1].Issue != key {
			finished = append(finished, IssuePlan{Issue: key, Title: title, ClosedAt: closed.UTC(), Ratings: []Rating{}})
			runs = append(runs, nil)
			planMade = append(planMade, time.Time{})
		}
		last := len(finished) - 1
		switch {
		case kind == nil:
		case *kind == KindStep:
			step, err := stepFrom(*id, attributes)
			if err != nil {
				return nil, err
			}
			runs[last] = append(runs[last], step)
		case *kind == KindPlan:
			plan, err := planFrom(*id, attributes)
			if err != nil {
				return nil, err
			}
			if newerPlan(plan, *made, finished[last].Plan, planMade[last]) {
				finished[last].Plan, planMade[last] = &plan, *made
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the plans of %s: %w", project, err)
	}

	for index := range finished {
		issue := &finished[index]
		slices.SortFunc(runs[index], compareRuns)
		for _, step := range runs[index] {
			if step.StartedAt != nil && (issue.StartedAt == nil || step.StartedAt.Before(*issue.StartedAt)) {
				issue.StartedAt = step.StartedAt
			}
			if step.EndedAt != nil && (issue.EndedAt == nil || step.EndedAt.After(*issue.EndedAt)) {
				issue.EndedAt = step.EndedAt
			}
			if step.Kind == StepKindStep && step.Result == ResultFailed {
				issue.FailedSteps++
			}
			if step.Kind == StepKindRetro && step.Rating != 0 {
				issue.Ratings = append(issue.Ratings, Rating{Rating: step.Rating, Reason: step.RatingReason, StepID: step.ID, EndedAt: step.EndedAt})
			}
		}
	}
	return finished, nil
}

// newerPlan answers whether plan, its node made at made, is newer than held, made at heldMade: of
// a higher generation, or of the same generation and made later, or made at the same moment with
// the greater id, so the read picks one plan whatever order the rows came in.
func newerPlan(plan Plan, made time.Time, held *Plan, heldMade time.Time) bool {
	if held == nil {
		return true
	}
	if order := cmp.Compare(plan.Generation, held.Generation); order != 0 {
		return order > 0
	}
	if order := made.Compare(heldMade); order != 0 {
		return order > 0
	}
	return plan.ID > held.ID
}
