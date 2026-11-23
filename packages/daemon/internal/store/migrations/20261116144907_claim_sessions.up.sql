-- 20261116144907_claim_sessions.up.sql — every name of a session a claim has recorded, held to the
-- first claim of its project that recorded it.
--
-- A claim's agent reports its own session when it registers: an id and the session's file. The
-- daemon records both on the claim, enrolls the id with the secrets broker, whose enrollment takes
-- over the grants and pending requests of every other enrollment of the same pod account and
-- session (packages/envoy/internal/broker/enroll's adopt), and resumes the session at every
-- relaunch. Every pod of a project enrolls under that project's one account, so a role that
-- reported a sibling's session, or the session of another claim of its project, live or ended,
-- took that session's secret grants and could be relaunched into it (plan E's t-e-54). The
-- claims row keeps only the session a claim resumes now and the one it last
-- abandoned, so nothing recorded which names the other claims had recorded before: this table
-- does, one row per project, kind and name, written when a claim first records the name
-- (supervise's holdRecorded, through Store.HoldSessions), and deleted only by an operator's release
-- of its own claim (`legion claims release-names`), so a retired, failed or
-- suspended claim keeps every name it recorded. A registration, a resume or an enrollment under a
-- name another claim of the project holds is refused.
--
-- A name is each key a resume path or the broker finds a session by (supervise.HoldKind):
--   session    the session id, which the broker enrolls and a Claude Code launch resumes, and the
--              id a file named `<uuid>.jsonl` gives, which a Claude Code relaunch resumes;
--   file-name  the file's base name. Every key a resume finds a session by its file with is held by
--              it: an Oh My Pi launch resumes the recorded path (`--resume=<file>`), the launcher
--              finds the session's row by that path, and the tmux runtime resumes a file of the
--              same name one folder below a sessions directory when the path is missing. Two paths
--              that are one have one name, so no row holds a path alone;
--   file-id    the id after the first `_` of an Oh My Pi name `<timestamp>_<id>.jsonl`, by which the
--              tmux runtime resumes any `*_<id>.jsonl` there.
-- supervise.RecordedHolds is the same rule in Go, and the migration's test holds the two to one
-- answer.
--
-- A hold is a project's, as a daemon is: the daemon serves one project, and a claim of another
-- project is another daemon's. A role of one daemon that names another daemon's session takes
-- nothing of it at the broker, whose grants follow the daemon that enrolled a pod
-- (exchange.PodLaunchPrincipal).
--
-- No foreign key to claims: a hold outlives anything that becomes of its claim's row.
--
-- Every name the claims table records already is held for the claim that records it: of its
-- session, and of the session it last abandoned, which it recorded too. No two claims of a project
-- are meant to record one name, since every launch starts its agent on a session of its own
-- (claudelaunch.NewSessionID for Claude Code, Oh My Pi's own otherwise), so a name two claims record
-- is this hole already used, or a bug, and nothing recorded which claim took it first. Such a name
-- is held for neither: its claim is the empty string, which no claim's token is, so every claim is
-- refused every registration, resume and enrollment under it. The daemon logs each such name at boot
-- at Error, naming every claim whose current session records it, suspends each it can, and names at
-- Warn each it leaves as it was, being queued, launching uncertainly, failed or retired
-- (internal/daemon's suspendSessionConflicts). An operator records the rightful holder by hand:
--   update claim_sessions set claim = '<token>'
--     where project = E'<project>' and kind = E'<kind>' and name = E'<name>' and claim = ''
--       and '<token>' in (<the line's claims>);
-- (the line writes each value as an escape string literal, which reads the same whatever
-- standard_conforming_strings the operator's session sets)
-- and gives every other claim the line's `claims` names a new session with `legion claims
-- forget-session`, as its state allows: a suspended one is then resumed (`legion claims resume`), a
-- queued one launches by itself, a failed one is retried (`legion claims retry`) unless it is the
-- controller, which the controller keeper retries, and a retired one gets the forget alone. A claim
-- under a held suspension, or launching uncertainly, waits until that settles. A claim named only
-- in the line's `abandoned` needs nothing: a name only an abandoned session records suspends
-- nothing, since nothing resumes or enrolls an abandoned session, so the boot logs it at Warn alone.
-- docs/kubernetes.md gives the steps whole.
--
-- A daemon rolled back to a release before this one runs on this table without writing it, so the
-- claims it registers record names no row holds. This backfill runs once, so the daemon runs its
-- rule again at every boot (store.HoldRecordedSessions), holding each name no row holds for the one
-- claim recording it or for none, and suspends every claim whose current session records a name
-- another claim, or none, holds (store.SessionConflicts). After a rollback, each such claim is a
-- claim that took another's name while the rolled-back daemon ran: the operator gives it a new
-- session as above, and records a holder for each name held for none.
--
-- A session id or a file name longer than 255 bytes (claim.MaxSessionNameBytes) is held by no row:
-- the primary key's btree holds at most 2,704 bytes, so one such name would stop this migration,
-- and a registration of one is refused.
create table claim_sessions (
  project text not null,
  kind text not null check (kind in ('session', 'file-name', 'file-id')),
  name text not null check (name <> ''),
  claim text not null,
  recorded_at timestamptz not null default now(),
  primary key (project, kind, name));

-- A claim holds at most supervise.MaxHeldNamesPerClaim names: a hold ends only by an operator's
-- release, so without a bound one role could grow the table without end. A registration that adds names counts the
-- claim's rows (store.HoldSessions), which this index serves.
create index claim_sessions_claim on claim_sessions (project, claim);

with recorded as (
  select project, token, session, session_file as file from claims
  union all
  select project, token, abandoned_session ->> 'session', abandoned_session ->> 'sessionFile'
    from claims where abandoned_session is not null
), files as (
  -- base is filepath.Base: trailing separators removed, then what follows the last one; a path of
  -- separators alone is one separator. stem is base without `.jsonl`, null for any other name.
  select project, token, session, file, base,
    case when right(base, 6) = '.jsonl' then left(base, length(base) - 6) end as stem
  from (
    select project, token, session, file,
      case when rtrim(file, '/') = '' then '/' else regexp_replace(rtrim(file, '/'), '^.*/', '') end as base
    from recorded where coalesce(file, '') <> '') named
  where octet_length(base) <= 255
), names as (
  select project, token, 'session' as kind, session as name from recorded
    where coalesce(session, '') <> '' and octet_length(session) <= 255
  union all
  select project, token, 'file-name', base from files
  union all
  select project, token, 'file-id', substr(stem, strpos(stem, '_') + 1) from files
    where strpos(stem, '_') > 0 and substr(stem, strpos(stem, '_') + 1) <> ''
  union all
  select project, token, 'session', stem from files
    where stem ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
)
insert into claim_sessions (project, kind, name, claim)
select project, kind, name, case when count(distinct token) = 1 then min(token) else '' end
from names
group by project, kind, name;
