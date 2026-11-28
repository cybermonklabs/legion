-- 20261120171308_pull_request_reads_owed_link_backfill.up.sql
--
-- A link by hand owes a read of the pull request it links where Dispatch holds no head row of it
-- (R5 of the results lookup, delivery.OweLinkedPullRequestReads), but only from the link write on.
-- A link written before this release owes nothing, so a pull request that has no event since, a
-- merged one for instance, never gets its head row, and the checks of the issue that links it
-- read no head. This owes the read once for each link that is there now, as a link write of this
-- release owes it for the same links, and the reconcile's next passes read them (failures first,
-- at most 200 a pass) and write each head GitHub answers (Reconcile.readOwedPullRequests). Its
-- test runs that link write over the same links on a second database and compares the two
-- tables row for row (delivery.TestTheLinkBackfillOwesWhatALinkWriteOwes).
--
-- The rule is the link write's (delivery.linkedPullRequest, checkPullRequestKey, owedName,
-- oweLinkedRead), spelled out here because a migration is frozen text:
--   * A link names a pull request where it is https://github.com/<owner>/<name>/pull/<number> or
--     .../issues/<number>, its scheme and host in any letter case and `pull` or `issues` in any
--     case, every character of it ASCII, the owner a letter or digit and then up to 38 letters,
--     digits, `-` and `_` (refs.GitHubOwnerPattern), the name 1 to 100 letters, digits, `.`, `-`
--     and `_`, not `.` or `..`, and not empty, `.` or `..` once it is lowercased and one trailing
--     `.git` is trimmed (refs.SplitGitHubRepo), and the number 1 to 2147483647 with no zero before
--     it. An /issues/ link that names an issue of GitHub owes one read too, which GitHub answers
--     as no pull request and the reconcile settles (pullRequestAbsent), as for a link write.
--   * The read is owed under the repository's name as the link and the stored head rows spell it,
--     compared in any letter case. The migration reads no rename pair: no statement of this
--     release names repo_projects, which a later change drops, and a migration that named a
--     dropped table would fail on a database whose drop came first. The boot's rename pass, which
--     runs after the migrations, rewrites each old name of a pair in the links, the head rows and
--     the owed rows alike (store.RewriteRenamedRepositories), so an owe written here under an old
--     name is owed under the new one before the process serves. Where the pass had already
--     rewritten the links and the head rows at an earlier boot, every name this reads is its
--     canonical one, and this owes what a link write of this release owes. Where this boot is the
--     first with the pass, a link and its head row stored under two names of one pair (the link
--     under the old name and the row under the new one, or the other way round) owe one read,
--     which the reconcile makes and which writes the head GitHub answers.
--   * A link owes nothing where a head row of its pull request is stored, under the link's name
--     in any letter case, and nothing where a read of it is owed already in any letter case: such
--     a row is left as it is, where a link write gives it a new revision and time. No read of
--     this release is in flight while the migration runs, and the way back below must leave such
--     a row.
--   * Two links of one pull request, in two letter cases, owe one read, under the owed name first
--     in byte order, as a link write of both owes it.
--
-- Each link is parsed once (the materialized terms), and the stored heads are read once and joined
-- by hash, so the cost grows with the links and the head rows, and not with their product. Its
-- test measures it.
--
-- Each new row is a link's owe, as recordLinkedOwe writes it: a failure whose linked_revision is
-- its revision, a new value of pull_request_reads_owed_revision_seq, so a head row another writer
-- stores before the read settles hides nothing (delivery.owedReadJoin). Its owed_at is the time of
-- this migration's transaction, which is also the applied_at the runner records for it in
-- schema_migrations in the same transaction: the by-hand way back
-- (20261120171308_pull_request_reads_owed_link_backfill.down.sql) deletes the rows whose owed_at
-- is still that time, which every later owe moves.
--
-- The census answers 0: it inserts rows and rewrites and refuses none. The release before names
-- pull_request_reads_owed in none of its statements, so rolling back past this migration is an
-- image rollback alone; the owed rows wait for the next release that reads them.
insert into pull_request_reads_owed (repo, number, reason, revision, linked_revision)
select owed.repo, owed.number, 'failure', owed.revision, owed.revision
from (
	with parsed as materialized (
		select p.m[1] || '/' || p.m[2] as given, p.m[4] as digits
		from (
			select regexp_match(lower(left(l.url, 19)) || substr(l.url, 20),
				'^https://github\.com/([A-Za-z0-9][A-Za-z0-9_-]{0,38})/([A-Za-z0-9._-]{1,100})/([A-Za-z]+)/([1-9][0-9]{0,9})$') as m
			from issue_external_links l
			where octet_length(l.url) = length(l.url)
		) p
		where lower(p.m[3]) in ('pull', 'issues') and p.m[2] not in ('.', '..')
			and regexp_replace(lower(p.m[2]), '\.git$', '') not in ('', '.', '..')
	), linked as materialized (
		select p.given, case when p.digits::bigint <= 2147483647 then p.digits::int end as number
		from parsed p
		where p.digits::bigint <= 2147483647
	), named as materialized (
		select l.given as repo, l.number
		from linked l
	), held as materialized (
		select distinct lower(h.repo) as repo, h.number
		from pull_request_heads h
	), picked as (
		select distinct on (lower(n.repo), n.number) n.repo, n.number
		from named n
		where not exists (select 1 from held h where h.repo = lower(n.repo) and h.number = n.number)
			and not exists (select 1 from pull_request_reads_owed o where lower(o.repo) = lower(n.repo) and o.number = n.number)
		order by lower(n.repo), n.number, n.repo collate "C"
	)
	select p.repo, p.number, nextval('pull_request_reads_owed_revision_seq') as revision from picked p
) owed
on conflict (repo, number) do nothing;
