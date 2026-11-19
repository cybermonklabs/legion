-- 20261117045607_issue_tree_rooms.up.sql
--
-- One room per issue (dispatch://INFRA-1253, R5). An issue's tree node is an artifacts row of a new
-- kind, tree, one per issue, and its Yjs document is the issue's room: the tree artifact's own
-- fragment, prosemirror, holds the nodes written into the issue rather than into a document, and
-- each converted document of the issue holds a fragment of its own there, named by its artifact
-- id. doc_updates, doc_snapshots and doc_checkpoints keep their artifact_id column, their foreign
-- key and every row: the room's history is stored under the tree artifact's id, and the rows of a
-- document's room from before its conversion stay where they are.
--
-- document_rooms holds one row per converted document, from its artifact_id to its room, the tree
-- artifact. A document with no row is its own room. docs.Service.ServeHTTP reads the row beside the
-- issue in the one statement its admission makes (docs' issueOpen), and every room entry point that
-- takes an artifact id resolves it the same way first. The index on room is the read of an issue
-- room's fragments, every document whose row names the room.
--
-- issue_trees holds each issue's one tree (D14 of the plan): a partial unique index on artifacts
-- would be an index built over a table this migration does not create.
--
-- artifacts' kind check is restated to admit tree. The new list admits every kind the old one
-- did, so no stored row fails it, and it is added not valid, which changes the catalog alone, as
-- 20261102233625 restated embeddings' kind check; nothing validates it later.
--
-- server_processes.capabilities is what each process's build serves (processes.Register writes
-- it): an issue is born converted only while every live process lists issue_room there (decision
-- 7 of the plan). A process of the release before writes nothing into it and holds null.
--
-- Rolling deploy. The release before this one names neither table and no tree row, writes every
-- column of server_processes it names, and reads no row of a kind it does not know: a tree row is
-- written only once every live process lists issue_room. Rolling back past this migration is an
-- image rollback alone; the tables and the column left behind are read by nothing that release
-- runs.
create table document_rooms (
  artifact_id uuid primary key references artifacts (id),
  room uuid not null references artifacts (id)
);
create index document_rooms_room on document_rooms (room);

create table issue_trees (
  issue_key text primary key,
  artifact_id uuid not null unique references artifacts (id)
);

-- issue_room_blocks is the projection of an issue room's block's in_document tuple (D16 of the
-- plan): one row for each document_blocks row of an artifact of kind tree, written and deleted in
-- the transaction that writes or deletes that row, naming the document whose fragment of the room
-- holds the block, or the tree artifact itself for a block of the tree's own fragment.
-- document_blocks.artifact_id is the tree there, and no migration of this child alters
-- document_blocks, so this table is where the block census reads a room block's document.
create table issue_room_blocks (
  tree_artifact_id uuid not null references artifacts (id),
  block_id text not null,
  document_id uuid not null references artifacts (id),
  primary key (tree_artifact_id, block_id)
);

alter table artifacts drop constraint artifacts_kind_check;
alter table artifacts add constraint artifacts_kind_check check (kind in ('doc', 'image', 'file', 'tree')) not valid;

alter table server_processes add column capabilities text[];
