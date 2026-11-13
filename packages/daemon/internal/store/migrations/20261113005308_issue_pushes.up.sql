-- 20261113005308_issue_pushes.up.sql — the newest push of each issue's branch the daemon has
-- recorded, which no saved copy of the issue's workspace older than it is ever sent past
-- (INFRA-1117).
--
-- A copy of a workspace is restored by `jj edit` back to its head and `jj bookmark set
-- --allow-backwards` of each bookmark it holds. A copy taken before the roles pushed is therefore
-- a copy that moves the issue's branch back behind what GitHub holds, and the role's next push
-- from that workspace removes the pushed commits from GitHub: the branch read `c2, c1, …` and
-- after the restore and the next push read `c3, c1, …`. A running copy is taken while the roles
-- work and push, and an update round replaces a pod just after its roles pushed, so the case is
-- common rather than rare. store.TakeWorkspaceCopyForRestore reads this row in the transaction
-- that chooses between the issue's two copies and refuses every copy whose `at` (when the shim
-- took it) is before pushed_at, whichever table it is in and whichever release or cadence left it.
--
-- The record is the push webhook itself (intake.Push, the workflow engine's push handler), for
-- every push of `legion/<KEY>` on the repository the issue is recorded on, whether or not the
-- issue has a pull request: a branch is pushed before its pull request opens, and an issue with
-- no pull-request record is exactly the one a guard that read the pull request would have sent a
-- stale copy for. pushed_at is the daemon's clock when it applied the push, not GitHub's, since it
-- is compared with copies the daemon asked for; head is the commit the push left, which the
-- refusal's log line names as the work the copy would have rolled back. One row per issue holds
-- the newest of them: a push applied late, behind a newer one, moves neither column.
--
-- An issue the daemon has recorded no push for has no row, and nothing of it is refused by this
-- rule, because nothing newer than its copies is known to be on GitHub. That is also what a
-- deployment meets the first time it runs this release: its pushes before the upgrade were never
-- recorded, so the rows its earlier daemon saved are refused only once a push is recorded after
-- them. A row outlives the issue's close, since the push it records happened and stays true; it is
-- one small row per issue. For the same reason a rollback to an earlier daemon deletes the rows of
-- workspace_running_copies (docs/kubernetes.md, "Rolling back the running copy"): that daemon
-- records no push while it runs, so after a roll forward the copies taken before the rollback
-- would be older than pushes this table never saw, and nothing here could refuse them.
create table issue_pushes (
    issue     text        primary key,
    pushed_at timestamptz not null,
    head      text        not null
);

-- The two outcomes an attempt can now record beside `sent` and `refused_older`. `refused_pushed`
-- is the rule above: the copy is older than the issue's newest recorded push, so it is sent to no
-- pod and its bytes stay for an operator. `superseded` is the older of the issue's two copies when
-- the newer one is offered to the pod: the pods after it hold that work's descendants, so the
-- older copy is closed unsent. Both spend the copy's one attempt, as `refused_older` does. A
-- daemon of an earlier release reads neither, since it never reads the outcome of a row whose
-- attempt is spent.
alter table workspace_bundles
  drop constraint workspace_bundles_attempt_outcome,
  add constraint workspace_bundles_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'refused_pushed', 'superseded') end);

alter table workspace_running_copies
  drop constraint workspace_running_copies_attempt_outcome,
  add constraint workspace_running_copies_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'refused_pushed', 'superseded') end);
