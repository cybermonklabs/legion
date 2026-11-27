-- 20261126005335_check_states_owe_check_results.up.sql
-- A role's result, whichever build records it, marks the check states of its check's issue owed
-- (check_states_owe_result, 20261126005334). Its own migration, so the transaction holds
-- check_results' SHARE ROW EXCLUSIVE alone, for the catalog write (0122's precedent).
create trigger check_states_owe_check_results after insert on check_results
  for each row execute function check_states_owe_result();
