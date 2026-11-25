// priority_queries.go is the read behind GET /api/v1/priorities: every live priority with the
// work that advances it, and the two faults. It is the query half of priorities.go, as
// issue_queries.go is of the issue routes. It writes neither `priority_nodes` nor
// `priority_tuples`, which prioritystore alone writes; it reads them through prioritystore.List
// and through the SQL below, which splices prioritystore.AdvancesTupleRead, the one reading of
// the `advances` relation, and joins it to the issues it counts. The parked rows carry their
// `advances` entries through issueAdvancesTerms, the inheritance walk every issue read climbs.
//
// The body is two statements in two round trips. The priorities are prioritystore.List; then one
// statement (priorityReadQuery) reads the `advances` relations of those priorities, walks the
// issues that advance each of them once (priorityMemberTerms), and answers the counts, the newest
// event under each priority and the two fault lists from that one walk, as four jsonb columns of
// one row.
//
// `?summary=true` answers the two fault counts alone, so its one statement (prioritySummaryQuery)
// counts both faults from one scan of the open issues that stand in flight or parked, and reads
// nothing that fills a priority's row.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

// priorityInFlightStatuses are the four statuses work is running in, which is what the first
// fault reads: an issue standing in one of them advances a priority or is a fault. The page's
// "in flight" column adds exactly these four counts.
//
// priorityParkedStatuses are the two the second fault reads: an issue that advances a priority
// and stands in either is parked work, whichever way it got there.
//
// Both are the lifecycle's own groupings (model.IssueStatusesInFlight, model.IssueStatusesParked)
// rather than a list of this file's own, because the seeded corpus reports the same two faults
// over the same two groupings (cmd/dispatch-seed) and the two readings have to ask one thing.
var (
	priorityInFlightStatuses = model.IssueStatusesInFlight
	priorityParkedStatuses   = model.IssueStatusesParked
)

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

// priorityMemberTerms are the issues that advance each live priority ($1, the live priority ids),
// the terms of a `with recursive` that both statements of the read open with.
//
// `members_seed` is the `advances` relation of every live priority as it stands now, one row per
// (issue, priority): the newest tuple of each pair, kept when it asserts the relation. It is
// bounded by the live ids rather than its answer: to_id is part of the `distinct on` key, so the
// two are the same rows and the narrowing cuts what the read sorts. A priority whose state is
// `gone` is no priority, and the work under it advances none.
//
// `members_spread` descends from each stated issue down the parent links: an issue advances a
// priority when its own row names it or when any issue on its parent chain does, written as one
// descent per request rather than one climb per issue, which is the shape issueAdvancesFilterTerms
// gives the same rule for the listing. A relation on a child places nothing above it, so nothing
// climbs. The descent is bounded by parentDepthCap, which no chain passes (refuseParentTooDeep), so
// a descendant the cap cuts off is one no chain could have reached anyway, and a raced reparent
// that ever committed a cycle still terminates. Each step joins issues on parent_key, which the
// planner hashes from one read of the table.
//
// `members` is distinct because an issue inherits the same priority from every issue above it
// that states it. It is materialized: every part of the full read takes its rows, so the walk runs
// once for the whole read.
var priorityMemberTerms = `members_seed as (
		select newest.from_id as key, newest.to_id as priority
		from (
			` + prioritystore.AdvancesTupleRead(`, t.asserted`, `and t.to_id = any($1::text[])`) + `
		) newest
		where newest.asserted
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m join issues c on c.parent_key = m.key
		where m.depth < ` + parentDepthCapSQL + `
	), members as materialized (
		select distinct key, priority from members_spread
	)`

// priorityProjectFilter narrows a read to the projects the request named ($2), over the issue
// aliased `i`; a request that named none ($2 empty) reads every project. It narrows the counts and
// the two fault lists and never the list of priorities, which belongs to no project.
const priorityProjectFilter = `(cardinality($2::text[]) = 0 or i.project_key = any($2::text[]))`

// priorityFaultRowColumns are the columns of a fault row, as the jsonb object pairs that
// priorityFaultRow decodes, over the issue aliased `i`.
const priorityFaultRowColumns = `'key', i.key, 'title', i.title, 'project', i.project_key, 'status', i.status,
			'assignee', i.assignee, 'claimed_by', i.claimed_by, 'claimed_at', i.claimed_at, 'changed_at', i.updated_at`

// The names EXPLAIN gives the parts of priorityReadQuery that a test holds on their own: the
// newest event's read of one event of each member issue, and the parked list's read of each
// listed issue's move.
const (
	priorityNewestEventCTE = "CTE member_event"
	priorityParkedListCTE  = "CTE parked_listed"
)

// priorityReadQuery is the whole body of the full read but the priorities themselves, as one
// statement over one walk of the member issues (priorityMemberTerms). Its one row holds four jsonb
// columns, one for each part of the body, which priorityReadAnswer decodes: the counts, the newest
// event under each priority, the first fault and the second. Every part reads one snapshot, so two
// parts cannot disagree about a table a write is moving.
//
// `member_issue` is the issues row of each member issue that the project filter keeps, read once
// and taken by the counts, the newest event and the parked work. It is the join of `members` to
// issues grouped by the key, which the planner hashes from one read of issues rather than probe
// issues_pkey for each member issue; the group by makes one row of each issue, whatever number of
// priorities it advances.
//
// `member_event` is the newest event of each member issue that has one (last_seq > 0): the time
// of its event of issues.last_seq. Each append sets last_seq to the seq it took, in the statement
// that inserts the event (events/broker.go, QueueAppendWithLogins), so that event is the issue's
// newest by seq. An issue with no event has last_seq 0 and answers no row; a priority none of
// whose issues has an event answers no row and keeps its null.
//
// The row answers where it can. The statement that advances last_seq writes the event's
// created_at and seq beside it, as last_event_at and last_event_at_seq (20261125094500,
// events/broker.go), so where last_event_at_seq equals last_seq the row holds the time of its
// event of last_seq, and the `case` takes it with no read of events. Where the two differ - an
// event a process of the release before 20261125094500 appended, during a roll or after a
// rollback, or a row 20261125094600 did not fill - or the pair is null, the row's pair names no
// event this read can trust, and the `else` arm probes the unique (issue_key, seq) of events
// (events_issue_key_seq_key) for that one event. A `case` runs its `else` only for the rows that
// reach it, so in steady state the probe never runs, and the answer is the probe's own wherever
// either arm answers, in steady state, in a roll and after a rollback alike. The probe reads that
// one event of each member issue and no other, and no event of an issue that advances no
// priority; the key is unique, so it answers at most one row, and a probe that finds none answers
// null, which max() passes over. A subquery in the select list is planned as a subplan of its
// own and never flattened into a join the planner may hash, as it hashed all of a million events
// for a plain join on (issue_key, seq).
//
// It is one select over member_issue rather than a union of the two arms: the planner reads no
// statistics of a column of a union, so it took the key of such a member_event for 200 distinct
// values and sized its join to members at 23,027 rows on the seed of R1, and under the plan made
// for the values it merged the two after sorting both by key, 22 ms of the full statement against
// 4.8 ms under the generic plan (dispatch://INFRA-5791, 2026-11-25). Over member_issue alone the
// key is that of member_issue's group by, which the planner takes for distinct, so both plans hash
// member_event, in about 4 ms. `materialized` is a fence: the planner plans the CTE on its own, so
// the join to `members` cannot move below the probe, and the probe runs once for each member issue
// rather than once for each (issue, priority) pair: without it, on the database cmd/dispatch-seed
// fills at --scale 1, the planner probed each member row, 14,712 probes for 6,362 member issues
// (TestStatementTimings, PostgreSQL 18.6, 2026-11-24).
//
// The time can be earlier than the newest created_at over all the events of the members, but only
// inside a bound. An event that Dispatch appends takes the start of its transaction as its
// created_at (the now() default of events.created_at, store/migrations/0001_init.up.sql). Each
// write of an issue holds the issue's lock from before it takes its seq until it commits
// (store.LockOwner, and the issue's row in lockEventOwner of events/broker.go), so the seq follows
// the order in which the writes of the issue commit, and the event of last_seq is that of the
// write that committed last. Its time is earlier than the time of another event of the issue only
// when its transaction began before that other write committed, and then it is earlier by less
// than the time from that start to that commit. Over the members of a priority, the difference is
// less than the longest such time. It reads the event and not issues.updated_at: the broker moves
// updated_at on every append, to the later of its value and the event's created_at, but a patch
// moves it too, to the clock time of its own statement (issueUpdatedNow), so updated_at is not the
// time of an event.
//
// `unplaced` is the first fault: an open issue in flight that advances no live priority, by its
// own relation or by one above it. `parked` is the second: a member issue that stands in `backlog`
// or `icebox`. `parked_listed` is the newest priorityFaultRows of them by the time each was moved
// there, with the time and the actor of that move (parkedMoveLateral), which is the one read the
// body makes over the events of the parked members alone. An issue whose events do not reach back
// to the move answers both cells null and stays in the list, since the fault is the work standing
// there and not the record of how it arrived. Each listed parked row carries its `advances`
// relations as every issue read answers them: the listed rows are the subjects of
// issueAdvancesTerms, the inheritance walk the issue read and the listing climb, and the row
// answers issueAdvancesColumns, which advancesScan turns into the entries, own and inherited.
//
// The counts are one row per (priority, status) over the issues that advance it: the open issues
// standing in that status, how many of those a session holds, the open asks on them, and the ones
// that closed inside priorityClosedWindow. A closed issue counts in nothing else, so a status
// whose issues are all closed answers 0 open and its closed count. The open-ask read keeps its
// state filter in its own where clause, so it matches the partial asks_open(issue_key) where
// state = 'open' index rather than scanning asks, as the listing's own open-ask read does
// (issueSummaryJoins).
//
// Each fault answers its whole count, its counts by project or by priority, and its newest
// priorityFaultRows rows; the rows are cut by their order with the key's bytes breaking a tie
// (`collate "C"`), which is the order priorityFault.resolve sorts them in again.
var priorityReadQuery = `with recursive ` + priorityMemberTerms + `, member_issue as materialized (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at,
		       i.closed_at, i.last_seq, i.last_event_at, i.last_event_at_seq
		from members m
		join issues i on i.key = m.key
		where ` + priorityProjectFilter + `
		group by i.key
	), member_event as materialized (
		select member_issue.key,
		       case when member_issue.last_event_at_seq = member_issue.last_seq then member_issue.last_event_at
		            else (select last_seq_event.created_at
		                  from events last_seq_event
		                  where last_seq_event.issue_key = member_issue.key and last_seq_event.seq = member_issue.last_seq)
		       end as created_at
		from member_issue
		where member_issue.last_seq > 0
	), unplaced as (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at
		from issues i
		where i.closed_at is null
			and i.status in (` + sqlStringList(priorityInFlightStatuses) + `)
			and ` + priorityProjectFilter + `
			and not exists (select 1 from members m where m.key = i.key)
	), parked as (
		select p.key, p.title, p.project_key, p.status, p.assignee, p.claimed_by, p.claimed_at, p.updated_at
		from member_issue p
		where p.closed_at is null
			and p.status in (` + sqlStringList(priorityParkedStatuses) + `)
	), parked_listed as (
		select p.*, moved.created_at as moved_at, moved.actor as moved_by
		from parked p
		` + parkedMoveLateral("p", "") + `
		order by moved.created_at desc nulls last, p.key collate "C" asc
		limit ` + strconv.Itoa(priorityFaultRows) + `
	), subjects as (
		select key from parked_listed
	), ` + issueAdvancesTerms + `
	select
		(select coalesce(jsonb_agg(jsonb_build_object(
		        'priority', counted.priority, 'status', counted.status, 'open', counted.open,
		        'claimed', counted.claimed, 'asks', counted.asks, 'closed', counted.closed)), '[]'::jsonb)
		 from (
			select m.priority, i.status,
			       count(*) filter (where i.closed_at is null) as open,
			       count(*) filter (where i.closed_at is null and i.claimed_by is not null) as claimed,
			       coalesce(sum(open_asks.total) filter (where i.closed_at is null), 0)::bigint as asks,
			       count(*) filter (where i.closed_at >= now() - interval '` + priorityClosedWindow + `') as closed
			from members m
			join member_issue i on i.key = m.key
			left join (
				select a.issue_key, count(*) as total
				from asks a
				where a.state = 'open' and a.issue_key in (select key from member_issue)
				group by a.issue_key
			) open_asks on open_asks.issue_key = i.key
			group by m.priority, i.status
		 ) counted) as counts,
		(select coalesce(jsonb_agg(jsonb_build_object('priority', newest.priority, 'at', newest.newest_at)), '[]'::jsonb)
		 from (
			select m.priority, max(member_event.created_at) as newest_at
			from members m
			join member_event on member_event.key = m.key
			group by m.priority
		 ) newest) as newest_events,
		jsonb_build_object(
			'total', (select count(*) from unplaced),
			'counts', (select coalesce(jsonb_object_agg(counted.project_key, counted.total), '{}'::jsonb)
			           from (select project_key, count(*) as total from unplaced group by project_key) counted),
			'rows', (select coalesce(jsonb_agg(jsonb_build_object(` + priorityFaultRowColumns + `)), '[]'::jsonb)
			         from (
			           select * from unplaced
			           order by updated_at desc, key collate "C" asc
			           limit ` + strconv.Itoa(priorityFaultRows) + `
			         ) i)) as unplaced,
		jsonb_build_object(
			'total', (select count(*) from parked),
			'counts', (select coalesce(jsonb_object_agg(counted.priority, counted.total), '{}'::jsonb)
			           from (
			             select m.priority, count(*) as total
			             from members m join parked p on p.key = m.key
			             group by m.priority
			           ) counted),
			'rows', (select coalesce(jsonb_agg(jsonb_build_object(` + priorityFaultRowColumns + `,
			           'advances', jsonb_build_array(` + issueAdvancesColumns + `),
			           'moved_at', i.moved_at, 'moved_by', i.moved_by)), '[]'::jsonb)
			         from parked_listed i
			         ` + issueAdvancesJoin + `)) as parked`

// prioritySummaryQuery is the whole summary: the two fault counts the navigation rail reads, from
// one scan of the open issues that stand in flight or parked, each joined to the member issues of
// the one walk (priorityMemberTerms). An issue in flight that no member is counts under the first
// fault, and a parked issue that a member is under the second. The keys of `members` are distinct
// before the join, so an issue that advances several priorities counts once.
var prioritySummaryQuery = `with recursive ` + priorityMemberTerms + `
	select count(*) filter (where i.status in (` + sqlStringList(priorityInFlightStatuses) + `) and member_key.key is null),
	       count(*) filter (where i.status in (` + sqlStringList(priorityParkedStatuses) + `) and member_key.key is not null)
	from issues i
	left join (select distinct key from members) member_key on member_key.key = i.key
	where i.closed_at is null
		and i.status in (` + sqlStringList(slices.Concat(priorityInFlightStatuses, priorityParkedStatuses)) + `)
		and ` + priorityProjectFilter

// parkedMoveLateral reads when the issue aliased `issue` was moved into `backlog` or `icebox` and
// who moved it, as `moved.created_at` and `moved.actor`: that issue's own newest `issue.updated`
// event whose payload names `status` in `changed` and whose status is one of the two. It is the
// one reading of the move, which the second fault (priorityReadQuery's parked_listed) and the
// listing's `?moved=true` (issueSummaryJoins) both splice, so the two answer the same cells for
// one issue. An issue whose events do not reach back to the move answers both null.
//
// The lateral walks the partial index events_parked_moves (migration 20261124005151), whose
// predicate is the lateral's own conditions on the event, backwards from the issue's newest move
// and stops at the first row, so a read costs the issues it is asked for rather than the table.
// gate, where it is not empty, is a condition the read runs under at all; it is written against
// the request's parameters and the outer issue alone, so the planner makes it a one-time filter
// above the index scan and a row it is false for reads no event.
func parkedMoveLateral(issue, gate string) string {
	if gate != "" {
		gate += `
			and `
	}
	return `left join lateral (
			select e.created_at, e.actor
			from events e
			where ` + gate + `e.issue_key = ` + issue + `.key and e.type = 'issue.updated'
				and e.payload->'changed' @> '"status"'::jsonb
				and e.payload->>'status' in (` + sqlStringList(priorityParkedStatuses) + `)
			order by e.seq desc
			limit 1
		) moved on true`
}

// readPriorities answers the whole body of GET /api/v1/priorities: the priorities in rank order,
// the work under each, and the two faults, narrowed to `projects` where it is narrowed at all.
//
// It is two round trips: prioritystore.List, then the one statement of the form asked for, which
// takes the live priority ids and reads their relations itself. A summary reads no issue of a
// priority that has no fault under it beyond the walk, and fills no priority's row. A deployment
// that has written no priority sends an empty list of ids, so the walk starts from no relation and
// every open issue in flight is work under no priority.
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

	if summary {
		if err := s.deps.Store.Pool.QueryRow(ctx, prioritySummaryQuery, ids, projects).
			Scan(&body.WorkUnderNoPriority.Count, &body.PriorityWorkParked.Count); err != nil {
			return model.Priorities{}, fmt.Errorf("count the work under no priority and the parked work: %w", err)
		}
		return body, nil
	}
	var answer priorityReadAnswer
	if err := s.deps.Store.Pool.QueryRow(ctx, priorityReadQuery, ids, projects).Scan(answer.targets()...); err != nil {
		return model.Priorities{}, fmt.Errorf("read the work under the priorities: %w", err)
	}
	// Each priority is placed by its index in the answer's own slice rather than by a pointer
	// into it, which the appends below would leave behind the moment the slice grew.
	at := make(map[string]int, len(nodes))
	for _, node := range nodes {
		at[node.ID] = len(body.Priorities)
		body.Priorities = append(body.Priorities, model.PriorityRow{
			Priority: priorityFrom(node),
			Work:     model.PriorityWork{ByStatus: zeroPriorityStatusCounts()},
		})
	}
	answer.fill(&body, at)
	return body, nil
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

// priorityReadAnswer is priorityReadQuery's one row: its four jsonb columns, each decoded as the
// statement wrote it.
type priorityReadAnswer struct {
	counts   []priorityStatusCount
	newest   []priorityNewestEvent
	unplaced priorityFault
	parked   priorityFault
}

// targets are the row's scan targets, in the statement's column order.
func (a *priorityReadAnswer) targets() []any {
	return []any{&a.counts, &a.newest, &a.unplaced, &a.parked}
}

// priorityStatusCount is one row of the counts: the work under one priority that stands in one
// status.
type priorityStatusCount struct {
	Priority string `json:"priority"`
	Status   string `json:"status"`
	Open     int    `json:"open"`
	Claimed  int    `json:"claimed"`
	Asks     int    `json:"asks"`
	Closed   int    `json:"closed"`
}

// priorityNewestEvent is the newest event under one priority. A priority no issue advances
// answers no row and keeps its null.
type priorityNewestEvent struct {
	Priority string     `json:"priority"`
	At       *time.Time `json:"at"`
}

// fill places the answer on body, whose priorities `at` places by their id. A row naming a
// priority the list does not hold is dropped: the list and the statement are one snapshot each,
// and a priority deleted between them is one this answer no longer lists. Every time comes back
// in UTC, as the pool scans every timestamptz (store.scanTimestampsInUTC), whatever zone the
// session wrote it in.
func (a priorityReadAnswer) fill(body *model.Priorities, at map[string]int) {
	for _, counted := range a.counts {
		index, listed := at[counted.Priority]
		if !listed {
			continue
		}
		row := &body.Priorities[index]
		row.Work.ByStatus[counted.Status] += counted.Open
		row.Work.Claimed += counted.Claimed
		row.Work.OpenAsks += counted.Asks
		row.Work.ClosedLast7Days += counted.Closed
	}
	for _, newest := range a.newest {
		if index, listed := at[newest.Priority]; listed {
			body.Priorities[index].Work.NewestEventAt = inUTC(newest.At)
		}
	}
	count, counts, issues := a.unplaced.resolve(false)
	body.WorkUnderNoPriority = model.WorkUnderNoPriority{Count: count, ByProject: counts, Issues: issues}
	count, counts, issues = a.parked.resolve(true)
	body.PriorityWorkParked = model.PriorityWorkParked{Count: count, ByPriority: counts, Issues: issues}
}

// inUTC is the time t names, in UTC, and nil for none.
func inUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// priorityFault is one fault as the statement answers it: the whole count, the counts by project
// or by priority, and the newest rows.
type priorityFault struct {
	Total  int                `json:"total"`
	Counts map[string]int     `json:"counts"`
	Rows   []priorityFaultRow `json:"rows"`
}

// priorityFaultRow is one row of a fault list as the statement answers it. The parked fault
// carries five values more: the three `advances` columns every issue read answers
// (issueAdvancesColumns), as one array, and the two cells of the move into `backlog` or `icebox`.
// The first fault carries none of them: its rows advance no live priority, so their `advances` is
// the empty list a zero advancesScan answers.
type priorityFaultRow struct {
	Key       string                `json:"key"`
	Title     string                `json:"title"`
	Project   string                `json:"project"`
	Status    string                `json:"status"`
	Assignee  *string               `json:"assignee"`
	ClaimedBy *model.Actor          `json:"claimed_by"`
	ClaimedAt *time.Time            `json:"claimed_at"`
	ChangedAt time.Time             `json:"changed_at"`
	Advances  priorityFaultAdvances `json:"advances"`
	MovedAt   *time.Time            `json:"moved_at"`
	MovedBy   *model.Actor          `json:"moved_by"`
}

// priorityFaultAdvances is a parked row's `advances` columns (issueAdvancesColumns), which the
// statement answers as one array of the three in advancesScan's order.
type priorityFaultAdvances advancesScan

func (a *priorityFaultAdvances) UnmarshalJSON(raw []byte) error {
	var columns []json.RawMessage
	if err := json.Unmarshal(raw, &columns); err != nil {
		return err
	}
	targets := (*advancesScan)(a).targets()
	if len(columns) != len(targets) {
		return fmt.Errorf("the advances of a parked row hold %d values, want %d", len(columns), len(targets))
	}
	for index, target := range targets {
		if err := json.Unmarshal(columns[index], target); err != nil {
			return err
		}
	}
	return nil
}

// resolve turns a row into the issue the fault list carries. An issue's claim is both of its
// claim columns or none (the issues_claim_complete constraint).
func (r priorityFaultRow) resolve() model.PriorityIssue {
	issue := model.PriorityIssue{
		Key:       r.Key,
		Title:     r.Title,
		Project:   r.Project,
		Status:    r.Status,
		Assignee:  r.Assignee,
		ChangedAt: r.ChangedAt.UTC(),
		Advances:  (*advancesScan)(&r.Advances).resolve(),
		MovedAt:   inUTC(r.MovedAt),
		MovedBy:   r.MovedBy,
	}
	if r.ClaimedBy != nil && r.ClaimedAt != nil {
		issue.Claim = &model.IssueClaim{Actor: *r.ClaimedBy, At: r.ClaimedAt.UTC()}
	}
	return issue
}

// resolve reads one fault: its whole count, its grouped counts and its rows.
//
// The rows are ordered again here, by the columns the statement cut its 50 on: jsonb_agg keeps
// no order of its own, so the order the answer carries is this one rather than the plan's. The
// key breaks a tie in both by its bytes, which is what strings.Compare reads and what the
// statement's `collate "C"` names, so the 50 the statement cut are the first 50 of this order
// whatever the database's collation. Left to the default collation, which on a deployment weighs
// punctuation last, the cut and this sort would be two orders, and where a tie straddles the cut
// (the parked issues whose move no event records all tie) the list would draw a row the order it
// answers puts after one it leaves out.
func (f priorityFault) resolve(parked bool) (int, map[string]int, []model.PriorityIssue) {
	issues := make([]model.PriorityIssue, 0, len(f.Rows))
	for _, row := range f.Rows {
		issues = append(issues, row.resolve())
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
	return f.Total, f.Counts, issues
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
