package workflowstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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

	slices.SortFunc(steps, func(left, right StepRead) int {
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
	})
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
