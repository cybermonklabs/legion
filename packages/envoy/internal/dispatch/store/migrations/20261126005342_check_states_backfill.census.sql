-- The backfill reads pull_request_heads and writes only check_states_owed, which 20261126005334
-- creates and which holds no row at deploy: its marks are new work owed, not a rewrite.
select 0;
