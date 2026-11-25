-- A personal agent token records, when it is minted, the Google account (`sub`) of the credential
-- that minted it (agent_tokens.subject, migration 20261117064356), and Dispatch holds the token to
-- that account from then on: it acts, and its approves name that account, only while the address's
-- people.sub records the same one (api.personalTokenActor, api.approverStanding, the mint's
-- re-check). A token that records none is held to its creation time (agent_tokens.created_at)
-- instead, against the moment the address's account was recorded (people.sub_recorded_at). That
-- holds only if neither ever changes, so this trigger refuses every update that would change
-- either, null to a value included, whatever code or migration sends it, with SQLSTATE 23514
-- (check_violation). It fires only on an update that names one of the two in its SET list
-- (`before update of`), so a write that names neither (a token's revoke, its last use, its
-- scopes) costs nothing, and one that restates them passes. The function's name is Dispatch's
-- own: the broker's migrations may share this schema (CI runs both sets on one database), and
-- broker migration 0043 has a function of its own for the same refusal
-- (TestTheBrokerAndDispatchSchemasShareOneDatabase).
-- This header was edited in place after the migration identity check arrived, three times (the
-- broker migration's number, then these lines, then that number again), and the three lines below
-- name the texts before each edit, so a database that applied any of them still boots.
-- replaces: 20261117160116_agent_token_subject_unchanged.up.sql sha256:68676325a89e13a4e8b67d7ba8f445a23788e5d2eb47d26d93c6a12c197b359e
-- replaces: 20261117160116_agent_token_subject_unchanged.up.sql sha256:da2643939aaad25b9b4626c4a99af86e77addc9595d637ce2476ebeafd40dff6
-- replaces: 20261117160116_agent_token_subject_unchanged.up.sql sha256:47d1ca5c8d1d532ec9517681045958a96060f545c68585df63de0b00e04e2a50
create function refuse_agent_token_account_change() returns trigger language plpgsql as $$
begin
  raise exception 'the Google account % records is written once, when it is made, and is never changed', tg_table_name
    using errcode = 'check_violation';
end
$$;

create trigger agent_tokens_subject_unchanged before update of subject, created_at on agent_tokens for each row
  when (old.subject is distinct from new.subject or old.created_at is distinct from new.created_at)
  execute function refuse_agent_token_account_change();
