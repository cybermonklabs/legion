-- 20261125005340_check_states_owe_ci_check_results.up.sql
-- A CI job's result marks the check states of its check's issue owed (check_states_owe_result,
-- 20261125005338). Its own migration, so the transaction holds ci_check_results' SHARE ROW
-- EXCLUSIVE alone, for the catalog write.
create trigger check_states_owe_ci_check_results after insert on ci_check_results
  for each row execute function check_states_owe_result();
