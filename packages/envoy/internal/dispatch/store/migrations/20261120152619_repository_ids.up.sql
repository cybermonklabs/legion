-- 20261120152619_repository_ids.up.sql — the GitHub id of each repository name Dispatch has asked
-- GitHub about.
--
-- A repository keeps its id when GitHub renames it or moves it to another owner, and its name does
-- not. An issue names a repository by owner/name, so a private project's repository read under a
-- new name would otherwise belong to no project. Dispatch records here, for each name a private
-- project's issue resolves to, the id of the repository the App reads under that name
-- (refs.LearnRepositoryIDs), and for each pull request the delivery intake fetches, its
-- repository's id (delivery's completePullRequest). The GitHub proxy and Delivery read a
-- repository as hidden when its id is the id of a name a private project hidden from the reader
-- owns (refs.RepositoryClause).
--
-- repo is the name lower-cased, as every comparison of a repository is made. github_id is null
-- where GitHub answered that the App reads no repository under the name (its installation lookup's
-- 404, or the repository's); such a name is asked again an hour after its answer. learned_at is
-- when GitHub last answered for the name. No route serves the table.
--
-- The table is new, so the migration touches no existing row. Its identifier is the UTC moment it
-- was written, as `migrationid` requires of every migration past the numbered series, which closed
-- at 0161.
create table repository_ids (
  repo text primary key check (repo = lower(repo)),
  github_id bigint,
  learned_at timestamptz not null default now()
);

create index repository_ids_github_id on repository_ids (github_id) where github_id is not null;
