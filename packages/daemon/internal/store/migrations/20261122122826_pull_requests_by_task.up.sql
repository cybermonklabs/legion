-- 20261122122826_pull_requests_by_task.up.sql — the pull request of each task of an issue:
-- pull_requests keyed by the issue and the branch, what each pull request remembers of its settled
-- heads keyed by them too, and the outbox kind that closes the other pull requests of an issue
-- (task_pull_requests_close).
--
-- The table held one row for each issue, keyed by issue (0004); each task of an issue now keeps
-- its own pull request on its own branch, so the key is (issue, branch), and 0004's unique
-- pull_requests_repo_branch stays. The table stores no task: a pull request's task is the task of
-- its issue's plan at the issue's generation whose branch the pull request is on, which the record
-- reads from plan_tasks at each read (record.PullRequest.Task), so no row can hold a task that a
-- branch given to a task after the row was written contradicts. Each row the upgrade finds keeps
-- its issue and its branch, which is legion/<KEY>, so it is the pull request of the task that holds
-- that branch: the one task of a plan of one, which the release before ran (plan.Supported refused
-- more).
--
-- pull_request_settled_heads (20261120144241) keeps what a pull request remembers of its settled
-- heads by the issue alone, with a foreign key on pull_requests (issue), which the key of one row
-- for each issue served. That key goes here, and the table refers to it, so the foreign key goes
-- first; the table takes the branch of its pull request, filled from the issue's one row, which
-- every row of it has (the foreign key held it so), and is keyed by (issue, branch, kind, head)
-- with its foreign key on pull_requests (issue, branch), so each task's pull request keeps its own
-- memory and a row deleted takes its memory with it (on delete cascade), as before. The record
-- reads and writes the memory by the issue and the branch (record's loadSettledHeads,
-- putSettledHeads).
--
-- The outbox check is one list, and main writes it whole on both sides of this version:
-- 20261122154643 (the basis seeds) below it and 20261126110259 (the merge holds change) above it.
-- So this migration writes no list of its own, as 20261121165047 writes none: it adds
-- task_pull_requests_close to the list the database holds (its last statement), whichever
-- migration wrote it. On a database main's release migrated, that list keeps every kind main's
-- daemon queues, merge_holds_changed among them; on a fresh database a kind main adds above this
-- version is still refused below the migration that adds it
-- (TestTheOutboxTakesTheMergeHoldsKindWith20261126110259). 20261126194217 writes the list again
-- above main's lists with this kind in it.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1; a rollback past
-- 20261121165047 too runs this block before that migration's own. The release before writes each
-- row with `on conflict (issue)`, which needs the key of one row for each issue, and decodes no
-- task_pull_requests_close row. So the block deletes each such row first, since the check written
-- again without the kind refuses a row that holds it, and takes the kind out of the list the
-- database holds, which keeps every other kind, main's among them; then keeps, of each issue, the
-- row on the branch of the task that merges (the one task of its plan at its generation that no
-- task needs, on the branch its row holds or on legion/<KEY> where it holds none) or on
-- legion/<KEY> where the issue has no plan rows there, and deletes the others, which takes the
-- settled heads of each with it; keys pull_requests by the issue again, and
-- pull_request_settled_heads by (issue, kind, head) without its branch, with the foreign key
-- 20261120144241 wrote; and deletes its version row, which is owed to the roll-forward, so that it
-- runs this file again. What the way back costs is the row of each task's pull request but the one
-- that merges, and what it remembered of its settled heads: the record holds it no longer, so a
-- later event of that pull request names no row.
--
-- begin;
-- delete from outbox where kind = 'task_pull_requests_close';
-- do $$
-- declare
--   kinds text[] := array(select k.m[1] from pg_constraint c,
--     regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
--     where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
-- begin
--   alter table outbox drop constraint outbox_kind_check;
--   execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
--     (select string_agg(quote_literal(u.kind), ', ' order by u.n)
--       from unnest(array_remove(kinds, 'task_pull_requests_close')) with ordinality u(kind, n)));
-- end
-- $$;
-- delete from pull_requests pr where pr.branch <> coalesce((select coalesce(nullif(t.branch, ''), 'legion/' || t.issue) from plan_tasks t join issues i on i.key = t.issue and i.generation = t.generation where t.issue = pr.issue and not exists (select 1 from plan_tasks u where u.issue = t.issue and u.generation = t.generation and t.task = any(u.needs)) order by t.task limit 1), 'legion/' || pr.issue);
-- alter table pull_request_settled_heads drop constraint pull_request_settled_heads_issue_fkey,
--   drop constraint pull_request_settled_heads_pkey;
-- alter table pull_request_settled_heads drop column branch,
--   add constraint pull_request_settled_heads_pkey primary key (issue, kind, head);
-- alter table pull_requests drop constraint pull_requests_pkey, add constraint pull_requests_pkey primary key (issue);
-- alter table pull_request_settled_heads add constraint pull_request_settled_heads_issue_fkey
--   foreign key (issue) references pull_requests (issue) on delete cascade;
-- delete from schema_version where version = 20261122122826;
-- commit;
alter table pull_request_settled_heads drop constraint pull_request_settled_heads_issue_fkey;
alter table pull_request_settled_heads add column branch text;
update pull_request_settled_heads heads set branch = pr.branch from pull_requests pr where pr.issue = heads.issue;
alter table pull_request_settled_heads alter column branch set not null;

alter table pull_requests drop constraint pull_requests_pkey,
  add constraint pull_requests_pkey primary key (issue, branch);

alter table pull_request_settled_heads drop constraint pull_request_settled_heads_pkey,
  add constraint pull_request_settled_heads_pkey primary key (issue, branch, kind, head),
  add constraint pull_request_settled_heads_issue_fkey foreign key (issue, branch)
    references pull_requests (issue, branch) on delete cascade;

do $$
declare
  kinds text[] := array(select k.m[1] from pg_constraint c,
    regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
    where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
begin
  if cardinality(kinds) = 0 then
    raise exception 'outbox_kind_check names no kind, so this migration has no list to add task_pull_requests_close to';
  end if;
  alter table outbox drop constraint outbox_kind_check;
  execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
    (select string_agg(quote_literal(u.kind), ', ' order by u.n)
      from unnest(array_remove(kinds, 'task_pull_requests_close') || 'task_pull_requests_close'::text)
        with ordinality u(kind, n)));
end
$$;
