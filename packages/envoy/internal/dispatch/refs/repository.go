package refs

import "github.com/sjawhar/envoy/internal/dispatch/access"

// RepositoryClause passes the GitHub repository repoExpr names, as owner/name, to the viewer bound
// at viewerParam (VisibilityArg) by the issues that resolve to it: when an issue the viewer sees
// resolves to it, or when the viewer is open and no issue of a project it does not see resolves to
// it. Every other repository is hidden, so a repository no issue resolves to passes for an open
// viewer alone. The server's own viewer passes every repository.
//
// An issue resolves to a repository by the server's one rule (IssueRepoTerms, IssueRepoJoin,
// IssueRepoResolved), with every issue as its subjects: its own repository, else its nearest
// parent's, else its project's default. An issue whose chain the walk cannot finish resolves to
// its project's default, as the terms answer it, so it hides that repository from a viewer who
// does not see the project rather than shows it. Every issue counts, open or done, since a
// repository stays a private project's repository when its issues close.
//
// Names are compared lower-cased on both sides, as GitHub spells an owner and a name as their
// owners do. No rename is read (CanonicalRepoSQL reads the mappings' renamed_to): the rule is the
// issues' own repositories alone. It is in refs rather than access because refs imports access.
//
// repoExpr is spliced once; the clause answers true or false, never null.
func RepositoryClause(repoExpr, viewerParam string) string {
	seen := access.ProjectClause("i.project_key", viewerParam)
	open := access.ViewerFlag(viewerParam, "open")
	return `(` + access.ViewerFlag(viewerParam, "all") + ` or coalesce((
		with recursive subjects as (select key from issues), ` + IssueRepoTerms + `
		select bool_or(` + seen + `) or (` + open + ` and bool_and(` + seen + `))
		from issues i ` + IssueRepoJoin + `
		where lower(` + IssueRepoResolved + `) = lower(` + repoExpr + `)
	), ` + open + `))`
}
