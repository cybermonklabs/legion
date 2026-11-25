package asof

import "slices"

// The shadows of an issue and of the tables its own write keeps beside it: issue_links
// (blocked_by), issue_external_links and issue_components (its own attachment). Each reads the
// issue's state from its newest issue.created, issue.updated or issue.closed payload at the cut,
// by seq: the payload holds the issue row as that write left it (model.IssueEventPayload, built
// from the issue the write read back). An issue whose first such event is later than the cut did
// not exist at the cut and is in no shadow.

// issueStateTypes are the events whose payload is the whole issue. The predicate of the partial
// index events_issue_state (migration 20261120004315) names exactly these types, in this order,
// so that the planner can prove that the index serves the shadow's read, and the predicate of
// events_issue_activity names every other type as not these (issueActivityAt): a change here
// needs new indexes.
var issueStateTypes = []string{"issue.created", "issue.updated", "issue.closed"}

// IssueStateTypes are the event types the issues shadow reads an issue's state from, in the order
// of the predicates of events_issue_state and events_issue_activity.
func IssueStateTypes() []string { return slices.Clone(issueStateTypes) }

// issueClaimTypes are the events whose payload is the issue's claim alone
// (model.IssueClaimEventPayload). The predicate of the partial index events_issue_claim names
// exactly these types, in this order, so that the planner can prove that the index serves the
// claim's read (issueClaimAt): a change here needs a new index.
var issueClaimTypes = []string{"issue.claimed", "issue.released"}

// IssueClaimTypes are the event types the issues shadow reads an issue's claim from, in the order
// of the predicate of events_issue_claim.
func IssueClaimTypes() []string { return slices.Clone(issueClaimTypes) }

// The type lists of the issues' shadows as SQL lists (sqlList), each quoted once.
var (
	issueStateList = sqlList(issueStateTypes)
	issueClaimList = sqlList(issueClaimTypes)
)

// issueStateAt is the newest whole-issue event of the issue aliased issue at the cut, by seq:
// its seq, its created_at and its payload. One owner's seq is its events' commit order, which
// created_at, each transaction's start, is not.
//
// The seq is found first from events_issue_state alone, which includes created_at, and only the
// one event that holds it is read from the table (an issue's seq is unique, events_issue_key_seq_key).
// A read that compared each event's created_at from the table fetched every event of an issue made
// after the cut and found none: at an early cut most issues are such issues, and an issue read,
// whose children's progress reads every issue, spent 1.2 to 1.6 s in those fetches on a copy of
// production's event log. The outer LIMIT keeps the read a subquery of one row; without it the
// planner flattens it into the statement, counts a fraction of a row for each issue, and plans
// the issue listing as if it held a few dozen issues (87 s on the copy).
func issueStateAt(issue, cut string) string {
	return `select e.seq, e.created_at, e.payload from public.events e
			where e.issue_key = ` + issue + `.key and e.type in ` + issueStateList + `
				and e.seq = (
					select sx.seq from public.events sx
					where sx.issue_key = ` + issue + `.key and sx.type in ` + issueStateList + `
						and sx.created_at <= ` + cut + `
					order by sx.seq desc limit 1)
			limit 1`
}

// issueStateAfter is the first whole-issue event of the issue aliased issue after the cut, by
// seq: its seq and its payload, no row when there is none. As in issueStateAt, the seq is found
// from events_issue_state alone, which includes created_at, and only the one event that holds it
// is read from the table.
func issueStateAfter(issue, cut string) string {
	return `select e.seq, e.payload from public.events e
			where e.issue_key = ` + issue + `.key and e.type in ` + issueStateList + `
				and e.seq = (
					select sx.seq from public.events sx
					where sx.issue_key = ` + issue + `.key and sx.type in ` + issueStateList + `
						and sx.created_at > ` + cut + `
					order by sx.seq limit 1)
			limit 1`
}

// issueClaimAt is the claim of the issue aliased issue at the cut, its newest state at the cut
// being state: the claim of whichever is newer by seq, that state's payload or the issue's newest
// issue.claimed or issue.released at the cut, which carries the claim alone; SQL null for no
// claim. It is a scalar subquery of the shadow's select list, one for each claim column, so a
// statement that reads neither column reads no claim: PostgreSQL drops a select-list column of an
// inlined WITH member that the statement does not read, and a lateral join it cannot drop.
func issueClaimAt(issue, state, cut string) string {
	return `(select ` + jsonValue("x.claim") + ` from (
			select ` + state + `.seq, ` + state + `.payload->'claim' as claim
			union all
			(select e.seq, e.payload->'claim' from public.events e
				where e.issue_key = ` + issue + `.key and e.type in ` + issueClaimList + `
					and e.created_at <= ` + cut + `
				order by e.seq desc limit 1)
		) x order by x.seq desc limit 1)`
}

// issueActivityAt is aggregate, over e, of the events at the cut of the issue aliased i that are
// not whole-issue events, as a scalar subquery. Its predicate names the type list of
// events_issue_activity, so that the read reads that index alone; that index's predicate also
// holds issue_key is not null, which e.issue_key = i.key implies.
func issueActivityAt(aggregate, cut string) string {
	return `(select ` + aggregate + ` from public.events e
				where e.issue_key = i.key and e.type not in ` + issueStateList + ` and e.created_at <= ` + cut + `)`
}

// issuesShadow is decision 2's issues. Columns that no write changes after the insert (key,
// project_key, number, created_by, created_at) are the row's. Every other column is the newest
// payload's, a key a payload of an older shape lacks reading as the column's default (repo before
// event 3,946), with these exceptions:
//
//   - claimed_by and claimed_at: the claim of whichever is newer by seq, that payload or the
//     newest issue.claimed or issue.released, which carries the claim alone (issueClaimAt).
//
//   - last_seq: the newest seq of the issue's events at the cut, as the append sets it
//     (events.Broker.QueueAppend).
//
//   - updated_at: the latest created_at of the issue's events at the cut, which the append keeps
//     with greatest(), or the payload's own updated_at when that is later.
//
//     Each of the two is the greater of what the issue's whole-issue events at the cut give, read
//     from events_issue_state (the newest of them is the state's own seq), and what its other
//     events at the cut give, read from events_issue_activity (issueActivityAt), each from the
//     index alone. One index of every event of the issue would serve the newest state's read too,
//     and the planner, which takes an issue to hold a few events at an early cut, then read the
//     state of a busy issue from it and sorted its events.
//
//   - closed_at: null where the newest payload at the cut carries none. Else the close the cut
//     sees stamped the row again at its end (clock_timestamp(), api/issue_patch.go), after its
//     events took the transaction's start, so its payload holds the first stamp and the row the
//     second. A closed issue takes no other close before a reopen, and every write of it appends
//     a whole-issue event read back from the row. So the first whole-issue event after the cut
//     decides (issueStateAfter): with none, the row's closed_at, the same close's second stamp;
//     one that carries a closed_at read the row after that stamp, so it carries the second stamp
//     of the same close; one that carries none is a reopen, which nulled the row's stamp, and no
//     record keeps the second stamp of a close that was undone, so the closed_at is the payload's.
//     A rule that compared every later payload with the first stamp read a later rank move as
//     another close, and the same cut then answered the second stamp before the move and the
//     first one after it.
//
//   - rank: the payload's, in the column's collation "C" (migration 0049): rank compares its
//     base-62 digits as bytes, and a text read from a payload takes the database's default
//     collation, which orders the ranks U, Z and a as a, U, Z.
//
//   - search and title_lexemes: what the issues_search trigger computes, from the title at the
//     cut and the primary document's newest version at the cut (issue_search_vector, title_lexemes),
//     never the present text.
//
//   - tasks_done, tasks_total and tasks_version: the row's present values. Progress at a cut is
//     counted in Go from the primary document's version at the cut (decision 9).
func issuesShadow(cut string) string {
	title := `coalesce(s.payload->>'title', '')`
	return `select i.key as key,
		i.project_key as project_key,
		i.number as number,
		` + title + ` as title,
		coalesce(s.payload->>'status', 'triage') as status,
		` + textArray(`s.payload->'labels'`) + ` as labels,
		s.payload->>'parent' as parent_key,
		s.payload->>'route' as route,
		i.created_by as created_by,
		i.created_at as created_at,
		greatest(
			` + issueActivityAt("max(e.created_at)", cut) + `,
			(select max(e.created_at) from public.events e
				where e.issue_key = i.key and e.type in ` + issueStateList + ` and e.created_at <= ` + cut + `),
			(s.payload->>'updated_at')::timestamptz) as updated_at,
		case
			when s.payload->>'closed_at' is null then null
			else coalesce((
				select coalesce(n.payload->>'closed_at', s.payload->>'closed_at')::timestamptz
				from (` + issueStateAfter("i", cut) + `) n), i.closed_at, (s.payload->>'closed_at')::timestamptz)
		end as closed_at,
		greatest(s.seq, ` + issueActivityAt("max(e.seq)", cut) + `) as last_seq,
		public.issue_search_vector(i.key, ` + title + `, (
			select v.markdown from public.artifacts a join public.artifact_versions v on v.artifact_id = a.id
			where a.issue_key = i.key and a.is_primary and v.created_at <= ` + cut + `
			order by v.number desc limit 1)) as search,
		(coalesce(s.payload->>'rank', '') collate "C") as rank,
		(s.payload->>'priority')::smallint as priority,
		s.payload->>'assignee' as assignee,
		` + issueClaimAt("i", "s", cut) + `->'actor' as claimed_by,
		(` + issueClaimAt("i", "s", cut) + `->>'at')::timestamptz as claimed_at,
		i.tasks_done as tasks_done,
		i.tasks_total as tasks_total,
		i.tasks_version as tasks_version,
		public.title_lexemes(` + title + `) as title_lexemes,
		s.payload->>'repo' as repo
	from public.issues i
	cross join lateral (` + issueStateAt("i", cut) + `) s`
}

// issueLinksShadow is each blocked_by link the issue's newest payload at the cut names (the
// table's one kind, issue_links_kind_check). A write of blocked_by deletes the issue's links and
// inserts the new set (writeBlockedBy, api/issue_parent.go), each with the write's now(); so a
// present row whose created_at is at or before the cut is the row as it stood, and a link whose
// row was written again since takes the time of the payload's own event, at which it stood.
func issueLinksShadow(cut string) string {
	return `select i.key as issue_key,
		'blocked_by'::text as kind,
		b.target_key as target_key,
		coalesce(l.created_at, s.created_at) as created_at
	from public.issues i
	cross join lateral (` + issueStateAt("i", cut) + `) s
	cross join lateral jsonb_array_elements_text(` + jsonArray(`s.payload->'blocked_by'`) + `) as b(target_key)
	left join public.issue_links l on l.issue_key = i.key and l.kind = 'blocked_by'
		and l.target_key = b.target_key and l.created_at <= ` + cut
}

// issueExternalLinksShadow is each external link the issue's newest payload at the cut names. A
// link the request gave no kind is stored as 'url' (api/issue_patch.go), and the payload, read
// back from the table, carries the stored kind.
func issueExternalLinksShadow(cut string) string {
	return `select i.key as issue_key,
		x.link->>'url' as url,
		coalesce(nullif(x.link->>'kind', ''), 'url') as kind
	from public.issues i
	cross join lateral (` + issueStateAt("i", cut) + `) s
	cross join lateral jsonb_array_elements(` + jsonArray(`s.payload->'external_links'`) + `) as x(link)
	where jsonb_typeof(x.link) = 'object' and x.link->>'url' is not null`
}

// issueComponentsShadow is the issue's own attachment at the cut, read from the effective
// attachment its newest payload carries (model.IssueComponents): the issue has a row of its own
// exactly when that attachment is its own, mode explicit or none with no inherited_from. A write
// deletes the row and inserts the new one with the write's now() (writeIssueComponents,
// api/issue_components.go); a present row last written at or before the cut gives its times,
// and one written again since takes the time of the payload's own event. The members of an
// explicit attachment are issue_component_members, which no shadow holds: the component split is
// left out at a cut (reason M).
func issueComponentsShadow(cut string) string {
	return `select i.key as issue_key,
		s.payload->'components'->>'mode' as mode,
		s.payload->'components'->>'reason' as reason,
		coalesce(c.created_at, s.created_at) as created_at,
		coalesce(c.updated_at, s.created_at) as updated_at
	from public.issues i
	cross join lateral (` + issueStateAt("i", cut) + `) s
	left join public.issue_components c on c.issue_key = i.key and c.updated_at <= ` + cut + `
	where jsonb_typeof(s.payload->'components') = 'object'
		and s.payload->'components'->>'mode' in ('explicit', 'none')
		and s.payload->'components'->>'inherited_from' is null`
}
