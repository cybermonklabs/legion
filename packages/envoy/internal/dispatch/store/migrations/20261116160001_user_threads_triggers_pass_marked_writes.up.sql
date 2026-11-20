-- 20261116160001_user_threads_triggers_pass_marked_writes.up.sql
--
-- INFRA-1152's D3: one Go writer, inboxwrite.Record, appends the participation and delivery
-- tuples of every comment, message, ask and answer this release writes, and writes the user_threads
-- projection of them, in the write's own transaction after its event, through graphstore. The four
-- trigger functions that wrote those projections before it stay for a write a build of the release
-- before this one makes during a roll, or after a rollback: each now returns at once for a write
-- this release marked, the marker set in the very insert or update the trigger fires on, and keeps
-- today's behaviour for any other. The backfill gives an older build's write its tuples once.
--
--   user_threads_comment   comments.tuples_stored (20261116124447): true for a comment whose
--                          writer stated its pointers in the store, which is every comment this
--                          release writes but one past the store's address bound.
--   user_threads_message   the message's node pair (20261116114515), which a row names exactly
--                          where its writer stated it in the store.
--   user_threads_ask       on an insert, asks.tuples_stored_at is not null (20261116140001), which
--                          every ask this release creates holds from its insert; on an answer, the
--                          answer passes only where asks.tuples_stored_at differs from the answer's
--                          own moment, (new.answer->>'at')::timestamptz, since each answer this
--                          release writes sets the marker to that moment in the update that writes
--                          it (asks.Settle): an older build's answer to an ask this release created,
--                          whose update leaves the marker at the ask's created_at, is still
--                          projected here.
--   user_agent_thread_dismissed_clear
--                          the message's node pair: a session's reply this release writes clears
--                          its root's Agent dismissal in Go, with the retraction of each dismissed
--                          tuple it clears (inboxwrite.Record).
--
-- user_threads_record itself is unchanged: it is reached only from the three trigger functions
-- above, below each one's marker test, and api's TestNoProjectionRowIsWrittenWithoutTheStore reads
-- the catalogue to hold both it and user_agent_thread_dismissed_clear to that.
--
-- Its own migration, as 20261105093501's replacement of user_threads_record is: `create or replace
-- function` takes ACCESS EXCLUSIVE on each function's own catalogue row and no lock at all on
-- comments, messages, asks, user_threads or user_agent_thread_dismissed, so the window is four
-- catalogue writes, and the triggers that call them pick up the new bodies on their next statement.
-- No row is read or rewritten and the census answers 0.
create or replace function user_threads_comment() returns trigger language plpgsql as $$
begin
  -- A comment whose pointers its writer stated is a write of this release's, whose thread
  -- inboxwrite.Record recorded in Go.
  if new.tuples_stored then
    return null;
  end if;
  perform user_threads_record(
    coalesce(new.ask_id, new.reply_to, new.id),
    case when new.ask_id is not null then 'ask' else 'comment' end,
    new.author->>'kind', new.author->>'id', new.created_at, new.id, true,
    new.issue_key, new.artifact_id
  );
  return null;
end;
$$;

create or replace function user_threads_message() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
begin
  if new.issue_key is null then
    return null;
  end if;
  -- A message that names its node is a write of this release's, whose thread inboxwrite.Record
  -- recorded in Go.
  if new.node_kind is not null and new.node_address is not null then
    return null;
  end if;
  while v_parent is not null and v_depth < 64 loop
    v_root := v_parent;
    select in_reply_to into v_parent from messages where id = v_root;
    v_depth := v_depth + 1;
  end loop;
  perform user_threads_record(
    v_root, 'message', new.author->>'kind', new.author->>'id', new.created_at, new.id, true,
    new.issue_key, null
  );
  return null;
end;
$$;

create or replace function user_threads_ask() returns trigger language plpgsql as $$
begin
  -- An ask this release created holds its marker from its insert, and inboxwrite.Record recorded
  -- its asker in Go.
  if tg_op = 'INSERT' and new.tuples_stored_at is not null then
    return null;
  end if;
  if tg_op = 'INSERT' and new.author->>'kind' = 'user' then
    perform user_threads_record(
      new.id, 'ask', 'user', new.author->>'id', new.created_at, null, false,
      new.issue_key, new.artifact_id
    );
  end if;
  -- An answer this release wrote moved the marker to its own moment in this same update, and
  -- inboxwrite.Record recorded it in Go; any other answer is an older build's.
  if new.answer->>'user' is not null and (tg_op = 'INSERT' or new.answer is distinct from old.answer)
     and new.tuples_stored_at is distinct from (new.answer->>'at')::timestamptz then
    perform user_threads_record(
      new.id, 'ask', 'user', new.answer->>'user', (new.answer->>'at')::timestamptz, new.id, true,
      new.issue_key, new.artifact_id
    );
  end if;
  return null;
end;
$$;

create or replace function user_agent_thread_dismissed_clear() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
begin
  -- A message that names its node is a write of this release's, whose root inboxwrite.Record
  -- cleared in Go, with the retraction of each dismissal it cleared.
  if new.node_kind is not null and new.node_address is not null then
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
  delete from user_agent_thread_dismissed where message_id = v_root;
  return null;
end;
$$;
