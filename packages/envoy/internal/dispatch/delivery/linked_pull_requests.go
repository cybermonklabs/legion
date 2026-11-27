package delivery

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkedPullRequestTerms are the `with recursive` terms of the pull requests the external links
// of the issue $1 name in a repository the issue does not resolve to: linked_pulls (repo,
// number, url), one row per link that names a pull request by the one link rule
// (LinkedPullRequestSQL, linkedPullRequest in Go), the repository lowercased, as refs.ExternalRepo
// spells a repository, and under the canonical name it is called today (refs.CanonicalRepoSQL
// over renames, the pairs the boot read).
//
// The record route reads them to say which repositories a result of the issue may name a commit
// of (LinkedPullRequests, api/results_linked.go's refuseUnknownCommit): an issue whose work spans
// two repositories links the pull request it has in the second one, and a result at that pull
// request's head names a commit of the second repository. They are read from the links
// themselves, since the route asks GitHub about each pull request they name, and Dispatch may hold
// no row of it yet. The results read takes the same pull request from the statement instead
// (HeadsAttributedTo, MergeIn): a link of a pull request's own URL gives that pull
// request to the issue in any repository (attributedTerms), so it is one of the issue's heads
// once Dispatch holds its row, and the timeline names the same issue for it. A link to a pull
// request of the repository the issue resolves to names nothing here, since that repository is
// asked about the commit before any link is read. The terms hold the issue to a repository by the
// attribution's own rule, so the two cannot disagree about which repository is the issue's.
//
// The link rule is the link write's, so each link the write owes a read for names its pull request
// here too: the /pull/ and the /issues/ form, the scheme, the host and the path word in any letter
// case, a number up to 2147483647, and an owner and a name GitHub can have (refs.SplitGitHubRepo).
// A link the rule refuses names nothing here.
//
// linked_pulls is materialized: it is the handful of pull requests one issue links, read once,
// so a reader of it compares each row with a stored name and number, rather than evaluating the
// link's pattern and the canonical name's expression again for every row it meets.
func linkedPullRequestTerms(renames refs.Renames) string {
	return `subjects as (select $1::text as key), ` + refs.IssueRepoTerms + `,
	linked_pulls as materialized (
		select ` + refs.CanonicalRepoSQL(renames, "link.repo") + ` as repo, link.number, link.url
		from (
			select l.url, lower(lp.repo) as repo, lp.number
			from issue_external_links l cross join lateral ` + LinkedPullRequestSQL("l.url") + ` lp
			where l.issue_key = $1
		) link
		join issues i on i.key = $1
		` + refs.IssueRepoJoin + `
		where not ` + attributionHeld(renames, "link.repo") + `
	)`
}

// LinkedPullRequest is one pull request an issue's external links name in a repository the
// issue does not resolve to (linkedPullRequestTerms).
type LinkedPullRequest struct {
	Repo   string
	Number int
}

// linkedPullRequestsQuery reads linkedPullRequestTerms in the order the links are listed in.
func linkedPullRequestsQuery(renames refs.Renames) string {
	return `with recursive ` + linkedPullRequestTerms(renames) + `
	select repo, number from linked_pulls order by url`
}

// LinkedPullRequests is every pull request the external links of issueKey name in a repository
// the issue does not resolve to, in the order the issue lists its links. The result route reads
// it to say which repositories a result of the issue may name a commit of (api/results_linked.go,
// refuseUnknownCommit).
func (a *Attributor) LinkedPullRequests(ctx context.Context, q refs.Queryer, issueKey string) ([]LinkedPullRequest, error) {
	rows, err := q.Query(ctx, linkedPullRequestsQuery(a.renames), issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
	}
	defer rows.Close()
	var linked []LinkedPullRequest
	for rows.Next() {
		var pull LinkedPullRequest
		if err := rows.Scan(&pull.Repo, &pull.Number); err != nil {
			return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
		}
		linked = append(linked, pull)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
	}
	return linked, nil
}

// LinkedHeadReadsPerRead bounds how many linked pull requests one read of an issue's results reads
// from GitHub (ReadLinkedHeads): an issue links a handful of pull requests, and a read that met
// more leaves the rest to the next read and to the reconcile, which reads every owed one.
const LinkedHeadReadsPerRead = 10

// LinkedHead is one pull request an issue links by hand whose head Dispatch does not hold
// (LinkedHeadsUnread): Repo is its owed name (the canonical name, Attributor.owedName), and owed is
// the read of it owed, nil where none is, read before GitHub is asked so that ReadLinkedHead settles
// that revision alone.
type LinkedHead struct {
	Repo   string
	Number int
	owed   *owedRead
}

// LinkedHeadsUnread is each pull request the external links of issueKey name, in any repository
// (the issue's own one too, unlike LinkedPullRequests), whose head Dispatch does not hold: no head
// row under its canonical name in any letter case. A row Dispatch holds is a head the results read
// compares with, whether or not a read of GitHub is owed for it, and the reconcile reads every
// owed one (Reconcile.readOwedPullRequests), so the results read rewrites no row it holds and asks
// GitHub nothing for it. A read owed that GitHub refused and that is recorded unreadable at its
// revision is left out: nothing can change GitHub's answer until a write gives the row a new
// revision (pull_request_reads_unreadable.go), and UnreadableLinksOf names it. The list is in the
// order of the owed names, at most LinkedHeadReadsPerRead of them. answers is the caller's
// request's, as HeadsAttributedTo takes them: a read-only caller's read reads the rows its own
// read-throughs would have stored.
func (a *Attributor) LinkedHeadsUnread(ctx context.Context, q Querier, issueKey string, answers HeadAnswers) ([]LinkedHead, error) {
	var unread []LinkedHead
	err := answers.Read(ctx, q, func(q Querier) error {
		var err error
		unread, err = a.linkedHeadsUnread(ctx, q, issueKey)
		return err
	})
	return unread, err
}

// linkedHeadsUnread is LinkedHeadsUnread's reads on q.
func (a *Attributor) linkedHeadsUnread(ctx context.Context, q Querier, issueKey string) ([]LinkedHead, error) {
	rows, err := q.Query(ctx, `select url from issue_external_links where issue_key = $1`, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	urls, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	var linked []LinkedHead
	seen := map[string]struct{}{}
	for _, url := range urls {
		given, number, ok := linkedPullRequest(url)
		if !ok {
			continue
		}
		repoFull, err := a.owedName(given, number)
		if err != nil {
			continue
		}
		key := strings.ToLower(repoFull) + "#" + fmt.Sprint(number)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		linked = append(linked, LinkedHead{Repo: repoFull, Number: number})
	}
	slices.SortFunc(linked, func(x, y LinkedHead) int {
		return cmp.Or(strings.Compare(strings.ToLower(x.Repo), strings.ToLower(y.Repo)), cmp.Compare(x.Number, y.Number))
	})
	var unread []LinkedHead
	for _, link := range linked {
		if len(unread) == LinkedHeadReadsPerRead {
			break
		}
		var held bool
		var owedRepo *string
		var revision *int64
		var unreadable bool
		if err := q.QueryRow(ctx, `
			select exists (select 1 from pull_request_heads h
				where h.number = $2 and lower(`+refs.CanonicalRepoSQL(a.renames, "h.repo")+`) = lower($1)),
				o.repo, o.revision, coalesce(o.unreadable_revision = o.revision, false)
			from (select 1) one left join lateral (select o.repo, o.revision, o.unreadable_revision from `+owedRowInAnyCase+`) o on true
		`, link.Repo, link.Number).Scan(&held, &owedRepo, &revision, &unreadable); err != nil {
			return nil, fmt.Errorf("read whether the head of %s#%d is held: %w", link.Repo, link.Number, err)
		}
		if unreadable || held {
			continue
		}
		if owedRepo != nil {
			link.owed = &owedRead{Repo: *owedRepo, Number: link.Number, Revision: *revision}
		}
		unread = append(unread, link)
	}
	return unread, nil
}

// ReadLinkedHead reads one pull request of LinkedHeadsUnread from GitHub and stores its head, its
// state and its texts through the refresh every stored row takes (RefreshPullRequestHead), as the
// reconcile's read of an owed pull request does (Reconcile.readOwedPullRequests), and records how
// the owed read it carries ended (recordOwedReadOutcome): settled where GitHub answered, counted
// as a failure or a refusal otherwise, so the reconcile reads it again. A pull request with no owed
// read that GitHub did not answer owes nothing new here: the next read of the results asks again.
// It answers the read's error, a rate limit included, for its caller to log.
func (a *Attributor) ReadLinkedHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, link LinkedHead, now time.Time) error {
	return a.readLinkedHead(ctx, facts{pool: pool}, client, link, now)
}

// AnswerLinkedHead is ReadLinkedHead for a read-only caller's results read: the same reads of
// GitHub, with each statement made in a transaction that holds answers' writes and is rolled
// back, and each write kept in answers (facts, HeadAnswers), so it stores nothing.
func (a *Attributor) AnswerLinkedHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, answers *HeadAnswers, link LinkedHead, now time.Time,
) error {
	return a.readLinkedHead(ctx, facts{pool: pool, answers: answers}, client, link, now)
}

// readLinkedHead is ReadLinkedHead through f.
func (a *Attributor) readLinkedHead(ctx context.Context, f facts, client *githubapp.Client, link LinkedHead, now time.Time) error {
	_, refused, err := a.refreshPullRequestHead(ctx, f, client, nil, link.Repo, link.Number, now)
	if link.owed != nil {
		outcome, refusedAs := owedReadAnswered, ""
		switch {
		case err != nil:
			outcome = owedReadFailed
		case refused != nil:
			outcome, refusedAs = owedReadRefused, refusalOf(refused.err)
		}
		owed := *link.owed
		if _, recordErr := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
			return false, recordOwedReadOutcome(ctx, db, owed, outcome, refusedAs)
		}); recordErr != nil {
			slog.Warn("dispatch: record how a linked pull request's owed read ended; the reconcile reads it again",
				"repo", link.Repo, "number", link.Number, "error", recordErr)
		}
	}
	if err == nil && refused != nil {
		return refused.err
	}
	return err
}
