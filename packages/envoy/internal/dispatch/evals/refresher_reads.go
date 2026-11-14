package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// refresher_reads.go holds the refresher's reads of where each item a change names stands, and its
// two writes of zero rows. Each is one statement.

// readProjects is every project's key, in key order.
func (s *Store) readProjects(ctx context.Context) ([]string, error) {
	rows, err := s.database.Pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	return keys, nil
}

// eventPage is what one page of the event log names: where the events cursor now stands, whether
// the page was full, the projects whose count of unowned P0 issues an event can have moved, and
// the issues whose repository or parent an event moved.
type eventPage struct {
	position   int64
	full       bool
	unowned    []string
	reparented []string
}

// refresherEventTypes is every event type that can move an issue's priority, status, route, claim,
// repository or parent: the issue writes (api's issue create, update and close, whose payload's
// `changed` names the fields the write moved, issueChangedFields) and the claim's two.
var refresherEventTypes = []string{"issue.created", "issue.updated", "issue.closed", "issue.claimed", "issue.released"}

// readRefresherEvents is the page of the event log after the cursor: up to limit events of
// refresherEventTypes above after and at or below safe, oldest first. The page stops at safe
// because events.id is not commit order, so a cursor moved past an event still committing would
// never read it (events, the "Safe horizon" section); a page that came back short read every such
// event up to safe, and the cursor moves to safe.
//
// An event moves the count of open P0 issues with no owner (delivery.ListUnownedP0's rule) where
// it can move one of its terms - the issue's creation or close, a claim or a release, or a write
// whose `changed` names its priority, status or route - and the issue is a P0 now or its priority
// moved, which may have made it one or taken it off: the project's row of today is then written
// again. An event whose `changed` names the repository or the parent moves the repository every
// issue below it resolves to.
func (s *Store) readRefresherEvents(ctx context.Context, after, safe int64, limit int) (eventPage, error) {
	page := eventPage{position: after}
	if safe <= after {
		return page, nil
	}
	rows, err := s.database.Pool.Query(ctx, `
		select event.id, issue.project_key,
		       (event.type in ('issue.created', 'issue.closed', 'issue.claimed', 'issue.released')
		          or coalesce(event.payload -> 'changed' ?| array['priority', 'status', 'route'], false))
		         and (coalesce(issue.priority = 0, false) or coalesce(event.payload -> 'changed' ? 'priority', false)),
		       coalesce(event.payload -> 'changed' ?| array['repo', 'parent'], false),
		       issue.key
		from events event
		join issues issue on issue.key = event.issue_key
		where event.id > $1 and event.id <= $2 and event.type = any($3)
		order by event.id
		limit $4
	`, after, safe, refresherEventTypes, limit)
	if err != nil {
		return eventPage{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	unowned, reparented := map[string]bool{}, map[string]bool{}
	count := 0
	var id int64
	var project, issue string
	var p0, moved bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &project, &p0, &moved, &issue}, func() error {
		count++
		page.position = id
		if p0 {
			unowned[project] = true
		}
		if moved {
			reparented[issue] = true
		}
		return nil
	}); err != nil {
		return eventPage{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	page.full = count == limit
	if !page.full {
		page.position = safe
	}
	page.unowned, page.reparented = slices.Sorted(maps.Keys(unowned)), slices.Sorted(maps.Keys(reparented))
	return page, nil
}

// deliveryItemDays is each pull request and run where its row stands now: the UTC day of the pull
// request's merge and of the run's start, by primary key, nil for a pull request not merged and
// for a row no longer stored.
func (s *Store) deliveryItemDays(ctx context.Context, prs map[string]pullRequestRef, runs map[string]runRef) ([]deliveryItem, error) {
	at := map[string]*time.Time{}
	if len(prs) > 0 {
		members, repos, numbers := []string{}, []string{}, []int32{}
		for _, member := range slices.Sorted(maps.Keys(prs)) {
			members, repos, numbers = append(members, member), append(repos, prs[member].repo), append(numbers, int32(prs[member].number))
		}
		rows, err := s.database.Pool.Query(ctx, `
			select given.member, request.merged_at
			from unnest($1::text[], $2::text[], $3::int[]) as given(member, repo, number)
			join delivery_pull_requests request on request.repo = given.repo and request.number = given.number
			where request.merged_at is not null
		`, members, repos, numbers)
		if err != nil {
			return nil, fmt.Errorf("read the merges of %d changed pull requests: %w", len(members), err)
		}
		if err := collectDays(rows, at); err != nil {
			return nil, fmt.Errorf("read the merges of %d changed pull requests: %w", len(members), err)
		}
	}
	if len(runs) > 0 {
		members, repos, ids := []string{}, []string{}, []int64{}
		for _, member := range slices.Sorted(maps.Keys(runs)) {
			members, repos, ids = append(members, member), append(repos, runs[member].repo), append(ids, runs[member].id)
		}
		rows, err := s.database.Pool.Query(ctx, `
			select given.member, run.started_at
			from unnest($1::text[], $2::text[], $3::bigint[]) as given(member, repo, run_id)
			join delivery_runs run on run.repo = given.repo and run.run_id = given.run_id
		`, members, repos, ids)
		if err != nil {
			return nil, fmt.Errorf("read the starts of %d changed runs: %w", len(members), err)
		}
		if err := collectDays(rows, at); err != nil {
			return nil, fmt.Errorf("read the starts of %d changed runs: %w", len(members), err)
		}
	}
	members := slices.Concat(slices.Collect(maps.Keys(prs)), slices.Collect(maps.Keys(runs)))
	slices.Sort(members)
	items := make([]deliveryItem, len(members))
	for i, member := range members {
		items[i] = deliveryItem{member: member, day: at[member]}
	}
	return items, nil
}

// collectDays reads rows of (member, time) into at, each time as its UTC midnight.
func collectDays(rows pgx.Rows, at map[string]*time.Time) error {
	var member string
	var moment time.Time
	_, err := pgx.ForEachRow(rows, []any{&member, &moment}, func() error {
		day := utcMidnight(moment)
		at[member] = &day
		return nil
	})
	return err
}

// heldMember is where eval_day_members holds an item: the project and the day it was last counted
// on.
type heldMember struct {
	project   string
	member    string
	day       time.Time
	shippedBy string
}

// heldMembers is where each of members was last counted in each of projects, under families, with
// the deploy run that shipped it then: the primary key's range of each project, family and key.
func (s *Store) heldMembers(ctx context.Context, projects, families, members []string) ([]heldMember, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select project_key, row_key, day, shipped_by from eval_day_members
		where project_key = any($1) and family = any($2) and row_key = any($3)
		order by project_key, row_key, day
	`, projects, families, members)
	if err != nil {
		return nil, fmt.Errorf("read where %d items were counted: %w", len(members), err)
	}
	return collectHeld(rows, fmt.Sprintf("where %d items were counted", len(members)))
}

// shippedMembersQuery is shippedMembers' statement. It carries eval_day_members_shipped's predicate
// (migration 20261113082154: shipped_by is not the empty string), which `shipped_by = any($2)`
// implies only for the values it is executed with: under the generic plan pgx's cached statement
// settles on, which knows no value of $2, the planner proves the partial index from that clause
// alone, as delivery/unowned.go's statements carry issues_open_p0's.
const shippedMembersQuery = `
	select project_key, row_key, day, shipped_by from eval_day_members
	where project_key = any($1) and shipped_by = any($2) and shipped_by <> '' and family = $3
	order by project_key, row_key, day
`

// shippedMembers is where each pull request that one of runs shipped when it was last counted was
// counted, in each of projects (eval_day_members_shipped): the merge days whose rows hold a run's
// completion as their deploy time.
func (s *Store) shippedMembers(ctx context.Context, projects, runs []string) ([]heldMember, error) {
	rows, err := s.database.Pool.Query(ctx, shippedMembersQuery, projects, runs, KindDelivery)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests %d runs shipped: %w", len(runs), err)
	}
	return collectHeld(rows, fmt.Sprintf("the pull requests %d runs shipped", len(runs)))
}

// collectHeld reads rows of (project, row key, day, shipped by).
func collectHeld(rows pgx.Rows, what string) ([]heldMember, error) {
	var held []heldMember
	var one heldMember
	if _, err := pgx.ForEachRow(rows, []any{&one.project, &one.member, &one.day, &one.shippedBy}, func() error {
		one.day = utcMidnight(one.day)
		held = append(held, one)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return held, nil
}

// projectDay is one project's day.
type projectDay struct {
	project string
	day     time.Time
}

// legionItemDays is, for each (project, issue key) whose stored facts changed, the UTC day the
// issue closed on as its row stands now and the day eval_day_members holds it under: both
// primary-key reads, and neither where the issue is open and was never counted.
func (s *Store) legionItemDays(ctx context.Context, issues [][2]string) ([]projectDay, error) {
	projects, keys := make([]string, len(issues)), make([]string, len(issues))
	for i, issue := range issues {
		projects[i], keys[i] = issue[0], issue[1]
	}
	rows, err := s.database.Pool.Query(ctx, `
		select given.project, facts.closed_at, member.day
		from unnest($1::text[], $2::text[]) as given(project, issue)
		left join issue_facts facts on facts.project_key = given.project and facts.issue_key = given.issue
		left join eval_day_members member on member.project_key = given.project and member.family = $3
			and member.row_key = given.issue
	`, projects, keys, KindLegion)
	if err != nil {
		return nil, fmt.Errorf("read the close days of %d issues: %w", len(issues), err)
	}
	var days []projectDay
	var project string
	var closed, held *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&project, &closed, &held}, func() error {
		for _, at := range []*time.Time{closed, held} {
			if at != nil {
				days = append(days, projectDay{project: project, day: utcMidnight(*at)})
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the close days of %d issues: %w", len(issues), err)
	}
	return days, nil
}

// subtreeFilingDays is the project and the UTC day of every filing of each of keys and of every
// issue below them, down the parent links (issues_parent_key) to refs.IssueDepthCap: the filings
// whose repository a move of those issues' repository or parent can move.
func (s *Store) subtreeFilingDays(ctx context.Context, keys []string) ([]projectDay, error) {
	rows, err := s.database.Pool.Query(ctx, `
		with recursive below as (
			select key, 0 as depth from issues where key = any($1)
			union all
			select child.key, below.depth + 1 from issues child join below on child.parent_key = below.key
			where below.depth < $2
		)
		select distinct filing.project_key, filing.filed_at
		from issue_filings filing
		where filing.issue_key in (select key from below)
	`, keys, refs.IssueDepthCap)
	if err != nil {
		return nil, fmt.Errorf("read the filings below %d issues: %w", len(keys), err)
	}
	var days []projectDay
	var project string
	var filed time.Time
	if _, err := pgx.ForEachRow(rows, []any{&project, &filed}, func() error {
		days = append(days, projectDay{project: project, day: utcMidnight(filed)})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the filings below %d issues: %w", len(keys), err)
	}
	return days, nil
}

// firstDays is the first day of project's all-repositories rows of each eval rows names, by eval
// id: the earliest over the series rows names of the eval (a zero day's rows, which name each
// series an eval's builder writes on every day it has input), each the first entry of one range
// of the primary key. An eval with no such row has no entry.
func (s *Store) firstDays(ctx context.Context, project string, rows []DayRow) (map[string]time.Time, error) {
	first := map[string]time.Time{}
	if len(rows) == 0 {
		return first, nil
	}
	evals, series := make([]string, len(rows)), make([]string, len(rows))
	for i, row := range rows {
		evals[i], series[i] = row.EvalID, row.Series
	}
	read, err := s.database.Pool.Query(ctx, `
		select given.eval_id, min(stored.day)
		from unnest($2::text[], $3::text[]) as given(eval_id, series)
		cross join lateral (
			select day from eval_days
			where project_key = $1 and eval_id = given.eval_id and series = given.series and repo = ''
			order by day
			limit 1
		) stored
		group by given.eval_id
	`, project, evals, series)
	if err != nil {
		return nil, fmt.Errorf("read the first days of %s's series: %w", project, err)
	}
	var id string
	var day time.Time
	if _, err := pgx.ForEachRow(read, []any{&id, &day}, func() error {
		first[id] = utcMidnight(day)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the first days of %s's series: %w", project, err)
	}
	return first, nil
}

// deliveryInputStart is the earliest moment a delivery day can hold input: the first merge of a
// pull request (delivery_pull_requests_merged_at) and the first start of a deploy or PR-checks run
// of the deploy repository (delivery_runs_kind_started_at), nil where neither exists.
func (s *Store) deliveryInputStart(ctx context.Context, deployRepo string) (*time.Time, error) {
	var earliest *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select least(
			(select min(merged_at) from delivery_pull_requests where merged_at is not null),
			(select min(started_at) from delivery_runs where repo = $1 and kind = $2),
			(select min(started_at) from delivery_runs where repo = $1 and kind = $3))
	`, deployRepo, string(delivery.DeliveryRunKindDeploy), string(delivery.DeliveryRunKindPRChecks)).Scan(&earliest); err != nil {
		return nil, fmt.Errorf("read the first delivery input: %w", err)
	}
	return earliest, nil
}

// legionInputStart is the earliest moment project's Legion days can hold input: its first close of
// an issue with Legion phase work (issue_facts_closed) and its first filing (issue_filings_day),
// nil where neither exists.
func (s *Store) legionInputStart(ctx context.Context, project string) (*time.Time, error) {
	var earliest *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select least(
			(select min(closed_at) from issue_facts where project_key = $1),
			(select min(filed_at) from issue_filings where project_key = $1))
	`, project).Scan(&earliest); err != nil {
		return nil, fmt.Errorf("read the first Legion input of %s: %w", project, err)
	}
	return earliest, nil
}

// insertZeroRows writes each of rows that no stored row has the key of (insertMissingDays), in one
// transaction.
func (s *Store) insertZeroRows(ctx context.Context, rows []DayRow, builtBy string) error {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the zero rows of %s: %w", rows[0].ProjectKey, err)
	}
	defer tx.Rollback(ctx)
	if err := insertMissingDays(ctx, tx, rows, builtBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the zero rows of %s: %w", rows[0].ProjectKey, err)
	}
	return nil
}

// fillLoggedZeros writes, for each series of project's logged evals (every one where ids is nil,
// else those of ids that are logged), the zero row of every day from the series' first day to
// through that holds no all-repositories row of it, in one statement, and answers how many it
// wrote. A series' first day is its first stored row's, which a reading's write made (T4's
// increment, days.go), and the zero row is the row that write leaves where every value is taken
// away: n 0, a zero sum, numerator and denominator, no sample, no maximum and no last value, so a
// later reading of that day adds to it as to any row. A day that holds a row keeps it.
func (s *Store) fillLoggedZeros(ctx context.Context, project string, ids []string, through time.Time, builtBy string) (int64, error) {
	rows, err := s.database.Pool.Query(ctx, `select eval_id from evals_current where project_key = $1 and source = $2 order by eval_id`,
		project, SourceLogged)
	if err != nil {
		return 0, fmt.Errorf("read the logged evals of %s: %w", project, err)
	}
	logged, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read the logged evals of %s: %w", project, err)
	}
	if ids != nil {
		logged = slices.DeleteFunc(logged, func(id string) bool { return !slices.Contains(ids, id) })
	}
	if len(logged) == 0 {
		return 0, nil
	}
	tag, err := s.database.Pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, num, den, samples, built_by)
		select first.project_key, first.eval_id, first.series, '', day::date, first.listed, 0, 0, 0, 0, '{}', $4
		from (
			select project_key, eval_id, series, min(day) as first_day,
			       (array_agg(listed order by day desc))[1] as listed
			from eval_days
			where project_key = $1 and eval_id = any($2) and repo = ''
			group by project_key, eval_id, series
		) first
		cross join generate_series(first.first_day::timestamp, $3::date::timestamp, interval '1 day') as day
		order by first.eval_id, first.series, day
		on conflict (project_key, eval_id, series, repo, day) do nothing
	`, project, logged, through, builtBy)
	if err != nil {
		return 0, fmt.Errorf("write the zero rows of %s's logged evals: %w", project, err)
	}
	return tag.RowsAffected(), nil
}
