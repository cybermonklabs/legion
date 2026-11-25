-- 20261117025903_messages_refs_forms_trigger.up.sql
-- An issue message any build writes is owed an index with the forms 20261117025900 adds
-- (refs_forms_owed), on 20261103205040's predicate: every insert and every write of the body of a
-- message with an issue. A direct message is indexed by nothing, so it owes nothing.
-- Its own migration, so the transaction holds messages' lock alone (0125's precedent).
create trigger messages_refs_forms_owed after insert or update of body on messages
  for each row when (new.issue_key is not null) execute function refs_forms_owe('message');
