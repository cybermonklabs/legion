-- 20261120220000_issue_tree_version_renders.up.sql — the versions rendered from the tree.
--
-- A version of a document is rendered from the tree's tuples once a pass of this release states
-- that version's tree, and every version written before keeps the bytes it stores. version_renders
-- holds one row for each version this release's writer rendered from the tree, inserted in the
-- transaction that writes the version (docs.queueStatedRenders, docs.QueueVersionRender), so the
-- row and the version commit together. A version with no row, or whose row holds no time, answers
-- artifact_versions.markdown byte for byte (docs.RenderVersion); the boundary is the row, so no
-- tuple, however it is dated, moves a version that holds none.
--
--   record_sha256  the digest of the record of the query whose record covers the version
--                  (docs.IssueTree.QueryRecord), which artifact_version_text_sha256 answers for the
--                  version (20261120220100) and so an approval of it pins; null for a version of a
--                  document no query record covers (an upload, a project's document, an issue
--                  room's own fragment), for one whose digest is owed, and for one no pass has
--                  stated.
--   at             the time the version renders as of: the time of the cut its pass read, no
--                  earlier than every tuple the writing transaction dated before it read the cut;
--                  null until a pass states the version.
--   read_seq       the highest graph_tuples.seq that cut holds; null until a pass states the
--                  version.
--   digest_owed    true from the write that inserts the row with its cut until the fill after
--                  that write's COMMIT records the record's digest (docs.fillOwedDigests), so the
--                  write keeps its window to one batch and its COMMIT (R15 of this branch's spec).
--   tree_mismatch  true where the pass that states the version found the rendering of the tree it
--                  states is not the version's stored markdown, byte for byte; the row then holds
--                  no time, and the census counts it in a class of its own until a repair.
--   tree_mismatch_repaired_at  the time of the repair that cleared tree_mismatch.
--
-- The partial indexes serve the block-index tick's reads of what is left to repair: the issues
-- whose record digests are still owed once their writes committed (docs.fillLostDigests), and the
-- versions a pass marked tree_mismatch, which each tick's repair takes first
-- (docs.repairTreeMismatches), so a tick that finds neither reads nothing else of the table.
--
-- A row goes with its document: the foreign key cascades, as every row of a document's own table
-- does, so a removal of an untouched document leaves none behind. No key reaches artifact_versions,
-- which a key would make the pre-deploy census size.
--
-- Rolling deploy. The table is new and holds no row; the release before names it nowhere and
-- reads every version's stored markdown, which is the render of every version this table holds.
-- Rolling back past this migration is an image rollback alone; the table left behind is read by
-- nothing that release runs, and its digest function (20261120220100) answers the stored markdown's
-- digest again only once that migration is rolled back too.
create table version_renders (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  number integer not null,
  record_sha256 text check (record_sha256 ~ '^[0-9a-f]{64}$'),
  at timestamptz,
  read_seq bigint,
  digest_owed boolean not null default false,
  tree_mismatch boolean not null default false,
  tree_mismatch_repaired_at timestamptz,
  primary key (artifact_id, number)
);
create index version_renders_digest_owed on version_renders (artifact_id) where digest_owed;
create index version_renders_tree_mismatch on version_renders (artifact_id, number) where tree_mismatch;
