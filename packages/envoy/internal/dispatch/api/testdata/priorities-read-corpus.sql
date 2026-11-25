-- The corpus of production's counts that the priorities read is measured and compared over
-- (dispatch://INFRA-5791, R1 to R3). seedPrioritiesReadCorpus (priority_read_corpus_test.go) runs
-- it on one connection, after it has written the four projects and the first issue of each, and
-- holds what it wrote to its counts.
--
-- It is the production side of the corpus of INFRA-1098's R32 (api/testdata/
-- moved-read-cost-corpus.sql on branch legion/INFRA-1098-t22), ported to the tables this tree
-- reads: issues, events, asks, priority_nodes and priority_tuples. Each row has the shape its
-- writer stores: prioritystore's Create and Advance for the priorities and the advances tuples,
-- events.Broker's append for the events of the issues, and the ask writer's row for the open asks.
-- The port leaves out R32's rows of the graph store and of the workflow store, and the 8,580
-- finished issues of its workflow side, which Legion never ran: each of them advanced no priority
-- and had no parent, so no statement of the priorities read reads one. No value is random: what
-- is drawn is drawn by the md5 of a fixed text. Every moment of an issue, a priority and an
-- advances tuple is a fixed time before the moment the corpus is written, so a close in the last
-- 7 days reads as one whenever the run is.
--
-- Two settings name the projects the caller wrote before this file runs:
--   priorities_read_corpus.projects          the keys of three projects, comma-separated.
--   priorities_read_corpus.advances_project  the key of the project of the open issues that
--                                            advance a priority.
-- Each of the four stands with its first issue, <key>-1: open, in triage, under no priority, with
-- the one event of its creation. Issue number n + 1 of a project is the corpus's issue n.
--
-- The issues take production's shape, as its API answered at 2026-11-23T23:20Z:
--   * 13,298 issues, the four first issues among them, and 13 live priorities.
--   * Production has 7,677 issues that advance a priority (57.7%), 1,600 of them by a relation of
--     their own (12%). Here each of 1,593 open issues of the advances project states one priority
--     (a root), and every finished issue and every other open issue of that project hangs below a
--     root, at most four levels down: the roots in order hold capacities of 1, 2, 1, 3, 2, 1, 4, 2,
--     3 and 21 issues below them, ten at a time, and each takes the next issues in the md5 order of
--     their keys as a binary heap, so a root of 21 reaches four levels. The other 7 own relations
--     are on parked issues four levels down that state a second priority beside the one they
--     inherit: the backlog and icebox pairs (525 + 211) are 7 more than the parked issues (729).
--     The first of them is <advances project>-2. Every own relation whose place is a multiple of 8
--     is preceded by an assert and a retraction of another priority on the same issue. All 4,663
--     finished issues advance a priority, 601 in each of the three other projects and 2,860 in the
--     advances project, so 7,651 issues do: production's 2,995 open pairs leave its 7,677 room for
--     no more finished ones than its 35% done.
--   * The open pairs of an issue and a priority by status are production's: in_progress 972, todo
--     826, backlog 525, triage 217, icebox 211, needs_review 119, testing 104 and retro 21. 622
--     of them are claimed by a session, 78 open asks stand on them, one each, and 1,461 finished
--     pairs closed in the last 7 days. Parked (backlog or icebox, advancing a priority): 729.
--   * The open issues of the second and third projects, numbered after their finished ones,
--     advance no priority: 115 in in_progress (production's unplaced issues), and as many in todo
--     and backlog as bring all issues to production's 17% and 32%; the rest are in icebox and
--     triage, half each. The 4,663 finished issues are 35.1% (production's 35% is 4,654), and with
--     the 1,216 open members in flight the unplaced make production's 10% in flight. As many of
--     these issues have a parent among them as bring all issues to production's 68% with a parent.
--   * Each of these issues has its events, seq 1 to its count, created_at rising with seq, and its
--     row's last_seq at the newest. The counts are drawn from production's events per issue (mean
--     51, p50 16, p90 118, p99 425, at most 937: the newest event's seq of 120 issues that advance a
--     priority). Each issue takes that distribution's quantile at its place in the md5 order of
--     these issues' keys.

create temp table corpus_counts as
with production as (
  -- Production's counts, and the moment the corpus is written.
  select now() as written_at, 13 as priorities, 4 as firsts, 13298 as issues,
         601 as finished_each, 2860 as finished_advances,
         972 as in_progress, 104 as testing, 119 as needs_review, 21 as retro, 826 as todo, 217 as triage,
         211 as icebox, 525 as backlog_pairs, 729 as parked, 1600 as own, 115 as unplaced,
         round(0.17 * 13298)::int as todo_all, round(0.32 * 13298)::int as backlog_all,
         round(0.68 * 13298)::int as with_parent,
         622 as claimed, 78 as asks, 1461 as closed_recently
), members as (
  -- The open members by status, the parked ones that advance two priorities among the backlog's,
  -- the roots that state the rest of the own relations, and the finished issues: finished_each in
  -- each of the three other projects and finished_advances in the advances project.
  select p.*, p.backlog_pairs + p.icebox - p.parked as doubled, p.parked - p.icebox as backlog,
         p.in_progress + p.testing + p.needs_review + p.retro + p.todo + p.triage + p.parked as open_members,
         p.own - (p.backlog_pairs + p.icebox - p.parked) as roots,
         3 * p.finished_each + p.finished_advances as done
  from production p
), rest as (
  select m.*, m.issues - m.firsts - m.done - m.open_members as unadvanced,
         m.todo_all - m.todo as todo_unadvanced, m.backlog_all - m.backlog as backlog_unadvanced,
         m.issues - m.firsts - m.done - m.open_members - m.unplaced - (m.todo_all - m.todo) - (m.backlog_all - m.backlog)
           as shelved_unadvanced,
         m.with_parent - (m.done + m.open_members - m.roots) as children_unadvanced
  from members m
)
select r.*, r.shelved_unadvanced / 2 as icebox_unadvanced, r.shelved_unadvanced - r.shelved_unadvanced / 2 as triage_unadvanced
from rest r;

-- The three projects beside the advances project, each with its place.
create temp table corpus_projects as
select p.key, p.place - 1 as slot
from unnest(string_to_array(current_setting('priorities_read_corpus.projects'), ',')) with ordinality as p(key, place);

-- Every issue the corpus writes, by what it stands for, and its status. In each of the three
-- projects the first issues are finished. After them, in the second and third projects, half in
-- each, stand the open issues that advance no priority (unadvanced). The advances project holds
-- the issues that advance one: first the doubled ones, which state a second priority beside the
-- one they inherit, then the roots, then the open issues below the roots (grown), and then the
-- finished issues of that project. The open issues of each group take its statuses by count in
-- the md5 order of their keys.
create temp table corpus_roles as
with written as (
  select p.key as project, n, p.key || '-' || (n + 1) as key,
         case when n <= c.finished_each then 'finished' else 'unadvanced' end as role
  from corpus_projects p
  cross join corpus_counts c
  cross join lateral generate_series(1, c.finished_each + case when p.slot = 0 then 0 else (c.unadvanced + 2 - p.slot) / 2 end) n
  union all
  select a.key, n, a.key || '-' || (n + 1),
         case when n <= c.doubled then 'doubled' when n <= c.doubled + c.roots then 'root' when n <= c.open_members then 'grown'
              else 'finished' end
  from (select current_setting('priorities_read_corpus.advances_project') as key) a
  cross join corpus_counts c
  cross join lateral generate_series(1, c.open_members + c.finished_advances) n
), placed as (
  select w.*, row_number() over (partition by w.role in ('root', 'grown'), w.role = 'unadvanced'
                                 order by md5('priorities-read-corpus status/' || w.key)) as place
  from written w
)
select p.project, p.n, p.key, p.role, case
         when p.role = 'finished' then 'done'
         when p.role = 'doubled' then 'backlog'
         else (select drawn.status
               from (select shares.status, sum(shares.share) over (order by shares.turn) as reach
                     from unnest(case when p.role = 'unadvanced'
                                      then array['in_progress', 'todo', 'backlog', 'icebox', 'triage']
                                      else array['in_progress', 'testing', 'needs_review', 'retro', 'todo', 'triage', 'icebox', 'backlog'] end,
                                 case when p.role = 'unadvanced'
                                      then array[c.unplaced, c.todo_unadvanced, c.backlog_unadvanced, c.icebox_unadvanced, c.triage_unadvanced]
                                      else array[c.in_progress, c.testing, c.needs_review, c.retro, c.todo, c.triage, c.icebox, c.backlog - c.doubled] end)
                          with ordinality as shares(status, share, turn)) drawn
               where p.place <= drawn.reach
               order by drawn.reach
               limit 1)
       end as status
from placed p
cross join corpus_counts c;

-- Where each issue that advances a priority hangs, and the priority it states or, below a root,
-- inherits. The finished and the grown issues, in the md5 order of their keys, fill the roots in
-- order, each root's issues a binary heap below it (position 1 is the root, and position j hangs
-- under position j / 2). Root r states priority (r - 1) % 13 + 1. A doubled issue d hangs under
-- position 8 of root 10d, four levels down, and states the priority after the one it inherits.
-- The own relations are numbered roots first, then the doubled issues.
create temp table corpus_tree as
with grown as (
  select r.key, row_number() over (order by md5('priorities-read-corpus tree/' || r.key)) - 1 as m
  from corpus_roles r
  where r.role in ('finished', 'grown')
), placed as (
  select g.key, g.m, 10 * (g.m / 40) + at.tree + 1 as root, at.position
  from grown g
  cross join lateral (
    select block.place - 1 as tree, g.m % 40 - block.start + 2 as position
    from unnest(array[0, 1, 3, 4, 7, 9, 10, 14, 16, 19]) with ordinality as block(start, place)
    where block.start <= g.m % 40
    order by block.place desc
    limit 1
  ) at
)
select p.key, case when p.position / 2 = 1 then a.key || '-' || (c.doubled + p.root + 1) else up.key end as parent_key,
       (p.root - 1) % c.priorities + 1 as priority, null::int as own
from placed p
cross join corpus_counts c
cross join (select current_setting('priorities_read_corpus.advances_project') as key) a
left join placed up on up.m = p.m - p.position + p.position / 2 and p.position / 2 > 1
union all
select r.key, null, (r.n - c.doubled - 1) % c.priorities + 1, r.n - c.doubled
from corpus_roles r
cross join corpus_counts c
where r.role = 'root'
union all
select r.key, up.key, 10 * r.n % c.priorities + 1, c.roots + r.n
from corpus_roles r
cross join corpus_counts c
join placed up on up.m = 40 * r.n - 15
where r.role = 'doubled';

-- Every issue the corpus writes, before its row is written: its status, its parent, its life, the
-- count of its events, and whether it is claimed or asked. Each issue takes its place in the draw
-- of the counts by the md5 of its key, and an issue in backlog or icebox the seq of its move there
-- by the next eight hex digits of that md5, from 2 to its count. An issue's first event creates
-- it and every status but todo is reached by a move, so every issue but one in todo has at least
-- two events. A finished issue lives 9 hours, and the ones drawn to have closed in the last 7 days
-- closed from 1 hour to 6 days 1 hour before the corpus is written, the others from 8 to 60 days
-- before; an open issue was made from 1 to 60 days before and last changed 2 hours later. The
-- claims are drawn among the open members in flight, and the asks among the open members that
-- advance one priority.
create temp table corpus_issues as
with drawn as (
  select r.*, coalesce(t.parent_key, u.parent_key) as parent_key, t.own,
         ('x' || substr(md5('priorities-read-corpus moment/' || r.key), 1, 8))::bit(32)::bigint / 4294967296.0 as moment_place,
         row_number() over (partition by r.role = 'finished' order by md5('priorities-read-corpus closed/' || r.key)) as closed_place,
         row_number() over (partition by r.role in ('root', 'grown') and r.status in ('in_progress', 'testing', 'needs_review', 'retro')
                            order by md5('priorities-read-corpus claim/' || r.key)) as claim_place,
         row_number() over (partition by r.role in ('root', 'grown') order by md5('priorities-read-corpus ask/' || r.key)) as ask_place
  from corpus_roles r
  left join corpus_tree t on t.key = r.key
  left join (
    select child.key, top.key as parent_key
    from (select r.key, row_number() over (order by md5('priorities-read-corpus parent/' || r.key)) as place
          from corpus_roles r where r.role = 'unadvanced') child
    cross join corpus_counts c
    join (select r.key, row_number() over (order by md5('priorities-read-corpus parent/' || r.key)) as place
          from corpus_roles r where r.role = 'unadvanced') top
      on top.place = (child.place - (c.unadvanced - c.children_unadvanced) - 1) % (c.unadvanced - c.children_unadvanced) + 1
    where child.place > c.unadvanced - c.children_unadvanced
  ) u on u.key = r.key
), lived as (
  select d.*,
         case when d.role <> 'finished' then null
              when d.closed_place <= c.closed_recently then c.written_at - interval '1 hour' - d.moment_place * interval '6 days'
              else c.written_at - interval '8 days' - d.moment_place * interval '52 days' end as closed_at,
         c.written_at - interval '1 day' - d.moment_place * interval '59 days' as opened_at,
         d.role in ('root', 'grown') and d.status in ('in_progress', 'testing', 'needs_review', 'retro') and d.claim_place <= c.claimed as claimed,
         d.role in ('root', 'grown') and d.ask_place <= c.asks as asked
  from drawn d
  cross join corpus_counts c
), placed as (
  select l.*, case when l.role = 'finished' then 'Finished work item ' || l.n || ' of project ' || l.project
                   else 'Open work item ' || l.n || ' of project ' || l.project end as title,
         coalesce(l.closed_at - interval '9 hours', l.opened_at) as created_at,
         coalesce(l.closed_at, l.opened_at + interval '2 hours') as updated_at,
         (row_number() over (order by md5('priorities-read-corpus events/' || l.key)) - 0.5)::float8 / count(*) over () as place,
         ('x' || substr(md5('priorities-read-corpus events/' || l.key), 9, 8))::bit(32)::bigint / 4294967296.0 as move_place
  from lived l
)
select p.project, p.n, p.key, p.role, p.title, p.status, p.parent_key, p.own, p.created_at, p.updated_at, p.closed_at,
       p.claimed, p.asked, drawn.events,
       case when p.status in ('backlog', 'icebox') then 2 + floor(p.move_place * (drawn.events - 1))::int end as moved_at
from placed p
-- The quantile at the place: log-linear through 1 at 0, production's p50 of 16 and p90 of 118,
-- 425 at 0.99 and 900 at the top, which hold the mean at 51.
cross join lateral (
  select greatest(case when p.status = 'todo' then 1 else 2 end, round(case
    when p.place < 0.5 then power(16, p.place / 0.5)
    when p.place < 0.9 then 16 * power(118 / 16.0, (p.place - 0.5) / 0.4)
    when p.place < 0.99 then 118 * power(425 / 118.0, (p.place - 0.9) / 0.09)
    else 425 * power(900 / 425.0, (p.place - 0.99) / 0.01) end)::int) as events
) drawn;

-- The issues, made by alice as every seeded row is (seededWriter). A parent in another project is
-- written in the same statement, which checks its key at the end. Each row's board key is its
-- number, zero-padded as seededRank writes it, and its last_seq is the seq of its newest event,
-- below; a claimed issue is held by a session since its last change.
insert into issues (key, project_key, number, title, status, created_by, rank, parent_key, created_at, updated_at,
                    closed_at, last_seq, claimed_by, claimed_at)
select i.key, i.project, i.n + 1, i.title, i.status, '{"kind": "user", "id": "alice"}'::jsonb, lpad((i.n + 1)::text, 8, '0'),
       i.parent_key, i.created_at, i.updated_at, i.closed_at, i.events,
       case when i.claimed then '{"kind": "session", "id": "session-0123456789abcdef"}'::jsonb end,
       case when i.claimed then i.updated_at end
from corpus_issues i
order by i.project = current_setting('priorities_read_corpus.advances_project'), i.project, i.n;

update projects p set next_number = (select max(number) + 1 from issues i where i.project_key = p.key)
where p.key = any (string_to_array(current_setting('priorities_read_corpus.projects'), ',') || current_setting('priorities_read_corpus.advances_project'));

-- The priorities (prioritystore.Create): a node row whose attributes are what the person wrote, made
-- by that person with no event before any issue, and the tuples Create states, in its order.
insert into priority_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'priority', 'priority-' || lpad(p::text, 2, '0'), 'priority', 'project', 'live',
       jsonb_build_object(
         'title', 'Priority ' || p || ': ship what the people on the work board need first',
         'description', 'Work that moves priority ' || p || ' forward, written by its owner as one paragraph of why it matters now.',
         'owner', 'owner' || (p % 7),
         'rank', 'a' || lpad(p::text, 4, '0')),
       c.written_at - interval '61 days' + p * interval '1 hour',
       '{"kind": "user", "id": "alice"}'::jsonb, 0
from corpus_counts c cross join lateral generate_series(1, c.priorities) p;

insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'priority', n.id, stated.kind, 'value', stated.value, n.made_at, n.made_by, 0, true
from priority_nodes n
cross join lateral (values
  (1, 'visibility', 'project'),
  (2, 'state', 'live'),
  (3, 'description', n.attributes->>'description'),
  (4, 'owner', n.attributes->>'owner'),
  (5, 'rank', n.attributes->>'rank'),
  (6, 'title', n.attributes->>'title')
) as stated(place, kind, value)
order by n.made_at, n.id, stated.place;

-- The advances tuples (prioritystore.Advance): stated on the issue, its far end the priority, 10
-- minutes into the issue's life; an own relation whose number is a multiple of 8 follows an assert
-- and a retraction of the priority five after it.
insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'issue', i.key, 'advances', 'priority', 'priority-' || lpad(stated.priority::text, 2, '0'),
       i.created_at + stated.after, '{"kind": "user", "id": "alice"}'::jsonb, 0, stated.asserted
from corpus_issues i
join corpus_tree t on t.key = i.key
cross join corpus_counts c
cross join lateral (values
  (1, interval '4 minutes', (t.priority + 4) % c.priorities + 1, true),
  (2, interval '7 minutes', (t.priority + 4) % c.priorities + 1, false),
  (3, interval '10 minutes', t.priority, true)
) as stated(turn, after, priority, asserted)
where t.own is not null and (stated.turn = 3 or t.own % 8 = 0)
order by i.created_at + stated.after, i.key, stated.turn;

-- What an issue's events say of it beside its row (api's newIssueEventPayload): its ancestors,
-- parent first, and the priorities it advances, its own and those it inherits, in id order, with
-- the greatest seq of its own advances tuples. Every event of an issue says what its row and the
-- tuples above hold now.
create temp table corpus_issue_terms as
with recursive own as (
  select newest.from_id as key,
         coalesce(array_agg(newest.to_id order by newest.to_id) filter (where newest.asserted and live.id is not null), '{}') as ids,
         max(newest.seq) as revision
  from (
    select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.asserted, t.seq
    from priority_tuples t
    where t.from_kind = 'issue' and t.kind = 'advances' and t.to_kind = 'priority'
    order by t.from_id, t.to_id, t.seq desc
  ) newest
  left join priority_nodes live on live.kind = 'priority' and live.id = newest.to_id and live.state = 'live'
  group by newest.from_id
), chain as (
  select i.key, i.parent_key as above, 1 as depth from corpus_issues i where i.parent_key is not null
  union all
  select c.key, up.parent_key, c.depth + 1
  from chain c join corpus_issues up on up.key = c.above
  where up.parent_key is not null
), entries as (
  select own.key, stated.id, false as inherited from own cross join unnest(own.ids) stated(id)
  union
  select c.key, stated.id, true
  from chain c join own up on up.key = c.above cross join unnest(up.ids) stated(id)
  where not exists (select 1 from own mine where mine.key = c.key and stated.id = any (mine.ids))
)
select i.key, coalesce(ancestry.keys, '[]') as ancestors, coalesce(advanced.entries, '[]') as advances,
       coalesce(own.revision, 0) as revision
from corpus_issues i
left join own on own.key = i.key
left join (select key, jsonb_agg(above order by depth) as keys from chain group by key) ancestry on ancestry.key = i.key
left join (select key, jsonb_agg(jsonb_build_object('id', id, 'inherited', inherited) order by id collate "C") as entries
           from entries group by key) advanced on advanced.key = i.key;

-- The events of each issue, as events.Broker's append writes them: seq 1 to the issue's count,
-- each at its share of the issue's life from created_at to updated_at, all of them in the order of
-- their moments across the issues. Seq 1 is the issue.created, written by the issue's creator in
-- todo. An issue in backlog or icebox has the issue.updated that moved it there at the seq drawn
-- for it, which the parked read (api's parkedMoveLateral) walks back to from the newest. A
-- finished issue has the issue.updated that moved it to in_progress at seq 2 and its issue.closed
-- last, and an issue in any other status but todo the issue.updated that moved it there at seq 2.
-- Of the other seqs every third is a comment.created and the rest an issue.updated of one field;
-- one in four is a person's, the issue's creator's, and the rest a session's. The payloads are
-- api's IssueEventPayload, last_seq the event's own seq, and CommentEventPayload. notify is what
-- events.Broker.Notify answers, true for a person's write. Each event is published to its issue's
-- topic a second after it is written, so the outbox owes it nothing.
insert into events (issue_key, seq, type, actor, payload, notify, published_at, published_destinations, created_at)
select i.key, k, step.type, step.actor,
       case when step.type = 'comment.created' then jsonb_build_object(
         'id', md5('comment.created/' || i.key || '/' || k)::uuid::text, 'issue_key', i.key, 'artifact_id', null,
         'author', step.actor,
         'body', 'Comment ' || k || ' on ' || i.key || '. The step finished and its report is attached. It read the plan, '
           'ran the checks the plan names and kept the output of each beside its command. Two checks passed at once; the '
           'third failed on a fixture older than the change and passed once the fixture was rewritten to the shape the '
           'writer stores today. Nothing outside the files in scope moved. The next step can start from the head this '
           'comment names, and the reviewer can read the diff, the checks and their output without asking for more.',
         'anchor', null, 'reply_to', null, 'answers', null, 'ask_id', null, 'turn', null, 'resolved', false,
         'resolved_by', null, 'resolved_at', null, 'edited_at', null, 'suggestion', null, 'created_at', step.stamp,
         'mentions', '[]'::jsonb, 'deliveries', '[]'::jsonb, 'reply_block', null, 'artifact_name', '',
         'suppress_route', false, 'thread_logins', '[]'::jsonb)
       else jsonb_build_object(
         'key', i.key, 'project', i.project, 'number', stored.number, 'title', stored.title, 'status', step.status,
         'priority', stored.priority, 'rank', stored.rank, 'labels', to_jsonb(stored.labels), 'parent', stored.parent_key,
         'needs', '[]'::jsonb, 'duplicate_of', null, 'filed_from', null, 'advances', terms.advances,
         'advances_revision', terms.revision, 'assignee', stored.assignee,
         'claim', case when stored.claimed_by is not null then jsonb_build_object('actor', stored.claimed_by,
                    'at', to_char(stored.claimed_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) end,
         'components', '{"mode": "inherit", "ids": [], "unknown": [], "reason": null, "inherited_from": null}'::jsonb,
         'external_links', '[]'::jsonb, 'route', null, 'repo', null, 'resolved_repo', null,
         'created_by', stored.created_by,
         'created_at', to_char(stored.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
         'updated_at', step.stamp,
         'closed_at', case when step.type = 'issue.closed' then to_char(stored.closed_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') end,
         'primary_artifact_id', '', 'verification_artifact_id', '', 'plan_artifact_id', '', 'last_seq', k,
         'changed', step.changed, 'ancestors', terms.ancestors)
         || case when k > 1 and step.changed ? 'status' then '{"thread_logins": []}'::jsonb else '{}'::jsonb end
       end,
       step.actor->>'kind' = 'user', moment.at + interval '1 second',
       array['notifications.dispatch.issue.' || i.key || '.' || step.type], moment.at
from corpus_issues i
join issues stored on stored.key = i.key
join corpus_issue_terms terms on terms.key = i.key
cross join lateral generate_series(1, i.events) k
cross join lateral (
  select case
           when k = 1 then 'created'
           when i.status = 'done' and k = i.events then 'closed'
           when k = i.moved_at or (i.status not in ('todo', 'backlog', 'icebox') and k = 2) then 'moved'
           when k % 3 = 0 then 'comment'
           else 'edited'
         end as kind,
         i.created_at + (i.updated_at - i.created_at) * ((k - 1)::float8 / greatest(i.events - 1, 1)) as at
) moment
cross join lateral (
  select case moment.kind when 'created' then 'issue.created' when 'closed' then 'issue.closed'
                          when 'comment' then 'comment.created' else 'issue.updated' end as type,
         case when k = 1 or k % 4 = 0 then stored.created_by
              else '{"kind": "session", "id": "session-0123456789abcdef"}'::jsonb end as actor,
         case
           when k = 1 then 'todo'
           when i.moved_at is not null and k < i.moved_at then 'todo'
           when i.status = 'done' and k < i.events then 'in_progress'
           else i.status
         end as status,
         case moment.kind
           when 'created' then case when stored.parent_key is null then '["title", "status", "rank", "components"]'::jsonb
                                    else '["title", "status", "rank", "parent", "components"]'::jsonb end
           when 'closed' then '["status", "closed_at"]'::jsonb
           when 'moved' then '["status"]'::jsonb
           when 'edited' then jsonb_build_array((array['title', 'rank', 'labels', 'priority'])[k % 4 + 1])
         end as changed,
         to_char(moment.at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as stamp
) step
order by moment.at, i.key, k;

-- The open asks, one on each issue drawn for one (the ask writer's row: its author the issue's
-- creator, open, a question with no options), asked at the issue's last change.
insert into asks (issue_key, author, question, created_at)
select i.key, stored.created_by,
       'Which of the two shapes the plan names should the next step of ' || i.key || ' take: the one that keeps the reads '
         'where they are, or the one that moves them beside the writes?',
       i.updated_at
from corpus_issues i
join issues stored on stored.key = i.key
where i.asked
order by i.updated_at, i.key;

drop table corpus_counts, corpus_projects, corpus_roles, corpus_tree, corpus_issues, corpus_issue_terms;
