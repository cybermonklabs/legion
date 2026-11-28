-- 20261121060000_issue_tree_views.up.sql — the per-issue record of how an issue was converted
-- and whether its spec is its own.
--
-- A child issue is a view on its ancestor's spec where it was born converted, or converted by the
-- conversion with the conversion's view mark, its spec is not its own, and its nearest ancestor
-- that holds requirement nodes and is no view is not done (R23). issue_trees holds an issue's key
-- and its tree artifact alone, and both a child born converted and one the conversion reached
-- holding requirements of its own hold a row there, so it cannot hold these facts. issue_views
-- holds them, one row per issue that one of them reaches, and docs.ViewOf is its one reader:
--
--   converted    how the issue was converted: born, which POST /api/v1/issues writes in the
--                transaction that creates an issue born converted (api's createIssueTree), or
--                t19b, which the conversion writes; null for an issue no conversion reached.
--   view_marked  the conversion's view mark, set where the child it converts holds no requirement
--                node of its own, and so only on a row the conversion wrote.
--   own_spec_at  the own-spec mark: the time a requirement first reached the issue while it was no
--                view, set by a write through the API that holds a requirement block and by a pass
--                of the block index that states a requirement node of the issue
--                (docs.QueueOwnSpec), null until then. An issue that holds it is never a view.
--
-- Every writer upserts on issue_key, so the writers meet in any order. The mark keeps its first
-- time, and no statement sets it back to null. An issue with no row reads as one with null, false
-- and null.
--
-- Rolling deploy. The table is new and holds no row; issue_trees, whose rows alone could say an
-- issue was converted, is created by a migration of this same release
-- (20261117045607_issue_tree_rooms), so no stored issue holds a conversion to carry over. The
-- release before names the table nowhere. Rolling back past this migration is an image rollback
-- alone; the table left behind is read by nothing that release runs.
create table issue_views (
  issue_key text primary key,
  converted text check (converted in ('born', 't19b')),
  view_marked boolean not null default false check (not view_marked or converted = 't19b'),
  own_spec_at timestamptz
);
