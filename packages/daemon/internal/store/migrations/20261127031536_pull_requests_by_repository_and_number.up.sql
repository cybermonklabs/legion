-- 20261127031536_pull_requests_by_repository_and_number.up.sql — each pull request of an issue
-- keyed by its repository and its number (dispatch://INFRA-5910): pull_requests and what each pull
-- request remembers of its settled heads keyed by (repo, number), the review decision of each pull
-- request, the issue's links to pull requests (pull_request_links), and the outbox kind that reads
-- a linked pull request (pull_request_link_read).
--
-- 20261122122826 keyed pull_requests by the issue and the branch, so two pull requests on one
-- branch of an issue shared one row, and the second replaced the first: theorem-labs/agent-infra
-- #3654 onto main and the draft #3955 onto legion/INFRA-1276-ci-base stand on one branch, and the
-- draft took #3654's row. GitHub names a pull request by its repository and its number, so that is
-- the key here, and the issue is a column of the row: a pull request belongs to one issue. Every
-- read of a repository name in the record compares it without case (record's PullRequestByNumber),
-- so a unique index on (lower(repo), number) stands beside the key, in place of the index of that
-- name that 20261102095707 made, and two spellings of one repository cannot make two rows. The
-- rows that collide without case are merged first: of each set the row in the spelling of its
-- issue's github_repo stays, then the newest (head_updated_at), then the first by issue and
-- branch, and the others go, each with what it remembers of its settled heads (the foreign key
-- 20261122122826 wrote cascades the delete). Two rows under the two names of a configured rename
-- are not this migration's to find, since the renames are the daemon's configuration and no table:
-- the rename pass merges them by (lower(repo), number) (record's RenameIssueRepository). 0004's
-- unique pull_requests_repo_branch goes, since one branch of a repository can hold several pull
-- requests; 20261102095707's pull_requests_lower_repo_branch, which is not unique, stays for the
-- reads that have only a branch (record's OpenPullRequestsOnBranch). The partial index of the open
-- rows by repository and base (pull_requests_open_on_base, 20261029120300) is made again on
-- lower(repo), the expression record's OpenPullRequestsOnBase compares.
--
-- pull_request_settled_heads keeps what a pull request remembers of its settled heads with a
-- foreign key on pull_requests (issue, branch), which the key (issue, branch) serves, so the
-- foreign key goes first: PostgreSQL refuses to drop a key another table's foreign key depends on
-- (SQLSTATE 2BP01). The table takes the repository and the number of its pull request, filled from
-- pull_requests by the issue and the branch, which every row of it has (the foreign key held it
-- so), drops the issue and the branch, and is keyed by (repo, number, kind, head) with its foreign
-- key on pull_requests (repo, number), on delete cascade as before, and on update cascade, since the
-- rename pass writes a new repo on a pull request (record's RenameIssueRepository) and its memory
-- follows it. The record reads and writes the memory by the repository and the number (record's
-- loadSettledHeads, putSettledHeads).
--
-- pull_requests.review_decision is the newest deciding review of the review round for that pull
-- request, at its head, in the shape of the reviewer's decision (record.ReviewDecision), null while
-- none decided: a round with several open pull requests judges each of them. A new generation
-- clears it (record's clearGeneration).
--
-- pull_request_links is the daemon's copy of the links of each issue that Dispatch holds and that
-- name a pull request (https://github.com/<owner>/<name>/pull/<number>), one row for each link,
-- keyed by the pull request's repository and number, with a unique index on (lower(repo), number)
-- beside the key, since Dispatch lets one URL link one issue alone. refused is why the daemon took
-- the link into no set ('' for a link it took): its head is in a fork ('fork'), the record holds
-- the pull request for another issue ('other-issue'), its repository fails the checks the daemon
-- makes of a recorded repository ('unserved'), or GitHub answered 404 for it ('unreadable').
-- told_at is when a notice to the issue's architect named the link, null while none did. The rows
-- go with their issue (on delete cascade), and a new generation keeps them and clears told_at:
-- the links are Dispatch's and outlive a run. The record reads whether a pull request is linked at
-- each read and stores it on no pull_requests row (record.PullRequest.Linked).
--
-- The outbox check is one list, so this migration names every kind of 20261126144204, its
-- thirty-six, in its order, and pull_request_link_read, thirty-seven in all. Its version stands
-- above 20261126144204 and main's 20261126110259, so on any database this list is the last word:
-- Migrate applies every unrecorded migration in version order, and a list of a lower version would
-- be applied after this one on a database that had not recorded it and refuse the new kind.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, before the way back
-- of any earlier migration. The release before decodes an outbox row strictly and knows neither the
-- kind pull_request_link_read, nor the notice kinds pr-closed-others-open, pr-holds-close and
-- pr-link-refused, nor the key repo on the payloads that name a pull request by its number; it
-- reads no hold reason pull-requests; and it writes pull_requests with `on conflict (issue,
-- branch)`, which needs the key (issue, branch). So the block deletes each row of the new kind,
-- each unsent notice of the three kinds and each row whose payload names a repo, and writes the
-- kind check of 20261126144204 again; ends the reason of each hold for pull-requests, which leaves
-- the issue held from merging with no reason; drops pull_request_links; keeps, of each issue, the
-- rows on the branch of a task of its plan at its generation, in the issue's own repository (on
-- legion/<KEY> where the issue has no plan rows there, as 20261122122826's way back reads it), and
-- of two rows on one branch the newer (the higher head_updated_at, then the higher number), and
-- deletes the others, which takes the settled heads of each with them; keys
-- pull_request_settled_heads by (issue, branch, kind, head) again, filled from pull_requests by the
-- repository and the number, with the foreign key 20261122122826 wrote; keys pull_requests by
-- (issue, branch) again with 0004's unique pull_requests_repo_branch, 20261102095707's index of the
-- repository and the number and 20261029120300's index of the open rows; drops review_decision;
-- and deletes its version row, which is owed to the roll-forward, so that it runs this file again.
-- What the way back costs is the row of each linked pull request and of each other pull request on
-- a task's branch, and what each remembered of its settled heads, each review decision, each link
-- and each hold for pull-requests: the earlier release reads none of them, and the roll-forward's
-- daemon reads each linked pull request again from the issue's links.
--
-- begin;
-- delete from outbox where kind = 'pull_request_link_read';
-- delete from outbox where kind = 'notice' and payload->>'kind' in ('pr-closed-others-open', 'pr-holds-close', 'pr-link-refused');
-- delete from outbox where kind in ('merge_queue_read', 'merge_queue_removal', 'conflict_confirm', 'merge_forward',
--   'task_pull_requests_close') and payload ? 'repo';
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in (
--   'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
--   'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
--   'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
--   'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
--   'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
--   'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
--   'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove'));
-- update issues set hold_reason = null where hold_reason = 'pull-requests';
-- drop table pull_request_links;
-- delete from pull_requests pr where not exists (select 1 from issues i where i.key = pr.issue
--   and lower(pr.repo) in (lower(i.repo), lower(i.github_repo))
--   and pr.branch in (select coalesce(nullif(t.branch, ''), 'legion/' || t.issue) from plan_tasks t
--     where t.issue = i.key and t.generation = i.generation
--     union all select 'legion/' || i.key
--     where not exists (select 1 from plan_tasks t where t.issue = i.key and t.generation = i.generation)));
-- delete from pull_requests pr using (select repo, number, row_number() over (partition by issue, branch
--   order by head_updated_at desc, number desc) as rank from pull_requests) ranked
--   where ranked.repo = pr.repo and ranked.number = pr.number and ranked.rank > 1;
-- alter table pull_request_settled_heads drop constraint pull_request_settled_heads_pull_request_fkey;
-- alter table pull_request_settled_heads drop constraint pull_request_settled_heads_pkey;
-- alter table pull_request_settled_heads add column issue text, add column branch text;
-- update pull_request_settled_heads heads set issue = pr.issue, branch = pr.branch from pull_requests pr
--   where pr.repo = heads.repo and pr.number = heads.number;
-- alter table pull_request_settled_heads alter column issue set not null, alter column branch set not null;
-- alter table pull_request_settled_heads drop column repo, drop column number;
-- drop index pull_requests_lower_repo_number;
-- drop index pull_requests_open_on_base;
-- alter table pull_requests drop constraint pull_requests_pkey;
-- alter table pull_requests add constraint pull_requests_pkey primary key (issue, branch);
-- alter table pull_requests drop column review_decision;
-- create unique index pull_requests_repo_branch on pull_requests (repo, branch);
-- create index pull_requests_lower_repo_number on pull_requests (lower(repo), number);
-- create index pull_requests_open_on_base on pull_requests (repo, base) where state = 'open';
-- alter table pull_request_settled_heads add constraint pull_request_settled_heads_pkey primary key (issue, branch, kind, head),
--   add constraint pull_request_settled_heads_issue_fkey foreign key (issue, branch)
--     references pull_requests (issue, branch) on delete cascade;
-- delete from schema_version where version = 20261127031536;
-- commit;
delete from pull_requests pr using (
  select p.issue, p.branch, row_number() over (partition by lower(p.repo), p.number
    order by coalesce(p.repo = i.github_repo, false) desc, p.head_updated_at desc, p.issue, p.branch) as rank
  from pull_requests p left join issues i on i.key = p.issue) ranked
  where ranked.issue = pr.issue and ranked.branch = pr.branch and ranked.rank > 1;

alter table pull_request_settled_heads drop constraint pull_request_settled_heads_issue_fkey;
alter table pull_request_settled_heads add column repo text, add column number integer;
update pull_request_settled_heads heads set repo = pr.repo, number = pr.number
  from pull_requests pr where pr.issue = heads.issue and pr.branch = heads.branch;
alter table pull_request_settled_heads alter column repo set not null, alter column number set not null;
alter table pull_request_settled_heads drop constraint pull_request_settled_heads_pkey;
alter table pull_request_settled_heads drop column issue, drop column branch;
alter table pull_request_settled_heads add constraint pull_request_settled_heads_pkey primary key (repo, number, kind, head);

alter table pull_requests drop constraint pull_requests_pkey;
alter table pull_requests add constraint pull_requests_pkey primary key (repo, number);
drop index pull_requests_repo_branch;
drop index pull_requests_lower_repo_number;
create unique index pull_requests_lower_repo_number on pull_requests (lower(repo), number);
drop index pull_requests_open_on_base;
create index pull_requests_open_on_base on pull_requests (lower(repo), base) where state = 'open';
alter table pull_requests add column review_decision jsonb;

alter table pull_request_settled_heads add constraint pull_request_settled_heads_pull_request_fkey
  foreign key (repo, number) references pull_requests (repo, number) on delete cascade on update cascade;

create table pull_request_links (
  issue text not null references issues (key) on delete cascade,
  repo text not null,
  number integer not null check (number > 0),
  refused text not null default '' check (refused in ('', 'fork', 'other-issue', 'unserved', 'unreadable')),
  told_at timestamptz,
  primary key (repo, number)
);
create unique index pull_request_links_lower_repo_number on pull_request_links (lower(repo), number);
create index pull_request_links_issue on pull_request_links (issue);

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove',
  'pull_request_link_read'));
