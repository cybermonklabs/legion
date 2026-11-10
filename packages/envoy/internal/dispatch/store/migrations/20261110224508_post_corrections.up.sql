-- 20261110224508_post_corrections.up.sql
--
-- The author of a comment or of an issue message replaces its body, or deletes it, and every
-- body a replacement took the place of is kept. Three things the schema has no room for today:
-- a comment carries edited_at (0021) but no record of who withdrew it, a message carries
-- neither, and no table holds a body a write replaced.
--
-- comments.deleted_at and comments.deleted_by, and messages.edited_at, messages.deleted_at and
-- messages.deleted_by, are plain nullable adds with no default: a catalog change that reads and
-- rewrites no row. A deleted post keeps its row and its place in its thread, with deleted_by
-- the actor that deleted it, in the jsonb every other actor column of this schema holds
-- (comments.author, comments.resolved_by).
--
-- Neither body column loses its `not null`. A delete writes '', which is what clearing the body
-- means everywhere in this change: the search triggers (0057-0061) rewrite the row's vector
-- from the empty body on that update, so a deleted post stops matching a search, and a reader
-- reaches it by id, where deleted_at carries the mark.
--
-- post_body_history holds each body an edit or a delete replaced, with the actor that replaced
-- it and the time. One table serves both kinds, which is what a reader comparing a comment's
-- history with a message's needs, and a polymorphic key cannot do that: two nullable columns
-- with an exclusive check is this schema's own answer to one row with one owner of two kinds
-- (comments_one_owner, 0009_project_artifacts.up.sql:39). Each foreign key cascades, since a
-- history row is a part of its post and nothing without it. edited_at defaults to
-- clock_timestamp() rather than now(), so two edits of one transaction order by it
-- (0100_delivery_changes.up.sql:21 takes it for the same reason), and the id breaks a tie
-- within one clock reading.
--
-- The two partial indexes are the two reads: GET /api/v1/comments/{id}/history and
-- GET /api/v1/messages/{id}/history each answer one post's rows newest first, with ?before= for
-- the page after. Each holds only the rows of its own kind, since a row carries exactly one
-- owner, and its order is the read's, so neither read sorts.
--
-- messages_item_blocks_owed (0144) fires on an insert alone, because a message was never edited
-- when it was written. It becomes the comment twin of 0143, `after insert or update of body`,
-- so an edited message is owed a block index as an edited comment is. The replacement needs no
-- seed: every message stored carries a state row already (0144's own seed and its trigger), and
-- no release before this one updates a message's body. `create or replace trigger` keeps the
-- trigger's own lock, SHARE ROW EXCLUSIVE on messages, which blocks that table's writers for
-- the catalogue write and no reader, and reads no row; the column adds above hold the stronger
-- ACCESS EXCLUSIVE on comments and on messages for the same transaction, each for the moment a
-- catalog row is written.
--
-- The release before this one names none of the new columns and none of the new table in any
-- statement, and every statement it sends to comments and to messages goes on working, so the
-- deploy is a rolling one (cmd/dispatch/README.md, "Database migrations").
alter table comments add column deleted_at timestamptz, add column deleted_by jsonb;

alter table messages add column edited_at timestamptz, add column deleted_at timestamptz, add column deleted_by jsonb;

create table post_body_history (
    id bigint generated always as identity primary key,
    comment_id uuid references comments(id) on delete cascade,
    message_id uuid references messages(id) on delete cascade,
    body text not null,
    edited_by jsonb not null,
    edited_at timestamptz not null default clock_timestamp(),
    constraint post_body_history_one_owner check ((comment_id is null) <> (message_id is null))
);

create index post_body_history_comment on post_body_history (comment_id, edited_at desc, id desc)
  where comment_id is not null;
create index post_body_history_message on post_body_history (message_id, edited_at desc, id desc)
  where message_id is not null;

create or replace trigger messages_item_blocks_owed after insert or update of body on messages
  for each row execute function item_blocks_owe('message');
