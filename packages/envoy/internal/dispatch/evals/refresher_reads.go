package evals

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// refresher_reads.go holds the refresher's reads of where each item a change names stands, each
// one statement, or one for each cut of a list that grows past statementBytes (statementChunks),
// and its fill of the logged evals' zero rows (fillLoggedZeros).

// eventPage is what one page of the event log names: where the events cursor now stands, whether
// the page was full, the projects whose unowned-p0 rows (the count of unowned P0 issues, and its
// repositories' counts) an event can have moved, and the issues whose repository or parent an
// event moved.
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
// issue below it resolves to, and so the repository an unowned P0 issue below it is counted under
// (WriteUnownedP0Day): the project's rows of today are then written again where it holds such an
// issue now (delivery.CountUnownedP0), since a project that holds none has every repository's
// count at 0 already, from the write that followed the event that took its last one off.
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
	unowned, reparented, movedIn := map[string]bool{}, map[string]bool{}, map[string]bool{}
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
			reparented[issue], movedIn[project] = true, true
		}
		return nil
	}); err != nil {
		return eventPage{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	for project := range movedIn {
		if unowned[project] {
			continue
		}
		held, err := delivery.CountUnownedP0(ctx, s.database.Pool, project)
		if err != nil {
			return eventPage{}, fmt.Errorf("count the unowned P0 issues of %s: %w", project, err)
		}
		if held > 0 {
			unowned[project] = true
		}
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
// for a row no longer stored. With the pull requests come their PR-checks runs, each with its
// start day: a PR-checks run counts for the project its pull request counts for (attribution), so
// a change of the pull request moves which project counts each of its runs, whichever day the run
// started on. They are read through delivery_runs_pull_request (migration 20261117100020), a probe
// of the index for each pull request.
func (s *Store) deliveryItemDays(ctx context.Context, prs map[string]pullRequestRef, runs map[string]runRef) ([]deliveryItem, error) {
	at := map[string]*time.Time{}
	checks := map[string]bool{}
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
		rows, err = s.database.Pool.Query(ctx, `
			select run.repo, run.run_id, run.started_at
			from unnest($1::text[], $2::int[]) as given(repo, number)
			join delivery_runs run on run.repo = given.repo and run.pr_number = given.number
			where run.kind = $3 and run.event = 'pull_request'
		`, repos, numbers, string(delivery.DeliveryRunKindPRChecks))
		if err != nil {
			return nil, fmt.Errorf("read the PR-checks runs of %d changed pull requests: %w", len(members), err)
		}
		var repo string
		var id int64
		var started time.Time
		if _, err := pgx.ForEachRow(rows, []any{&repo, &id, &started}, func() error {
			member, day := RunMember(repo, id), measures.Midnight(started)
			at[member], checks[member] = &day, true
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the PR-checks runs of %d changed pull requests: %w", len(members), err)
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
	for member := range runs {
		delete(checks, member)
	}
	members := slices.Concat(slices.Collect(maps.Keys(prs)), slices.Collect(maps.Keys(runs)), slices.Collect(maps.Keys(checks)))
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
// whose day it is, by a write of another day; those a failed pass left among them. projects is
// every project and owner key (refresh.owners), whose keys grow with the history, so it goes in
// statements of at most statementBytes: a project's days are all in the one statement that names
// it, in the order of its family and its day.
func (s *Store) owedDays(ctx context.Context, projects []string) ([]owedDay, error) {
	var owed []owedDay
	for chunk := range statementChunks(projects, paramBytes(1), textBytes) {
		rows, err := s.database.Pool.Query(ctx, `
			select project_key, family, day from eval_days_owed where project_key = any($1) order by project_key, family, day
		`, chunk)
		if err != nil {
			return nil, fmt.Errorf("read the days owed a rewrite: %w", err)
		}
		var one owedDay
		if _, err := pgx.ForEachRow(rows, []any{&one.project, &one.family, &one.day}, func() error {
			one.day = measures.Midnight(one.day)
			owed = append(owed, one)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the days owed a rewrite: %w", err)
		}
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
// deploy repository and key. projects (every project and owner key) and members (a page's items)
// grow with the history and the page, so each goes in cuts (statementChunks): the projects in
// cuts of at most half of statementBytes, all in one where they fit, and the members in cuts of
// what the rest of a statement holds, each statement one cut of each.
func (s *Store) heldMembers(ctx context.Context, projects, families, deployRepos, members []string) ([]heldMember, error) {
	projectBytes := 0
	for i := range projects {
		projectBytes += textBytes(&projects[i])
	}
	// A cut of the projects takes at most reserve bytes, the rest of a statement its other lists:
	// statementChunks closes a cut at statementBytes, counted from statementBytes-reserve.
	reserve := min(projectBytes, statementBytes/2)
	base := paramBytes(4) + reserve
	for _, list := range [][]string{families, deployRepos} {
		for i := range list {
			base += textBytes(&list[i])
		}
	}
	what := fmt.Sprintf("where %d items were counted", len(members))
	var held []heldMember
	for named := range statementChunks(projects, statementBytes-reserve, textBytes) {
		for keys := range statementChunks(members, base, textBytes) {
			rows, err := s.database.Pool.Query(ctx, `
				select project_key, row_key, day, shipped_by from eval_day_members
				where project_key = any($1) and family = any($2) and deploy_repo = any($3) and row_key = any($4)
				order by project_key, row_key, day
			`, named, families, deployRepos, keys)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", what, err)
			}
			found, err := collectHeld(rows, what)
			if err != nil {
				return nil, err
			}
			held = append(held, found...)
		}
	}
	return held, nil
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
// completion as their deploy time. projects (every project and owner key) and runs (a page's
// items) grow with the history and the page, so each goes in cuts as heldMembers cuts them: the
// projects in cuts of at most half of statementBytes, all in one where they fit, and the runs in
// cuts of what the rest of a statement holds, each statement one cut of each.
func (s *Store) shippedMembers(ctx context.Context, projects, runs []string) ([]heldMember, error) {
	projectBytes := 0
	for i := range projects {
		projectBytes += textBytes(&projects[i])
	}
	reserve := min(projectBytes, statementBytes/2)
	base := paramBytes(2, KindDelivery) + reserve
	what := fmt.Sprintf("the pull requests %d runs shipped", len(runs))
	var held []heldMember
	for named := range statementChunks(projects, statementBytes-reserve, textBytes) {
		for shipped := range statementChunks(runs, base, textBytes) {
			rows, err := s.database.Pool.Query(ctx, shippedMembersQuery, named, shipped, KindDelivery)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", what, err)
			}
			found, err := collectHeld(rows, what)
			if err != nil {
				return nil, err
			}
			held = append(held, found...)
		}
	}
	return held, nil
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

// firstDays is firstDaysIn on the pool.
func (s *Store) firstDays(ctx context.Context, zeros map[string][]DayRow, before *time.Time) (map[string]map[evalKey]time.Time, error) {
	return firstDaysIn(ctx, s.database.Pool, zeros, before)
}

// firstDaysIn reads in q, for each project of zeros, the first day of its all-repositories rows of
// each eval its rows name, by eval and deploy repository (evalKey), among the computation's rows
// before before (every row, where before is nil): the earliest over the series the project's rows
// name of the eval and deploy repository (a zero day's rows, which name each series an eval's
// builder writes on every day it has input, for each deploy repository whose rows are kept), each
// the first entry of one range of the primary key, which the bound ends. An eval with no such row
// has no entry, and a project with none an empty map. One read for every project, the rows in
// statements of at most statementBytes (statementChunks): a project's rows of one eval and deploy
// repository that two statements share are the earliest of both. The refresher reads it on the
// pool (firstDays), and the read-time path in its read's snapshot (ComputeAtRead).
func firstDaysIn(ctx context.Context, q delivery.Querier, zeros map[string][]DayRow, before *time.Time) (map[string]map[evalKey]time.Time, error) {
	type given struct {
		project string
		row     *DayRow
	}
	first := make(map[string]map[evalKey]time.Time, len(zeros))
	var named []given
	for _, project := range slices.Sorted(maps.Keys(zeros)) {
		first[project] = map[evalKey]time.Time{}
		for i := range zeros[project] {
			named = append(named, given{project: project, row: &zeros[project][i]})
		}
	}
	size := func(one *given) int {
		return 4*4 + len(one.project) + len(one.row.EvalID) + len(one.row.DeployRepo) + len(one.row.Series)
	}
	for chunk := range statementChunks(named, paramBytes(4, writerComputation)+4+8, size) {
		projects, evals, deployRepos, series := make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
		for i, one := range chunk {
			projects[i], evals[i], deployRepos[i], series[i] = one.project, one.row.EvalID, one.row.DeployRepo, one.row.Series
		}
		read, err := q.Query(ctx, `
			select given.project_key, given.eval_id, given.deploy_repo, min(stored.day)
			from unnest($1::text[], $2::text[], $5::text[], $3::text[]) as given(project_key, eval_id, deploy_repo, series)
			cross join lateral (
				select day from eval_days
				where project_key = given.project_key and eval_id = given.eval_id and writer = $4 and deploy_repo = given.deploy_repo
					and series = given.series and repo = '' and day < coalesce($6::date, 'infinity'::date)
				order by day
				limit 1
			) stored
			group by given.project_key, given.eval_id, given.deploy_repo
		`, projects, evals, series, writerComputation, deployRepos, before)
		if err != nil {
			return nil, fmt.Errorf("read the first days of %d projects' series: %w", len(zeros), err)
		}
		var project string
		var key evalKey
		var day time.Time
		if _, err := pgx.ForEachRow(read, []any{&project, &key.eval, &key.deployRepo, &day}, func() error {
			day = measures.Midnight(day)
			if since, held := first[project][key]; !held || day.Before(since) {
				first[project][key] = day
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the first days of %d projects' series: %w", len(zeros), err)
		}
	}
	return first, nil
}

// heldDays is each day from from through through on which a project of projects holds a computed
// all-repositories row of one of the evals slugs names, by project, family and day: the days a
// walk's batch writes whether or not it builds a row of them, so a day whose input left keeps no
// row a whole walk would not hold (refresh.walkDays). One read of eval_days_window (project, repo,
// day, over the listed rows) for the batch: every series of a computed eval is listed but a job
// series, which a day holds only beside its eval's listed series. Its evals are named by their
// slugs, a row's eval id being its project's key, a `/` and its slug (eval_days_eval_of_project),
// which a filter of the index range reads as the eval ids of every project would be, and the
// projects, every project and owner key, which grow with the history, go in statements of at most
// statementBytes.
func (s *Store) heldDays(ctx context.Context, family string, projects, slugs []string, from, through time.Time) (map[builtDay]bool, error) {
	held := map[builtDay]bool{}
	base := paramBytes(2, writerComputation) + 2*(4+8)
	for i := range slugs {
		base += textBytes(&slugs[i])
	}
	for chunk := range statementChunks(projects, base, textBytes) {
		rows, err := s.database.Pool.Query(ctx, `
			select distinct project_key, day from eval_days
			where project_key = any($1) and repo = '' and listed and day >= $2 and day <= $3
				and substr(eval_id, length(project_key) + 2) = any($4) and writer = $5
		`, chunk, from, through, slugs, writerComputation)
		if err != nil {
			return nil, fmt.Errorf("read the days holding %s rows from %s through %s: %w", family, from.Format(time.DateOnly), through.Format(time.DateOnly), err)
		}
		var project string
		var day time.Time
		if _, err := pgx.ForEachRow(rows, []any{&project, &day}, func() error {
			held[builtDay{family: family, project: project, day: measures.Midnight(day)}] = true
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the days holding %s rows from %s through %s: %w", family, from.Format(time.DateOnly), through.Format(time.DateOnly), err)
		}
	}
	return held, nil
}

// deliveryInputStart is the earliest moment a delivery day can hold input: the first merge of a
// pull request (delivery_pull_requests_merged_at), the first start of a deploy or PR-checks run of
// any of repos, the repositories whose rows are kept per deploy repository (one probe of
// delivery_runs_kind_started_at for each repository and kind), and the start of each events
// repository among them (keptRepo.since, its first posted deploy's day); nil where none exists.
func (s *Store) deliveryInputStart(ctx context.Context, repos []keptRepo) (*time.Time, error) {
	var earliest *time.Time
	names := []string{}
	for _, repo := range repos {
		if !IsEventsRepo(repo.name) {
			names = append(names, repo.name)
		}
	}
	if err := s.database.Pool.QueryRow(ctx, `
		select least(
			(select min(merged_at) from delivery_pull_requests where merged_at is not null),
			(select min(runs.first) from unnest($1::text[]) as kept(repo)
				cross join lateral (select least(
					(select min(started_at) from delivery_runs where repo = kept.repo and kind = $2),
					(select min(started_at) from delivery_runs where repo = kept.repo and kind = $3)) as first) runs))
	`, names, string(delivery.DeliveryRunKindDeploy), string(delivery.DeliveryRunKindPRChecks)).Scan(&earliest); err != nil {
		return nil, fmt.Errorf("read the first delivery input: %w", err)
	}
	for _, repo := range repos {
		if IsEventsRepo(repo.name) && (earliest == nil || repo.since.Before(*earliest)) {
			since := repo.since
			earliest = &since
		}
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

// fillLoggedZeros writes, for each series of project's logged evals, the zero row of every day from
// the series' first day to the pass's today that holds no all-repositories row of it, in one insert
// (loggedZeros, the statement the readings write sends for the days its rows leave owed), and
// answers how many it wrote. Its rows are the readings write's (writer `readings`), since a logged
// eval's fold reads those alone: a series' first day is its first such row's, which a readings
// write made and which holds a value (days.go, dropEmptiedRows). A day that holds a row keeps
// it, and the zero row takes the listing of the series' newest row.
//
// The days it writes are the gaps between a series' stored rows, each row followed by the days up
// to the next one's (lead), and after its newest row the days up to today. It reads every stored
// all-repositories row of each eval it fills, once and under the evals' locks (a window over each
// series' rows answers each row's next), and generates and probes the missing days alone, where
// the whole span from the first day would cost a probe of every day the series holds.
//
// The readings write writes the zero rows its rows leave owed through its own today
// (writeLoggedZeros), so what is left to the fill is the turn of each new day, which no write
// makes, and a walk begun again (walkDaily). At a midnight, a write that read the day before as its
// today and the fill of the new day leave every day of the eval its row, and write none twice: a
// row both would write is the same zero row, inserted once (on conflict do nothing), each under
// the eval's lock. Of a write of an eval the fill reads as logged, either the write commits first,
// and the fill's insert, which reads the rows once the fill holds the eval's lock, fills after the
// write's rows; or the fill commits first, and the write, which reads its today once it holds the
// eval's lock, reads the new day. A write that registers its eval, which no read of the fill's
// finds until it commits, holds the lock of the project's `evals-md` from before it stores the eval
// to its commit (lockDocument), so the fill first waits out every such write that is running
// (awaitRegistrations): either the eval is stored when the fill reads the project's evals, or the
// write takes that lock after the wait and reads its today after it, the new day. Each reading of
// the new day holds while the database's clock and the refresher's agree to within the time a pass
// takes to reach its fill.
//
// It takes the lock of every eval it fills first (lockEvals), as every other writer of these rows
// takes its eval's (lockEval), and reads the listings after it: so the listing it copies is the
// one the last register write of the eval left on every stored row (relistDays), and no register
// write moves it until the fill commits. Its change rows wait in the change log's outbox
// (migration 20261117100014), so it takes no change log lock; a batch of them is moved into the
// log once it has committed (drainChanges).
func (r *refresh) fillLoggedZeros(ctx context.Context, project string) (int64, error) {
	if err := r.awaitRegistrations(ctx, project); err != nil {
		return 0, err
	}
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the zero rows of %s's logged evals: %w", project, err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select eval_id from evals_current where project_key = $1 and source = $2`, project, SourceLogged)
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
	// so the insert reads which of them are logged again, under their locks. The days it writes are
	// the days whose stored figures it may have moved, folded again in its transaction
	// (foldFiguresInTx) under the locks it holds.
	spans := map[string]daySpan{}
	written, err := writeZeroRows(ctx, tx, `
		select project_key, eval_id, series, day,
		       lead(day) over series_days as next_day,
		       last_value(listed) over (series_days rows between unbounded preceding and unbounded following) as listed
		from eval_days
		where project_key = $1 and writer = $3 and repo = ''
		  and eval_id = any(array(select eval_id from evals_current where project_key = $1 and source = $5 and eval_id = any($6)))
		window series_days as (partition by eval_id, series order by day)
	`, spans, project, r.today, writerReadings, r.build, SourceLogged, evalIDs)
	if err != nil {
		return 0, fmt.Errorf("write the zero rows of %s's logged evals: %w", project, err)
	}
	if err := foldFiguresInTx(ctx, tx, r.build, map[string]map[string]daySpan{project: spans}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the zero rows of %s's logged evals: %w", project, err)
	}
	r.store.drainChanges(ctx)
	return written, nil
}

// awaitRegistrations waits until no write that registers an eval of project is running. Each takes
// the lock of the project's `evals-md` before it stores the eval and holds it to its commit
// (lockDocument): the document's owner lock, or, where the project has none yet, the project's
// documents' lock (docs.LockProjectArtifacts), under which it creates the document. This takes the
// second and then the first, the order every taker of both takes them in, in a transaction of its
// own that takes nothing else and commits at once, so it joins no lock cycle.
func (r *refresh) awaitRegistrations(ctx context.Context, project string) error {
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the wait for %s's registrations: %w", project, err)
	}
	defer tx.Rollback(ctx)
	if err := docs.LockProjectArtifacts(ctx, tx, project); err != nil {
		return err
	}
	var document string
	switch err := tx.QueryRow(ctx, `
		select id::text from artifacts where project_key = $1 and issue_key is null and session_id is null and slug = $2
	`, project, DocumentSlug).Scan(&document); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read the %s document of %s: %w", DocumentSlug, project, err)
	default:
		if err := store.LockOwner(ctx, tx, store.ArtifactOwner(document)); err != nil {
			return fmt.Errorf("wait for the registrations of %s: %w", project, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("end the wait for %s's registrations: %w", project, err)
	}
	return nil
}
