package refs

import "github.com/sjawhar/envoy/internal/dispatch/access"

// ProjectRepositoriesSQL is the server's one rule of which GitHub repositories each project owns,
// as a query of the rows (project text, repo text): a project owns every repository an issue of it
// resolves to by the server's one rule (IssueRepoTerms, IssueRepoJoin, IssueRepoResolved: the
// issue's own repository, else its nearest parent's, else its project's default), whether the
// issue is open or done. Issues are a repository's only owners: a repository mapping gives no
// project a repository, and no recorded rename is read. An issue's own repository may name any
// owner/name, so an issue of one project can make another project's repository its own as well:
// ownership is then shared, never moved. An issue whose chain the walk cannot finish resolves to
// its project's default, as the terms answer it. Every rule that hides a repository by the projects
// that own it reads this query (RepositoryClause), so no two of them can come to different owners.
//
// repo is the resolved name lower-cased, as GitHub spells an owner and a name as their owners do,
// so a caller compares it with lower() of the name it holds. A project that owns a repository
// several times over (two issues) answers a row for each.
//
// projects narrows the rows to the projects it passes: a SQL boolean over the bare column
// project_key, the project a row would answer, true or false and never null. Only the issues of
// the projects it passes are walked, so a narrow filter costs a narrow walk. It may name the
// embedding statement's placeholders, and any column it names other than project_key must be
// qualified by an alias of the embedding statement.
//
// The text is a whole query that begins `with recursive`, so a statement embeds it as a
// parenthesized subquery, as this one answers the projects that own each repository of an array
// bound at $1:
//
//	select distinct asked.repo, owned.project
//	from unnest($1::text[]) as asked(repo)
//	join (<ProjectRepositoriesSQL("true")>) owned on owned.repo = lower(asked.repo)
//
// It reads every project's issues, whoever runs it: it answers ownership, not what a viewer sees,
// which RepositoryClause answers from it.
func ProjectRepositoriesSQL(projects string) string {
	return `with recursive subjects as (
			select i.key from issues i where ` + projects + `
		), ` + IssueRepoTerms + `
		select i.project_key as project, lower(` + IssueRepoResolved + `) as repo
		from subjects s join issues i on i.key = s.key ` + IssueRepoJoin + `
		where ` + IssueRepoResolved + ` is not null`
}

// RepositoryClause passes the GitHub repository repoExpr names, as owner/name, to the viewer bound
// at viewerParam (VisibilityArg). A repository passes in two cases, and fails in every other:
//
//   - an issue of a project the viewer sees resolves to it (ProjectRepositoriesSQL), whatever other
//     project, private ones included, owns it too: where issues of a private project and of a
//     public project work in one repository, the public project's people read it whole;
//   - the viewer is open, and no private project the viewer does not see owns it.
//
// So a viewer who is not open, a restricted member or a credential that lists projects, reads
// only a repository an issue of a project it sees resolves to. A repository is held by its name
// alone: once GitHub renames a private project's repository or moves it to another owner, an open
// viewer reads it under the new name until an issue of that project resolves to the new name.
//
// The server's own viewer passes every repository, and so does an open viewer of a deployment with
// no private project, which reads no issue for it (R9). For an open viewer the private projects it
// does not see are read first, and those it sees only where one of the others owns the repository;
// for any other viewer only the projects it sees are read. It is in refs rather than access because
// refs imports access. The GitHub proxy (routes) and delivery's pull requests that name no issue
// and runs that name none (api, delivery) each hold a repository to it.
//
// repoExpr is spliced more than once, so it must be a column, a bound parameter or another
// side-effect-free expression, not a bare column named repo or project, nor name the alias owned;
// the clause answers true or false, never null.
func RepositoryClause(repoExpr, viewerParam string) string {
	open := access.ViewerFlag(viewerParam, "open")
	return `(` + access.ViewerFlag(viewerParam, "all") + ` or (` + open + ` and not ` + access.ViewerFlag(viewerParam, "private") + `) or case
		when ` + open + ` and not ` + repositoryHeld(repoExpr, viewerParam) + ` then true
		else ` + repositorySeen(repoExpr, viewerParam) + `
	end)`
}

// repositorySeen is whether an issue of a project the viewer sees resolves to the repository
// repoExpr names. It sits under RepositoryClause's CASE, so it ends in `offset 0`, as every
// `exists` a clause adds there does (#3178 at 0f8b389c).
func repositorySeen(repoExpr, viewerParam string) string {
	return `exists (select from (` + ProjectRepositoriesSQL(access.ProjectClause("project_key", viewerParam)) + `) owned
		where owned.repo = lower((` + repoExpr + `)::text)
		offset 0)`
}

// repositoryHeld is whether a private project the viewer does not see owns the repository repoExpr
// names: an issue of it resolves to the name. It ends in `offset 0`, as repositorySeen does.
func repositoryHeld(repoExpr, viewerParam string) string {
	return `exists (select from (` + ProjectRepositoriesSQL(access.HiddenPrivateProjectClause("project_key", viewerParam)) + `) owned
		where owned.repo = lower((` + repoExpr + `)::text)
		offset 0)`
}
