-- 20261124072849_answer_tool_calls_tool.up.sql — the answer loop's tool that ran each call.
--
-- The loop's tools are gh, dispatch (Dispatch's own `dispatch` command, run as the stopped agent's
-- session, internal/dispatchtool) and web_search, the one after it, and every call of each is a
-- row of answer_tool_calls, its seq the call's place among all of its answer's calls. tool names
-- the tool that ran it, so a restart looks up only a gh call of unknown outcome on GitHub, the
-- re-run list names each call's tool, and the daemon's reply on a fault names each write by its
-- own. Every row before this one is a gh call, so the column's default is gh, and the check is the
-- one list of the loop's tools, web_search among them, so its change needs no migration of its
-- own. A dispatch or web_search row's app and owner are '', since it runs as no GitHub App.
--
-- A rollback below the release that answers for a stopped agent leaves the table and this column
-- as they are: no earlier release reads or writes answer_tool_calls (docs/kubernetes.md, "Rolling
-- back below the release that answers for a stopped agent").
alter table answer_tool_calls add column tool text not null default 'gh'
  constraint answer_tool_calls_tool_check check (tool in ('gh', 'dispatch', 'web_search'));
