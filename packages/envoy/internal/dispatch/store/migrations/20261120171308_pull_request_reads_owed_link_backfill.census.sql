-- 20261120171308 inserts owed reads of pull_request_reads_owed and leaves every existing row as it
-- is (`on conflict do nothing`): it refuses and rewrites no row.
select 0;
