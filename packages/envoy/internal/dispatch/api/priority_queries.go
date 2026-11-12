// priority_queries.go is the read behind GET /api/v1/priorities: every live priority with the
// work that advances it, and the two faults. It is the query half of priorities.go, as
// issue_queries.go is of the issue routes, and it touches `priority_nodes` and `priority_tuples`
// through prioritystore alone.
//
// The body is five statements. The priorities are prioritystore.List; the relations are the
// `advances` tuples every live priority holds now (priorityRelationsQuery), read once and then
// sent as the seed of each statement that follows, so the tuple log is read one time however many
// counts rest on it; the counts, the newest event under each priority and the two fault lists are
// four statements over that seed, sent as one batch, which is one round trip (readPriorities).
//
// `?summary=true` answers the two fault counts alone, so it runs the two fault statements in the
// shape that counts and lists nothing (priorityUnplacedQuery, priorityParkedQuery) and neither of
// the two reads that fill a priority's row.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

// priorityInFlightStatuses are the four statuses work is running in, which is what the first
// fault reads: an issue standing in one of them advances a priority or is a fault. They are the
// lifecycle's own words (model.IssueStatuses) and the page's "in flight" column adds exactly
// these four counts.
var priorityInFlightStatuses = []string{"in_progress", "testing", "needs_review", "retro"}

// priorityParkedStatuses are the two statuses the second fault reads: an issue that advances a
// priority and stands in either is parked work, whichever way it got there.
var priorityParkedStatuses = []string{"backlog", "icebox"}

// priorityFaultRows is how many rows each fault list carries beside its whole count. The count is
// over every faulted issue and the rows are the newest of them, so a reader sees the size of the
// fault and the work it is newest on without the answer growing with the deployment.
const priorityFaultRows = 50

// priorityClosedWindow is how far back "closed recently" reaches, as an interval literal.
const priorityClosedWindow = "7 days"

// priorityNoneWritten is the sentence the answer carries when no priority is live: the route then
// answers 200 with an empty list rather than an error, and the page draws this.
const priorityNoneWritten = "No priority is live now, so the list is empty. Write one, then say which work advances it."

// sqlStringList writes values as the literal list an `in` clause takes. Its callers pass this
// file's own status constants and never a value a request carried, so nothing it writes comes
// from outside the binary.
func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "'" + value + "'"
	}
	return strings.Join(quoted, ", ")
}

// priorityRelationsQuery reads the `advances` relation of every live priority as it stands now,
// one row per (issue, priority): the newest tuple of each pair, kept when it asserts the relation,
// over the priority_tuples_current index this reading of the log was written for
// (prioritystore.CurrentOut). $1 is the live priority ids, so a priority whose state is `gone` is
// no priority and the work under it advances none.
//
// Its answer is the seed every statement below takes, rather than each of them reading the log
// again: the relations are hundreds of rows where the issues under them are thousands.
const priorityRelationsQuery = `select newest.from_id, newest.to_id
	from (
		select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.asserted
		from priority_tuples t
		where t.from_kind = '` + prioritystore.KindIssue + `' and t.kind = '` + prioritystore.RelationAdvances + `'
			and t.to_kind <> '` + prioritystore.KindValue + `'
		order by t.from_id, t.to_id, t.seq desc
	) newest
	where newest.asserted and newest.to_id = any($1::text[])
	order by newest.from_id, newest.to_id`

// priorityMemberTerms are the issues that advance each priority: the relations the request read
// ($1 the issue keys, $2 the priority ids, one pair per row) and every issue below one of them.
// An issue advances a priority when its own row names it or when any issue on its parent chain
// does, so the walk descends from each stated issue — written as one descent per request rather
// than one climb per issue, which is the shape issueAdvancesFilterTerms gives the same rule for
// the listing. A relation on a child places nothing above it, so nothing climbs.
//
// The descent is bounded by parentDepthCap, which no chain passes (refuseParentTooDeep), so a
// descendant the cap cuts off is one no chain could have reached anyway, and a raced reparent
// that ever committed a cycle still terminates. `members` is distinct because an issue inherits
// the same priority from every issue above it that states it.
var priorityMemberTerms = `members_seed as (
		select key, priority from unnest($1::text[], $2::text[]) as seed(key, priority)
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m join issues c on c.parent_key = m.key
		where m.depth < ` + parentDepthCapSQL + `
	), members as (
		select distinct key, priority from members_spread
	)`

// priorityProjectFilter narrows a read to the projects the request named, over the issue aliased
// `i`; a request that named none ($3 empty) reads every project. It narrows the counts and the
// two fault lists and never the list of priorities, which belongs to no project.
const priorityProjectFilter = `(cardinality($3::text[]) = 0 or i.project_key = any($3::text[]))`

// priorityCountsQuery counts the work under each priority, one row per (priority, status) over
// the issues that advance it: the open issues standing in that status, how many of those a
// session holds, the open asks on them, and the ones that closed inside priorityClosedWindow. The
// closed count rides the same grouping rather than a statement of its own, since it is a filter
// over rows this read already has in hand; a closed issue counts in nothing else, so a status
// whose issues are all closed answers 0 open and its closed count.
//
// The open-ask read keeps its state filter in its own where clause, so it matches the partial
// asks_open(issue_key) where state = 'open' index rather than scanning asks, as the listing's own
// open-ask read does (issueSummaryJoins).
var priorityCountsQuery = `with recursive ` + priorityMemberTerms + `
	select m.priority, i.status,
	       count(*) filter (where i.closed_at is null) as open,
	       count(*) filter (where i.closed_at is null and i.claimed_by is not null) as claimed,
	       coalesce(sum(open_asks.total) filter (where i.closed_at is null), 0)::bigint as asks,
	       count(*) filter (where i.closed_at >= now() - interval '` + priorityClosedWindow + `') as closed
	from members m
	join issues i on i.key = m.key
	left join (
		select a.issue_key, count(*) as total
		from asks a
		where a.state = 'open' and a.issue_key in (select key from members)
		group by a.issue_key
	) open_asks on open_asks.issue_key = i.key
	where ` + priorityProjectFilter + `
	group by m.priority, i.status`

// priorityNewestEventQuery reads the newest event under each priority: max(events.created_at)
// over the issues that advance it. It is the events table and not issues.updated_at, which a
// claim and an issue patch write and a comment does not — comment_create.go appends a
// `comment.created` event and writes no issue row — so a priority whose work was last spoken
// about rather than last moved still reads as having moved.
var priorityNewestEventQuery = `with recursive ` + priorityMemberTerms + `
	select m.priority, max(e.created_at)
	from members m
	join issues i on i.key = m.key
	join events e on e.issue_key = i.key
	where ` + priorityProjectFilter + `
	group by m.priority`

// priorityUnplacedQuery is the first fault: an open issue in flight that advances no live
// priority, by its own relation or by one above it. `summary` answers the count alone, which is
// what the navigation rail reads; the full text answers the count, the count per project and the
// newest priorityFaultRows rows by the time each issue last changed, all three from one scan of
// one snapshot, since two statements read two snapshots and can disagree about a table a write is
// moving.
//
// The count and the per-project counts ride every row because a statement answers one shape: the
// rows are the left side's single row joined to the page, so an empty fault still answers its
// count with every row column null, which scanPriorityFault drops.
func priorityUnplacedQuery(summary bool) string {
	faulted := `with recursive ` + priorityMemberTerms + `, unplaced as (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at
		from issues i
		where i.closed_at is null
			and i.status in (` + sqlStringList(priorityInFlightStatuses) + `)
			and ` + priorityProjectFilter + `
			and not exists (select 1 from members m where m.key = i.key)
	)`
	if summary {
		return faulted + `
	select count(*) from unplaced`
	}
	return faulted + `
	select (select count(*) from unplaced) as total,
	       (select coalesce(jsonb_object_agg(counted.project_key, counted.total), '{}'::jsonb)
	        from (select project_key, count(*) as total from unplaced group by project_key) counted) as by_project,
	       listed.key, listed.title, listed.project_key, listed.status, listed.assignee,
	       listed.claimed_by, listed.claimed_at, listed.updated_at
	from (select 1 as one) seed
	left join (
		select * from unplaced
		order by updated_at desc, key asc
		limit ` + strconv.Itoa(priorityFaultRows) + `
	) listed on true`
}

// priorityParkedQuery is the second fault: an issue that advances a live priority and stands in
// `backlog` or `icebox`. Beside the row's own columns it carries the time the issue was moved
// there and the actor who moved it, read from that issue's own `issue.updated` events — the
// newest whose payload names `status` in `changed` and whose status is one of the two — which is
// the one read this body makes over the events of the parked members alone. An issue whose events
// do not reach back to the move answers both cells null and stays in the list, since the fault is
// the work standing there and not the record of how it arrived.
//
// Each lateral walks one issue's events backwards over the unique (issue_key, seq) index and
// stops at the first match, so the read costs the parked members rather than the table.
func priorityParkedQuery(summary bool) string {
	statuses := sqlStringList(priorityParkedStatuses)
	parked := `with recursive ` + priorityMemberTerms + `, parked as (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at,
		       array_agg(distinct m.priority order by m.priority) as priorities
		from members m
		join issues i on i.key = m.key
		where i.closed_at is null
			and i.status in (` + statuses + `)
			and ` + priorityProjectFilter + `
		group by i.key
	)`
	if summary {
		return parked + `
	select count(*) from parked`
	}
	return parked + `
	select (select count(*) from parked) as total,
	       (select coalesce(jsonb_object_agg(counted.priority, counted.total), '{}'::jsonb)
	        from (
	          select m.priority, count(*) as total
	          from members m join parked p on p.key = m.key
	          group by m.priority
	        ) counted) as by_priority,
	       listed.key, listed.title, listed.project_key, listed.status, listed.assignee,
	       listed.claimed_by, listed.claimed_at, listed.updated_at,
	       listed.priorities, listed.moved_at, listed.moved_by
	from (select 1 as one) seed
	left join (
		select p.*, moved.created_at as moved_at, moved.actor as moved_by
		from parked p
		left join lateral (
			select e.created_at, e.actor
			from events e
			where e.issue_key = p.key and e.type = 'issue.updated'
				and e.payload->'changed' @> '"status"'::jsonb
				and e.payload->>'status' in (` + statuses + `)
			order by e.seq desc
			limit 1
		) moved on true
		order by moved.created_at desc nulls last, p.key asc
		limit ` + strconv.Itoa(priorityFaultRows) + `
	) listed on true`
}

// The four texts the two faults run, built once at start: pgx caches a prepared plan per
// statement text, so the summary call and the full call never replan each other's.
var (
	unplacedQuery        = priorityUnplacedQuery(false)
	unplacedSummaryQuery = priorityUnplacedQuery(true)
	parkedQuery          = priorityParkedQuery(false)
	parkedSummaryQuery   = priorityParkedQuery(true)
)

// readPriorities answers the whole body of GET /api/v1/priorities: the priorities in rank order,
// the work under each, and the two faults, narrowed to `projects` where it is narrowed at all.
//
// It is two round trips plus the relations' own: prioritystore.List, then the relations, then one
// batch of the statements that rest on them. A summary sends the two fault counts in that batch
// and neither read that fills a priority's row, so it reads no issue of a priority that has no
// fault under it.
func (s *server) readPriorities(ctx context.Context, projects []string, summary bool) (model.Priorities, error) {
	nodes, err := prioritystore.List(ctx, s.deps.Store.Pool)
	if err != nil {
		return model.Priorities{}, err
	}
	body := model.Priorities{
		Priorities:          make([]model.PriorityRow, 0, len(nodes)),
		WorkUnderNoPriority: model.WorkUnderNoPriority{ByProject: map[string]int{}, Issues: []model.PriorityIssue{}},
		PriorityWorkParked:  model.PriorityWorkParked{ByPriority: map[string]int{}, Issues: []model.PriorityIssue{}},
	}
	if len(nodes) == 0 {
		body.Empty = priorityNoneWritten
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	seedKeys, seedIDs, err := s.readPriorityRelations(ctx, ids)
	if err != nil {
		return model.Priorities{}, err
	}
	arguments := []any{seedKeys, seedIDs, projects}

	batch := &pgx.Batch{}
	if !summary {
		rows := make(map[string]*model.PriorityRow, len(nodes))
		for _, node := range nodes {
			body.Priorities = append(body.Priorities, model.PriorityRow{
				Priority: priorityFrom(node),
				Work:     model.PriorityWork{ByStatus: zeroPriorityStatusCounts()},
			})
			rows[node.ID] = &body.Priorities[len(body.Priorities)-1]
		}
		batch.Queue(priorityCountsQuery, arguments...).Query(func(scanned pgx.Rows) error {
			return scanPriorityCounts(scanned, rows)
		})
		batch.Queue(priorityNewestEventQuery, arguments...).Query(func(scanned pgx.Rows) error {
			return scanPriorityNewestEvents(scanned, rows)
		})
	}
	queuePriorityFault(batch, unplacedQuery, unplacedSummaryQuery, summary, arguments, false,
		func(count int, counts map[string]int, issues []model.PriorityIssue) {
			body.WorkUnderNoPriority.Count = count
			if counts != nil {
				body.WorkUnderNoPriority.ByProject = counts
			}
			body.WorkUnderNoPriority.Issues = issues
		})
	queuePriorityFault(batch, parkedQuery, parkedSummaryQuery, summary, arguments, true,
		func(count int, counts map[string]int, issues []model.PriorityIssue) {
			body.PriorityWorkParked.Count = count
			if counts != nil {
				body.PriorityWorkParked.ByPriority = counts
			}
			body.PriorityWorkParked.Issues = issues
		})
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return model.Priorities{}, fmt.Errorf("read the work under the priorities: %w", err)
	}
	return body, nil
}

// readPriorityRelations reads the `advances` relations of every live priority as two lists in one
// order, the issue key and the priority id of each: the pair of arrays the statements below take
// as their seed, which `unnest` reads back as rows.
func (s *server) readPriorityRelations(ctx context.Context, ids []string) ([]string, []string, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, priorityRelationsQuery, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read the advances relations: %w", err)
	}
	defer rows.Close()
	keys := []string{}
	priorities := []string{}
	for rows.Next() {
		var key, priority string
		if err := rows.Scan(&key, &priority); err != nil {
			return nil, nil, fmt.Errorf("read an advances relation: %w", err)
		}
		keys = append(keys, key)
		priorities = append(priorities, priority)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the advances relations: %w", err)
	}
	return keys, priorities, nil
}

// zeroPriorityStatusCounts is a priority's status counts before any row is read: every status of
// the lifecycle at 0, so a reader adding the columns it draws finds each key whatever the work
// under the priority is, and a priority nothing advances answers zeros rather than an empty map.
func zeroPriorityStatusCounts() map[string]int {
	counts := make(map[string]int, len(model.IssueStatuses))
	for _, status := range model.IssueStatuses {
		counts[status] = 0
	}
	return counts
}

// scanPriorityCounts reads priorityCountsQuery's rows onto the priorities they count. A row
// naming a priority the list does not hold is dropped: the two reads are one snapshot each, and a
// priority deleted between them is one this answer no longer lists.
func scanPriorityCounts(rows pgx.Rows, into map[string]*model.PriorityRow) error {
	for rows.Next() {
		var id, status string
		var open, claimed, asks, closed int
		if err := rows.Scan(&id, &status, &open, &claimed, &asks, &closed); err != nil {
			return fmt.Errorf("read the counts of a priority: %w", err)
		}
		row, listed := into[id]
		if !listed {
			continue
		}
		row.Work.ByStatus[status] += open
		row.Work.Claimed += claimed
		row.Work.OpenAsks += asks
		row.Work.ClosedLast7Days += closed
	}
	return rows.Err()
}

// scanPriorityNewestEvents reads priorityNewestEventQuery's rows onto the priorities they belong
// to. A priority no issue advances answers no row and keeps its null.
func scanPriorityNewestEvents(rows pgx.Rows, into map[string]*model.PriorityRow) error {
	for rows.Next() {
		var id string
		var newest *time.Time
		if err := rows.Scan(&id, &newest); err != nil {
			return fmt.Errorf("read the newest event under a priority: %w", err)
		}
		if row, listed := into[id]; listed {
			row.Work.NewestEventAt = newest
		}
	}
	return rows.Err()
}

// queuePriorityFault queues one fault's statement and hands its answer to `apply`: the whole
// count, the counts by project or by priority, and the rows, which a summary answers as the count
// alone with a nil map and no row.
func queuePriorityFault(
	batch *pgx.Batch,
	query, summaryQuery string,
	summary bool,
	arguments []any,
	parked bool,
	apply func(count int, counts map[string]int, issues []model.PriorityIssue),
) {
	if summary {
		batch.Queue(summaryQuery, arguments...).QueryRow(func(row pgx.Row) error {
			count := 0
			if err := row.Scan(&count); err != nil {
				return fmt.Errorf("count a fault: %w", err)
			}
			apply(count, nil, []model.PriorityIssue{})
			return nil
		})
		return
	}
	batch.Queue(query, arguments...).Query(func(rows pgx.Rows) error {
		count, counts, issues, err := scanPriorityFault(rows, parked)
		if err != nil {
			return err
		}
		apply(count, counts, issues)
		return nil
	})
}

// priorityFaultRow is one row of a fault statement as it is scanned: the whole count and the
// grouped counts, which every row carries, and the issue's own columns, which are null on the row
// an empty fault answers.
type priorityFaultRow struct {
	total      int
	counts     map[string]int
	key        *string
	title      *string
	project    *string
	status     *string
	assignee   *string
	claim      claimScan
	changedAt  *time.Time
	priorities []string
	movedAt    *time.Time
	movedBy    []byte
}

// targets are the row's scan targets. The parked fault carries three columns more: the priorities
// the issue advances, and the two cells of the move into `backlog` or `icebox`.
func (r *priorityFaultRow) targets(parked bool) []any {
	targets := []any{&r.total, &r.counts, &r.key, &r.title, &r.project, &r.status, &r.assignee}
	targets = append(targets, r.claim.targets()...)
	targets = append(targets, &r.changedAt)
	if parked {
		targets = append(targets, &r.priorities, &r.movedAt, &r.movedBy)
	}
	return targets
}

// resolve turns a scanned row into the issue the fault list carries.
func (r *priorityFaultRow) resolve() (model.PriorityIssue, error) {
	claim, err := r.claim.resolve(*r.key)
	if err != nil {
		return model.PriorityIssue{}, err
	}
	issue := model.PriorityIssue{
		Key:        *r.key,
		Title:      *r.title,
		Project:    *r.project,
		Status:     *r.status,
		Assignee:   r.assignee,
		Claim:      claim,
		ChangedAt:  *r.changedAt,
		Priorities: r.priorities,
		MovedAt:    r.movedAt,
	}
	if issue.Priorities == nil {
		issue.Priorities = []string{}
	}
	if len(r.movedBy) > 0 {
		var actor model.Actor
		if err := json.Unmarshal(r.movedBy, &actor); err != nil {
			return model.PriorityIssue{}, fmt.Errorf("decode who moved %s: %w", issue.Key, err)
		}
		issue.MovedBy = &actor
	}
	return issue, nil
}

// scanPriorityFault reads one fault statement's answer: its whole count, its grouped counts and
// its rows. A fault with no issue answers one row whose issue columns are all null, which is the
// count with no row beside it.
//
// The rows are ordered again here, by the column the statement cut its 50 on: a join keeps no
// order of its own, so the order the answer carries is this one rather than the plan's.
func scanPriorityFault(rows pgx.Rows, parked bool) (int, map[string]int, []model.PriorityIssue, error) {
	count := 0
	counts := map[string]int{}
	issues := []model.PriorityIssue{}
	for rows.Next() {
		row := priorityFaultRow{}
		if err := rows.Scan(row.targets(parked)...); err != nil {
			return 0, nil, nil, fmt.Errorf("read a fault row: %w", err)
		}
		count = row.total
		if row.counts != nil {
			counts = row.counts
		}
		if row.key == nil {
			continue
		}
		issue, err := row.resolve()
		if err != nil {
			return 0, nil, nil, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, nil, fmt.Errorf("read a fault: %w", err)
	}
	slices.SortFunc(issues, func(a, b model.PriorityIssue) int {
		if parked {
			if order := comparePriorityTimes(a.MovedAt, b.MovedAt); order != 0 {
				return order
			}
		} else if order := b.ChangedAt.Compare(a.ChangedAt); order != 0 {
			return order
		}
		return strings.Compare(a.Key, b.Key)
	})
	return count, counts, issues, nil
}

// comparePriorityTimes orders two move times newest first, with an issue whose events do not
// reach back to its move last: it stays in the list and stands after every issue whose move is
// recorded, which is where the statement's `nulls last` put it.
func comparePriorityTimes(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return b.Compare(*a)
	}
}
