-- 20261128175523_comment_thread_follows.up.sql
--
-- A session's own choice to follow or leave a comment thread, so a session can stop receiving
-- the replies of a thread it once wrote in, as it can leave an ask (ask_followers).
--
--   comment_thread_follows
--                      one row per thread and session, keyed by the thread's root comment (a
--                      comment with no reply_to and no ask_id). follows is true for a session that
--                      follows the thread (PUT /api/v1/comments/{id}/followers/{session_id}) and
--                      false for one that left it (DELETE on the same path). since is when the
--                      choice was made, on the database's clock_timestamp(), the clock
--                      comments.created_at is written on (0065), so the outbox can tell a leave
--                      from a later comment of the same session in the thread: that comment
--                      rejoins the thread, as a reply to an ask follows the ask
--                      (outbox.commentRecipientsQuery). A row goes with its root comment.
--
-- The table is new: the release before this one neither reads nor writes it, so it serves beside
-- this one while the release rolls, and its threads reach every session that wrote in them, as
-- they always did. The census answers 0.
--
-- The way back is an image rollback alone. By hand, with no process of this release serving, the
-- block below also takes the schema back: it loses every session's choice, so every thread again
-- reaches every session that wrote in it, and its last statement makes the next boot of this
-- release apply this migration again.
--
-- drop table comment_thread_follows;
-- delete from schema_migrations where version = 20261128175523;
create table comment_thread_follows (
    comment_id uuid not null references comments(id) on delete cascade,
    session_id text not null check (session_id <> ''),
    follows boolean not null,
    since timestamptz not null default clock_timestamp(),
    primary key (comment_id, session_id)
);
