-- The corpus of c-moved-read-cost, which TestNoMovedReadGetsSlower (moved_read_cost_test.go) writes
-- by SQL over the database of the release before, once that build has migrated it and is stopped.
-- Each row has the shape its writer in that release stores: prioritystore's Create and relate for
-- the priorities and the advances tuples, workflowstore's putNode for the runs and the plans,
-- events.Broker's append for the events of the issues, and the ask writer's row for the open asks.
-- These are the shapes of the R32 bench's corpus (dispatch://INFRA-1098/artifact/r32-bench-tar-gz),
-- with the events added. No value is random: what is drawn is drawn by the md5 of a fixed text.
-- Every moment of an issue, a priority and an advances tuple is a fixed time before the moment
-- the corpus is written, so a close in the last 7 days reads as one whenever the run is.
--
-- Three settings name what the run made through the release before's own API before this file runs:
--   moved_read_cost.scale             1 for the check's corpus; 0.1 a tenth. Every count of issues and
--                                     of tuples of other kinds is scaled; the 13 priorities are not.
--                                     An issue keeps its runs and its children at every scale.
--   moved_read_cost.projects          the keys of the three projects, comma-separated. The first is
--                                     the project the steps read and the plans read address.
--   moved_read_cost.advances_project  the key of the project of the open issues that advance a
--                                     priority.
-- Each of the four projects stands, with its first issue, <key>-1, which that build's issue writer
-- made. Every issue below copies that issue's row and its node, so it has the columns that build
-- writes, and <key>-1 stays open, in triage, under no priority.
--
-- The workflow side is the check's. In each of the three projects, issue number n + 1 is the
-- corpus's issue n, and n = 1 to 3,461 are finished: status done, closed_at set.
--   * n = 1 has 200 runs, the plan they ran from and 339 other part_of children: 34 comments, 4
--     asks and 2 messages, as production held under INFRA-1098, and 20 artifacts and 279 blocks,
--     as dispatch://INFRA-1253 puts under it;
--   * n = 5, 10, ..., 3,000 are 600 issues with 30 runs each, the plan they ran from and 100 other
--     part_of children: 50 comments, 35 blocks, 10 asks and 5 messages;
--   * the other 2,860 of n = 1 to 3,461 are finished issues Legion never ran, which the plans read
--     lists beside the 601 it ran.
-- The first project's 2,860 finished issues Legion never ran are among production's issues,
-- below: each advances a priority, as every finished issue of production's shape does. The 5,720
-- of the two other projects stand beside production's issues, and not among them: each advances
-- no priority, has no parent, and closed from 8 to 60 days before the corpus is written. The test
-- counts them apart from production's issues. Every issue, these among them, has its events drawn
-- from production's events per issue, below. Beside all the issues stand 1,000,000 tuples of other
-- kinds, split by the tuples_a_day of each `census: sizing tuples-a-write` line that
-- cybermonklabs/legion#3178's body records.
--
-- Every issue but those 5,720 takes production's shape, as its API answered at 2026-11-23T23:20Z,
-- in place of the 50 priorities and 5,000 advances tuples over 2,000 issues of the check's
-- Verification version 150:
--   * 13,298 issues, the four first issues, the 1,803 issues Legion ran and the first project's
--     2,860 finished issues it never ran among them, and 13 live priorities.
--   * Production has 7,677 issues that advance a priority (57.7%), 1,600 of them by a relation of
--     their own (12%). Here each of 1,593 open issues of the advances project states one priority
--     (a root), and every finished issue of production's shape and every other open issue of that
--     project hangs below a root, at most four levels down: the roots in order hold capacities of
--     1, 2, 1, 3, 2, 1, 4, 2, 3 and 21 issues below them, ten at a time, and each takes the next
--     issues in the md5 order of their keys as a binary heap, so a root of 21 reaches four levels.
--     The other 7 own relations are on parked issues four levels down that state a second priority
--     beside the one they inherit: the backlog and icebox pairs (525 + 211) are 7 more than the
--     parked issues (729). The first of them is <advances project>-2. Every own relation whose
--     place is a multiple of 8 is preceded by an assert and a retraction of another priority on
--     the same issue. All 4,663 finished issues of production's shape advance a priority, the
--     1,803 Legion ran and the first project's 2,860 it never ran, so 7,651 issues do: production's
--     2,995 open pairs leave its 7,677 room for no more finished ones than its 35% done.
--   * The open pairs of an issue and a priority by status are production's: in_progress 972, todo
--     826, backlog 525, triage 217, icebox 211, needs_review 119, testing 104 and retro 21. 622
--     of them are claimed by a session, 78 open asks stand on them, one each, and 1,461 finished
--     pairs closed in the last 7 days. Parked (backlog or icebox, advancing a priority): 729.
--   * The open issues of the other two projects, numbered after their finished ones, advance no
--     priority: 115 in in_progress (production's unplaced issues), and as many in todo and backlog
--     as bring all issues to production's 17% and 32%; the rest are in icebox and triage, half
--     each. The 4,663 finished issues are 35.1% (production's 35% is 4,654), and with the 1,216
--     open members in flight the unplaced make production's 10% in flight. As many of these issues
--     have a parent among them as bring all issues to production's 68% with a parent.
--   * Each issue the corpus writes, of production's shape or beside it, has its events, seq 1 to its
--     count. The counts are drawn from production's events per issue (mean 51, p50 16, p90 118, p99
--     425, at most 937: the newest event's seq of 120 issues that advance a priority). Each issue
--     of production's shape takes that distribution's quantile at its place in the md5 order of
--     these issues' keys, and each issue beside them at its place among those beside, so every
--     scale draws the same distribution in each and only the count of issues scales.

create temp table moved_counts as
with production as (
  -- Production's counts at the run's scale, and the moment the corpus is written.
  select now() as written_at, 13 as priorities,
         cardinality(string_to_array(current_setting('moved_read_cost.projects'), ',')) + 1 as templates,
         round(13298 * s)::int as issues, round(3461 * s)::int as finished, round(600 * s)::int as ran,
         round(1000000 * s)::bigint as others,
         round(972 * s)::int as in_progress, round(104 * s)::int as testing, round(119 * s)::int as needs_review,
         round(21 * s)::int as retro, round(826 * s)::int as todo, round(217 * s)::int as triage,
         round(211 * s)::int as icebox, round(525 * s)::int as backlog_pairs, round(729 * s)::int as parked,
         round(1600 * s)::int as own, round(115 * s)::int as unplaced, round(0.17 * 13298 * s)::int as todo_all,
         round(0.32 * 13298 * s)::int as backlog_all, round(0.68 * 13298 * s)::int as with_parent,
         round(622 * s)::int as claimed, round(78 * s)::int as asks, round(1461 * s)::int as closed_recently
  from (select current_setting('moved_read_cost.scale')::numeric as s) setting
), members as (
  -- The open members by status, the parked ones that advance two priorities among the backlog's,
  -- the roots that state the rest of the own relations, the finished issues Legion never ran in
  -- each project (unran), and the finished issues of production's shape (done): the 3 x (ran + 1)
  -- Legion ran, and the first project's never-ran ones.
  select p.*, p.backlog_pairs + p.icebox - p.parked as doubled, p.parked - p.icebox as backlog,
         p.in_progress + p.testing + p.needs_review + p.retro + p.todo + p.triage + p.parked as open_members,
         p.own - (p.backlog_pairs + p.icebox - p.parked) as roots, p.finished - p.ran - 1 as unran,
         p.finished + 2 * (p.ran + 1) as done
  from production p
), rest as (
  select m.*, m.issues - m.templates - m.done - m.open_members as unadvanced,
         m.todo_all - m.todo as todo_unadvanced, m.backlog_all - m.backlog as backlog_unadvanced,
         m.issues - m.templates - m.done - m.open_members - m.unplaced - (m.todo_all - m.todo) - (m.backlog_all - m.backlog)
           as shelved_unadvanced,
         m.with_parent - (m.done + m.open_members - m.roots) as children_unadvanced
  from members m
)
select r.*, r.shelved_unadvanced / 2 as icebox_unadvanced, r.shelved_unadvanced - r.shelved_unadvanced / 2 as triage_unadvanced
from rest r;

-- The projects of the workflow side, each with the moment the check's corpus starts its runs at.
create temp table moved_projects as
select p.key, p.place - 1 as slot,
       timestamptz '2026-11-16 22:47:00+00' + (p.place - 1) * interval '5 days' as t0
from unnest(string_to_array(current_setting('moved_read_cost.projects'), ',')) with ordinality as p(key, place);

-- Every issue the corpus writes, by what it stands for, and its status. In each of the three
-- projects the first issues are finished: those Legion ran, and the others, which Legion never
-- ran. Those Legion ran, and the first project's others, are of production's shape (finished);
-- the never-ran ones of the second and third projects stand beside them (beside). After them, in
-- the second and third projects, half in each, stand the open issues that advance no priority
-- (unadvanced). The advances project holds the open issues that advance one: first the doubled
-- ones, which state a second priority beside the one they inherit, then the roots, then the open
-- issues below the roots (grown). The open issues of each group take its statuses by count in the
-- md5 order of their keys.
create temp table moved_roles as
with written as (
  select p.key as project, n, p.key || '-' || (n + 1) as key,
         case when n = 1 or (n % 5 = 0 and n <= 5 * c.ran) or (p.slot = 0 and n <= c.finished) then 'finished'
              when n <= c.finished then 'beside'
              else 'unadvanced' end as role
  from moved_projects p
  cross join moved_counts c
  cross join lateral generate_series(1, c.finished + case when p.slot = 0 then 0 else (c.unadvanced + 2 - p.slot) / 2 end) n
  union all
  select a.key, n, a.key || '-' || (n + 1),
         case when n <= c.doubled then 'doubled' when n <= c.doubled + c.roots then 'root' else 'grown' end
  from (select current_setting('moved_read_cost.advances_project') as key) a
  cross join moved_counts c
  cross join lateral generate_series(1, c.open_members) n
), placed as (
  select w.*, row_number() over (partition by w.role in ('root', 'grown'), w.role = 'unadvanced'
                                 order by md5('moved-read-cost status/' || w.key)) as place
  from written w
)
select p.project, p.n, p.key, p.role, case
         when p.role in ('finished', 'beside') then 'done'
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
cross join moved_counts c;

-- Where each issue that advances a priority hangs, and the priority it states or, below a root,
-- inherits. The finished and the grown issues, in the md5 order of their keys, fill the roots in
-- order, each root's issues a binary heap below it (position 1 is the root, and position j hangs
-- under position j / 2). Root r states priority (r - 1) % 13 + 1. A doubled issue d hangs under
-- position 8 of root 10d, four levels down, and states the priority after the one it inherits.
-- The own relations are numbered roots first, then the doubled issues.
create temp table moved_tree as
with grown as (
  select r.key, row_number() over (order by md5('moved-read-cost tree/' || r.key)) - 1 as m
  from moved_roles r
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
cross join moved_counts c
cross join (select current_setting('moved_read_cost.advances_project') as key) a
left join placed up on up.m = p.m - p.position + p.position / 2 and p.position / 2 > 1
union all
select r.key, null, (r.n - c.doubled - 1) % c.priorities + 1, r.n - c.doubled
from moved_roles r
cross join moved_counts c
where r.role = 'root'
union all
select r.key, up.key, 10 * r.n % c.priorities + 1, c.roots + r.n
from moved_roles r
cross join moved_counts c
join placed up on up.m = 40 * r.n - 15
where r.role = 'doubled';

-- Every issue the corpus writes, before its row is written: its status, its parent, its life, the
-- count of its events, and whether it is claimed or asked. Each issue takes its place in the draw
-- of the counts by the md5 of its key, those of production's shape among themselves and those
-- beside them among themselves, and an issue in backlog or icebox the seq of its move there by the
-- next eight hex digits of that md5, from 2 to its count. An issue's first event creates it and
-- every status but todo is reached by a move, so every issue but one in todo has at least two
-- events. A finished issue lives 9 hours, and the ones of production's shape drawn to have closed
-- in the last 7 days closed from 1 hour to 6 days 1 hour before the corpus is written, the others
-- from 8 to 60 days before; an open issue was made from 1 to 60 days before and last changed 2
-- hours later. The claims are drawn among the open members in flight, and the asks among the open
-- members that advance one priority.
create temp table moved_issues as
with drawn as (
  select r.*, coalesce(t.parent_key, u.parent_key) as parent_key, t.own,
         ('x' || substr(md5('moved-read-cost moment/' || r.key), 1, 8))::bit(32)::bigint / 4294967296.0 as moment_place,
         row_number() over (partition by r.role = 'finished' order by md5('moved-read-cost closed/' || r.key)) as closed_place,
         row_number() over (partition by r.role in ('root', 'grown') and r.status in ('in_progress', 'testing', 'needs_review', 'retro')
                            order by md5('moved-read-cost claim/' || r.key)) as claim_place,
         row_number() over (partition by r.role in ('root', 'grown') order by md5('moved-read-cost ask/' || r.key)) as ask_place
  from moved_roles r
  left join moved_tree t on t.key = r.key
  left join (
    select child.key, top.key as parent_key
    from (select r.key, row_number() over (order by md5('moved-read-cost parent/' || r.key)) as place
          from moved_roles r where r.role = 'unadvanced') child
    cross join moved_counts c
    join (select r.key, row_number() over (order by md5('moved-read-cost parent/' || r.key)) as place
          from moved_roles r where r.role = 'unadvanced') top
      on top.place = (child.place - (c.unadvanced - c.children_unadvanced) - 1) % (c.unadvanced - c.children_unadvanced) + 1
    where child.place > c.unadvanced - c.children_unadvanced
  ) u on u.key = r.key
), lived as (
  select d.*,
         case when d.role not in ('finished', 'beside') then null
              when d.role = 'finished' and d.closed_place <= c.closed_recently
                then c.written_at - interval '1 hour' - d.moment_place * interval '6 days'
              else c.written_at - interval '8 days' - d.moment_place * interval '52 days' end as closed_at,
         c.written_at - interval '1 day' - d.moment_place * interval '59 days' as opened_at,
         d.role in ('root', 'grown') and d.status in ('in_progress', 'testing', 'needs_review', 'retro') and d.claim_place <= c.claimed as claimed,
         d.role in ('root', 'grown') and d.ask_place <= c.asks as asked
  from drawn d
  cross join moved_counts c
), placed as (
  select l.*, case when l.role in ('finished', 'beside') then 'Finished work item ' || l.n || ' of project ' || l.project
                   else 'Open work item ' || l.n || ' of project ' || l.project end as title,
         coalesce(l.closed_at - interval '9 hours', l.opened_at) as created_at,
         coalesce(l.closed_at, l.opened_at + interval '2 hours') as updated_at,
         (row_number() over (partition by l.role = 'beside' order by md5('moved-read-cost events/' || l.key)) - 0.5)::float8
           / count(*) over (partition by l.role = 'beside') as place,
         ('x' || substr(md5('moved-read-cost events/' || l.key), 9, 8))::bit(32)::bigint / 4294967296.0 as move_place
  from lived l
)
select p.project, p.n, p.key, p.role, p.title, p.status, p.parent_key, p.own, p.created_at, p.updated_at, p.closed_at,
       p.claimed, p.asked, drawn.events,
       case when p.status in ('backlog', 'icebox') then 2 + floor(p.move_place * (drawn.events - 1))::int end as moved_at
from placed p
-- The quantile at the place: log-linear through 1 at 0, production's p50 of 16 and p90 of 118, 425
-- at 0.99 and 900 at the top, which hold the mean at 51.
cross join lateral (
  select greatest(case when p.status = 'todo' then 1 else 2 end, round(case
    when p.place < 0.5 then power(16, p.place / 0.5)
    when p.place < 0.9 then 16 * power(118 / 16.0, (p.place - 0.5) / 0.4)
    when p.place < 0.99 then 118 * power(425 / 118.0, (p.place - 0.9) / 0.09)
    else 425 * power(900 / 425.0, (p.place - 0.99) / 0.01) end)::int) as events
) drawn;

-- The issues, each a copy of its project's first issue. A parent in another project is written in
-- the same statement, which checks its key at the end. Each row's last_seq is the seq of its newest
-- event, below; a claimed issue is held by a session since its last change.
insert into issues (key, project_key, number, title, status, created_by, labels, priority, rank, node_kind, node_address,
                    parent_key, created_at, updated_at, closed_at, last_seq, claimed_by, claimed_at)
select i.key, i.project, i.n + 1, i.title, i.status,
       template.created_by, template.labels, template.priority, template.rank || lpad(i.n::text, 7, '0'), template.node_kind,
       i.key, i.parent_key, i.created_at, i.updated_at, i.closed_at, i.events,
       case when i.claimed then '{"kind": "session", "id": "session-0123456789abcdef"}'::jsonb end,
       case when i.claimed then i.updated_at end
from moved_issues i
join issues template on template.key = i.project || '-1'
order by i.project = current_setting('moved_read_cost.advances_project'), i.project, i.n;

update projects p set next_number = (select max(number) + 1 from issues i where i.project_key = p.key)
where p.key = any (string_to_array(current_setting('moved_read_cost.projects'), ',') || current_setting('moved_read_cost.advances_project'));

-- Each issue's node, a copy of its project's first issue's node.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
select template.kind, i.key, template.supertag, template.visibility, template.state, template.attributes, i.created_at,
       template.made_by, template.source_event, template.project_key, template.parties
from issues i
join graph_nodes template on template.kind = 'issue' and template.id = i.project_key || '-1'
where i.number > 1
  and i.project_key = any (string_to_array(current_setting('moved_read_cost.projects'), ',') || current_setting('moved_read_cost.advances_project'));

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
       '{"kind": "person", "id": "owner"}'::jsonb, 0
from moved_counts c cross join lateral generate_series(1, c.priorities) p;

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

-- The advances tuples (prioritystore relate): stated on the issue, its far end the priority, 10
-- minutes into the issue's life; an own relation whose number is a multiple of 8 follows an assert
-- and a retraction of the priority five after it.
insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'issue', i.key, 'advances', 'priority', 'priority-' || lpad(stated.priority::text, 2, '0'),
       i.created_at + stated.after, '{"kind": "person", "id": "owner"}'::jsonb, 0, stated.asserted
from moved_issues i
join moved_tree t on t.key = i.key
cross join moved_counts c
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
create temp table moved_issue_terms as
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
  select i.key, i.parent_key as above, 1 as depth from moved_issues i where i.parent_key is not null
  union all
  select c.key, up.parent_key, c.depth + 1
  from chain c join moved_issues up on up.key = c.above
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
from moved_issues i
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
from moved_issues i
join issues stored on stored.key = i.key
join moved_issue_terms terms on terms.key = i.key
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
-- creator, open, a question with no options), asked at the issue's last change. As the release
-- before writes an ask, the row names no node yet, and its blocks and its references are owed
-- (the asks' triggers), which that build's block-index tick pays.
insert into asks (issue_key, author, question, created_at)
select i.key, stored.created_by,
       'Which of the two shapes the plan names should the next step of ' || i.key || ' take: the one that keeps the reads '
         'where they are, or the one that moves them beside the writes?',
       i.updated_at
from moved_issues i
join issues stored on stored.key = i.key
where i.asked
order by i.updated_at, i.key;

-- The issues Legion ran: in each project, issue 1 with 200 runs and 339 children, and every fifth
-- issue up to the 600th with 30 runs and 100 children.
create temp table moved_ran as
select p.key as project, p.slot, p.t0, p.key || '-2' as issue, 1 as n, 200 as runs, true as busy
from moved_projects p
union all
select p.key, p.slot, p.t0, p.key || '-' || (5 * k + 1), 5 * k, 30, false
from moved_projects p cross join moved_counts c cross join lateral generate_series(1, c.ran) k;

-- The runs, one row each, with every field the daemon states: generations of eight steps, a retro
-- and a merge; the fourth step of the first two generations failed and returned to step 2.
create temp table moved_runs as
select md5('step/' || r.issue || '/' || k)::uuid::text as id, r.project, r.issue,
       (k - 1) / 10 + 1 as generation,
       case when j < 8 then 'step' when j = 8 then 'retro' else 'merge' end as kind,
       case when j < 8 then j + 1 else 0 end as place,
       (array['planner', 'implementer', 'tester', 'reviewer', 'implementer', 'tester', 'reviewer', 'implementer', 'implementer', 'merger'])[j + 1] as role,
       case when j < 8 then 'code' end as type,
       'session-' || substr(md5('session/' || r.issue || '/' || k), 1, 16) as session,
       r.t0 + r.n * interval '2 minutes' + k * interval '15 minutes' as started_at,
       r.t0 + r.n * interval '2 minutes' + k * interval '15 minutes' + interval '12 minutes' as ended_at,
       case when j = 3 and (k - 1) / 10 + 1 < 3 then 'failed' else 'passed' end as result,
       case when j = 3 and (k - 1) / 10 + 1 < 3 then 2 end as returned_to,
       md5('handoff/' || r.issue || '/' || k)::uuid::text as report,
       case when j = 8 then 1 + (r.n + k) % 5 end as rating
from moved_ran r
cross join lateral generate_series(1, r.runs) k
cross join lateral (select (k - 1) % 10 as j) place;

-- Each run's writes, in the order StepWrite.attributes states them: the start write ends with the
-- part_of to the issue, the end write with the reports to the handoff, and a retro's rating is a
-- third write.
create temp table moved_run_tuples as
select run.id, stated.at, stated.place, stated.kind, stated.to_kind, stated.to_id
from moved_runs run
cross join lateral (values
  (run.started_at, 1, 'issue', 'value', run.issue),
  (run.started_at, 2, 'project', 'value', run.project),
  (run.started_at, 3, 'generation', 'value', run.generation::text),
  (run.started_at, 4, 'task', 'value', 'default'),
  (run.started_at, 5, 'kind', 'value', run.kind),
  (run.started_at, 6, 'step', 'value', run.place::text),
  (run.started_at, 7, 'role', 'value', run.role),
  (run.started_at, 8, 'type', 'value', run.type),
  (run.started_at, 9, 'agent', 'value', 'claude-code'),
  (run.started_at, 10, 'session', 'value', run.session),
  (run.started_at, 11, 'model', 'value', 'claude-opus'),
  (run.started_at, 12, 'started_at', 'value', to_char(run.started_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
  (run.started_at, 13, 'part_of', 'issue', run.issue),
  (run.ended_at, 14, 'result', 'value', run.result),
  (run.ended_at, 15, 'ended_at', 'value', to_char(run.ended_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
  (run.ended_at, 16, 'returned_to', 'value', run.returned_to::text),
  (run.ended_at, 17, 'report', 'value', run.report),
  (run.ended_at, 18, 'reports', 'handoff', run.report),
  (run.ended_at + interval '1 minute', 19, 'rating', 'value', run.rating::text),
  (run.ended_at + interval '1 minute', 20, 'rating_reason', 'value',
     case when run.rating is not null then 'The plan named the steps the work took, in the order it took them.' end)
) as stated(at, place, kind, to_kind, to_id)
where stated.to_id is not null;

insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'step', run.id, 'step', 'project', 'live',
       jsonb_strip_nulls(jsonb_build_object(
         'issue', run.issue, 'project', run.project, 'generation', run.generation, 'task', 'default',
         'kind', run.kind, 'step', run.place, 'role', run.role, 'type', run.type, 'agent', 'claude-code',
         'session', run.session, 'model', 'claude-opus',
         'started_at', to_char(run.started_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
         'result', run.result,
         'ended_at', to_char(run.ended_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
         'returned_to', run.returned_to, 'report', run.report, 'rating', run.rating,
         'rating_reason', case when run.rating is not null then 'The plan named the steps the work took, in the order it took them.' end)),
       run.started_at, jsonb_build_object('kind', 'session', 'id', run.session), 0
from moved_runs run
order by run.started_at, run.id;

-- The plan each issue ran from: an owner's document, its newest generation, written once before its
-- first run (PlanWrite.attributes, then the part_of).
create temp table moved_plans as
select md5('plan/' || r.issue)::uuid::text as id, r.project, r.issue,
       md5('artifact/' || r.issue)::uuid::text as artifact_id,
       r.t0 + r.n * interval '2 minutes' + interval '6 minutes' as made_at,
       '[{"id":"default","steps":["planner","implementer","tester","reviewer","implementer","tester","reviewer","implementer"]}]' as tasks
from moved_ran r;

insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'plan', plan.id, 'plan', 'project', 'live',
       jsonb_build_object('issue', plan.issue, 'project', plan.project, 'generation', 3, 'source', 'owner',
                          'artifact_id', plan.artifact_id, 'version', 2, 'tasks', plan.tasks::jsonb),
       plan.made_at, '{"kind": "session", "id": "session-legion-daemon"}'::jsonb, 0
from moved_plans plan
order by plan.made_at;

-- Every tuple of the runs and the plans goes in by its moment, so the runs of the issues interleave
-- in the log as the daemon writes them.
insert into workflow_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select written.from_kind, written.id, written.kind, written.to_kind, written.to_id, written.at, written.who, 0, true
from (
  select 'step' as from_kind, t.id, t.kind, t.to_kind, t.to_id, t.at, t.place,
         jsonb_build_object('kind', 'session', 'id', run.session) as who
  from moved_run_tuples t join moved_runs run on run.id = t.id
  union all
  select 'plan', plan.id, stated.kind, stated.to_kind, stated.to_id, plan.made_at, stated.place,
         '{"kind": "session", "id": "session-legion-daemon"}'::jsonb
  from moved_plans plan
  cross join lateral (values
    (1, 'issue', 'value', plan.issue),
    (2, 'project', 'value', plan.project),
    (3, 'generation', 'value', '3'),
    (4, 'source', 'value', 'owner'),
    (5, 'artifact_id', 'value', plan.artifact_id),
    (6, 'version', 'value', '2'),
    (7, 'tasks', 'value', plan.tasks),
    (8, 'part_of', 'issue', plan.issue)
  ) as stated(place, kind, to_kind, to_id)
) written
order by written.at, written.id, written.place;

-- The other part_of children of each issue Legion ran, in the graph store: a node and its part_of
-- tuple into the issue, each with an event of its own.
create temp table moved_children as
select child.kind, md5(child.kind || '/' || r.issue || '/' || c)::uuid::text as id, r.project, r.issue,
       r.t0 + r.n * interval '2 minutes' + c * interval '4 minutes' as at,
       4000000 + r.slot * 2000000 + r.n * 400 + c as source_event
from moved_ran r
cross join lateral generate_series(1, case when r.busy then 339 else 100 end) c
cross join lateral (
  select case
    when r.busy then case when c <= 34 then 'comment' when c <= 38 then 'ask' when c <= 40 then 'message'
                          when c <= 60 then 'artifact' else 'block' end
    else case when c <= 50 then 'comment' when c <= 85 then 'block' when c <= 95 then 'ask' else 'message' end
  end as kind
) child;

insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key)
select kind, id, case kind when 'block' then 'paragraph' when 'artifact' then 'document' else kind end, 'project', 'live',
       jsonb_build_object('text', 'A ' || kind || ' of ' || issue || ' that the store keeps beside its runs'),
       at, '{"kind": "session", "id": "session-0123456789abcdef"}'::jsonb, source_event, project
from moved_children
order by at, id;

insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select kind, id, 'part_of', 'issue', issue, at, '{"kind": "session", "id": "session-0123456789abcdef"}'::jsonb, source_event, true
from moved_children
order by at, id;

-- The tuples of other kinds: each term of cybermonklabs/legion#3178's 59 takes its share of
-- `others` by its tuples_a_day, each a value tuple of a node of the term's kind, as the driver of
-- c-stores-moved writes them (packages/dispatch/e2e/acceptance/stores-moved.sh, other_tuples).
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select t.kind, 'other-' || (n / 7), t.kind, 'value', md5(n::text || t.kind), timestamptz '2026-11-01' + n * interval '1 second',
       '{"kind": "session", "id": "moved-read-cost"}'::jsonb, 0, null, true
from (
  select w.kind, round(c.others * w.weight / sum(w.weight) over ())::bigint as share
  from moved_counts c cross join (values
    ('comment', 1.50), ('authored_by', 0.75), ('anchored_to', 0.00), ('owned_by', 0.00), ('author_alias', 0.04),
    ('delivery', 0.14), ('bus_delivery', 0.25), ('fallback_delivery', 0.00), ('block', 2.00), ('block_restored', 0.00),
    ('block_gone', 0.00), ('block_parent_moved', 0.00), ('block_ref', 0.07), ('block_text', 1.00), ('message_delivery', 0.14),
    ('read_mark', 0.39), ('clear', 0.00), ('dismissal', 0.00), ('snooze', 0.04), ('person', 0.14), ('session', 2.50),
    ('role', 0.21), ('page', 0.32), ('message', 1.00), ('message_pointer', 0.32), ('reply', 0.36), ('mention', 0.32),
    ('participation', 0.39), ('person_delivery', 0.36), ('ask_asked_of', 0.00), ('body_block', 4.39), ('body_block_gone', 0.00),
    ('body_block_ref', 0.14), ('delivery_attempt', 4.50), ('issue', 0.86), ('issue_relation', 0.04), ('artifact', 1.39),
    ('document_version', 0.93), ('criterion', 5.00), ('result', 0.00), ('result_screenshot', 0.00), ('result_retraction', 0.00),
    ('component', 0.00), ('component_relation', 0.00), ('ask', 2.29), ('ask_edit', 0.00), ('answer', 0.18), ('ask_resolved', 0.07),
    ('ask_follower', 0.04), ('keep', 0.00), ('decision', 50.00), ('reading', 10.00), ('interaction', 40.00), ('frame', 500.00),
    ('lens', 12.00), ('lens_finding', 40.00), ('lens_kind', 3.14), ('constraint', 5.00), ('risk', 44.00)
  ) as w(kind, weight)
) t
cross join lateral generate_series(1, t.share) n;

drop table moved_counts, moved_projects, moved_roles, moved_tree, moved_issues, moved_issue_terms, moved_ran, moved_runs,
  moved_run_tuples, moved_plans, moved_children;
