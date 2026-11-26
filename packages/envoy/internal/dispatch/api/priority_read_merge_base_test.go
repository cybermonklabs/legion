//go:build !race

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The read below this change (R2 of dispatch://INFRA-5791): GET /api/v1/priorities as
// readPriorities answered it at prioritiesReadMergeBase, the commit this branch is built on,
// frozen here letter for letter. It holds readPriorities and readPriorityRelations, the text of
// every statement they send, prioritystore.List's among them, every term those texts splice
// (priorityRelationSeed, priorityMemberTerms, priorityProjectFilter, advancesOriginTerms,
// backlogMoveLateral, issueAdvancesTerms, prioritystore.AdvancesTupleRead, the status lists, the
// depth cap and the window), and every function that scans their answers (scanPriorityCounts,
// scanPriorityNewestEvents, queuePriorityFault, scanPriorityFault with its row, claimScan,
// advancesScan, prioritystore's scanNode and priorityFrom). Each is that commit's code with its
// name prefixed by mergeBase and each constant it read written out as the value it held there.
// Nothing here reads the package's own constants, which move with the change and would carry a
// break into both sides. When the base of this branch moves, the copy is frozen again at the new
// base.
//
// The copy is read once, by TestThePrioritiesReadAnswersAsTheMergeBaseAnswers.

// prioritiesReadMergeBase is the commit the copy below is frozen at: b94d073e, the head of
// cybermonklabs/legion#5316 (branch legion/INFRA-4865), on which this branch is built. It merged
// main's #4316, #5302 and #5361 and the second fault's rule that an issue in backlog or icebox is
// a fault only where it states a priority no issue above it carries (priorityBacklogQuery there).
// The read's statements and the terms they splice (priorities.go, priority_queries.go,
// prioritystore, issueAdvancesTerms and the status lists) are the same text at it as at
// d7528aa8, where the copy was first frozen; only their comments differ.
const prioritiesReadMergeBase = "b94d073eb23d9a9ddf044e9339ad15cd9a5514a6"

var (
	mergeBasePriorityInFlightStatuses = []string{"in_progress", "testing", "needs_review", "retro"}
	mergeBasePriorityBacklogStatuses  = []string{"icebox", "backlog"}
)

// mergeBaseIssueStatuses is model.IssueStatuses at the merge base, which
// mergeBaseZeroPriorityStatusCounts reads.
var mergeBaseIssueStatuses = []string{"triage", "icebox", "backlog", "todo", "in_progress", "testing", "needs_review", "retro", "done"}

const mergeBasePriorityFaultRows = 50

const mergeBasePriorityClosedWindow = "7 days"

const mergeBasePriorityNoneWritten = "No priority is live now, so the list is empty. Write one, then say which work advances it."

// mergeBaseParentDepthCapSQL is parentDepthCapSQL at the merge base: refs.IssueDepthCap, 32.
const mergeBaseParentDepthCapSQL = "32"

func mergeBaseSQLStringList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "'" + value + "'"
	}
	return strings.Join(quoted, ", ")
}

// mergeBaseAdvancesTupleRead is prioritystore.AdvancesTupleRead at the merge base, with
// KindIssue, RelationAdvances and KindPriority written out.
func mergeBaseAdvancesTupleRead(columns, narrow string) string {
	return `select distinct on (t.from_id, t.to_id) t.from_id, t.to_id` + columns + `
		from priority_tuples t
		where t.from_kind = 'issue' and t.kind = 'advances'
			and t.to_kind = 'priority'
			` + narrow + `
		order by t.from_id, t.to_id, t.seq desc`
}

// mergeBasePriorityListQuery is prioritystore.List's statement at the merge base, with
// nodeColumns, KindPriority and rankKey written out; its one argument is StateLive.
const mergeBasePriorityListQuery = `
		select kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event
		from priority_nodes
		where kind = 'priority' and state = $1
		order by attributes->>'rank' collate "C", id
	`

var mergeBasePriorityRelationsQuery = `select newest.from_id, newest.to_id
	from (
		` + mergeBaseAdvancesTupleRead(`, t.asserted`, `and t.to_id = any($1::text[])`) + `
	) newest
	where newest.asserted
	order by newest.from_id, newest.to_id`

const mergeBasePriorityRelationSeed = `select ($1::text[])[i] as key, ($2::text[])[i] as priority
		from generate_subscripts($1::text[], 1) as seed(i)`

var mergeBasePriorityMemberTerms = `members_seed as (
		` + mergeBasePriorityRelationSeed + `
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m join issues c on c.parent_key = m.key
		where m.depth < ` + mergeBaseParentDepthCapSQL + `
	), members as (
		select distinct key, priority from members_spread
	)`

const mergeBasePriorityProjectFilter = `(cardinality($3::text[]) = 0 or i.project_key = any($3::text[]))`

var mergeBasePriorityCountsQuery = `with recursive ` + mergeBasePriorityMemberTerms + `
	select m.priority, i.status,
	       count(*) filter (where i.closed_at is null) as open,
	       count(*) filter (where i.closed_at is null and i.claimed_by is not null) as claimed,
	       coalesce(sum(open_asks.total) filter (where i.closed_at is null), 0)::bigint as asks,
	       count(*) filter (where i.closed_at >= now() - interval '` + mergeBasePriorityClosedWindow + `') as closed
	from members m
	join issues i on i.key = m.key
	left join (
		select a.issue_key, count(*) as total
		from asks a
		where a.state = 'open' and a.issue_key in (select key from members)
		group by a.issue_key
	) open_asks on open_asks.issue_key = i.key
	where ` + mergeBasePriorityProjectFilter + `
	group by m.priority, i.status`

var mergeBasePriorityNewestEventQuery = `with recursive ` + mergeBasePriorityMemberTerms + `, member_event as materialized (
		select member_issue.key, last_seq_event.created_at
		from (select distinct key from members) member_issue
		cross join lateral (
			select i.last_seq, i.project_key from issues i where i.key = member_issue.key offset 0
		) i
		cross join lateral (
			select last_seq_event.created_at
			from events last_seq_event
			where last_seq_event.issue_key = member_issue.key and last_seq_event.seq = i.last_seq
			limit 1
		) last_seq_event
		where ` + mergeBasePriorityProjectFilter + `
	)
	select m.priority, max(member_event.created_at)
	from members m
	join member_event on member_event.key = m.key
	group by m.priority`

func mergeBasePriorityUnplacedQuery(summary bool) string {
	faulted := `with recursive ` + mergeBasePriorityMemberTerms + `, unplaced as (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at
		from issues i
		where i.closed_at is null
			and i.status in (` + mergeBaseSQLStringList(mergeBasePriorityInFlightStatuses) + `)
			and ` + mergeBasePriorityProjectFilter + `
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
		order by updated_at desc, key collate "C" asc
		limit ` + strconv.Itoa(mergeBasePriorityFaultRows) + `
	) listed on true`
}

func mergeBasePriorityBacklogQuery(summary bool) string {
	faulted := `with recursive stated as (
		` + mergeBasePriorityRelationSeed + `
	), backlog_keys as (
		select distinct s.key
		from stated s join issues i on i.key = s.key
		where i.closed_at is null
			and i.status in (` + mergeBaseSQLStringList(mergeBasePriorityBacklogStatuses) + `)
			and ` + mergeBasePriorityProjectFilter + `
	), ` + mergeBaseAdvancesOriginTerms("stated", "backlog_keys") + `, in_backlog as (
		select i.key, i.title, i.project_key, i.status, i.assignee, i.claimed_by, i.claimed_at, i.updated_at
		from issues i
		where i.key in (select key from advances_origins)
	)`
	if summary {
		return faulted + `
	select count(*) from in_backlog`
	}
	return faulted + `, listed as (
		select p.*, moved.created_at as moved_at, moved.actor as moved_by
		from in_backlog p
		` + mergeBaseBacklogMoveLateral("p", "") + `
		order by moved.created_at desc nulls last, p.key collate "C" asc
		limit ` + strconv.Itoa(mergeBasePriorityFaultRows) + `
	), subjects as (
		select key from listed
	), ` + mergeBaseIssueAdvancesTerms + `
	select (select count(*) from in_backlog) as total,
	       (select coalesce(jsonb_object_agg(counted.priority, counted.total), '{}'::jsonb)
	        from (
	          select o.priority, count(*) as total
	          from advances_origins o
	          group by o.priority
	        ) counted) as by_priority,
	       i.key, i.title, i.project_key, i.status, i.assignee,
	       i.claimed_by, i.claimed_at, i.updated_at,
	       ` + mergeBaseIssueAdvancesColumns + `, i.moved_at, i.moved_by
	from (select 1 as one) seed
	left join listed i on true
	` + mergeBaseIssueAdvancesJoin
}

// mergeBaseAdvancesOriginTerms is advancesOriginTerms at the merge base, with parentDepthCapSQL
// written out.
func mergeBaseAdvancesOriginTerms(relations, keys string) string {
	return `advances_above as (
		select k.key, i.parent_key as above, 1 as depth
		from ` + keys + ` k join issues i on i.key = k.key
		where i.parent_key is not null
		union all
		select a.key, p.parent_key, a.depth + 1
		from advances_above a join issues p on p.key = a.above
		where p.parent_key is not null and a.depth < ` + mergeBaseParentDepthCapSQL + `
	), advances_origins as (
		select r.key, r.priority
		from ` + relations + ` r
		where r.key in (select key from ` + keys + `)
			and not exists (
				select 1
				from advances_above a
				join ` + relations + ` up on up.key = a.above and up.priority = r.priority
				where a.key = r.key
			)
	)`
}

func mergeBaseBacklogMoveLateral(issue, gate string) string {
	if gate != "" {
		gate += `
			and `
	}
	return `left join lateral (
			select e.created_at, e.actor
			from events e
			where ` + gate + `e.issue_key = ` + issue + `.key and e.type = 'issue.updated'
				and e.payload->'changed' @> '"status"'::jsonb
				and e.payload->>'status' in (` + mergeBaseSQLStringList(mergeBasePriorityBacklogStatuses) + `)
			order by e.seq desc
			limit 1
		) moved on true`
}

const mergeBaseIssueAdvancesColumns = `coalesce(advances.ids, '{}'), coalesce(advances.revision, 0), coalesce(advances.inherited, '{}')`

// mergeBaseIssueAdvancesTerms is issueAdvancesTerms at the merge base, with KindPriority and
// StateLive written out.
var mergeBaseIssueAdvancesTerms = `advances_chain as (
		select s.key as start, s.key as issue_key, 0 as depth
		from subjects s
		union all
		select c.start, p.parent_key, c.depth + 1
		from advances_chain c join issues p on p.key = c.issue_key
		where p.parent_key is not null and c.depth < ` + mergeBaseParentDepthCapSQL + `
	), advances_current as (
		` + mergeBaseAdvancesTupleRead(`, t.seq, t.asserted`, `and t.from_id in (select issue_key from advances_chain)`) + `
	), advances as (
		select c.start as issue_key,
		       coalesce(array_agg(distinct cu.to_id collate "C" order by cu.to_id collate "C")
		                filter (where cu.asserted and live.id is not null and c.depth = 0), '{}') as ids,
		       coalesce(max(cu.seq) filter (where c.depth = 0), 0) as revision,
		       coalesce(array_agg(distinct cu.to_id collate "C" order by cu.to_id collate "C")
		                filter (where cu.asserted and live.id is not null and c.depth > 0), '{}') as inherited
		from advances_chain c
		left join advances_current cu on cu.from_id = c.issue_key
		left join priority_nodes live on live.kind = 'priority' and live.id = cu.to_id
			and live.state = 'live'
		group by c.start
	)`

const mergeBaseIssueAdvancesJoin = `left join advances on advances.issue_key = i.key`

var (
	mergeBaseUnplacedQuery        = mergeBasePriorityUnplacedQuery(false)
	mergeBaseUnplacedSummaryQuery = mergeBasePriorityUnplacedQuery(true)
	mergeBaseBacklogQuery         = mergeBasePriorityBacklogQuery(false)
	mergeBaseBacklogSummaryQuery  = mergeBasePriorityBacklogQuery(true)
)

func (s *server) mergeBaseReadPriorities(ctx context.Context, projects []string, summary bool) (model.Priorities, error) {
	nodes, err := mergeBaseListPriorities(ctx, s.deps.Store.Pool)
	if err != nil {
		return model.Priorities{}, err
	}
	body := model.Priorities{
		Priorities:          make([]model.PriorityRow, 0, len(nodes)),
		WorkUnderNoPriority: model.WorkUnderNoPriority{ByProject: map[string]int{}, Issues: []model.PriorityIssue{}},
		PriorityWorkParked:  model.PriorityWorkParked{ByPriority: map[string]int{}, Issues: []model.PriorityIssue{}},
	}
	if len(nodes) == 0 {
		body.Empty = mergeBasePriorityNoneWritten
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	seedKeys, seedIDs := []string{}, []string{}
	if len(nodes) > 0 {
		seedKeys, seedIDs, err = s.mergeBaseReadPriorityRelations(ctx, ids)
		if err != nil {
			return model.Priorities{}, err
		}
	}
	arguments := []any{seedKeys, seedIDs, projects}

	batch := &pgx.Batch{}
	if !summary {
		at := make(map[string]int, len(nodes))
		for _, node := range nodes {
			at[node.ID] = len(body.Priorities)
			body.Priorities = append(body.Priorities, model.PriorityRow{
				Priority: mergeBasePriorityFrom(node),
				Work:     model.PriorityWork{ByStatus: mergeBaseZeroPriorityStatusCounts()},
			})
		}
		batch.Queue(mergeBasePriorityCountsQuery, arguments...).Query(func(scanned pgx.Rows) error {
			return mergeBaseScanPriorityCounts(scanned, body.Priorities, at)
		})
		batch.Queue(mergeBasePriorityNewestEventQuery, arguments...).Query(func(scanned pgx.Rows) error {
			return mergeBaseScanPriorityNewestEvents(scanned, body.Priorities, at)
		})
	}
	mergeBaseQueuePriorityFault(batch, mergeBaseUnplacedQuery, mergeBaseUnplacedSummaryQuery, summary, arguments, false,
		func(count int, counts map[string]int, issues []model.PriorityIssue) {
			body.WorkUnderNoPriority.Count = count
			if counts != nil {
				body.WorkUnderNoPriority.ByProject = counts
			}
			body.WorkUnderNoPriority.Issues = issues
		})
	mergeBaseQueuePriorityFault(batch, mergeBaseBacklogQuery, mergeBaseBacklogSummaryQuery, summary, arguments, true,
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

func (s *server) mergeBaseReadPriorityRelations(ctx context.Context, ids []string) ([]string, []string, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, mergeBasePriorityRelationsQuery, ids)
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

// mergeBaseListPriorities is prioritystore.List at the merge base.
func mergeBaseListPriorities(ctx context.Context, q prioritystore.Queryer) ([]prioritystore.Node, error) {
	rows, err := q.Query(ctx, mergeBasePriorityListQuery, "live")
	if err != nil {
		return nil, fmt.Errorf("read the live priorities: %w", err)
	}
	defer rows.Close()
	var nodes []prioritystore.Node
	for rows.Next() {
		node, err := mergeBaseScanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live priorities: %w", err)
	}
	return nodes, nil
}

// mergeBaseScanNode is prioritystore's scanNode at the merge base.
func mergeBaseScanNode(row pgx.Row) (prioritystore.Node, error) {
	var node prioritystore.Node
	var attributes, madeBy []byte
	if err := row.Scan(&node.Kind, &node.ID, &node.Supertag, &node.Visibility, &node.State,
		&attributes, &node.MadeAt, &madeBy, &node.SourceEvent); err != nil {
		return prioritystore.Node{}, err
	}
	if err := json.Unmarshal(attributes, &node.Attributes); err != nil {
		return prioritystore.Node{}, fmt.Errorf("decode attributes of priority %s: %w", node.ID, err)
	}
	if err := json.Unmarshal(madeBy, &node.MadeBy); err != nil {
		return prioritystore.Node{}, fmt.Errorf("decode writer of priority %s: %w", node.ID, err)
	}
	return node, nil
}

// mergeBasePriorityFrom is priorityFrom at the merge base, with the attribute names written out.
func mergeBasePriorityFrom(node prioritystore.Node) model.Priority {
	title, _ := node.Attribute("title")
	description, _ := node.Attribute("description")
	owner, _ := node.Attribute("owner")
	order, _ := node.Attribute("rank")
	return model.Priority{
		ID:          node.ID,
		Title:       title,
		Description: description,
		Owner:       owner,
		Rank:        order,
		State:       node.State,
		MadeAt:      node.MadeAt,
		MadeBy:      node.MadeBy,
	}
}

func mergeBaseZeroPriorityStatusCounts() map[string]int {
	counts := make(map[string]int, len(mergeBaseIssueStatuses))
	for _, status := range mergeBaseIssueStatuses {
		counts[status] = 0
	}
	return counts
}

func mergeBaseScanPriorityCounts(rows pgx.Rows, into []model.PriorityRow, at map[string]int) error {
	for rows.Next() {
		var id, status string
		var open, claimed, asks, closed int
		if err := rows.Scan(&id, &status, &open, &claimed, &asks, &closed); err != nil {
			return fmt.Errorf("read the counts of a priority: %w", err)
		}
		index, listed := at[id]
		if !listed {
			continue
		}
		row := &into[index]
		row.Work.ByStatus[status] += open
		row.Work.Claimed += claimed
		row.Work.OpenAsks += asks
		row.Work.ClosedLast7Days += closed
	}
	return rows.Err()
}

func mergeBaseScanPriorityNewestEvents(rows pgx.Rows, into []model.PriorityRow, at map[string]int) error {
	for rows.Next() {
		var id string
		var newest *time.Time
		if err := rows.Scan(&id, &newest); err != nil {
			return fmt.Errorf("read the newest event under a priority: %w", err)
		}
		if index, listed := at[id]; listed {
			into[index].Work.NewestEventAt = newest
		}
	}
	return rows.Err()
}

func mergeBaseQueuePriorityFault(
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
		count, counts, issues, err := mergeBaseScanPriorityFault(rows, parked)
		if err != nil {
			return err
		}
		apply(count, counts, issues)
		return nil
	})
}

type mergeBasePriorityFaultRow struct {
	total     int
	counts    map[string]int
	key       *string
	title     *string
	project   *string
	status    *string
	assignee  *string
	claim     mergeBaseClaimScan
	changedAt *time.Time
	advances  mergeBaseAdvancesScan
	movedAt   *time.Time
	movedBy   []byte
}

func (r *mergeBasePriorityFaultRow) targets(parked bool) []any {
	targets := []any{&r.total, &r.counts, &r.key, &r.title, &r.project, &r.status, &r.assignee}
	targets = append(targets, r.claim.targets()...)
	targets = append(targets, &r.changedAt)
	if parked {
		targets = append(targets, r.advances.targets()...)
		targets = append(targets, &r.movedAt, &r.movedBy)
	}
	return targets
}

func (r *mergeBasePriorityFaultRow) resolve() (model.PriorityIssue, error) {
	claim, err := r.claim.resolve(*r.key)
	if err != nil {
		return model.PriorityIssue{}, err
	}
	issue := model.PriorityIssue{
		Key:       *r.key,
		Title:     *r.title,
		Project:   *r.project,
		Status:    *r.status,
		Assignee:  r.assignee,
		Claim:     claim,
		ChangedAt: *r.changedAt,
		Advances:  r.advances.resolve(),
		MovedAt:   r.movedAt,
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

func mergeBaseScanPriorityFault(rows pgx.Rows, parked bool) (int, map[string]int, []model.PriorityIssue, error) {
	count := 0
	counts := map[string]int{}
	issues := []model.PriorityIssue{}
	for rows.Next() {
		row := mergeBasePriorityFaultRow{}
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
			if order := mergeBaseComparePriorityTimes(a.MovedAt, b.MovedAt); order != 0 {
				return order
			}
		} else if order := b.ChangedAt.Compare(a.ChangedAt); order != 0 {
			return order
		}
		return strings.Compare(a.Key, b.Key)
	})
	return count, counts, issues, nil
}

func mergeBaseComparePriorityTimes(a, b *time.Time) int {
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

type mergeBaseClaimScan struct {
	actor []byte
	at    *time.Time
}

func (c *mergeBaseClaimScan) targets() []any { return []any{&c.actor, &c.at} }

func (c *mergeBaseClaimScan) resolve(key string) (*model.IssueClaim, error) {
	if len(c.actor) == 0 || c.at == nil {
		return nil, nil
	}
	claim := model.IssueClaim{At: *c.at}
	if err := json.Unmarshal(c.actor, &claim.Actor); err != nil {
		return nil, fmt.Errorf("decode issue %s claim actor: %w", key, err)
	}
	return &claim, nil
}

type mergeBaseAdvancesScan struct {
	ids       []string
	revision  int64
	inherited []string
}

func (a *mergeBaseAdvancesScan) targets() []any { return []any{&a.ids, &a.revision, &a.inherited} }

func (a *mergeBaseAdvancesScan) resolve() []model.IssueAdvance {
	entries := make([]model.IssueAdvance, 0, len(a.ids)+len(a.inherited))
	own, inherited := a.ids, a.inherited
	for len(own) > 0 || len(inherited) > 0 {
		if len(own) > 0 && (len(inherited) == 0 || own[0] <= inherited[0]) {
			if len(inherited) > 0 && inherited[0] == own[0] {
				inherited = inherited[1:]
			}
			entries = append(entries, model.IssueAdvance{ID: own[0], Inherited: false})
			own = own[1:]
			continue
		}
		entries = append(entries, model.IssueAdvance{ID: inherited[0], Inherited: true})
		inherited = inherited[1:]
	}
	return entries
}

// TestThePrioritiesReadAnswersAsTheMergeBaseAnswers holds the priorities read to R2 of
// dispatch://INFRA-5791: it answers, byte for byte, what the read below this change answers (the
// frozen copy above). On the corpus of production's counts (seedPrioritiesReadCorpus) and on a
// store that holds no live priority, it makes each of the six requests a client can send - the
// full read and the summary, each with no project, with the largest project and with the two
// largest named - of the server, and holds the bytes it writes to the bytes the copy's answer
// encodes to as the route encodes it. A comparison of two empty answers proves nothing, so on the
// corpus the copy's answer with no project named must hold the counts the corpus was built to
// hold (holdReadCorpusAnswer), and each named project must hold work under a priority.
//
// On the corpus it compares in each of the three states of the newest-event pairs
// (mergeBaseCorpusStates), one subtest each, in order: after the fill, every pair null, and after
// an append of the release before. Before the first, it holds the fill (holdNewestEventFill).
func TestThePrioritiesReadAnswersAsTheMergeBaseAnswers(t *testing.T) {
	t.Parallel()
	corpus := seedPrioritiesReadCorpus(t)
	t.Log(corpus.census)
	corpus.census.hold(t)
	requireReadCorpusJITOff(t, corpus.database, "the merge base's comparison")
	t.Logf("the read below this change is readPriorities at %s, frozen in priority_read_merge_base_test.go", prioritiesReadMergeBase)
	holdNewestEventFill(t, corpus.database)

	largest := readCorpusLargestProjects(t, corpus.database, 2)
	for _, state := range mergeBaseCorpusStates {
		t.Run(state.name, func(t *testing.T) {
			if state.setup != nil {
				state.setup(t, corpus.database)
			}
			compareReadsWithMergeBase(t, corpus.database, "the corpus "+state.name, [][]string{{}, largest[:1], largest}, func(form prioritiesReadForm, projects []string, want model.Priorities) {
				switch {
				case len(projects) == 0:
					holdReadCorpusAnswer(t, want, form, "the read below this change")
				case want.PriorityWorkParked.Count == 0:
					t.Errorf("%s with %s answers no parked work in the read below this change, so its comparison proves little", form.called, projectsNamed(projects))
				}
			})
		})
	}

	empty := seedNoLivePriority(t)
	compareReadsWithMergeBase(t, empty.database, "a store with no live priority", [][]string{{}, empty.projects[:1], empty.projects}, func(form prioritiesReadForm, projects []string, want model.Priorities) {
		if want.Empty != mergeBasePriorityNoneWritten || len(want.Priorities) != 0 {
			t.Errorf("%s with %s on a store with no live priority answers %d priorities and empty %q in the read below this change, want none and the sentence saying why",
				form.called, projectsNamed(projects), len(want.Priorities), want.Empty)
		}
		if want.WorkUnderNoPriority.Count == 0 {
			t.Errorf("%s with %s on a store with no live priority counts no work under none in the read below this change, so its comparison proves little", form.called, projectsNamed(projects))
		}
	})
}

// mergeBaseCorpusStates are the three states of the corpus's newest-event pairs that
// TestThePrioritiesReadAnswersAsTheMergeBaseAnswers compares the two reads in, each made by its
// setup from the one before:
//
//   - after the fill, the steady state, as the corpus is seeded: the read takes each newest event
//     from the issue row;
//   - every pair null, as on a store before 20261125144002 fills it: the read probes the event of
//     last_seq of each member issue;
//   - after an append of the release before: the fill again (fillNewestEventPairs), then, for one
//     member issue of each live priority, what QueueAppendWithLogins of the release before
//     20261125143901 writes (appendAsTheReleaseBefore). Its pair then names the seq before
//     last_seq, and its event of last_seq is the newest of every priority it advances, which only
//     the probe reads.
var mergeBaseCorpusStates = []struct {
	name  string
	setup func(t *testing.T, database *store.Store)
}{
	{"after the fill", nil},
	{"with every pair null", func(t *testing.T, database *store.Store) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `update issues set last_event_at = null, last_event_at_seq = null`); err != nil {
			t.Fatalf("null every newest-event pair: %v", err)
		}
	}},
	{"after an append of the release before", func(t *testing.T, database *store.Store) {
		t.Helper()
		fillNewestEventPairs(t, database)
		appendAsTheReleaseBefore(t, database)
	}},
}

// holdNewestEventFill holds the corpus to what 20261125144002's fill leaves, before any state is
// read: every issue with last_seq > 0 holds last_event_at_seq = last_seq and the created_at of its
// event of last_seq as last_event_at, and every issue with last_seq = 0 holds both null. The COPY
// wrote every pair null, so the issues whose pair is not null are the rows the fill wrote, which it
// prints. It fails on each row that breaks the rule, naming the first of them.
func holdNewestEventFill(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var wrote int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issues where last_event_at is not null or last_event_at_seq is not null`).Scan(&wrote); err != nil {
		t.Fatalf("count the rows the fill wrote: %v", err)
	}
	t.Logf("the fill wrote the newest-event pair of %d issues", wrote)
	rows, err := database.Pool.Query(ctx, `
		select i.key, i.last_seq, i.last_event_at_seq, i.last_event_at, e.created_at
		from issues i
		left join events e on e.issue_key = i.key and e.seq = i.last_seq
		where (i.last_seq > 0 and (i.last_event_at_seq is distinct from i.last_seq or i.last_event_at is distinct from e.created_at))
		   or (i.last_seq = 0 and (i.last_event_at is not null or i.last_event_at_seq is not null))
		order by i.key collate "C"
	`)
	if err != nil {
		t.Fatalf("read the pairs the fill left: %v", err)
	}
	var unnamed, unknown []string
	for rows.Next() {
		var key string
		var lastSeq int64
		var pairSeq *int64
		var pairAt, eventAt *time.Time
		if err := rows.Scan(&key, &lastSeq, &pairSeq, &pairAt, &eventAt); err != nil {
			rows.Close()
			t.Fatalf("read a pair the fill left: %v", err)
		}
		row := fmt.Sprintf("%s (last_seq %d, pair seq %v at %v, its event of last_seq at %v)", key, lastSeq, pointedTo(pairSeq), pointedTo(pairAt), pointedTo(eventAt))
		if lastSeq > 0 {
			unnamed = append(unnamed, row)
		} else {
			unknown = append(unknown, row)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the pairs the fill left: %v", err)
	}
	if len(unnamed) > 0 {
		t.Errorf("after the fill, %d issues with an event hold a pair that does not name their event of last_seq, want 0: %s", len(unnamed), firstRows(unnamed))
	}
	if len(unknown) > 0 {
		t.Errorf("after the fill, %d issues with no event hold a pair, want both null: %s", len(unknown), firstRows(unknown))
	}
}

// pointedTo is what p points to, and "null" for nil, as a failure names a column.
func pointedTo[T any](p *T) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// firstRows is the first ten of rows, and how many more there are.
func firstRows(rows []string) string {
	const shown = 10
	if len(rows) <= shown {
		return strings.Join(rows, "; ")
	}
	return strings.Join(rows[:shown], "; ") + fmt.Sprintf("; and %d more", len(rows)-shown)
}

// appendAsTheReleaseBefore writes, for one member issue of each live priority - the first by key
// that states its relation (priorityCorpusSeed) - what QueueAppendWithLogins of the release before
// 20261125143901 writes: an event at last_seq + 1 and the row's new last_seq and updated_at, in one
// statement, with the pair left as it was. Each event's created_at is later than every event of
// the corpus, one minute apart, so it is the newest event of every priority its issue advances.
// It holds that each such issue's pair then names the seq before its last_seq.
func appendAsTheReleaseBefore(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	ids := livePriorityIDs(t, database)
	seedKeys, seedIDs := priorityCorpusSeed(t, database, ids)
	picked := map[string]bool{}
	var appended []string
	for index, key := range seedKeys {
		if picked[seedIDs[index]] {
			continue
		}
		picked[seedIDs[index]] = true
		if slices.Contains(appended, key) {
			continue
		}
		appended = append(appended, key)
	}
	if len(picked) != len(ids) {
		t.Fatalf("the corpus states a relation for %d of its %d live priorities, want each", len(picked), len(ids))
	}
	var newest time.Time
	if err := database.Pool.QueryRow(ctx, `select max(created_at) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the corpus's newest event: %v", err)
	}
	for minutes, key := range appended {
		at := newest.Add(time.Duration(minutes+1) * time.Minute)
		if _, err := database.Pool.Exec(ctx, `
			with appended as (
				insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
				select i.key, i.last_seq + 1, 'comment.created', '{"kind":"user","id":"alice"}'::jsonb, '{}'::jsonb, false, $2::timestamptz, $2::timestamptz
				from issues i
				where i.key = $1
				returning issue_key, seq, created_at
			)
			update issues set last_seq = appended.seq, updated_at = greatest(issues.updated_at, appended.created_at)
			from appended where issues.key = appended.issue_key
		`, key, at); err != nil {
			t.Fatalf("append to %s as the release before does: %v", key, err)
		}
	}
	var behind int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issues where key = any($1::text[]) and last_event_at_seq = last_seq - 1`, appended).Scan(&behind); err != nil {
		t.Fatalf("read the appended issues' pairs: %v", err)
	}
	if behind != len(appended) {
		t.Fatalf("after an append of the release before to %d issues, %d hold a pair a seq behind last_seq, want each", len(appended), behind)
	}
	t.Logf("appended an event as the release before does to %d member issues, one for each of the %d live priorities: %v", len(appended), len(ids), appended)
}

// compareReadsWithMergeBase makes each form of the read with each list of projects of the
// server over database, and holds the bytes it writes to those of the frozen copy's answer over
// the same database, encoded as the route encodes it (WriteJSON). check is handed the copy's
// answer of each, to hold it to what the store was built to hold.
func compareReadsWithMergeBase(t *testing.T, database *store.Store, over string, projectLists [][]string, check func(prioritiesReadForm, []string, model.Priorities)) {
	t.Helper()
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: database, identity: headerIdentity(database)})
	frozen := directServer(Deps{Store: database})
	for _, form := range prioritiesReadForms {
		for _, projects := range projectLists {
			target := form.target(projects)
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read %s over %s: status=%d body=%.300s", target, over, response.Code, response.Body.String())
			}
			want, err := frozen.mergeBaseReadPriorities(context.Background(), slices.Clone(projects), form.summary)
			if err != nil {
				t.Fatalf("read %s with the read below this change over %s: %v", target, over, err)
			}
			encoded := httptest.NewRecorder()
			WriteJSON(encoded, http.StatusOK, want)
			got, wanted := response.Body.Bytes(), encoded.Body.Bytes()
			t.Logf("%s with %s over %s: the server answers %d bytes, sha256 %x; the read below this change %d bytes, sha256 %x",
				form.called, projectsNamed(projects), over, len(got), sha256.Sum256(got), len(wanted), sha256.Sum256(wanted))
			if !bytes.Equal(got, wanted) {
				t.Errorf("%s with %s answers %d bytes, sha256 %x, want the read below this change's %d bytes, sha256 %x; the first JSON path where they differ: %s",
					form.called, projectsNamed(projects), len(got), sha256.Sum256(got), len(wanted), sha256.Sum256(wanted), firstJSONDifference(got, wanted))
			}
			check(form, projects, want)
		}
	}
}

// projectsNamed is how a line names the projects a request named.
func projectsNamed(projects []string) string {
	switch len(projects) {
	case 0:
		return "no project"
	case 1:
		return "project " + projects[0]
	default:
		return "projects " + strings.Join(projects[:len(projects)-1], ", ") + " and " + projects[len(projects)-1]
	}
}

// firstJSONDifference is the first path, in document order with an object's keys in byte order,
// at which the JSON answers got and want hold different values, with the two values there.
func firstJSONDifference(got, want []byte) string {
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return fmt.Sprintf("none: the answer is no JSON (%v)", err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		return fmt.Sprintf("none: the read below this change's answer is no JSON (%v)", err)
	}
	path, at, wanted := jsonDifference("$", a, b)
	if path == "" {
		return "none: the two answers hold the same values in other bytes"
	}
	describe := func(value any) string {
		encoded, _ := json.Marshal(value)
		return fmt.Sprintf("%.200s", encoded)
	}
	return fmt.Sprintf("%s (the server answers %s, the read below this change %s)", path, describe(at), describe(wanted))
}

// jsonDifference walks a and b together from path and answers the first path at which they
// differ, with the two values there, or "" where they hold the same values.
func jsonDifference(path string, a, b any) (string, any, any) {
	switch wanted := b.(type) {
	case map[string]any:
		got, isObject := a.(map[string]any)
		if !isObject {
			return path, a, b
		}
		keys := make([]string, 0, len(wanted)+len(got))
		for key := range wanted {
			keys = append(keys, key)
		}
		for key := range got {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			if differs, at, want := jsonDifference(path+"."+key, got[key], wanted[key]); differs != "" {
				return differs, at, want
			}
		}
		return "", nil, nil
	case []any:
		got, isList := a.([]any)
		if !isList {
			return path, a, b
		}
		for index := range min(len(got), len(wanted)) {
			if differs, at, want := jsonDifference(fmt.Sprintf("%s[%d]", path, index), got[index], wanted[index]); differs != "" {
				return differs, at, want
			}
		}
		if len(got) != len(wanted) {
			return fmt.Sprintf("%s[%d]", path, min(len(got), len(wanted))), len(got), len(wanted)
		}
		return "", nil, nil
	default:
		if !reflect.DeepEqual(a, b) {
			return path, a, b
		}
		return "", nil, nil
	}
}

// noLivePriority is a deployment that holds no live priority: two projects, work in flight and
// parked in each, and one priority that is gone with relations to it still asserted, so the work
// under it advances none.
type noLivePriority struct {
	database *store.Store
	projects []string
}

// seedNoLivePriority writes that deployment, with JIT off on its connections as on the corpus's.
func seedNoLivePriority(t *testing.T) noLivePriority {
	t.Helper()
	database := withJITOff(t, storetest.Open(t))
	at := time.Now().UTC().Add(-48 * time.Hour)
	projects := []string{"EMPA", "EMPB"}
	statuses := []string{"in_progress", "testing", "needs_review", "retro", "backlog", "icebox", "todo", "triage"}
	projectRows, issueRows, eventRows, tuples := [][]any{}, [][]any{}, [][]any{}, [][]any{}
	for place, project := range projects {
		for number := 1; number <= len(statuses); number++ {
			key := seededIssueKey(project, number)
			status := statuses[(number-1+place)%len(statuses)]
			var parent *string
			if number > 1 {
				above := seededIssueKey(project, 1)
				parent = &above
			}
			written := at.Add(time.Duration(place*len(statuses)+number) * time.Minute)
			issueRows = append(issueRows, seededIssueRow{
				key: key, project: project, number: number, title: "Work " + key, status: status, parent: parent,
				rank: seededRank(number), at: written, lastSeq: 2,
			}.values())
			eventRows = append(eventRows,
				seededEvent(key, 1, "issue.created", seededIssuePayload(t, "todo"), written),
				seededEvent(key, 2, "issue.updated", seededIssuePayload(t, status, "status"), written.Add(time.Minute)))
		}
		projectRows = append(projectRows, seededProject(project, len(statuses), at))
		tuples = append(tuples, seededAdvancesTuple(seededIssueKey(project, 1), "priority-gone", at))
	}
	gone := seededPriorityNode(t, "priority-gone", "A priority that is gone", "What it was for.", "alice", "a0001", at)
	gone[slices.Index(seededNodeColumns, "state")] = prioritystore.StateGone
	copySeededRows(t, database, "projects", seededProjectColumns, projectRows)
	copySeededRows(t, database, "issues", seededIssueColumns, issueRows)
	copySeededRows(t, database, "events", seededEventColumns, eventRows)
	copySeededRows(t, database, "priority_nodes", seededNodeColumns, [][]any{gone})
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	settleSpeedCorpus(t, database)
	return noLivePriority{database: database, projects: projects}
}
