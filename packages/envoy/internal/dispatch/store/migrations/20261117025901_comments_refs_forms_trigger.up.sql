-- 20261117025901_comments_refs_forms_trigger.up.sql
-- A comment any build writes is owed an index with the forms 20261117025900 adds
-- (refs_forms_owed), on the writes 20261103205038's triggers owe refs_grammar_owed for: every
-- write of the body, and of the columns that say which document a `#id` in it resolves against
-- (the anchor's artifact_id, the document or the ask that owns it, the comment it replies to, and
-- its issue). These forms resolve against none of those columns, but a build that predates them
-- re-reads the comment's whole text on each such write, and that index deletes the rows these
-- forms wrote, so each owes the text again. Whatever the text says: no cheap SQL predicate finds a
-- tab address or a document's dashboard URL. A write this build makes deletes the row in its own
-- transaction (refs.replaceRefs), so only a write of an older build stays owed.
-- The update is a trigger of its own with a WHEN of its own, and the insert one of its own, for
-- 20261103205038's reasons: `update of ... anchor` fires on every anchor write, and a settlement's
-- anchor writes (docs/marks.go, docs/anchor_blocks.go) re-index nothing; a WHEN clause of an
-- insert trigger may not name OLD.
-- Its own migration, so the transaction holds comments' lock alone (0123's precedent).
create trigger comments_refs_forms_owed_insert after insert on comments
  for each row execute function refs_forms_owe('comment');
create trigger comments_refs_forms_owed_update
  after update of body, reply_to, anchor, artifact_id, ask_id, issue_key on comments
  for each row when (old.body is distinct from new.body
                     or old.reply_to is distinct from new.reply_to
                     or old.anchor->>'artifact_id' is distinct from new.anchor->>'artifact_id'
                     or old.artifact_id is distinct from new.artifact_id
                     or old.ask_id is distinct from new.ask_id
                     or old.issue_key is distinct from new.issue_key)
  execute function refs_forms_owe('comment');
