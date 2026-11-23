-- 20261121060636 creates one function and replaces the bodies of two: artifact_versions_search,
-- the document search trigger's function, and issue_search_vector, which the issues trigger and
-- 20261105145243's trigger on artifact_versions call. It names no table, so it locks none above
-- ACCESS SHARE, and it writes no row: each stored vector stays as it is until its row is written
-- again, and only text that holds an ask block between two characters of an underscore run is
-- indexed differently then.
select 0;
