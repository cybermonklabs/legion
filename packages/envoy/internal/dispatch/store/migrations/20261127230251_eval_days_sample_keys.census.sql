-- 20261127230251 adds sample_keys, a nullable text[] with no default, to eval_days and to
-- eval_rebuild_days, writes eval_days_updated again with that column among those it compares, and
-- creates eval_days_sample_keys_kept and its trigger. No existing row is read, refused or rewritten:
-- every row reads null keys, which every read takes as it stands, and the trigger runs only on
-- later updates. It cannot name the column, which does not exist when the census is taken.
select 0
