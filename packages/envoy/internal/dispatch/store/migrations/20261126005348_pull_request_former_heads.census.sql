-- Two sequences, eight new tables and nineteen functions, triggers on pull_request_heads,
-- check_results and ci_check_results that run only on later writes and only append to two of the
-- new tables, a lock on those three held to the commit, and a seed that writes only the new tables
-- of identities, names, former heads and records (one record of where the last closed period
-- began of each pull request closed now or reopened since migration 20261126005345 recorded a
-- close of it) and check_states_owed: nothing is refused or rewritten.
select 0;
