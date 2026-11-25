-- 20261125053125_user_agent_thread_dismissed_root.up.sql
--
-- INFRA-1152: the one writer of messages.agent_dismissed_at (20261125052902), the mark of an Agent
-- dismissal on the root it dismisses. A trigger after each insert, update and delete of a
-- user_agent_thread_dismissed row mirrors the row onto its root, so every write of a mark moves the
-- column in the statement that writes the mark, whichever build wrote it and through whichever
-- path: the author's dismissal (api/inbox_dismiss.go's inboxDismissMarkAgentThreads, an upsert that
-- inserts the mark or moves it later), the restore (inboxRestoreAgentThreadRows, a delete), and a
-- session's reply that brings the thread back (inboxwrite's queueAgentDismissalsClear in this
-- release, the trigger user_agent_thread_dismissed_clear for a reply of the release before, each a
-- delete of every mark of the root), each of them as the release before writes it too while it
-- serves beside this one.
--
-- The column is the author's mark alone: a row moves its root's column only where the root is a
-- user's message whose login, lowered as the Inbox reads it (myAgentRootFilter), is the row's, so
-- a mark naming a message of somebody else's, which no route writes, moves nothing. An insert or
-- an update sets the column to the row's dismissed_at; a delete, or an update that moves the row
-- off its root or its login, clears that root's column. A write that leaves the column as it
-- stands writes no row of messages.
--
-- Every writer of a root's column takes the root's row first, `for no key update` or stronger, and
-- the mark after it (the lock order written on api/inbox_read.go's markInboxReadThreads): the
-- dismissal takes the roots `for update` (inboxDismissLockAgentRoots), the restore `for no key
-- update` (inboxRestoreLockAgentRoots), and a session's reply takes its root `for no key update`
-- before its insert (inboxwrite's Recording.QueueRootLock). A reply of the release before reaches
-- the marks through user_agent_thread_dismissed_clear, whose body this migration replaces with the
-- one 20261116160001 wrote plus one step: where a mark of the root stands, it takes the root `for
-- no key update` before its delete, beside the `for key share` it always takes, so a reply into a
-- thread nobody dismissed takes the root as it did and two sessions replying there still do not
-- queue behind each other. Without that order a restore that holds a mark it deletes waits at the
-- root its mirror updates while a reply that holds the root waits at the mark, a cycle Postgres
-- breaks by failing one of the two. The read of the marks in that step is a statement of its own,
-- after the root is held `for key share`, so it sees every mark committed while the reply waited
-- there, and none can be written behind it: a dismissal writes a mark only under the root's `for
-- update`, which the reply's `for key share` holds off.
--
-- Then the catch-up: every mark stored before this migration, which the release before wrote with
-- no trigger to mirror it, sets its root's column here, after the trigger is attached. `create
-- trigger` holds SHARE ROW EXCLUSIVE on user_agent_thread_dismissed to the end of the migration, so
-- no mark is written or deleted between the catch-up's read and its commit, and every mark written
-- after the commit is the trigger's. The update rewrites one messages row for each stored mark of
-- an author, read by the marks' own primary key into messages by its own, and no other row. The
-- census holds messages to its size limit (pgmigrate.CensusTableLimit), since an update driven by
-- the marks is no bounded write: messages held 5,810 rows and 20 MiB at the census of 2026-10-30,
-- far under it.
--
-- The locks: `create function` and `create or replace function` take no table lock; `create
-- trigger` takes SHARE ROW EXCLUSIVE on user_agent_thread_dismissed, which blocks the writers of
-- marks and no reader; the update takes ROW EXCLUSIVE on messages and the rows it rewrites. A
-- dismissal of the release before can hold a root `for update` and wait to write its mark behind
-- the first lock while the catch-up waits at that root, so the migration bounds its lock waits at
-- half a second, shorter than deadlock_timeout (1 s by default), as 20261116114515 does for the
-- same kind of cycle: it fails with 55P03 having applied nothing, the dismissal finishes, and the
-- runner applies it again (pgmigrate.LockTimeoutAttempts).
set local lock_timeout = '500ms';

create function user_agent_thread_dismissed_root() returns trigger language plpgsql as $$
begin
  -- The root the row named before, where the row no longer names it with the same login.
  if tg_op = 'DELETE' or (tg_op = 'UPDATE' and (new.message_id, new.login) is distinct from (old.message_id, old.login)) then
    update messages set agent_dismissed_at = null
    where id = old.message_id and agent_dismissed_at is not null
      and author->>'kind' = 'user' and lower(author->>'id') = old.login;
  end if;
  -- The root the row names now, at the row's moment.
  if tg_op <> 'DELETE' then
    update messages set agent_dismissed_at = new.dismissed_at
    where id = new.message_id and agent_dismissed_at is distinct from new.dismissed_at
      and author->>'kind' = 'user' and lower(author->>'id') = new.login;
  end if;
  return null;
end;
$$;

create trigger user_agent_thread_dismissed_root after insert or update or delete on user_agent_thread_dismissed
  for each row execute function user_agent_thread_dismissed_root();

create or replace function user_agent_thread_dismissed_clear() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
begin
  -- A message that names its node, or which this release recorded in its thread, is a write whose
  -- root inboxwrite cleared in Go, with the retraction of each dismissal it cleared.
  if (new.node_kind is not null and new.node_address is not null) or new.threads_recorded then
    return null;
  end if;
  -- A root is dismissed by the person who sent it; a root's own insert can clear nothing, and a
  -- write by anyone but the session leaves the mark where it is.
  if new.in_reply_to is null or new.author->>'kind' <> 'session' then
    return null;
  end if;
  while v_parent is not null and v_depth < 64 loop
    v_root := v_parent;
    select in_reply_to into v_parent from messages where id = v_root;
    v_depth := v_depth + 1;
  end loop;
  perform 1 from messages where id = v_root for key share;
  -- The delete below moves the root's agent_dismissed_at (user_agent_thread_dismissed_root), so
  -- where a mark stands the root is taken `for no key update` before it, the order every writer of
  -- that column takes.
  if exists (select 1 from user_agent_thread_dismissed where message_id = v_root) then
    perform 1 from messages where id = v_root for no key update;
  end if;
  delete from user_agent_thread_dismissed where message_id = v_root;
  return null;
end;
$$;

update messages m set agent_dismissed_at = d.dismissed_at
from user_agent_thread_dismissed d
where m.id = d.message_id
  and m.author->>'kind' = 'user' and lower(m.author->>'id') = d.login
  and m.agent_dismissed_at is distinct from d.dismissed_at;
