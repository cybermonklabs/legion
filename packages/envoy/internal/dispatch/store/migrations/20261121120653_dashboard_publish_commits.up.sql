-- 20261121120653_dashboard_publish_commits.up.sql
-- When each build last published its dashboard, by the commit of the build that published it, for
-- the embeddings partition swap's fleet read (store.ErrEmbeddingsSwapWaits).
--
-- dashboard_builds (migration 0136) is keyed by the web build, the hash of the dist's index.html
-- (routes.DashboardBuild), and a publish of a build already stored only renews its last_started_at
-- (dashboard.Publish: `on conflict (build) do update set last_started_at = now()`). So its commit is
-- the first publisher's, not the latest one's: when two releases ship one web dist, every boot of
-- the later release renews a row naming the earlier commit, and a boot of the earlier one renews a
-- row naming the later. Read for whose boot it was, the stored row names the wrong build either way.
--
-- dashboard_publishes holds one row a commit: the last time a build of that commit published its
-- dashboard. dashboard_publish_commit, BEFORE INSERT on dashboard_builds, writes it from the
-- inserting row, NEW.commit and the database's now(). An `insert ... on conflict ... do update`
-- fires BEFORE INSERT whether or not the row exists, as the owner lock's arrival trigger rests on
-- too (migration 20261115200014), so every publish of every build is recorded under its own
-- commit, a renewal included. A row only moves forward: two publishes of one commit may commit in
-- either order. One row a release, so the table stays as small as the list of releases; nothing
-- prunes it.
--
-- The rows of dashboard_builds already there were last renewed by some build's boot, whose commit
-- the table cannot say. Each row within the hour is copied in under the commit it names, so a build
-- that booted in that hour is waited for under some commit other than this release's for at most
-- that hour and once; a row naming this release's own commit is not, since nothing of this release
-- has published before its own migration runs (cmd/dispatch's main migrates before it publishes).
--
-- The release before this one names its columns in every insert it makes on dashboard_builds and
-- reads nothing of this table, so a rolling update or a rollback leaves it untouched: its
-- publishes fire the trigger and record their own commit, which is what the swap reads them for
-- (cmd/dispatch/README.md, "Database migrations", "Two builds at once"). Every name is written with
-- its schema, and every function the trigger calls with pg_catalog, as 20261115200014's are.
--
-- Locks: the CREATE TRIGGER takes dashboard_builds SHARE ROW EXCLUSIVE until the commit; the table
-- takes a write only at a process's boot, so a publish waits for an instant.
create table public.dashboard_publishes (
  commit text primary key,
  published_at timestamptz not null
);

insert into public.dashboard_publishes (commit, published_at)
select commit, pg_catalog.max(last_started_at) from public.dashboard_builds
where last_started_at > pg_catalog.now() - interval '1 hour'
group by commit;

create function public.dashboard_publish_commit() returns trigger language plpgsql as $$
begin
  insert into public.dashboard_publishes (commit, published_at) values (new.commit, pg_catalog.now())
  on conflict (commit) do update
    set published_at = greatest(public.dashboard_publishes.published_at, excluded.published_at);
  return new;
end $$;

create trigger dashboard_builds_publish_commit before insert on public.dashboard_builds
  for each row execute function public.dashboard_publish_commit();
