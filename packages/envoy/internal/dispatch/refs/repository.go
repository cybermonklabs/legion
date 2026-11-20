package refs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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

// RepositoryClause passes the GitHub repository repoExpr names, as owner/name, whose GitHub id
// idExpr answers (a bigint, null where it is not known), to the viewer bound at viewerParam
// (VisibilityArg). A repository passes in two cases, and fails in every other:
//
//   - an issue of a project the viewer sees resolves to it (ProjectRepositoriesSQL), by its name;
//   - the viewer is open, and no project the viewer does not see owns it, by its name or by its id.
//
// A project owns a repository by its id when an issue of it resolves to a name repository_ids
// records the same id for (LearnRepositoryIDs), which is how a private project's repository is
// still its own after GitHub renames it or moves it to another owner. Only a private project the
// viewer does not see is read for the id. Where such a project owns any repository, a repository
// whose id is not known, or any repository at all while a name such a project owns has no id
// repository_ids knows, fails for an open viewer whom no issue it sees gives the repository: the
// clause cannot tell that repository from the hidden one, so it answers as hidden
// (RepositoryIDNeededClause says when a caller must ask GitHub for the id).
//
// The server's own viewer passes every repository, and so does an open viewer of a deployment with
// no private project, which reads no issue and no id for it (R9). For an open viewer the private
// projects it does not see are read first, and those it sees only where one of the others could
// hide the repository; for any other viewer only the projects it sees are read. It is in refs
// rather than access because refs imports access. The GitHub proxy (routes) and delivery's pull
// requests that name no issue and runs that name none (api, delivery) each hold a repository to it.
//
// repoExpr and idExpr are each spliced more than once, so each must be a column, a bound parameter
// or another side-effect-free expression, and neither may be a bare column named repo or project,
// nor name the aliases owned or learned; the clause answers true or false, never null.
func RepositoryClause(repoExpr, idExpr, viewerParam string) string {
	open := access.ViewerFlag(viewerParam, "open")
	return `(` + access.ViewerFlag(viewerParam, "all") + ` or (` + open + ` and not ` + access.ViewerFlag(viewerParam, "private") + `) or case
		when ` + open + ` and not ` + repositoryHeld(repoExpr, idExpr, viewerParam) + ` then true
		else ` + repositorySeen(repoExpr, viewerParam) + `
	end)`
}

// RepositoryIDNeededClause is true when RepositoryClause's answer for the repository repoExpr names
// turns on its GitHub id for the viewer bound at viewerParam: the viewer is open, sees not every
// object, a project is private, no issue of a project the viewer sees resolves to the repository,
// and a private project the viewer does not see owns repositories, none of them under this name.
// Only then does the GitHub proxy ask GitHub for the repository's id (routes' repositoryHidden);
// every other answer is decided by the name, and GitHub is asked nothing about a repository the
// name hides.
func RepositoryIDNeededClause(repoExpr, viewerParam string) string {
	return `(not ` + access.ViewerFlag(viewerParam, "all") + ` and ` + access.ViewerFlag(viewerParam, "open") + ` and ` +
		access.ViewerFlag(viewerParam, "private") + ` and not ` + repositorySeen(repoExpr, viewerParam) + ` and coalesce((
		select bool_and(owned.repo <> lower((` + repoExpr + `)::text)) from (` + ProjectRepositoriesSQL(hiddenPrivateProjects(viewerParam)) + `) owned
	), false))`
}

// RepositoryIDSQL is the GitHub id repository_ids records for the repository repoExpr names, null
// where it records none, or records that the App reads no repository under that name: the id a
// stored row of a repository, which carries none of its own, is held to RepositoryClause with.
func RepositoryIDSQL(repoExpr string) string {
	return `(select recorded.github_id from repository_ids recorded where recorded.repo = lower((` + repoExpr + `)::text))`
}

// repositorySeen is whether an issue of a project the viewer sees resolves to the repository
// repoExpr names.
func repositorySeen(repoExpr, viewerParam string) string {
	return `exists (select from (` + ProjectRepositoriesSQL(access.ProjectClause("project_key", viewerParam)) + `) owned
		where owned.repo = lower((` + repoExpr + `)::text))`
}

// repositoryHeld is whether a private project the viewer does not see owns the repository repoExpr
// names, whose id idExpr answers, or may: an issue of it resolves to the name, or to a name
// repository_ids records that id for, or it owns any repository while the id is not known, or
// while a name it owns has no id repository_ids knows.
func repositoryHeld(repoExpr, idExpr, viewerParam string) string {
	return `coalesce((select bool_or(owned.repo = lower((` + repoExpr + `)::text) or learned.github_id is null or (` + idExpr + `)::bigint is null
			or learned.github_id = (` + idExpr + `)::bigint)
		from (` + ProjectRepositoriesSQL(hiddenPrivateProjects(viewerParam)) + `) owned
		left join repository_ids learned on learned.repo = owned.repo), false)`
}

// hiddenPrivateProjects passes, over the bare column project_key, a private project the viewer does
// not see.
func hiddenPrivateProjects(viewerParam string) string {
	return `not ` + access.ProjectClause("project_key", viewerParam) + ` and project_key in (select hiding.key from projects hiding where hiding.visibility = 'private')`
}

// RepositoryIDsPerLearn is how many names one LearnRepositoryIDs asks GitHub about at most, so a
// request that learns pays for a bounded number of GitHub reads; the names left are learned by the
// next call, and until then RepositoryClause answers as hidden what turns on them.
const RepositoryIDsPerLearn = 10

// RepositoryIDRecheck is how long GitHub's answer that the App reads no repository under a name
// stands before LearnRepositoryIDs asks again, so a name the App cannot read costs one GitHub read
// an hour rather than one a request. RepositoryClause reads such a name as one whose id it does not
// know whatever its age.
const RepositoryIDRecheck = time.Hour

// repositoryIDsOwedQuery is every name an issue of a private project resolves to that
// repository_ids holds no answer for, or holds an answer older than $1 seconds that the App reads no
// repository under it, at most $2 of them, sorted.
var repositoryIDsOwedQuery = `select distinct owned.repo
	from (` + ProjectRepositoriesSQL(`project_key in (select owning.key from projects owning where owning.visibility = 'private')`) + `) owned
	left join repository_ids learned on learned.repo = owned.repo
	where learned.repo is null or (learned.github_id is null and learned.learned_at < now() - $1::bigint * interval '1 second')
	order by owned.repo
	limit $2`

// LearnRepositoryIDs asks GitHub, through client, for the id of each repository an issue of a
// private project resolves to that repository_ids holds no id for (repositoryIDsOwedQuery), and
// records each answer: the id GitHub gives the repository it reads under the name (GET
// /repos/{owner}/{repo}, with the token of the App installation covering it), with the name GitHub
// gives that repository now beside it when that is another name (a name renamed away is answered by
// GitHub under its new one), or no id where GitHub answers that the App reads no repository under
// the name. A deployment with no private project owes no name, so GitHub is asked nothing (R9),
// and so is one with no App key (a nil client), whose proxy reads no repository at all.
//
// The first other failure, a rate limit among them, ends the call and is returned: its name and the
// names after it stay owed, and RepositoryClause answers as hidden what turns on them. The GitHub
// proxy calls it before it judges a repository by its id (routes' repositoryHidden), and the
// delivery intake on each pull-request envelope, so Delivery's reads find the ids they turn on.
func LearnRepositoryIDs(ctx context.Context, pool *store.Pool, client *githubapp.Client) error {
	if client == nil {
		return nil
	}
	rows, err := pool.Query(ctx, repositoryIDsOwedQuery, int64(RepositoryIDRecheck/time.Second), RepositoryIDsPerLearn)
	if err != nil {
		return fmt.Errorf("read the repositories whose GitHub id is owed: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the repositories whose GitHub id is owed: %w", err)
	}
	for _, name := range names {
		owner, repo, _ := strings.Cut(name, "/")
		identity, err := ReadRepositoryIdentity(ctx, client, owner, repo)
		switch {
		case errors.Is(err, ErrNoRepository):
			if err := recordRepositoryID(ctx, pool, name, nil); err != nil {
				return err
			}
			continue
		case err != nil:
			return fmt.Errorf("learn the GitHub id of %s: %w", name, err)
		}
		if err := RecordRepositoryID(ctx, pool, name, identity.ID); err != nil {
			return err
		}
		if current := strings.ToLower(identity.FullName); current != name && strings.Contains(current, "/") {
			if err := RecordRepositoryID(ctx, pool, current, identity.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrNoRepository is GitHub's answer that the App reads no repository under a name: no installation
// of the App covers it, or the installation's read of it is refused or not found.
var ErrNoRepository = errors.New("the GitHub App reads no repository under this name")

// ReadRepositoryIdentity is GitHub's GET /repos/{owner}/{repo} read with the token of the App
// installation covering it: the id of the repository the App reads under the name, and the name
// GitHub gives it now. It is ErrNoRepository where GitHub answers that the App reads none there,
// and GitHub's own error, a rate limit among them, for any other failure. The GitHub proxy reads
// the id of the repository it is asked for through it (routes' repositoryHidden), and
// LearnRepositoryIDs the id of each name it learns.
func ReadRepositoryIdentity(ctx context.Context, client *githubapp.Client, owner, repo string) (githubapp.RepositoryAnswer, error) {
	if owner == "" || repo == "" || strings.Contains(repo, "/") {
		return githubapp.RepositoryAnswer{}, ErrNoRepository
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err == nil {
		var answer githubapp.RepositoryAnswer
		answer, err = client.RepositoryIdentity(ctx, token, owner, repo, "")
		if err == nil && answer.ID == 0 {
			err = fmt.Errorf("GitHub answered %s/%s with no id", owner, repo)
		}
		if err == nil {
			return answer, nil
		}
	}
	if errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrRepositoryUnreadable) {
		return githubapp.RepositoryAnswer{}, ErrNoRepository
	}
	return githubapp.RepositoryAnswer{}, err
}

// RecordRepositoryID records id as the GitHub id of the repository repo names (owner/name, in any
// case), replacing what repository_ids held for the name: an answer GitHub gave just now, as the
// delivery intake reads it from a pull request it fetched (its base repository's id) and
// LearnRepositoryIDs from the repository itself.
func RecordRepositoryID(ctx context.Context, pool *store.Pool, repo string, id int64) error {
	return recordRepositoryID(ctx, pool, repo, &id)
}

// recordRepositoryID is RecordRepositoryID with no id for GitHub's answer that the App reads no
// repository under the name.
func recordRepositoryID(ctx context.Context, pool *store.Pool, repo string, id *int64) error {
	if _, err := pool.Exec(ctx, `
		insert into repository_ids (repo, github_id) values (lower($1), $2)
		on conflict (repo) do update set github_id = excluded.github_id, learned_at = now()
	`, repo, id); err != nil {
		answer := "no id"
		if id != nil {
			answer = strconv.FormatInt(*id, 10)
		}
		return fmt.Errorf("record %s as the GitHub id of %s: %w", answer, repo, err)
	}
	return nil
}
