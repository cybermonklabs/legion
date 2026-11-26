-- 20261125023100_pod_replacements.up.sql — the record of each replacement of an issue pod that may
-- have taken a disk, kept from before the act until the issue's architect's notice of it is queued.
--
-- What this adds: pod_replacements, one row per destructive act on an issue's pod, keyed by the
-- project and the act's own identity (operation), never by the pod: two acts on one pod are two
-- rows, and the daemon writes no row over another (GPT-6.1 Sol's point 3 on #5285 at fa9deb04).
-- pod_uid is the pod the act takes, '' where the issue's Sandbox ran no pod of its own. takes is
-- what the act takes, and target_uid the uid of what it takes (GPT-6.1 Sol's point 4): 'pod', the
-- suspension of the issue's Sandbox over the pod of that uid, which takes a disk that is the pod's
-- own with it, or 'volume', the delete of the issue's Sandbox of that uid, which takes the kept
-- volume it owns. A Suspended Sandbox shows the first made, and only a Sandbox of that uid gone
-- shows the second. with_pod is the act's WithPod (runtime.TakenPod.WithPod): a launch's delete of
-- the issue's Sandbox and its kept volume over pod_uid, a live pod of that Sandbox, which takes
-- that pod with the volume, so a record of the pod's own act is over with it (the Legion
-- reviewer's Important on #5285 at 69e00593). The daemon writes the row before the runtime acts:
-- an update round's replacement before the call whose last word the runtime asks under the pod's
-- launch turn, one made at a launch as the launch's reading of the round answers it, and a
-- launch's own replacement before its suspension (R7 of #5285, GPT-6.1 Sol's points 1 and 2 on
-- #5285).
-- record is the daemon's own encoding of the replacement: the round and its recorded reasons, or
-- the launch's claim and why it replaced the pod, and the read of the workspace made before the
-- act, what it found or the error it failed with. written_at is the moment the row was written, by
-- the database's clock. Once the act is made the daemon deletes the row and queues the architect's
-- workspace-taken notice in the same transaction, so the notice goes out exactly once whatever
-- stops between. A replacement whose call failed, or whose daemon stopped, after its act landed is
-- finished from its row at a later sweep, from the read made before it, never from a read made
-- after; a row whose act the runtime shows was not made is deleted, and one whose act the runtime
-- cannot show made or not stands.
--
-- The release before reads and writes no such row. A rollback to it drops the table with whatever
-- rows it holds, and each of them is a notice the architect is not sent: run the way back below,
-- the last paragraph of this header (migrations.WayBack), with every daemon of the database
-- stopped, before the earlier image is pinned, and a rollback past 20261123050533 runs it first
-- (docs/kubernetes.md, "Rolling back the release that keeps no saved workspace"). It drops the table
-- and deletes this migration's version row, so a roll forward runs this file again.
--
-- drop table if exists public.pod_replacements;
-- delete from public.schema_version where version = 20261125023100;
create table if not exists pod_replacements (
    project text not null check (project <> ''),
    operation text not null check (operation <> ''),
    issue text not null check (issue <> ''),
    pod_uid text not null,
    takes text not null check (takes in ('pod', 'volume')),
    target_uid text not null check (target_uid <> ''),
    with_pod boolean not null default false,
    record jsonb not null,
    written_at timestamptz not null default now(),
    primary key (project, operation)
);
