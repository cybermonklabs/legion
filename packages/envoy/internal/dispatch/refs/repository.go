package refs

import "github.com/sjawhar/envoy/internal/dispatch/access"

// ProjectRepositoriesSQL is the server's one rule of which GitHub repositories each project owns,
// as a query of the rows (project text, repo text): a project owns every repository an issue of it
// resolves to by the server's one rule (IssueRepoTerms, IssueRepoJoin, IssueRepoResolved: the
// issue's own repository, else its nearest parent's, else its project's default mapping), whether
// the issue is open or done, and every repository it maps (repo_projects). An issue's own
// repository may name any owner/name, so an issue of one project can make another project's
// repository its own as well: ownership is then shared, never moved. An issue whose chain the walk
// cannot finish resolves to its project's default, as the terms answer it. Every rule that hides a
// repository, or relays its events, by the projects that own it reads this query
// (RepositoryClause), so no two of them can come to different owners.
//
// repo is the repository's canonical name (CanonicalRepoSQL), lower-cased: a repository is one
// repository under each of its names, as GitHub spells an owner and a name as their owners do, so
// a caller compares it with lower(CanonicalRepoSQL(name)) of the name it holds. A project that owns
// a repository several times over (two issues, an issue and a mapping) answers a row for each.
//
// projects narrows the rows to the projects it passes: a SQL boolean over the bare column
// project_key, the project a row would answer, true or false and never null, which is spliced
// twice ("true" answers every project). Only the issues of the projects it passes are walked, so a
// narrow filter costs a narrow walk. It may name the embedding statement's placeholders, and any
// column it names other than project_key must be qualified by an alias of the embedding statement.
//
// The text is a whole query that begins `with recursive`, so a statement embeds it as a
// parenthesized subquery, as this one answers the projects that own each repository of an array
// bound at $1:
//
//	select distinct asked.repo, owned.project
//	from unnest($1::text[]) as asked(repo)
//	join (<ProjectRepositoriesSQL("true")>) owned on owned.repo = lower(<CanonicalRepoSQL("asked.repo")>)
//
// It reads every project's issues and mappings, whoever runs it: it answers ownership, not what a
// viewer sees, which RepositoryClause answers from it.
func ProjectRepositoriesSQL(projects string) string {
	return `with recursive subjects as (
			select i.key from issues i where ` + projects + `
		), ` + IssueRepoTerms + `
		select i.project_key as project, lower(` + CanonicalRepoSQL(IssueRepoResolved) + `) as repo
		from subjects s join issues i on i.key = s.key ` + IssueRepoJoin + `
		where ` + IssueRepoResolved + ` is not null
		union all
		select mapping.project_key, lower(` + CanonicalRepoSQL("mapping.repo") + `)
		from (select mapped.project as project_key, mapped.repo from repo_projects mapped) mapping
		where ` + projects
}

// RepositoryClause passes the GitHub repository repoExpr names, as owner/name, to the viewer bound
// at viewerParam (VisibilityArg) when no private project the viewer does not see owns it
// (ProjectRepositoriesSQL), and, for a viewer that is not open, when also a project the viewer sees
// owns it. So an issue's own repository, which may name any owner/name, never shows a repository a
// private project hidden from the viewer owns; a repository a project the viewer sees shares with a
// public project the viewer does not see passes, since that project hides nothing of the
// repository from anyone; and a repository no project owns passes for an open viewer alone. The
// server's own viewer passes every repository, and so does an open viewer of a deployment with no
// private project, which reads no issue and no mapping for it (R9).
//
// For an open viewer only the projects it does not see are read, since only a private one of them
// can hide a repository from it; for any other viewer every project is. It is in refs rather than
// access because refs imports access. The GitHub proxy (routes), the App-installation route and
// delivery's pull requests that name no issue and runs that name none (api, delivery) each hold a
// repository to it.
//
// repoExpr is spliced once, and must not be a bare column named repo or project; the clause answers
// true or false, never null.
func RepositoryClause(repoExpr, viewerParam string) string {
	open := access.ViewerFlag(viewerParam, "open")
	seen := access.ProjectClause("owned.project", viewerParam)
	read := `(not ` + open + ` or not ` + access.ProjectClause("project_key", viewerParam) + `)`
	return `(` + access.ViewerFlag(viewerParam, "all") + ` or (` + open + ` and not ` + access.ViewerFlag(viewerParam, "private") + `) or coalesce((
		select not coalesce(bool_or(not ` + seen + ` and owner.visibility = 'private'), false) and (` + open + ` or coalesce(bool_or(` + seen + `), false))
		from (` + ProjectRepositoriesSQL(read) + `) owned join projects owner on owner.key = owned.project
		where owned.repo = (select lower(` + CanonicalRepoSQL("named.repo") + `) from (select (` + repoExpr + `)::text as repo) named)
	), false))`
}
