-- 20261110224508_post_corrections.up.sql
--
-- The author of a comment or of an issue message replaces its body, or deletes it, and every
-- body a replacement took the place of is kept. Six things the schema has no room for today:
-- a comment carries edited_at (0021) but no record of who withdrew it, a message carries
-- neither, no table holds a body a write replaced, no version records the comment whose accepted
-- suggestion named it, no version records that a delete renamed it, and nothing records which
-- posts an answer run gave its model the text of.
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
-- artifact_versions.renamed_by_delete names the comment whose delete renamed the version: an
-- accept names the version it makes after the suggestion's body, and the delete of that comment
-- gives the version the name a suggestion with no body takes, setting this in the same update. A
-- keyed repeat whose kept answer holds that version by its document and number is never
-- replayed, since the name the answer holds is the deleted body's. It is a plain nullable add
-- with no default too, so every version stored before this migration reads null, as every
-- version no delete renamed does. It carries no foreign key: it records what a delete did to the
-- version, which stands whatever becomes of the comment's row.
--
-- artifact_versions.accepted_from names the comment whose accepted suggestion named the version:
-- the accept sets it in its own transaction, right after it writes the version, and the delete of
-- that comment finds the version it renames by it. It is a plain nullable add with no default
-- too, so every version stored before this migration reads null, as every version no accept made
-- does, and the delete of a comment accepted before it finds its version by the time the accept
-- wrote it, as it did before. It carries no foreign key either: it records what an accept did to
-- the version. It needs no index: the delete reads it among the versions of one document, which
-- the unique key on (artifact_id, number) already narrows to.
--
-- The two partial indexes are the two reads: GET /api/v1/comments/{id}/history and
-- GET /api/v1/messages/{id}/history each answer one post's rows newest first, with ?before= for
-- the page after. Each holds only the rows of its own kind, since a row carries exactly one
-- owner, and its order is the read's, so neither read sorts.
--
-- going_request_posts names each comment and each issue message whose text an answer run gave
-- its model (api.goingBodies), so every read of that run's stream ends at the delete of one of
-- them rather than serving an answer that may quote it (api.goingRunStateOf). A row is written
-- before the text it names reaches the model, so it is stored before any frame that text can be
-- in. Its owner pair and exclusive check are post_body_history's. Its foreign key to
-- going_requests cascades, so the sweep's delete of a run past its hour takes the run's rows with
-- it, and its one index is that delete's and the stream's read, both by the run. comment_id and
-- message_id carry no foreign key: a post whose row is gone stands no more than a deleted one,
-- and the stream reads both alike, which a cascade from the post would undo by taking the record
-- with the row. A run the release before this one answers records no row, so its stream is not
-- ended at a delete; it is gone an hour after it was asked (contracts.GoingRequestTTL).
--
-- messages_item_blocks_owed (0144) fires on an insert alone, because a message was never edited
-- when it was written. It becomes the comment twin of 0143, `after insert or update of body`,
-- so an edited message is owed a block index as an edited comment is. The replacement needs no
-- seed: every message stored carries a state row already (0144's own seed and its trigger), and
-- no release before this one updates a message's body. `create or replace trigger` keeps the
-- trigger's own lock, SHARE ROW EXCLUSIVE on messages, which blocks that table's writers for
-- the catalogue write and no reader, and reads no row; going_request_posts' foreign key takes
-- SHARE ROW EXCLUSIVE on going_requests for the same transaction, which holds back a question's
-- POST and the sweep's delete of old runs and no read and no frame's write; the column adds
-- above hold the stronger ACCESS EXCLUSIVE on comments, on messages and on artifact_versions for
-- the same transaction, each for the moment a catalog row is written.
--
-- The release before this one names none of the new columns and none of the new tables in any
-- statement, and every statement it sends to comments, to messages and to artifact_versions goes
-- on working, so the deploy is a rolling one (cmd/dispatch/README.md, "Database migrations").
alter table comments add column deleted_at timestamptz, add column deleted_by jsonb;

alter table messages add column edited_at timestamptz, add column deleted_at timestamptz, add column deleted_by jsonb;

alter table artifact_versions add column renamed_by_delete uuid, add column accepted_from uuid;

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

create table going_request_posts (
    request_id uuid not null references going_requests(id) on delete cascade,
    comment_id uuid,
    message_id uuid,
    constraint going_request_posts_one_post check ((comment_id is null) <> (message_id is null))
);

create index going_request_posts_request on going_request_posts (request_id);

create or replace trigger messages_item_blocks_owed after insert or update of body on messages
  for each row execute function item_blocks_owe('message');
