-- 20261113005308_issue_pushes.up.sql — the newest push of each issue's branch the daemon has
-- recorded, which no running copy of the issue's workspace older than it is ever sent past
-- (INFRA-1117).
--
-- A copy of a workspace is restored by `jj edit` back to its head and `jj bookmark set
-- --allow-backwards` of each bookmark it holds but the issue's own branch `legion/<KEY>`, which no
-- restore sets (runtime.RestorableBookmarks): the branch stays where the new pod's fetch from
-- GitHub put it, so no restore can move it behind the pushed commits. This record is the second
-- line. A running copy is taken while the roles work and push, and an update round replaces a pod
-- just after its roles pushed, so a running copy taken before the push is common: restoring it
-- would `jj edit` the workspace back behind the push and set its other bookmarks there, and the
-- roles would have to bring their work forward over the pushed commits.
-- store.TakeWorkspaceCopyForRestore reads this row in the transaction that chooses between the
-- issue's two copies and refuses a running copy whose `at` (when the shim took it) is before
-- pushed_at (runtime.DecideWorkspaceRestore). It guards running copies only: a release bundle is
-- taken after the issue's roles stopped, so a push recorded after it is no push of the work it
-- holds, and the bundle is restored as any other.
--
-- The record is the push webhook itself (intake.Push, the workflow engine's push handler), for
-- every push of `legion/<KEY>` on the repository the issue is recorded on, whether or not the
-- issue has a pull request: a branch is pushed before its pull request opens, and an issue with
-- no pull-request record is exactly the one a guard that read the pull request would have sent a
-- stale copy for. pushed_at is the daemon's clock when it applied the push, not GitHub's, since it
-- is compared with copies the daemon asked for; head is the commit the push left, which the
-- refusal's log line names as the work the copy would have rolled back.
--
-- One row per issue holds the newest of them, and pushed_at cannot say which that is: every push
-- is applied at the daemon's own now, so a redelivered webhook, the per-issue read's rebuild of a
-- push whose webhook then arrives as well, and a webhook an hour late are each applied later than
-- the push they repeat or that replaced them. issue_push_heads says it instead. It holds every
-- commit the issue's recorded pushes replaced or left, so a push that leaves the branch on a
-- commit it holds — one the record holds already, one behind it, or one setting the branch back
-- on a commit it held — moves neither column; a push that leaves the branch on a commit it does
-- not hold moves both, and pushed_at only ever later (store.Store.RecordIssuePush, which says
-- what the record cannot know). The all-zero sha GitHub names as a new branch's before and a
-- deleted branch's after is no commit and is never held, and a deletion moves neither column.
-- seen_at is when the daemon first saw the commit, and each issue keeps the 256 it saw last, more
-- than the commits of the hundred newest entries of the branch's activity, which is all the
-- per-issue read rebuilds pushes from.
--
-- An issue the daemon has recorded no push for has no row, and nothing of it is refused by this
-- rule, because nothing newer than its copies is known to be on GitHub. That is also what a
-- deployment meets the first time it runs this release: its pushes before the upgrade were never
-- recorded, so the running copies its earlier daemon saved are refused only once a push is
-- recorded after them. A row and its commits outlive the issue's close, since the pushes they
-- record happened and stay true; they are one small row and at most 256 commits per issue. For the
-- same reason a rollback to an earlier daemon deletes the rows of workspace_running_copies
-- (docs/kubernetes.md, "Rolling back the running copy"): that daemon records no push while it
-- runs, so after a roll forward the copies taken before the rollback would be older than pushes
-- this table never saw, and nothing here could refuse them.
create table issue_pushes (
    issue     text        primary key,
    pushed_at timestamptz not null,
    head      text        not null
);

create table issue_push_heads (
    issue   text        not null,
    head    text        not null,
    seen_at timestamptz not null,
    primary key (issue, head)
);

-- The outcomes an attempt can now record beside `sent` and `refused_older`. `refused_pushed` is
-- the rule above: a running copy is older than the issue's newest recorded push, so it is sent to
-- no pod and its bytes stay for an operator. It is a running copy's alone, and the release bundle's
-- check does not admit it: a release bundle is taken after the roles stop, so no later push holds
-- its work, and a refused bundle would hold the issue's pod from every later idle release
-- (workflow.IdleHoldBundleUnrestored). `superseded` is the older of the issue's two copies when
-- the newer one is offered to the pod: the pods after it hold that work's descendants, so the
-- older copy is closed unsent. Both spend the copy's one attempt, as `refused_older` does. A
-- daemon of an earlier release reads neither, since it never reads the outcome of a row whose
-- attempt is spent.
alter table workspace_bundles
  drop constraint workspace_bundles_attempt_outcome,
  add constraint workspace_bundles_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'superseded') end);

alter table workspace_running_copies
  drop constraint workspace_running_copies_attempt_outcome,
  add constraint workspace_running_copies_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'refused_pushed', 'superseded') end);
