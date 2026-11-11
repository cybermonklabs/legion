-- 20261111021610_ask_declined.up.sql -- a person may decline to answer an ask, and the daemon
-- keeps that decline on the ask's mirror row so it can tell the root's architect so, in Dispatch's
-- own words (INFRA-1282).
--
-- Dispatch closes a question nobody will answer with the resolution kind `declined`, whose payload
-- carries the person's note apart from the four sentences Dispatch writes for the asking agent
-- (its model.DeclineDispatchWords). root_asks keeps both as they arrived: declined says the ask
-- was closed that way, declined_note the person's own note (null where they wrote none, as every
-- other optional text of this table is) and declined_words those sentences. The words are kept as
-- data rather than written into the daemon as a constant of its own: a constant here would be a
-- second copy of four sentences with nothing holding the two equal, and the two packages release
-- apart.
--
-- The decline is a fact of the ask, like kind and by_tree beside it, so it is kept whatever order
-- the ask's events arrive in and has no fence of its own: a question closed with no answer stays
-- declined, and Dispatch's settlement restoring such an ask restores that same close. Every row
-- recorded before this migration takes the default, which is what the record of those asks says:
-- no decline was ever read for them.
--
-- The ninth value of the answer check is `declined`, the act that answered a root's wait: a
-- person's decline of an ask of the root, the one the wait names or any other
-- (record.AnsweredByDecline). Postgres has no `alter constraint`, so the column's own check is
-- dropped and added again with the ninth value beside the eight 0057_awaiting_person.up.sql
-- declared.
--
-- What an older daemon binary reads on this schema. It never writes `declined`, since it has no
-- such value, and the check is never narrowed again, so a row this release wrote stands under it:
-- that binary reads such a wait, resumes the root through workflow's resumeTask, whose switch has
-- a default arm, and tells the architect that a person acted on the issue and to read its asks and
-- its spec. So the root starts again and the architect still finds the decline in Dispatch; what
-- is lost is Dispatch's words and the person's note in the task itself. It reads none of the three
-- new columns either — its root_asks reads name the columns it knows, and the new ones take their
-- defaults on its writes — so the decline stays on the row for the roll forward, while any row it
-- writes over records no decline it never read.
--
-- There is no down file: migrations here are forward-only.
alter table root_asks add column declined boolean not null default false;
alter table root_asks add column declined_note text;
alter table root_asks add column declined_words text;

alter table issues drop constraint issues_awaiting_answered_by_check;
alter table issues add constraint issues_awaiting_answered_by_check check (
  awaiting_answered_by in ('ask', 'retracted', 'closed', 'reply', 'turn', 'approved', 'changes_requested', 'version', 'declined'));
