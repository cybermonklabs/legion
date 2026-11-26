-- 20261125114600_claim_session_agent.up.sql — the agent whose session a claim holds.
--
-- claims.session_agent is supervise.Claim.SessionAgent: the agent whose session the claim holds,
-- in the identity a step's start names it by (plan.Agent.ID): `key:<key>` for the agent of a key,
-- `step:<place>:<task>` for the agent of a step with no key. One name can go to two agents of an
-- issue in turn — a later version of a plan can give the name a stopped task's agent ran under to
-- a new agent — so the claim records which of them its session is. Each start of a step names its
-- step's agent (record.SuperviseRequest.Agent). The decision that brings the claim back leaves the
-- session behind where the two differ, so the new agent starts in a session of its own, and
-- records the start's agent once the claim is admitted. A start that gives its task to the process
-- a claim still runs records that start's agent where the claim records none. Besides this file,
-- nothing else writes it.
--
-- Every row the table already holds records the agent that ran on it last where a start the
-- release before kept shows which, else the agent the issue's plan rows give its name last, and
-- takes the default, the empty string, which records no agent, where neither names one: the next
-- start of a step's agent that reaches such a claim takes the session it holds, as the release
-- before took it, and records that agent, whether the start brings the claim back or gives its
-- task to the process the claim still runs.
--
-- The agent that ran on a claim last is read from step_starts, where the release before kept the
-- latest start of each place: the claim's token (agent), the moment (started_at) and the start's
-- record, which names its generation. The claim's latest start, by its moment, names a place and a
-- generation; the plan_steps row there, where it still holds the claim's name, names the agent,
-- in the identity plan.Agent.ID gives it: `key:<key>` where the row names a key,
-- `step:<place>:<task>` where it names none. Where the claim has no such start, or the row at its
-- latest start's place no longer holds its name (a later version replaced it), the agent is the
-- one the rows give the claim's name last: the latest plan_steps row of the issue that holds the
-- name, in the order record.Postgres.PlanStepsOf reads the rows (generation, task, place). A row of
-- the owner, or one that holds no name, is left out of both, as record.NameAgents takes no name
-- from it. A claim's name is the last part of its token after the issue's part (claim.Token.Cut).
-- NameAgents keeps a key's name from the rows and from the claims that record a key's session
-- (record.NameClaimsOf), and a write of a plan deletes rows: a version replaces the rows of each
-- task it gives (record.Postgres.PutPlan), a later version removes a task with its rows
-- (DeleteTasks), and the first step of a new owner's run deletes the plans an earlier owner gave
-- (workflow's passOverEarlierOwners). Left recording no agent, a claim would keep nothing of whose
-- session it holds once those rows were gone: a later version could give its name to a new agent,
-- whose first start would take the conversation of the agent that ran on the claim. Recorded, the
-- claim keeps a key's name for the key whatever rows a later write deletes, the agent it records
-- keeps its session, and the start of any other agent that reaches the claim, one the rows name but
-- that has not started among them, starts a session of its own.
--
-- The rule is the function claim_agent_from_steps(issue, token), which this file defines and fills
-- the claims by, and which the daemon asks again for a start that names no agent, of a claim that
-- records none and has run nothing (the outbox's start, store.ClaimAgentFromSteps), on whichever
-- try of the start made the claim: a start the release before queued names no agent, and where its
-- claim is not made yet, it makes the claim when it runs, so the daemon records on that claim the
-- agent the function gives then. The function is one select of the plan_steps rows that hold the
-- claim's name: the row at the place of the claim's latest kept start first, then the latest. The
-- facts the function reads do not
-- move under such a start while it can still act: its own start is the latest kept start of its
-- claim's token, written when it was queued, and a later version that would replace its step's
-- row is refused while its task runs, a new owner's run is a later generation, whose fence ends
-- the start, and no step_starts row is ever deleted. Where its claim stands, the start takes the
-- session that claim records, which this file wrote. So the file writes no queued row: a daemon of
-- the release before, of any project on the database, reads each one as that release wrote it.
--
-- What the record leaves: where no start the release before kept names the agent that ran on a
-- claim last (a run older than step_starts, or a place whose row a later version replaced), the
-- claim records the agent the rows give its name last, so where that agent had not started at the
-- upgrade, its first start takes the session the claim holds, that of the agent that ran there
-- before it, as the release before did, and the agent that ran there, named again, starts a
-- session of its own. And the release before gave a key's name to an agent with no key in a later
-- version, which this release never does, so where that agent ran on the claim last, the claim
-- records it, while NameAgents still holds the name for the key as long as the key's earlier row
-- stands, since a later row of the name with no key does not end a key's hold: a later version
-- names that agent with no key `<role>-2`, in a claim and a session of its own.
--
-- The release before this one names each column it reads and writes (the claim store's
-- claimSelect and upsertClaim), so it neither reads nor writes this one, and every row it inserts
-- takes the default.
--
-- That release does not know the key `agent` of a supervise row, which this release writes on the
-- start of each step. It decodes an outbox payload strictly (record.DecodeOutboxPayload,
-- DisallowUnknownFields), so a start still queued with the key would fail its decode at every
-- attempt, and fail every read of its role's runs (record.Store.RoleRuns decodes the queued
-- supervise rows of each run it reads). The update takes the key off each such row and leaves
-- every other key, and that release then starts the claim as it did, resuming the session it
-- holds. The update is owed to that release, not to the roll-forward.
--
-- The fill writes the claims of every project the database holds: a migration is one static file
-- that each database runs once (store.Store.apply), and nothing in it names the project of the
-- daemon that runs it, so it cannot keep to one project, and no later run would fill another
-- project's claims. The release before reads no column it does not name, so that costs it
-- nothing, and the file writes no queued row, which that release decodes strictly. The fill
-- writes the claims that stand when it runs: a claim that a daemon of the release before, of
-- another project on the same database, makes after this file ran records no agent, since that
-- release writes the column's default, and nothing fills it later. The first start of a step's
-- agent that reaches such a claim takes the session it holds and records that agent, as for a
-- claim the fill names no agent for.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with this release's daemon stopped and before the release
-- before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The drop and the delete of
-- this migration's row go together: the delete is what makes the version the store reports that
-- release's own again, and both are owed to the roll-forward, which then runs this file again: it
-- adds the column at its default and the function again, and records on each claim the agent the
-- steps and the rows then give it. What the drop costs is the record of each claim's agent: after
-- the roll-forward a claim records only the agent its latest kept start or the rows give it, or
-- none, so a claim whose session was another agent's when the rollback came is resumed once for
-- the next start that reaches it of the agent it records, or of any agent where it records none,
-- as the release before resumed it. What the update costs is that same record on each start it
-- strips: the release before resumes the claim's session for it.
--
-- begin;
-- update outbox set payload = payload - 'agent' where kind = 'supervise' and payload ? 'agent';
-- drop function if exists claim_agent_from_steps(text, text);
-- alter table claims drop column if exists session_agent;
-- delete from schema_version where version = 20261125114600;
-- commit;
alter table claims add column session_agent text not null default '';
create function claim_agent_from_steps(claim_issue text, claim_token text) returns text language sql stable as $$
  select case when p.agent_key <> '' then 'key:' || p.agent_key else 'step:' || p.position || ':' || p.task end
  from plan_steps p
  left join (select task, place, (record->>'generation')::bigint as generation
      from step_starts
      where issue = claim_issue and agent = claim_token and kind = 'step' and record ? 'generation'
      order by started_at desc nulls last limit 1) s
    on p.generation = s.generation and p.task = s.task and p.position = s.place
  where p.issue = claim_issue and p.agent_key <> 'owner' and p.agent <> ''
    and p.agent = substring(claim_token from '^legion-[^-]+-[^-]+-[^-]+-(.+)$')
  order by s.place is not null desc, p.generation desc, p.task desc, p.position desc
  limit 1
$$;
update claims c set session_agent = f.agent
from (select token, claim_agent_from_steps(issue, token) as agent from claims) f
where f.token = c.token and f.agent is not null;
