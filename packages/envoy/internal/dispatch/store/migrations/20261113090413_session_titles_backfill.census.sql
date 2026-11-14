-- A new table, session_titles_backfill, whose one row this migration inserts: it writes no
-- session_titles row, reads no other table, and refuses and rewrites nothing already stored.
-- The fill that row stands for runs at boot, in committed batches outside every migration
-- transaction (store.FillSessionTitles).
select 0;
