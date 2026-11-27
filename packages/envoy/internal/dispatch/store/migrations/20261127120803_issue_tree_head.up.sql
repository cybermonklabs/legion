-- 20261127120803_issue_tree_head.up.sql — the head projection of each issue's tree, and the mark
-- that says an issue's rows may be read.
--
-- It is dated after every type migration of its release. While it is pending, the census reads
-- its triggers on graph_nodes, graph_tuples, document_blocks, comments, comment_blocks and
-- comment_block_texts as a change of those tables, and a type migration after it in the same deploy
-- could no longer be admitted as a bounded write (pgmigrate's census).
--
-- The tree read with no version (GET /api/v1/issues/{key}/tree) answers the head of an issue's
-- tree. Walking the tuple log for it probes graph_tuples several times a node, and over a tree of
-- thousands of nodes that walk costs tens of times the read of the same document's blocks. The
-- head read reads one range of issue_head_nodes instead, and a read at a version keeps the walk.
--
-- issue_head_nodes holds one row per node of an issue's tree, keyed by the issue and the node:
--
--   issue_key          the issue whose tree holds the node. A node of an issue's own (a block, a
--                      document, a version of a document whose ref_key the key begins, the issue's
--                      own node, an ask, a comment or a message of the issue and each block of one)
--                      is held under that issue; any other node (a lens, a finding, a risk, a
--                      result) under each issue its part_of parent is held under; and a node of
--                      another issue placed under one of its nodes under the parent's issue as
--                      well, as a node the walk of that issue answers and does not enter.
--   node_kind,         the node, as graph_nodes keys it.
--   node_address
--   parent_kind,       the far end of its standing part_of, the newest asserted one, null where it
--   parent_address     has none.
--   rank, text         its rank and its text, the newest value of each, null where it holds none.
--   supertag, state,   its graph_nodes row's columns, null where the store holds no row of it.
--   visibility,
--   made_at, made_by
--   authors            the who of each authored_by it states now, in seq order, null where none.
--   first_authors,     for a document block, the authors and the time of the version that first
--   first_at           held it, where Dispatch knows the block was made there, as the block loader
--                      reads them.
--   document_id,       for a document block, its document (the issue room's document for a block
--   block_type,        of a room) and its document_blocks row's type, text sum and changed version,
--   text_sha256,       null where no row stands.
--   changed_version
--   value_kinds,       every other value it states now, by its attribute.
--   value_texts
--   relation_kinds,    every relation it states now, ordered by kind, far end kind and far end id.
--   relation_to_kinds,
--   relation_to_ids
--   item_issue_key,    for a comment, what the comment loader reads of its row: its issue, its
--   item_author,       author, its body, when it was written and last edited, the comment it
--   item_text,         replies to, the block it replies to, its ask, its document (artifact_id), the
--   item_made_at,      document it is written on (its anchor's, else artifact_id) and the version
--   item_changed_at,   it is written at (its anchor's, else written_version); for a current block of
--   item_reply_to,     a comment (kind comment_block, its comment_blocks row's node_id, a node the
--   item_replies_to_block, store holds no graph_nodes row of), what the comment block loader reads:
--   item_ask_id,       its comment's issue, author and document, its text at its newest revision,
--   item_artifact_id,  when it was made and changed, its changed revision, its block id, its
--   item_document_id,  excerpt and its position. Null for every other node.
--   item_version,
--   item_block_id,
--   item_excerpt,
--   item_position
--   item_edited_at,    for a comment and for a current block of one, the comment's marks as both
--   item_deleted_at,   loaders read them: when its author last edited it, and when its author
--   item_deleted_by    deleted it and who did. Null for every other node.
--
-- The primary key reads one issue's rows in one range, and the read with no view reads its
-- skeleton in ranges of it: one for each kind of node but the blocks and the comments, one for
-- the blocks of each document it holds, whose addresses begin with the document's ref_key and
-- `#`, one for the findings, whose addresses begin with `lens/`, and the comments of its comment
-- page (refs.HeadRows). node_kind and node_address sort byte by byte (collate "C") so that a
-- range of addresses that begin alike is a range of the key: the database's own collation, en_US
-- on the Aurora dispatch database, passes over punctuation at its first level, where `#` and `/`
-- stand. issue_head_nodes_node finds every row of one node, which each writer replaces
-- (graphstore.QueueHeads).
--
-- Every writer of a node or of a tuple out of a node of an issue's tree writes the node's rows in
-- its own transaction, from what the store holds there, and so does every writer of a comment's
-- row, of its blocks' rows and of their texts. issue_head_marks names each issue whose rows may be
-- read: the write that makes an issue's first node marks it, and the backfill marks an issue once
-- it has filled its rows from the store, in the transaction that holds the issue's owner lock. The
-- head read reads the rows of an issue it names and walks the tuples of any other.
--
-- Rolling deploy. Both tables are new and hold no row. The release before names neither, and a
-- write it makes leaves the rows of the nodes it writes behind the store: the triggers below take
-- the mark of each issue such a write reaches away in the write's own transaction, so no issue
-- stays marked over rows a process of the release before left behind, during the roll or after a
-- rollback and a roll forward. The block index's tick fills such an issue again. Three tables carry
-- that trigger: graph_nodes (a node's supertag, state, visibility, made_at, made_by), graph_tuples
-- (its place, its values, its relations, its authors) and document_blocks (a block's document,
-- type, text sum and changed version), each for an insert, a delete and, on graph_nodes and on the
-- columns of document_blocks a head row reads, an update, since the release before writes all three
-- and inserts and deletes document_blocks rows in its index pass; the three tables of a comment's
-- row, its blocks and their texts (comments, comment_blocks, comment_block_texts), whose columns the
-- rows of a comment and of its blocks hold, carry triggers of their own (issue_head_items_drop),
-- which fire inside the statement that writes the row as the three above do. Rolling back past
-- this migration is an image rollback alone: the tables left behind are read by nothing that
-- release runs, and the rollback script drops them with the triggers.
create table issue_head_nodes (
  issue_key text not null,
  node_kind text collate "C" not null,
  node_address text collate "C" not null,
  parent_kind text,
  parent_address text,
  rank text,
  text text,
  supertag text,
  state text,
  visibility text,
  made_at timestamptz,
  made_by jsonb,
  authors jsonb,
  first_authors jsonb,
  first_at timestamptz,
  document_id uuid,
  block_type text,
  text_sha256 text,
  changed_version integer,
  value_kinds text[] not null,
  value_texts text[] not null,
  relation_kinds text[] not null,
  relation_to_kinds text[] not null,
  relation_to_ids text[] not null,
  item_issue_key text,
  item_author jsonb,
  item_text text,
  item_made_at timestamptz,
  item_changed_at timestamptz,
  item_reply_to text,
  item_replies_to_block text,
  item_ask_id text,
  item_artifact_id uuid,
  item_document_id uuid,
  item_version integer,
  item_block_id text,
  item_excerpt text,
  item_position integer,
  item_edited_at timestamptz,
  item_deleted_at timestamptz,
  item_deleted_by jsonb,
  primary key (issue_key, node_kind, node_address)
);

create index issue_head_nodes_node on issue_head_nodes (node_kind, node_address);

create table issue_head_marks (
  issue_key text primary key,
  filled_at timestamptz not null
);

-- issue_head_marks_drop takes away the mark of every issue whose tree holds a node whose head rows
-- a write of graph_tuples, graph_nodes or document_blocks moves, for a write the head rows are not
-- kept for. It runs for each row of a statement whose transaction has not set dispatch.head_rows to
-- 'kept' (the WHEN of the three triggers below), which every statement of this release that writes
-- one of those tables sets as it runs (graphstore's KeptHeadRows) and no release before sets: a
-- write of such a release, during a roll or after a rollback and a roll forward, leaves the rows of
-- the nodes it writes behind the store, so the head read of each issue it reached walks the tuples
-- until the backfill fills it again. A delete moves the head rows as an insert does - a tuple, a
-- node or a block's row the rows projected is gone - so each trigger runs on delete too, reading the
-- row the delete took (OLD); no writer of either release deletes a tuple or a node, and the release
-- before deletes the document_blocks rows of the blocks a version drops.
--
-- The nodes are a tuple's near end, and for a part_of its far end too, since a node such a write
-- makes holds no row yet and joins a tree only through its part_of; a node row's node; and a
-- document_blocks row's block node (node_id), before and after an update. The issues are those that
-- hold a row of one of the nodes (issue_head_nodes_node, one probe of its index a node), and the
-- issue a node's address names of its own (an issue's node, a document's, a version's, a document
-- block's, the key its address begins with), which no probe needs and which holds while a fill
-- writes that issue's first rows, which no other transaction sees yet.
--
-- Before it deletes an issue's mark it takes the issue's head fill lock (store.HeadFillLockNamespace,
-- 'HEAD' = 1212498244, and hashtext of 'issue:<key>', as the owner lock hashes its key) in share
-- mode, which the fill of an issue takes in exclusive mode right after the issue's owner lock and
-- holds until it commits the issue's mark, and which nothing else takes: such a write that holds no
-- owner lock of the issue - an issue's create - waits for a fill of the issue that runs, and its
-- delete, a statement of its own with a snapshot of its own, reads the mark the fill committed;
-- while the fill waits for nothing that write or any writer of the issue holds, since each of those
-- holds the owner lock, which the fill takes first, or takes this lock last. Share mode, so two
-- such writes of one issue never wait for each other here: each waits only for a fill. Not the
-- owner lock itself: a create of the release before reads its parent's row `for share` before its
-- tuples, and a writer of this build holding the parent's owner lock then takes that row `for no key
-- update`, so the trigger waiting there for the owner lock would close a cycle. Several issues'
-- locks are taken in the order of their hash.
-- Deleting a mark rewrites no fact of the store: graph_tuples, graph_nodes and document_blocks stay
-- as the write left them.
create function issue_head_marks_drop() returns trigger
language plpgsql as $$
declare
  held text;
  fact graph_tuples;
  node graph_nodes;
  named_kinds text[] := '{}';
  named_ids text[] := '{}';
begin
  if tg_table_name = 'graph_tuples' then
    if tg_op = 'DELETE' then
      fact := old;
    else
      fact := new;
    end if;
    named_kinds := array[fact.from_kind];
    named_ids := array[fact.from_id];
    if fact.kind = 'part_of' and fact.to_kind <> 'value' then
      named_kinds := named_kinds || fact.to_kind;
      named_ids := named_ids || fact.to_id;
    end if;
  elsif tg_table_name = 'graph_nodes' then
    if tg_op = 'DELETE' then
      node := old;
    else
      node := new;
    end if;
    named_kinds := array[node.kind];
    named_ids := array[node.id];
  else
    if tg_op <> 'INSERT' then
      if old.node_id is not null then
        named_kinds := named_kinds || 'block'::text;
        named_ids := named_ids || old.node_id;
      end if;
    end if;
    if tg_op <> 'DELETE' then
      if new.node_id is not null then
        named_kinds := named_kinds || 'block'::text;
        named_ids := named_ids || new.node_id;
      end if;
    end if;
  end if;
  for held in
    select found.issue_key from (
      select head.issue_key
      from unnest(named_kinds, named_ids) named (kind, id)
      join issue_head_nodes head on head.node_kind = named.kind collate "C" and head.node_address = named.id collate "C"
      union
      select case when named.kind = 'issue' then named.id else split_part(named.id, '/', 1) end
      from unnest(named_kinds, named_ids) named (kind, id)
      where named.kind = 'issue' or (named.kind in ('artifact', 'version', 'block') and named.id not like 'lens/%')
    ) found
    where found.issue_key is not null
    order by hashtext('issue:' || found.issue_key), found.issue_key
  loop
    perform pg_advisory_xact_lock_shared(1212498244, hashtext('issue:' || held));
    delete from issue_head_marks mark where mark.issue_key = held;
  end loop;
  return null;
end
$$;

create trigger graph_tuples_head_marks after insert or delete on graph_tuples
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_marks_drop();

create trigger graph_nodes_head_marks after insert or update or delete on graph_nodes
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_marks_drop();

create trigger document_blocks_head_marks
  after insert or delete or update of node_id, artifact_id, block_id, type, text_sha256, changed_version
  on document_blocks
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_marks_drop();

-- issue_head_items_drop takes away the mark of every issue whose tree holds a comment, or a block
-- of one, that a write of comments, comment_blocks or comment_block_texts writes, for a write the
-- head rows are not kept for: the comment's own issue, its row's issue_key before and after the
-- write, which holds the rows of its blocks too, and each issue that holds a row of the comment
-- (issue_head_nodes_node). It runs for each row of a statement whose transaction has not set
-- dispatch.head_rows to 'kept' (the WHEN of the four triggers below), inside that statement, as
-- issue_head_marks_drop runs: every statement of this release that writes one of the three tables
-- sets it as it runs (graphstore's KeptHeadRows), and its writer writes the head rows of what it
-- writes (graphstore.QueueHeads) in the same transaction; a writer of a release before sets
-- nothing, and its writes of a comment's blocks and their texts write no node, no tuple and no
-- document_blocks row, which the three triggers above would see. None of the four is deferred:
-- comments is a projection of the node store, and every check of such a table runs inside the
-- statement that writes its row, where its writer is named (api's
-- TestNoProjectionRowIsWrittenWithoutTheStore). An update fires them only where it sets a column
-- the head rows read, and the update trigger of comments only where a value the head rows read
-- moves: a reopen, a resolution, the anchor's orphaned or block_id, or the row's node address
-- moves none. Each issue's head fill lock is taken in share mode before its mark is deleted, in the
-- order of the locks' hash, as issue_head_marks_drop takes them and for the same reasons.
create function issue_head_items_drop() returns trigger
language plpgsql as $$
declare
  held text;
begin
  if tg_table_name = 'comments' then
    for held in
      select found.issue_key from (
        select new.issue_key
        union
        select old.issue_key
        union
        select head.issue_key from issue_head_nodes head
        where head.node_kind = 'comment' and head.node_address = coalesce(new.id, old.id)::text
      ) found (issue_key)
      where found.issue_key is not null
      order by hashtext('issue:' || found.issue_key), found.issue_key
    loop
      perform pg_advisory_xact_lock_shared(1212498244, hashtext('issue:' || held));
      delete from issue_head_marks mark where mark.issue_key = held;
    end loop;
  else
    for held in
      select item.issue_key from comments item
      where item.id = coalesce(new.comment_id, old.comment_id) and item.issue_key is not null
    loop
      perform pg_advisory_xact_lock_shared(1212498244, hashtext('issue:' || held));
      delete from issue_head_marks mark where mark.issue_key = held;
    end loop;
  end if;
  return null;
end
$$;

create trigger comments_head_marks after insert or delete on comments
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_items_drop();

create trigger comments_head_marks_update
  after update of issue_key, author, body, created_at, edited_at, deleted_at, deleted_by, reply_to, replies_to_block,
    ask_id, artifact_id, anchor, written_version on comments
  for each row
  when (current_setting('dispatch.head_rows', true) is distinct from 'kept'
        and (old.issue_key, old.author, old.body, old.created_at, old.edited_at, old.deleted_at, old.deleted_by, old.reply_to,
             old.replies_to_block, old.ask_id, old.artifact_id, old.anchor->>'artifact_id', old.anchor->>'version',
             old.written_version)
            is distinct from
            (new.issue_key, new.author, new.body, new.created_at, new.edited_at, new.deleted_at, new.deleted_by, new.reply_to,
             new.replies_to_block, new.ask_id, new.artifact_id, new.anchor->>'artifact_id', new.anchor->>'version',
             new.written_version))
  execute function issue_head_items_drop();

create trigger comment_blocks_head_marks
  after insert or delete or update of comment_id, block_id, node_id, position, excerpt, made_at, changed_revision,
    changed_at on comment_blocks
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_items_drop();

create trigger comment_block_texts_head_marks
  after insert or delete or update of comment_id, block_id, revision, text on comment_block_texts
  for each row when (current_setting('dispatch.head_rows', true) is distinct from 'kept')
  execute function issue_head_items_drop();
