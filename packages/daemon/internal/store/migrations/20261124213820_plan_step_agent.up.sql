-- 20261124213820_plan_step_agent.up.sql — the name of the agent of each step of a plan (record's
-- agents.go and plan.go).
--
-- plan_steps.agent is record.PlanStep.Agent: the name of the step's agent, the last part of its
-- claim token (claim.AgentToken), so the steps of one key share one name and one claim in every
-- task, version and generation of the issue. record.NameAgents gives it before a version's steps
-- are written, from the issue's earlier rows, and nothing else writes it. A step of the key owner
-- holds none: its agent is the tree's architect.
--
-- plan_steps.leave_session is record.PlanStep.LeaveSession: the session the claim of the step's
-- agent recorded when the step was written, where that agent takes a name a key's agent held in an
-- earlier generation, so its claim's session is that key's. Only the project's default steps
-- write one (workflow's leaveKeySessions); every start of the step's agent carries it
-- (record.SuperviseRequest.LeaveSession), and the claim leaves it where it still records it. Every
-- other row holds the empty string, and so does every row the table already holds.
--
-- plan_key_names is record.KeyName, the lasting record of every name a key's agent held on an
-- issue: record.PutPlan writes one row for each step of a key (but the owner's), and
-- record.NameAgents reserves each name for its key beside the plan_steps rows. The rows of a plan
-- go when a later version gives the task again (PutPlan) or the first step of a run passes over
-- an earlier owner's plan (DeletePlan); no deletion of a plan touches this table, and no code
-- removes a row of it: a stored claim keeps the
-- session it records across the issue's generations (supervise's launch resumes it), so a name a
-- key held names that key's session for as long as the issue does. The table names no issues row,
-- as the plan tables name none (20261117034837_issue_plans), because an owner can give a plan
-- before the daemon records its issue, at generation 0, and PutPlan writes this table with it; so
-- an operator who removes an issue's record by hand removes its rows here too. Each row holds the
-- latest generation whose steps gave the key that name (record.KeyName.Generation), which tells a
-- row with no key that shows the name before the key held it from one written after it (workflow's
-- handedBefore). The table is filled below from the plan_steps rows this migration names.
--
-- Every row the table already holds is named by the update below with the name record.NameAgents
-- gave it, where the rows it holds still show that name. A step with no key is named for its role:
-- the release before this one ran each role at most once in a task and refused every key but the
-- owner's, so the role of each of its steps named that step's agent, and every agent keeps the
-- claim it had. A step of the key owner keeps the empty string. A step of any other key is a row
-- this release wrote before a rollback, whose name the way back below dropped, and it takes the
-- role of the key's first row of its issue by generation, task and place. NameAgents gives a key
-- the name of its newest earlier row, and the version that wrote that row gave it the name the
-- generation before held, so every generation's rows of the key hold the name of its first
-- generation: the role of its first step there. The first row is that step while the first
-- generation's rows stand as its first version wrote them, and the role of the key's first step in
-- a later generation is not its name. So the key's steps share one name in every task and
-- generation, the name the next version gives the key too.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with every daemon on that database stopped, before the earlier image is pinned:
--
--   PGPASSFILE=<pgpass file> psql '<postgres_url>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   update outbox set payload = payload - 'claim'
--   where kind in ('supervise', 'issue_bundle', 'update_ask', 'hook_prompt', 'merge_forward', 'controller_notice') and payload ? 'claim';
--   update outbox set payload = payload - 'refusedPod' - 'leaveSession' where kind = 'supervise' and (payload ? 'refusedPod' or payload ? 'leaveSession');
--   update claims set role = substring(token from '^legion-[^-]+-[^-]+-[0-9]+-(.+)$')
--   where role <> substring(token from '^legion-[^-]+-[^-]+-[0-9]+-(.+)$');
--   alter table plan_steps drop column if exists agent;
--   alter table plan_steps drop column if exists leave_session;
--   drop table if exists plan_key_names;
--   delete from schema_version where version = 20261124213820;
--   commit;
--   SQL
--
-- The release before this one boots over the column: its plan_steps reads and writes name each
-- column they touch (record's PutPlan, Plan and CarryPlan), so the column would stand there unread,
-- and every row that release inserts would take the default. The drop and the delete are owed to
-- the roll forward: with the column dropped and the row deleted, store.apply finds no row of this
-- version and runs this file again, which adds the column again and names every row again, as
-- above. What the drop costs is the name of a key that came from a row no longer stored: a key whose
-- first generation's later version gave its first place to a step of another role, whose write
-- deleted the rows the name came from, is named for the role of its first row that stands, and so
-- can take another name. The fill reads tasks in the order of their ids, where NameAgents reads a
-- version's tasks in the plan's order; this release's plans hold one task (plan.Supported).
-- A rollback past 20261117034837_issue_plans, which drops plan_steps whole, runs this way back
-- first: its row would otherwise stand after that table is gone, and the roll forward would create
-- plan_steps again with no agent column, which every plan read names.
--
-- The update of the outbox is owed to the release before, not to the roll forward. This release
-- names the claim a row acts on where the row's producer holds it: a supervise request, a release's
-- workspace check, an update round's ask, a hook's prompt and a merge forward's message each carry
-- the key claim (record.SuperviseRequest.Claim, IssueWorkspaceCheck.Claim, UpdateAsk.Claim,
-- HookPrompt.Claim and MergeForwardMessage.Claim), and so does a controller notice of a kind that
-- stands per claim, the provider-errors notice (ControllerNotice.Claim). The release before knows
-- no such key and decodes an outbox payload strictly (record.DecodeOutboxPayload,
-- DisallowUnknownFields), so a row still queued with it would fail its decode at every attempt and
-- retry until the roll forward. The update takes the key off each such row, and the release before
-- then acts on the claim of the row's role, as it did, and keys a notice by its issue and role.
-- Only the top-level key of those six kinds goes: a notice's update round names claims of its own
-- under the same key, which that release reads.
--
-- The update of a queued start's `refusedPod` and `leaveSession` is owed to the release before as
-- well. This release marks a start a refused launch queues with the pod whose refusal queued it
-- (record.SuperviseRequest.RefusedPod), and a start of a step whose agent takes a key's name with
-- the session to leave (record.SuperviseRequest.LeaveSession), keys that release does not know and
-- decodes strictly as it decodes `claim`, so the update takes them off each queued supervise row
-- that carries them.
--
-- The update of claims is owed to the release before too. This release moves a claim to the role of
-- the step its key works (supervise.Machine.Rerole writes claims.role), and the release before
-- finds a step's worker by the token it builds from the step's role but grants a claim the role
-- claims.role holds. A claim left in the role of its key's step would be read as that role's worker
-- beside the claim the release before makes for that role's token, and would be resumed in that
-- role on a step of its own token's role. The update puts each claim back in the role its token
-- names, the token's last part (claim.Token.Cut); the controller's token names no issue and matches
-- nothing, so its claim keeps its role. A claim moved while it works a step is suspended before the
-- rollback (docs/kubernetes.md), since the update moves no process: the release before then starts
-- that step's own worker, and resumes the moved claim only in its own role.
alter table plan_steps add column agent text not null default '';
update plan_steps s set agent = case when s.agent_key = '' then s.role else
  (select k.role from plan_steps k where k.issue = s.issue and k.agent_key = s.agent_key
    order by k.generation, k.task, k.position limit 1) end
where s.agent_key <> 'owner';
alter table plan_steps add column leave_session text not null default '';
create table plan_key_names (
  issue text not null,
  name text not null check (name <> ''),
  agent_key text not null check (agent_key <> '' and agent_key <> 'owner'),
  generation bigint not null check (generation >= 0),
  primary key (issue, name));
insert into plan_key_names (issue, name, agent_key, generation)
select distinct on (s.issue, s.agent) s.issue, s.agent, s.agent_key, s.generation
from plan_steps s
where s.agent_key <> '' and s.agent_key <> 'owner' and s.agent <> ''
order by s.issue, s.agent, s.generation desc, s.task desc, s.position desc;
