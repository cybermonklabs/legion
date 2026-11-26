-- 20261126090000_eval_source_owners.up.sql
--
-- The reader of a source key (dispatch://INFRA-978, R12; evals.sourceKey). A deploy run that ships
-- no pull request counts for the delivery-on project whose deploy source reads its repository, and
-- for no other, through that repository. Its day rows, members and owed days are written once, under
-- the owner key ':' || repo || '||>~' of its repository (eval_day_owners: repo, no sharer, the lists
-- '{>~}'), whichever project's source reads the repository, and the reader is chosen at read time:
-- the project eval_days_rules records for the repository (20261123020640), which the refresher
-- writes in the commit that brings the repository's rows to its source (evals.recordRules), and
-- null once no source reads it. So a source moved to another repository and back, or a
-- repository's source moved to another project, moves which project reads the key and rewrites no
-- day row.
--
-- eval_project_owners (20261117100006) is re-stated with one more arm: a key whose lists are
-- '{>~}' is read by the project eval_days_rules records for its repository, compared without case
-- as every arm compares a repository, through the repository (via true), and by no other project.
-- The arm of a key that carries lists leaves such a key out. It would give it no reader in any case:
-- eval_owner_readers picks the list's terminal '~', which no row of projects holds
-- (projects_key_check), so the view as 20261117100006 states it, the release before's, gives a
-- source key no reader and logs no change of one. The release before writes no such key, and once
-- this migration has run it reads the view as re-stated here, as this release does. Every read and
-- the change trigger's lookup of an owner key's readers (eval_days_change_log, 20261117100014) go
-- through the view, so each follows the arm with no change of its own.
create or replace view eval_project_owners as
    select key as project_key, key as owner, false as via from projects
    union all
    select r.project_key, o.owner, true
    from eval_project_repos r join eval_day_owners o on lower(o.repo) = lower(r.repo)
    where o.lists = '{}' and not (r.project_key = any(o.sharers))
    union all
    select reader.project_key, o.owner, reader.via
    from eval_day_owners o cross join lateral eval_owner_readers(o.repo, o.sharers, o.lists) as reader
    where o.lists <> '{}' and o.lists <> '{>~}'
    union all
    select rule.project_key, o.owner, true
    from eval_day_owners o
    join eval_days_rules rule on lower(rule.repo) = lower(o.repo)
    join projects p on p.key = rule.project_key
    where o.lists = '{>~}';
