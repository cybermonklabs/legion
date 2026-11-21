package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// refresher_reads.go holds the refresher's reads of where each item a change names stands, each
// one statement, and its fill of the logged evals' zero rows (fillLoggedZeros).

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
		day := measures.Midnight(moment)
		at[member] = &day
		return nil
	})
	return err
}

// owedDay is one project's day of a family a write left owing a rewrite (eval_days_owed).
type owedDay struct {
	project string
	family  string
	day     time.Time
}

// owedDays is every day of projects that a write left owing a rewrite and no write of the day
// has cleared (eval_days_owed, by its primary key): an item moved off it, or off the deploy run
// whose day it is, by a write of another day; those a failed pass left among them.
func (s *Store) owedDays(ctx context.Context, projects []string) ([]owedDay, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select project_key, family, day from eval_days_owed where project_key = any($1) order by project_key, family, day
	`, projects)
	if err != nil {
		return nil, fmt.Errorf("read the days owed a rewrite: %w", err)
	}
	var owed []owedDay
	var one owedDay
	if _, err := pgx.ForEachRow(rows, []any{&one.project, &one.family, &one.day}, func() error {
		one.day = measures.Midnight(one.day)
		owed = append(owed, one)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the days owed a rewrite: %w", err)
	}
	return owed, nil
}

// heldMember is where eval_day_members holds an item: the project and the day it was last counted
// on.
type heldMember struct {
	project   string
	member    string
	day       time.Time
	shippedBy string
}

// heldMembers is where each of members was last counted in each of projects, under families and
// deployRepos (the kept repositories and "", every deploy repository a member is counted under),
// with the deploy run that shipped it then: the primary key's range of each project, family,
// deploy repository and key.
func (s *Store) heldMembers(ctx context.Context, projects, families, deployRepos, members []string) ([]heldMember, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select project_key, row_key, day, shipped_by from eval_day_members
		where project_key = any($1) and family = any($2) and deploy_repo = any($3) and row_key = any($4)
		order by project_key, row_key, day
	`, projects, families, deployRepos, members)
	if err != nil {
		return nil, fmt.Errorf("read where %d items were counted: %w", len(members), err)
	}
	return collectHeld(rows, fmt.Sprintf("where %d items were counted", len(members)))
}

// shippedMembersQuery is shippedMembers' statement. It carries eval_day_members_shipped's predicate
// (migration 20261117100006: shipped_by is not the empty string), which `shipped_by = any($2)`
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
		one.day = measures.Midnight(one.day)
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
		days = append(days, projectDay{project: project, day: measures.Midnight(filed)})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the filings below %d issues: %w", len(keys), err)
	}
	return days, nil
}

// subtreeCloseDays is the project and the UTC day of the close of each of keys and of every issue
// below them, down the parent links to refs.IssueDepthCap, as issue_facts holds it now, and the day
// eval_day_members holds each under: the Legion close days whose repository rows a move of those
// issues' repository or parent can move, since an issue with no repository of its own resolves to
// the repository of the issue above it (ReadLegionDay), which no stored fact of its own changes
// with. Each is a primary-key read of the issue's row.
func (s *Store) subtreeCloseDays(ctx context.Context, keys []string) ([]projectDay, error) {
	rows, err := s.database.Pool.Query(ctx, `
		with recursive below as (
			select key, project_key, 0 as depth from issues where key = any($1)
			union all
			select child.key, child.project_key, below.depth + 1 from issues child join below on child.parent_key = below.key
			where below.depth < $2
		)
		select facts.project_key, facts.closed_at
		from below join issue_facts facts on facts.project_key = below.project_key and facts.issue_key = below.key
		where facts.closed_at is not null
		union
		select member.project_key, member.day::timestamp at time zone 'UTC'
		from below join eval_day_members member on member.project_key = below.project_key and member.family = $3
			and member.row_key = below.key
	`, keys, refs.IssueDepthCap, KindLegion)
	if err != nil {
		return nil, fmt.Errorf("read the closes below %d issues: %w", len(keys), err)
	}
	var days []projectDay
	var project string
	var closed time.Time
	if _, err := pgx.ForEachRow(rows, []any{&project, &closed}, func() error {
		days = append(days, projectDay{project: project, day: measures.Midnight(closed)})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the closes below %d issues: %w", len(keys), err)
	}
	return days, nil
}

// firstDays is the first day of project's all-repositories rows of each eval rows names, by eval
// and deploy repository (evalKey), among the computation's rows: the earliest over the series rows
// names of the eval and deploy repository (a zero day's rows, which name each series an eval's
// builder writes on every day it has input, for each deploy repository whose rows are kept), each
// the first entry of one range of the primary key. An eval with no such row has no entry.
func (s *Store) firstDays(ctx context.Context, project string, rows []DayRow) (map[evalKey]time.Time, error) {
	first := map[evalKey]time.Time{}
	if len(rows) == 0 {
		return first, nil
	}
	evals, deployRepos, series := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for i, row := range rows {
		evals[i], deployRepos[i], series[i] = row.EvalID, row.DeployRepo, row.Series
	}
	read, err := s.database.Pool.Query(ctx, `
		select given.eval_id, given.deploy_repo, min(stored.day)
		from unnest($2::text[], $5::text[], $3::text[]) as given(eval_id, deploy_repo, series)
		cross join lateral (
			select day from eval_days
			where project_key = $1 and eval_id = given.eval_id and writer = $4 and deploy_repo = given.deploy_repo
				and series = given.series and repo = ''
			order by day
			limit 1
		) stored
		group by given.eval_id, given.deploy_repo
	`, project, evals, series, writerComputation, deployRepos)
	if err != nil {
		return nil, fmt.Errorf("read the first days of %s's series: %w", project, err)
	}
	var key evalKey
	var day time.Time
	if _, err := pgx.ForEachRow(read, []any{&key.eval, &key.deployRepo, &day}, func() error {
		first[key] = measures.Midnight(day)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the first days of %s's series: %w", project, err)
	}
	return first, nil
}

// deliveryInputStart is the earliest moment a delivery day can hold input: the first merge of a
// pull request (delivery_pull_requests_merged_at) and the first start of a deploy or PR-checks run
// of any of repos, the repositories whose rows are kept per deploy repository (one probe of
// delivery_runs_kind_started_at for each repository and kind), nil where neither exists.
func (s *Store) deliveryInputStart(ctx context.Context, repos []string) (*time.Time, error) {
	var earliest *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select least(
			(select min(merged_at) from delivery_pull_requests where merged_at is not null),
			(select min(runs.first) from unnest($1::text[]) as kept(repo)
				cross join lateral (select least(
					(select min(started_at) from delivery_runs where repo = kept.repo and kind = $2),
					(select min(started_at) from delivery_runs where repo = kept.repo and kind = $3)) as first) runs))
	`, nonNil(repos), string(delivery.DeliveryRunKindDeploy), string(delivery.DeliveryRunKindPRChecks)).Scan(&earliest); err != nil {
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

// fillLoggedZeros writes, for each series of project's logged evals (every one where ids is nil,
// else those of ids that are logged), the zero row of every day from the series' first day to the
// pass's today that holds no all-repositories row of it, in one insert, and answers how many it
// wrote. Its rows are the readings write's (writer `readings`), since a logged eval's fold reads
// those alone: a series' first day is its first such row's, which a readings write made
// (days.go), and the zero row is the row that write leaves where every value is taken away: n 0,
// a zero sum, numerator and denominator, no sample, no maximum and no last value, so a later
// reading of that day adds to it as to any row. A day that holds a row keeps it. The zero row
// takes the listing of the series' newest row.
//
// The days it writes are the gaps between a series' stored rows, each row followed by the days up
// to the next one's (lead), and after its newest row the days up to today: so it generates and
// probes the missing days alone, where the whole span from the first day would cost a probe of
// every day the series holds on every readings write that reaches it.
//
// It takes the lock of every eval it fills first (lockEvals), as every other writer of these rows
// takes its eval's (lockEval), and reads the listings after it: so the listing it copies is the
// one the last register write of the eval left on every stored row (relistDays), and no register
// write moves it until the fill commits. The insert takes its rows in the byte order of their keys
// (eval, series, day), the order every statement that inserts or locks one eval's day rows takes
// them in (writeDays); left to the database's collation, a series `latency` and a series `Queue`
// would be taken in the other order. Its change rows wait in the change log's outbox (migration
// 20261117100014), so it takes no change log lock; a batch of them is moved into the log once it
// has committed (drainChanges).
func (r *refresh) fillLoggedZeros(ctx context.Context, project string, ids []string) (int64, error) {
	// loggedEvals is project's logged evals ($1 the project, $2 SourceLogged), which the fill reads
	// twice, each read ending in the ids it keeps.
	const loggedEvals = `select eval_id from evals_current where project_key = $1 and source = $2`
	logged := loggedEvals
	args := []any{project, SourceLogged}
	if ids != nil {
		logged += ` and eval_id = any($3)`
		args = append(args, ids)
	}
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the zero rows of %s's logged evals: %w", project, err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, logged, args...)
	if err != nil {
		return 0, fmt.Errorf("read %s's logged evals: %w", project, err)
	}
	evalIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read %s's logged evals: %w", project, err)
	}
	if len(evalIDs) == 0 {
		return 0, nil
	}
	if err := lockEvals(ctx, tx, evalIDs); err != nil {
		return 0, err
	}
	// An eval read logged above that the seed took over before its lock was taken is computed now,
	// so the insert reads which of them are logged again, under their locks.
	inserted, err := tx.Query(ctx, `
		insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, n, sum, num, den, samples, built_by)
		select stored.project_key, stored.eval_id, $5::text, stored.series, '', gap.day::date, stored.listed, 0, 0, 0, 0, '{}', $4
		from (
			select project_key, eval_id, series, day,
			       lead(day) over series_days as next_day,
			       last_value(listed) over (series_days rows between unbounded preceding and unbounded following) as listed
			from eval_days
			where project_key = $1 and writer = $5 and repo = ''
			  and eval_id = any(array(`+loggedEvals+` and eval_id = any($6)))
			window series_days as (partition by eval_id, series order by day)
		) stored
		cross join generate_series((stored.day + 1)::timestamp,
			least(coalesce(stored.next_day - 1, $3::date), $3::date)::timestamp, interval '1 day') as gap(day)
		order by stored.eval_id collate "C", stored.series collate "C", gap.day
		on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do nothing
		returning eval_id, day
	`, project, SourceLogged, r.today, r.build, writerReadings, evalIDs)
	if err != nil {
		return 0, fmt.Errorf("write the zero rows of %s's logged evals: %w", project, err)
	}
	// The days it wrote are the days whose stored figures it may have moved, folded again in its
	// transaction (foldFiguresInTx) under the locks it holds.
	spans := map[string]daySpan{}
	var written int64
	var evalID string
	var day time.Time
	if _, err := pgx.ForEachRow(inserted, []any{&evalID, &day}, func() error {
		written++
		addSpan(spans, evalID, measures.Midnight(day), measures.Midnight(day))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write the zero rows of %s's logged evals: %w", project, err)
	}
	if err := foldFiguresInTx(ctx, tx, r.build, project, spans); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the zero rows of %s's logged evals: %w", project, err)
	}
	r.store.drainChanges(ctx)
	return written, nil
}
