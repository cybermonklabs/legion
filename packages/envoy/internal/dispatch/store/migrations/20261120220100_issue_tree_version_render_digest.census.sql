-- The migration restates artifact_version_text_sha256 alone and writes no row: no trigger runs on
-- it and no review it pinned moves, since the update trigger refuses that. For every version that
-- holds no version_renders row - every stored version, since that table is new and holds none - it
-- answers the sha256 of the stored markdown, as it answered before. The census names no table only
-- a pending migration creates, and version_renders is one, so it counts nothing. The number of
-- existing rows this migration would refuse or rewrite is therefore 0.
select 0
