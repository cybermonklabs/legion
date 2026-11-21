-- 20261120213507 adds a nullable column with no default to artifact_versions and writes no row:
-- every stored version reads null there, which every read answers as its stored text, as before.
-- Nothing is refused or rewritten.
select 0;
