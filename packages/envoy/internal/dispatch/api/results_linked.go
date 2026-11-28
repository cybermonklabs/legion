package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// results_linked.go holds what the results route does with a linked repository: one the check's
// issue does not resolve to, in which its external links name a GitHub pull request. An issue
// whose work spans two repositories links the pull request it has in the second one, and a
// result at that pull request's head names a commit of a repository the issue does not resolve
// to. The record route takes such a commit (refuseUnknownCommit) and keeps the repository with
// the row (model.CheckResult.CommitRepo); the results read says whose a result's commit is from
// that row (commitRepositoryOf) and walks a branch check's result in a linked repository
// (linkedBranchBases), where results_comparison.go walks every other result in the repository
// the check's issue resolves to.

// linkedPullRequestReads is the most linked pull requests one record reads the state of from
// GitHub (refuseUnknownCommit, linkedRepositoryHolds). Each read is a GitHub call the record
// makes one after another before it answers, and an issue may hold any number of links, since
// the issue PATCH bounds none: unbounded, one record would spend a call for every link a person
// or an agent ever added, against the App's hourly quota that every other read of Dispatch
// shares. A repository is read only until one of its linked pull requests counts, so the bound
// is also the most linked repositories one record reads, and each of those is asked about the
// commit at most once. Ten leaves room for an issue whose work spans several repositories, or
// carries a few links that name no pull request at all, and holds one record to at most ten such
// calls and ten commit reads. A link past the bound is left unread, and a refusal says how many
// were. A refusal names at most this many of the linked pull requests Dispatch could not read,
// and counts the rest (linkedReads.couldNotRead).
const linkedPullRequestReads = 10

// refuseUnknownCommit resolves a result's repository before its transaction opens.
// A nil repo needs no GitHub read. Otherwise a stored open or merged head decides first
// (headRepositoryOther). Every other commit is asked of the issue's repository, the other
// attributed-head repositories in name order, then the remaining linked repositories.
// A proved hold stops the search. Each repository is asked once.
//
// A failed head-list read refuses the result unless GitHub proves the issue's own repository
// holds the commit. A failed link-list read also refuses it. With no proved hold, an unreadable
// linked repository refuses it for now (linkedReadRefusal). An unreadable attributed repository
// proves no absence and otherwise leaves the result in the issue's repository.
//
// A linked pull request counts only while open or merged (linkedRepositoryHolds).
// At most linkedPullRequestReads linked pull requests are read. A 400 RESULT_COMMIT_UNKNOWN
// names the repositories proved not to hold the commit, invalid links, and links left unread.
// Refusals leave neither a result row nor a result.recorded event.
//
// A non-nil answer is the other repository's canonical name, stored in CheckResult.CommitRepo
// by recordResultUnderOwnerLocks. A nil answer leaves the result in the issue's repository.
// The results read and its read-through use that stored choice (commitRepositoryOf).
func (s *server) refuseUnknownCommit(ctx context.Context, q refs.Queryer, issueKey string, repo *string, sha string) (*string, error) {
	if repo == nil {
		return nil, nil
	}
	heads, headsErr := s.deps.Attributor.HeadsAttributedTo(ctx, q, issueKey, delivery.HeadAnswers{})
	if at := slices.IndexFunc(heads, func(head delivery.AttributedHead) bool { return head.HeadSHA == sha }); at >= 0 {
		return headRepositoryOther(heads[at], *repo), nil
	}
	held, err := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, *repo, sha)
	if err == nil && held {
		return nil, nil
	}
	if headsErr != nil {
		return nil, headsErr
	}
	// An unreadable attributed repository proves no absence. A failed linked read is kept
	// separately below, because that path refuses a record whose repository is not established.
	unread := err != nil
	if unread {
		slog.Info("dispatch: GitHub could not say whether a recorded result's commit is held",
			"repo", *repo, "commit", sha, "error", err)
	}
	checked := []string{*repo}
	asked := []string{*repo}
	for _, other := range otherHeadRepositories(heads, *repo) {
		asked = append(asked, other)
		held, err := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, other, sha)
		switch {
		case err != nil:
			slog.Info("dispatch: GitHub could not say whether the repository of a pull request of the issue holds a recorded result's commit",
				"issue", issueKey, "repo", other, "commit", sha, "error", err)
			unread = true
		case held:
			return &other, nil
		default:
			checked = append(checked, other)
		}
	}
	links, err := s.linkedRepositories(ctx, q, issueKey)
	if err != nil {
		return nil, err
	}
	// failed is the first linked read that did not answer, and failedRepo its repository.
	var failed error
	var failedRepo string
	reads := linkedReads{left: linkedPullRequestReads}
	for _, link := range links {
		if slices.ContainsFunc(asked, func(name string) bool { return strings.EqualFold(name, link.repo) }) {
			continue
		}
		counts, held, err := s.linkedRepositoryHolds(ctx, link, sha, &reads)
		switch {
		case err != nil:
			slog.Info("dispatch: GitHub could not say whether a linked repository holds a recorded result's commit",
				"issue", issueKey, "repo", link.repo, "commit", sha, "error", err)
			if failed == nil {
				failed, failedRepo = err, link.repo
			}
		case held:
			return &link.repo, nil
		case counts:
			checked = append(checked, link.repo)
		}
	}
	switch {
	case failed != nil:
		return nil, linkedReadRefusal(failedRepo, sha, failed)
	case unread:
		return nil, nil
	}
	return nil, errorf(apicode.ResultCommitUnknown,
		"%s no commit %s: record the result once the commit is pushed, or link the issue to an open or merged pull request of the repository that holds it%s%s%s",
		repositoriesHold(checked), sha, reads.couldNotRead(), reads.setAside(), reads.leftUnread())
}

// headRepositoryOther is the repository head is of where it is not repo, the repository the
// check's issue resolves to, and nil where it is: a linked pull request's under the canonical
// name its link is read by (delivery.AttributedHead.LinkedRepo), and else the row's own, compared
// without case.
func headRepositoryOther(head delivery.AttributedHead, repo string) *string {
	switch {
	case head.LinkedRepo != "":
		return &head.LinkedRepo
	case !strings.EqualFold(head.Repo, repo):
		return &head.Repo
	}
	return nil
}

// otherHeadRepositories is the repository of each pull request in heads that is attributed to
// the issue and not of repo, each once, compared without case, in name order. A linked pull
// request's repository is not among them: refuseUnknownCommit reads it as a linked repository.
func otherHeadRepositories(heads []delivery.AttributedHead, repo string) []string {
	var others []string
	for _, head := range heads {
		if head.LinkedRepo != "" || strings.EqualFold(head.Repo, repo) {
			continue
		}
		if !slices.ContainsFunc(others, func(name string) bool { return strings.EqualFold(name, head.Repo) }) {
			others = append(others, head.Repo)
		}
	}
	slices.Sort(others)
	return others
}

// linkedReadRefusal is the refusal of a record whose read of repo, a linked repository, did not
// answer (err) where no repository was shown to hold the commit: 502 GITHUB_UNREADABLE, the code
// Dispatch answers a GitHub read that failed with (getAppInstallation), naming the repository,
// the commit and GitHub's error, and telling the caller to record the result again. Nothing is
// stored, so the record is made again whole. GitHub's error is worded by gitHubReason, as
// getAppInstallation's 502 words it: where GitHub refused the read (githubapp.StatusError) or
// rate-limited it (githubapp.RateLimitError), the body the error's text ends with is replaced by
// the `message` GitHub wrote, and left out where it wrote none; and the text is cut to
// gitHubReasonBytes, saying so, since any other error keeps its text: an installation token mint
// GitHub refused carries GitHub's body as githubapp.ShownBody shows it, its first 512 characters,
// which may take 2,048 bytes, and its size. Where the read that failed is the installation token
// mint (delivery.ErrTokenMint), as for a suspended installation's 403, the sentence names the
// mint, since GitHub was asked nothing about the repository itself. A rate limit names GitHub's
// wait, in the sentence and in Retry-After, rounded up to whole seconds as every Retry-After
// Dispatch writes is (ratelimit.RetryAfterSeconds); any other failed read names none, since
// GitHub gave none.
func linkedReadRefusal(repo, sha string, err error) error {
	reason := gitHubReason(err)
	limited, rateLimited := githubapp.AsRateLimit(err)
	again := "record the result again"
	retryAfter := 0
	if rateLimited && limited.Wait > 0 {
		retryAfter = ratelimit.RetryAfterSeconds(limited.Wait)
		again = fmt.Sprintf("record the result again in %s, the wait GitHub named", time.Duration(retryAfter)*time.Second)
	}
	failed := fmt.Sprintf("GitHub could not be read for whether %s, a repository the issue links a pull request in, holds commit %s", repo, sha)
	if errors.Is(err, delivery.ErrTokenMint) {
		failed = fmt.Sprintf("Dispatch's GitHub App could not mint an installation token for %s, a repository the issue links a pull request in, so GitHub could not be read for whether it holds commit %s", repo, sha)
	}
	refusal := errorf(apicode.GitHubUnreadable, "%s: %s. Nothing was recorded: %s", failed, reason, again)
	refusal.retryAfter = retryAfter
	return refusal
}

// repositoriesHold is the subject of a refusal's sentence, every repository it checked: "a
// holds" for one, "a and b hold" for two, "a, b and c hold" for more.
func repositoriesHold(repos []string) string {
	if len(repos) == 1 {
		return repos[0] + " holds"
	}
	return joinAnd(repos) + " hold"
}

// joinAnd lists items as a sentence does: "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// linkedReads is what one record has left of its bound on the linked pull requests it reads
// (linkedPullRequestReads), how many linked pull requests the bound left unread, each repository
// Dispatch cannot read (delivery.RepositoryUnreadable) and each repository whose name GitHub's
// naming rule refuses (refs.ErrNotGitHubRepo), in the order the record met them: unreadRepos
// names each it cannot read, unreadPulls each linked pull request of a repository whose read of a
// pull request's state answered so, and unreadCommits the pull request read open or merged of a
// repository whose commit read answered so, each pull request named
// `owner/repo#n (repository_unreadable)`; and misnamed names each repository the rule refuses.
type linkedReads struct {
	left, unread  int
	unreadRepos   []string
	unreadPulls   []string
	unreadCommits []string
	misnamed      []string
}

// cannotRead keeps link, a repository whose read of a linked pull request's state answered that
// Dispatch cannot read it, and all its linked pull requests: the answer is about the whole
// repository, so none of its pull requests can be read until someone grants the App it.
func (r *linkedReads) cannotRead(link linkedRepository) {
	r.unreadRepos = append(r.unreadRepos, link.repo)
	for _, number := range link.numbers {
		r.unreadPulls = append(r.unreadPulls, unreadPull(link.repo, number))
	}
}

// cannotReadCommit keeps link, a repository whose linked pull request number read open or merged
// and whose commit read then answered that Dispatch cannot read it: the pull request was read,
// and what Dispatch could not read is the commit in its repository.
func (r *linkedReads) cannotReadCommit(link linkedRepository, number int) {
	r.unreadRepos = append(r.unreadRepos, link.repo)
	r.unreadCommits = append(r.unreadCommits, unreadPull(link.repo, number))
}

// misnamedRepository keeps link, a repository whose name GitHub's naming rule refuses
// (refs.SplitGitHubRepo), which a read answers before it asks GitHub anything: the answer is
// about the name, and so about every pull request the issue links under it, until someone mends
// the link or the rename that makes the name.
func (r *linkedReads) misnamedRepository(link linkedRepository) {
	r.misnamed = append(r.misnamed, link.repo)
}

// unreadPull is a linked pull request of a repository Dispatch cannot read as a refusal names it.
func unreadPull(repo string, number int) string {
	return fmt.Sprintf("%s#%d (%s)", repo, number, model.ReasonRepositoryUnreadable)
}

// couldNotRead is the clause a refusal names what Dispatch could not read of the linked
// repositories with, each with its reason, and says how to mend; "" where it met no repository it
// cannot read. A linked pull request whose read of its state answered so is named as a pull
// request Dispatch could not read; where that read answered and the commit read of its repository
// did not, the clause names the commit in the repository of that pull request instead. The pull
// requests of the first kind are named up to linkedPullRequestReads and the rest counted, since a
// repository Dispatch cannot read lists every pull request the issue links in it, which are as
// many as a person writes; each of the second kind spent one of those reads, so the bound holds
// them already.
func (r linkedReads) couldNotRead() string {
	var parts []string
	if pulls := r.unreadPulls; len(pulls) > 0 {
		noun := "the linked pull request "
		if len(pulls) > 1 {
			noun = "the linked pull requests "
		}
		if more := len(pulls) - linkedPullRequestReads; more > 0 {
			pulls = append(pulls[:linkedPullRequestReads:linkedPullRequestReads], fmt.Sprintf("%d more", more))
		}
		parts = append(parts, noun+joinAnd(pulls))
	}
	switch len(r.unreadCommits) {
	case 0:
	case 1:
		parts = append(parts, "the commit in the repository of the linked pull request "+r.unreadCommits[0])
	default:
		parts = append(parts, "the commit in the repositories of the linked pull requests "+joinAnd(r.unreadCommits))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("; Dispatch could not read %s: grant Dispatch's GitHub App %s, or link a readable pull request",
		strings.Join(parts, ", and "), joinAnd(r.unreadRepos))
}

// leftUnread is the clause a refusal ends with where the bound left links unread, and "" where
// every link was read or needed no read.
func (r linkedReads) leftUnread() string {
	switch r.unread {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("; 1 linked pull request was left unread, past the %d one record reads", linkedPullRequestReads)
	}
	return fmt.Sprintf("; %d linked pull requests were left unread, past the %d one record reads", r.unread, linkedPullRequestReads)
}

// setAside is the clause a refusal names each linked repository with whose name GitHub's naming
// rule refuses, and "" where it met none. Each such repository spent one of the record's reads, so
// the bound holds them already.
func (r linkedReads) setAside() string {
	switch len(r.misnamed) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("; Dispatch set aside the links to pull requests of %s, which GitHub's naming rule refuses as a repository's name",
			r.misnamed[0])
	}
	return fmt.Sprintf("; Dispatch set aside the links to pull requests of %s, which GitHub's naming rule refuses as repositories' names",
		joinAnd(r.misnamed))
}

// linkedRepository is one repository an issue's external links name GitHub pull requests in,
// under the canonical name the repository is called today, with the numbers of those pull
// requests in the order the issue lists its links.
type linkedRepository struct {
	repo    string
	numbers []int
}

// linkedRepositories is every repository that the issue does not resolve to and in which the
// external links of issueKey name a GitHub pull request, each once with the numbers of its
// linked pull requests, in the order the issue lists its links. It groups
// delivery.Attributor.LinkedPullRequests, the list whose heads the results read compares a
// `before_merge` check with, so the route and the read take one list, under the rename pairs
// the boot read. A link to a pull request of the issue's own repository names nothing here:
// that repository is asked about the commit before any link is read.
func (s *server) linkedRepositories(ctx context.Context, q refs.Queryer, issueKey string) ([]linkedRepository, error) {
	pulls, err := s.deps.Attributor.LinkedPullRequests(ctx, q, issueKey)
	if err != nil {
		return nil, err
	}
	var linked []linkedRepository
	for _, pull := range pulls {
		at := slices.IndexFunc(linked, func(l linkedRepository) bool { return strings.EqualFold(l.repo, pull.Repo) })
		if at < 0 {
			linked = append(linked, linkedRepository{repo: pull.Repo})
			at = len(linked) - 1
		}
		linked[at].numbers = append(linked[at].numbers, pull.Number)
	}
	return linked, nil
}

// linkedRepositoryHolds answers whether link's repository counts — one of its linked pull
// requests is open or merged, read from GitHub in link order until one is
// (delivery.PullRequestOpenOrMerged) — and, for a repository that counts, whether it holds sha
// (delivery.RepositoryHoldsCommit). Each pull request read spends one of reads, and one the
// bound leaves unread is counted there and read no further.
//
// A repository does not count, with no error, where every linked pull request closed without
// merging or is one GitHub answers 404 or 410 for, and where a read answers that no read of that
// repository can work: either read, where the App is not installed on it or its token is refused
// it (delivery.RepositoryUnreadable: githubapp.ErrNoInstallation, and
// githubapp.ErrRepositoryUnreadable, the 403 that is no rate limit), and the first read of a pull
// request's state, where the name a link and a stored rename make of it is none GitHub could hold
// (refs.ErrNotGitHubRepo, which refs.SplitGitHubRepo answers before any GitHub call). The commit
// read never answers that name refusal: it splits the same name the same way, and is made only
// after a state read took the name. That 404 or 410
// (delivery.ErrPullRequestNotFound) is an answer and not a failed read: GitHub says there is no
// such pull request, which is a link a person mistyped or one to a pull request since deleted, so
// it counts for nothing, exactly as a closed one that did not merge, and the next linked pull
// request is read. A repository no read of which can work is the same answer about every pull
// request of it, so none of them is read further and its commits are not asked about: a result
// kept under it would read unknown for good. No retry changes it until someone gives the App the
// repository or mends the link, and the two mend apart, so each is kept apart in reads, which the
// refusal names them from: a repository Dispatch cannot read (linkedReads.cannotRead,
// linkedReads.cannotReadCommit), and one whose name the rule refuses
// (linkedReads.misnamedRepository). Read as a failure, any of these would refuse every record of
// the issue until someone removed the link.
//
// The error is a read that did not answer — a rate limit, a 5xx, a transport failure — about the
// commit, or about a pull request's state where no other pull request of the repository is open
// or merged, which refuseUnknownCommit refuses the record for, to be made again.
func (s *server) linkedRepositoryHolds(ctx context.Context, link linkedRepository, sha string, reads *linkedReads) (counts, held bool, err error) {
	for index, number := range link.numbers {
		if reads.left == 0 {
			reads.unread += len(link.numbers) - index
			return false, false, err
		}
		reads.left--
		open, readErr := delivery.PullRequestOpenOrMerged(ctx, s.deps.GitHub, link.repo, number)
		switch {
		case errors.Is(readErr, refs.ErrNotGitHubRepo):
			reads.misnamedRepository(link)
			return false, false, nil
		case delivery.RepositoryUnreadable(readErr):
			reads.cannotRead(link)
			return false, false, nil
		case errors.Is(readErr, delivery.ErrPullRequestNotFound):
			// No such pull request: it counts for nothing, and the next one is read.
		case readErr != nil:
			if err == nil {
				err = readErr
			}
		case open:
			held, err = delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, link.repo, sha)
			if delivery.RepositoryUnreadable(err) {
				reads.cannotReadCommit(link, number)
				return false, false, nil
			}
			return true, held, err
		}
	}
	return false, false, err
}

// commitRepository is whose a result's commit is taken to be, as commitRepositoryOf answers it:
// the repository the check's issue resolves to, where linked is "", or the linked repository
// linked names.
type commitRepository struct {
	// linked is the linked repository the commit is of, under the name it is called today, and ""
	// where the commit is of the repository the check's issue resolves to, which no answer about
	// it names.
	linked string
	// readable says the App answered the default branch of linked (defaultBranchIn): the one
	// answer to whether Dispatch can read a linked repository, which commitStands decides a
	// `before_merge` result by and readThroughRepository decides its read-through by. It is false
	// where linked is "", since no linked repository is read then.
	readable bool
}

// commitRepositoryOf is whose the commit of a result of the check of issueKey is taken to be,
// commitRepo being the linked repository the result's row names (model.CheckResult.CommitRepo).
// It is the one place the results read decides it, and commitStands, onNamedBranch and
// readThroughRepository each read it, so no two of them can answer the question apart.
//
// A row that names no repository, which is every result at a commit of the issue's own
// repository and every row recorded before the column existed, is the issue's own repository's,
// and reads nothing, so the ordinary result costs what it always did. A row that names one names
// a repository GitHub said holds the commit when the result was recorded (refuseUnknownCommit),
// under the name it was called then, which is read again under the name it is called today
// (refs.Renames.Canonical over Deps.Renames, the pairs the boot read, so it costs no read): the
// repository can be renamed since, and GitHub answers 404 for the old name's installation, so
// every result of it would read unknown — the reason the repository of the check's issue is read
// under its canonical name (checkIssueRepository).
//
// A name that is the repository the check's issue resolves to now, compared without case, is the
// issue's own repository: a row stored while the issue resolved elsewhere names that repository,
// and once the issue has come to resolve to it, the result is read as every other result of the
// issue is, and no answer about it names a repository.
//
// For a linked repository, it also reads whether the App can read that repository (readable),
// from defaultBranchIn's memo. Every reader of such a repository reads that memo for it next
// anyway, so the answer costs no GitHub call they would not make.
func (c *comparisonReader) commitRepositoryOf(ctx context.Context, issueKey string, commitRepo *string) commitRepository {
	owner := commitRepository{linked: c.commitRepositoryName(ctx, issueKey, commitRepo)}
	if owner.linked != "" {
		_, owner.readable = c.defaultBranchIn(ctx, owner.linked)
	}
	return owner
}

// commitRepositoryName is commitRepositoryOf's linked alone, which reads no GitHub: the linked
// repository commitRepo names, under its canonical name, and "" where it names none or names the
// repository the check's issue resolves to now. resultBasisAt reads it to tell the heads of the
// repository a result's commit is taken to be of from the others.
func (c *comparisonReader) commitRepositoryName(ctx context.Context, issueKey string, commitRepo *string) string {
	if commitRepo == nil {
		return ""
	}
	linked := c.server.deps.Renames.Canonical(*commitRepo)
	if own, answered := c.repositoryOf(ctx, issueKey); answered && own != nil && strings.EqualFold(linked, *own) {
		return ""
	}
	return linked
}

// linkedBranchBases chooses the branches of one linked repository, each naming its repository
// and the merge a result there must hold. The memo is per issue, when and repository.
func (c *comparisonReader) linkedBranchBases(ctx context.Context, check model.CheckBlock, repo string) comparison {
	key := [3]string{check.IssueKey, check.When, strings.ToLower(repo)}
	if compared, held := c.linkedBases[key]; held {
		return compared
	}
	ownRepo, answered := c.repositoryOf(ctx, check.IssueKey)
	if !answered || ownRepo == nil {
		return noBases(model.ReasonReadFailed)
	}
	own := c.mergesIn(ctx, check.IssueKey, *ownRepo, false)
	linked := c.linkedMerged(ctx, check.IssueKey)
	if !own.read || len(own.pulls) == 0 && !linked.read {
		return noBases(model.ReasonReadFailed)
	}
	compared := c.branchesIn(ctx, check, repo, true, len(own.pulls) > 0 || len(linked.repos) > 0)
	if len(compared.bases) == 0 && compared.reason == "" {
		compared = noBases(model.ReasonRepositoryUnreadable)
	}
	c.linkedBases[key] = compared
	return compared
}

type linkedMergesAnswer struct {
	repos []string
	read  bool
}

// linkedMerged lists the repositories where this issue links a merged pull request, whether or
// not a result has named a commit there. A failed read is not the absence of a merge.
func (c *comparisonReader) linkedMerged(ctx context.Context, issueKey string) linkedMergesAnswer {
	if answer, held := c.linkedMerges[issueKey]; held {
		return answer
	}
	links, err := c.server.linkedRepositories(ctx, c.server.deps.Store.Pool, issueKey)
	answer := linkedMergesAnswer{read: err == nil}
	if err != nil {
		slog.Warn("dispatch: read linked repositories for merged work", "issue", issueKey, "error", err)
	}
	for _, link := range links {
		set := c.mergesIn(ctx, issueKey, link.repo, true)
		answer.read = answer.read && set.read
		if len(set.pulls) > 0 {
			answer.repos = append(answer.repos, link.repo)
		}
	}
	c.linkedMerges[issueKey] = answer
	return answer
}
