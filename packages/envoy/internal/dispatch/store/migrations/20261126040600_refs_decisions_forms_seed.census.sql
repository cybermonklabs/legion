-- 20261126040600 writes only refs_decisions_forms_seed_owed, which 20261126040000 of the same
-- release creates and which holds no row at deploy; it refuses and rewrites no existing row. The
-- rows it inserts are new work owed, not a rewrite, and a non-zero count would refuse every deploy
-- over a text already stored.
select 0
