-- 20261120021308_search_text_versions.up.sql
--
-- One row per version of the text keyword search reads of an issue, a comment, an issue message
-- or an ask, so a search can be answered as that text stood at a past time. A version starts at
-- the event that changed the text and holds until the item's next version, in its owner's seq
-- order, starts. A row is never updated: a new version is a new row. The keyword table's fill
-- (internal/dispatch/textversions) writes every row from the event log while Dispatch serves, and
-- no migration writes one. A search joins store.TextVersions, the relation of each item's version
-- that held at one time, which store.TextVersionsAt hands out only for a time the fill has
-- reached.
--
--   kind              the kind of item the row is a version of: 'issue', 'comment', 'message' or
--                     'ask', the present search's own kinds and no other, which the check below
--                     holds it to.
--   id                the item: an issue's key, which never changes, or the comment's, message's
--                     or ask's uuid as text.
--   issue_key         the item's issue; null for a comment or an ask on a project document.
--   project_key       the item's project.
--   event_id          the id of the event that started the version.
--   event_seq         that event's seq in its owner's order. An item's events all have one
--                     owner and a seq is unique within an owner, so (kind, id, event_seq) names
--                     one version, and an item's versions are ordered by it.
--   started_at        that event's created_at: the start of the transaction that wrote it.
--   body              the text: an issue's title, a comment's or a message's body, or an ask's
--                     question, a space and ask_search_suffix of its options and answer.
--   body_from         where the body came from, one of five values and no other, which the check
--                     below holds it to: 'event' (the event's payload, whole), 'row' (the item's
--                     present row, which shows no later change), 'blocks' (revision 1 of a
--                     comment's blocks), 'history' (the post's body history) and 'cut' (the
--                     event's own text, cut short, where no other source holds the whole body).
--   spec_artifact_id  the issue's primary document, on an issue's row alone.
--   spec_version      the version of that document the issue's text held, on an issue's row
--                     alone.
--   search            the version's vector, made by the functions the item's own search column is
--                     made by, so a version of an unchanged item equals that column.
--
-- search_text_versions_fill holds the fill's one row:
--
--   id                1, which the check below holds it to.
--   event_id          the cursor: every event whose id is at or below it has its rows.
--   event_at          the created_at of the event at the cursor, which tells a log that was reset
--                     from the log the cursor was read from.
--   reach             the latest time at or before which every event has its rows, stored by the
--                     fill after each batch and never lowered but by a reset, which nulls it
--                     (store.TextVersionsReach computes it again on a read the stored value does
--                     not reach).
--   holder            the fill holder that wrote the last batch.
--   rules             the version of the event rules that wrote the rows up to the cursor, so a
--                     later build that reads more event types knows which events to read again.
--   updated_at        when the row was last written.
--
-- Neither table carries a foreign key. A foreign key locks the table it names, and every table
-- these rows name (issues, comments, messages, asks, artifacts and events) holds rows; and a
-- version is history that must not follow, or be refused by, a later change to the row it names.
--
-- search_text_versions_item_start is the index the relation's test for a later version of the
-- same item reads. search_text_versions_search is the GIN index a search's match reads, built
-- with fastupdate off, so a search never scans a pending list and an insert never empties one.
-- At production's counts the table and its indexes take about 1.9 GB and grow by 30 to 40 MB a
-- day, above the 1 GiB a migration may touch: once filled, no later migration may lock the table
-- but one the census exempts, and a change to its shape is a new table the fill writes again.
--
-- New tables nothing before this release names: the release before this one neither reads nor
-- writes them, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and
-- the census answers 0 because the tables hold no row at this schema. Rolling back past it is an
-- image rollback alone. The identifier is the UTC moment this file was created, as every
-- migration past the numbered series is named.
create table search_text_versions (
    kind text not null check (kind in ('issue', 'comment', 'message', 'ask')),
    id text not null,
    issue_key text,
    project_key text not null,
    event_id bigint not null,
    event_seq integer not null,
    started_at timestamptz not null,
    body text not null,
    body_from text not null check (body_from in ('event', 'row', 'blocks', 'history', 'cut')),
    spec_artifact_id uuid,
    spec_version integer,
    search tsvector not null,
    primary key (kind, id, event_seq)
);

create index search_text_versions_item_start on search_text_versions (kind, id, started_at) include (event_seq);

create index search_text_versions_search on search_text_versions using gin (search) with (fastupdate = off);

create table search_text_versions_fill (
    id smallint primary key check (id = 1),
    event_id bigint not null,
    event_at timestamptz,
    reach timestamptz,
    holder uuid,
    rules smallint not null,
    updated_at timestamptz not null default now()
);
