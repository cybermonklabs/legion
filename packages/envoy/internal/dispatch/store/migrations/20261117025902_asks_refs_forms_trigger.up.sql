-- 20261117025902_asks_refs_forms_trigger.up.sql
-- An ask any build writes is owed an index with the forms 20261117025900 adds (refs_forms_owed),
-- on the writes 20261103205039's first two triggers owe refs_grammar_owed for: every write of the
-- question, and of the columns that say which document a `#id` in it resolves against (the
-- document its block is on, the anchor's artifact_id, the document that owns it, its issue). A
-- build that predates these forms re-reads the question on each such write and deletes the rows
-- they wrote, so each owes the text again, as the comments trigger before it says.
-- It owes every write of the options and of the answer as well, which 20261103205039 does not
-- watch: they are the ask's other parts (refs.AskParts), and a build that predates these forms
-- re-indexes every part with the grammar it knows on such a write - an answer through the one row
-- write its transition makes (`update asks set state = 'answered', answer = ...`, api's
-- closeAskTx), an edit of the options alone through the options - so a form the answer or a new
-- option holds is never indexed, and the part edges these forms wrote are deleted from
-- ask_block_refs. A resolution writes neither column; the part edges its re-index deletes owe the
-- ask through ask_block_refs' own trigger (20261117025910).
-- The scope triggers of 20261103205039 have no counterpart: these forms change no `#id`, so a
-- question block's settlement moves none of their edges.
-- The update is a trigger of its own with a WHEN of its own, and the insert one of its own, for
-- the comments trigger's reasons.
-- Both ride in one migration: they are triggers of one table, so one transaction holds asks' lock
-- once (0124's precedent).
create trigger asks_refs_forms_owed_insert after insert on asks
  for each row execute function refs_forms_owe('ask');
create trigger asks_refs_forms_owed_update
  after update of question, options, answer, anchor, block_artifact_id, artifact_id, issue_key on asks
  for each row when (old.question is distinct from new.question
                     or old.options is distinct from new.options
                     or old.answer is distinct from new.answer
                     or old.anchor->>'artifact_id' is distinct from new.anchor->>'artifact_id'
                     or old.block_artifact_id is distinct from new.block_artifact_id
                     or old.artifact_id is distinct from new.artifact_id
                     or old.issue_key is distinct from new.issue_key)
  execute function refs_forms_owe('ask');
