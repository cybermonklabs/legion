package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// issueStatusOrderSQL derives the SQL ordering from Dispatch's canonical lifecycle.
func issueStatusOrderSQL() string {
	var order strings.Builder
	order.WriteString("case i.status")
	for index, status := range model.IssueStatuses {
		fmt.Fprintf(&order, "\n\t\twhen '%s' then %d", status, index+1)
	}
	order.WriteString("\n\t\tend")
	return order.String()
}

var issueStatusCase = issueStatusOrderSQL()

// The listing has four statement texts: unpaged and paged, each of them pinned or not
// (issueSummaryQuery writes them). A request that names a page and no route_status filter runs
// the paged text, which cuts the page in SQL and answers `total` beside it; a route_status
// request keeps the unpaged text and listIssues' own page cut, because that filter runs in Go
// after the Envoy roster enrichment and total must count what is left after it. pgx caches a
// plan for each text, so neither shape replans the other's.
var (
	listIssuesQuery           = issueSummaryQuery(false, false, true)
	listPinnedIssuesQuery     = issueSummaryQuery(true, false, true)
	listIssuesPageQuery       = issueSummaryQuery(false, true, true)
	listPinnedIssuesPageQuery = issueSummaryQuery(true, true, true)
)

// issueSummaryQuery writes one listing statement. `filtered` applies every filter to `issues i`
// alone, so `total` is a count over it with no join and the paged text orders and cuts it before
// anything else is resolved: the six joins then run for the page's rows alone, and a listing of
// twenty rows costs twenty issues' blockers, specs, open asks, children, components and
// repository rather than every matching issue's. Both counts come from the one statement, since
// two statements read two snapshots and can disagree about a table a write is moving.
//
// An empty page still carries the count the caller asked for, so `listed` answers one issue of
// the filtered set marked `in_page` false when the page holds nothing, and scanIssuePage drops
// it without resolving it; an empty filtered set answers no row at all, and the total is then 0.
// The order ends on the key, the one column no two issues share, so it is total and consecutive
// offsets cannot show a row twice or never while the listing does not change between the reads
// (issuePage.of says what a change does). Without the key they could: ranks repeat across
// projects (every project's first issue is "U"), nothing makes one unique within a project, and
// created_at is the creating transaction's start.
// readAdvances false is the one listing text that answers no `advances` relation: it leaves out
// the relation read, the inheritance walk it climbs and the columns' join, and answers the three
// columns as the constants an issue with no relation reads as, so one scan and one argument list
// serve both texts. Its one caller is the speed test that holds the relation read to twice the
// time of the same page without it; every text the server runs passes true. The `advances`
// filter term stays in both, because it is a filter rather than a read: a request that names no
// `advances` key leaves its seed gated off (issueAdvancesFilterTerms), so the text without the
// read measures the read alone. The move into `backlog` or `icebox` stays in both for the same
// reason: it is read only under `?moved=true` ($14), and a request without it reads no event.
func issueSummaryQuery(pinned, paged, readAdvances bool) string {
	parameter := 15
	pinnedJoin := ""
	if pinned {
		pinnedJoin = fmt.Sprintf("\n\t\tjoin user_issue_state s on s.issue_key = i.key and s.login = $%d and s.pinned", parameter)
		parameter++
	}
	query := `with recursive ` + issueAdvancesFilterTerms + `, filtered as (
		select i.key, i.status, i.rank, i.created_at
		from issues i` + pinnedJoin + `
		left join advancing on advancing.key = i.key
		` + issueListFilters + `
	)`
	advancesColumns := issueAdvancesColumns
	terms := issueComponentsTerms + `, ` + refs.IssueRepoTerms
	if readAdvances {
		terms = issueAdvancesTerms + `, ` + terms
	} else {
		advancesColumns = issueAdvancesColumnsUnread
	}
	rows, columns := "filtered", issueSummaryColumns(advancesColumns)
	if paged {
		query += fmt.Sprintf(`, page as (
		select i.key from filtered i
		order by %s
		limit $%d offset $%d
	), listed as (
		select p.key, true as in_page from page p
		union all
		select (select f.key from filtered f limit 1), false where not exists (select 1 from page)
	)`, issueListOrder, parameter, parameter+1)
		rows = "listed"
		columns = "(select count(*) from filtered) as total, listed.in_page,\n\t       " + columns
	}
	return query + `, subjects as (
		select key from ` + rows + `
	), ` + terms + `
	select ` + columns + `
	from ` + rows + ` join issues i on i.key = ` + rows + `.key
	` + issueSummaryJoins(readAdvances) + `
	order by ` + issueListOrder + `
`
}

// issueListFilters are the ten filters a listing request carries, written as thirteen parameters
// (the priority filter takes a list and a flag for the unset, and the `advances` filter a list
// and a flag for each of its two keywords), every one but the last written against `issues i`
// alone, so the filtered term joins nothing. The status filter ($2) is a list of lifecycle
// statuses, an issue standing in any of them, and the empty list is every status. The closed
// window ($9, $10) is [closed_since, closed_before), and an open issue, closed at no time, is
// outside every window. The last is the `advances` filter, which no column of an issue holds and
// whose answer a relation of an issue above it can decide: it reads one term computed once per
// request (issueAdvancesFilterTerms), joined to the issue by its key, so the filtered term still
// resolves nothing per row and the page and the count both read it. Its four arms are a union:
// the ids named ($11), the work under none ($12), and the work under any ($13), which is every
// issue `advancing` holds and so exactly the issues the arm for none leaves out.
//
// $14, `?moved=true`, is no filter: it is the gate on the move read the joins make
// (issueSummaryJoins), and the parameters after it are the pinned login and the page.
const issueListFilters = `where ($1 = '' or i.project_key = $1)
		  and (cardinality($2::text[]) = 0 or i.status = any($2::text[]))
		  and ($3 = '' or i.parent_key = $3)
		  and ($4::timestamptz is null or i.updated_at >= $4)
		  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
		  and (not $6::boolean or i.closed_at is null)
		  and ((cardinality($7::smallint[]) = 0 and not $8::boolean)
		       or i.priority = any($7::smallint[])
		       or ($8::boolean and i.priority is null))
		  and ($9::timestamptz is null or i.closed_at >= $9)
		  and ($10::timestamptz is null or i.closed_at < $10)
		  and ((cardinality($11::text[]) = 0 and not $12::boolean and not $13::boolean)
		       or coalesce(advancing.named, false)
		       or ($12::boolean and advancing.key is null)
		       or ($13::boolean and advancing.key is not null))`

// issueListOrder is the listing's order, over the issue aliased `i`: the lifecycle status, the
// rank, the creation time, then the key. The paged text orders the filtered keys by it and the
// statement orders its answer by it again, since a join keeps no order of its own.
var issueListOrder = issueStatusCase + `, i.rank asc, i.created_at asc, i.key asc`

// issueClaimColumns is the claim half of an issue select, named once like
// issueComponentsColumns so every read scans the two columns in one order.
const issueClaimColumns = `i.claimed_by, i.claimed_at`

// issueProgressColumns are an issue's progress in one order (progressScan): the task counts a
// version write stored on the row, and its direct children counted on this read, every status,
// done being `status = 'done'`. A grouped read answers no row for an issue with no child, where
// the single-issue lateral answers 0, so both coalesce to the count progressScan expects.
const issueProgressColumns = `i.tasks_done, i.tasks_total, coalesce(progress.children_done, 0), coalesce(progress.children_total, 0)`

// issueProgressLateral counts one issue's direct children in one pass over the partial
// issues_parent_key index, for the single-issue read; a listing counts the children of its whole
// page in one grouped read instead (issueSummaryJoins).
const issueProgressLateral = `
	left join lateral (
		select count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c where c.parent_key = i.key
	) progress on true`

// issueAdvancesColumns are the three values every issue read answers for its `advances`
// relations, in advancesScan's order: the live priority ids the issue's own row states now, the
// revision a whole-set write of them carries, and the live ids it inherits from the issues above
// it. An issue that states none and inherits none reads as two empty lists and revision 0. The
// revision is the issue's own relations alone, inheritance counted in neither: it is what a
// write of this issue's own set is held against, and no write of this issue states or retracts
// an inherited relation.
const issueAdvancesColumns = `coalesce(advances.ids, '{}'), coalesce(advances.revision, 0), coalesce(advances.inherited, '{}')`

// issueAdvancesColumnsUnread answers those three columns as the constants an issue with no
// relation reads as, for the one listing text that leaves the relation read out
// (issueSummaryQuery's readAdvances): the row keeps its shape, so one scan serves both texts.
const issueAdvancesColumnsUnread = `'{}'::text[], 0::bigint, '{}'::text[]`

// issueAdvancesTerms are the `advances` relations of every issue of `subjects` — its own and
// the ones it inherits — which both the listing and the single-issue read splice into their
// `with recursive` list and join to (issueAdvancesJoin). An issue advances a priority when its
// own row names it or when any issue on its parent chain does, so the answer is read every time
// and stored nowhere; a relation on a child places no issue above it, which is why the walk
// climbs and never descends.
//
// `advances_chain` is that climb, one row per (subject, issue whose relations the subject
// reads): the subject itself at depth 0 and each issue above it one deeper, bounded by
// parentDepthCap like legionManagedStatusSQL's walk of the same chain for the `legion` label,
// so it terminates even if a raced reparent ever commits a cycle. One walk climbs every
// subject's chain together, as refs.IssueRepoTerms' does, so a page of issues pays one walk
// rather than one per row.
//
// `advances_current` is prioritystore.AdvancesTupleRead's reading of the log for every issue
// those chains name at once (over priority_tuples_current): the newest tuple per (issue,
// priority), asserted or not, read once however many subjects inherit from that issue.
//
// `advances` groups the two back onto the subject. The ids are the asserted relations to live
// priorities alone: a relation whose last word is a retraction is no relation now, and a priority
// whose state is `gone` is no priority, so the join to priority_nodes leaves it out of both
// lists, as issueAdvancesFilterTerms' join leaves it out of the filter and priorityRelationsQuery
// out of the priorities read. Without it the issue read and every listing row would name a
// priority that `advances=none` and the first fault already count as none. Depth 0 answers the
// issue's own ids and the revision, and every greater depth answers what it inherits.
//
// The revision is the greatest seq of the issue's own set, retractions and relations to a gone
// priority counted, because it is what the store holds for the issue rather than what it answers,
// and a tuple a writer did not see must move it whichever of the two it is. The join to the live
// priorities is a left join on their key, so it drops no tuple from the max. A tuple that is the
// greatest of the issue is the newest of its own pair as well, so the max over these rows is the
// max over every tuple. Both lists come back in id order, which is the order advancesScan merges
// them in.
var issueAdvancesTerms = `advances_chain as (
		select s.key as start, s.key as issue_key, 0 as depth
		from subjects s
		union all
		select c.start, p.parent_key, c.depth + 1
		from advances_chain c join issues p on p.key = c.issue_key
		where p.parent_key is not null and c.depth < ` + parentDepthCapSQL + `
	), advances_current as (
		` + prioritystore.AdvancesTupleRead(`, t.seq, t.asserted`, `and t.from_id in (select issue_key from advances_chain)`) + `
	), advances as (
		select c.start as issue_key,
		       coalesce(array_agg(distinct cu.to_id order by cu.to_id)
		                filter (where cu.asserted and live.id is not null and c.depth = 0), '{}') as ids,
		       coalesce(max(cu.seq) filter (where c.depth = 0), 0) as revision,
		       coalesce(array_agg(distinct cu.to_id order by cu.to_id)
		                filter (where cu.asserted and live.id is not null and c.depth > 0), '{}') as inherited
		from advances_chain c
		left join advances_current cu on cu.from_id = c.issue_key
		left join priority_nodes live on live.kind = '` + prioritystore.KindPriority + `' and live.id = cu.to_id
			and live.state = '` + prioritystore.StateLive + `'
		group by c.start
	)`

// issueAdvancesJoin attaches issueAdvancesTerms' answer to the issue aliased `i`, one row per
// subject.
const issueAdvancesJoin = `left join advances on advances.issue_key = i.key`

// issueAdvancesFilterTerms answer the listing's `advances` query keys, as one term computed once
// per request rather than a read per row: $11 is the priority ids the request named, $12 is
// whether it asked for the work that advances none and $13 whether it asked for the work that
// advances any. `advancing` holds one row per issue that advances a live priority by either
// route, `named` saying whether one of them is an id the request asked for, so `filtered`
// decides every key by one join on the issue's key (issueListFilters) and still resolves nothing
// per row.
//
// `advances_stated` is the issues whose own rows state an asserted relation now, by the reading
// of the log issueAdvancesTerms makes for a page. A priority whose state is `gone` is no
// priority, so the join to priority_nodes drops it and the work under it advances none. The seed
// is gated on the request naming an `advances` key at all: the clause holds no column, so the
// planner makes it a one-time filter and a listing that asks for no priority reads no tuple.
//
// `advances_spread` carries each seed down to the issues below it, which is the same
// inheritance the read climbs, written as one descent per request instead of one climb per row;
// it is bounded by parentDepthCap, which no chain passes (refuseParentTooDeep), so a descendant
// the cap cuts off is one no chain could have reached anyway.
var issueAdvancesFilterTerms = `advances_stated as (
		select newest.from_id as key, bool_or(newest.to_id = any($11::text[])) as named
		from (
			` + prioritystore.AdvancesTupleRead(`, t.asserted`, `and (cardinality($11::text[]) > 0 or $12::boolean or $13::boolean)`) + `
		) newest
		join priority_nodes n on n.kind = '` + prioritystore.KindPriority + `' and n.id = newest.to_id
			and n.state = '` + prioritystore.StateLive + `'
		where newest.asserted
		group by newest.from_id
	), advances_spread as (
		select s.key, s.named, 0 as depth from advances_stated s
		union all
		select c.key, a.named, a.depth + 1
		from advances_spread a join issues c on c.parent_key = a.key
		where a.depth < ` + parentDepthCapSQL + `
	), advancing as (
		select key, bool_or(named) as named from advances_spread group by key
	)`

// issueSummaryColumns are the columns a listing row scans, in issueSummaryRow's order, around
// the three `advances` columns the text was built with: either the relation read's
// (issueAdvancesColumns) or the constants of the text that leaves it out
// (issueAdvancesColumnsUnread), which is why they are a parameter rather than spliced in here.
// The two cells of the move (issueMovedColumns) stand right behind them.
func issueSummaryColumns(advances string) string {
	return `i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
	       coalesce(blockers.keys, '{}'),
	       ` + advances + `,
	       ` + issueMovedColumns + `,
	       i.assignee, i.route, i.updated_at, i.last_seq, i.created_by, i.created_at,
	       coalesce(spec.id, ''),
	       ` + issueClaimColumns + `,
	       case when i.closed_at is null then coalesce(open_asks.total, 0) else 0 end,
	       ` + issueProgressColumns + `,
	       ` + issueComponentsColumns + `,
	       ` + refs.IssueRepoColumns + `,
	       i.closed_at`
}

// issueMovedColumns are the two cells `?moved=true` answers on a listing row, in
// issueSummaryRow's order: when the issue was moved into `backlog` or `icebox` and who moved it
// (parkedMoveLateral). Both are null on every row of a request that did not ask for them, and on
// a row the read finds no move for.
const issueMovedColumns = `moved.created_at, moved.actor`

// issueMovedJoin is that read for the issue aliased `i`, gated on the request asking for it ($14)
// and on the issue standing in one of the two statuses now. A row the gate is false for reads no
// event, so a listing without the key pays nothing for the read and one with it pays for its
// parked rows alone. An issue standing in any other status answers null although it may once
// have been moved there, since the cells say how the issue came to stand where it stands.
var issueMovedJoin = parkedMoveLateral("i", `$14::boolean and i.status in (`+sqlStringList(priorityParkedStatuses)+`)`)

// issueSummaryJoins resolve the seven values a listing row carries beside the issue's own
// columns, each for every issue of `subjects` at once rather than for one row at a time, and the
// move a request asked for (issueMovedJoin), one issue at a time. The open-ask read keeps the
// state filter in its own where clause, so it matches the partial asks_open(issue_key) where
// state = 'open' index instead of scanning asks, and the closed-issue rule stays in the select
// list: a closed issue reports 0 open asks however many it has. The `advances` join is the one a
// text may leave out (issueSummaryQuery's readAdvances), since nothing else reads its terms.
func issueSummaryJoins(readAdvances bool) string {
	joins := `left join (
		select l.issue_key, array_agg(l.target_key order by l.target_key) as keys
		from issue_links l
		where l.kind = 'blocked_by' and l.issue_key in (select key from subjects)
		group by l.issue_key
	) blockers on blockers.issue_key = i.key
	left join (
		select a.issue_key, a.id::text as id
		from artifacts a
		where a.is_primary and a.issue_key in (select key from subjects)
	) spec on spec.issue_key = i.key
	left join (
		select a.issue_key, count(*) as total
		from asks a
		where a.state = 'open' and a.issue_key in (select key from subjects)
		group by a.issue_key
	) open_asks on open_asks.issue_key = i.key
	left join (
		select c.parent_key, count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c
		where c.parent_key in (select key from subjects)
		group by c.parent_key
	) progress on progress.parent_key = i.key
	`
	if readAdvances {
		joins += issueAdvancesJoin + `
	`
	}
	return joins + issueComponentsJoin + `
	` + refs.IssueRepoJoin + `
	` + issueMovedJoin
}

const (
	maxIssueLabels  = 20
	maxIssueLabel16 = 40
	// unsetPriorityFilter is the `priority` query value that matches an issue with no priority.
	unsetPriorityFilter = "none"
	// noAdvancesFilter is the `advances` query value that matches an issue advancing no live
	// priority, by its own row or by an issue above it, and anyAdvancesFilter the one that
	// matches an issue advancing at least one, which is every other issue. A priority create
	// refuses each as an id, given or derived (priorityIDRefusal), so a keyword can never also
	// name a priority.
	noAdvancesFilter  = "none"
	anyAdvancesFilter = "any"
)

// parseStatusFilter reads the repeated `status` query parameter: each value is a status of the
// lifecycle (model.IssueStatuses), and the listing answers the issues standing in any of them.
// No value means every status, and so does a blank one, which is what `status=` has always
// answered; one value means what it meant when the key took one. A value the lifecycle does not
// hold is refused, naming it, as a write of one is (issue_patch.go): it would otherwise match no
// issue and read as a status nothing stands in.
func parseStatusFilter(values []string) ([]string, error) {
	statuses := []string{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		switch {
		case value == "":
		case model.IsIssueStatus(value):
			statuses = append(statuses, value)
		default:
			return nil, errorf(http.StatusBadRequest, "INVALID_STATUS",
				"status filter values must be one of %s, got %q", strings.Join(model.IssueStatuses, ", "), raw)
		}
	}
	slices.Sort(statuses)
	return slices.Compact(statuses), nil
}

// parsePriorityFilter reads the repeated `priority` query parameter: each value is 0..3, or
// "none" for an issue with no priority. No values means no priority filter.
func parsePriorityFilter(values []string) (priorities []int16, unset bool, err error) {
	priorities = []int16{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == unsetPriorityFilter {
			unset = true
			continue
		}
		switch value {
		case "0", "1", "2", "3":
			priorities = append(priorities, int16(value[0]-'0'))
		default:
			return nil, false, errorf(http.StatusBadRequest, "INVALID_PRIORITY", "priority filter values must be 0 to 3 or %s, got %q", unsetPriorityFilter, raw)
		}
	}
	return priorities, unset, nil
}

// parseAdvancesFilter reads the repeated `advances` query parameter: each value is a priority
// id, "none" for an issue that advances none, or "any" for an issue that advances one. No values
// means no advances filter, and several together are a union, as the `priority` filter's two
// halves are, so "none" and "any" together are every issue. An id no priority carries, and one
// of a priority that is gone, matches no issue rather than refusing: the listing answers what
// the store holds now. A blank value is refused, since it names neither a priority nor a
// keyword.
func parseAdvancesFilter(values []string) (ids []string, none, some bool, err error) {
	ids = []string{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		switch value {
		case noAdvancesFilter:
			none = true
		case anyAdvancesFilter:
			some = true
		case "":
			return nil, false, false, errorf(http.StatusBadRequest, "INVALID_ADVANCES",
				"advances filter values must be a priority id, %s or %s, got %q", noAdvancesFilter, anyAdvancesFilter, raw)
		default:
			ids = append(ids, value)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), none, some, nil
}

// microsecondCeiling is a listing's time bound as the statement compares it. Postgres stores a
// timestamptz to the microsecond and pgx encodes a bound by truncating its nanoseconds, which
// lowers it: an issue stored at the truncated microsecond, before the bound the caller named,
// would be kept by `>= since` and dropped by `< before`. Every stored time is a whole
// microsecond, so it is at or after a bound exactly when it is at or after the bound rounded up
// to the next microsecond, and before the bound exactly when it is before that: rounding up keeps
// both meanings. nil is no bound.
func microsecondCeiling(bound *time.Time) *time.Time {
	if bound == nil {
		return nil
	}
	ceiling := bound.Truncate(time.Microsecond)
	if ceiling.Before(*bound) {
		ceiling = ceiling.Add(time.Microsecond)
	}
	return &ceiling
}

// claimScan reads an issue's two claim columns as one nullable claim. A row has both or
// neither (the issues_claim_complete constraint), so either absent means unclaimed.
type claimScan struct {
	actor []byte
	at    *time.Time
}

func (c *claimScan) targets() []any { return []any{&c.actor, &c.at} }

func (c *claimScan) resolve(key string) (*model.IssueClaim, error) {
	if len(c.actor) == 0 || c.at == nil {
		return nil, nil
	}
	claim := model.IssueClaim{At: *c.at}
	if err := json.Unmarshal(c.actor, &claim.Actor); err != nil {
		return nil, fmt.Errorf("decode issue %s claim actor: %w", key, err)
	}
	return &claim, nil
}

// progressScan reads issueProgressColumns for one issue. The task columns are null together on an
// issue whose spec was never counted (issues_task_progress_complete), and a counted spec with no
// task item is total 0: both answer nil, so the API's `tasks: null` means "no task list" either
// way. Children is nil for an issue with no child.
type progressScan struct {
	tasksDone, tasksTotal       *int
	childrenDone, childrenTotal int
}

func (p *progressScan) targets() []any {
	return []any{&p.tasksDone, &p.tasksTotal, &p.childrenDone, &p.childrenTotal}
}

func (p *progressScan) resolve() model.IssueProgress {
	var progress model.IssueProgress
	if p.tasksTotal != nil && *p.tasksTotal > 0 {
		progress.Tasks = &model.ProgressCount{Done: *p.tasksDone, Total: *p.tasksTotal}
	}
	if p.childrenTotal > 0 {
		progress.Children = &model.ProgressCount{Done: p.childrenDone, Total: p.childrenTotal}
	}
	return progress
}

// advancesScan reads issueAdvancesColumns for one issue: the priority ids of its own current
// `advances` relations, the revision the store holds for them, and the ids it inherits from the
// issues above it.
type advancesScan struct {
	ids       []string
	revision  int64
	inherited []string
}

func (a *advancesScan) targets() []any { return []any{&a.ids, &a.revision, &a.inherited} }

// resolve answers one entry per priority the issue advances, in id order: the issue's own
// marked Inherited false, the rest true. An id the issue holds both ways is one entry marked
// own, since a write of this issue's set states and retracts that one. Both lists arrive in id
// order (issueAdvancesTerms), so this is one merge rather than a sort.
func (a *advancesScan) resolve() []model.IssueAdvance {
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

func normalizeIssueLabels(values []string) ([]string, error) {
	if len(values) > maxIssueLabels {
		return nil, countExceededError("LABELS_INPUT", "labels", len(values), maxIssueLabels)
	}
	labels := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		label := strings.TrimSpace(value)
		if length := itemblocks.Units16(label); length == 0 || length > maxIssueLabel16 {
			return nil, errorf(http.StatusBadRequest, "LABELS_INPUT", "each label must be 1 to %d characters", maxIssueLabel16)
		}
		key := strings.ToLower(label)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels, nil
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	pinned := query.Get("pinned") == "true"
	login := ""
	if pinned {
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		login = actor.ID
	} else if !s.requireAuthenticated(w, r) {
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	statuses, err := parseStatusFilter(query["status"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	parent := strings.TrimSpace(query.Get("parent"))
	labels, err := normalizeIssueLabels(query["label"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index, label := range labels {
		labels[index] = strings.ToLower(label)
	}
	// Each time bound is read under its literal key rather than through a helper taking the key,
	// so routes_query_keys_test.go's walk can tie each one to issueListQuery.
	var updatedSince *time.Time
	if query.Has("updated_since") {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("updated_since")))
		if err != nil {
			writeError(w, "INVALID_UPDATED_SINCE", http.StatusBadRequest, "updated_since must be an RFC3339 timestamp")
			return
		}
		updatedSince = &parsed
	}
	var closedSince *time.Time
	if query.Has("closed_since") {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("closed_since")))
		if err != nil {
			writeError(w, "INVALID_CLOSED_SINCE", http.StatusBadRequest, "closed_since must be an RFC3339 timestamp")
			return
		}
		closedSince = &parsed
	}
	var closedBefore *time.Time
	if query.Has("closed_before") {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("closed_before")))
		if err != nil {
			writeError(w, "INVALID_CLOSED_BEFORE", http.StatusBadRequest, "closed_before must be an RFC3339 timestamp")
			return
		}
		closedBefore = &parsed
	}
	// The window is judged as the caller wrote it, before rounding: two bounds inside one
	// microsecond are a valid window, not a backwards one, and the statement answers whatever
	// stored times fall between their rounded values (none, unless the lower bound is whole).
	if closedSince != nil && closedBefore != nil && !closedSince.Before(*closedBefore) {
		writeError(w, "INVALID_CLOSED_WINDOW", http.StatusBadRequest, "closed_since must be before closed_before")
		return
	}
	priorities, unsetPriority, err := parsePriorityFilter(query["priority"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advances, noAdvances, anyAdvances, err := parseAdvancesFilter(query["advances"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	routeStatus, err := parseRouteStatusFilter(query.Get("route_status"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	page, err := parseIssuePage(query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A route matters only while there is work to reach, so the route_status filter reads open
	// issues alone: the owner audit is every open issue whose route reaches nobody.
	open := query.Get("open") == "true" || routeStatus != ""
	// The move into `backlog` or `icebox` is read only when the request asks for it, and a row
	// carries its two cells only then (model.IssueSummary's MovedAt and MovedBy).
	moved := query.Get("moved") == "true"
	arguments := []any{project, statuses, parent, microsecondCeiling(updatedSince), labels, open, priorities, unsetPriority,
		microsecondCeiling(closedSince), microsecondCeiling(closedBefore), advances, noAdvances, anyAdvances, moved}
	if pinned {
		arguments = append(arguments, login)
	}
	// SQL cuts the page only when no route_status filter runs after the read: that filter needs
	// the Envoy roster, which this read cannot reach, and total counts what is left after it.
	inSQL := page != nil && routeStatus == ""
	listQuery := listIssuesQuery
	switch {
	case pinned && inSQL:
		listQuery = listPinnedIssuesPageQuery
	case pinned:
		listQuery = listPinnedIssuesQuery
	case inSQL:
		listQuery = listIssuesPageQuery
	}
	var issues []model.IssueSummary
	total := 0
	if inSQL {
		arguments = append(arguments, page.limit, page.offset)
		issues, total, err = s.scanIssuePage(r.Context(), listQuery, arguments)
	} else {
		issues, err = s.scanIssueSummaries(r.Context(), listQuery, arguments)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if routed := slices.ContainsFunc(issues, func(issue model.IssueSummary) bool { return issue.Route != nil }); routed {
		judged := routeStatus != "" && routeStatus != model.RouteUnknown
		var roster routeRoster
		if judged {
			// A filter for a judged state cannot be cut from an answer it could not read: a failed
			// read of the stored answer is the database's failure and is answered as one, and only
			// a listener that did not answer is the listener's.
			if roster, err = s.readRouteRoster(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		} else {
			roster = s.routeRosterOrUnknown(r.Context())
		}
		if judged && !roster.answered {
			// No route can be judged, so a filter for a judged state refuses rather than answer
			// a list that would read as "every route reaches someone" (or "nobody's does").
			if roster.reason == model.RouteReasonRefilling {
				writeEnvoyRefilling(w, roster.retryAfter)
				return
			}
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable,
				"the Envoy listener did not answer, so Dispatch cannot tell which routes reach a live session; route_status=unknown lists the routed issues")
			return
		}
		for index := range issues {
			issues[index].IssueRouteReach = roster.reach(issues[index].Route)
		}
	}
	if routeStatus != "" {
		issues = slices.DeleteFunc(issues, func(issue model.IssueSummary) bool {
			return issue.RouteStatus == nil || *issue.RouteStatus != routeStatus
		})
	}
	if inSQL {
		WriteJSON(w, http.StatusOK, page.holding(issues, total))
		return
	}
	// A route_status page is cut from the listing every filter has narrowed, that one included,
	// so total counts what the caller's filters match.
	if page != nil {
		WriteJSON(w, http.StatusOK, page.of(issues))
		return
	}
	WriteJSON(w, http.StatusOK, issues)
}

// issueSummaryRow holds one listing row as the statement answers it. Scanning and resolving are
// two steps because a paged read answers one row that is no part of its page (issueSummaryQuery
// says why), and resolving that row would refuse a page over a parent chain of its own.
type issueSummaryRow struct {
	issue      model.IssueSummary
	advances   advancesScan
	claim      claimScan
	progress   progressScan
	components componentsScan
	repo       repoScan
}

// targets are the row's scan targets, after leading, which a paged read fills with the columns
// it carries ahead of the issue's own.
func (r *issueSummaryRow) targets(leading ...any) []any {
	targets := append(leading, &r.issue.Key, &r.issue.Title, &r.issue.Status, &r.issue.Priority, &r.issue.Rank, &r.issue.Labels, &r.issue.Parent, &r.issue.BlockedBy)
	targets = append(targets, r.advances.targets()...)
	targets = append(targets, &r.issue.MovedAt, &r.issue.MovedBy)
	targets = append(targets, &r.issue.Assignee, &r.issue.Route, &r.issue.UpdatedAt, &r.issue.LastSeq, &r.issue.CreatedBy, &r.issue.CreatedAt, &r.issue.PrimaryArtifactID)
	targets = append(targets, r.claim.targets()...)
	targets = append(targets, &r.issue.OpenAsks)
	targets = append(targets, r.progress.targets()...)
	targets = append(targets, r.components.targets()...)
	targets = append(targets, r.repo.targets()...)
	return append(targets, &r.issue.ClosedAt)
}

// resolve turns the scanned row into the summary the response carries, refusing an issue whose
// parent chain the repository walk could not finish (repoScan.resolve).
func (r *issueSummaryRow) resolve() (model.IssueSummary, error) {
	issue := r.issue
	var err error
	if issue.Repo, issue.ResolvedRepo, err = r.repo.resolve(issue.Key); err != nil {
		return model.IssueSummary{}, err
	}
	claim, err := r.claim.resolve(issue.Key)
	if err != nil {
		return model.IssueSummary{}, err
	}
	issue.Claim = claim
	issue.Advances = r.advances.resolve()
	issue.AdvancesRevision = r.advances.revision
	issue.Progress = r.progress.resolve()
	issue.Components = r.components.resolve(issue.Key)
	return issue, nil
}

// scanIssueSummaries runs an unpaged list query and returns its rows with the pooled connection
// already released, so the listener read that follows holds nothing.
func (s *server) scanIssueSummaries(ctx context.Context, listQuery string, arguments []any) ([]model.IssueSummary, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, listQuery, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []model.IssueSummary{}
	for rows.Next() {
		var row issueSummaryRow
		if err := rows.Scan(row.targets()...); err != nil {
			return nil, err
		}
		issue, err := row.resolve()
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// scanIssuePage runs a paged list query and returns the page's rows and the count of every issue
// the filters match, which the same statement made of the same snapshot. The row the statement
// answers for an empty page carries that count alone: it is dropped before it is resolved, so a
// parent chain of an issue off the page refuses nothing. No row at all is an empty filtered set,
// whose count is 0.
func (s *server) scanIssuePage(ctx context.Context, listQuery string, arguments []any) ([]model.IssueSummary, int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, listQuery, arguments...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	issues := []model.IssueSummary{}
	total := 0
	for rows.Next() {
		var row issueSummaryRow
		var inPage bool
		if err := rows.Scan(row.targets(&total, &inPage)...); err != nil {
			return nil, 0, err
		}
		if !inPage {
			continue
		}
		issue, err := row.resolve()
		if err != nil {
			return nil, 0, err
		}
		issues = append(issues, issue)
	}
	return issues, total, rows.Err()
}

// getIssue answers the issue page's read. Its database work is two batches, each one round trip
// on one pooled connection once that connection has prepared their statements (pgx prepares a
// statement it has not cached in a round trip of its own first, as before a single query), where
// it was a statement at a time: the issue row, its links, its artifacts, a version query per
// artifact, its documents' reviews and approval requests, its open asks with their events and
// counts, its children and its own backlink count, every one of them a connection taken and
// given back in turn (store.sharedPoolSize says what that costs a loaded server).
//
// Batch 1 is everything keyed on the issue key alone: the issue row, its external links, its
// artifacts, its open asks, its children and its backlink count. A key no issue carries is what
// the issue row's callback records, answered as that row's pgx.ErrNoRows - 404 NOT_FOUND - once
// the batch has closed: a callback that returned the error would close the batch on it, and pgx
// invalidates every statement of a batch whose Close answers an error in the connection's
// statement and description caches (invalidateCachesOnBatchResultsError), so one 404 would cost
// that connection the whole read's prepared statements. The other callbacks run against an issue
// with no row, answer empty lists, and readIssue drops them with the rest of the read.
//
// Batch 2 is what only batch 1's answers name: every artifact's versions in one statement, the
// documents' latest reviews and open approval requests, and the open asks' identity events and
// backlink counts. An issue with no artifact and no open ask names nothing, so it makes no
// second round trip at all.
//
// Each batch's results are closed before anything else reads: a batch holds its connection until
// then, and a second connection taken while it is held is the nesting store.ErrNestedAcquire exists
// to stop. The pool would not refuse it here: store.Pool.SendBatch checks the request's mark when
// the batch is sent but sets none while the batch is open, so no callback below reads the database.
// The route's reach is read after both (routeRosterOrUnknown), and only for an issue with a route:
// a statement of its own for the listener's stored answer (storedRouteRoster), and a shared listener
// fetch only when that answer is stale, so a routed issue's read is one round trip more.
func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	read, err := s.readIssue(r.Context(), r.PathValue("key"), wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// One roster read serves both answers beside the issue: where its route reaches, and what
	// state the session holding its claim is in. An issue with neither costs no listener call.
	if read.Route != nil || issueClaimSession(read.Claim) != "" {
		roster := s.routeRosterOrUnknown(r.Context())
		read.IssueRouteReach = roster.reach(read.Route)
		read.IssueClaimLiveness = roster.claimLiveness(read.Claim, time.Now())
	}
	WriteJSON(w, http.StatusOK, read)
}

// issueDetail is everything GET /api/v1/issues/{key} answers, in the shape its response carries:
// readIssue fills every field but the route's reach and the claim's liveness, which getIssue takes
// from the roster, and the batch test builds one from the sequential loaders, so one type is the
// encoding all three answer.
type issueDetail struct {
	model.Issue
	model.IssueRouteReach
	// IssueClaimLiveness sits beside the issue rather than inside its claim: model.IssueClaim
	// is the payload of issue.claimed and issue.released, and a state read at this moment
	// must never be stored in one (model.IssueClaimLiveness carries the rest of the reason).
	model.IssueClaimLiveness
	Progress  model.IssueProgress `json:"progress"`
	Artifacts []artifactRead      `json:"artifacts"`
	OpenAsks  []issueOpenAsk      `json:"open_asks"`
	Children  []model.IssueChild  `json:"children"`
	// DesignGates is the document each of this issue's Legion design-gate steps records
	// (design_gates, PUT /api/v1/issues/{key}/design-gates), in the order a gate runs the
	// steps. An issue whose gates record nothing carries [], as the route's own answer does:
	// the record Dispatch holds is part of the issue, and a missing list would say the read
	// never asked rather than that no gate names a document here.
	DesignGates []issueDesignGate `json:"design_gates"`
	// ReferencedByCount is what the header's `Referenced by (N)` control names. The page
	// pays for it here, with the detail it already reads, instead of a graph request of
	// its own; the edges themselves are read only when the reader opens the panel.
	ReferencedByCount int `json:"referenced_by_count"`
}

// readIssue runs getIssue's two batches, whose statements and scans are the ones the sequential
// loaders run - loadIssueWithProgress, loadArtifacts, queryOwnerAsks with issueOpenAsksOf,
// issueChildrenStatement with scanIssueChildren and refs.BacklinkCounts - so the two paths answer
// the same bytes. trees is what the request asked for (wantsDisplayTrees): the display trees of
// the documents' approval reasons and of the open asks' texts, which only the dashboard reads.
func (s *server) readIssue(ctx context.Context, key string, trees bool) (issueDetail, error) {
	var read issueDetail
	var links []model.ExternalLink
	var artifacts []model.Artifact
	var asks []model.Ask
	var replies map[string]model.AskLastReply
	issueCounts := map[string]int{}
	missing := false

	first := &pgx.Batch{}
	first.Queue(issueRowStatement, key).QueryRow(func(row pgx.Row) error {
		issue, progress, err := scanIssueRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
			return nil
		}
		read.Issue, read.Progress = issue, progress
		return err
	})
	first.Queue(issueExternalLinksStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := scanExternalLinks(rows)
		links = scanned
		return err
	})
	first.Queue(issueArtifactsStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := collectArtifacts(rows)
		artifacts = scanned
		return err
	})
	first.Queue(listIssueAsksQueryOpen, key).Query(func(rows pgx.Rows) error {
		scannedAsks, scannedReplies, err := scanAskReads(rows)
		asks, replies = scannedAsks, scannedReplies
		return err
	})
	first.Queue(issueChildrenStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := scanIssueChildren(rows)
		read.Children = scanned
		return err
	})
	first.Queue(issueDesignGatesStatement, key, model.GatedStepsKnown).Query(func(rows pgx.Rows) error {
		scanned, err := scanIssueDesignGates(rows)
		read.DesignGates = scanned
		return err
	})
	if err := refs.QueueBacklinkCounts(first, "issue", []string{key}, nil, issueCounts); err != nil {
		return issueDetail{}, err
	}
	if err := s.deps.Store.Pool.SendBatch(ctx, first).Close(); err != nil {
		return issueDetail{}, err
	}
	if missing {
		return issueDetail{}, pgx.ErrNoRows
	}
	read.Issue.ExternalLinks = links
	read.ReferencedByCount = issueCounts[read.Issue.Key]

	if err := s.readIssueDetails(ctx, artifactPointers(artifacts), askReadPointers(asks)); err != nil {
		return issueDetail{}, err
	}
	// The approvals are read by the second batch, so their reasons are registered after it,
	// where the sequential loader read them after loadArtifacts. The reasons and the open asks'
	// texts are one query of the stored trees, and what it did not hold is stored on a goroutine
	// of its own, beside the rest of the request.
	reader := s.readDisplayTrees(ctx, trees)
	read.Artifacts = artifactReads(artifacts, reader)
	read.OpenAsks = issueOpenAsksOf(asks, replies, reader)
	reader.resolve()
	return read, nil
}

// readIssueDetails is getIssue's second batch: what only the first batch's answers name, filled
// into the artifacts and asks it read, by the one queue-and-apply each of them has
// (queueArtifactDetails, queueAskDetails). An issue with no artifact and no open ask names
// nothing, so it queues nothing and sends no batch.
func (s *server) readIssueDetails(ctx context.Context, artifacts []*model.Artifact, asks []*model.Ask) error {
	batch := &pgx.Batch{}
	applyArtifacts := queueArtifactDetails(batch, artifacts)
	applyAsks := queueAskDetails(batch, asks)
	if batch.Len() == 0 {
		return nil
	}
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	applyArtifacts()
	return applyAsks()
}

// issueOpenAsk is an open ask on the issue detail together with the newest
// comment in its thread — the same last_reply the inbox row carries — so the
// issue header can show who spoke last beside the ask's waiting_on.
type issueOpenAsk struct {
	shapedAsk
	LastReply *model.AskLastReply `json:"last_reply"`
}

// issueOpenAsksOf pairs each ask with the newest reply in its thread, the shape the detail
// response lists; an ask nobody has replied to carries a null last_reply. Each ask is read as
// the dashboard reads one (shapedAsk.shape), its texts registered with the request's reader.
func issueOpenAsksOf(asks []model.Ask, replies map[string]model.AskLastReply, reader *displayTreeReader) []issueOpenAsk {
	openAsks := make([]issueOpenAsk, len(asks))
	for index, ask := range asks {
		openAsks[index].shapedAsk.Ask = ask
		openAsks[index].shapedAsk.shape(reader)
		if reply, ok := replies[ask.ID]; ok {
			openAsks[index].LastReply = &reply
		}
	}
	return openAsks
}

func (s *server) loadIssue(ctx context.Context, q queryer, key string) (model.Issue, error) {
	issue, _, err := s.loadIssueWithProgress(ctx, q, key)
	return issue, err
}

// issueRowStatement reads one issue with its progress, $1 its key; scanIssueRow decodes it. It
// is the issue read's first queued statement (getIssue), whose callback records a missing issue
// for the read to answer as this row's pgx.ErrNoRows whatever the rest of the batch found. The
// text is built once, at start, rather than on every read: its component and repository terms
// are package-level values of their own, so the concatenation is work a request would repeat
// for a statement that never differs, and pgx caches a prepared plan per statement text.
var issueRowStatement = `
		with recursive subjects as (select $1::text as key),
		` + issueAdvancesTerms + `,
		` + issueComponentsTerms + `,
		` + refs.IssueRepoTerms + `
		select i.key, i.project_key, i.number, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
		       coalesce((select array_agg(l.target_key order by l.target_key) from issue_links l where l.issue_key = i.key and l.kind = 'blocked_by'), '{}'),
		       ` + issueAdvancesColumns + `,
		       i.assignee, i.route, i.created_by, i.created_at, i.updated_at, i.closed_at,
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and ` + aliasedVerificationRole + `), ''),
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and ` + aliasedPlanRole + `), ''),
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and ` + aliasedDecisionsRole + `), ''),
		       i.last_seq, ` + issueClaimColumns + `,
		       ` + issueProgressColumns + `,
		       ` + issueComponentsColumns + `,
		       ` + refs.IssueRepoColumns + `
		from issues i
		` + issueProgressLateral + `
		` + issueAdvancesJoin + `
		` + issueComponentsJoin + `
		` + refs.IssueRepoJoin + `
		where i.key = $1
	`

// issueExternalLinksStatement reads an issue's external links, $1 its key, in the order the
// response lists them; scanExternalLinks decodes them.
const issueExternalLinksStatement = `select url, kind from issue_external_links where issue_key = $1 order by url`

// scanIssueRow decodes one issueRowStatement row into the issue and the progress the read
// carries beside it. ExternalLinks is left nil: issueExternalLinksStatement carries them, run
// after this row or queued beside it.
func scanIssueRow(row pgx.Row) (model.Issue, model.IssueProgress, error) {
	var issue model.Issue
	var createdBy []byte
	var components componentsScan
	var claim claimScan
	var progress progressScan
	var repo repoScan
	var advances advancesScan
	targets := []any{
		&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels,
		&issue.Parent, &issue.BlockedBy,
	}
	targets = append(targets, advances.targets()...)
	targets = append(targets,
		&issue.Assignee, &issue.Route, &createdBy, &issue.CreatedAt, &issue.UpdatedAt, &issue.ClosedAt,
		&issue.PrimaryArtifactID, &issue.VerificationArtifactID, &issue.PlanArtifactID, &issue.DecisionsArtifactID, &issue.LastSeq,
	)
	targets = append(targets, claim.targets()...)
	targets = append(targets, progress.targets()...)
	targets = append(targets, components.targets()...)
	if err := row.Scan(append(targets, repo.targets()...)...); err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	own, resolvedRepo, err := repo.resolve(issue.Key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.Repo, issue.ResolvedRepo = own, resolvedRepo
	issue.Advances = advances.resolve()
	issue.AdvancesRevision = advances.revision
	issue.Components = components.resolve(issue.Key)
	resolved, err := claim.resolve(issue.Key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.Claim = resolved
	if err := json.Unmarshal(createdBy, &issue.CreatedBy); err != nil {
		return model.Issue{}, model.IssueProgress{}, fmt.Errorf("decode issue actor: %w", err)
	}
	return issue, progress.resolve(), nil
}

// scanExternalLinks reads the rows issueExternalLinksStatement returns. An issue with none keeps
// a non-nil empty list, which the response carries as [].
func scanExternalLinks(rows pgx.Rows) ([]model.ExternalLink, error) {
	links := []model.ExternalLink{}
	for rows.Next() {
		var link model.ExternalLink
		if err := rows.Scan(&link.URL, &link.Kind); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

// loadIssueWithProgress is loadIssue together with the issue's progress, which the read response
// carries beside the issue (model.IssueProgress says why it is not a field of it).
func (s *server) loadIssueWithProgress(ctx context.Context, q queryer, key string) (model.Issue, model.IssueProgress, error) {
	issue, progress, err := scanIssueRow(q.QueryRow(ctx, issueRowStatement, key))
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	rows, err := q.Query(ctx, issueExternalLinksStatement, key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	defer rows.Close()
	links, err := scanExternalLinks(rows)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.ExternalLinks = links
	return issue, progress, nil
}

// issueChildrenStatement reads the issue's direct children, $1 its key, each with a rollup over
// its whole subtree (the child itself included, every status): done/total counts and the newest
// updated_at. The recursive walk uses `union` with parentDepthCap like refs.Closure, so it
// terminates even if a raced reparent ever commits a cycle. scanIssueChildren decodes its rows.
//
// The walk reads each subtree issue's row already, so it carries that row's status and
// updated_at and the rollup counts them where it stands. Joining the walk's keys back to issues
// instead read the whole table: the planner has no estimate of a recursive CTE's size, so it
// hashed all 20,000 issues rather than key 1,834 primary-key lookups, which was 2,499 of the
// statement's 8,097 buffers on the measurement database. The two columns are functionally
// determined by the key the walk already carries, so `union` removes exactly the rows it
// removed before.
var issueChildrenStatement = `
		with recursive subtree as (
			select key as root, key, 1 as depth, status, updated_at from issues where parent_key = $1
			union
			select s.root, i.key, s.depth + 1, i.status, i.updated_at
			from issues i join subtree s on i.parent_key = s.key
			where s.depth < ` + parentDepthCapSQL + `
		), rollup as (
			select s.root,
			       count(distinct s.key) as total,
			       count(distinct s.key) filter (where s.status = 'done') as done,
			       max(s.updated_at) as active_at
			from subtree s
			group by s.root
		)
		select c.key, c.title, c.status, r.done, r.total, r.active_at,
		       coalesce((select json_agg(json_build_object('url', l.url, 'kind', l.kind) order by l.url)
		                 from issue_external_links l where l.issue_key = c.key), '[]')
		from issues c join rollup r on r.root = c.key
		where c.parent_key = $1 order by c.key
	`

// scanIssueChildren reads the rows issueChildrenStatement returns. An issue with no child keeps
// a non-nil empty list, which the response carries as [].
func scanIssueChildren(rows pgx.Rows) ([]model.IssueChild, error) {
	children := []model.IssueChild{}
	for rows.Next() {
		var child model.IssueChild
		var links []byte
		if err := rows.Scan(&child.Key, &child.Title, &child.Status,
			&child.SubtreeDone, &child.SubtreeTotal, &child.ActiveAt, &links); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(links, &child.ExternalLinks); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

// issueDesignGatesStatement reads the documents an issue's design-gate steps record, $1 the
// issue's key and $2 the steps in the order a gate runs them, which is the order the read
// answers them in and the order PUT /api/v1/issues/{key}/design-gates answers its own write in
// (issue_design_gates.go). The order is a parameter rather than a case expression built into
// the text, so model.GatedStepsKnown stays the one place that order is written. A key no issue
// carries returns no row, which is what the issue read's first batch requires of every
// statement beside the issue row's.
const issueDesignGatesStatement = `
		select step, artifact_id from design_gates where issue = $1
		order by array_position($2::text[], step)
	`

// scanIssueDesignGates reads the rows issueDesignGatesStatement returns. An issue whose gates
// record no document keeps a non-nil empty list, which the response carries as [].
func scanIssueDesignGates(rows pgx.Rows) ([]issueDesignGate, error) {
	gates := []issueDesignGate{}
	for rows.Next() {
		var gate issueDesignGate
		if err := rows.Scan(&gate.Step, &gate.ArtifactID); err != nil {
			return nil, err
		}
		gates = append(gates, gate)
	}
	return gates, rows.Err()
}
