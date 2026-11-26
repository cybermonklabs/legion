package workflowstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// IssueRunsRevision answers the revision of one issue's runs: the count of the workflow_tuples rows
// of every run a `part_of` tuple ties to the issue, 0 for an issue Legion never ran. Nothing updates
// a tuple and nothing deletes one, and every write that moves a run appends one or more (putNode),
// so the count rises with each commit of such a write and never falls. The live view of the issue's
// Legion state carries it, and moves on RunsChannel, which the same writes signal.
//
// It is a count and not the newest seq of those tuples, which would miss a run: two writes of one
// issue's runs hold the issue's owner lock shared (api/steps.go's lockRecordedIssue), so neither
// waits for the other, and seq is a bigserial drawn before the commit, so the write that drew the
// earlier seq can commit after the one that drew the later. A read between the two commits sees
// the later seq, and the newest seq then stays where it was when the other write lands.
//
// It reads the `part_of` tuples into the issue through workflow_tuples_inbound, as IssueSteps
// does, and each run's own tuples through the two partial indexes that key a node's tuples by the
// node: workflow_tuples_values its attributes and workflow_tuples_current its relations, the
// literal to_kind of each half implying its index's predicate. A join that named no to_kind would
// imply neither predicate and read the whole table. So it costs the issue's own runs' tuples and
// nothing else.
func IssueRunsRevision(ctx context.Context, q Queryer, issue string) (int64, error) {
	rows, err := q.Query(ctx, `
		select count(*)
		from workflow_tuples t, lateral (
			select 1 from workflow_tuples v
			where v.from_kind = t.from_kind and v.from_id = t.from_id and v.to_kind = '`+KindValue+`'
			union all
			select 1 from workflow_tuples r
			where r.from_kind = t.from_kind and r.from_id = t.from_id and r.to_kind <> '`+KindValue+`'
		) tuples
		where t.to_kind = '`+KindIssue+`' and t.to_id = $1 and t.kind = '`+RelationPartOf+`'
			and t.from_kind = '`+KindStep+`' and t.asserted
	`, issue)
	if err != nil {
		return 0, fmt.Errorf("read the runs' revision of %s: %w", issue, err)
	}
	revision, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, fmt.Errorf("read the runs' revision of %s: %w", issue, err)
	}
	return revision, nil
}

// ProjectPlans answers the finished issues of project that filter keeps, an issue whose status is
// done and whose closed_at is set, newest close first, its close within window ([Since, Until),
// each nil for no bound, each rounded up to the microsecond). Each issue carries its plan (the node
// of its newest generation, nil where it has none), the rating of each retro in the order the
// retros ran, the earliest start and the latest end its runs hold (nil where none holds one), its
// count of runs of kind step that failed, and the experiment and the variant the runs of its newest
// generation name (nil where none of them names one). A finished issue Legion never ran is listed
// with none of these. Beside the issues it counts every one the window and PlannedOrRated keep,
// and every finished issue of the window.
//
// It is one statement for the whole project, whatever the number of issues, and the statement
// keeps and bounds the issues itself, so it answers the rows of the issues it lists and no others:
// the project's finished issues, those of them with a plan node or a run whose rating is not 0 when
// PlannedOrRated asks (stepFrom's reading of a rating), the newest Limit of those, each left-joined
// through the `part_of` tuples into it (workflow_tuples_inbound, whose predicate the literal
// to_kind implies) to the node rows of its runs and plans. Its one row of counts stands alone when
// it lists no issue.
func ProjectPlans(ctx context.Context, q Queryer, project string, window Window, filter PlansFilter) (PlansRead, error) {
	// A null limit, which a Limit of 0 sends, keeps every issue.
	var limit *int
	if filter.Limit > 0 {
		limit = &filter.Limit
	}
	partOf := `t.to_kind = '` + KindIssue + `' and t.kind = '` + RelationPartOf + `' and t.asserted`
	rows, err := q.Query(ctx, `
		with finished as (
			select i.key, i.title, i.closed_at
			from issues i
			where i.project_key = $1 and i.status = 'done' and i.closed_at is not null
				and ($2::timestamptz is null or i.closed_at >= $2)
				and ($3::timestamptz is null or i.closed_at < $3)
		), kept as (
			select f.key, f.title, f.closed_at
			from finished f
			where not $4::boolean or exists (
				select 1
				from workflow_tuples t join workflow_nodes n on n.kind = t.from_kind and n.id = t.from_id
				where `+partOf+` and t.to_id = f.key
					and (t.from_kind = '`+KindPlan+`'
						or (t.from_kind = '`+KindStep+`' and (n.attributes->>'`+AttributeRating+`')::int <> 0))
			)
		), listed as (
			select k.key, k.title, k.closed_at
			from kept k
			order by k.closed_at desc, k.key
			limit $5::int
		)
		select (select count(*) from kept), (select count(*) from finished),
			l.key, l.title, l.closed_at, n.kind, n.id, n.attributes, n.made_at
		from (values (1)) as answer (one)
		left join (listed l
			left join (workflow_tuples t join workflow_nodes n on n.kind = t.from_kind and n.id = t.from_id)
				on `+partOf+` and t.to_id = l.key and t.from_kind in ('`+KindStep+`', '`+KindPlan+`'))
			on true
		order by l.closed_at desc, l.key
	`, project, store.MicrosecondCeiling(window.Since), store.MicrosecondCeiling(window.Until), filter.PlannedOrRated, limit)
	if err != nil {
		return PlansRead{}, fmt.Errorf("read the plans of %s: %w", project, err)
	}
	defer rows.Close()

	// gathering is one finished issue as its rows build it: its answer, its runs and its newest
	// plan with the moment that plan's node was made, the two folded into the answer once every row
	// is read.
	type gathering struct {
		issue IssuePlan
		runs  []Step
		plan  *MadePlan
	}
	var issues []gathering
	var read PlansRead
	for rows.Next() {
		var key, title, kind, id *string
		var closed, made *time.Time
		var attributes []byte
		if err := rows.Scan(&read.Total, &read.FinishedTotal, &key, &title, &closed, &kind, &id, &attributes, &made); err != nil {
			return PlansRead{}, fmt.Errorf("read the plans of %s: %w", project, err)
		}
		if key == nil {
			continue
		}
		if len(issues) == 0 || issues[len(issues)-1].issue.Issue != *key {
			issues = append(issues, gathering{issue: IssuePlan{Issue: *key, Title: *title, ClosedAt: closed.UTC()}})
		}
		last := &issues[len(issues)-1]
		switch {
		case kind == nil:
		case *kind == KindStep:
			step, err := stepFrom(*id, attributes)
			if err != nil {
				return PlansRead{}, err
			}
			last.runs = append(last.runs, step)
		case *kind == KindPlan:
			plan, err := planFrom(*id, attributes)
			if err != nil {
				return PlansRead{}, err
			}
			if candidate := (MadePlan{Plan: plan, Made: *made}); candidate.newerThan(last.plan) {
				last.plan = &candidate
			}
		}
	}
	if err := rows.Err(); err != nil {
		return PlansRead{}, fmt.Errorf("read the plans of %s: %w", project, err)
	}

	read.Issues = make([]IssuePlan, 0, len(issues))
	for _, gathered := range issues {
		issue := gathered.issue
		if gathered.plan != nil {
			issue.Plan = &gathered.plan.Plan
		}
		slices.SortFunc(gathered.runs, compareRuns)
		// The runs come in the order of their generation, so a run of a newer generation than the
		// one read so far drops the variant an older generation named.
		var newest int64
		for _, step := range gathered.runs {
			if step.Generation > newest {
				newest, issue.Experiment = step.Generation, nil
			}
			if issue.Experiment == nil {
				issue.Experiment = step.Experiment
			}
			if step.StartedAt != nil && (issue.StartedAt == nil || step.StartedAt.Before(*issue.StartedAt)) {
				issue.StartedAt = step.StartedAt
			}
			if step.EndedAt != nil && (issue.EndedAt == nil || step.EndedAt.After(*issue.EndedAt)) {
				issue.EndedAt = step.EndedAt
			}
			if step.Kind == StepKindStep && step.Result == ResultFailed {
				issue.FailedSteps++
			}
			if step.Rating != nil {
				issue.Ratings = append(issue.Ratings, Rating{Rating: step.Rating.Rating, Reason: step.Rating.Reason, StepID: step.ID, EndedAt: step.EndedAt})
			}
		}
		read.Issues = append(read.Issues, issue)
	}
	return read, nil
}

// MadePlan is a plan with the moment its node was made, which is when the run's work started and
// which picks between two plans of one generation.
type MadePlan struct {
	Plan Plan
	Made time.Time
}

// newerThan answers whether candidate is newer than held (nil for none): of a higher generation,
// or of the same generation and made later, or made at the same moment with the greater id, so the
// read picks one plan whatever order the rows came in.
func (candidate MadePlan) newerThan(held *MadePlan) bool {
	if held == nil {
		return true
	}
	if order := cmp.Compare(candidate.Plan.Generation, held.Plan.Generation); order != 0 {
		return order > 0
	}
	if order := candidate.Made.Compare(held.Made); order != 0 {
		return order > 0
	}
	return candidate.Plan.ID > held.Plan.ID
}

// IssuesPlans answers, by issue, each version of the plan of each run of each issue of keys, with
// the moment that version's write made it, oldest first in newerThan's order. One run keeps one
// plan node, whose made_at is its first write's, and a later version replaces the node's tasks
// whole (planFact); every write of the tasks appends a value tuple of them, stamped with the
// write's moment (putNode). So each version is read from its tuple, with its document version from
// the tuple the same write appended, and the metrics' fallback timeline reads each mark by the
// version in force at that mark, not the whole run by the newest. An issue Legion never ran answers
// none. It reads the `part_of` tuples into those issues (workflow_tuples_inbound, whose predicate
// the literal to_kind implies), joins each to its node row and the node's own tasks tuples, so it
// costs their plans alone.
func IssuesPlans(ctx context.Context, q Queryer, keys []string) (map[string][]MadePlan, error) {
	rows, err := q.Query(ctx, `
		select t.to_id, n.id,
			n.attributes || coalesce(jsonb_build_object('`+AttributeTasks+`', v.to_id::jsonb), '{}'::jsonb)
				|| coalesce(jsonb_build_object('`+AttributeVersion+`', w.to_id::bigint), '{}'::jsonb),
			coalesce(v.at, n.made_at)
		from workflow_tuples t join workflow_nodes n on n.kind = t.from_kind and n.id = t.from_id
		left join workflow_tuples v on v.from_kind = n.kind and v.from_id = n.id and v.kind = '`+AttributeTasks+`'
			and v.to_kind = '`+KindValue+`' and v.asserted
		left join workflow_tuples w on w.from_kind = n.kind and w.from_id = n.id and w.kind = '`+AttributeVersion+`'
			and w.to_kind = '`+KindValue+`' and w.asserted and w.at = v.at
		where t.to_kind = '`+KindIssue+`' and t.to_id = any($1) and t.kind = '`+RelationPartOf+`'
			and t.from_kind = '`+KindPlan+`' and t.asserted
	`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the plans of %d issues: %w", len(keys), err)
	}
	defer rows.Close()
	plans := map[string][]MadePlan{}
	for rows.Next() {
		var issue, id string
		var attributes []byte
		var made time.Time
		if err := rows.Scan(&issue, &id, &attributes, &made); err != nil {
			return nil, fmt.Errorf("read the plans of %d issues: %w", len(keys), err)
		}
		plan, err := planFrom(id, attributes)
		if err != nil {
			return nil, err
		}
		plans[issue] = append(plans[issue], MadePlan{Plan: plan, Made: made.UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the plans of %d issues: %w", len(keys), err)
	}
	for _, held := range plans {
		slices.SortFunc(held, func(a, b MadePlan) int {
			switch {
			case b.newerThan(&a):
				return -1
			case a.newerThan(&b):
				return 1
			}
			return 0
		})
	}
	return plans, nil
}
