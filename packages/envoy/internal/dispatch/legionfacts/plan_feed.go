package legionfacts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// PlanSignals is the plan feed of one process (Deps.Plans): the issues whose plans a write
// signalled since the last pass took them, and whether the next pass owes the compare of every
// stored row's plan_tuples with the count of its plans' tuples now. A plan is an input of a row,
// since the computation orders an issue's phases by its plans (metrics' Issue.phaseLine), and its
// write (workflowstore.PutPlan) appends no event, so none of the three cursor feeds names it. The
// write signals its issue's key on workflowstore.PlansChannel at its commit; the process's listener
// hub hands the key to signalled, which adds it to the set and wakes the loop; and the next pass
// takes the set (take) and computes each issue as it computes an event's issue, with the issues
// above and below it (rowsToCompute).
//
// Postgres sends a notification only to a connection that listens, so a plan write that commits
// while the hub has no connection reaches nobody. A new PlanSignals therefore owes the compare,
// which is the consumer's start, and so does each connection of the hub's listener (connected,
// pglisten.Hub.OnConnected): the pass then reads, for each project it computes narrowly, the
// stored rows whose plan_tuples differs from the count now (plansMoved), and computes each. A
// plan's tuples are never deleted or rewritten and every write that moves a plan appends one
// (workflowstore.PlanTuplesOf), so the count tells a row that read every plan write of its issue
// from one that missed a write, whichever order two writes committed in, where a cursor over the
// tuples' sequence would pass a tuple whose writer commits later.
//
// signalled and connected run on the hub's goroutine, which must not wait, and take the mutex
// alone. A nil PlanSignals is no plan feed: it holds no key, owes no compare and never wakes the
// loop, which is how a rig that calls a pass itself runs it.
type PlanSignals struct {
	mu      sync.Mutex
	keys    map[string]bool
	compare bool
	wake    chan struct{}
}

// ListenForPlans registers the plan feed on hub, the process's listener hub, and answers the
// PlanSignals that Deps.Plans takes: hub calls signalled with the key of each plan write on
// workflowstore.PlansChannel, and connected after each LISTEN that succeeds. hub must not be
// running yet (pglisten.Hub takes no registration once Run starts).
func ListenForPlans(hub *pglisten.Hub) *PlanSignals {
	plans := &PlanSignals{keys: map[string]bool{}, compare: true, wake: make(chan struct{}, 1)}
	hub.Handle(workflowstore.PlansChannel, plans.signalled)
	hub.OnConnected(plans.connected)
	return plans
}

// signalled adds the key a plan write signalled to the set the next pass takes, and wakes the
// loop.
func (p *PlanSignals) signalled(key string) {
	p.mu.Lock()
	p.keys[key] = true
	p.mu.Unlock()
	p.signal()
}

// connected owes the next pass the compare, since a plan write that committed while the hub had
// no connection signalled nobody, and wakes the loop.
func (p *PlanSignals) connected() {
	p.mu.Lock()
	p.compare = true
	p.mu.Unlock()
	p.signal()
}

// signal wakes the loop without waiting: a wake already buffered says the same.
func (p *PlanSignals) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// wakes is the loop's wake of the feed, nil for no feed.
func (p *PlanSignals) wakes() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.wake
}

// planWork is what one page took of the plan feed: the issues whose plans a write signalled, and
// whether the page compares every stored row's plan_tuples with its count now.
type planWork struct {
	keys    []string
	compare bool
}

// take answers what the feed holds and empties it, so a key a write signals while the page runs
// waits for the next pass. The page that carries the work no cursor feed names takes it (readPage).
func (p *PlanSignals) take() planWork {
	if p == nil {
		return planWork{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	work := planWork{keys: slices.Sorted(maps.Keys(p.keys)), compare: p.compare}
	clear(p.keys)
	p.compare = false
	return work
}

// restore gives the feed back what a page took and wrote nothing for, a page that failed or met a
// reset of the logs, so the next pass computes those issues and makes that compare.
func (p *PlanSignals) restore(work planWork) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range work.keys {
		p.keys[key] = true
	}
	p.compare = p.compare || work.compare
}

// plannedIssues is the issues the plan feed names to a page, by project, as the touches a feed's
// row makes: each key the page took with the project its issue belongs to, and, where the page
// owes the compare, each stored row of every project it computes narrowly whose plan_tuples
// differs from its count now (plansMoved). A project the page reads whole computes every row
// already, plan_tuples included, so it is not compared. A key that names no issue names nothing.
func plannedIssues(ctx context.Context, q Querier, work planWork, projects map[string]projectStanding) ([]touch, error) {
	touches := []touch{}
	if len(work.keys) > 0 {
		rows, err := q.Query(ctx, `select project_key, key from issues where key = any($1)`, work.keys)
		if err != nil {
			return nil, fmt.Errorf("read the projects of the issues whose plans moved: %w", err)
		}
		var touched touch
		if _, err := pgx.ForEachRow(rows, []any{&touched.project, &touched.issue}, func() error {
			touches = append(touches, touched)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the projects of the issues whose plans moved: %w", err)
		}
	}
	if !work.compare {
		return touches, nil
	}
	for _, project := range slices.Sorted(maps.Keys(projects)) {
		if projects[project].whole {
			continue
		}
		keys, err := plansMoved(ctx, q, project)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			touches = append(touches, touch{project: project, issue: key})
		}
	}
	return touches, nil
}

// plansMoved is the issue of every stored row of project whose plan_tuples differs from the count
// of the tuples of its plans now, in one statement (plansMovedQuery): the rows a plan write left
// behind whose signal the plan feed never heard. It reads the project's rows by the primary key
// and the tuples of each row's plans through workflowstore.PlanTuplesOf's indexes, and a pass
// makes it at the consumer's start and after each connection of its listener alone.
func plansMoved(ctx context.Context, q Querier, project string) ([]string, error) {
	rows, err := q.Query(ctx, plansMovedQuery, project)
	if err != nil {
		return nil, fmt.Errorf("compare the plans of %s's stored rows: %w", project, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("compare the plans of %s's stored rows: %w", project, err)
	}
	return keys, nil
}

// plansMovedQuery is plansMoved's read: the issue of each stored row of project $1 whose
// plan_tuples is not the count of its plans' tuples now.
var plansMovedQuery = `
	select fact.issue_key from issue_facts fact
	where fact.project_key = $1 and fact.plan_tuples <> ` + workflowstore.PlanTuplesOf("fact.issue_key")
