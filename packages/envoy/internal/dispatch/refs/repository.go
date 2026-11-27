package refs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// ProjectRepositoriesSQL is the server's one rule of which GitHub repositories each project owns,
// as a query of the rows (project text, issue text, number bigint, repo text), one for each issue
// of the projects it passes that resolves to a repository: a project owns every repository an
// issue of it resolves to by the server's one rule (IssueRepoTerms, IssueRepoJoin,
// IssueRepoResolved: the issue's own repository, else its nearest parent's, else its project's
// default), whether the issue is open or done. Issues are a repository's only owners: a repository
// mapping gives no project a repository, and no recorded rename is read. An issue's own repository
// may name any owner/name, so an issue of one project can make another project's repository its
// own as well: ownership is then shared, never moved. Every rule that hides a repository by the
// projects that own it reads this query (RepositoryClause), and so does ProjectRepositories, so no
// two of them can come to different owners.
//
// An issue whose chain the walk cannot finish (IssueRepoTooDeep: deeper than IssueDepthCap links,
// or a loop) answers a row whose repo is null: it resolves to no repository, so it owns none and
// hides none, and RepositoryClause, comparing null with a name, never matches it. Only
// ProjectRepositories refuses its project (*UnresolvedChainError), so a list of a project's
// repositories never leaves out one its unread ancestors may name.
//
// repo is the resolved name lower-cased, as GitHub spells an owner and a name as their owners do,
// so a caller compares it with lower() of the name it holds. A project that owns a repository
// several times over (two issues) answers a row for each.
//
// projects narrows the rows to the projects it passes: a SQL boolean over the bare column
// project_key, the project a row would answer, true or false and never null. Only the issues of
// the projects it passes are walked, so a narrow filter costs a narrow walk. It may name the
// embedding statement's placeholders and the bare column key, the issue's own; any other column
// it names must be qualified by an alias of the embedding statement.
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
		select i.project_key as project, i.key as issue, i.number as number,
			case when ` + IssueRepoTooDeep + ` then null else lower(` + IssueRepoResolved + `) end as repo
		from subjects s join issues i on i.key = s.key ` + IssueRepoJoin + `
		where ` + IssueRepoResolved + ` is not null or ` + IssueRepoTooDeep
}

// UnresolvedChainError is ProjectRepositories' refusal of a project one of whose issues has a
// parent chain the walk could not finish, deeper than IssueDepthCap links or a loop: Issue is the
// first such issue by number. Its unread ancestors may name a repository, so no list is answered
// rather than one that leaves that repository out.
type UnresolvedChainError struct {
	Project, Issue string
}

func (e *UnresolvedChainError) Error() string {
	return fmt.Sprintf("the repositories of project %s are unresolved: the parent chain of %s is deeper than %d links or loops",
		e.Project, e.Issue, IssueDepthCap)
}

// projectRepositoriesQuery is ProjectRepositoriesSQL over the project bound at $1, in one walk:
// whether the project exists, the first issue by number whose chain the walk could not finish, and
// the repositories the others resolve to, each once and sorted byte-wise (collate "C", the order
// Go's sort.Strings gives, whatever the database's collation). The text is built once, at start.
var projectRepositoriesQuery = `
	select exists (select 1 from projects where key = $1), answer.unresolved, coalesce(answer.repositories, '{}')
	from (
		select (array_agg(owned.issue order by owned.number) filter (where owned.repo is null))[1] as unresolved,
			array_agg(distinct owned.repo collate "C" order by owned.repo collate "C") filter (where owned.repo is not null) as repositories
		from (` + ProjectRepositoriesSQL("project_key = $1") + `) owned
	) answer`

// ProjectRepositories is the repository each issue of project resolves to, open or done, read
// through q: its own, else the nearest one above it on `child_of`, else its project's default. It
// reads ProjectRepositoriesSQL, the text by which the GitHub proxy and Delivery hide a repository
// (RepositoryClause), so a repository is in this list exactly when that rule counts the project
// as its owner: issues alone own repositories, so a mapping the project holds adds nothing, and
// no recorded rename is read. Each repository is named once, lower-cased, and the list is sorted
// byte-wise; a project none of whose issues resolves to one answers [], never nil. An issue whose
// chain the walk could not finish is an *UnresolvedChainError naming the first by number, never
// the list of the others, and an unknown project is access.ErrProjectNotFound.
func ProjectRepositories(ctx context.Context, q Queryer, project string) ([]string, error) {
	var exists bool
	var unresolved *string
	repositories := []string{}
	if err := q.QueryRow(ctx, projectRepositoriesQuery, project).Scan(&exists, &unresolved, &repositories); err != nil {
		return nil, fmt.Errorf("resolve the repositories of project %s: %w", project, err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", access.ErrProjectNotFound, project)
	}
	if unresolved != nil {
		return nil, &UnresolvedChainError{Project: project, Issue: *unresolved}
	}
	return repositories, nil
}

// RepositoryClause passes the GitHub repository repoExpr names, as owner/name, to the viewer bound
// at viewerParam (VisibilityArg). A repository passes in two cases, and fails in every other:
//
//   - an issue of a project the viewer sees resolves to it (ProjectRepositoriesSQL), whatever other
//     project, private ones included, owns it too: where issues of a private project and of a
//     public project work in one repository, the public project's people read it whole;
//   - the viewer is open, and no private project the viewer does not see owns it.
//
// So a viewer who is not open, a restricted member or a credential acting for one, reads only a
// repository an issue of a project it sees resolves to; a full member's credential is open whatever
// it lists (access.CredentialCovers). A repository is held by its name
// alone: once GitHub renames a private project's repository or moves it to another owner, an open
// viewer reads it under the new name until an issue of that project resolves to the new name.
//
// The server's own viewer passes every repository, and so does an open viewer of a deployment with
// no private project, which reads no issue for it (R9). For an open viewer the private projects it
// does not see are read first, and those it sees only where one of the others owns the repository;
// for any other viewer only the projects it sees are read. It is in refs rather than access because
// refs imports access. The GitHub proxy (routes), the App-installation route (api) and delivery's
// pull requests that name no issue and runs that name none (api, delivery) each hold a repository
// to it.
//
// repoExpr is spliced more than once, so it must be a column, a bound parameter or another
// side-effect-free expression, not a bare column named repo or project, nor name the aliases owned,
// own_named, own_project, own_top, own_child or own_parent, nor repo_default; the clause answers
// true or false, never null.
func RepositoryClause(repoExpr, viewerParam string) string {
	open := access.ViewerFlag(viewerParam, "open")
	return `(` + access.ViewerFlag(viewerParam, "all") + ` or (` + open + ` and not ` + access.ViewerFlag(viewerParam, "private") + `) or case
		when ` + open + ` and not ` + repositoryHeld(repoExpr, viewerParam) + ` then true
		else ` + repositorySeen(repoExpr, viewerParam) + `
	end)`
}

// RepositoryHidden is RepositoryClause's negation: true where the GitHub proxy hides the
// repository repoExpr names from the viewer bound at viewerParam, false where it shows it, never
// null. A write that would give a repository to the people of a project its writer sees holds the
// repository to it with the writer as the viewer (api's refuseRepoHidden: an issue's own
// repository, a project's default), so a writer gives no project a repository the proxy would not
// show the writer, and the write and the proxy cannot come to different answers. repoExpr is
// spliced as RepositoryClause's.
func RepositoryHidden(repoExpr, viewerParam string) string {
	return `(not ` + RepositoryClause(repoExpr, viewerParam) + `)`
}

// repositorySeen is whether an issue of a project the viewer sees resolves to the repository
// repoExpr names (ProjectRepositoriesSQL). It asks three questions, each cheaper than a walk of
// every issue the viewer sees, and between them they find every such issue:
//
//   - is there a seen issue whose own repository is that name? It resolves to it. Issue
//     repositories are stored lower-cased, so the issues_repo index answers first; a name stored
//     under another case is read by a scan only when the index finds none.
//   - is there a seen project whose default is that name and which holds an issue that names no
//     repository and has no parent? That issue resolves to the default.
//   - does ProjectRepositoriesSQL resolve to that name a seen issue that names no repository and
//     whose parent is of another project? Those are the only issues the first two can miss: an
//     issue whose chain stays in its own project resolves to the own repository of an issue above
//     it in that project, which the first question finds, or to its project's default through a
//     top issue of that project, which the second finds. An issue whose chain leaves its project
//     resolves as the last issue of its project on the chain does, the one whose parent is of
//     another project, so only those are walked: the chain stops at the project edge
//     (IssueRepoTerms), so such an issue resolves to its own project's default, never to a
//     repository its parent's project names.
//
// It sits under RepositoryClause's CASE, so each `exists` ends in `offset 0`, as every `exists` a
// clause adds there does (#3178 at 0f8b389c).
func repositorySeen(repoExpr, viewerParam string) string {
	named := `lower((` + repoExpr + `)::text)`
	return `(case when exists (select from issues own_named where own_named.repo = ` + named + `
			and ` + access.ProjectClause("own_named.project_key", viewerParam) + ` offset 0) then true
		when exists (select from issues own_named where own_named.repo is not null and lower(own_named.repo) = ` + named + `
			and ` + access.ProjectClause("own_named.project_key", viewerParam) + ` offset 0) then true
		when exists (select from projects own_project where ` + access.ProjectClause("own_project.key", viewerParam) + `
			and lower(` + projectDefaultRepoSQL("own_project.key") + `) = ` + named + `
			and exists (select from issues own_top where own_top.project_key = own_project.key and own_top.parent_key is null and own_top.repo is null
				and ` + access.ProjectClause("own_top.project_key", viewerParam) + ` offset 0)
			offset 0) then true
		else exists (select from (` + ProjectRepositoriesSQL(access.ProjectClause("project_key", viewerParam)+` and key in (
				select own_child.key from issues own_child join issues own_parent on own_parent.key = own_child.parent_key
				where own_child.repo is null and own_parent.project_key <> own_child.project_key
					and `+access.ProjectClause("own_child.project_key", viewerParam)+`)`) + `) owned
			where owned.repo = ` + named + `
			offset 0) end)`
}

// repositoryHeld is whether a private project the viewer does not see owns the repository repoExpr
// names: an issue of it resolves to the name. It ends in `offset 0`, as repositorySeen does.
func repositoryHeld(repoExpr, viewerParam string) string {
	return `exists (select from (` + ProjectRepositoriesSQL(access.HiddenPrivateProjectClause("project_key", viewerParam)) + `) owned
		where owned.repo = lower((` + repoExpr + `)::text)
		offset 0)`
}
