-- 20261125164530 adds two columns to pull_request_heads, two to check_results, and nullable
-- run_attempt to delivery_runs. Constant defaults and nullable columns rewrite no stored row.
-- It builds two partial indexes and creates commit_holds and deploys, including the internal
-- source-completion marker. No existing row is refused or changed by this migration.
select 0;
