-- 20261124182040_issue_pod_disks.up.sql — the disks of the pods of an issue's kept volume, one row
-- per issue and pod: each pod whose Sandbox owns the issue's kept volume and in which a role
-- started, with when that Sandbox was made, and whether the pod's disk holds work from before the
-- pod, which the issue's update round reads from the store.
--
-- A kept volume outlives its pod: an eviction, a launch's replacement, an update round's
-- replacement and an idle release keep it for the issue's next pod, until a reset mark or a change
-- of the disk's shape replaces the Sandbox with its volume. So a pod that mounts a kept volume an
-- earlier pod of the issue mounted holds what the roles of that earlier pod left unpushed. Nothing
-- else records which pod a volume was mounted by, for as long as the update round asks.
--
-- pod_uid is the pod. kept_since is when the API server made the pod's Sandbox, and it names the
-- volume, since the volume goes only with its Sandbox. holds_earlier_work is whether the pod's disk
-- holds work from before the pod: another pod of the issue recorded the same kept_since first. One
-- statement writes a row: a role's start in a pod whose Sandbox owns a kept volume
-- (store.StartKeptPod), in a transaction of its own, at every start, where the start's project
-- records the issue. A start whose record fails goes no further, so no role runs in a pod of a kept
-- volume that has no row. The mark is decided at that write: a Sandbox runs one pod at a time, so
-- at a pod's first record every other row of its volume is an earlier pod's, while the update
-- round can read a pod after the next pod of its volume has recorded. A later pod of a volume never
-- marks an earlier one, so the first pod of a volume is never marked, and no write clears a mark.
-- internal/daemon's update round reads the mark of its pod (store.PodHoldsEarlierWork,
-- workOnRoundPod).
--
-- The rows of an issue go when the issue's work is over: the outbox's forgetIdleRelease, at the
-- issue's close and at its tree's close (store.DeleteIssuePodDisks).
--
-- The migration adds a table and changes none that an earlier release reads, so a daemon of the
-- release before runs over the migrated database, as a rollback leaves it: it writes no row of the
-- table and deletes none. After a roll forward, a pod of a kept volume that ran under that daemon
-- has no row; where the earlier pods of a kept volume ran before the roll forward, the first pod of
-- that volume that this release records is not marked for them. An end over a pod that is not
-- marked names a role by its set alone; the roll forward's restart reads the set of every claim
-- that showed a session as unknown, so such a claim is named. The rows of an issue whose work ended
-- while that daemon ran stay, and only the records and the rounds of that issue's own pods read
-- them. No statement is owed on the way back, so this header carries none.
create table issue_pod_disks (
    issue              text        not null,
    pod_uid            text        not null check (pod_uid <> ''),
    kept_since         timestamptz not null,
    holds_earlier_work boolean     not null,
    primary key (issue, pod_uid)
);
