package delivery

// A link by hand owes a read of the pull request it names in any repository
// (OweLinkedPullRequestReads), and an issue links pull requests and issues of repositories the App
// is not installed on, as a bug report of another project is linked. GitHub refuses every read of
// such a link the same way, so the first refusal records the owed row as unreadable at its
// revision (recordOwedReadOutcome), and no pass reads it again until something can change
// GitHub's answer: a new owe of the pull request, which gives the row a new revision, or an
// installation of the App that comes to cover the repository (wakeUnreadableReads). The row stays,
// so the record of which pull request a link named is kept (R4 of the results lookup), and the
// issue's results name each such link (UnreadableLinksOf).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// wakeUnreadableReads makes each row recorded as unreadable readable again where an installation
// of the App now lists its repository, so the same pass reads it (readOwedPullRequests). It asks
// nothing where no row is unreadable. Otherwise it reads the App's installations and the
// repositories of each through the client's caches (githubapp.Client.ListInstallations and
// ListInstallationRepositoriesByID, each trusted for the client's installation span and shared
// with the merged search), so a pass costs no call while those answers are fresh, and an
// installation that comes to cover a repository is found within that span. A repository no
// installation lists stays unreadable. An installation whose repositories cannot be listed wakes
// nothing of its own, and its error is answered beside the rows the other installations woke; a
// failure to list the installations wakes nothing. Either leaves the rows to the next pass.
func wakeUnreadableReads(ctx context.Context, pool *store.Pool, client *githubapp.Client) error {
	rows, err := pool.Query(ctx, `
		select distinct lower(repo) from pull_request_reads_owed where unreadable_revision = revision`)
	if err != nil {
		return fmt.Errorf("list the repositories of the unreadable links: %w", err)
	}
	unreadable := map[string]bool{}
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			rows.Close()
			return fmt.Errorf("list the repositories of the unreadable links: %w", err)
		}
		unreadable[repo] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list the repositories of the unreadable links: %w", err)
	}
	if len(unreadable) == 0 || client == nil {
		return nil
	}
	installations, err := client.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("list the App's installations for the unreadable links: %w", err)
	}
	var woken []string
	var failures []error
	for _, installation := range installations {
		names, err := client.ListInstallationRepositoriesByID(ctx, installation.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("list the repositories of installation %d: %w", installation.ID, err))
			continue
		}
		for _, name := range names {
			if repo := strings.ToLower(name); unreadable[repo] {
				woken = append(woken, repo)
				delete(unreadable, repo)
			}
		}
	}
	if len(woken) > 0 {
		if _, err := pool.Exec(ctx, `
			update pull_request_reads_owed set unreadable_revision = null
			where unreadable_revision = revision and lower(repo) = any($1::text[])`, woken); err != nil {
			failures = append(failures, fmt.Errorf("make the links of %d repositories the App now covers readable: %w", len(woken), err))
		}
	}
	return errors.Join(failures...)
}

// UnreadableLinksOf is each link of issueKey whose owed read is recorded as unreadable
// (recordOwedReadOutcome), in the order of the links' URLs: a link to a pull request or an issue
// of a repository the App is not installed on, which no pass reads again until a new owe or an
// installation of the App makes it readable. A link names its pull request as the owe does
// (linkedPullRequest, owedName), so the row it finds is the one its owe wrote.
func UnreadableLinksOf(ctx context.Context, pool *store.Pool, issueKey string) ([]model.UnreadableLink, error) {
	rows, err := pool.Query(ctx, `select url from issue_external_links where issue_key = $1 order by url`, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	var urls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
		}
		urls = append(urls, url)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	var links []model.UnreadableLink
	for _, url := range urls {
		given, number, ok := linkedPullRequest(url)
		if !ok {
			continue
		}
		repoFull, err := owedName(ctx, pool, given, number)
		if err != nil {
			return nil, err
		}
		var link model.UnreadableLink
		err = pool.QueryRow(ctx, `
			select refused_since from pull_request_reads_owed
			where repo = $1 and number = $2 and unreadable_revision = revision`, repoFull, number).Scan(&link.RefusedSince)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("read whether the link %s of %s is unreadable: %w", url, issueKey, err)
		}
		link.URL = url
		links = append(links, link)
	}
	return links, nil
}
