-- 20261124085206_check_states.up.sql
--
-- The state of each `before_merge` check at each open pull request head of an issue, as Dispatch
-- last published it in a check.state_changed event, and the marks that say an issue's states may
-- have changed since (api/check_states.go).
--
--   check_states_owed    one row per mark: a write to a table a state is computed from marked
--                        the issue owed, in the writer's own transaction (check_states_owe). The
--                        settler reads the marks of an issue that are due (due_at <= now()) and
--                        deletes the ones it settled in the transaction that appends their
--                        events. failures counts the failed reads of GitHub behind a mark the
--                        settler inserted itself, due later, to read again. No unique key on the
--                        issue and no foreign key: a mark waits on no other mark, and it takes no
--                        lock on the issue's row.
--   check_states         one row per issue, repository, head and check: the state the settler
--                        last published there, with the seq of the event that carried it. The
--                        row whose check_block_id is '' is the head's set row: the block ids of
--                        the issue's `before_merge` checks the last event at that head named
--                        (checks). check_artifact_id is null where the issue has no Verification
--                        document; state and check_version are null on the set row. Only the
--                        settler writes it, under the issue's lease, so a settle compares each
--                        computed state with the newest one published.
--   check_state_serials  one row per issue: how many check.state_changed events it holds, the
--                        serial the newest one carries. A test handoff stores it
--                        (handoffs.check_states_serial) once every mark due when the handoff
--                        began is settled.
--
-- check_states_owe(issue) marks an issue owed and notifies dispatch_check_states_owed, which
-- Postgres delivers at the writer's commit and never for a rollback, and which wakes the settler
-- in every process through its shared listener. The four trigger functions below mark the issue
-- a row of each watched table belongs to; the migrations after this one attach them, one table
-- each. A null issue marks nothing: a pull request no issue holds, a block of a document that is
-- no Verification document.
--
-- New tables and functions: nothing existing is read, locked or rewritten, and the release before
-- this one neither reads nor writes them, so it rolls beside this one (cmd/dispatch/README.md,
-- "Database migrations"). The census answers 0.
create table check_states_owed (
    id bigint generated always as identity primary key,
    issue_key text not null, -- no foreign key: a mark takes no lock on the issue's row
    due_at timestamptz not null default now(),
    failures integer not null default 0 check (failures >= 0)
);
create index check_states_owed_issue on check_states_owed (issue_key, id);
create index check_states_owed_due on check_states_owed (due_at);

create table check_states (
    issue_key text not null references issues (key) on delete cascade,
    repo text not null,
    head_sha text not null check (head_sha ~ '^[0-9a-f]{40}$'),
    check_block_id text not null, -- '' is the head's set row
    check_artifact_id uuid, -- null: the issue has no Verification document
    check_version integer,
    state text check (state in ('yes', 'no', 'unknown')),
    reason text not null default '',
    roles text[] not null default '{}',
    ci_case text not null default '',
    checks text[] not null default '{}',
    pull_requests integer[] not null default '{}',
    event_seq bigint not null,
    primary key (issue_key, repo, head_sha, check_block_id)
);

create table check_state_serials (
    issue_key text primary key references issues (key) on delete cascade,
    serial bigint not null check (serial >= 0)
);

create function check_states_owe(issue text) returns void language plpgsql as $$
begin
  if issue is null then return; end if;
  insert into check_states_owed (issue_key) values (issue);
  perform pg_notify('dispatch_check_states_owed', '');
end $$;

-- A recorded result, a role's (check_results) or a CI job's (ci_check_results): its check's issue.
create function check_states_owe_result() returns trigger language plpgsql as $$
begin
  perform check_states_owe(new.check_issue_key);
  return null;
end $$;

-- A retraction: the check's issue of the result it retracts, a role's result or, for a row of
-- ci_check_result_retractions, a CI job's.
create function check_states_owe_retraction() returns trigger language plpgsql as $$
declare
  owed text;
begin
  if tg_table_name = 'ci_check_result_retractions' then
    select check_issue_key into owed from ci_check_results where id = new.result_id;
  else
    select check_issue_key into owed from check_results where id = new.result_id;
  end if;
  perform check_states_owe(owed);
  return null;
end $$;

-- A check block of a Verification document inserted, changed or deleted: the document's issue.
-- The predicate is the server's documentRolePredicate for the verification role (api/artifacts.go):
-- the role itself, or, for an issue's row a rolling deploy left with no role, the slug a document
-- of that name carries.
create function check_states_owe_check_block() returns trigger language plpgsql as $$
declare
  owed text;
begin
  select issue_key into owed from artifacts
  where id = coalesce(new.artifact_id, old.artifact_id)
    and (role = 'verification' or (role is null and issue_key is not null and slug = 'verification-md'));
  perform check_states_owe(owed);
  return null;
end $$;

-- A pull request head inserted, changed or deleted: the issue it belonged to and, where the write
-- gave it another, the issue it belongs to now.
create function check_states_owe_head() returns trigger language plpgsql as $$
begin
  if tg_op in ('UPDATE', 'DELETE') then
    perform check_states_owe(old.issue_key);
  end if;
  if tg_op = 'INSERT' or (tg_op = 'UPDATE' and new.issue_key is distinct from old.issue_key) then
    perform check_states_owe(new.issue_key);
  end if;
  return null;
end $$;
