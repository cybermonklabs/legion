-- 20261124021727_answer_questions_thread_reply.up.sql — the answer loop's question of a reply in a
-- comment thread.
--
-- A reply in a Dispatch comment thread that mentions nobody is a question for the stopped agent
-- that wrote the newest comment before it, or the thread's root after a person's, and the loop
-- answers it in that thread (answerloop.KindThreadReply). Its row's kind is thread_reply, so the
-- check names it beside the four kinds 20261106214002 wrote. The check is one list, so this
-- migration names every kind.
--
-- A rollback below the release that answers for a stopped agent leaves the table and this check
-- as they are: no earlier release reads or writes answer_questions (docs/kubernetes.md, "Rolling
-- back below the release that answers for a stopped agent").
alter table answer_questions drop constraint if exists answer_questions_kind_check;
alter table answer_questions add constraint answer_questions_kind_check
  check (kind in ('message', 'mention', 'ask_reply', 'envoy', 'thread_reply'));
